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
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	v1 "kubevirt.io/api/core/v1"
	api2 "kubevirt.io/client-go/api"
	"kubevirt.io/client-go/kubecli"
	kubevirtfake "kubevirt.io/client-go/kubevirt/fake"

	virtcontroller "kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/libvmi"
	libvmistatus "kubevirt.io/kubevirt/pkg/libvmi/status"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/testutils"
	virtcache "kubevirt.io/kubevirt/pkg/virt-handler/cache"
	cmdclient "kubevirt.io/kubevirt/pkg/virt-handler/cmd-client"
	container_disk "kubevirt.io/kubevirt/pkg/virt-handler/container-disk"
	hotplugvolume "kubevirt.io/kubevirt/pkg/virt-handler/hotplug-disk"
	launcher_clients "kubevirt.io/kubevirt/pkg/virt-handler/launcher-clients"
	migrationproxy "kubevirt.io/kubevirt/pkg/virt-handler/migration-proxy"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

// Kept separate from the legacy migration fixture files excluded by hack/test-unit.sh.
var _ = Describe("Decentralized lifecycle regression", func() {
	DescribeTable("does not issue launcher commands after migration completion", func(failed bool) {
		vmi := api2.NewMinimalVMI("source")
		vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
			Completed: true, Failed: failed, EndTimestamp: pointer.P(metav1.Now()),
		}
		domain := api.NewMinimalDomain("source")
		domain.Status.Status = api.Running
		c := &MigrationSourceController{launcherClients: &launcher_clients.MockLauncherClientManager{
			Initialized: true, Error: errors.New("completed migration accessed its launcher"),
		}}
		Expect(c.processVMI(vmi, domain)).To(Succeed())
	},
		Entry("after success", false),
		Entry("after failure", true),
	)

	DescribeTable("recognizes the migration through the whole address exchange", func(source, target bool) {
		vmi := api2.NewMinimalVMI("source")
		st := &v1.VirtualMachineInstanceMigrationState{
			SourceState: &v1.VirtualMachineInstanceMigrationSourceState{},
			TargetState: &v1.VirtualMachineInstanceMigrationTargetState{},
		}
		if source {
			st.SourceState.SyncAddress = pointer.P("source:9185")
		}
		if target {
			st.TargetState.SyncAddress = pointer.P("target:9185")
		}
		vmi.Status.MigrationState = st
		Expect(vmi.IsDecentralizedMigration()).To(Equal(source || target))
	},
		Entry("before synchronization", false, false),
		Entry("with only the source address", true, false),
		Entry("with only the target address", false, true),
		Entry("with both addresses", true, true),
	)

	DescribeTable("finalizes source phase only after the domain migrated", func(failed bool) {
		now := metav1.Now()
		vmi := api2.NewMinimalVMI("source")
		vmi.Status.Phase = v1.Running
		vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
			SourceNode: "source-node", TargetNode: "target-node", Completed: true, Failed: failed, EndTimestamp: &now,
			SourceState: &v1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{SyncAddress: pointer.P("source:9185")},
			},
			TargetState: &v1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{SyncAddress: pointer.P("target:9185")},
				DomainDetected: true, DomainReadyTimestamp: &now,
			},
		}
		domain := api.NewMinimalDomain("source")
		domain.Status.Status = api.Running
		c := &MigrationSourceController{BaseController: &BaseController{host: "source-node"}, recorder: record.NewFakeRecorder(1)}
		Expect(c.updateStatus(vmi, domain)).To(Succeed())
		Expect(vmi.Status.Phase).To(Equal(v1.Running))
		domain.Status.Status = api.Shutoff
		domain.Status.Reason = api.ReasonMigrated
		Expect(c.updateStatus(vmi, domain)).To(Succeed())
		if failed {
			Expect(vmi.Status.Phase).To(Equal(v1.Failed))
		} else {
			Expect(vmi.Status.Phase).To(Equal(v1.Succeeded))
		}
	},
		Entry("after success", false),
		Entry("after failure following switchover", true),
	)
	It("does not overwrite peer failure with older source domain metadata", func() {
		now := metav1.Now()
		vmi := api2.NewMinimalVMI("source")
		vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
			MigrationUID: "current", SourceNode: "source-node", Completed: true, Failed: true, EndTimestamp: &now,
			SourceState: &v1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{SyncAddress: pointer.P("source:9185")},
			},
			TargetState: &v1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{SyncAddress: pointer.P("target:9185")},
				NodeAddress: pointer.P("target"),
			},
		}
		domain := api.NewMinimalDomain("source")
		domain.Spec.Metadata.KubeVirt.Migration = &api.MigrationMetadata{UID: "current", StartTimestamp: &now}
		c := &MigrationSourceController{BaseController: &BaseController{host: "source-node"}}
		c.setMigrationProgressStatus(vmi, domain)
		Expect(vmi.Status.MigrationState.Failed).To(BeTrue())
		Expect(vmi.Status.MigrationState.EndTimestamp).To(Equal(&now))
	})

	It("stops both proxies while retaining the final synchronization state", func() {
		vmi := api2.NewMinimalVMI("target")
		vmi.UID = "local-vmi-uid"
		vmi.Annotations = map[string]string{v1.CreateMigrationTarget: "true"}
		vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
			Completed: true, EndTimestamp: pointer.P(metav1.Now()),
			SourceState: &v1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{
					VirtualMachineInstanceUID: pointer.P(types.UID("remote-vmi-uid")),
				},
			},
			TargetState: &v1.VirtualMachineInstanceMigrationTargetState{},
		}
		mocks := gomock.NewController(GinkgoT())
		launcher := cmdclient.NewMockLauncherClient(mocks)
		launcher.EXPECT().FinalizeVirtualMachineMigration(vmi, gomock.Any()).Return(nil)
		objects := kubevirtfake.NewSimpleClientset(vmi.DeepCopy())
		client := kubecli.NewMockKubevirtClient(mocks)
		client.EXPECT().VirtualMachineInstance(vmi.Namespace).Return(objects.KubevirtV1().VirtualMachineInstances(vmi.Namespace))
		config, _, _ := testutils.NewFakeClusterConfigUsingKVConfig(&v1.KubeVirtConfiguration{})
		proxy := &stoppedTargetListeners{}
		c := &MigrationTargetController{
			BaseController: &BaseController{clusterConfig: config}, clientset: client,
			launcherClients: &launcher_clients.MockLauncherClientManager{Client: launcher}, migrationProxy: proxy,
		}
		Expect(c.finalCleanup(vmi, vmi.Status.DeepCopy(), vmi.Labels, nil)).To(Succeed())
		Expect(proxy.keys).To(ConsistOf("local-vmi-uid", "remote-vmi-uid"))
		actual, err := objects.KubevirtV1().VirtualMachineInstances(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(actual.Status.MigrationState.SourceState).NotTo(BeNil())
		Expect(actual.Status.MigrationState.TargetState).NotTo(BeNil())
	})
	DescribeTable("handles terminal target metadata", func(domainStatus api.LifeCycle, domainMigrationUID types.UID, failed bool) {
		now := metav1.Now()
		vmi := api2.NewMinimalVMI("vm")
		vmi.UID = "vmi-uid"
		vmi.Status.Phase = v1.Scheduled
		vmi.Annotations = map[string]string{v1.CreateMigrationTarget: "true"}
		vmi.Labels = map[string]string{v1.MigrationTargetNodeNameLabel: "target"}
		vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
			MigrationUID: "receive-uid", SourceNode: "source", TargetNode: "target",
			StartTimestamp: &now, TargetNodeDomainDetected: true,
			SourceState: &v1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{MigrationUID: "remote-source-uid"},
			},
		}
		domain := api.NewMinimalDomainWithUUID(vmi.Name, vmi.UID)
		domain.Namespace = vmi.Namespace
		domain.Status.Status = domainStatus
		domain.Spec.Metadata.KubeVirt.Migration = &api.MigrationMetadata{UID: domainMigrationUID, StartTimestamp: &now, EndTimestamp: &now}
		vs, ds := cache.NewStore(cache.MetaNamespaceKeyFunc), cache.NewStore(cache.MetaNamespaceKeyFunc)
		if err := vs.Add(vmi); err != nil {
			Fail(err.Error())
		}
		if err := ds.Add(domain); err != nil {
			Fail(err.Error())
		}
		objects := kubevirtfake.NewSimpleClientset(vmi.DeepCopy())
		mocks := gomock.NewController(GinkgoT())
		client := kubecli.NewMockKubevirtClient(mocks)
		client.EXPECT().VirtualMachineInstance(vmi.Namespace).Return(objects.KubevirtV1().VirtualMachineInstances(vmi.Namespace)).AnyTimes()
		c := &MigrationTargetController{BaseController: &BaseController{host: "target", vmiStore: vs, domainStore: ds}, clientset: client}
		if err := c.execute(vmi.Namespace + "/" + vmi.Name); err != nil {
			Fail(err.Error())
		}
		got, err := objects.KubevirtV1().VirtualMachineInstances(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
		if err != nil {
			Fail(err.Error())
		}
		Expect(got.Status.MigrationState.Failed).To(Equal(failed))
		Expect(got.Status.MigrationState.Completed).To(Equal(failed))
	},
		Entry("shutoff", api.Shutoff, types.UID("receive-uid"), true),
		Entry("crashed", api.Crashed, types.UID("receive-uid"), true),
		Entry("metadata from the remote source", api.Shutoff, types.UID("remote-source-uid"), true),
		Entry("metadata from an older migration", api.Shutoff, types.UID("old-uid"), false),
		Entry("metadata without a migration identity", api.Shutoff, types.UID(""), false),
	)
})

