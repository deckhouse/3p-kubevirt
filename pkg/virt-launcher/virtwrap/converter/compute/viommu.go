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
)

// VIOMMUDomainConfigurator adds an emulated Intel IOMMU when the VMI carries
// the v1.EnableVIOMMUAnnotation annotation.
type VIOMMUDomainConfigurator struct{}

func (VIOMMUDomainConfigurator) Configure(vmi *v1.VirtualMachineInstance, domain *api.Domain) error {
	if vmi.Annotations[v1.EnableVIOMMUAnnotation] != "true" {
		return nil
	}

	if domain.Spec.Features == nil {
		domain.Spec.Features = &api.Features{}
	}
	// intremap requires the split irqchip provided by the qemu ioapic;
	// caching_mode is required for VFIO to track guest DMA mappings.
	domain.Spec.Features.IOAPIC = &api.FeatureIOAPIC{Driver: "qemu"}
	domain.Spec.Devices.IOMMU = append(domain.Spec.Devices.IOMMU, api.IOMMUDevice{
		Model: "intel",
		Driver: &api.IOMMUDriver{
			IntRemap:    "on",
			CachingMode: "on",
		},
	})
	return nil
}
