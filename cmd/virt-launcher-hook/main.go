/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

// Libvirt hook binary for qemu.
// Installed at /etc/libvirt/hooks/qemu.
// Called by libvirt with: domain_name operation sub-operation [extra]
// Domain XML is passed via stdin.
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	conntrackHookSock = "/var/run/kubevirt/sockets/conntrack-hook.sock"
	socketTimeout     = 1 * time.Second

	qemuConfPath = "/etc/libvirt/qemu.conf"
)

func main() {
	// Write error logs to container stderr
	if f, err := os.OpenFile("/proc/1/fd/2", os.O_WRONLY|os.O_APPEND, 0); err == nil {
		log.SetOutput(f)
	}
	log.SetFlags(0)
	log.SetPrefix("qemu-hook: ")

	if len(os.Args) < 4 {
		log.Printf("expected at least 3 args, got %d", len(os.Args)-1)
		os.Exit(0)
	}

	operation := os.Args[2]
	subOperation := os.Args[3]

	if operation == "migrate" && subOperation == "begin" {
		if err := injectSharedDiskSeclabels(os.Stdin, os.Stdout); err != nil {
			log.Printf("seclabel injection error: %v", err)
		}
		return
	}

	// "started begin" fires on the destination after migration data transfer
	// completes but before VM resumes. Used to gate conntrack injection.
	if operation == "started" && subOperation == "begin" {
		if err := waitForConntrackSync(); err != nil {
			log.Printf("conntrack sync error: %v", err)
		}
		return
	}
}

// sourceLineRE matches a libvirt disk <source file='X'/> or <source file='X'></source>
// line that does NOT already carry child elements
var sourceLineRE = regexp.MustCompile(`<source\s+file=(['"])([^'"]+)\1\s*((?:/>)|(?:></source>))`)

func injectSharedDiskSeclabels(in io.Reader, out io.Writer) error {
	xmlBytes, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	sharedPaths := getSharedFilesystemPaths()
	if len(sharedPaths) == 0 {
		// No shared paths declared — passthrough original XML unchanged.
		_, err = out.Write(xmlBytes)
		return err
	}

	modified := sourceLineRE.ReplaceAllStringFunc(string(xmlBytes), func(match string) string {
		groups := sourceLineRE.FindStringSubmatch(match)
		if len(groups) < 4 {
			return match
		}
		quote := groups[1]
		path := groups[2]
		if !pathInShared(path, sharedPaths) {
			return match
		}
		return fmt.Sprintf("<source file=%s%s%s><seclabel relabel='no'/></source>", quote, path, quote)
	})

	_, err = io.WriteString(out, modified)
	return err
}

func getSharedFilesystemPaths() []string {
	if env := os.Getenv("SHARED_FILESYSTEM_PATHS"); env != "" {
		return splitNonEmpty(env, ":")
	}
	return parseSharedFilesystemsFromQemuConf(qemuConfPath)
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

var sharedFsConfRE = regexp.MustCompile(`(?m)^\s*shared_filesystems\s*=\s*\[\s*([^\]]*)\]`)
var quotedPathRE = regexp.MustCompile(`"([^"]+)"`)

func parseSharedFilesystemsFromQemuConf(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	m := sharedFsConfRE.FindStringSubmatch(string(data))
	if len(m) < 2 {
		return nil
	}
	var out []string
	for _, mm := range quotedPathRE.FindAllStringSubmatch(m[1], -1) {
		if len(mm) >= 2 && mm[1] != "" {
			out = append(out, mm[1])
		}
	}
	return out
}

func pathInShared(path string, sharedPaths []string) bool {
	clean := filepath.Clean(path)
	for _, sp := range sharedPaths {
		c := filepath.Clean(sp)
		if clean == c || strings.HasPrefix(clean, c+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func waitForConntrackSync() error {
	if _, err := os.Stat(conntrackHookSock); err != nil {
		return nil
	}

	conn, err := net.DialTimeout("unix", conntrackHookSock, socketTimeout)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(socketTimeout))

	if _, err := conn.Write([]byte("wait\n")); err != nil {
		return fmt.Errorf("failed to send wait: %w", err)
	}

	response, err := readResponse(conn)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	if response != "ok" {
		return fmt.Errorf("unexpected response: %s", response)
	}

	return nil
}

func readResponse(conn net.Conn) (string, error) {
	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		return scanner.Text(), nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no response")
}
