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
 * Copyright 2018 Red Hat, Inc.
 *
 */

package admitters

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/client-go/api"

	admissionv1 "k8s.io/api/admission/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfield "k8s.io/apimachinery/pkg/util/validation/field"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	instancetypeapi "kubevirt.io/api/instancetype"
	instancetypev1beta1 "kubevirt.io/api/instancetype/v1beta1"

	v1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/kubevirt/pkg/instancetype"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/testutils"
	"kubevirt.io/kubevirt/pkg/virt-api/webhooks"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

var _ = Describe("Validating VM Admitter", func() {
	config, crdInformer, kvInformer := testutils.NewFakeClusterConfigUsingKVConfig(&v1.KubeVirtConfiguration{})
	var (
		vmsAdmitter         *VMsAdmitter
		dataSourceInformer  cache.SharedIndexInformer
		namespaceInformer   cache.SharedIndexInformer
		instancetypeMethods *testutils.MockInstancetypeMethods
		migrationInterface  *kubecli.MockVirtualMachineInstanceMigrationInterface
		mockVMIClient       *kubecli.MockVirtualMachineInstanceInterface
		virtClient          *kubecli.MockKubevirtClient
		k8sClient           *k8sfake.Clientset
	)

	enableFeatureGate := func(featureGate string) {
		testutils.UpdateFakeKubeVirtClusterConfig(kvInformer, &v1.KubeVirt{
			Spec: v1.KubeVirtSpec{
				Configuration: v1.KubeVirtConfiguration{
					DeveloperConfiguration: &v1.DeveloperConfiguration{
						FeatureGates: []string{featureGate},
					},
				},
			},
		})
	}
	disableFeatureGates := func() {
		testutils.UpdateFakeKubeVirtClusterConfig(kvInformer, &v1.KubeVirt{
			Spec: v1.KubeVirtSpec{
				Configuration: v1.KubeVirtConfiguration{
					DeveloperConfiguration: &v1.DeveloperConfiguration{
						FeatureGates: make([]string, 0),
					},
				},
			},
		})
	}

	notRunning := false
	runStrategyManual := v1.RunStrategyManual
	runStrategyHalted := v1.RunStrategyHalted

	BeforeEach(func() {
		dataSourceInformer, _ = testutils.NewFakeInformerFor(&cdiv1.DataSource{})
		namespaceInformer, _ = testutils.NewFakeInformerFor(&k8sv1.Namespace{})
		ns1 := &k8sv1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ns1",
			},
		}
		ns2 := &k8sv1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ns2",
			},
		}
		ns3 := &k8sv1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ns3",
			},
		}
		Expect(namespaceInformer.GetStore().Add(ns1)).To(Succeed())
		Expect(namespaceInformer.GetStore().Add(ns2)).To(Succeed())
		Expect(namespaceInformer.GetStore().Add(ns3)).To(Succeed())
		instancetypeMethods = testutils.NewMockInstancetypeMethods()

		ctrl := gomock.NewController(GinkgoT())
		mockVMIClient = kubecli.NewMockVirtualMachineInstanceInterface(ctrl)
		migrationInterface = kubecli.NewMockVirtualMachineInstanceMigrationInterface(ctrl)
		k8sClient = k8sfake.NewSimpleClientset()
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		vmsAdmitter = &VMsAdmitter{
			VirtClient:          virtClient,
			DataSourceInformer:  dataSourceInformer,
			NamespaceInformer:   namespaceInformer,
			ClusterConfig:       config,
			InstancetypeMethods: instancetypeMethods,
			cloneAuthFunc: func(dv *cdiv1.DataVolume, requestNamespace, requestName string, proxy cdiv1.AuthorizationHelperProxy, saNamespace, saName string) (bool, string, error) {
				return true, "", nil
			},
		}
		virtClient.EXPECT().AuthorizationV1().Return(k8sClient.AuthorizationV1()).AnyTimes()
	})

	It("reject invalid VirtualMachineInstance spec", func() {
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testdisk",
		})
		vm := &v1.VirtualMachine{
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
		}

		resp := admitVm(vmsAdmitter, vm)
		Expect(resp.Allowed).To(BeFalse())
		Expect(resp.Result.Details.Causes).To(HaveLen(1))
		Expect(resp.Result.Details.Causes[0].Field).To(Equal("spec.template.spec.domain.devices.disks[0].name"))
	})

	It("should allow VM that is being deleted", func() {
		vmi := api.NewMinimalVMI("testvmi")
		now := metav1.Now()
		vm := &v1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				DeletionTimestamp: &now,
			},
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
		}
		resp := admitVm(vmsAdmitter, vm)
		Expect(resp.Allowed).To(BeTrue())
	})

	It("should allow VM with missing volume disk or filesystem", func() {
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testvol",
			VolumeSource: v1.VolumeSource{
				ContainerDisk: testutils.NewFakeContainerDiskSource(),
			},
		})
		vm := &v1.VirtualMachine{
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
		}
		resp := admitVm(vmsAdmitter, vm)
		Expect(resp.Allowed).To(BeTrue())
	})

	It("should accept valid vmi spec", func() {
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testdisk",
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testdisk",
			VolumeSource: v1.VolumeSource{
				ContainerDisk: testutils.NewFakeContainerDiskSource(),
			},
		})

		vm := &v1.VirtualMachine{
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
		}

		resp := admitVm(vmsAdmitter, vm)
		Expect(resp.Allowed).To(BeTrue())
	})

	It("should accept VM requesting hugepages but missing spec.template.spec.domain.resources.requests.memory - bug #9102", func() {
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Spec.Domain.Resources = v1.ResourceRequirements{}
		guestMemory := resource.MustParse("1Gi")
		vmi.Spec.Domain.Memory = &v1.Memory{
			Guest: &guestMemory,
			Hugepages: &v1.Hugepages{
				PageSize: "2Mi",
			},
		}
		vm := &v1.VirtualMachine{
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
		}

		resp := admitVm(vmsAdmitter, vm)
		Expect(resp.Allowed).To(BeTrue())
	})

	DescribeTable("should reject VolumeRequests on a migrating vm", func(requests []v1.VirtualMachineVolumeRequest) {
		now := metav1.Now()
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Status = v1.VirtualMachineInstanceStatus{
			MigrationState: &v1.VirtualMachineInstanceMigrationState{
				StartTimestamp: &now,
			},
		}
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testdisk",
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testdisk",
			VolumeSource: v1.VolumeSource{
				ContainerDisk: testutils.NewFakeContainerDiskSource(),
			},
		})

		vm := &v1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vmi.Name,
				Namespace: vmi.Namespace,
			},
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: *vmi.Spec.DeepCopy(),
				},
			},
			Status: v1.VirtualMachineStatus{
				VolumeRequests: requests,
				Ready:          true,
			},
		}
		vmBytes, _ := json.Marshal(&vm)

		ar := &admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				Resource: webhooks.VirtualMachineGroupVersionResource,
				Object: runtime.RawExtension{
					Raw: vmBytes,
				},
			},
		}

		virtClient.EXPECT().VirtualMachineInstance(gomock.Any()).Return(mockVMIClient)
		mockVMIClient.EXPECT().Get(context.Background(), gomock.Any(), gomock.Any()).Return(vmi, nil)
		resp := vmsAdmitter.Admit(ar)
		Expect(resp.Allowed).To(BeFalse())
	},
		Entry("with valid request to add volume", []v1.VirtualMachineVolumeRequest{
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testdisk2",
					Disk: &v1.Disk{
						Name: "testdisk2",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "madeup",
						}},
					},
				},
			},
		}),
		Entry("with valid request to remove volume", []v1.VirtualMachineVolumeRequest{
			{
				RemoveVolumeOptions: &v1.RemoveVolumeOptions{
					Name: "testdisk",
				},
			},
		}),
	)

	DescribeTable("should validate VolumeRequest on running vm", func(requests []v1.VirtualMachineVolumeRequest, isValid bool) {
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testdisk",
		})
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testpvcdisk",
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testdisk",
			VolumeSource: v1.VolumeSource{
				ContainerDisk: testutils.NewFakeContainerDiskSource(),
			},
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testpvcdisk",
			VolumeSource: v1.VolumeSource{
				PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: "testpvcdiskclaim",
				}},
			},
		})

		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "a-pvcdisk",
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "a-pvcdisk",
			VolumeSource: v1.VolumeSource{
				PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: "a-pvcdiskclaim",
				}},
			},
		})

		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "t-pvcdisk",
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "t-pvcdisk",
			VolumeSource: v1.VolumeSource{
				PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: "t-pvcdiskclaim",
				}},
			},
		})

		vm := &v1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vmi.Name,
				Namespace: vmi.Namespace,
			},
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: *vmi.Spec.DeepCopy(),
				},
			},
			Status: v1.VirtualMachineStatus{
				VolumeRequests: requests,
				Ready:          true,
			},
		}

		// add some additional volumes to the running VMI so we can simulate
		// more advanced validation scenarios where VM and VMI specs drift.
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testpvcdisk-extra",
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testpvcdisk-extra",
			VolumeSource: v1.VolumeSource{
				PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: "testpvcdiskclaim-extra",
				},
				},
			},
		})

		virtClient.EXPECT().VirtualMachineInstance(gomock.Any()).Return(mockVMIClient)
		mockVMIClient.EXPECT().Get(context.Background(), gomock.Any(), gomock.Any()).Return(vmi, nil)
		resp := admitVm(vmsAdmitter, vm)
		Expect(resp.Allowed).To(Equal(isValid))
	},
		Entry("with valid request to add volume", []v1.VirtualMachineVolumeRequest{
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testdisk2",
					Disk: &v1.Disk{
						Name: "testdisk2",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "madeup",
						},
						},
					},
				},
			},
		},
			true),
		Entry("with invalid request to add volume that conflicts with running vmi", []v1.VirtualMachineVolumeRequest{
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testpvcdisk-extra",
					Disk: &v1.Disk{
						Name: "testpvcdisk-extra",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "NOT-IDENTICAL-TO-WHAT-IS-IN-VMI",
						}},
					},
				},
			},
		},
			false),
		Entry("with valid request to add volume that is identical to one in vmi", []v1.VirtualMachineVolumeRequest{
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "a-pvcdisk",
					Disk: &v1.Disk{
						Name: "a-pvcdisk",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "a-pvcdiskclaim",
						}},
					},
				},
			},
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testpvcdisk-extra1",
					Disk: &v1.Disk{
						Name: "testpvcdisk-extra1",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "testpvcdiskclaim-extra1",
						}},
					},
				},
			},
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testpvcdisk-extra",
					Disk: &v1.Disk{
						Name: "testpvcdisk-extra",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "testpvcdiskclaim-extra",
						}},
					},
				},
			},
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testpvcdisk-extra2",
					Disk: &v1.Disk{
						Name: "testpvcdisk-extra2",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "testpvcdiskclaim-extra2",
						}},
					},
				},
			},
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "t-pvcdisk",
					Disk: &v1.Disk{
						Name: "t-pvcdisk",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "t-pvcdiskclaim",
						}},
					},
				},
			},
		},
			true),
	)

	DescribeTable("should validate VolumeRequest on offline vm", func(requests []v1.VirtualMachineVolumeRequest, isValid bool) {
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testdisk",
		})
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testpvcdisk",
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testdisk",
			VolumeSource: v1.VolumeSource{
				ContainerDisk: testutils.NewFakeContainerDiskSource(),
			},
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testpvcdisk",
			VolumeSource: v1.VolumeSource{
				PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: "testpvcdiskclaim",
				}},
			},
		})

		vm := &v1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vmi.Name,
				Namespace: vmi.Namespace,
			},
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
			Status: v1.VirtualMachineStatus{
				VolumeRequests: requests,
			},
		}

		resp := admitVm(vmsAdmitter, vm)
		Expect(resp.Allowed).To(Equal(isValid))
	},
		Entry("with valid request to add volume", []v1.VirtualMachineVolumeRequest{
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testdisk2",
					Disk: &v1.Disk{
						Name: "testdisk2",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "madeup",
						}},
					},
				},
			},
		},
			true),

		Entry("with invalid request to add the same volume twice", []v1.VirtualMachineVolumeRequest{
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testdisk2",
					Disk: &v1.Disk{
						Name: "testdisk2",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "madeup",
						}},
					},
				},
			},
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testdisk2",
					Disk: &v1.Disk{
						Name: "testdisk2",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "madeup",
						}},
					},
				},
			},
		},
			false),
		Entry("with invalid request to add volume that already exists", []v1.VirtualMachineVolumeRequest{
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testdisk",
					Disk: &v1.Disk{
						Name: "testdisk",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "madeup",
						}},
					},
				},
			},
		},
			false),

		Entry("with valid request to add the exact same volume that already exists.", []v1.VirtualMachineVolumeRequest{
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testdisk",
					Disk: &v1.Disk{
						Name: "testpvcdisk",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "testpvcdiskclaim",
						}},
					},
				},
			},
		},
			false),
		Entry("with valid request to remove volume", []v1.VirtualMachineVolumeRequest{
			{
				RemoveVolumeOptions: &v1.RemoveVolumeOptions{
					Name: "testdisk",
				},
			},
		},
			true),
		Entry("with invalid request to remove same volume twice", []v1.VirtualMachineVolumeRequest{
			{
				RemoveVolumeOptions: &v1.RemoveVolumeOptions{
					Name: "testdisk",
				},
			},
			{
				RemoveVolumeOptions: &v1.RemoveVolumeOptions{
					Name: "testdisk",
				},
			},
		},
			false),
		Entry("with invalid request with no options", []v1.VirtualMachineVolumeRequest{
			{},
		},
			false),
		Entry("with invalid request with multiple options", []v1.VirtualMachineVolumeRequest{
			{
				AddVolumeOptions: &v1.AddVolumeOptions{
					Name: "testdisk2",
					Disk: &v1.Disk{
						Name: "testdisk2",
						DiskDevice: v1.DiskDevice{
							Disk: &v1.DiskTarget{
								Bus: "scsi",
							},
						},
					},
					VolumeSource: &v1.HotplugVolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{
							ClaimName: "madeup",
						}},
					},
				},
				RemoveVolumeOptions: &v1.RemoveVolumeOptions{
					Name: "testdisk",
				},
			},
		},
			false),
	)

	It("should accept valid DataVolumeTemplate", func() {
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testdisk",
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testdisk",
			VolumeSource: v1.VolumeSource{
				DataVolume: &v1.DataVolumeSource{
					Name: "dv1",
				},
			},
		})

		vm := &v1.VirtualMachine{
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
		}

		vm.Spec.DataVolumeTemplates = append(vm.Spec.DataVolumeTemplates, v1.DataVolumeTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dv1",
			},
			Spec: cdiv1.DataVolumeSpec{
				PVC: &k8sv1.PersistentVolumeClaimSpec{},
			},
		})

		testutils.AddDataVolumeAPI(crdInformer)
		resp := admitVm(vmsAdmitter, vm)
		Expect(resp.Allowed).To(BeTrue())
	})

	It("should accept DataVolumeTemplate with deleted sourceRef if vm is going to be deleted", func() {
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testdisk",
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testdisk",
			VolumeSource: v1.VolumeSource{
				DataVolume: &v1.DataVolumeSource{
					Name: "dv1",
				},
			},
		})

		vm := &v1.VirtualMachine{
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
		}
		now := metav1.Now()
		vm.DeletionTimestamp = &now

		vm.Spec.DataVolumeTemplates = append(vm.Spec.DataVolumeTemplates, v1.DataVolumeTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dv1",
			},
			Spec: cdiv1.DataVolumeSpec{
				PVC: &k8sv1.PersistentVolumeClaimSpec{},
				SourceRef: &cdiv1.DataVolumeSourceRef{
					Kind: "DataSource",
					Name: "fakeName",
				},
			},
		})

		testutils.AddDataVolumeAPI(crdInformer)
		resp := admitVm(vmsAdmitter, vm)
		Expect(resp.Allowed).To(BeTrue())
	})

	It("should reject VM with DataVolumeTemplate in another namespace", func() {
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testdisk",
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testdisk",
			VolumeSource: v1.VolumeSource{
				DataVolume: &v1.DataVolumeSource{
					Name: "dv1",
				},
			},
		})

		vm := &v1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "vm-namespace",
			},
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
		}

		vm.Spec.DataVolumeTemplates = append(vm.Spec.DataVolumeTemplates, v1.DataVolumeTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dv1",
				Namespace: "another-namespace",
			},
			Spec: cdiv1.DataVolumeSpec{
				PVC: &k8sv1.PersistentVolumeClaimSpec{},
			},
		})

		testutils.AddDataVolumeAPI(crdInformer)
		resp := admitVm(vmsAdmitter, vm)
		Expect(resp.Allowed).To(BeFalse())
		Expect(resp.Result.Details.Causes[0].Message).To(Equal("Embedded DataVolume namespace another-namespace differs from VM namespace vm-namespace"))
	})

	It("should reject invalid DataVolumeTemplate with no Volume reference in VMI template", func() {
		vmi := api.NewMinimalVMI("testvmi")
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks, v1.Disk{
			Name: "testdisk",
		})
		vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
			Name: "testdisk",
			VolumeSource: v1.VolumeSource{
				DataVolume: &v1.DataVolumeSource{
					Name: "WRONG-DATAVOLUME",
				},
			},
		})

		vm := &v1.VirtualMachine{
			Spec: v1.VirtualMachineSpec{
				Running: &notRunning,
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
		}

		vm.Spec.DataVolumeTemplates = append(vm.Spec.DataVolumeTemplates, v1.DataVolumeTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dv1",
			},
			// this is needed as we have 'better' openapi spec now
			Spec: cdiv1.DataVolumeSpec{
				PVC: &k8sv1.PersistentVolumeClaimSpec{},
			},
		})

		testutils.AddDataVolumeAPI(crdInformer)
		resp := admitVm(vmsAdmitter, vm)
		Expect(resp.Allowed).To(BeFalse())
		Expect(resp.Result.Details.Causes).To(HaveLen(1))
		Expect(resp.Result.Details.Causes[0].Field).To(Equal("spec.dataVolumeTemplate[0]"))
	})

	Context("with Volume", func() {

		BeforeEach(func() {
			enableFeatureGate(virtconfig.HostDiskGate)
		})

		AfterEach(func() {
			disableFeatureGates()
		})

		DescribeTable("should accept valid volumes",
			func(volumeSource v1.VolumeSource) {
				vmi := api.NewMinimalVMI("testvmi")
				vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
					Name:         "testvolume",
					VolumeSource: volumeSource,
				})

				testutils.AddDataVolumeAPI(crdInformer)
				causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
				Expect(causes).To(BeEmpty())
			},
			Entry("with pvc volume source", v1.VolumeSource{PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{}}),
			Entry("with cloud-init volume source", v1.VolumeSource{CloudInitNoCloud: &v1.CloudInitNoCloudSource{UserData: "fake", NetworkData: "fake"}}),
			Entry("with containerDisk volume source", v1.VolumeSource{ContainerDisk: testutils.NewFakeContainerDiskSource()}),
			Entry("with ephemeral volume source", v1.VolumeSource{Ephemeral: &v1.EphemeralVolumeSource{}}),
			Entry("with emptyDisk volume source", v1.VolumeSource{EmptyDisk: &v1.EmptyDiskSource{}}),
			Entry("with dataVolume volume source", v1.VolumeSource{DataVolume: &v1.DataVolumeSource{Name: "fake"}}),
			Entry("with hostDisk volume source", v1.VolumeSource{HostDisk: &v1.HostDisk{Path: "fake", Type: v1.HostDiskExistsOrCreate}}),
			Entry("with configMap volume source", v1.VolumeSource{ConfigMap: &v1.ConfigMapVolumeSource{LocalObjectReference: k8sv1.LocalObjectReference{Name: "fake"}}}),
			Entry("with secret volume source", v1.VolumeSource{Secret: &v1.SecretVolumeSource{SecretName: "fake"}}),
			Entry("with serviceAccount volume source", v1.VolumeSource{ServiceAccount: &v1.ServiceAccountVolumeSource{ServiceAccountName: "fake"}}),
		)
		It("should reject DataVolume when feature gate is disabled", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				Name:         "testvolume",
				VolumeSource: v1.VolumeSource{DataVolume: &v1.DataVolumeSource{Name: "fake"}},
			})

			testutils.RemoveDataVolumeAPI(crdInformer)
			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0]"))
		})
		It("should reject DataVolume when DataVolume name is not set", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				Name:         "testvolume",
				VolumeSource: v1.VolumeSource{DataVolume: &v1.DataVolumeSource{Name: ""}},
			})

			testutils.AddDataVolumeAPI(crdInformer)
			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(string(causes[0].Type)).To(Equal("FieldValueRequired"))
			Expect(causes[0].Field).To(Equal("fake[0].name"))
			Expect(causes[0].Message).To(Equal("DataVolume 'name' must be set"))
		})
		It("should reject volume with no volume source set", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				Name: "testvolume",
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0]"))
		})
		It("should reject volume with multiple volume sources set", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				Name: "testvolume",
				VolumeSource: v1.VolumeSource{
					ContainerDisk:         testutils.NewFakeContainerDiskSource(),
					PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0]"))
		})
		It("should reject volumes with duplicate names", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				Name: "testvolume",
				VolumeSource: v1.VolumeSource{
					ContainerDisk: testutils.NewFakeContainerDiskSource(),
				},
			})
			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				Name: "testvolume",
				VolumeSource: v1.VolumeSource{
					ContainerDisk: testutils.NewFakeContainerDiskSource(),
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[1].name"))
		})

		DescribeTable("should verify cloud-init userdata length", func(userDataLen int, expectedErrors int, base64Encode bool) {
			vmi := api.NewMinimalVMI("testvmi")

			// generate fake userdata
			userdata := ""
			for i := 0; i < userDataLen; i++ {
				userdata = fmt.Sprintf("%sa", userdata)
			}

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{VolumeSource: v1.VolumeSource{CloudInitNoCloud: &v1.CloudInitNoCloudSource{}}})

			if base64Encode {
				vmi.Spec.Volumes[0].VolumeSource.CloudInitNoCloud.UserDataBase64 = base64.StdEncoding.EncodeToString([]byte(userdata))
			} else {
				vmi.Spec.Volumes[0].VolumeSource.CloudInitNoCloud.UserData = userdata
			}

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(expectedErrors))
			for _, cause := range causes {
				Expect(cause.Field).To(ContainSubstring("fake[0].cloudInitNoCloud"))
			}
		},
			Entry("should accept userdata under max limit", 10, 0, false),
			Entry("should accept userdata equal max limit", cloudInitUserMaxLen, 0, false),
			Entry("should reject userdata greater than max limit", cloudInitUserMaxLen+1, 1, false),
			Entry("should accept userdata base64 under max limit", 10, 0, true),
			Entry("should accept userdata base64 equal max limit", cloudInitUserMaxLen, 0, true),
			Entry("should reject userdata base64 greater than max limit", cloudInitUserMaxLen+1, 1, true),
		)

		DescribeTable("should verify cloud-init networkdata length", func(networkDataLen int, expectedErrors int, base64Encode bool) {
			vmi := api.NewMinimalVMI("testvmi")

			// generate fake networkdata
			networkdata := ""
			for i := 0; i < networkDataLen; i++ {
				networkdata = fmt.Sprintf("%sa", networkdata)
			}

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{VolumeSource: v1.VolumeSource{CloudInitNoCloud: &v1.CloudInitNoCloudSource{}}})
			vmi.Spec.Volumes[0].VolumeSource.CloudInitNoCloud.UserData = "#config"

			if base64Encode {
				vmi.Spec.Volumes[0].VolumeSource.CloudInitNoCloud.NetworkDataBase64 = base64.StdEncoding.EncodeToString([]byte(networkdata))
			} else {
				vmi.Spec.Volumes[0].VolumeSource.CloudInitNoCloud.NetworkData = networkdata
			}

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(expectedErrors))
			for _, cause := range causes {
				Expect(cause.Field).To(ContainSubstring("fake[0].cloudInitNoCloud"))
			}
		},
			Entry("should accept networkdata under max limit", 10, 0, false),
			Entry("should accept networkdata equal max limit", cloudInitNetworkMaxLen, 0, false),
			Entry("should reject networkdata greater than max limit", cloudInitNetworkMaxLen+1, 1, false),
			Entry("should accept networkdata base64 under max limit", 10, 0, true),
			Entry("should accept networkdata base64 equal max limit", cloudInitNetworkMaxLen, 0, true),
			Entry("should reject networkdata base64 greater than max limit", cloudInitNetworkMaxLen+1, 1, true),
		)

		It("should reject cloud-init with invalid base64 userdata", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				VolumeSource: v1.VolumeSource{
					CloudInitNoCloud: &v1.CloudInitNoCloudSource{
						UserDataBase64: "#######garbage******",
					},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0].cloudInitNoCloud.userDataBase64"))
		})

		It("should reject cloud-init with invalid base64 networkdata", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				VolumeSource: v1.VolumeSource{
					CloudInitNoCloud: &v1.CloudInitNoCloudSource{
						UserData:          "fake",
						NetworkDataBase64: "#######garbage******",
					},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0].cloudInitNoCloud.networkDataBase64"))
		})

		It("should reject cloud-init with multiple userdata sources", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				VolumeSource: v1.VolumeSource{
					CloudInitNoCloud: &v1.CloudInitNoCloudSource{
						UserData: "fake",
						UserDataSecretRef: &k8sv1.LocalObjectReference{
							Name: "fake",
						},
					},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0].cloudInitNoCloud"))
		})

		It("should reject cloud-init with multiple networkdata sources", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				VolumeSource: v1.VolumeSource{
					CloudInitNoCloud: &v1.CloudInitNoCloudSource{
						UserData:    "fake",
						NetworkData: "fake",
						NetworkDataSecretRef: &k8sv1.LocalObjectReference{
							Name: "fake",
						},
					},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0].cloudInitNoCloud"))
		})

		It("should reject hostDisk without required parameters", func() {
			vmi := api.NewMinimalVMI("testvmi")
			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				VolumeSource: v1.VolumeSource{
					HostDisk: &v1.HostDisk{},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(2))
			Expect(causes[0].Field).To(Equal("fake[0].hostDisk.path"))
			Expect(causes[1].Field).To(Equal("fake[0].hostDisk.type"))
		})

		It("should reject hostDisk without given 'path'", func() {
			vmi := api.NewMinimalVMI("testvmi")
			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				VolumeSource: v1.VolumeSource{
					HostDisk: &v1.HostDisk{
						Type: v1.HostDiskExistsOrCreate,
					},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0].hostDisk.path"))
		})

		It("should reject hostDisk with invalid type", func() {
			vmi := api.NewMinimalVMI("testvmi")
			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				VolumeSource: v1.VolumeSource{
					HostDisk: &v1.HostDisk{
						Path: "fakePath",
						Type: "fakeType",
					},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0].hostDisk.type"))
		})

		It("should reject hostDisk when the capacity is specified with a `DiskExists` type", func() {
			vmi := api.NewMinimalVMI("testvmi")
			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				VolumeSource: v1.VolumeSource{
					HostDisk: &v1.HostDisk{
						Path:     "fakePath",
						Type:     v1.HostDiskExists,
						Capacity: resource.MustParse("1Gi"),
					},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0].hostDisk.capacity"))
		})

		It("should reject a configMap without the configMapName field", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				VolumeSource: v1.VolumeSource{
					ConfigMap: &v1.ConfigMapVolumeSource{},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0].configMap.name"))
		})

		It("should reject a secret without the secretName field", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				VolumeSource: v1.VolumeSource{
					Secret: &v1.SecretVolumeSource{},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0].secret.secretName"))
		})

		It("should reject a serviceAccount without the serviceAccountName field", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				VolumeSource: v1.VolumeSource{
					ServiceAccount: &v1.ServiceAccountVolumeSource{},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake[0].serviceAccount.serviceAccountName"))
		})

		It("should reject multiple serviceAccounts", func() {
			vmi := api.NewMinimalVMI("testvmi")

			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				Name: "sa1",
				VolumeSource: v1.VolumeSource{
					ServiceAccount: &v1.ServiceAccountVolumeSource{ServiceAccountName: "test1"},
				},
			})
			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				Name: "sa2",
				VolumeSource: v1.VolumeSource{
					ServiceAccount: &v1.ServiceAccountVolumeSource{ServiceAccountName: "test2"},
				},
			})

			causes := validateVolumes(k8sfield.NewPath("fake"), vmi.Spec.Volumes, config)
			Expect(causes).To(HaveLen(1))
			Expect(causes[0].Field).To(Equal("fake"))
		})

		DescribeTable("should successfully authorize clone", func(arNamespace, vmNamespace, sourceNamespace,
			serviceAccount, expectedSourceNamespace, expectedTargetNamespace, expectedServiceAccount string) {

			vm := &v1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: vmNamespace,
				},
				Spec: v1.VirtualMachineSpec{
					Template: &v1.VirtualMachineInstanceTemplateSpec{},
					DataVolumeTemplates: []v1.DataVolumeTemplateSpec{
						{
							ObjectMeta: metav1.ObjectMeta{
								Name: "whatever",
							},
							Spec: cdiv1.DataVolumeSpec{
								Source: &cdiv1.DataVolumeSource{
									PVC: &cdiv1.DataVolumeSourcePVC{
										Name:      "whocares",
										Namespace: sourceNamespace,
									},
								},
							},
						},
					},
				},
			}

			if serviceAccount != "" {
				vm.Spec.Template.Spec.Volumes = []v1.Volume{
					{
						VolumeSource: v1.VolumeSource{
							ServiceAccount: &v1.ServiceAccountVolumeSource{
								ServiceAccountName: serviceAccount,
							},
						},
					},
				}
			}

			ar := &admissionv1.AdmissionRequest{
				Namespace: arNamespace,
			}

			vmsAdmitter.cloneAuthFunc = makeCloneAdmitFunc(k8sClient, expectedSourceNamespace, "whocares",
				expectedTargetNamespace, expectedServiceAccount)
			causes, err := vmsAdmitter.authorizeVirtualMachineSpec(ar, vm)
			Expect(err).ToNot(HaveOccurred())
			Expect(causes).To(BeEmpty())
		},
			Entry("when source namespace suppied", "ns1", "", "ns3", "", "ns3", "ns1", "default"),
			Entry("when vm namespace suppied and source not", "ns1", "ns2", "", "", "ns2", "ns2", "default"),
			Entry("when ar namespace suppied and vm/source not", "ns1", "", "", "", "ns1", "ns1", "default"),
			Entry("when everything suppied with default service account", "ns1", "ns2", "ns3", "", "ns3", "ns2", "default"),
			Entry("when everything suppied with 'sa' service account", "ns1", "ns2", "ns3", "sa", "ns3", "ns2", "sa"),
		)

		DescribeTable("should successfully authorize clone from sourceRef", func(arNamespace,
			vmNamespace,
			sourceRefNamespace,
			sourceNamespace,
			serviceAccount,
			expectedSourceNamespace,
			expectedTargetNamespace,
			expectedServiceAccount string) {

			sourceRefName := "sourceRef"
			ds := &cdiv1.DataSource{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: vmNamespace,
					Name:      sourceRefName,
				},
				Spec: cdiv1.DataSourceSpec{
					Source: cdiv1.DataSourceSource{
						PVC: &cdiv1.DataVolumeSourcePVC{
							Name: "whocares",
						},
					},
				},
			}

			vm := &v1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: vmNamespace,
				},
				Spec: v1.VirtualMachineSpec{
					Template: &v1.VirtualMachineInstanceTemplateSpec{},
					DataVolumeTemplates: []v1.DataVolumeTemplateSpec{
						{
							ObjectMeta: metav1.ObjectMeta{
								Name: "whatever",
							},
							Spec: cdiv1.DataVolumeSpec{
								SourceRef: &cdiv1.DataVolumeSourceRef{
									Kind: "DataSource",
									Name: sourceRefName,
								},
							},
						},
					},
				},
			}

			if sourceRefNamespace != "" {
				ds.Namespace = sourceRefNamespace
				vm.Spec.DataVolumeTemplates[0].Spec.SourceRef.Namespace = &sourceRefNamespace
			}

			if sourceNamespace != "" {
				ds.Spec.Source.PVC.Namespace = sourceNamespace
			}

			if serviceAccount != "" {
				vm.Spec.Template.Spec.Volumes = []v1.Volume{
					{
						VolumeSource: v1.VolumeSource{
							ServiceAccount: &v1.ServiceAccountVolumeSource{
								ServiceAccountName: serviceAccount,
							},
						},
					},
				}
			}

			ar := &admissionv1.AdmissionRequest{
				Namespace: arNamespace,
			}

			vmsAdmitter.DataSourceInformer.GetIndexer().Add(ds)

			vmsAdmitter.cloneAuthFunc = makeCloneAdmitFunc(k8sClient, expectedSourceNamespace,
				"whocares",
				expectedTargetNamespace,
				expectedServiceAccount)

			causes, err := vmsAdmitter.authorizeVirtualMachineSpec(ar, vm)
			Expect(err).ToNot(HaveOccurred())
			Expect(causes).To(BeEmpty())
		},
			Entry("when source namespace suppied", "ns1", "", "ns2", "ns3", "", "ns3", "ns1", "default"),
			Entry("when vm namespace suppied and source not", "ns1", "ns2", "", "", "", "ns2", "ns2", "default"),
			Entry("when everything suppied with default service account", "ns1", "ns2", "", "ns3", "", "ns3", "ns2", "default"),
			Entry("when everything suppied with 'sa' service account", "ns1", "ns2", "", "ns3", "sa", "ns3", "ns2", "sa"),
			Entry("when source namespace and sourceRef namespace suppied", "ns1", "", "foo", "ns3", "", "ns3", "ns1", "default"),
			Entry("when vm namespace and sourceRef namespace suppied and source not", "ns1", "ns2", "foo", "", "", "foo", "ns2", "default"),
			Entry("when ar namespace and sourceRef namespace suppied and vm/source not", "ns1", "", "foo", "", "", "foo", "ns1", "default"),
			Entry("when everything and sourceRef suppied with default service account", "ns1", "ns2", "foo", "ns3", "", "ns3", "ns2", "default"),
			Entry("when everything and sourceRef suppied with 'sa' service account", "ns1", "ns2", "foo", "ns3", "sa", "ns3", "ns2", "sa"),
		)

		DescribeTable("should deny clone", func(sourceNamespace, sourceName, failMessage string, failErr error, expectedMessage string) {
			vm := &v1.VirtualMachine{
				Spec: v1.VirtualMachineSpec{
					Template: &v1.VirtualMachineInstanceTemplateSpec{},
					DataVolumeTemplates: []v1.DataVolumeTemplateSpec{
						{
							ObjectMeta: metav1.ObjectMeta{
								Name: "whatever",
							},
							Spec: cdiv1.DataVolumeSpec{
								Source: &cdiv1.DataVolumeSource{
									PVC: &cdiv1.DataVolumeSourcePVC{
										Name:      sourceName,
										Namespace: sourceNamespace,
									},
								},
							},
						},
					},
				},
			}

			ar := &admissionv1.AdmissionRequest{}

			vmsAdmitter.cloneAuthFunc = makeCloneAdmitFailFunc(failMessage, failErr)
			causes, err := vmsAdmitter.authorizeVirtualMachineSpec(ar, vm)
			if failErr != nil {
				Expect(err).To(Equal(failErr))
			} else {
				Expect(err).ToNot(HaveOccurred())
				Expect(causes).To(HaveLen(1))
				Expect(causes[0].Message).To(Equal(expectedMessage))
			}
		},
			Entry("when user not authorized", "sourceNamespace", "sourceName", "no permission", nil, "Authorization failed, message is: no permission"),
			Entry("error occurs", "sourceNamespace", "sourceName", "", fmt.Errorf("bad error"), ""),
		)
	})

	DescribeTable("when snapshot is in progress, should", func(mutateFn func(*v1.VirtualMachine) bool) {
		vmi := api.NewMinimalVMI("testvmi")
		vm := &v1.VirtualMachine{
			Spec: v1.VirtualMachineSpec{
				Running: &[]bool{false}[0],
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
			Status: v1.VirtualMachineStatus{
				SnapshotInProgress: &[]string{"testsnapshot"}[0],
			},
		}
		oldObjectBytes, _ := json.Marshal(vm)

		allow := mutateFn(vm)
		objectBytes, _ := json.Marshal(vm)

		ar := &admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				Operation: admissionv1.Update,
				Resource:  webhooks.VirtualMachineGroupVersionResource,
				OldObject: runtime.RawExtension{
					Raw: oldObjectBytes,
				},
				Object: runtime.RawExtension{
					Raw: objectBytes,
				},
			},
		}

		resp := vmsAdmitter.Admit(ar)
		Expect(resp.Allowed).To(Equal(allow))

		if !allow {
			Expect(resp.Result.Details.Causes).To(HaveLen(1))
			Expect(resp.Result.Details.Causes[0].Field).To(Equal("spec"))
		}
	},
		Entry("reject update to spec", func(vm *v1.VirtualMachine) bool {
			vm.Spec.Running = &[]bool{true}[0]
			return false
		}),
		Entry("accept update to metadata", func(vm *v1.VirtualMachine) bool {
			vm.Annotations = map[string]string{"foo": "bar"}
			return true
		}),
		Entry("accept update to status", func(vm *v1.VirtualMachine) bool {
			vm.Status.Ready = true
			return true
		}),
	)

	DescribeTable("when restore is in progress, should", func(mutateFn func(*v1.VirtualMachine) bool, updateRunStrategy bool) {
		vmi := api.NewMinimalVMI("testvmi")
		vm := &v1.VirtualMachine{
			Spec: v1.VirtualMachineSpec{
				Template: &v1.VirtualMachineInstanceTemplateSpec{
					Spec: vmi.Spec,
				},
			},
			Status: v1.VirtualMachineStatus{
				RestoreInProgress: &[]string{"testrestore"}[0],
			},
		}
		if updateRunStrategy {
			vm.Spec.RunStrategy = &runStrategyHalted
		} else {
			vm.Spec.Running = &[]bool{false}[0]
		}
		oldObjectBytes, _ := json.Marshal(vm)

		allow := mutateFn(vm)
		objectBytes, _ := json.Marshal(vm)

		ar := &admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				Operation: admissionv1.Update,
				Resource:  webhooks.VirtualMachineGroupVersionResource,
				OldObject: runtime.RawExtension{
					Raw: oldObjectBytes,
				},
				Object: runtime.RawExtension{
					Raw: objectBytes,
				},
			},
		}

		resp := vmsAdmitter.Admit(ar)
		Expect(resp.Allowed).To(Equal(allow))

		if !allow {
			Expect(resp.Result.Details.Causes).To(HaveLen(1))
			Expect(resp.Result.Details.Causes[0].Field).To(Equal("spec"))
		}
	},
		Entry("reject update to running true", func(vm *v1.VirtualMachine) bool {
			vm.Spec.Running = &[]bool{true}[0]
			return false
		}, false),
		Entry("reject update of runStrategy", func(vm *v1.VirtualMachine) bool {
			vm.Spec.RunStrategy = &runStrategyManual
			return false
		}, true),
		Entry("accept update to spec except running true", func(vm *v1.VirtualMachine) bool {
			vm.Spec.Template = &v1.VirtualMachineInstanceTemplateSpec{}
			return true
		}, false),
		Entry("accept update to metadata", func(vm *v1.VirtualMachine) bool {
			vm.Annotations = map[string]string{"foo": "bar"}
			return true
		}, false),
		Entry("accept update to status", func(vm *v1.VirtualMachine) bool {
			vm.Status.Ready = true
			return true
		}, false),
	)

	Context("Instancetype", func() {
		var (
			vm *v1.VirtualMachine
		)

		BeforeEach(func() {
			vmi := api.NewMinimalVMI("testvmi")
			vm = &v1.VirtualMachine{
				Spec: v1.VirtualMachineSpec{
					Instancetype: &v1.InstancetypeMatcher{
						Name: "test",
						Kind: instancetypeapi.SingularResourceName,
					},
					Preference: &v1.PreferenceMatcher{
						Name: "test",
						Kind: instancetypeapi.SingularPreferenceResourceName,
					},
					Running: &notRunning,
					Template: &v1.VirtualMachineInstanceTemplateSpec{
						Spec: vmi.Spec,
					},
				},
			}
		})

		It("should reject if instancetype is not found", func() {
			instancetypeMethods.FindInstancetypeSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachineInstancetypeSpec, error) {
				return nil, fmt.Errorf("instancetype not found")
			}

			response := admitVm(vmsAdmitter, vm)
			Expect(response.Allowed).To(BeFalse())
			Expect(response.Result.Details.Causes).To(HaveLen(1))
			Expect(response.Result.Details.Causes[0].Type).To(Equal(metav1.CauseTypeFieldValueNotFound))
			Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.instancetype"))
		})

		It("should reject if preference is not found", func() {
			instancetypeMethods.FindPreferenceSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachinePreferenceSpec, error) {
				return nil, fmt.Errorf("preference not found")
			}

			response := admitVm(vmsAdmitter, vm)
			Expect(response.Allowed).To(BeFalse())
			Expect(response.Result.Details.Causes).To(HaveLen(1))
			Expect(response.Result.Details.Causes[0].Type).To(Equal(metav1.CauseTypeFieldValueNotFound))
			Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.preference"))
		})

		It("should reject if instancetype fails to apply to VMI", func() {
			var (
				basePath = k8sfield.NewPath("spec", "template", "spec")
				path1    = basePath.Child("example", "path")
				path2    = basePath.Child("domain", "example", "path")
			)
			instancetypeMethods.FindInstancetypeSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachineInstancetypeSpec, error) {
				return &instancetypev1beta1.VirtualMachineInstancetypeSpec{}, nil
			}
			instancetypeMethods.FindInstancetypeSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachineInstancetypeSpec, error) {
				return &instancetypev1beta1.VirtualMachineInstancetypeSpec{}, nil
			}
			instancetypeMethods.FindPreferenceSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachinePreferenceSpec, error) {
				return &instancetypev1beta1.VirtualMachinePreferenceSpec{}, nil
			}
			instancetypeMethods.ApplyToVmiFunc = func(_ *k8sfield.Path, _ *instancetypev1beta1.VirtualMachineInstancetypeSpec, _ *instancetypev1beta1.VirtualMachinePreferenceSpec, _ *v1.VirtualMachineInstanceSpec) instancetype.Conflicts {
				return instancetype.Conflicts{path1, path2}
			}

			response := admitVm(vmsAdmitter, vm)
			Expect(response.Allowed).To(BeFalse())
			Expect(response.Result.Details.Causes).To(HaveLen(2))
			Expect(response.Result.Details.Causes[0].Type).To(Equal(metav1.CauseTypeFieldValueInvalid))
			Expect(response.Result.Details.Causes[0].Field).To(Equal(path1.String()))
			Expect(response.Result.Details.Causes[1].Type).To(Equal(metav1.CauseTypeFieldValueInvalid))
			Expect(response.Result.Details.Causes[1].Field).To(Equal(path2.String()))
		})

		It("should apply instancetype to VMI before validating VMI", func() {
			// Test that VMI without instancetype application is valid
			response := admitVm(vmsAdmitter, vm)
			Expect(response.Allowed).To(BeTrue())

			// Instancetype application sets invalid memory value
			instancetypeMethods.FindInstancetypeSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachineInstancetypeSpec, error) {
				return &instancetypev1beta1.VirtualMachineInstancetypeSpec{}, nil
			}
			instancetypeMethods.ApplyToVmiFunc = func(_ *k8sfield.Path, _ *instancetypev1beta1.VirtualMachineInstancetypeSpec, _ *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *v1.VirtualMachineInstanceSpec) instancetype.Conflicts {
				vmiSpec.Domain.Resources.Requests[k8sv1.ResourceMemory] = resource.MustParse("-1Mi")
				return nil
			}

			// Test that VMI fails
			response = admitVm(vmsAdmitter, vm)
			Expect(response.Allowed).To(BeFalse())
			Expect(response.Result.Details.Causes).To(HaveLen(1))
			Expect(response.Result.Details.Causes[0].Field).
				To(Equal("spec.template.spec.domain.resources.requests.memory"))
		})

		It("should not apply instancetype to the VMISpec of the original VM", func() {

			instancetypeMethods.FindInstancetypeSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachineInstancetypeSpec, error) {
				return &instancetypev1beta1.VirtualMachineInstancetypeSpec{}, nil
			}

			// Mock out ApplyToVmiFunc so that it applies some changes to the CPU of the provided VMISpec
			instancetypeMethods.ApplyToVmiFunc = func(_ *k8sfield.Path, _ *instancetypev1beta1.VirtualMachineInstancetypeSpec, _ *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *v1.VirtualMachineInstanceSpec) instancetype.Conflicts {
				vmiSpec.Domain.CPU = &v1.CPU{Cores: 1, Threads: 1, Sockets: 1}
				return nil
			}

			// Nil out CPU within the DomainSpec of the VMISpec being admitted to assert this remains untouched
			vm.Spec.Template.Spec.Domain.CPU = nil

			// The VM should be admitted successfully
			response := admitVm(vmsAdmitter, vm)
			Expect(response.Allowed).To(BeTrue())

			// Ensure CPU has remained nil within the now admitted VMISpec
			Expect(vm.Spec.Template.Spec.Domain.CPU).To(BeNil())

		})

		It("should reject if preference requirements are not met", func() {
			instancetypeMethods.FindPreferenceSpecFunc = func(_ *v1.VirtualMachine) (*instancetypev1beta1.VirtualMachinePreferenceSpec, error) {
				return &instancetypev1beta1.VirtualMachinePreferenceSpec{}, nil
			}
			instancetypeMethods.CheckPreferenceRequirementsFunc = func(_ *instancetypev1beta1.VirtualMachineInstancetypeSpec, _ *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *v1.VirtualMachineInstanceSpec) (*k8sfield.Path, error) {
				return k8sfield.NewPath("spec", "instancetype"), fmt.Errorf("requirements not met")
			}
			response := admitVm(vmsAdmitter, vm)
			Expect(response.Allowed).To(BeFalse())
			Expect(response.Result.Details.Causes).To(HaveLen(1))
			Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.instancetype"))
			Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring("failure checking preference requirements"))
		})
	})

	Context("Live update features", func() {
		Context("CPU", func() {
			var vm *v1.VirtualMachine
			const maximumSockets uint32 = 24

			BeforeEach(func() {
				vmi := api.NewMinimalVMI("testvmi")
				enableFeatureGate(virtconfig.VMLiveUpdateFeaturesGate)
				vm = &v1.VirtualMachine{
					Spec: v1.VirtualMachineSpec{
						LiveUpdateFeatures: &v1.LiveUpdateFeatures{
							CPU: &v1.LiveUpdateCPU{
								MaxSockets: pointer.P(maximumSockets),
							},
						},
						Running: &notRunning,
						Template: &v1.VirtualMachineInstanceTemplateSpec{
							Spec: vmi.Spec,
						},
					},
				}
			})
			Context("feature gate disabled", func() {
				It("should reject when the feature gate is disabled", func() {
					disableFeatureGates()
					response := admitVm(vmsAdmitter, vm)
					Expect(response.Allowed).To(BeFalse())
					Expect(response.Result.Details.Causes).To(HaveLen(1))
					Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.liveUpdateFeatures"))
					Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring(fmt.Sprintf("%s feature gate is not enabled", virtconfig.VMLiveUpdateFeaturesGate)))
				})
			})

			It("should reject configuration of maxSockets in VM template", func() {
				vm.Spec.Template.Spec.Domain.CPU = &v1.CPU{
					MaxSockets: 1,
				}

				response := admitVm(vmsAdmitter, vm)
				Expect(response.Allowed).To(BeFalse())
				Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.template.spec.domain.cpu.maxSockets"))
				Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring(""))
			})

			It("should reject VM creation when VM has instance type assigned", func() {
				vm.Spec.Instancetype = &v1.InstancetypeMatcher{
					Name: "foobar",
				}
				response := admitVm(vmsAdmitter, vm)
				Expect(response.Allowed).To(BeFalse())
				Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.liveUpdateFeatures"))
				Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring("Live update features cannot be used when instance type is configured"))
			})

			It("should reject VM creation when number of sockets exceeds the maximum configured", func() {
				vm.Spec.Template.Spec.Domain.CPU = &v1.CPU{
					Sockets: maximumSockets + 1,
				}
				response := admitVm(vmsAdmitter, vm)
				Expect(response.Allowed).To(BeFalse())
				Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.liveUpdateFeatures"))
				Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring("Number of sockets in CPU topology is greater than the maximum sockets allowed"))
			})

			It("should reject VM creation when resource requests are configured", func() {
				vm.Spec.Template.Spec.Domain.Resources.Requests = make(k8sv1.ResourceList)
				vm.Spec.Template.Spec.Domain.Resources.Requests[k8sv1.ResourceCPU] = resource.MustParse("400m")

				response := admitVm(vmsAdmitter, vm)
				Expect(response.Allowed).To(BeFalse())
				Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.liveUpdateFeatures"))
				Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring("Configuration of CPU resource requirements is not allowed when CPU live update is enabled"))
			})

			It("should reject VM creation when resource limits are configured", func() {
				vm.Spec.Template.Spec.Domain.Resources.Limits = make(k8sv1.ResourceList)
				vm.Spec.Template.Spec.Domain.Resources.Limits[k8sv1.ResourceCPU] = resource.MustParse("400m")

				response := admitVm(vmsAdmitter, vm)
				Expect(response.Allowed).To(BeFalse())
				Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.liveUpdateFeatures"))
				Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring("Configuration of CPU resource requirements is not allowed when CPU live update is enabled"))
			})

			When("Hot CPU change is in progress", func() {
				BeforeEach(func() {
					vm.Status.Ready = true
				})

				It("should reject updating CPU sockets while CPU hot update is enabled ", func() {
					vmi := api.NewMinimalVMI("testvmi")
					newCondition := v1.VirtualMachineInstanceCondition{
						Type:               v1.VirtualMachineInstanceVCPUChange,
						LastTransitionTime: metav1.Now(),
						Status:             k8sv1.ConditionTrue,
					}
					vmi.Status.Conditions = append(vmi.Status.Conditions, newCondition)
					vm.ObjectMeta = metav1.ObjectMeta{
						Name:      vmi.Name,
						Namespace: vmi.Namespace,
					}

					vm.Spec.Template.Spec.Domain.CPU = &v1.CPU{
						Cores:   2,
						Sockets: 1,
					}
					oldVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					vm.Spec.Template.Spec.Domain.CPU.Sockets++
					newVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					ar := &admissionv1.AdmissionReview{
						Request: &admissionv1.AdmissionRequest{
							Resource: webhooks.VirtualMachineGroupVersionResource,
							Object: runtime.RawExtension{
								Raw: newVMBytes,
							},
							OldObject: runtime.RawExtension{
								Raw: oldVMBytes,
							},
							Operation: admissionv1.Update,
						},
					}

					virtClient.EXPECT().VirtualMachineInstance(gomock.Any()).Return(mockVMIClient)
					mockVMIClient.EXPECT().Get(context.Background(), vmi.Name, gomock.Any()).Return(vmi, nil)
					response := vmsAdmitter.Admit(ar)
					Expect(response.Allowed).To(BeFalse())
					Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.template.spec.domain.cpu.sockets"))
					Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring("cannot update CPU sockets while another CPU change is in progress"))
				})
			})
			When("VMI is migratng", func() {

				BeforeEach(func() {
					vm.Status = v1.VirtualMachineStatus{
						Ready: true,
					}
				})
				It("should reject updating CPU Sockets while VMI is migrating ", func() {
					now := metav1.Now()
					vmi := api.NewMinimalVMI("testvmi")
					vmi.Status = v1.VirtualMachineInstanceStatus{
						MigrationState: &v1.VirtualMachineInstanceMigrationState{
							StartTimestamp: &now,
						},
					}
					vm.ObjectMeta = metav1.ObjectMeta{
						Name:      vmi.Name,
						Namespace: vmi.Namespace,
					}
					vm.Spec.Template.Spec.Domain.CPU = &v1.CPU{
						Cores:   2,
						Sockets: 1,
					}
					oldVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					vm.Spec.Template.Spec.Domain.CPU.Sockets++
					newVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					ar := &admissionv1.AdmissionReview{
						Request: &admissionv1.AdmissionRequest{
							Resource: webhooks.VirtualMachineGroupVersionResource,
							Object: runtime.RawExtension{
								Raw: newVMBytes,
							},
							OldObject: runtime.RawExtension{
								Raw: oldVMBytes,
							},
							Operation: admissionv1.Update,
						},
					}

					virtClient.EXPECT().VirtualMachineInstance(gomock.Any()).Return(mockVMIClient)
					mockVMIClient.EXPECT().Get(context.Background(), vmi.Name, gomock.Any()).Return(vmi, nil)
					response := vmsAdmitter.Admit(ar)
					Expect(response.Allowed).To(BeFalse())
					Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.template.spec.domain.cpu.sockets"))
					Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring("cannot update CPU sockets while VMI migration is in progress"))
				})
				It("should reject updating CPU Sockets while VMIMigration exist ", func() {
					vmi := api.NewMinimalVMI("testvmi")

					inFlightMigration := v1.VirtualMachineInstanceMigration{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: vmi.Namespace,
						},
						Spec: v1.VirtualMachineInstanceMigrationSpec{
							VMIName: vmi.Name,
						},
					}
					vm.ObjectMeta = metav1.ObjectMeta{
						Name:      vmi.Name,
						Namespace: vmi.Namespace,
					}
					vm.Spec.Template.Spec.Domain.CPU = &v1.CPU{
						Cores:   2,
						Sockets: 1,
					}
					oldVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					vm.Spec.Template.Spec.Domain.CPU.Sockets++
					newVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					ar := &admissionv1.AdmissionReview{
						Request: &admissionv1.AdmissionRequest{
							Resource: webhooks.VirtualMachineGroupVersionResource,
							Object: runtime.RawExtension{
								Raw: newVMBytes,
							},
							OldObject: runtime.RawExtension{
								Raw: oldVMBytes,
							},
							Operation: admissionv1.Update,
						},
					}
					virtClient.EXPECT().VirtualMachineInstance(gomock.Any()).Return(mockVMIClient)
					mockVMIClient.EXPECT().Get(context.Background(), inFlightMigration.Spec.VMIName, gomock.Any()).Return(vmi, nil)
					virtClient.EXPECT().VirtualMachineInstanceMigration(gomock.Any()).Return(migrationInterface)
					migrationInterface.EXPECT().List(gomock.Any()).Return(kubecli.NewMigrationList(inFlightMigration), nil).AnyTimes()

					response := vmsAdmitter.Admit(ar)
					Expect(response.Allowed).To(BeFalse())
					Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.template.spec.domain.cpu.sockets"))
					Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring("cannot update CPU sockets while VMI migration is in progress: in-flight migration detected"))
				})
			})
			When("VM is running", func() {
				BeforeEach(func() {
					vm.Status = v1.VirtualMachineStatus{
						Ready: true,
					}
				})

				It("should reject updating of VM live update features", func() {
					oldVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					vm.Spec.LiveUpdateFeatures.CPU = &v1.LiveUpdateCPU{}
					newVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					ar := &admissionv1.AdmissionReview{
						Request: &admissionv1.AdmissionRequest{
							Resource: webhooks.VirtualMachineGroupVersionResource,
							Object: runtime.RawExtension{
								Raw: newVMBytes,
							},
							OldObject: runtime.RawExtension{
								Raw: oldVMBytes,
							},
							Operation: admissionv1.Update,
						},
					}

					response := vmsAdmitter.Admit(ar)
					Expect(response.Allowed).To(BeFalse())
					Expect(response.Result.Details.Causes[0].Field).To(Equal("spec"))
					Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring("Cannot update VM live features while VM is running"))
				})

				It("should reject updating CPU cores while CPU update feature is enabled ", func() {
					vm.Spec.Template.Spec.Domain.CPU = &v1.CPU{
						Cores: 8,
					}
					oldVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					vm.Spec.Template.Spec.Domain.CPU.Cores = 16
					newVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					ar := &admissionv1.AdmissionReview{
						Request: &admissionv1.AdmissionRequest{
							Resource: webhooks.VirtualMachineGroupVersionResource,
							Object: runtime.RawExtension{
								Raw: newVMBytes,
							},
							OldObject: runtime.RawExtension{
								Raw: oldVMBytes,
							},
							Operation: admissionv1.Update,
						},
					}

					response := vmsAdmitter.Admit(ar)
					Expect(response.Allowed).To(BeFalse())
					Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.template.spec.domain.cpu.cores"))
					Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring("Cannot update CPU cores while live update features configured"))
				})

				It("should reject updating CPU threads while CPU update feature is enabled", func() {
					vm.Spec.Template.Spec.Domain.CPU = &v1.CPU{
						Threads: 8,
					}
					oldVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					vm.Spec.Template.Spec.Domain.CPU.Threads = 16
					newVMBytes, err := json.Marshal(&vm)
					Expect(err).ToNot(HaveOccurred())

					ar := &admissionv1.AdmissionReview{
						Request: &admissionv1.AdmissionRequest{
							Resource: webhooks.VirtualMachineGroupVersionResource,
							Object: runtime.RawExtension{
								Raw: newVMBytes,
							},
							OldObject: runtime.RawExtension{
								Raw: oldVMBytes,
							},
							Operation: admissionv1.Update,
						},
					}

					response := vmsAdmitter.Admit(ar)
					Expect(response.Allowed).To(BeFalse())
					Expect(response.Result.Details.Causes[0].Field).To(Equal("spec.template.spec.domain.cpu.threads"))
					Expect(response.Result.Details.Causes[0].Message).To(ContainSubstring("Cannot update CPU threads while live update features configured"))
				})
			})
		})
	})
})

