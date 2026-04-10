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
	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/vcpu"
)

type CPUDomainConfigurator struct {
	isHotplugSupported       bool
	requiresMPXCPUValidation bool
	crossArchEmulation       bool
}

func NewCPUDomainConfigurator(isHotplugSupported, requiresMPXCPUValidation, crossArchEmulation bool) CPUDomainConfigurator {
	return CPUDomainConfigurator{
		isHotplugSupported:       isHotplugSupported,
		requiresMPXCPUValidation: requiresMPXCPUValidation,
		crossArchEmulation:       crossArchEmulation,
	}
}

func (c CPUDomainConfigurator) Configure(vmi *v1.VirtualMachineInstance, domain *api.Domain) error {
	// Set VM CPU cores
	// CPU topology will be created everytime, because user can specify
	// number of cores in vmi.Spec.Domain.Resources.Requests/Limits, not only
	// in vmi.Spec.Domain.CPU
	cpuTopology := vcpu.GetCPUTopology(vmi)
	cpuCount := vcpu.CalculateRequestedVCPUs(cpuTopology)

	domain.Spec.CPU.Topology = cpuTopology
	domain.Spec.VCPU = &api.VCPU{
		Placement: "static",
		CPUs:      cpuCount,
	}
	// set the maximum number of sockets here to allow hot-plug CPUs
	if vmiCPU := vmi.Spec.Domain.CPU; vmiCPU != nil && vmiCPU.MaxSockets != 0 && c.isHotplugSupported {
		if _, ok := vmi.Annotations[v1.VCPUTopologyDynamicCoresAnnotation]; ok {
			// "Dynamic cores" considers that user can change cores number instead of sockets.
			domainVCPUTopologyForHotplugDynamicCores(vmi, domain)
		} else {
			domainVCPUTopologyForHotplug(vmi, domain)
		}
	}

	if vmi.Spec.Domain.CPU != nil {
		// Set VM CPU model and vendor
		if vmi.Spec.Domain.CPU.Model != "" {
			isHostCPUMode := vmi.Spec.Domain.CPU.Model == v1.CPUModeHostModel || vmi.Spec.Domain.CPU.Model == v1.CPUModeHostPassthrough
			if c.crossArchEmulation && isHostCPUMode {
				// host-passthrough and host-model are incompatible with cross-architecture
				// TCG emulation. Use "max" which exposes all features QEMU can emulate.
				domain.Spec.CPU.Mode = "custom"
				domain.Spec.CPU.Model = "max"
			} else if isHostCPUMode {
				domain.Spec.CPU.Mode = vmi.Spec.Domain.CPU.Model
			} else {
				domain.Spec.CPU.Mode = "custom"
				domain.Spec.CPU.Model = vmi.Spec.Domain.CPU.Model
			}
		}

		// Set VM CPU features
		existingFeatures := make(map[string]struct{})
		if vmi.Spec.Domain.CPU.Features != nil {
			for _, feature := range vmi.Spec.Domain.CPU.Features {
				existingFeatures[feature.Name] = struct{}{}
				domain.Spec.CPU.Features = append(domain.Spec.CPU.Features, api.CPUFeature{
					Name:   feature.Name,
					Policy: feature.Policy,
				})
			}
		}

		/*
						Libvirt validation fails when a CPU model is usable
						by QEMU but lacks features listed in
						`/usr/share/libvirt/cpu_map/[CPU Model].xml` on a node
						To avoid the validation error mentioned above we can disable
						deprecated features in the `/usr/share/libvirt/cpu_map/[CPU Model].xml` files.
						Examples of validation error:
			    		https://bugzilla.redhat.com/show_bug.cgi?id=2122283 - resolve by obsolete Opteron_G2
						https://gitlab.com/libvirt/libvirt/-/issues/304 - resolve by disabling mpx which is deprecated
						Issue in Libvirt: https://gitlab.com/libvirt/libvirt/-/issues/608
						once the issue is resolved we can remove mpx disablement
		*/

		_, exists := existingFeatures["mpx"]
		if c.requiresMPXCPUValidation && !exists && vmi.Spec.Domain.CPU.Model != v1.CPUModeHostModel && vmi.Spec.Domain.CPU.Model != v1.CPUModeHostPassthrough {
			domain.Spec.CPU.Features = append(domain.Spec.CPU.Features, api.CPUFeature{
				Name:   "mpx",
				Policy: "disable",
			})
		}
	}

	if vmi.Spec.Domain.CPU == nil || vmi.Spec.Domain.CPU.Model == "" {
		if c.crossArchEmulation {
			domain.Spec.CPU.Mode = "custom"
			domain.Spec.CPU.Model = "max"
		} else {
			domain.Spec.CPU.Mode = v1.CPUModeHostModel
		}
	}

	return nil
}

