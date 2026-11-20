package util

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("arch-capabilities CPU features", func() {
	DescribeTable("recognizes the features derived from the IA32_ARCH_CAPABILITIES MSR", func(name string, expected bool) {
		Expect(IsArchCapabilitiesCPUFeature(name)).To(Equal(expected))
	},
		Entry("arch-capabilities", "arch-capabilities", true),
		Entry("mds-no", "mds-no", true),
		Entry("gds-no", "gds-no", true),
		Entry("rfds-no", "rfds-no", true),
		Entry("skip-l1dfl-vmentry", "skip-l1dfl-vmentry", true),
		Entry("avx2", "avx2", false),
		Entry("spec-ctrl", "spec-ctrl", false),
		Entry("an empty name", "", false),
	)

	DescribeTable("tells the machine types that keep the features", func(machineType string, expected bool) {
		Expect(MachineTypeKeepsArchCapabilities(machineType)).To(Equal(expected))
	},
		Entry("pc-q35-9.2", "pc-q35-9.2", true),
		Entry("pc-q35-9.0", "pc-q35-9.0", true),
		Entry("pc-q35-8.2", "pc-q35-8.2", true),
		Entry("pc-q35-7.0", "pc-q35-7.0", true),
		Entry("pc-q35-10.0", "pc-q35-10.0", true),
		Entry("pc-i440fx-9.2", "pc-i440fx-9.2", true),
		Entry("pc-q35-10.1", "pc-q35-10.1", false),
		Entry("pc-q35-10.2", "pc-q35-10.2", false),
		Entry("pc-q35-11.0", "pc-q35-11.0", false),
		Entry("the q35 alias", "q35", false),
		Entry("the pc alias", "pc", false),
		Entry("a distribution machine type", "pc-q35-rhel9.2.0", false),
		Entry("a machine type with a suffix", "pc-q35-9.2.1", false),
		Entry("an empty machine type", "", false),
	)
})
