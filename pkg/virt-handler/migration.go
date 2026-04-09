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

package virthandler

import (
	"fmt"
	"net"
	"os"

	v1 "kubevirt.io/api/core/v1"
)

// MigrationNetworkInterfaceEnv overrides the dedicated migration network
// interface name that virt-handler inspects to discover the migration IP.
// When unset, the upstream default v1.MigrationInterfaceName ("migration0") is used.
const MigrationNetworkInterfaceEnv = "MIGRATION_NETWORK_INTERFACE"

// FindMigrationIP looks for a dedicated migration network interface. If found,
// sets migration IP to its first global unicast address. The interface name
// defaults to v1.MigrationInterfaceName ("migration0") and can be overridden
// via the MIGRATION_NETWORK_INTERFACE environment variable.
func FindMigrationIP(migrationIp string) (string, error) {
	return FindMigrationIPOnInterface(migrationIp, os.Getenv(MigrationNetworkInterfaceEnv))
}

// FindMigrationIPOnInterface is the explicit form of FindMigrationIP that
// takes the interface name directly. An empty ifaceName falls back to
// v1.MigrationInterfaceName.
func FindMigrationIPOnInterface(migrationIp, ifaceName string) (string, error) {
	if ifaceName == "" {
		ifaceName = v1.MigrationInterfaceName
	}
	ief, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return migrationIp, nil
	}
	addrs, err := ief.Addrs()
	if err != nil { // get addresses
		return migrationIp, fmt.Errorf("%s present but doesn't have an IP", ifaceName)
	}
	for _, addr := range addrs {
		if !addr.(*net.IPNet).IP.IsGlobalUnicast() {
			// skip local/multicast IPs
			continue
		}
		ip := addr.(*net.IPNet).IP.To16()
		if ip != nil {
			return ip.String(), nil
		}
	}

	return migrationIp, fmt.Errorf("no IP found on %s", ifaceName)
}