var _ = Describe("Decentralized source lifecycle", func() {
	const host = "master"
	var controller *MigrationSourceController
	var virtfakeClient *kubevirtfake.Clientset
	var recorder *record.FakeRecorder
	var vmiTestUUID types.UID
	BeforeEach(func() {
		vmiTestUUID = "source-vmi-uid"
		recorder = record.NewFakeRecorder(100)
		virtfakeClient = kubevirtfake.NewSimpleClientset()
		mocks := gomock.NewController(GinkgoT())
		client := kubecli.NewMockKubevirtClient(mocks)
		client.EXPECT().VirtualMachineInstance(metav1.NamespaceDefault).
			Return(virtfakeClient.KubevirtV1().VirtualMachineInstances(metav1.NamespaceDefault)).AnyTimes()
		config, _, _ := testutils.NewFakeClusterConfigUsingKVConfig(&v1.KubeVirtConfiguration{})
		launcher := cmdclient.NewMockLauncherClient(mocks)
		launcher.EXPECT().GetDomainStats().Return(nil, false, nil).AnyTimes()
		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
		DeferCleanup(queue.ShutDown)
		controller = &MigrationSourceController{
			BaseController: &BaseController{
				host: host, clusterConfig: config,
				vmiStore: cache.NewStore(cache.MetaNamespaceKeyFunc), domainStore: cache.NewStore(cache.MetaNamespaceKeyFunc),
			},
			clientset: client, recorder: recorder, queue: queue,
			launcherClients: &launcher_clients.MockLauncherClientManager{Client: launcher, Initialized: true},
			migrationProxy:  &lifecycleSourceProxy{}, netStat: &netStatStub{},
			vmiExpectations: virtcontroller.NewUIDTrackingControllerExpectations(virtcontroller.NewControllerExpectations()),
		}
	})
	newDecentralizedSource := func() *v1.VirtualMachineInstance {
		vmi := api2.NewMinimalVMI("source")
		vmi.Status.Phase = v1.Running
		vmi.Status.NodeName = host
		vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
			MigrationUID: "1234", SourceNode: host,
			SourceState: &v1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{
					SyncAddress: pointer.P("source:9185"),
				},
			},
			TargetState: &v1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{
					SyncAddress: pointer.P("target:9185"),
				},
				NodeAddress: pointer.P("target"),
			},
		}
		return vmi
	}
	newDomain := func(end *metav1.Time) *api.Domain {
		domain := api.NewMinimalDomainWithUUID("source", "1234")
		domain.Spec.Metadata.KubeVirt.Migration = &api.MigrationMetadata{UID: "1234", EndTimestamp: end}
		if end != nil {
			domain.Status.Status = api.Shutoff
			domain.Status.Reason = api.ReasonMigrated
		}
		return domain
	}
	It("fails the migration and the VMI when the source virt-launcher is gone", func() {
		vmi := newDecentralizedSource()
		vmi.Status.MigrationState.StartTimestamp = pointer.P(metav1.Now())
		controller.launcherClients = &launcher_clients.MockLauncherClientManager{
			Error: fmt.Errorf("No command socket found for vmi %s: %w", vmi.UID, os.ErrNotExist),
		}
		Expect(controller.updateStatus(vmi, nil)).To(Succeed())
		Expect(vmi.Status.Phase).To(Equal(v1.Failed))
		Expect(vmi.Status.MigrationState.Completed).To(BeTrue())
		Expect(vmi.Status.MigrationState.Failed).To(BeTrue())
		Expect(vmi.Status.MigrationState.EndTimestamp).ToNot(BeNil())
		Expect(vmi.Status.MigrationState.FailureReason).To(Equal("the source virt-launcher is gone"))
		testutils.ExpectEvent(recorder, v1.Migrated.String())
	})
	DescribeTable("finishes a source whose cached launcher stopped responding", func(unresponsive, initialized, domainExists, failed bool) {
		vmi := newDecentralizedSource()
		vmi.Status.MigrationState.StartTimestamp = pointer.P(metav1.Now())
		manager := controller.launcherClients.(*launcher_clients.MockLauncherClientManager)
		manager.UnResponsive = unresponsive
		manager.Initialized = initialized
		var domain *api.Domain
		if domainExists {
			domain = newDomain(nil)
		}
		Expect(controller.updateStatus(vmi, domain)).To(Succeed())
		Expect(vmi.Status.MigrationState.Completed).To(Equal(failed))
		if failed {
			Expect(vmi.Status.Phase).To(Equal(v1.Failed))
			testutils.ExpectEvent(recorder, v1.Migrated.String())
		} else {
			Expect(vmi.Status.Phase).To(Equal(v1.Running))
		}
	},
		Entry("watchdog confirmed a dead cached launcher", true, true, false, true),
		Entry("the domain is still observed", true, true, true, false),
		Entry("the launcher is responsive", false, true, false, false),
		Entry("the launcher is still initializing", true, false, false, false),
	)
	It("keeps polling while the domain of the source is still there", func() {
		vmi := newDecentralizedSource()
		controller.launcherClients = &launcher_clients.MockLauncherClientManager{
			Error: fmt.Errorf("No command socket found for vmi %s: %w", vmi.UID, os.ErrNotExist),
		}
		Expect(controller.updateStatus(vmi, newDomain(nil))).To(Succeed())
		Expect(vmi.Status.Phase).To(Equal(v1.Running))
		Expect(vmi.Status.MigrationState.Failed).To(BeFalse())
	})
	Context("decentralized migration completion", func() {
		It("marks the source VMI Succeeded when both SyncAddresses are set after domain migration", func() {
			syncAddress := "10.0.0.1:9185"
			now := metav1.Now()
			vmi := api2.NewMinimalVMI("testvmi")
			vmi.UID = vmiTestUUID
			vmi.Status.Phase = v1.Running
			vmi.Status.NodeName = host
			vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
				TargetNode:   "othernode",
				SourceNode:   host,
				Completed:    true,
				EndTimestamp: &now,
				SourceState: &v1.VirtualMachineInstanceMigrationSourceState{
					VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{
						Node:         host,
						SyncAddress:  &syncAddress,
						MigrationUID: "source-migration-uid",
					},
				},
				TargetState: &v1.VirtualMachineInstanceMigrationTargetState{
					VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{
						Node:         "othernode",
						SyncAddress:  &syncAddress,
						MigrationUID: "target-migration-uid",
					},
					DomainDetected:       true,
					DomainReadyTimestamp: &now,
				},
			}

			Expect(vmi.IsDecentralizedMigration()).To(BeTrue(),
				"fully synced decentralized migrations must still be recognized when both SyncAddresses are set")

			domain := api.NewMinimalDomainWithUUID("testvmi", vmiTestUUID)
			domain.Status.Status = api.Shutoff
			domain.Status.Reason = api.ReasonMigrated

			Expect(controller.updateStatus(vmi, domain)).To(Succeed())
			Expect(vmi.Status.Phase).To(Equal(v1.Succeeded))
		})
	})
	DescribeTable("finishes the source phase after completion arrived before the migrated domain",
		func(sourceNode string, expected v1.VirtualMachineInstancePhase) {
			syncAddress := "10.0.0.1:9185"
			now := metav1.Now()
			vmi := api2.NewMinimalVMI("testvmi")
			vmi.UID = vmiTestUUID
			vmi.Status.Phase = v1.Running
			vmi.Status.NodeName = host
			vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
				TargetNode:   "othernode",
				SourceNode:   sourceNode,
				Completed:    true,
				EndTimestamp: &now,
				SourceState: &v1.VirtualMachineInstanceMigrationSourceState{
					VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{
						Node:         host,
						SyncAddress:  &syncAddress,
						MigrationUID: "source-migration-uid",
					},
				},
				TargetState: &v1.VirtualMachineInstanceMigrationTargetState{
					VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{
						Node:         "othernode",
						SyncAddress:  &syncAddress,
						MigrationUID: "target-migration-uid",
					},
					DomainDetected:       true,
					DomainReadyTimestamp: &now,
				},
			}

			Expect(vmi.IsDecentralizedMigration()).To(BeTrue(),
				"fully synced decentralized migrations must still be recognized when both SyncAddresses are set")

			domain := api.NewMinimalDomainWithUUID("testvmi", vmiTestUUID)
			domain.Status.Status = api.Shutoff
			domain.Status.Reason = api.ReasonMigrated

			Expect(controller.vmiStore.Add(vmi)).To(Succeed())
			Expect(controller.domainStore.Add(domain)).To(Succeed())
			_, err := virtfakeClient.KubevirtV1().VirtualMachineInstances(vmi.Namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.execute(virtcontroller.VirtualMachineInstanceKey(vmi))).To(Succeed())
			got, err := virtfakeClient.KubevirtV1().VirtualMachineInstances(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Status.Phase).To(Equal(expected))
		},
		Entry("on the source node", host, v1.Succeeded),
		Entry("on another node", "another-node", v1.Running),
	)
	Context("failPostMigration", func() {
		DescribeTable("should mark the VMI as failed",
			func(migrationState *v1.VirtualMachineInstanceMigrationState, expectMigrationStateUpdated bool) {
				vmi := libvmi.New(libvmistatus.WithStatus(libvmistatus.New(
					libvmistatus.WithPhase(v1.Running),
				)))
				vmi.Status.MigrationState = migrationState

				Expect(func() { controller.failPostMigration(vmi) }).ToNot(Panic())
				Expect(vmi.Status.Phase).To(Equal(v1.Failed))

				if !expectMigrationStateUpdated {
					Expect(vmi.Status.MigrationState).To(BeNil())
					return
				}

				Expect(vmi.Status.MigrationState).ToNot(BeNil())
				Expect(vmi.Status.MigrationState.Completed).To(BeTrue())
				Expect(vmi.Status.MigrationState.Failed).To(BeTrue())
				Expect(vmi.Status.MigrationState.EndTimestamp).ToNot(BeNil())
			},
			Entry("when MigrationState is nil", nil, false),
			Entry("when MigrationState is present", &v1.VirtualMachineInstanceMigrationState{}, true),
		)

		It("should preserve an existing EndTimestamp", func() {
			existingEnd := metav1.NewTime(time.Now().Add(-time.Minute))
			vmi := libvmi.New(libvmistatus.WithStatus(libvmistatus.New(
				libvmistatus.WithPhase(v1.Running),
				libvmistatus.WithMigrationState(v1.VirtualMachineInstanceMigrationState{
					EndTimestamp: &existingEnd,
				}),
			)))

			controller.failPostMigration(vmi)

			Expect(vmi.Status.MigrationState.EndTimestamp).To(Equal(&existingEnd))
			Expect(vmi.Status.MigrationState.Completed).To(BeTrue())
			Expect(vmi.Status.MigrationState.Failed).To(BeTrue())
		})
	})
	Context("updateStatus after domain migration", func() {
		migratedDomain := func() *api.Domain {
			d := api.NewMinimalDomain("testvmi")
			d.Status.Status = api.Shutoff
			d.Status.Reason = api.ReasonMigrated
			return d
		}

		It("should not finalize handoff while the domain is still running", func() {
			vmi := libvmi.New(libvmistatus.WithStatus(libvmistatus.New(
				libvmistatus.WithPhase(v1.Running),
				libvmistatus.WithMigrationState(v1.VirtualMachineInstanceMigrationState{
					SourceNode: host,
				}),
			)))
			originalStatus := vmi.Status.DeepCopy()

			runningDomain := api.NewMinimalDomain("testvmi")
			runningDomain.Status.Status = api.Running

			Expect(controller.updateStatus(vmi, runningDomain)).To(Succeed())
			Expect(vmi.Status).To(Equal(*originalStatus))
		})

		It("should not panic and should fail the VMI when MigrationState is nil", func() {
			vmi := libvmi.New(libvmistatus.WithStatus(libvmistatus.New(
				libvmistatus.WithPhase(v1.Running),
			)))

			Expect(func() {
				Expect(controller.updateStatus(vmi, migratedDomain())).To(Succeed())
			}).ToNot(Panic())

			Expect(vmi.Status.Phase).To(Equal(v1.Failed))
			Expect(vmi.Status.MigrationState).To(BeNil())
			testutils.ExpectEvent(recorder, v1.Migrated.String())
		})

		It("should fail the VMI when TargetNode is empty", func() {
			vmi := libvmi.New(libvmistatus.WithStatus(libvmistatus.New(
				libvmistatus.WithPhase(v1.Running),
				libvmistatus.WithMigrationState(v1.VirtualMachineInstanceMigrationState{}),
			)))

			Expect(controller.updateStatus(vmi, migratedDomain())).To(Succeed())

			Expect(vmi.Status.Phase).To(Equal(v1.Failed))
			Expect(vmi.Status.MigrationState.Completed).To(BeTrue())
			Expect(vmi.Status.MigrationState.Failed).To(BeTrue())
			Expect(vmi.Status.MigrationState.EndTimestamp).ToNot(BeNil())
			testutils.ExpectEvent(recorder, v1.Migrated.String())
		})

		It("should fail the VMI when the target never detects the domain within the timeout", func() {
			oldEnd := metav1.NewTime(time.Now().Add(-2 * time.Minute))
			vmi := libvmi.New(libvmistatus.WithStatus(libvmistatus.New(
				libvmistatus.WithPhase(v1.Running),
				libvmistatus.WithMigrationState(v1.VirtualMachineInstanceMigrationState{
					TargetNode:   "othernode",
					EndTimestamp: &oldEnd,
				}),
			)))

			Expect(controller.updateStatus(vmi, migratedDomain())).To(Succeed())

			Expect(vmi.Status.Phase).To(Equal(v1.Failed))
			Expect(vmi.Status.MigrationState.Completed).To(BeTrue())
			Expect(vmi.Status.MigrationState.Failed).To(BeTrue())
			Expect(vmi.Status.MigrationState.EndTimestamp).To(Equal(&oldEnd))
			testutils.ExpectEvent(recorder, v1.Migrated.String())
		})

		It("should not fail the VMI when the target has detected the domain", func() {
			now := metav1.Now()
			vmi := libvmi.New(libvmistatus.WithStatus(libvmistatus.New(
				libvmistatus.WithPhase(v1.Running),
				libvmistatus.WithMigrationState(v1.VirtualMachineInstanceMigrationState{
					TargetNode:   "othernode",
					EndTimestamp: &now,
					TargetState: &v1.VirtualMachineInstanceMigrationTargetState{
						DomainDetected:       true,
						DomainReadyTimestamp: &now,
					},
				}),
			)))

			Expect(controller.updateStatus(vmi, migratedDomain())).To(Succeed())

			Expect(vmi.Status.Phase).To(Equal(v1.Running))
			Expect(vmi.Status.MigrationState.Failed).To(BeFalse())
			Expect(vmi.Status.MigrationState.Completed).To(BeFalse())
		})
	})
})

