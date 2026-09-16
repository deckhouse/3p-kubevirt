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
 * Copyright 2026 Flant JSC
 *
 */

package vm

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	virtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/pointer"
)

var _ = Describe("volumeMigrationFinished", func() {
	const (
		volumeName = "rootdisk"
		sourcePVC  = "pvc-source"
		targetPVC  = "pvc-target"
	)

	claimVolume := func(claim string) virtv1.Volume {
		return virtv1.Volume{
			Name: volumeName,
			VolumeSource: virtv1.VolumeSource{
				PersistentVolumeClaim: &virtv1.PersistentVolumeClaimVolumeSource{
					PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
				},
			},
		}
	}

	newVM := func(claim string) *virtv1.VirtualMachine {
		return &virtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "testvm", Namespace: metav1.NamespaceDefault, Generation: 3},
			Spec: virtv1.VirtualMachineSpec{
				UpdateVolumesStrategy: pointer.P(virtv1.UpdateVolumesStrategyMigration),
				Template: &virtv1.VirtualMachineInstanceTemplateSpec{
					Spec: virtv1.VirtualMachineInstanceSpec{Volumes: []virtv1.Volume{claimVolume(claim)}},
				},
			},
			Status: virtv1.VirtualMachineStatus{
				VolumeUpdateState: &virtv1.VolumeUpdateState{
					VolumeMigrationState: &virtv1.VolumeMigrationState{
						MigratedVolumes: []virtv1.StorageMigratedVolumeInfo{{
							VolumeName:         volumeName,
							SourcePVCInfo:      &virtv1.PersistentVolumeClaimInfo{ClaimName: sourcePVC},
							DestinationPVCInfo: &virtv1.PersistentVolumeClaimInfo{ClaimName: targetPVC},
						}},
					},
				},
			},
		}
	}

	newVMI := func(claim string) *virtv1.VirtualMachineInstance {
		return &virtv1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "testvm", Namespace: metav1.NamespaceDefault},
			Spec:       virtv1.VirtualMachineInstanceSpec{Volumes: []virtv1.Volume{claimVolume(claim)}},
			Status:     virtv1.VirtualMachineInstanceStatus{Phase: virtv1.Running},
		}
	}

	It("reports a migration that landed on both the machine and its instance", func() {
		Expect(volumeMigrationFinished(newVM(targetPVC), newVMI(targetPVC))).To(BeTrue())
	})

	It("waits while the instance still runs the source claim", func() {
		Expect(volumeMigrationFinished(newVM(targetPVC), newVMI(sourcePVC))).To(BeFalse())
	})

	It("ignores a round that was reverted to the source", func() {
		Expect(volumeMigrationFinished(newVM(sourcePVC), newVMI(sourcePVC))).To(BeFalse())
	})

	It("waits while the volume change is still in flight", func() {
		vmi := newVMI(targetPVC)
		vmi.Status.Conditions = append(vmi.Status.Conditions, virtv1.VirtualMachineInstanceCondition{
			Type:   virtv1.VirtualMachineInstanceVolumesChange,
			Status: k8sv1.ConditionTrue,
		})
		Expect(volumeMigrationFinished(newVM(targetPVC), vmi)).To(BeFalse())
	})

	It("does nothing without a recorded migration", func() {
		vm := newVM(targetPVC)
		vm.Status.VolumeUpdateState = nil
		Expect(volumeMigrationFinished(vm, newVMI(targetPVC))).To(BeFalse())
	})

	It("does nothing once the strategy has already been dropped", func() {
		vm := newVM(targetPVC)
		vm.Spec.UpdateVolumesStrategy = nil
		Expect(volumeMigrationFinished(vm, newVMI(targetPVC))).To(BeFalse())
	})

	It("does nothing while the instance is not running", func() {
		vmi := newVMI(targetPVC)
		vmi.Status.Phase = virtv1.Scheduling
		Expect(volumeMigrationFinished(newVM(targetPVC), vmi)).To(BeFalse())
	})

	It("does nothing for an instance on its way out", func() {
		vmi := newVMI(targetPVC)
		vmi.DeletionTimestamp = pointer.P(metav1.Now())
		Expect(volumeMigrationFinished(newVM(targetPVC), vmi)).To(BeFalse())
	})
})
