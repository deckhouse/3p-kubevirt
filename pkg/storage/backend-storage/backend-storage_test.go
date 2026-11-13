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
 * Copyright 2024 The KubeVirt Contributors
 *
 */

package backendstorage

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	k8sfake "k8s.io/client-go/kubernetes/fake"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	virtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/testutils"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

var _ = Describe("Backend Storage", func() {
	var backendStorage *BackendStorage
	var config *virtconfig.ClusterConfig
	var kvStore cache.Store
	var storageClassStore cache.Store
	var storageProfileStore cache.Store
	var pvcStore cache.Store
	var virtClient *kubecli.MockKubevirtClient

	BeforeEach(func() {
		ctrl := gomock.NewController(GinkgoT())
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		kubevirtFakeConfig := &virtv1.KubeVirtConfiguration{}
		config, _, kvStore = testutils.NewFakeClusterConfigUsingKVConfig(kubevirtFakeConfig)
		storageClassInformer, _ := testutils.NewFakeInformerFor(&storagev1.StorageClass{})
		storageProfileInformer, _ := testutils.NewFakeInformerFor(&cdiv1.StorageProfile{})
		storageClassStore = storageClassInformer.GetStore()
		storageProfileStore = storageProfileInformer.GetStore()
		pvcInformer, _ := testutils.NewFakeInformerFor(&v1.PersistentVolumeClaim{})
		pvcStore = pvcInformer.GetStore()

		backendStorage = NewBackendStorage(virtClient, config, storageClassStore, storageProfileStore, pvcStore)
	})

	Context("Storage class", func() {
		It("Should return VMStateStorageClass and RWX when set", func() {
			By("Setting a VM state storage class in the CR")
			kvCR := testutils.GetFakeKubeVirtClusterConfig(kvStore)
			kvCR.Spec.Configuration.VMStateStorageClass = "myfave"
			testutils.UpdateFakeKubeVirtClusterConfig(kvStore, kvCR)

			By("Expecting getStorageClass() to return that one")
			sc, err := backendStorage.getStorageClass()
			Expect(err).NotTo(HaveOccurred())
			Expect(sc).To(Equal("myfave"))

			By("Expecting getAccessMode() to return RWX")
			accessMode := backendStorage.getAccessMode(sc, v1.PersistentVolumeFilesystem)
			Expect(accessMode).To(Equal(v1.ReadWriteMany))
		})

		It("Should return the default storage class when VMStateStorageClass is not set", func() {
			By("Creating 5 storage classes with one default")
			for i := 0; i < 5; i++ {
				sc := storagev1.StorageClass{
					ObjectMeta: k8smetav1.ObjectMeta{
						Name: fmt.Sprintf("sc%d", i),
					},
				}
				if i == 3 {
					sc.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
				}
				err := storageClassStore.Add(&sc)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Expecting getStorageClass() to return the default one")
			sc, err := backendStorage.getStorageClass()
			Expect(err).NotTo(HaveOccurred())
			Expect(sc).To(Equal("sc3"))

			By("Expecting getAccessMode() to return RWO")
			accessMode := backendStorage.getAccessMode(sc, v1.PersistentVolumeFilesystem)
			Expect(accessMode).To(Equal(v1.ReadWriteOnce))
		})
	})

	Context("Access mode", func() {
		BeforeEach(func() {
			By("Creating a storage profile with no access/volume mode")
			sp := &cdiv1.StorageProfile{
				ObjectMeta: k8smetav1.ObjectMeta{
					Name: "nomode",
				},
				Spec: cdiv1.StorageProfileSpec{},
				Status: cdiv1.StorageProfileStatus{
					ClaimPropertySets: []cdiv1.ClaimPropertySet{},
				},
			}
			err := storageProfileStore.Add(sp)
			Expect(err).NotTo(HaveOccurred())

			By("Creating a storage profile with RWO FS as its only mode")
			sp = sp.DeepCopy()
			sp.Name = "onlyrwo"
			sp.Status.ClaimPropertySets = []cdiv1.ClaimPropertySet{{
				AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
				VolumeMode:  pointer.P(v1.PersistentVolumeFilesystem),
			}}
			err = storageProfileStore.Add(sp)
			Expect(err).NotTo(HaveOccurred())

			By("Creating a storage profile that supports FS in both RWO and RWX")
			sp = sp.DeepCopy()
			sp.Name = "both"
			sp.Status.ClaimPropertySets = []cdiv1.ClaimPropertySet{{
				AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteMany, v1.ReadWriteOnce},
				VolumeMode:  pointer.P(v1.PersistentVolumeFilesystem),
			}}
			err = storageProfileStore.Add(sp)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should default to RWO when no storage profile is defined", func() {
			accessMode := backendStorage.getAccessMode("doesntexist", v1.PersistentVolumeFilesystem)
			Expect(accessMode).To(Equal(v1.ReadWriteOnce))
		})

		It("Should default to RWO when the storage profile doesn't have any access mode", func() {
			accessMode := backendStorage.getAccessMode("nomode", v1.PersistentVolumeFilesystem)
			Expect(accessMode).To(Equal(v1.ReadWriteOnce))
		})

		It("Should pick RWX when both RWX and RWO are available", func() {
			accessMode := backendStorage.getAccessMode("both", v1.PersistentVolumeFilesystem)
			Expect(accessMode).To(Equal(v1.ReadWriteMany))
		})

		It("Should pick RWO when RWX isn't possible", func() {
			accessMode := backendStorage.getAccessMode("onlyrwo", v1.PersistentVolumeFilesystem)
			Expect(accessMode).To(Equal(v1.ReadWriteOnce), fmt.Sprintf("%#v", storageProfileStore.ListKeys()))
		})
	})

	Context("Migration", func() {
		var k8sClient *k8sfake.Clientset
		var migration *virtv1.VirtualMachineInstanceMigration
		const (
			nsName        = "testns"
			vmiName       = "testvmi"
			sourcePVCName = "sourcepvc"
			targetPVCName = "targetpvc"
			migrationName = "migration"
		)

		BeforeEach(func() {
			k8sClient = k8sfake.NewSimpleClientset()
			virtClient.EXPECT().CoreV1().Return(k8sClient.CoreV1()).AnyTimes()
			sourcePVC := &v1.PersistentVolumeClaim{
				ObjectMeta: k8smetav1.ObjectMeta{
					Name:            sourcePVCName,
					UID:             "source-pvc-uid",
					ResourceVersion: "1",
					Labels:          map[string]string{"persistent-state-for": vmiName},
				},
			}
			targetPVC := &v1.PersistentVolumeClaim{
				ObjectMeta: k8smetav1.ObjectMeta{
					Name:            targetPVCName,
					UID:             "target-pvc-uid",
					ResourceVersion: "2",
					Labels:          map[string]string{"kubevirt.io/migrationName": migrationName},
				},
			}
			pvc, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Create(context.TODO(), sourcePVC, k8smetav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			err = pvcStore.Add(pvc)
			Expect(err).NotTo(HaveOccurred())
			pvc, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Create(context.TODO(), targetPVC, k8smetav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			err = pvcStore.Add(pvc)
			Expect(err).NotTo(HaveOccurred())
			migration = &virtv1.VirtualMachineInstanceMigration{
				ObjectMeta: k8smetav1.ObjectMeta{
					Name:      migrationName,
					Namespace: nsName,
				},
				Spec: virtv1.VirtualMachineInstanceMigrationSpec{
					VMIName: vmiName,
				},
				Status: virtv1.VirtualMachineInstanceMigrationStatus{
					MigrationState: &virtv1.VirtualMachineInstanceMigrationState{
						SourcePersistentStatePVCName: sourcePVCName,
						TargetPersistentStatePVCName: targetPVCName,
					},
				},
			}
		})
		It("Should preserve a local PVC named like the remote source on incoming handoff", func() {
			migration.Spec.Receive = &virtv1.VirtualMachineInstanceMigrationTarget{MigrationID: "peer-migration"}
			other, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.Background(), sourcePVCName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			other.Labels = map[string]string{"persistent-state-for": "another-vm"}
			_, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Update(context.Background(), other, k8smetav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(MigrationHandoff(virtClient, pvcStore, migration)).To(Succeed())
			_, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.Background(), sourcePVCName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "a remote source name must not select an unrelated local PVC for deletion")
		})

		It("Should preserve a local PVC named like the remote target on outgoing abort", func() {
			migration.Spec.SendTo = &virtv1.VirtualMachineInstanceMigrationSource{MigrationID: "peer-migration", ConnectURL: "peer:9185"}
			other, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.Background(), targetPVCName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			other.Labels = map[string]string{"persistent-state-for": "another-vm"}
			_, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Update(context.Background(), other, k8smetav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(MigrationAbort(virtClient, migration)).To(Succeed())
			_, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.Background(), targetPVCName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "a remote target name must not select an unrelated local PVC for deletion")
		})

		It("Should finalize an incoming handoff when both clusters use the same PVC name", func() {
			migration.Spec.Receive = &virtv1.VirtualMachineInstanceMigrationTarget{MigrationID: "peer-migration"}
			migration.Status.MigrationState.SourcePersistentStatePVCName = targetPVCName
			Expect(MigrationHandoff(virtClient, pvcStore, migration)).To(Succeed())
			target, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.Background(), targetPVCName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(target.Labels).To(HaveKeyWithValue(PVCPrefix, vmiName))
			Expect(target.Labels).NotTo(HaveKey(virtv1.MigrationNameLabel))
			Expect(pvcStore.Update(target)).To(Succeed())
			Expect(MigrationHandoff(virtClient, pvcStore, migration)).To(Succeed(),
				"handoff must be idempotent after the informer observes relabelling")
		})

		It("Should remove the incoming target even when its name equals the remote source", func() {
			migration.Spec.Receive = &virtv1.VirtualMachineInstanceMigrationTarget{MigrationID: "peer-migration"}
			migration.Status.MigrationState.SourcePersistentStatePVCName = targetPVCName
			Expect(MigrationAbort(virtClient, migration)).To(Succeed())
			_, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.Background(), targetPVCName, k8smetav1.GetOptions{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		DescribeTable("Should refuse to delete a PVC reassigned to another owner", func(abort bool) {
			name := sourcePVCName
			if abort {
				name = targetPVCName
			}
			pvc, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.Background(), name, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			pvc.Labels = map[string]string{PVCPrefix: "another-vm"}
			_, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Update(context.Background(), pvc, k8smetav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			if abort {
				err = MigrationAbort(virtClient, migration)
			} else {
				err = MigrationHandoff(virtClient, pvcStore, migration)
			}
			Expect(err).To(HaveOccurred())
			_, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.Background(), name, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
		}, Entry("handoff", false), Entry("abort", true))

		DescribeTable("Should not mount a remote source PVC for incoming recovery", func(completed bool) {
			migration.Spec.Receive = &virtv1.VirtualMachineInstanceMigrationTarget{MigrationID: "peer-migration"}
			migration.Status.MigrationState.Completed = completed
			vmi := libvmi.New(libvmi.WithNamespace(nsName))
			Expect(RecoverFromBrokenMigration(virtClient, migration, pvcStore, vmi, "launcher")).To(Succeed())
			if completed {
				Expect(migration.Status.Phase).To(Equal(virtv1.MigrationSucceeded))
			} else {
				Expect(migration.Status.Phase).To(Equal(virtv1.MigrationFailed))
			}
			jobs, err := k8sClient.BatchV1().Jobs(nsName).List(context.Background(), k8smetav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs.Items).To(BeEmpty())
		}, Entry("recorded success", true), Entry("interrupted before success", false))

		It("refuses recovery from a source PVC reassigned to another VMI", func() {
			source, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.Background(), sourcePVCName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			source.Labels[PVCPrefix] = "another-vmi"
			_, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Update(context.Background(), source, k8smetav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			vmi := libvmi.New(libvmi.WithNamespace(nsName))
			err = RecoverFromBrokenMigration(virtClient, migration, pvcStore, vmi, "launcher")
			Expect(err).To(MatchError(ContainSubstring("does not belong to VMI")))
			jobs, err := k8sClient.BatchV1().Jobs(nsName).List(context.Background(), k8smetav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs.Items).To(BeEmpty())
		})

		It("Should recover an outgoing migration using only its local source PVC", func() {
			migration.Spec.SendTo = &virtv1.VirtualMachineInstanceMigrationSource{MigrationID: "peer-migration", ConnectURL: "peer:9185"}
			// Equal names across clusters do not imply a shared volume.
			migration.Status.MigrationState.TargetPersistentStatePVCName = sourcePVCName
			virtClient.EXPECT().BatchV1().Return(k8sClient.BatchV1()).AnyTimes()
			vmi := libvmi.New(libvmi.WithNamespace(nsName))
			Expect(RecoverFromBrokenMigration(virtClient, migration, pvcStore, vmi, "launcher")).To(MatchError("a migration recovery had to be initiated"))
			job, err := k8sClient.BatchV1().Jobs(nsName).Get(context.Background(), "recover-"+migration.Name, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(sourcePVCName))
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: v1.ConditionTrue}}
			_, err = k8sClient.BatchV1().Jobs(nsName).UpdateStatus(context.Background(), job, k8smetav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(RecoverFromBrokenMigration(virtClient, migration, pvcStore, vmi, "launcher")).To(Succeed())
			Expect(migration.Status.Phase).To(Equal(virtv1.MigrationSucceeded))
			_, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.Background(), sourcePVCName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "source storage stays with the source VM")
		})

		It("Should leave local PVCs untouched on outgoing handoff", func() {
			migration.Spec.SendTo = &virtv1.VirtualMachineInstanceMigrationSource{MigrationID: "peer-migration", ConnectURL: "peer:9185"}
			k8sClient.ClearActions()
			Expect(MigrationHandoff(virtClient, pvcStore, migration)).To(Succeed())
			Expect(k8sClient.Actions()).To(BeEmpty())
		})

		It("Should abort a target that was already promoted by handoff", func() {
			migration.Spec.Receive = &virtv1.VirtualMachineInstanceMigrationTarget{MigrationID: "peer-migration"}
			Expect(MigrationHandoff(virtClient, pvcStore, migration)).To(Succeed())
			Expect(MigrationAbort(virtClient, migration)).To(Succeed())
			_, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.Background(), targetPVCName, k8smetav1.GetOptions{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		DescribeTable("Should condition deletion on the checked PVC identity and version", func(abort bool) {
			k8sClient.ClearActions()
			var err error
			if abort {
				err = MigrationAbort(virtClient, migration)
			} else {
				err = MigrationHandoff(virtClient, pvcStore, migration)
			}
			Expect(err).NotTo(HaveOccurred())
			var deletion clienttesting.DeleteAction
			for _, action := range k8sClient.Actions() {
				if action.Matches("delete", "persistentvolumeclaims") {
					deletion = action.(clienttesting.DeleteAction)
				}
			}
			Expect(deletion).NotTo(BeNil())
			uid, version := "source-pvc-uid", "1"
			if abort {
				uid, version = "target-pvc-uid", "2"
			}
			Expect(deletion.GetDeleteOptions().Preconditions).To(Equal(&k8smetav1.Preconditions{
				UID: pointer.P(types.UID(uid)), ResourceVersion: &version,
			}))
		}, Entry("handoff", false), Entry("abort", true))

		It("Should label the target PVC and remove the source PVC on migration success", func() {
			err := MigrationHandoff(virtClient, pvcStore, migration)
			Expect(err).NotTo(HaveOccurred())
			_, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.TODO(), sourcePVCName, k8smetav1.GetOptions{})
			Expect(err).To(MatchError(errors.IsNotFound, "k8serrors.IsNotFound"))
			targetPVC, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.TODO(), targetPVCName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(targetPVC.Labels).To(HaveKeyWithValue("persistent-state-for", vmiName))
		})
		It("Should remove the target PVC on migration failure", func() {
			err := MigrationAbort(virtClient, migration)
			Expect(err).NotTo(HaveOccurred())
			sourcePVC, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.TODO(), sourcePVCName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(sourcePVC.Labels).To(HaveKeyWithValue("persistent-state-for", vmiName))
			_, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.TODO(), targetPVCName, k8smetav1.GetOptions{})
			Expect(err).To(MatchError(errors.IsNotFound, "k8serrors.IsNotFound"))
		})
		It("Should keep the shared PVC on migration success", func() {
			migration.Status.MigrationState.TargetPersistentStatePVCName = sourcePVCName
			err := MigrationHandoff(virtClient, pvcStore, migration)
			Expect(err).NotTo(HaveOccurred())
			sourcePVC, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.TODO(), sourcePVCName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(sourcePVC.Labels).To(HaveKeyWithValue("persistent-state-for", vmiName))
		})
		It("Should keep the shared PVC on migration failure", func() {
			migration.Status.MigrationState.TargetPersistentStatePVCName = sourcePVCName
			err := MigrationAbort(virtClient, migration)
			Expect(err).NotTo(HaveOccurred())
			sourcePVC, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.TODO(), sourcePVCName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(sourcePVC.Labels).To(HaveKeyWithValue("persistent-state-for", vmiName))
		})
	})

	Context("createPVC", func() {
		var k8sClient *k8sfake.Clientset
		const (
			nsName  = "testns"
			vmiName = "testvmi"
			pvcName = "persistent-state-for-" + vmiName
		)

		BeforeEach(func() {
			k8sClient = k8sfake.NewSimpleClientset()
			virtClient.EXPECT().CoreV1().Return(k8sClient.CoreV1()).AnyTimes()
		})

		It("Should create a PVC with the correct labels", func() {
			vmi := &virtv1.VirtualMachineInstance{
				ObjectMeta: k8smetav1.ObjectMeta{
					Name:      vmiName,
					Namespace: nsName,
				},
			}

			sc := storagev1.StorageClass{
				ObjectMeta: k8smetav1.ObjectMeta{
					Name:        "sc",
					Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"},
				},
			}
			err := storageClassStore.Add(&sc)
			Expect(err).NotTo(HaveOccurred())

			pvc, err := backendStorage.createPVC(vmi, map[string]string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(pvc).NotTo(BeNil())
			Expect(pvc.Labels).To(HaveKeyWithValue(LabelApplyStorageProfile, "true"))
		})
	})

	Context("Legacy PVCs", func() {
		var k8sClient *k8sfake.Clientset

		const (
			nsName  = "testns"
			vmiName = "testvmi"
			pvcName = "persistent-state-for-" + vmiName
		)

		BeforeEach(func() {
			k8sClient = k8sfake.NewSimpleClientset()
			virtClient.EXPECT().CoreV1().Return(k8sClient.CoreV1()).AnyTimes()
			legacyPVC := &v1.PersistentVolumeClaim{
				ObjectMeta: k8smetav1.ObjectMeta{
					Name:      pvcName,
					Namespace: nsName,
				},
			}
			pvc, err := k8sClient.CoreV1().PersistentVolumeClaims(nsName).Create(context.TODO(), legacyPVC, k8smetav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			err = pvcStore.Add(pvc)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should get labelled by CreatePVCForVMI when called with a KubeVirt client", func() {
			vmi := &virtv1.VirtualMachineInstance{
				ObjectMeta: k8smetav1.ObjectMeta{
					Name:      vmiName,
					Namespace: nsName,
				},
			}
			pvc, err := backendStorage.CreatePVCForVMI(vmi)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvc).NotTo(BeNil())
			pvc, err = k8sClient.CoreV1().PersistentVolumeClaims(nsName).Get(context.TODO(), pvcName, k8smetav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(pvc.Labels).To(HaveKeyWithValue(PVCPrefix, vmiName))
		})
	})
	Context("IsBackendStorageNeeded", func() {
		var vm *virtv1.VirtualMachine

		BeforeEach(func() {
			vm = libvmi.NewVirtualMachine(libvmi.New(libvmi.WithUefi(false)))
			vm.Spec.Template.Spec.Domain.Devices.TPM = &virtv1.TPMDevice{}
		})

		DescribeTable("should", func(expected bool, alter func(vm *virtv1.VirtualMachine)) {
			alter(vm)
			Expect(IsBackendStorageNeededForVM(vm)).To(Equal(expected))
			Expect(IsBackendStorageNeededForVMI(&vm.Spec.Template.Spec)).To(Equal(expected))
		},
			Entry("be false when no persistent feature is set", false, func(_ *virtv1.VirtualMachine) {
			}),
			Entry("be true when persistent TPM is set", true, func(vm *virtv1.VirtualMachine) {
				vm.Spec.Template.Spec.Domain.Devices.TPM.Persistent = pointer.P(true)
			}),
			Entry("be true when persistent EFI is set", true, func(vm *virtv1.VirtualMachine) {
				vm.Spec.Template.Spec.Domain.Firmware.Bootloader.EFI.Persistent = pointer.P(true)
			}),
			Entry("be true when persistent TPM and EFI are set", true, func(vm *virtv1.VirtualMachine) {
				vm.Spec.Template.Spec.Domain.Devices.TPM.Persistent = pointer.P(true)
				vm.Spec.Template.Spec.Domain.Firmware.Bootloader.EFI.Persistent = pointer.P(true)
			}),
		)
	})
})
