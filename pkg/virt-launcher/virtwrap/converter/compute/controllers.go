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
	"slices"
	"strings"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/iothreads"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/vcpu"
)

const (
	defaultIOThread = uint(1)
)

type ControllersDomainConfigurator struct {
	isUSBNeeded               bool
	scsiModel                 string
	autoThreads               uint
	controllerDriver          *api.ControllerDriver
	supportPCIHole64Disabling bool
	virtioSerialModel         string
}

type controllersOption func(*ControllersDomainConfigurator)

func NewControllersDomainConfigurator(options ...controllersOption) ControllersDomainConfigurator {
	var configurator ControllersDomainConfigurator

	for _, f := range options {
		f(&configurator)
	}

	return configurator
}

func (c ControllersDomainConfigurator) Configure(vmi *v1.VirtualMachineInstance, domain *api.Domain) error {
	domain.Spec.Devices.Controllers = append(domain.Spec.Devices.Controllers, newUSBController(c.isUSBNeeded, vmi))

	if requiresSCSIController(vmi) {
		scsiControllerDriver := assignSCSIControllerIOThread(vmi, uint(c.autoThreads), c.controllerDriver.DeepCopy())
		domain.Spec.Devices.Controllers = append(domain.Spec.Devices.Controllers, newSCSIController(c.scsiModel, scsiControllerDriver))
	}

	if c.supportPCIHole64Disabling && shouldDisablePCIHole64(vmi) {
		domain.Spec.Devices.Controllers = append(domain.Spec.Devices.Controllers, newPCIControllerWithHole64Disabled())
	}

	if requiresVirtioSerialController(vmi) {
		domain.Spec.Devices.Controllers = append(domain.Spec.Devices.Controllers, newVirtioSerialController(c.virtioSerialModel, c.controllerDriver))
	}

	return nil
}

func ControllersWithUSBNeeded(isUSBNeeded bool) controllersOption {
	return func(c *ControllersDomainConfigurator) {
		c.isUSBNeeded = isUSBNeeded
	}
}

func ControllersWithSCSIModel(scsiModel string) controllersOption {
	return func(c *ControllersDomainConfigurator) {
		c.scsiModel = scsiModel
	}
}

func ControllersWithSCSIIOThreads(autoThreads uint) controllersOption {
	return func(c *ControllersDomainConfigurator) {
		c.autoThreads = autoThreads
	}
}

func ControllersWithControllerDriver(controllerDriver *api.ControllerDriver) controllersOption {
	return func(c *ControllersDomainConfigurator) {
		c.controllerDriver = controllerDriver
	}
}

func ControllersWithSupportPCIHole64Disabling(support bool) controllersOption {
	return func(c *ControllersDomainConfigurator) {
		c.supportPCIHole64Disabling = support
	}
}

func ControllersWithVirtioSerialModel(model string) controllersOption {
	return func(c *ControllersDomainConfigurator) {
		c.virtioSerialModel = model
	}
}

func newUSBController(usbNeeded bool, vmi *v1.VirtualMachineInstance) api.Controller {
	controller := api.Controller{
		Type:  "usb",
		Index: "0",
		Model: "none",
	}

	if usbNeeded {
		controller.Model = usbControllerModel(vmi)
		// UHCI provides two ports and rejects a port count; xHCI takes 15,
		// reserving ports for USB host devices hotplugged through DRA.
		if controller.Model != usbControllerUHCI {
			controller.Ports = pointer.P(uint(15))
		}
	}

	return controller
}

const (
	usbControllerXHCI = "qemu-xhci"
	usbControllerUHCI = "piix3-uhci"
)