var _ = Describe("IsDecentralizedMigration", func() {
	DescribeTable("detects decentralized migrations for the full sync lifetime",
		func(sourceSync, targetSync *string, expected bool) {
			vmi := api2.NewMinimalVMI("testvmi")
			vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
				SourceState: &v1.VirtualMachineInstanceMigrationSourceState{
					VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{
						SyncAddress: sourceSync,
					},
				},
				TargetState: &v1.VirtualMachineInstanceMigrationTargetState{
					VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{
						SyncAddress: targetSync,
					},
				},
			}
			Expect(vmi.IsDecentralizedMigration()).To(Equal(expected))
		},
		Entry("source-only SyncAddress (pre-sync source)", pointer.P("10.0.0.1:9185"), nil, true),
		Entry("target-only SyncAddress (pre-sync target)", nil, pointer.P("10.0.0.2:9185"), true),
		Entry("both SyncAddresses set (post-sync)", pointer.P("10.0.0.1:9185"), pointer.P("10.0.0.1:9185"), true),
		Entry("neither SyncAddress set", nil, nil, false),
	)

	It("is false when SourceState or TargetState is missing", func() {
		vmi := api2.NewMinimalVMI("testvmi")
		vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
			TargetState: &v1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{
					SyncAddress: pointer.P("10.0.0.1:9185"),
				},
			},
		}
		Expect(vmi.IsDecentralizedMigration()).To(BeFalse())
	})
})

