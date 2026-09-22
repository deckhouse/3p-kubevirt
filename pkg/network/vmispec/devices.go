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

package vmispec

import v1 "kubevirt.io/api/core/v1"

// RequiresVirtioNetDevice checks whether a VMI requires the presence of the "virtio" net device.
// This happens when the VMI wants to use a "virtio" network interface, and software emulation is disallowed.
func RequiresVirtioNetDevice(vmi *v1.VirtualMachineInstance, allowEmulation bool) bool {
	return (mayBePluggedAnInterface(vmi) || hasVirtioIface(vmi)) && !allowEmulation
}

// RequiresTunDevice always holds. Upstream withheld /dev/net/tun from a VMI with no interface
// and the pod interface autoattach rejected, which is exactly how DVP spells a machine
// whose networks are not Ready yet and whose interfaces are plugged once they are. The
// device cannot be added to a running pod, so every VMI claims it upfront.
func RequiresTunDevice(_ *v1.VirtualMachineInstance) bool {
	return true
}

// mayBePluggedAnInterface reports whether a VMI can still be plugged an interface while it
// runs, see RequiresTunDevice.
func mayBePluggedAnInterface(vmi *v1.VirtualMachineInstance) bool {
	return len(vmi.Spec.Domain.Devices.Interfaces) == 0
}
