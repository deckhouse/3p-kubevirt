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

	v1 "kubevirt.io/api/core/v1"
)

// FindMigrationIP returns the IP to bind live migration to. When ifaceName is
// empty, the upstream default v1.MigrationInterfaceName ("migration0") is
// used. If the interface is not present, migrationIp is returned unchanged.
func FindMigrationIP(migrationIp, ifaceName string) (string, error) {
	if ifaceName == "" {
		ifaceName = v1.MigrationInterfaceName
	}
	ief, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return migrationIp, nil
	}
	addrs, err := ief.Addrs()
	if err != nil {
		return migrationIp, fmt.Errorf("%s present but doesn't have an IP", ifaceName)
	}
	for _, addr := range addrs {
		if !addr.(*net.IPNet).IP.IsGlobalUnicast() {
			continue
		}
		ip := addr.(*net.IPNet).IP.To16()
		if ip != nil {
			return ip.String(), nil
		}
	}
	return migrationIp, fmt.Errorf("no IP found on %s", ifaceName)
}