type lifecycleSourceProxy struct{ migrationproxy.ProxyManager }

func (*lifecycleSourceProxy) StopSourceListener(string) {}

var _ = Describe("Decentralized target lifecycle", func() {
	const host = "master"
	var controller *MigrationTargetController
	var virtfakeClient *kubevirtfake.Clientset
	var client *cmdclient.MockLauncherClient
	var mockHotplugVolumeMounter *hotplugvolume.MockVolumeMounter
	var mockHotplugCDMounter *container_disk.MockHotplugMounter
	var vmiTestUUID, podTestUUID types.UID
	BeforeEach(func() {
		vmiTestUUID, podTestUUID = "target-vmi-uid", "target-pod-uid"
		mocks := gomock.NewController(GinkgoT())
		virtfakeClient = kubevirtfake.NewSimpleClientset()
		kvClient := kubecli.NewMockKubevirtClient(mocks)
		kvClient.EXPECT().VirtualMachineInstance(metav1.NamespaceDefault).
			Return(virtfakeClient.KubevirtV1().VirtualMachineInstances(metav1.NamespaceDefault)).AnyTimes()
		config, _, _ := testutils.NewFakeClusterConfigUsingKVConfig(&v1.KubeVirtConfiguration{})
		client = cmdclient.NewMockLauncherClient(mocks)
		disks := container_disk.NewMockMounter(mocks)
		disks.EXPECT().Unmount(gomock.Any()).Return(nil).AnyTimes()
		mockHotplugVolumeMounter = hotplugvolume.NewMockVolumeMounter(mocks)
		mockHotplugCDMounter = container_disk.NewMockHotplugMounter(mocks)
		cmdclient.SetPodsBaseDir(GinkgoT().TempDir())
		_ = virtcache.InitializeGhostRecordCache(virtcache.NewIterableCheckpointManager(GinkgoT().TempDir()))
		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
		DeferCleanup(queue.ShutDown)
		controller = &MigrationTargetController{
			queue: queue,
			BaseController: &BaseController{
				host: host, clusterConfig: config,
				vmiStore: cache.NewStore(cache.MetaNamespaceKeyFunc), domainStore: cache.NewStore(cache.MetaNamespaceKeyFunc),
			},
			clientset: kvClient, recorder: record.NewFakeRecorder(100), migrationProxy: &lifecycleTargetProxy{},
			launcherClients:      &launcher_clients.MockLauncherClientManager{Client: client, Initialized: true},
			containerDiskMounter: disks, hotplugVolumeMounter: mockHotplugVolumeMounter, hotplugContainerDiskMounter: mockHotplugCDMounter,
			netConf: &netConfStub{}, netStat: &netStatStub{},
		}
	})
	createVMI := func(vmi *v1.VirtualMachineInstance) {
		Expect(controller.vmiStore.Add(vmi)).To(Succeed())
		_, err := virtfakeClient.KubevirtV1().VirtualMachineInstances(vmi.Namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}
	sanityExecute := func() { Expect(controller.execute(metav1.NamespaceDefault + "/testvmi")).To(Succeed()) }
	It("does not transfer ownership to a target whose QEMU migration was aborted", func() {
		now := metav1.Now()
		vmi := api2.NewMinimalVMI("testvmi")
		vmi.UID = vmiTestUUID
		vmi.Status.NodeName = "source-node"
		vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
			MigrationUID: "migration-uid", SourceNode: "source-node", TargetNode: host,
			TargetNodeDomainDetected: true,
		}
		domain := api.NewMinimalDomainWithUUID(vmi.Name, vmi.UID)
		domain.Status.Status = api.Shutoff
		domain.Spec.Metadata.KubeVirt.Migration = &api.MigrationMetadata{
			UID: "migration-uid", StartTimestamp: &now, EndTimestamp: &now,
		}
		Expect(controller.updateStatus(vmi, domain)).To(Succeed())
		Expect(vmi.Status.NodeName).To(Equal("source-node"))
		Expect(vmi.Status.MigrationState.Failed).To(BeTrue())
		Expect(vmi.Status.MigrationState.Completed).To(BeTrue())
		Expect(vmi.Status.MigrationState.StartTimestamp).To(Equal(&now))
		Expect(vmi.Status.MigrationState.EndTimestamp).To(Equal(&now))
	})
	It("should preserve CreateMigrationTarget annotation on failed migration cleanup", func() {
		vmi := api2.NewMinimalVMI("testvmi")
		vmi.UID = vmiTestUUID
		vmi.ObjectMeta.ResourceVersion = "1"
		vmi.Status.Phase = v1.Running
		vmi.Labels = make(map[string]string)
		vmi.Annotations = map[string]string{
			v1.CreateMigrationTarget: "true",
		}
		vmi.Status.NodeName = "othernode"
		vmi.Labels[v1.MigrationTargetNodeNameLabel] = host
		vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
			TargetNode:   host,
			SourceNode:   "othernode",
			MigrationUID: "123",
			Failed:       true,
			EndTimestamp: pointer.P(metav1.Now()),
		}
		vmi.Status.MigrationState.SourceState = &v1.VirtualMachineInstanceMigrationSourceState{}
		vmi.Status.MigrationState.TargetState = &v1.VirtualMachineInstanceMigrationTargetState{}
		vmi = addActivePods(vmi, podTestUUID, host)

		createVMI(vmi)

		client.EXPECT().SignalTargetPodCleanup(vmi)
		mockHotplugVolumeMounter.EXPECT().UnmountAll(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockHotplugCDMounter.EXPECT().UmountAll(gomock.Any()).Return(nil)
		sanityExecute()

		updatedVMI, err := virtfakeClient.KubevirtV1().VirtualMachineInstances(metav1.NamespaceDefault).Get(
			context.TODO(), vmi.Name, metav1.GetOptions{},
		)
		Expect(err).ToNot(HaveOccurred())
		Expect(updatedVMI.Labels).To(Not(HaveKey(v1.MigrationTargetNodeNameLabel)))
		Expect(updatedVMI.Annotations).To(HaveKey(v1.CreateMigrationTarget))
		Expect(updatedVMI.Status.MigrationState.SourceState).NotTo(BeNil())
		Expect(updatedVMI.Status.MigrationState.TargetState).NotTo(BeNil())
	})
	It("should remove CreateMigrationTarget annotation on successful migration cleanup", func() {
		vmi := api2.NewMinimalVMI("testvmi")
		vmi.UID = vmiTestUUID
		vmi.ObjectMeta.ResourceVersion = "1"
		vmi.Status.Phase = v1.Running
		vmi.Labels = make(map[string]string)
		vmi.Annotations = map[string]string{
			v1.CreateMigrationTarget: "true",
		}
		vmi.Status.NodeName = host
		vmi.Labels[v1.MigrationTargetNodeNameLabel] = host
		pastTime := metav1.NewTime(metav1.Now().Add(time.Duration(-10) * time.Second))
		vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
			TargetNode:               host,
			TargetNodeAddress:        "127.0.0.1:12345",
			SourceNode:               "othernode",
			MigrationUID:             "123",
			TargetNodeDomainDetected: true,
			StartTimestamp:           &pastTime,
			EndTimestamp:             pointer.P(metav1.Now()),
			Completed:                true,
		}
		vmi.Status.MigrationState.SourceState = &v1.VirtualMachineInstanceMigrationSourceState{}
		vmi.Status.MigrationState.TargetState = &v1.VirtualMachineInstanceMigrationTargetState{}
		vmi = addActivePods(vmi, podTestUUID, host)

		createVMI(vmi)

		client.EXPECT().FinalizeVirtualMachineMigration(vmi, gomock.Any())
		sanityExecute()

		updatedVMI, err := virtfakeClient.KubevirtV1().VirtualMachineInstances(metav1.NamespaceDefault).Get(
			context.TODO(), vmi.Name, metav1.GetOptions{},
		)
		Expect(err).ToNot(HaveOccurred())
		Expect(updatedVMI.Labels).To(Not(HaveKey(v1.MigrationTargetNodeNameLabel)))
		Expect(updatedVMI.Annotations).To(Not(HaveKey(v1.CreateMigrationTarget)))
		Expect(updatedVMI.Status.MigrationState.SourceState).NotTo(BeNil())
		Expect(updatedVMI.Status.MigrationState.TargetState).NotTo(BeNil())
	})
})