func domainVCPUTopologyForHotplug(vmi *v1.VirtualMachineInstance, domain *api.Domain) {
	cpuTopology := vcpu.GetCPUTopology(vmi)
	cpuCount := vcpu.CalculateRequestedVCPUs(cpuTopology)
	// Always allow to hotplug to minimum of 1 socket
	minEnabledCpuCount := cpuTopology.Cores * cpuTopology.Threads
	// Total vCPU count
	enabledCpuCount := cpuCount
	cpuTopology.Sockets = vmi.Spec.Domain.CPU.MaxSockets
	cpuCount = vcpu.CalculateRequestedVCPUs(cpuTopology)
	VCPUs := &api.VCPUs{}
	for id := uint32(0); id < cpuCount; id++ {
		// Enable all requestd vCPUs
		isEnabled := id < enabledCpuCount
		// There should not be fewer vCPU than cores and threads within a single socket
		isHotpluggable := id >= minEnabledCpuCount
		vcpu := api.VCPUsVCPU{
			ID:           id,
			Enabled:      boolToYesNo(&isEnabled, true),
			Hotpluggable: boolToYesNo(&isHotpluggable, false),
		}
		VCPUs.VCPU = append(VCPUs.VCPU, vcpu)
	}

	domain.Spec.VCPUs = VCPUs
	domain.Spec.CPU.Topology = cpuTopology
	domain.Spec.VCPU = &api.VCPU{
		Placement: "static",
		CPUs:      cpuCount,
	}
}

// domainVCPUTopologyForHotplugDynamicCores implements "dynamic cores" strategy for CPU hotplug.
// This strategy considers that user can change cores number instead of sockets.
// To accomplish this without huge refactoring we change vmi domain fields meaning:
// 1) cores(-per-socket) field is used as a sockets number.
// 2) sockets field is used as a dynamic cores-per-socket number.
// 3) maxSockets field meaning is "max cores-per-socket".
//
// Also, sparse VCPUs array is used to distribute cores by sockets.
// Example:
// cores: 2, sockets: 9, maxSockets: 16
// It becomes: 2 sockets, 9 cores per socket, max 16 cores per socket.
// VCPUs array:
// 0-15 - cores for socket 1
// 16-31 - cores for socket 2.
// For each socket first 9 cores are enabled, other 7 cores are disabled.
// First 2 cores are non-hotpluggabled, others are hotpluggable.
//
// Note: It is not possible to have non-hotpluggable cores for each socket,
// as QEMU expects non-hotpluggable VCPUs first and then hotpluggable VCPUs, so
// we end up with simpler approach: make at least "sockets" count of VCPUs non-hotpluggable
// for the first socket.
func domainVCPUTopologyForHotplugDynamicCores(vmi *v1.VirtualMachineInstance, domain *api.Domain) {
	// Convert "dynamic sockets" topology to "dynamic cores": swap cores and sockets.
	cpuTopology := vcpu.GetCPUTopology(vmi)
	coresPerSocket := cpuTopology.Sockets
	sockets := cpuTopology.Cores
	cpuTopology.Cores = coresPerSocket
	cpuTopology.Sockets = sockets
	enabledCoresTotal := vcpu.CalculateRequestedVCPUs(cpuTopology)

	// Set cores to maximum limit to enable hotplug. (Max cores per socket value is passed as MaxSockets to bypass validation).
	cpuTopology.Cores = vmi.Spec.Domain.CPU.MaxSockets
	maxCoresTotal := vcpu.CalculateRequestedVCPUs(cpuTopology)

	// At least 1 core per socket should be non-hotpluggable.
	nonHotpluggableCores := sockets
	// Count vCPUs per socket.
	enabledCoresPerSocket := enabledCoresTotal / sockets
	maxCoresPerSocket := maxCoresTotal / sockets

	VCPUs := &api.VCPUs{}
	for sockId := uint32(0); sockId < sockets; sockId++ {
		for coreId := uint32(0); coreId < maxCoresPerSocket; coreId++ {
			vcpuId := maxCoresPerSocket*sockId + coreId
			// First nonHotpluggableCores vcpus in socket 0 will be non-hotpluggable.
			isHotpluggable := vcpuId >= nonHotpluggableCores
			// Enable requested number of vCPUs per socket.
			isEnabled := coreId < enabledCoresPerSocket

			vcpu := api.VCPUsVCPU{
				ID:           vcpuId,
				Enabled:      boolToYesNo(&isEnabled, true),
				Hotpluggable: boolToYesNo(&isHotpluggable, false),
				Order:        coreId*sockets + sockId + 1, // Order should be greater than 0.
			}
			VCPUs.VCPU = append(VCPUs.VCPU, vcpu)
		}
	}

	domain.Spec.VCPUs = VCPUs
	domain.Spec.CPU.Topology = cpuTopology
	domain.Spec.VCPU = &api.VCPU{
		Placement: "static",
		CPUs:      maxCoresTotal,
	}
}
