package util

import (
	"regexp"
	"strconv"
)

// archCapabilitiesCPUFeatures are the CPU features derived from the IA32_ARCH_CAPABILITIES MSR.
// QEMU 10.1 and newer leave them out of the host CPU model on AMD, so the node labeller does not publish
// them as node labels, while QEMU still provides them when a guest asks for them explicitly.
// The list mirrors ArchCapabilitiesCPUFeatures of the virtualization module API.
var archCapabilitiesCPUFeatures = map[string]struct{}{
	"arch-capabilities":  {},
	"rdctl-no":           {},
	"ibrs-all":           {},
	"rsba":               {},
	"skip-l1dfl-vmentry": {},
	"ssb-no":             {},
	"mds-no":             {},
	"pschange-mc-no":     {},
	"tsx-ctrl":           {},
	"taa-no":             {},
	"fb-clear":           {},
	"gds-no":             {},
	"rfds-no":            {},
	"rfds-clear":         {},
}

// versionedPCMachineType matches the versioned machine types of the x86 PC boards, such as pc-q35-9.2.
var versionedPCMachineType = regexp.MustCompile(`^pc-(?:q35|i440fx)-(\d+)\.(\d+)$`)

// IsArchCapabilitiesCPUFeature reports whether the CPU feature is derived from the IA32_ARCH_CAPABILITIES MSR.
func IsArchCapabilitiesCPUFeature(name string) bool {
	_, ok := archCapabilitiesCPUFeatures[name]
	return ok
}

// MachineTypeKeepsArchCapabilities reports whether QEMU gives the arch-capabilities CPU features
// to a guest of the machine type on AMD hosts.
// Versioned machine types up to pc-*-10.0 carry the x-arch-cap-always-on compatibility property, so a
// migration target started with such a machine type provides the features even though the node does not
// advertise them.
// 10.0 is the last version of the machine types that set x-arch-cap-always-on in pc_compat_10_0 of QEMU.
func MachineTypeKeepsArchCapabilities(machineType string) bool {
	match := versionedPCMachineType.FindStringSubmatch(machineType)
	if match == nil {
		return false
	}

	major, err := strconv.Atoi(match[1])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return false
	}

	return major < 10 || (major == 10 && minor == 0)
}