var _ = Describe("Decentralized source hotplug cleanup", func() {
	DescribeTable("cleans up a deleting source after its domain is marked gone",
		func(domainUID types.UID, domainDeleted, vmiDeleting, migrationDone, expectCleanup bool) {
			const host = "source-node"
			const sourceUID types.UID = "source-vmi-uid"
			now := metav1.Now()
			vmi := api2.NewMinimalVMI("source")
			vmi.UID = sourceUID
			vmi.Labels = map[string]string{v1.NodeNameLabel: host}
			vmi.Status.NodeName = host
			vmi.Status.Phase = v1.Running
			if vmiDeleting {
				vmi.DeletionTimestamp = &now
			}
			vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
				SourceNode: host,
				Completed:  migrationDone,
				Failed:     migrationDone,
				TargetState: &v1.VirtualMachineInstanceMigrationTargetState{
					VirtualMachineInstanceCommonMigrationState: v1.VirtualMachineInstanceCommonMigrationState{
						SyncAddress: pointer.P("target:9185"),
					},
					NodeAddress: pointer.P("target"),
				},
			}
			Expect(vmi.IsMigrationSource()).To(BeTrue())

			mocks := gomock.NewController(GinkgoT())
			objects := kubevirtfake.NewSimpleClientset(vmi.DeepCopy())
			client := kubecli.NewMockKubevirtClient(mocks)
			client.EXPECT().CoreV1().Return(fake.NewSimpleClientset().CoreV1()).AnyTimes()
			client.EXPECT().VirtualMachineInstance(vmi.Namespace).
				Return(objects.KubevirtV1().VirtualMachineInstances(vmi.Namespace)).AnyTimes()
			config, _, _ := testutils.NewFakeClusterConfigUsingKVConfig(&v1.KubeVirtConfiguration{})
			vmiInformer, _ := testutils.NewFakeInformerFor(&v1.VirtualMachineInstance{})
			domainInformer, _ := testutils.NewFakeInformerFor(&api.Domain{})
			Expect(vmiInformer.GetStore().Add(vmi)).To(Succeed())
			if domainUID != "" {
				domain := api.NewMinimalDomainWithUUID(vmi.Name, domainUID)
				domain.Status.Status = api.Running
				if domainDeleted {
					domain.DeletionTimestamp = &now
				}
				Expect(domainInformer.GetStore().Add(domain)).To(Succeed())
			}

			portRange, err := migrationproxy.NewPortRange(49152, 49215)
			Expect(err).NotTo(HaveOccurred())
			proxy := migrationproxy.NewMigrationProxyManager(portRange, nil, nil, config)
			recorder := record.NewFakeRecorder(10)
			virtcache.InitializeGhostRecordCache(virtcache.NewIterableCheckpointManager(GinkgoT().TempDir()))
			c, err := NewVirtualMachineController(recorder, client, host, GinkgoT().TempDir(), GinkgoT().TempDir(),
				&launcher_clients.MockLauncherClientManager{Error: os.ErrNotExist}, vmiInformer, vmiInformer.GetStore(),
				domainInformer, 10, config, nil, proxy, newFakeManager(), nil, "", &netConfStub{}, &netStatStub{}, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(c.queue.ShutDown)

			volumes := hotplugvolume.NewMockVolumeMounter(mocks)
			c.hotplugVolumeMounter = volumes
			if expectCleanup {
				volumes.EXPECT().UnmountAll(vmi, gomock.Any()).Return(nil)
			}

			Expect(c.execute(virtcontroller.VirtualMachineInstanceKey(vmi))).To(Succeed())
			updated, err := objects.KubevirtV1().VirtualMachineInstances(vmi.Namespace).
				Get(context.Background(), vmi.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			if expectCleanup {
				Expect(updated.Status.Phase).To(Equal(v1.Failed))
			} else {
				Expect(updated.Status.Phase).To(Equal(v1.Running))
			}
		},
		Entry("watchdog marked the domain of the source gone", types.UID("source-vmi-uid"), true, true, true, true),
		Entry("the source VMI was deleted before the migration finished", types.UID("source-vmi-uid"), true, true, false, true),
		Entry("the domain is still running", types.UID("source-vmi-uid"), false, true, true, false),
		Entry("the domain cache has no entry", types.UID(""), false, true, true, false),
		Entry("the domain cache entry is for an older VMI", types.UID("old-vmi-uid"), true, true, true, false),
		Entry("the source VMI is not being deleted", types.UID("source-vmi-uid"), true, false, true, false),
	)
})

var _ = Describe("Decentralized target phase calculation", func() {
	DescribeTable("preserves the receiving lifecycle while its domain is absent", func(
		phase v1.VirtualMachineInstancePhase, failed, unresponsive, target bool, expected v1.VirtualMachineInstancePhase,
	) {
		vmi := api2.NewMinimalVMI("target")
		vmi.Status.Phase = phase
		vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{Failed: failed}
		if target {
			vmi.Annotations = map[string]string{v1.CreateMigrationTarget: "true"}
		}
		controller := &VirtualMachineController{
			launcherClients: &launcher_clients.MockLauncherClientManager{Initialized: true, UnResponsive: unresponsive},
		}
		actual, err := controller.calculateVmPhaseForStatusReason(nil, vmi)
		Expect(err).NotTo(HaveOccurred())
		Expect(actual).To(Equal(expected))
	}, Entry("failed target waiting for synchronization", v1.WaitingForSync, true, false, true, v1.WaitingForSync),
		Entry("scheduled target whose launcher is unresponsive", v1.Scheduled, false, true, true, v1.WaitingForSync),
		Entry("running target whose domain vanished", v1.Running, false, false, true, v1.WaitingForSync),
		Entry("ordinary VMI whose domain vanished", v1.Running, false, false, false, v1.Failed))
})

type lifecycleTargetProxy struct{ stoppedTargetListeners }

func (*lifecycleTargetProxy) GetTargetListenerPorts(string) map[string]int { return nil }
func (*lifecycleTargetProxy) GetTargetBindAddress(string) string           { return "" }
