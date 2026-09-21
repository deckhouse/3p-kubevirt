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
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	virtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/kubevirt/fake"
	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/pointer"
)

var _ = Describe("reverted volume migration recovery", func() {
	var (
		ctx        context.Context
		client     *fake.Clientset
		coreClient *k8sfake.Clientset
		launcher   *k8sv1.Pod
		c          *Controller
		vm         *virtv1.VirtualMachine
		vmi        *virtv1.VirtualMachineInstance
	)

	BeforeEach(func() {
		ctx = context.Background()
		client = fake.NewSimpleClientset()
		coreClient = k8sfake.NewSimpleClientset()
		virtClient := kubecli.NewMockKubevirtClient(gomock.NewController(GinkgoT()))
		virtClient.EXPECT().CoreV1().Return(coreClient.CoreV1()).AnyTimes()
		virtClient.EXPECT().VirtualMachineInstance(metav1.NamespaceDefault).
			Return(client.KubevirtV1().VirtualMachineInstances(metav1.NamespaceDefault)).AnyTimes()
		virtClient.EXPECT().VirtualMachineInstanceMigration(metav1.NamespaceDefault).
			Return(client.KubevirtV1().VirtualMachineInstanceMigrations(metav1.NamespaceDefault)).AnyTimes()
		c = &Controller{
			clientset:       virtClient,
			Queue:           workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
			pvcStore:        cache.NewStore(cache.MetaNamespaceKeyFunc),
			dataVolumeStore: cache.NewStore(cache.MetaNamespaceKeyFunc),
		}
		DeferCleanup(c.Queue.ShutDown)
		vmi = libvmi.New(libvmi.WithName("testvm"), libvmi.WithNamespace(metav1.NamespaceDefault),
			libvmi.WithPersistentVolumeClaim("root", "source-root"),
			libvmi.WithPersistentVolumeClaim("data", "source-data"))
		vmi.UID = "same-running-instance"
		vmi.ResourceVersion = "10"
		vmi.Status.Phase = virtv1.Running
		vmi.Status.NodeName = "source-node"
		vmi.Status.Conditions = []virtv1.VirtualMachineInstanceCondition{
			{Type: virtv1.VirtualMachineInstanceReady, Status: k8sv1.ConditionTrue},
			{Type: virtv1.VirtualMachineInstanceVolumesChange, Status: k8sv1.ConditionFalse},
		}
		for _, name := range []string{"root", "data"} {
			vmi.Status.MigratedVolumes = append(vmi.Status.MigratedVolumes, virtv1.StorageMigratedVolumeInfo{
				VolumeName:         name,
				SourcePVCInfo:      &virtv1.PersistentVolumeClaimInfo{ClaimName: "source-" + name},
				DestinationPVCInfo: &virtv1.PersistentVolumeClaimInfo{ClaimName: "old-target-" + name},
			})
			vmi.Status.VolumeStatus = append(vmi.Status.VolumeStatus, virtv1.VolumeStatus{
				Name: name,
				PersistentVolumeClaimInfo: &virtv1.PersistentVolumeClaimInfo{
					ClaimName: "source-" + name, AccessModes: []k8sv1.PersistentVolumeAccessMode{k8sv1.ReadWriteOnce},
				},
			})
		}
		vm = libvmi.NewVirtualMachine(vmi)
		launcher = &k8sv1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "source-launcher", Namespace: vmi.Namespace, UID: "source-launcher-uid",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(vmi, virtv1.VirtualMachineInstanceGroupVersionKind)}},
			Spec:   k8sv1.PodSpec{NodeName: vmi.Status.NodeName},
			Status: k8sv1.PodStatus{Phase: k8sv1.PodRunning},
		}
		for _, name := range []string{"root", "data"} {
			launcher.Spec.Volumes = append(launcher.Spec.Volumes, k8sv1.Volume{
				Name: name, VolumeSource: k8sv1.VolumeSource{PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{ClaimName: "source-" + name}},
			})
		}
		_, err := coreClient.CoreV1().Pods(vmi.Namespace).Create(ctx, launcher, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	persist := func() {
		_, err := client.KubevirtV1().VirtualMachineInstances(vmi.Namespace).Create(ctx, vmi, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		client.ClearActions()
	}
	stored := func() *virtv1.VirtualMachineInstance {
		result, err := client.KubevirtV1().VirtualMachineInstances(vmi.Namespace).Get(ctx, vmi.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return result
	}
	addMigration := func(name string, phase virtv1.VirtualMachineInstanceMigrationPhase) *virtv1.VirtualMachineInstanceMigration {
		mig, err := client.KubevirtV1().VirtualMachineInstanceMigrations(vmi.Namespace).Create(ctx,
			&virtv1.VirtualMachineInstanceMigration{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: vmi.Namespace},
				Spec:       virtv1.VirtualMachineInstanceMigrationSpec{VMIName: vmi.Name},
				Status:     virtv1.VirtualMachineInstanceMigrationStatus{Phase: phase},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		return mig
	}

	DescribeTable("clears the old round when both specs already use the source claims", func(status k8sv1.ConditionStatus) {
		if status == "" {
			vmi.Status.Conditions = vmi.Status.Conditions[:1]
		} else {
			vmi.Status.Conditions[1].Status = status
		}
		addMigration("failed-before-target-start", virtv1.MigrationFailed)
		persist()
		original := vmi.DeepCopy()
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		updated := stored()
		Expect(updated.Status.MigratedVolumes).To(BeEmpty())
		Expect(updated.Spec).To(Equal(original.Spec))
		Expect(updated.UID).To(Equal(original.UID))
		Expect(updated.Status.Phase).To(Equal(virtv1.Running))
		Expect(updated.Status.Conditions[0]).To(Equal(original.Status.Conditions[0]))
		Expect(vmi).To(Equal(original), "the informer object must remain unchanged")
	},
		Entry("with VolumesChange=False", k8sv1.ConditionFalse),
		Entry("with VolumesChange=True", k8sv1.ConditionTrue),
		Entry("without VolumesChange", k8sv1.ConditionStatus("")),
	)

	It("prepares a new volume migration on the same instance after recovery", func() {
		persist()
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		vmi = stored()
		Expect(vmi.Status.MigratedVolumes).To(BeEmpty())
		vm.Spec.UpdateVolumesStrategy = pointer.P(virtv1.UpdateVolumesStrategyMigration)
		for i := range vm.Spec.Template.Spec.Volumes {
			vol := &vm.Spec.Template.Spec.Volumes[i]
			vol.PersistentVolumeClaim.ClaimName = "new-target-" + vol.Name
			for _, claim := range []string{"source-" + vol.Name, vol.PersistentVolumeClaim.ClaimName} {
				Expect(c.pvcStore.Add(&k8sv1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: vmi.Namespace},
					Spec:       k8sv1.PersistentVolumeClaimSpec{AccessModes: []k8sv1.PersistentVolumeAccessMode{k8sv1.ReadWriteOnce}},
				})).To(Succeed())
			}
		}
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		// The next reconcile observes the newly generated migration records
		// before applying the destination claims to the instance.
		vmi = stored()
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		updated := stored()
		Expect(updated.Spec.Volumes).To(Equal(vm.Spec.Template.Spec.Volumes))
		Expect(updated.Status.MigratedVolumes).To(HaveLen(2))
		for _, volume := range updated.Status.MigratedVolumes {
			Expect(volume.SourcePVCInfo.ClaimName).To(Equal("source-" + volume.VolumeName))
			Expect(volume.DestinationPVCInfo.ClaimName).To(Equal("new-target-" + volume.VolumeName))
		}
		Expect(updated.UID).To(Equal(vmi.UID))
	})

	DescribeTable("preserves the old round while a migration object is unfinished", func(phase virtv1.VirtualMachineInstanceMigrationPhase) {
		addMigration("unfinished", phase)
		persist()
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		Expect(stored()).To(Equal(vmi))
	},
		Entry("unset", virtv1.MigrationPhaseUnset),
		Entry("pending", virtv1.MigrationPending),
		Entry("scheduling", virtv1.MigrationScheduling),
		Entry("running", virtv1.MigrationRunning),
	)

	It("retries recovery when a pending migration finishes without updating the VMI", func() {
		migration := addMigration("pending", virtv1.MigrationPending)
		persist()
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		Expect(stored()).To(Equal(vmi))
		migration.Status.Phase = virtv1.MigrationFailed
		_, err := client.KubevirtV1().VirtualMachineInstanceMigrations(vmi.Namespace).Update(ctx, migration, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(c.Queue.Len, 10*time.Second, 50*time.Millisecond).Should(Equal(1))
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		Expect(stored().Status.MigratedVolumes).To(BeEmpty())
	})

	DescribeTable("recovers after a final migration state", func(state *virtv1.VirtualMachineInstanceMigrationState) {
		vmi.Status.MigrationState = state
		persist()
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		Expect(stored().Status.MigratedVolumes).To(BeEmpty())
	},
		Entry("failed", &virtv1.VirtualMachineInstanceMigrationState{Failed: true}),
		Entry("completed", &virtv1.VirtualMachineInstanceMigrationState{Completed: true}),
	)

	It("ignores an unfinished migration for another instance", func() {
		migration := addMigration("other-instance", virtv1.MigrationPending)
		migration.Spec.VMIName = "another-instance"
		_, err := client.KubevirtV1().VirtualMachineInstanceMigrations(vmi.Namespace).Update(ctx, migration, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		persist()
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		Expect(stored().Status.MigratedVolumes).To(BeEmpty())
	})

	DescribeTable("does not clear an instance that is not fully reverted", func(change func()) {
		change()
		persist()
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		Expect(stored().Status.MigratedVolumes).To(Equal(vmi.Status.MigratedVolumes))
	},
		Entry("the VM still requests the target", func() { vm.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "old-target-root" }),
		Entry("one VMI disk still uses the target", func() { vmi.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "old-target-root" }),
		Entry("one disk has no source record", func() { vmi.Status.MigratedVolumes[0].SourcePVCInfo = nil }),
		Entry("the VM no longer has one of the disks", func() { vm.Spec.Template.Spec.Volumes = vm.Spec.Template.Spec.Volumes[1:] }),
		Entry("the running disk status still refers to the target", func() { vmi.Status.VolumeStatus[0].PersistentVolumeClaimInfo.ClaimName = "old-target-root" }),
		Entry("the running disk status is missing", func() { vmi.Status.VolumeStatus = nil }),
		Entry("the VMI is being deleted", func() { vmi.DeletionTimestamp = pointer.P(metav1.Now()) }),
		Entry("the VMI is not running", func() { vmi.Status.Phase = virtv1.Scheduling }),
		Entry("the VM is being deleted", func() { vm.DeletionTimestamp = pointer.P(metav1.Now()) }),
		Entry("the VMI has an unfinished migration state", func() {
			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{MigrationUID: "active"}
		}),
	)

	It("fails closed when the migration list cannot be read", func() {
		persist()
		client.PrependReactor("list", "virtualmachineinstancemigrations", func(testing.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("migration list unavailable")
		})
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(MatchError(ContainSubstring("migration list unavailable")))
		Expect(stored()).To(Equal(vmi))
	})

	It("rejects cleanup when the VMI changes after it was read", func() {
		persist()
		newer := vmi.DeepCopy()
		newer.ResourceVersion = "11"
		newer.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{MigrationUID: "new-migration"}
		_, err := client.KubevirtV1().VirtualMachineInstances(vmi.Namespace).Update(ctx, newer, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).NotTo(Succeed())
		Expect(stored()).To(Equal(newer))
	})

	DescribeTable("keeps recovery records without proof that the source claims are mounted", func(change func()) {
		change()
		_, err := coreClient.CoreV1().Pods(vmi.Namespace).Update(ctx, launcher, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		persist()
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		Expect(stored()).To(Equal(vmi))
	},
		Entry("the launcher uses the target despite reverted VM and VMI metadata", func() { launcher.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "old-target-root" }),
		Entry("one source claim is not mounted", func() { launcher.Spec.Volumes = launcher.Spec.Volumes[:1] }),
		Entry("the launcher runs on a different node", func() { launcher.Spec.NodeName = "target-node" }),
		Entry("the launcher has failed", func() { launcher.Status.Phase = k8sv1.PodFailed }),
		Entry("the launcher is terminating", func() { launcher.DeletionTimestamp = pointer.P(metav1.Now()) }),
	)

	DescribeTable("checks whether old target pods can still use the destination", func(phase k8sv1.PodPhase, recover bool) {
		target := launcher.DeepCopy()
		target.Name = "old-target-launcher"
		target.UID = "old-target-uid"
		target.Spec.NodeName = "target-node"
		target.Status.Phase = phase
		target.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "old-target-root"
		_, err := coreClient.CoreV1().Pods(vmi.Namespace).Create(ctx, target, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		persist()
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		if recover {
			Expect(stored().Status.MigratedVolumes).To(BeEmpty())
		} else {
			Expect(stored()).To(Equal(vmi))
		}
	},
		Entry("running target holds the claim", k8sv1.PodRunning, false),
		Entry("pending target holds the claim", k8sv1.PodPending, false),
		Entry("failed target no longer holds the claim", k8sv1.PodFailed, true),
		Entry("completed target no longer holds the claim", k8sv1.PodSucceeded, true),
	)

	DescribeTable("verifies hotplug claims against attachment pods", func(claim string, ownedByCurrentLauncher bool, recover bool) {
		attachment := &k8sv1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "attachment", Namespace: vmi.Namespace, UID: "attachment-uid",
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: launcher.Name, UID: launcher.UID, Controller: pointer.P(true)}}},
			Spec: k8sv1.PodSpec{NodeName: vmi.Status.NodeName, Volumes: []k8sv1.Volume{{Name: "data",
				VolumeSource: k8sv1.VolumeSource{PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}}},
			Status: k8sv1.PodStatus{Phase: k8sv1.PodRunning},
		}
		if !ownedByCurrentLauncher {
			attachment.OwnerReferences[0].UID = "unrelated-launcher"
		}
		_, err := coreClient.CoreV1().Pods(vmi.Namespace).Create(ctx, attachment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		launcher.Spec.Volumes = launcher.Spec.Volumes[:1]
		_, err = coreClient.CoreV1().Pods(vmi.Namespace).Update(ctx, launcher, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		persist()
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(Succeed())
		if recover {
			Expect(stored().Status.MigratedVolumes).To(BeEmpty())
		} else {
			Expect(stored()).To(Equal(vmi))
		}
	},
		Entry("the current launcher owns the source attachment", "source-data", true, true),
		Entry("an unrelated launcher owns the source attachment", "source-data", false, false),
		Entry("the current attachment still uses the target", "old-target-data", true, false),
	)

	It("fails closed when the pods cannot be read", func() {
		persist()
		coreClient.PrependReactor("list", "pods", func(testing.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("pod list unavailable")
		})
		Expect(c.handleVolumeUpdateRequest(vm, vmi)).To(MatchError(ContainSubstring("pod list unavailable")))
		Expect(stored()).To(Equal(vmi))
	})
})
