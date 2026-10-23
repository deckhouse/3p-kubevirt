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

package synchronization

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	kvtls "kubevirt.io/kubevirt/pkg/util/tls"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"

	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	virtv1 "kubevirt.io/api/core/v1"
	clientgoapi "kubevirt.io/client-go/api"
	"kubevirt.io/client-go/kubecli"
	kubevirtfake "kubevirt.io/client-go/kubevirt/fake"

	kvcontroller "kubevirt.io/kubevirt/pkg/controller"
	virtcontroller "kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/pointer"
	syncv1 "kubevirt.io/kubevirt/pkg/synchronizer-com/synchronization/v1"
	"kubevirt.io/kubevirt/pkg/testutils"
)

const (
	testMigrationID         = "testMigrationID"
	testMigrationUID        = "testMigrationUID"
	targetTestMigrationName = "targetMigrationName"
	targetTestMigrationUID  = "targetMigrationUID"
	sourceTestMigrationName = "sourceMigrationName"
	sourceTestMigrationUID  = "sourceMigrationUID"
	uniqueMigration         = "uniqueMigrationID"
	targetNamespace         = "target-namespace"
	sourcePodName           = "sourcePod"
	targetPodName           = "targetPod"

	remoteHostURL = "localhost:9186"
)

var _ = Describe("VMI status synchronization controller", func() {
	var controller *SynchronizationController
	var mockQueue *testutils.MockWorkQueue[string]
	var virtClient *kubecli.MockKubevirtClient
	var virtfakeClient *kubevirtfake.Clientset
	var vmiInformer cache.SharedIndexInformer
	var migrationInformer cache.SharedIndexInformer

	addVMI := func(vmi *virtv1.VirtualMachineInstance) {
		// It doesn't really matter what store does get this
		controller.vmiInformer.GetStore().Add(vmi)
		key, err := virtcontroller.KeyFunc(vmi)
		Expect(err).To(Not(HaveOccurred()))
		controller.queue.Add(key)
	}

	BeforeEach(func() {
		ctrl := gomock.NewController(GinkgoT())
		k8sfakeClient := fake.NewSimpleClientset()
		virtfakeClient = kubevirtfake.NewSimpleClientset()
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		virtClient.EXPECT().CoreV1().Return(k8sfakeClient.CoreV1()).AnyTimes()
		virtClient.EXPECT().VirtualMachineInstance(metav1.NamespaceDefault).Return(virtfakeClient.KubevirtV1().VirtualMachineInstances(metav1.NamespaceDefault)).AnyTimes()
		virtClient.EXPECT().VirtualMachineInstance(targetNamespace).Return(virtfakeClient.KubevirtV1().VirtualMachineInstances(targetNamespace)).AnyTimes()
		virtClient.EXPECT().VirtualMachineInstanceMigration(k8sv1.NamespaceDefault).Return(virtfakeClient.KubevirtV1().VirtualMachineInstanceMigrations(k8sv1.NamespaceDefault)).AnyTimes()
		virtClient.EXPECT().VirtualMachineInstanceMigration(targetNamespace).Return(virtfakeClient.KubevirtV1().VirtualMachineInstanceMigrations(targetNamespace)).AnyTimes()
		vmiInformer, _ = testutils.NewFakeInformerWithIndexersFor(&virtv1.VirtualMachineInstance{}, kvcontroller.GetVMIInformerIndexers())
		migrationInformer, _ = testutils.NewFakeInformerFor(&virtv1.VirtualMachineInstanceMigration{})
		var err error
		controller, err = NewSynchronizationController(virtClient, vmiInformer, migrationInformer, "0.0.0.0", 9185, "")
		Expect(err).ToNot(HaveOccurred())
		mockQueue = testutils.NewMockWorkQueue(controller.queue)
		controller.queue = mockQueue

	})

	Context("controller", func() {
		It("Should not do anything if not migrating", func() {
			vmi := clientgoapi.NewMinimalVMI("testvmi")
			vmi.Status.Phase = virtv1.Running
			addVMI(vmi)
			controller.Execute()
		})
	})

	Context("grpc SyncSourceMigrationStatus", func() {
		var (
			vmi       *virtv1.VirtualMachineInstance
			migration *virtv1.VirtualMachineInstanceMigration
		)
		BeforeEach(func() {
			vmi = libvmi.New(libvmi.WithNamespace(k8sv1.NamespaceDefault))
			err := controller.vmiInformer.GetStore().Add(vmi)
			Expect(err).ToNot(HaveOccurred())
			migration = createTargetMigration(testMigrationID, vmi.Name, k8sv1.NamespaceDefault)
			err = controller.migrationInformer.GetStore().Add(migration)
			Expect(err).ToNot(HaveOccurred())
			vmi, err = controller.client.VirtualMachineInstance(vmi.Namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			foundMig, err := controller.findTargetMigrationFromMigrationID(migration.Spec.Receive.MigrationID)
			Expect(foundMig).To(Equal(migration))
		})

		It("should return proper error if no source migration state passed in", func() {
			request := &syncv1.VMIStatusRequest{
				MigrationID: testMigrationID,
			}
			resp, err := controller.SyncSourceMigrationStatus(migrationPeerContext(testMigrationID), request)
			Expect(err).To(HaveOccurred())
			Expect(resp.Message).To(Equal("must pass source status"))
		})

		It("should return proper error if no source migration can be found", func() {
			request := &syncv1.VMIStatusRequest{
				MigrationID: testMigrationID,
				VmiStatus: &syncv1.VMIStatus{
					VmiStatusJson: []byte{1, 2, 3},
				},
			}
			controller.migrationInformer.GetStore().Delete(migration)
			resp, err := controller.SyncSourceMigrationStatus(migrationPeerContext(testMigrationID), request)
			Expect(err).To(HaveOccurred())
			Expect(resp.Message).To(Equal(fmt.Sprintf(sourceUnableToLocateVMIMigrationIDErrorMsg, testMigrationID)))
		})

		It("should return proper error if no matching vmi for source migration can be found", func() {
			request := &syncv1.VMIStatusRequest{
				MigrationID: testMigrationID,
				VmiStatus: &syncv1.VMIStatus{
					VmiStatusJson: []byte{1, 2, 3},
				},
			}
			controller.vmiInformer.GetStore().Delete(vmi)
			resp, err := controller.SyncSourceMigrationStatus(migrationPeerContext(testMigrationID), request)
			Expect(err).To(HaveOccurred())
			Expect(resp.Message).To(Equal(fmt.Sprintf(sourceUnableToLocateVMIMigrationIDErrorMsgVMI, testMigrationID, fmt.Sprintf("%s/%s", vmi.Namespace, vmi.Name))))
		})

		It("should return a proper error when the source request vmi status json is invalid", func() {
			request := &syncv1.VMIStatusRequest{
				MigrationID: testMigrationID,
				VmiStatus: &syncv1.VMIStatus{
					VmiStatusJson: []byte{1, 2, 3},
				},
			}
			resp, err := controller.SyncSourceMigrationStatus(migrationPeerContext(testMigrationID), request)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid character"))
			Expect(resp.Message).To(ContainSubstring(fmt.Sprintf("unable to unmarshal vmistatus for migrationID %s", request.MigrationID)))
		})

		DescribeTable("should update the VMI if the remote source migration state is different", func(vmiStatus, remoteVMIStatus *virtv1.VirtualMachineInstanceStatus, expectedMessage string, deleteVMI bool) {
			if vmiStatus != nil {
				vmi.Status = *vmiStatus
			}
			vmi, err := controller.client.VirtualMachineInstance(vmi.Namespace).Update(context.Background(), vmi, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())

			if deleteVMI {
				controller.client.VirtualMachineInstance(vmi.Namespace).Delete(context.Background(), vmi.Name, metav1.DeleteOptions{})
			}
			vmiStatusJson, err := json.Marshal(remoteVMIStatus)
			Expect(err).ToNot(HaveOccurred())
			request := &syncv1.VMIStatusRequest{
				MigrationID: testMigrationID,
				VmiStatus: &syncv1.VMIStatus{
					VmiStatusJson: vmiStatusJson,
				},
			}
			_, err = controller.SyncSourceMigrationStatus(migrationPeerContext(testMigrationID), request)
			if expectedMessage != "" {
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(expectedMessage))
			} else {
				Expect(err).ToNot(HaveOccurred())
			}
		},
			Entry("no vmi status and no source status", nil, nil, "must pass source status", false),
			Entry("no vmi status and source status", nil, &virtv1.VirtualMachineInstanceStatus{
				MigrationState: &virtv1.VirtualMachineInstanceMigrationState{
					TargetState: &virtv1.VirtualMachineInstanceMigrationTargetState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node1",
						},
					},
					SourceState: &virtv1.VirtualMachineInstanceMigrationSourceState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node2",
						},
					},
				},
			}, "", false),
			Entry("no vmi status and source status, but vmi not found", nil, &virtv1.VirtualMachineInstanceStatus{
				MigrationState: &virtv1.VirtualMachineInstanceMigrationState{
					TargetState: &virtv1.VirtualMachineInstanceMigrationTargetState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node1",
						},
					},
					SourceState: &virtv1.VirtualMachineInstanceMigrationSourceState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node2",
						},
					},
				},
			}, "not found", true),
			Entry("sources are different", &virtv1.VirtualMachineInstanceStatus{
				MigrationState: &virtv1.VirtualMachineInstanceMigrationState{
					TargetState: &virtv1.VirtualMachineInstanceMigrationTargetState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node1",
						},
					},
					SourceState: &virtv1.VirtualMachineInstanceMigrationSourceState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node3",
						},
					},
				},
			}, &virtv1.VirtualMachineInstanceStatus{
				MigrationState: &virtv1.VirtualMachineInstanceMigrationState{
					TargetState: &virtv1.VirtualMachineInstanceMigrationTargetState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node1",
						},
					},
					SourceState: &virtv1.VirtualMachineInstanceMigrationSourceState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node2",
						},
					},
				},
			}, "", false),
		)
	})

	Context("grpc SyncTargetMigrationStatus", func() {
		var (
			vmi       *virtv1.VirtualMachineInstance
			migration *virtv1.VirtualMachineInstanceMigration
		)
		BeforeEach(func() {
			vmi = libvmi.New(libvmi.WithNamespace(k8sv1.NamespaceDefault))
			err := controller.vmiInformer.GetStore().Add(vmi)
			Expect(err).ToNot(HaveOccurred())
			migration = createSourceMigration(testMigrationID, vmi.Name, "", k8sv1.NamespaceDefault)
			err = controller.migrationInformer.GetStore().Add(migration)
			Expect(err).ToNot(HaveOccurred())
			vmi, err = controller.client.VirtualMachineInstance(vmi.Namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			foundMig, err := controller.findSourceMigrationFromMigrationID(migration.Spec.SendTo.MigrationID)
			Expect(foundMig).To(Equal(migration))
		})

		It("should return proper error if no target migration state passed in", func() {
			request := &syncv1.VMIStatusRequest{
				MigrationID: testMigrationID,
			}
			resp, err := controller.SyncTargetMigrationStatus(migrationPeerContext(testMigrationID), request)
			Expect(err).To(HaveOccurred())
			Expect(resp.Message).To(Equal("must pass target status"))
		})

		It("should return proper error if no target migration can be found", func() {
			request := &syncv1.VMIStatusRequest{
				MigrationID: testMigrationID,
				VmiStatus: &syncv1.VMIStatus{
					VmiStatusJson: []byte{1, 2, 3},
				},
			}
			controller.migrationInformer.GetStore().Delete(migration)
			resp, err := controller.SyncTargetMigrationStatus(migrationPeerContext(testMigrationID), request)
			Expect(err).To(HaveOccurred())
			Expect(resp.Message).To(Equal(fmt.Sprintf(targetUnableToLocateVMIMigrationIDErrorMsg, testMigrationID)))
		})

		It("should return proper error if no matching vmi for target migration can be found", func() {
			request := &syncv1.VMIStatusRequest{
				MigrationID: testMigrationID,
				VmiStatus: &syncv1.VMIStatus{
					VmiStatusJson: []byte{1, 2, 3},
				},
			}
			controller.vmiInformer.GetStore().Delete(vmi)
			resp, err := controller.SyncTargetMigrationStatus(migrationPeerContext(testMigrationID), request)
			Expect(err).To(HaveOccurred())
			Expect(resp.Message).To(Equal(fmt.Sprintf(targetUnableToLocateVMIMigrationIDErrorMsgVMI, testMigrationID, fmt.Sprintf("%s/%s", vmi.Namespace, vmi.Name))))
		})

		It("should return a proper error when the target request vmi status json is invalid", func() {
			request := &syncv1.VMIStatusRequest{
				MigrationID: testMigrationID,
				VmiStatus: &syncv1.VMIStatus{
					VmiStatusJson: []byte{1, 2, 3},
				},
			}
			resp, err := controller.SyncTargetMigrationStatus(migrationPeerContext(testMigrationID), request)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid character"))
			Expect(resp.Message).To(ContainSubstring(fmt.Sprintf("unable to unmarshal vmistatus for migrationID %s", request.MigrationID)))
		})

		DescribeTable("should update the VMI if the remote target migration state is different", func(vmiStatus, remoteVMIStatus *virtv1.VirtualMachineInstanceStatus, expectedMessage string, deleteVMI bool) {
			if vmiStatus != nil {
				vmi.Status = *vmiStatus
			}
			vmi, err := controller.client.VirtualMachineInstance(vmi.Namespace).Update(context.Background(), vmi, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())

			if deleteVMI {
				controller.client.VirtualMachineInstance(vmi.Namespace).Delete(context.Background(), vmi.Name, metav1.DeleteOptions{})
			}
			vmiStatusJson, err := json.Marshal(remoteVMIStatus)
			Expect(err).ToNot(HaveOccurred())
			request := &syncv1.VMIStatusRequest{
				MigrationID: testMigrationID,
				VmiStatus: &syncv1.VMIStatus{
					VmiStatusJson: vmiStatusJson,
				},
			}
			_, err = controller.SyncTargetMigrationStatus(migrationPeerContext(testMigrationID), request)
			if expectedMessage != "" {
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(expectedMessage))
			} else {
				Expect(err).ToNot(HaveOccurred())
			}
		},
			Entry("no vmi status and no remote target status", nil, nil, "must pass target status", false),
			Entry("no vmi status and remote target status", nil, &virtv1.VirtualMachineInstanceStatus{
				MigrationState: &virtv1.VirtualMachineInstanceMigrationState{
					TargetState: &virtv1.VirtualMachineInstanceMigrationTargetState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node1",
						},
					},
					SourceState: &virtv1.VirtualMachineInstanceMigrationSourceState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node2",
						},
					},
				},
			}, "", false),
			Entry("no vmi status and target status, but vmi not found, patch will fail", nil, &virtv1.VirtualMachineInstanceStatus{
				MigrationState: &virtv1.VirtualMachineInstanceMigrationState{
					TargetState: &virtv1.VirtualMachineInstanceMigrationTargetState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node1",
						},
					},
					SourceState: &virtv1.VirtualMachineInstanceMigrationSourceState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node2",
						},
					},
				},
			}, "not found", true),
			Entry("target are different", &virtv1.VirtualMachineInstanceStatus{
				MigrationState: &virtv1.VirtualMachineInstanceMigrationState{
					TargetState: &virtv1.VirtualMachineInstanceMigrationTargetState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node3",
						},
					},
					SourceState: &virtv1.VirtualMachineInstanceMigrationSourceState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node1",
						},
					},
				},
			}, &virtv1.VirtualMachineInstanceStatus{
				MigrationState: &virtv1.VirtualMachineInstanceMigrationState{
					TargetState: &virtv1.VirtualMachineInstanceMigrationTargetState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node2",
						},
					},
					SourceState: &virtv1.VirtualMachineInstanceMigrationSourceState{
						VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
							Node: "node1",
						},
					},
				},
			}, "", false),
		)
	})

	verifySource := func(controller *SynchronizationController, vmi *virtv1.VirtualMachineInstance, url string) {
		updatedSourceVMI, err := controller.client.VirtualMachineInstance(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(updatedSourceVMI).ToNot(BeNil())
		Expect(updatedSourceVMI.Status.MigrationState).ToNot(BeNil())
		Expect(updatedSourceVMI.Status.MigrationState.TargetState).ToNot(BeNil())
		Expect(updatedSourceVMI.Status.MigrationState.TargetState.MigrationUID).To(BeEquivalentTo(targetTestMigrationUID))
	}

	verifyTarget := func(controller *SynchronizationController, vmi *virtv1.VirtualMachineInstance, url string) {
		updatedTargetVMI, err := controller.client.VirtualMachineInstance(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(updatedTargetVMI).ToNot(BeNil())
		Expect(updatedTargetVMI.Status.MigrationState).ToNot(BeNil())
		Expect(updatedTargetVMI.Status.MigrationState.SourceState).ToNot(BeNil())
		Expect(updatedTargetVMI.Status.MigrationState.SourceState.MigrationUID).To(BeEquivalentTo(sourceTestMigrationUID))
	}

	Context("target controller is same as source", func() {
		// Migration from one namespace to another in same cluster
		var (
			err                              error
			targetVMI, sourceVMI             *virtv1.VirtualMachineInstance
			localTCPConn                     net.Listener
			done                             chan struct{}
			sourceMigration, targetMigration *virtv1.VirtualMachineInstanceMigration
		)

		BeforeEach(func() {
			localTCPConn, err = controller.createTcpListener()
			Expect(err).ToNot(HaveOccurred())
			done = make(chan struct{})

			go func() {
				defer close(done)
				controller.grpcServer.Serve(localTCPConn)
			}()
			id, err := controller.identity(testMigrationID)
			Expect(err).ToNot(HaveOccurred())

			targetVMI = libvmi.New(libvmi.WithNamespace(targetNamespace))
			targetVMI.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{}
			err = vmiInformer.GetStore().Add(targetVMI)
			Expect(err).ToNot(HaveOccurred())
			_, err = virtClient.VirtualMachineInstance(targetVMI.Namespace).Create(context.TODO(), targetVMI, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			targetMigration = createTargetMigration(testMigrationID, targetVMI.Name, targetVMI.Namespace)
			targetMigration.Annotations = map[string]string{virtv1.MigrationPeerSyncPinAnnotation: id.Pin}
			err = migrationInformer.GetStore().Add(targetMigration)
			Expect(err).ToNot(HaveOccurred())
			_, err = virtfakeClient.KubevirtV1().VirtualMachineInstanceMigrations(targetVMI.Namespace).Create(context.TODO(), targetMigration, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			sourceVMI = libvmi.New(libvmi.WithNamespace(k8sv1.NamespaceDefault))
			sourceVMI.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{}
			err = vmiInformer.GetStore().Add(sourceVMI)
			Expect(err).ToNot(HaveOccurred())
			_, err = virtClient.VirtualMachineInstance(sourceVMI.Namespace).Create(context.TODO(), sourceVMI, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			remoteURL, err := controller.getLocalSynchronizationAddress()
			sourceMigration = createSourceMigration(testMigrationID, sourceVMI.Name, remoteURL, sourceVMI.Namespace)
			sourceMigration.Annotations = map[string]string{virtv1.MigrationPeerSyncPinAnnotation: id.Pin}
			err = migrationInformer.GetStore().Add(sourceMigration)
			Expect(err).ToNot(HaveOccurred())
			_, err = virtfakeClient.KubevirtV1().VirtualMachineInstanceMigrations(k8sv1.NamespaceDefault).Create(context.TODO(), sourceMigration, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			controller.closeConnections()
			if done != nil {
				<-done
			}
		})

		Context("handleSourceState", func() {
			It("should not do anything if source doesn't have source migration", func() {
				// no source migration
				err := controller.handleSourceState(sourceVMI, sourceMigration)
				Expect(err).ToNot(HaveOccurred())
				// no migration at all
				sourceVMI.Status.MigrationState = nil
				err = controller.handleSourceState(sourceVMI, sourceMigration)
				Expect(err).ToNot(HaveOccurred())
			})

			It("should not do anything if source sync addres is not set", func() {
				sourceVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{}
				storeVMI(controller, sourceVMI)
				err := controller.handleSourceState(sourceVMI, sourceMigration)
				Expect(err).ToNot(HaveOccurred())
			})

			It("should error and fail to update target VMI if target VMI doesn't exist", func() {
				sourceVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
					VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
						SyncAddress:  pointer.P(localTCPConn.Addr().String()),
						MigrationUID: sourceTestMigrationUID,
					},
				}
				sourceVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
					VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
						SyncAddress:  pointer.P(localTCPConn.Addr().String()),
						MigrationUID: targetTestMigrationUID,
					},
				}
				err = controller.vmiInformer.GetStore().Delete(targetVMI)
				Expect(err).ToNot(HaveOccurred())
				storeVMI(controller, sourceVMI)
				err = controller.handleSourceState(sourceVMI, sourceMigration)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(fmt.Sprintf(sourceUnableToLocateVMIMigrationIDErrorMsg, testMigrationID)))
			})

			It("should update target VMI source migration from source VMI", func() {
				sourceVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
					VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
						SyncAddress:  pointer.P(localTCPConn.Addr().String()),
						MigrationUID: sourceTestMigrationUID,
					},
				}
				sourceVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
					VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
						SyncAddress:  pointer.P(localTCPConn.Addr().String()),
						MigrationUID: targetTestMigrationUID,
					},
				}
				storeVMI(controller, sourceVMI)
				err := controller.handleSourceState(sourceVMI, sourceMigration)
				Expect(err).ToNot(HaveOccurred())
				verifyTarget(controller, targetVMI, localTCPConn.Addr().String())
			})
		})

		Context("handleTargetState", func() {
			It("should not do anything if target doesn't have target migration", func() {
				// no source migration
				err := controller.handleTargetState(targetVMI, targetMigration)
				Expect(err).ToNot(HaveOccurred())
				// no migration at all
				targetVMI.Status.MigrationState = nil
				err = controller.handleTargetState(targetVMI, targetMigration)
				Expect(err).ToNot(HaveOccurred())
			})

			It("should not do anything if target sync addres is not set", func() {
				targetVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{}
				storeVMI(controller, targetVMI)
				err := controller.handleTargetState(targetVMI, targetMigration)
				Expect(err).ToNot(HaveOccurred())
			})

			It("should error and fail to update target VMI if source VMI doesn't exist", func() {
				targetVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
					VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
						SyncAddress:  pointer.P(localTCPConn.Addr().String()),
						MigrationUID: targetTestMigrationUID,
					},
				}
				targetVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
					VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
						SyncAddress:  pointer.P(localTCPConn.Addr().String()),
						MigrationUID: sourceTestMigrationUID,
					},
				}
				err = controller.vmiInformer.GetStore().Delete(sourceVMI)
				Expect(err).ToNot(HaveOccurred())
				storeVMI(controller, targetVMI)
				err = controller.handleTargetState(targetVMI, targetMigration)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(fmt.Sprintf(targetUnableToLocateVMIMigrationIDErrorMsg, testMigrationID)))
			})

			It("should update source VMI target migration from target VMI", func() {
				targetVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
					VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
						SyncAddress:  pointer.P(localTCPConn.Addr().String()),
						MigrationUID: targetTestMigrationUID,
					},
				}
				targetVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
					VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
						SyncAddress:  pointer.P(localTCPConn.Addr().String()),
						MigrationUID: sourceTestMigrationUID,
					},
				}
				storeVMI(controller, targetVMI)
				err := controller.handleTargetState(targetVMI, targetMigration)
				Expect(err).ToNot(HaveOccurred())
				verifySource(controller, sourceVMI, localTCPConn.Addr().String())
			})
		})

		It("should properly update both source and target if both handlers are called", func() {
			targetVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					MigrationUID: targetTestMigrationUID,
				},
			}
			targetVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					SyncAddress: pointer.P(localTCPConn.Addr().String()),
				},
			}

			sourceVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					MigrationUID: sourceTestMigrationUID,
				},
			}
			sourceVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					SyncAddress: pointer.P(localTCPConn.Addr().String()),
				},
			}
			err = controller.vmiInformer.GetStore().Update(targetVMI)
			Expect(err).ToNot(HaveOccurred())
			_, err = controller.client.VirtualMachineInstance(targetVMI.Namespace).Update(context.Background(), targetVMI, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())
			_, err = controller.client.VirtualMachineInstance(sourceVMI.Namespace).Update(context.Background(), sourceVMI, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())
			err = controller.vmiInformer.GetStore().Update(sourceVMI)
			Expect(err).ToNot(HaveOccurred())
			sourceKey, err := kvcontroller.KeyFunc(sourceVMI)
			Expect(err).ToNot(HaveOccurred())
			err = controller.execute(sourceKey)
			Expect(err).ToNot(HaveOccurred())
			targetKey, err := kvcontroller.KeyFunc(targetVMI)
			Expect(err).ToNot(HaveOccurred())
			refreshVMI(controller, sourceVMI)
			refreshVMI(controller, targetVMI)
			err = controller.execute(targetKey)
			Expect(err).ToNot(HaveOccurred())
			verifyTarget(controller, targetVMI, localTCPConn.Addr().String())
			verifySource(controller, sourceVMI, localTCPConn.Addr().String())
		})
	})

	Context("target controller different than source", func() {
		// Migrating from one cluster to another
		var (
			err                                  error
			remoteController                     *SynchronizationController
			targetVMI, sourceVMI                 *virtv1.VirtualMachineInstance
			targetMigration, sourceMigration     *virtv1.VirtualMachineInstanceMigration
			remoteURL, localURL                  string
			controllerDone, remoteControllerDone chan struct{}
		)

		BeforeEach(func() {
			remoteMigrationInformer, _ := testutils.NewFakeInformerFor(&virtv1.VirtualMachineInstanceMigration{})
			remoteController, err = NewSynchronizationController(virtClient, vmiInformer, remoteMigrationInformer, "0.0.0.0", 9186, "")
			Expect(err).ToNot(HaveOccurred())
			// The pins the management layer hands over between the clusters.
			localID, err := controller.identity(testMigrationID)
			Expect(err).ToNot(HaveOccurred())
			remoteID, err := remoteController.identity(testMigrationID)
			Expect(err).ToNot(HaveOccurred())

			remoteTCPConn, err := remoteController.createTcpListener()
			Expect(err).ToNot(HaveOccurred())
			remoteControllerDone = make(chan struct{})
			go func() {
				defer close(remoteControllerDone)
				remoteController.grpcServer.Serve(remoteTCPConn)
			}()

			localTCPConn, err := controller.createTcpListener()
			Expect(err).ToNot(HaveOccurred())
			controllerDone = make(chan struct{})
			go func() {
				defer close(controllerDone)
				controller.grpcServer.Serve(localTCPConn)
			}()

			targetVMI = libvmi.New(libvmi.WithNamespace(targetNamespace))
			targetVMI.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{}
			err = vmiInformer.GetStore().Add(targetVMI)
			Expect(err).ToNot(HaveOccurred())
			_, err = virtClient.VirtualMachineInstance(targetVMI.Namespace).Create(context.TODO(), targetVMI, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			targetMigration = createTargetMigration(testMigrationID, targetVMI.Name, targetVMI.Namespace)
			targetMigration.Annotations = map[string]string{virtv1.MigrationPeerSyncPinAnnotation: localID.Pin}
			err = remoteMigrationInformer.GetStore().Add(targetMigration)
			Expect(err).ToNot(HaveOccurred())
			_, err = virtfakeClient.KubevirtV1().VirtualMachineInstanceMigrations(targetVMI.Namespace).Create(context.TODO(), targetMigration, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			sourceVMI = libvmi.New(libvmi.WithNamespace(k8sv1.NamespaceDefault))
			sourceVMI.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{}
			err = vmiInformer.GetStore().Add(sourceVMI)
			Expect(err).ToNot(HaveOccurred())
			sourceVMI, err = virtClient.VirtualMachineInstance(sourceVMI.Namespace).Create(context.TODO(), sourceVMI, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			remoteURL, err = remoteController.getLocalSynchronizationAddress()
			sourceMigration = createSourceMigration(testMigrationID, sourceVMI.Name, remoteURL, sourceVMI.Namespace)
			sourceMigration.Annotations = map[string]string{virtv1.MigrationPeerSyncPinAnnotation: remoteID.Pin}
			err = migrationInformer.GetStore().Add(sourceMigration)
			Expect(err).ToNot(HaveOccurred())
			_, err = virtfakeClient.KubevirtV1().VirtualMachineInstanceMigrations(sourceVMI.Namespace).Create(context.TODO(), sourceMigration, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			localURL, err = controller.getLocalSynchronizationAddress()
			Expect(err).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			remoteController.closeConnections()
			controller.closeConnections()
			if remoteControllerDone != nil {
				<-remoteControllerDone
			}
			if controllerDone != nil {
				<-controllerDone
			}
		})

		It("should properly update both source and target if both handlers are called", func() {
			targetVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					MigrationUID: targetTestMigrationUID,
				},
			}
			targetVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					SyncAddress: pointer.P(localURL),
				},
			}
			sourceVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					MigrationUID: sourceTestMigrationUID,
				},
			}
			sourceVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					SyncAddress: pointer.P(remoteURL),
				},
			}
			By("setting up the clients and informers")
			err = remoteController.vmiInformer.GetStore().Update(targetVMI)
			Expect(err).ToNot(HaveOccurred())
			_, err = remoteController.client.VirtualMachineInstance(targetVMI.Namespace).Update(context.Background(), targetVMI, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())
			err = controller.vmiInformer.GetStore().Update(sourceVMI)
			Expect(err).ToNot(HaveOccurred())
			_, err = controller.client.VirtualMachineInstance(sourceVMI.Namespace).Update(context.Background(), sourceVMI, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("calling execute on the source")
			sourceKey, err := kvcontroller.KeyFunc(sourceVMI)
			Expect(err).ToNot(HaveOccurred())
			err = controller.execute(sourceKey)
			Expect(err).ToNot(HaveOccurred())

			By("calling execute on the target")
			targetKey, err := kvcontroller.KeyFunc(targetVMI)
			Expect(err).ToNot(HaveOccurred())
			refreshVMI(controller, sourceVMI)
			refreshVMI(remoteController, targetVMI)
			err = remoteController.execute(targetKey)
			Expect(err).ToNot(HaveOccurred())

			By("verifying the result")
			verifySource(controller, sourceVMI, remoteURL)
			verifyTarget(remoteController, targetVMI, localURL)

			By("updating the source state with a new sourcePodName")
			sourceVMI, err = controller.client.VirtualMachineInstance(sourceVMI.Namespace).Get(context.Background(), sourceVMI.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			sourceVMI.Status.MigrationState.SourceState.Pod = sourcePodName
			err = controller.vmiInformer.GetStore().Update(sourceVMI)
			Expect(err).ToNot(HaveOccurred())
			_, err = controller.client.VirtualMachineInstance(sourceVMI.Namespace).Update(context.Background(), sourceVMI, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("updating the remote informer with the remote client, we can properly patch")
			updatedTargetVMI, err := remoteController.client.VirtualMachineInstance(targetVMI.Namespace).Get(context.Background(), targetVMI.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			remoteController.vmiInformer.GetStore().Update(updatedTargetVMI)

			By("calling execute on the source")
			err = controller.execute(sourceKey)
			Expect(err).ToNot(HaveOccurred())

			By("verifying the result")
			verifyTarget(controller, targetVMI, localURL)
			updatedTargetVMI, err = remoteController.client.VirtualMachineInstance(targetVMI.Namespace).Get(context.Background(), targetVMI.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			remoteController.vmiInformer.GetStore().Update(updatedTargetVMI)
			Expect(err).ToNot(HaveOccurred())

			Expect(updatedTargetVMI).ToNot(BeNil())
			Expect(updatedTargetVMI.Status.MigrationState).ToNot(BeNil())
			Expect(updatedTargetVMI.Status.MigrationState.SourceState).ToNot(BeNil())
			Expect(updatedTargetVMI.Status.MigrationState.SourceState.MigrationUID).To(BeEquivalentTo(sourceTestMigrationUID))
			Expect(updatedTargetVMI.Status.MigrationState.SourceState.SyncAddress).ToNot(BeNil())
			Expect(*updatedTargetVMI.Status.MigrationState.SourceState.SyncAddress).To(Equal(localURL))
			Expect(updatedTargetVMI.Status.MigrationState.SourceState.Pod).To(Equal(sourcePodName))

			By("updating the target state")
			targetVMI, err = remoteController.client.VirtualMachineInstance(targetVMI.Namespace).Get(context.Background(), targetVMI.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			targetVMI.Status.MigrationState.TargetState.Pod = targetPodName
			err = remoteController.vmiInformer.GetStore().Update(targetVMI)
			Expect(err).ToNot(HaveOccurred())

			By("calling execute on the target")
			err = remoteController.execute(targetKey)
			Expect(err).ToNot(HaveOccurred())
			updatedSourceVMI, err := controller.client.VirtualMachineInstance(sourceVMI.Namespace).Get(context.Background(), sourceVMI.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedSourceVMI).ToNot(BeNil())
			Expect(updatedSourceVMI.Status.MigrationState).ToNot(BeNil())
			Expect(updatedSourceVMI.Status.MigrationState.TargetState).ToNot(BeNil())
			Expect(updatedSourceVMI.Status.MigrationState.TargetState.MigrationUID).To(BeEquivalentTo(targetTestMigrationUID))
			Expect(updatedSourceVMI.Status.MigrationState.TargetState.SyncAddress).ToNot(BeNil())
			Expect(*updatedSourceVMI.Status.MigrationState.TargetState.SyncAddress).To(Equal(remoteURL))
			Expect(updatedSourceVMI.Status.MigrationState.TargetState.Pod).To(Equal(targetPodName))
		})

		It("should not rebuild anything without migrations", func() {
			objs := remoteController.migrationInformer.GetStore().List()
			for _, obj := range objs {
				remoteController.migrationInformer.GetStore().Delete(obj)
			}
			err := remoteController.rebuildConnectionsAndUpdateSyncAddress()
			Expect(err).ToNot(HaveOccurred())
			remoteController.syncOutboundConnectionMap.Range(func(key, value any) bool {
				Fail(fmt.Sprintf("found object in outbound map %v, %v", key, value))
				return true
			})
		})

		It("should not rebuild anything if migrations are not ongoing", func() {
			objs := remoteController.migrationInformer.GetStore().List()
			for _, obj := range objs {
				migration, ok := obj.(*virtv1.VirtualMachineInstanceMigration)
				Expect(ok).To(BeTrue())
				migration.Status.Phase = virtv1.MigrationFailed
				remoteController.migrationInformer.GetStore().Update(migration)
			}
			err := remoteController.rebuildConnectionsAndUpdateSyncAddress()
			Expect(err).ToNot(HaveOccurred())
			remoteController.syncOutboundConnectionMap.Range(func(key, value any) bool {
				Fail(fmt.Sprintf("found object in outbound map %v, %v", key, value))
				return true
			})
		})

		It("should not rebuild anything if migration references missing VMI", func() {
			objs := remoteController.vmiInformer.GetStore().List()
			for _, obj := range objs {
				remoteController.vmiInformer.GetStore().Delete(obj)
			}

			err := remoteController.rebuildConnectionsAndUpdateSyncAddress()
			Expect(err).ToNot(HaveOccurred())
			remoteController.syncOutboundConnectionMap.Range(func(key, value any) bool {
				Fail(fmt.Sprintf("found object in outbound map %v, %v", key, value))
				return true
			})
		})

		It("should not rebuild anything if VMI doesn't have sync address", func() {
			err := remoteController.rebuildConnectionsAndUpdateSyncAddress()
			Expect(err).ToNot(HaveOccurred())
			remoteController.syncOutboundConnectionMap.Range(func(key, value any) bool {
				Fail(fmt.Sprintf("found object in outbound map %v, %v", key, value))
				return true
			})
			err = controller.rebuildConnectionsAndUpdateSyncAddress()
			Expect(err).ToNot(HaveOccurred())
			controller.syncReceivingConnectionMap.Range(func(key, value any) bool {
				Fail(fmt.Sprintf("found object in outbound map %v, %v", key, value))
				return true
			})
		})

		It("should properly rebuild target", func() {
			remoteURL, err := remoteController.getLocalSynchronizationAddress()
			Expect(err).ToNot(HaveOccurred())
			targetVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					SyncAddress: pointer.P(remoteURL),
				},
			}
			targetVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					MigrationUID: targetTestMigrationUID,
				},
			}
			_, err = remoteController.client.VirtualMachineInstance(targetVMI.Namespace).Update(context.Background(), targetVMI, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())
			err = remoteController.vmiInformer.GetStore().Update(targetVMI)
			Expect(err).ToNot(HaveOccurred())
			err = remoteController.rebuildConnectionsAndUpdateSyncAddress()
			Expect(err).ToNot(HaveOccurred())
			count := 0
			remoteController.syncReceivingConnectionMap.Range(func(key, value any) bool {
				count++
				Expect(key).To(Equal(testMigrationID))
				conn, ok := value.(*SynchronizationConnection)
				Expect(ok).To(BeTrue())
				Expect(conn.migrationID).To(Equal(testMigrationID))
				Expect(conn.grpcClientConnection.CanonicalTarget()).To(ContainSubstring(remoteURL))
				return true
			})
			Expect(count).To(Equal(1), "there should be one item in the map")
		})

		It("should properly rebuild source", func() {
			remoteURL, err := controller.getLocalSynchronizationAddress()
			Expect(err).ToNot(HaveOccurred())
			sourceVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					SyncAddress: pointer.P(remoteURL),
				},
			}
			sourceVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					MigrationUID: sourceTestMigrationUID,
				},
			}
			_, err = controller.client.VirtualMachineInstance(sourceVMI.Namespace).Update(context.Background(), sourceVMI, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())
			err = controller.vmiInformer.GetStore().Update(sourceVMI)
			Expect(err).ToNot(HaveOccurred())
			err = controller.rebuildConnectionsAndUpdateSyncAddress()
			Expect(err).ToNot(HaveOccurred())
			count := 0
			controller.syncOutboundConnectionMap.Range(func(key, value any) bool {
				count++
				Expect(key).To(Equal(testMigrationID))
				conn, ok := value.(*SynchronizationConnection)
				Expect(ok).To(BeTrue())
				Expect(conn.migrationID).To(Equal(testMigrationID))
				Expect(conn.grpcClientConnection.CanonicalTarget()).To(ContainSubstring(remoteURL))
				return true
			})
			Expect(count).To(Equal(1), "there should be one item in the map")
		})

		It("should cancel remote migration", func() {
			targetVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					MigrationUID: targetTestMigrationUID,
				},
			}
			targetVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					SyncAddress: pointer.P(localURL),
				},
			}
			sourceVMI.Status.MigrationState.SourceState = &virtv1.VirtualMachineInstanceMigrationSourceState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					MigrationUID: sourceTestMigrationUID,
				},
			}
			sourceVMI.Status.MigrationState.TargetState = &virtv1.VirtualMachineInstanceMigrationTargetState{
				VirtualMachineInstanceCommonMigrationState: virtv1.VirtualMachineInstanceCommonMigrationState{
					SyncAddress: pointer.P(remoteURL),
				},
			}
			By("setting up the clients and informers")
			err = remoteController.vmiInformer.GetStore().Update(targetVMI)
			Expect(err).ToNot(HaveOccurred())
			_, err = remoteController.client.VirtualMachineInstance(targetVMI.Namespace).Update(context.Background(), targetVMI, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())
			err = controller.vmiInformer.GetStore().Update(sourceVMI)
			Expect(err).ToNot(HaveOccurred())
			_, err = controller.client.VirtualMachineInstance(sourceVMI.Namespace).Update(context.Background(), sourceVMI, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())
			targetMigration.DeletionTimestamp = pointer.P(metav1.Now())
			err = remoteController.migrationInformer.GetStore().Update(targetMigration)
			Expect(err).ToNot(HaveOccurred())

			res, err := controller.CancelMigration(migrationPeerContext(testMigrationID), &syncv1.MigrationCancelRequest{
				MigrationUID: sourceTestMigrationUID,
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Message).To(Equal("migration canceled"))
		})
	})

	It("should return nil, nil if invalid resource type passed into index function", func() {
		res, err := indexByMigrationUID("invalid")
		Expect(res).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
		res, err = indexByActiveVmiName("invalid")
		Expect(res).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
		res, err = indexBySourceMigrationID("invalid")
		Expect(res).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
		res, err = indexByTargetMigrationID("invalid")
		Expect(res).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
	})

	It("should properly close connections", func() {
		testConnection := &SynchronizationConnection{
			migrationID: testMigrationID,
		}
		controller.syncOutboundConnectionMap.Store(testMigrationID, testConnection)
		controller.syncOutboundConnectionMap.Store("invalidID", "invalidConnection")
		_, loaded := controller.syncOutboundConnectionMap.Load("invalidID")
		Expect(loaded).To(BeTrue())
		_, loaded = controller.syncOutboundConnectionMap.Load(testMigrationID)
		Expect(loaded).To(BeTrue())

		controller.closeConnectionForMigrationID(controller.syncOutboundConnectionMap, testMigrationID)
		controller.closeConnectionForMigrationID(controller.syncOutboundConnectionMap, "invalidID")
		controller.closeConnectionForMigrationID(controller.syncOutboundConnectionMap, "unknownID")
		_, loaded = controller.syncOutboundConnectionMap.Load("invalidID")
		Expect(loaded).To(BeFalse())
		_, loaded = controller.syncOutboundConnectionMap.Load(testMigrationID)
		Expect(loaded).To(BeFalse())
		_, loaded = controller.syncOutboundConnectionMap.Load("unknownID")
		Expect(loaded).To(BeFalse())
	})

	Context("resource handler functions", func() {
		var (
			sourceMigration, targetMigration *virtv1.VirtualMachineInstanceMigration
		)
		BeforeEach(func() {
			sourceMigration = createSourceMigration(testMigrationID, "test-vmi", "", k8sv1.NamespaceDefault)
			targetMigration = createTargetMigration(testMigrationID, "test-vmi", k8sv1.NamespaceDefault)
			Expect(controller.queue.Len()).To(Equal(0))
		})

		It("should add proper key to queue when adding/updating new VMIM", func() {
			controller.addMigrationFunc(sourceMigration)
			Expect(controller.queue.Len()).To(Equal(1))
			key := fmt.Sprintf("%s/%s", sourceMigration.Namespace, sourceMigration.Spec.VMIName)
			res, shutdown := controller.queue.Get()
			Expect(shutdown).To(BeFalse())
			Expect(res).To(Equal(key))
			controller.queue.Done(key)
			Expect(controller.queue.Len()).To(Equal(0))

			controller.updateMigrationFunc(nil, sourceMigration)
			Expect(controller.queue.Len()).To(Equal(1))
			key = fmt.Sprintf("%s/%s", sourceMigration.Namespace, sourceMigration.Spec.VMIName)
			res, shutdown = controller.queue.Get()
			Expect(shutdown).To(BeFalse())
			Expect(res).To(Equal(key))
			controller.queue.Done(key)
			Expect(controller.queue.Len()).To(Equal(0))
		})

		It("should close existing connections when deleting vmim", func() {
			testConnection := &SynchronizationConnection{
				migrationID: testMigrationID,
			}
			controller.syncOutboundConnectionMap.Store(testMigrationID, testConnection)

			controller.deleteMigrationFunc(sourceMigration)
			Expect(controller.queue.Len()).To(Equal(1))
			key := fmt.Sprintf("%s/%s", sourceMigration.Namespace, sourceMigration.Spec.VMIName)
			res, shutdown := controller.queue.Get()
			Expect(shutdown).To(BeFalse())
			Expect(res).To(Equal(key))
			controller.queue.Done(key)
			Expect(controller.queue.Len()).To(Equal(0))
			_, loaded := controller.syncOutboundConnectionMap.Load(testMigrationID)
			Expect(loaded).To(BeFalse())

			controller.syncReceivingConnectionMap.Store(testMigrationID, testConnection)
			controller.deleteMigrationFunc(targetMigration)
			Expect(controller.queue.Len()).To(Equal(1))
			key = fmt.Sprintf("%s/%s", targetMigration.Namespace, targetMigration.Spec.VMIName)
			res, shutdown = controller.queue.Get()
			Expect(shutdown).To(BeFalse())
			Expect(res).To(Equal(key))
			controller.queue.Done(key)
			Expect(controller.queue.Len()).To(Equal(0))
			_, loaded = controller.syncReceivingConnectionMap.Load(testMigrationID)
			Expect(loaded).To(BeFalse())
		})
	})
	It("should be able to find the VMI from the migration", func() {
		vmi := libvmi.New(libvmi.WithNamespace(k8sv1.NamespaceDefault))
		err := controller.vmiInformer.GetStore().Add(vmi)
		Expect(err).ToNot(HaveOccurred())
		migration := createTargetMigration(testMigrationID, vmi.Name, vmi.Namespace)
		res, err := controller.getVMIFromMigration(migration)
		Expect(err).ToNot(HaveOccurred())
		Expect(res).To(BeEquivalentTo(vmi))
		migration = createTargetMigration(testMigrationID, "unknown", vmi.Namespace)
		res, err = controller.getVMIFromMigration(migration)
		Expect(err).ToNot(HaveOccurred())
		Expect(res).To(BeNil())
	})
})

