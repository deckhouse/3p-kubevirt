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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

var _ = Describe("isMigrationInProgress", func() {
	newFailedMigrationState := func() *v1.VirtualMachineInstanceMigrationState {
		return &v1.VirtualMachineInstanceMigrationState{
			StartTimestamp: pointer.P(metav1.Now()),
			EndTimestamp:   pointer.P(metav1.Now()),
			Failed:         true,
		}
	}

	newPausedDomain := func(reason api.StateChangeReason, migrationFailed bool) *api.Domain {
		domain := api.NewMinimalDomain("testvmi")
		domain.Status.Status = api.Paused
		domain.Status.Reason = reason
		domain.Spec.Metadata.KubeVirt.Migration = &api.MigrationMetadata{
			StartTimestamp: pointer.P(metav1.Now()),
			EndTimestamp:   pointer.P(metav1.Now()),
			Failed:         migrationFailed,
		}
		return domain
	}

	newVMI := func(state *v1.VirtualMachineInstanceMigrationState) *v1.VirtualMachineInstance {
		vmi := &v1.VirtualMachineInstance{}
		vmi.Status.MigrationState = state
		return vmi
	}

	It("should not consider the domain left paused by a failed migration as migrating", func() {
		Expect(isMigrationInProgress(newVMI(newFailedMigrationState()), newPausedDomain(api.ReasonPausedMigration, true))).To(BeFalse())
	})

	DescribeTable("should consider the paused domain as migrating", func(state *v1.VirtualMachineInstanceMigrationState, domain *api.Domain) {
		Expect(isMigrationInProgress(newVMI(state), domain)).To(BeTrue())
	},
		Entry("when the domain has not reported the failure yet",
			newFailedMigrationState(), newPausedDomain(api.ReasonPausedMigration, false),
		),
		Entry("when the vmi has not recorded the failure yet",
			&v1.VirtualMachineInstanceMigrationState{
				StartTimestamp: pointer.P(metav1.Now()),
				EndTimestamp:   pointer.P(metav1.Now()),
			},
			newPausedDomain(api.ReasonPausedMigration, true),
		),
		Entry("when the target has reported the domain ready",
			func() *v1.VirtualMachineInstanceMigrationState {
				state := newFailedMigrationState()
				state.TargetNodeDomainReadyTimestamp = pointer.P(metav1.Now())
				return state
			}(),
			newPausedDomain(api.ReasonPausedMigration, true),
		),
		Entry("when the migration was in post-copy",
			func() *v1.VirtualMachineInstanceMigrationState {
				state := newFailedMigrationState()
				state.Mode = v1.MigrationPostCopy
				return state
			}(),
			newPausedDomain(api.ReasonPausedMigration, true),
		),
		Entry("when the domain is paused in post-copy",
			newFailedMigrationState(), newPausedDomain(api.ReasonPausedPostcopy, true),
		),
	)
})
