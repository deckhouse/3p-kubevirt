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

package compute

import (
	"fmt"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

type UsbRedirectDeviceDomainConfigurator struct{}

func (_ UsbRedirectDeviceDomainConfigurator) Configure(vmi *v1.VirtualMachineInstance, domain *api.Domain) error {
	clientDevices := vmi.Spec.Domain.Devices.ClientPassthrough

	// Default is to have USB Redirection disabled
	if clientDevices == nil {
		return nil
	}

	// Note that at the moment, we don't require any specific input to configure the USB devices
	// so we simply create the maximum allowed dictated by v1.UsbClientPassthroughMaxNumberOf
	redirectDevices := make([]api.RedirectedDevice, v1.UsbClientPassthroughMaxNumberOf)

	// With SPICE enabled the redirected devices are wired to the SPICE channels, so a
	// SPICE client redirects USB on its own. Otherwise they stay on unix sockets, which
	// is what `virtctl usbredir` connects to.
	for i := 0; i < v1.UsbClientPassthroughMaxNumberOf; i++ {
		if SpiceEnabled(vmi) {
			redirectDevices[i] = api.RedirectedDevice{
				Type: "spicevmc",
				Bus:  "usb",
			}
			continue
		}
		path := fmt.Sprintf("/var/run/kubevirt-private/%s/virt-usbredir-%d", vmi.ObjectMeta.UID, i)
		redirectDevices[i] = api.RedirectedDevice{
			Type: "unix",
			Bus:  "usb",
			Source: &api.RedirectedDeviceSource{
				Mode: "bind",
				Path: path,
			},
		}
	}
	domain.Spec.Devices.Redirs = redirectDevices
	return nil
}

// SpiceEnabled reports whether the SPICE display is requested on this VMI.
func SpiceEnabled(vmi *v1.VirtualMachineInstance) bool {
	// The annotation constant lives in the api package: the same value is read by the
	// memory overhead calculation in virt-controller.
	return vmi.Annotations[v1.SpiceAnnotation] == "true"
}