func createSourceMigration(migrationID, vmiName, connectionURL, namespace string) *virtv1.VirtualMachineInstanceMigration {
	return &virtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sourceTestMigrationName,
			Namespace: namespace,
			UID:       sourceTestMigrationUID,
		},
		Spec: virtv1.VirtualMachineInstanceMigrationSpec{
			SendTo: &virtv1.VirtualMachineInstanceMigrationSource{
				MigrationID: migrationID,
				ConnectURL:  connectionURL,
			},
			VMIName: vmiName,
		},
	}
}

func createTargetMigration(migrationID, vmiName, namespace string) *virtv1.VirtualMachineInstanceMigration {
	return &virtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetTestMigrationName,
			Namespace: namespace,
			UID:       targetTestMigrationUID,
		},
		Spec: virtv1.VirtualMachineInstanceMigrationSpec{
			Receive: &virtv1.VirtualMachineInstanceMigrationTarget{
				MigrationID: migrationID,
			},
			VMIName: vmiName,
		},
	}
}

func createLegacyMigration() *virtv1.VirtualMachineInstanceMigration {
	return &virtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testMigrationID,
			Namespace: k8sv1.NamespaceDefault,
			UID:       testMigrationUID,
		},
	}
}

var _ = Describe("copyLegacySourceFields", func() {
	newSourceState := func(failed bool) *virtv1.VirtualMachineInstanceMigrationState {
		now := metav1.Now()
		return &virtv1.VirtualMachineInstanceMigrationState{
			SourceState:    &virtv1.VirtualMachineInstanceMigrationSourceState{},
			StartTimestamp: &now,
			EndTimestamp:   &now,
			Failed:         failed,
			FailureReason:  "Domain job failed",
		}
	}

	It("ends the migration on the target when the source failed", func() {
		vmi := &virtv1.VirtualMachineInstance{}
		vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{}

		copyLegacySourceFields(vmi, newSourceState(true))

		Expect(vmi.Status.MigrationState.EndTimestamp).ToNot(BeNil())
		Expect(vmi.Status.MigrationState.Failed).To(BeTrue())
		Expect(vmi.Status.MigrationState.FailureReason).To(Equal("Domain job failed"))
	})

	It("leaves the outcome to the target when the source has not failed", func() {
		vmi := &virtv1.VirtualMachineInstance{}
		vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{}

		copyLegacySourceFields(vmi, newSourceState(false))

		Expect(vmi.Status.MigrationState.Failed).To(BeFalse())
		Expect(vmi.Status.MigrationState.Completed).To(BeFalse())
	})
})

