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

package dra

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/dra/metadata"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

var _ = Describe("DRA device attributes from vmi.status.deviceStatus", func() {
	const (
		pciFromStatus   = "0000:03:00.0"
		pciFromMetadata = "0000:02:00.0"
		mdevFromStatus  = "123e4567-e89b-12d3-a456-426614174000"
	)

	var basePath string

	BeforeEach(func() {
		var err error
		basePath, err = os.MkdirTemp("", "dra-device-status-test")
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(os.RemoveAll, basePath)
	})

	writeMetadataWithPCI := func(pciAddr string) {
		dir := filepath.Join(basePath, "resourceclaims", "claim1", "req1")
		Expect(os.MkdirAll(dir, 0755)).To(Succeed())
		data, err := json.Marshal(&metadata.DeviceMetadata{
			TypeMeta:   metav1.TypeMeta{Kind: "DeviceMetadata", APIVersion: metadata.APIVersionV1Alpha1},
			ObjectMeta: metav1.ObjectMeta{Name: "claim1"},
			Requests: []metadata.DeviceMetadataRequest{{
				Name: "req1",
				Devices: []metadata.Device{{
					Driver: "gpu.example.com",
					Pool:   "pool",
					Name:   "device1",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						metadata.PCIBusIDAttribute: {StringValue: ptr.To(pciAddr)},
					},
				}},
			}},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(dir, "gpu.example.com-metadata.json"), data, 0644)).To(Succeed())
	}

	newVMI := func() *v1.VirtualMachineInstance {
		claimRequest := &v1.ClaimRequest{ClaimName: "claim1", RequestName: "req1"}
		return &v1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "testvmi", Namespace: "default"},
			Spec: v1.VirtualMachineInstanceSpec{
				ResourceClaims: []v1.VirtualMachineInstanceResourceClaim{{
					Name:              "claim1",
					ResourceClaimName: ptr.To("claim1"),
				}},
				Domain: v1.DomainSpec{
					Devices: v1.Devices{
						GPUs:        []v1.GPU{{Name: "gpu1", ClaimRequest: claimRequest}},
						HostDevices: []v1.HostDevice{{Name: "hostdev1", ClaimRequest: claimRequest}},
					},
				},
			},
		}
	}

	statusInfo := func(name string, attrs v1.DeviceAttribute) v1.DeviceStatusInfo {
		return v1.DeviceStatusInfo{
			Name: name,
			DeviceResourceClaimStatus: &v1.DeviceResourceClaimStatus{
				Name:              ptr.To("device1"),
				ResourceClaimName: ptr.To("claim1"),
				Attributes:        &attrs,
			},
		}
	}

	pciAddressOf := func(dev api.HostDevice) string {
		Expect(dev.Source.Address).ToNot(BeNil())
		return dev.Source.Address.Domain + ":" + dev.Source.Address.Bus + ":" + dev.Source.Address.Slot + "." + dev.Source.Address.Function
	}

	It("should build GPUs and host devices from the device status without metadata files", func() {
		vmi := newVMI()
		vmi.Status.DeviceStatus = &v1.DeviceStatus{
			GPUStatuses:        []v1.DeviceStatusInfo{statusInfo("gpu1", v1.DeviceAttribute{MDevUUID: ptr.To(mdevFromStatus)})},
			HostDeviceStatuses: []v1.DeviceStatusInfo{statusInfo("hostdev1", v1.DeviceAttribute{PCIAddress: ptr.To(pciFromStatus)})},
		}

		gpus, err := CreateDRAGPUHostDevices(vmi, basePath)
		Expect(err).ToNot(HaveOccurred())
		Expect(gpus).To(HaveLen(1))
		Expect(gpus[0].Type).To(Equal(api.HostDeviceMDev))
		Expect(gpus[0].Source.Address.UUID).To(Equal(mdevFromStatus))

		hostDevices, err := CreateDRAHostDevices(vmi, basePath)
		Expect(err).ToNot(HaveOccurred())
		Expect(hostDevices).To(HaveLen(1))
		Expect(hostDevices[0].Type).To(Equal(api.HostDevicePCI))
		Expect(pciAddressOf(hostDevices[0])).To(Equal("0x0000:0x03:0x00.0x0"))
	})

	It("should prefer the device status over a metadata file", func() {
		writeMetadataWithPCI(pciFromMetadata)
		vmi := newVMI()
		vmi.Spec.Domain.Devices.HostDevices = nil
		vmi.Status.DeviceStatus = &v1.DeviceStatus{
			GPUStatuses: []v1.DeviceStatusInfo{statusInfo("gpu1", v1.DeviceAttribute{PCIAddress: ptr.To(pciFromStatus)})},
		}

		gpus, err := CreateDRAGPUHostDevices(vmi, basePath)
		Expect(err).ToNot(HaveOccurred())
		Expect(gpus).To(HaveLen(1))
		Expect(pciAddressOf(gpus[0])).To(Equal("0x0000:0x03:0x00.0x0"))
	})

	It("should fall back to the metadata file when the device has no status", func() {
		writeMetadataWithPCI(pciFromMetadata)
		vmi := newVMI()
		vmi.Spec.Domain.Devices.GPUs = nil
		vmi.Status.DeviceStatus = &v1.DeviceStatus{
			GPUStatuses: []v1.DeviceStatusInfo{statusInfo("other-gpu", v1.DeviceAttribute{PCIAddress: ptr.To(pciFromStatus)})},
		}

		hostDevices, err := CreateDRAHostDevices(vmi, basePath)
		Expect(err).ToNot(HaveOccurred())
		Expect(hostDevices).To(HaveLen(1))
		Expect(pciAddressOf(hostDevices[0])).To(Equal("0x0000:0x02:0x00.0x0"))
	})
})