func admitVm(admitter *VMsAdmitter, vm *v1.VirtualMachine) *admissionv1.AdmissionResponse {
	vmBytes, _ := json.Marshal(vm)

	ar := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			Resource: webhooks.VirtualMachineGroupVersionResource,
			Object: runtime.RawExtension{
				Raw: vmBytes,
			},
		},
	}

	return admitter.Admit(ar)
}

func makeCloneAdmitFunc(k8sClient *k8sfake.Clientset, expectedSourceNamespace, expectedPVCName, expectedTargetNamespace, expectedServiceAccount string) CloneAuthFunc {
	k8sClient.Fake.PrependReactor("create", "subjectaccessreviews", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
		return true, &authorizationv1.SubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{
				Allowed: true,
			},
		}, nil
	})

	return func(dv *cdiv1.DataVolume, requestNamespace, requestName string, proxy cdiv1.AuthorizationHelperProxy, saNamespace, saName string) (bool, string, error) {
		response, err := dv.AuthorizeSA(requestNamespace, requestName, proxy, saNamespace, saName)
		Expect(err).ToNot(HaveOccurred())
		// Remove this when CDI patches the NS on the response
		// Expect(response.Handler.SourceNamespace).Should(Equal(expectedSourceNamespace))
		Expect(response.Handler.SourceName).Should(Equal(expectedPVCName))
		Expect(saNamespace).Should(Equal(expectedTargetNamespace))
		Expect(saName).Should(Equal(expectedServiceAccount))
		return true, "", nil
	}
}

func makeCloneAdmitFailFunc(message string, err error) CloneAuthFunc {
	return func(dv *cdiv1.DataVolume, requestNamespace, requestName string, proxy cdiv1.AuthorizationHelperProxy, saNamespace, saName string) (bool, string, error) {
		return false, message, err
	}
}