// usbControllerModel picks a USB controller the guest can actually drive. xHCI has
// no in-box driver in guests older than Windows 8, while the i440fx machine carries
// the PIIX3 UHCI controller, which Windows NT 5.x and contemporary Linux support.
// Other machines keep xHCI: q35 has no PIIX3, and arm64 "virt" has no UHCI at all.
func usbControllerModel(vmi *v1.VirtualMachineInstance) string {
	machine := vmi.Spec.Domain.Machine
	if machine == nil {
		return usbControllerXHCI
	}
	if machine.Type == "pc" || strings.HasPrefix(machine.Type, "pc-i440fx") {
		return usbControllerUHCI
	}
	return usbControllerXHCI
}

func newSCSIController(controllerModel string, controllerDriver *api.ControllerDriver) api.Controller {
	return api.Controller{
		Type:   "scsi",
		Index:  "0",
		Model:  controllerModel,
		Driver: controllerDriver,
	}
}

func newPCIControllerWithHole64Disabled() api.Controller {
	return api.Controller{
		Type:  "pci",
		Index: "0",
		Model: "pcie-root",
		PCIHole64: &api.PCIHole64{
			Value: 0,
			Unit:  "KiB",
		},
	}
}

func newVirtioSerialController(model string, controllerDriver *api.ControllerDriver) api.Controller {
	return api.Controller{
		Type:   "virtio-serial",
		Index:  "0",
		Model:  model,
		Driver: controllerDriver,
	}
}

func shouldDisablePCIHole64(vmi *v1.VirtualMachineInstance) bool {
	if val, ok := vmi.Annotations[v1.DisablePCIHole64]; ok {
		return strings.EqualFold(val, "true")
	}
	return false
}

func requiresVirtioSerialController(vmi *v1.VirtualMachineInstance) bool {
	return vmi.Spec.Domain.Devices.AutoattachSerialConsole == nil || *vmi.Spec.Domain.Devices.AutoattachSerialConsole
}

func requiresSCSIController(vmi *v1.VirtualMachineInstance) bool {
	if !vmi.Spec.Domain.Devices.DisableHotplug {
		return true
	}

	for _, disk := range vmi.Spec.Domain.Devices.Disks {
		if getBusFromDisk(disk) == v1.DiskBusSCSI {
			return true
		}
	}

	return false
}

func getBusFromDisk(disk v1.Disk) v1.DiskBus {
	if disk.LUN != nil {
		return disk.LUN.Bus
	}
	if disk.Disk != nil {
		return disk.Disk.Bus
	}
	if disk.CDRom != nil {
		return disk.CDRom.Bus
	}
	return ""
}

func shouldConfigSCSIThread(vmi *v1.VirtualMachineInstance) bool {
	return slices.ContainsFunc(vmi.Spec.Domain.Devices.Disks, func(disk v1.Disk) bool {
		return getBusFromDisk(disk) == v1.DiskBusSCSI && iothreads.HasDedicatedIOThread(disk)
	})
}

func assignSCSIControllerIOThread(vmi *v1.VirtualMachineInstance, autoThreads uint, scsiControllerDriver *api.ControllerDriver) *api.ControllerDriver {
	if autoThreads == 0 || !shouldConfigSCSIThread(vmi) {
		return scsiControllerDriver
	}

	if scsiControllerDriver == nil {
		scsiControllerDriver = &api.ControllerDriver{}
	}

	vcpus := uint(vcpu.CalculateRequestedVCPUs(vcpu.GetCPUTopology(vmi)))
	if vcpus == 0 {
		vcpus = 1
	}

	scsiControllerDriver.IOThread = computeScsiControllerThread(autoThreads, vmi.Spec.Domain.Devices.Disks)
	scsiControllerDriver.Queues = pointer.P(vcpus)

	return scsiControllerDriver
}

func computeScsiControllerThread(autoThreads uint, disks []v1.Disk) *uint {
	currentAutoThread := defaultIOThread

	for _, disk := range disks {
		if getBusFromDisk(disk) == v1.DiskBusVirtio && !iothreads.HasDedicatedIOThread(disk) {
			currentAutoThread = (currentAutoThread % autoThreads) + 1
		}
	}

	return &currentAutoThread
}