// storeVMI pushes the VMI of the informer to the client, as the controller patches the VMI it reads.
func storeVMI(c *SynchronizationController, vmi *virtv1.VirtualMachineInstance) {
	_, err := c.client.VirtualMachineInstance(vmi.Namespace).Update(context.Background(), vmi, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

// refreshVMI brings the informer up to date with the client after a patch by a handler.
func refreshVMI(c *SynchronizationController, vmi *virtv1.VirtualMachineInstance) {
	updated, err := c.client.VirtualMachineInstance(vmi.Namespace).Get(context.Background(), vmi.Name, metav1.GetOptions{})
	Expect(err).ToNot(HaveOccurred())
	Expect(c.vmiInformer.GetStore().Update(updated)).To(Succeed())
}

var _ = Describe("synchronization key", func() {
	It("outlives the connection closed on the final state", func() {
		s := &SynchronizationController{}
		id, err := s.identity("m1")
		Expect(err).ToNot(HaveOccurred())
		Expect(s.closeConnectionForMigrationID(&sync.Map{}, "m1")).To(Succeed())
		again, err := s.identity("m1")
		Expect(err).ToNot(HaveOccurred())
		Expect(again.Pin).To(Equal(id.Pin))
	})
})

func migrationPeerContext(migrationID string) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{ServerName: migrationID}}})
}

