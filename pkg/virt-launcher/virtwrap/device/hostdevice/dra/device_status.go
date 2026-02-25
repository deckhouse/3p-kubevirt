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
	"fmt"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	drautil "kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

// Device attributes come from two sources. The DRA status controller in virt-controller puts them into
// vmi.status.deviceStatus; this is how the fork's DRA drivers deliver them, and the only way that works
// for claims hotplugged through an attachment pod. Drivers that implement KEP-5304 deliver them as
// metadata files mounted into the virt-launcher pod. The status takes precedence, metadata is the fallback.

func gpuStatusAttributes(vmi *v1.VirtualMachineInstance, name string) *v1.DeviceAttribute {
	if vmi.Status.DeviceStatus == nil {
		return nil
	}
	return findStatusAttributes(vmi.Status.DeviceStatus.GPUStatuses, name)
}

func hostDeviceStatusAttributes(vmi *v1.VirtualMachineInstance, name string) *v1.DeviceAttribute {
	if vmi.Status.DeviceStatus == nil {
		return nil
	}
	return findStatusAttributes(vmi.Status.DeviceStatus.HostDeviceStatuses, name)
}

func findStatusAttributes(statuses []v1.DeviceStatusInfo, name string) *v1.DeviceAttribute {
	for _, status := range statuses {
		if status.Name == name && status.DeviceResourceClaimStatus != nil {
			return status.DeviceResourceClaimStatus.Attributes
		}
	}
	return nil
}

func getMDevUUID(
	statusAttrs *v1.DeviceAttribute,
	basePath string,
	resourceClaims []v1.VirtualMachineInstanceResourceClaim,
	claimName, requestName string,
) (string, error) {
	if statusAttrs == nil {
		return drautil.GetMDevUUIDForClaim(basePath, resourceClaims, claimName, requestName)
	}
	if statusAttrs.MDevUUID == nil {
		return "", fmt.Errorf("device status for claim %q request %q has no mdevUUID", claimName, requestName)
	}
	return *statusAttrs.MDevUUID, nil
}

func getPCIAddress(
	statusAttrs *v1.DeviceAttribute,
	basePath string,
	resourceClaims []v1.VirtualMachineInstanceResourceClaim,
	claimName, requestName string,
) (string, error) {
	if statusAttrs == nil {
		return drautil.GetPCIAddressForClaim(basePath, resourceClaims, claimName, requestName)
	}
	if statusAttrs.PCIAddress == nil {
		return "", fmt.Errorf("device status for claim %q request %q has no pciAddress", claimName, requestName)
	}
	return *statusAttrs.PCIAddress, nil
}

// usbHostDeviceFromStatus returns a USB host device when the device status carries a USB address.
// USB devices are known only from the device status: KEP-5304 metadata has no USB attribute.
func usbHostDeviceFromStatus(name string, statusAttrs *v1.DeviceAttribute) *api.HostDevice {
	if statusAttrs == nil || statusAttrs.USBAddress == nil {
		return nil
	}
	log.Log.V(2).Infof("Adding DRA USB HostDevice for %s", name)
	hostDevice := newUSBHostDevice(statusAttrs.USBAddress, name, false)
	return &hostDevice
}
