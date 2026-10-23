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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "kubevirt.io/api/core/v1"
)

var _ = DescribeTable("isMigratedHere",
	func(ms *v1.VirtualMachineInstanceMigrationState, nodeName string, want bool) {
		vmi := &v1.VirtualMachineInstance{Status: v1.VirtualMachineInstanceStatus{NodeName: nodeName, MigrationState: ms}}
		Expect(isMigratedHere(vmi, "node-b")).To(Equal(want))
	},
	Entry("completed onto this node", &v1.VirtualMachineInstanceMigrationState{Completed: true, TargetState: &v1.VirtualMachineInstanceMigrationTargetState{VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{Node: "node-b"}}}, "node-b", true),
	Entry("still migrating", &v1.VirtualMachineInstanceMigrationState{TargetState: &v1.VirtualMachineInstanceMigrationTargetState{VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{Node: "node-b"}}}, "node-b", false),
	Entry("failed", &v1.VirtualMachineInstanceMigrationState{Completed: true, Failed: true, TargetState: &v1.VirtualMachineInstanceMigrationTargetState{VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{Node: "node-b"}}}, "node-b", false),
	Entry("completed onto another node", &v1.VirtualMachineInstanceMigrationState{Completed: true, TargetState: &v1.VirtualMachineInstanceMigrationTargetState{VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{Node: "node-c"}}}, "node-c", false),
	Entry("no migration", nil, "node-b", false),
)