var _ = Describe("Migration RPC authorization", func() {
	DescribeTable("rejects unauthorized calls before accessing state", func(ctx context.Context, expected codes.Code) {
		controller := &SynchronizationController{}
		_, err := controller.SyncSourceMigrationStatus(ctx, &syncv1.VMIStatusRequest{MigrationID: "requested"})
		Expect(status.Code(err)).To(Equal(expected))
		_, err = controller.SyncTargetMigrationStatus(ctx, &syncv1.VMIStatusRequest{MigrationID: "requested"})
		Expect(status.Code(err)).To(Equal(expected))
		if expected == codes.Unauthenticated {
			_, err = controller.CancelMigration(ctx, &syncv1.MigrationCancelRequest{MigrationUID: "uid"})
			Expect(status.Code(err)).To(Equal(expected))
		}
	},
		Entry("without a peer", context.Background(), codes.Unauthenticated),
		Entry("without TLS", peer.NewContext(context.Background(), &peer.Peer{}), codes.Unauthenticated),
		Entry("without SNI", migrationPeerContext(""), codes.Unauthenticated),
		Entry("with another migration", migrationPeerContext("other"), codes.PermissionDenied),
	)

	DescribeTable("authorizes cancellation against the stored migration", func(receiving bool) {
		informer, _ := testutils.NewFakeInformerFor(&virtv1.VirtualMachineInstanceMigration{})
		Expect(informer.AddIndexers(cache.Indexers{"byUID": indexByMigrationUID})).To(Succeed())
		migration := createSourceMigration("requested", "vmi", "", metav1.NamespaceDefault)
		if receiving {
			migration = createTargetMigration("requested", "vmi", metav1.NamespaceDefault)
		}
		Expect(informer.GetStore().Add(migration)).To(Succeed())
		fakeClient := kubevirtfake.NewSimpleClientset(migration)
		mock := kubecli.NewMockKubevirtClient(gomock.NewController(GinkgoT()))
		mock.EXPECT().VirtualMachineInstanceMigration(migration.Namespace).Return(fakeClient.KubevirtV1().VirtualMachineInstanceMigrations(migration.Namespace)).AnyTimes()
		controller := &SynchronizationController{migrationInformer: informer, client: mock}
		request := &syncv1.MigrationCancelRequest{MigrationUID: string(migration.UID)}
		_, err := controller.CancelMigration(migrationPeerContext("other"), request)
		Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
		Expect(fakeClient.Actions()).To(BeEmpty())
		_, err = controller.CancelMigration(migrationPeerContext("requested"), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(fakeClient.Actions()).To(HaveLen(1))
		_, err = controller.CancelMigration(migrationPeerContext("requested"), &syncv1.MigrationCancelRequest{MigrationUID: "missing"})
		Expect(status.Code(err)).To(Equal(codes.NotFound))
	}, Entry("source", false), Entry("target", true))

	It("preserves authenticated SNI through the gRPC TLS server", func() {
		serverID, err := kvtls.NewEphemeralIdentity("server")
		Expect(err).ToNot(HaveOccurred())
		clientID, err := kvtls.NewEphemeralIdentity("client")
		Expect(err).ToNot(HaveOccurred())
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).ToNot(HaveOccurred())
		server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			config := serverID.ServerConfig(func() string { return clientID.Pin })
			config.NextProtos = []string{"h2"}
			return config, nil
		}})))
		syncv1.RegisterSynchronizeServer(server, &SynchronizationController{})
		go server.Serve(listener)
		DeferCleanup(server.Stop)
		connection, err := grpc.Dial(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(clientID.ClientConfig(func() string { return serverID.Pin }, "requested"))))
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(connection.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rpc := syncv1.NewSynchronizeClient(connection)
		_, err = rpc.SyncSourceMigrationStatus(ctx, &syncv1.VMIStatusRequest{MigrationID: "other"})
		Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
		_, err = rpc.SyncSourceMigrationStatus(ctx, &syncv1.VMIStatusRequest{MigrationID: "requested"})
		Expect(err.Error()).To(ContainSubstring(noSourceStatusErrorMsg))
	})
})
