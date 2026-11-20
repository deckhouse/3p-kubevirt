package services

import (
	"runtime"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	v1 "kubevirt.io/api/core/v1"
)

var _ = Describe("Node Selector Renderer", func() {
	var nsr *NodeSelectorRenderer

	Context("scheduling pods", func() {
		When("no selectors are defined in the VMI spec", func() {
			BeforeEach(func() {
				nsr = NewNodeSelectorRenderer(emptySelectors(), emptySelectors(), "")
			})

			It("the node requires the KubeVirt schedulable label", func() {
				Expect(nsr.Render()).To(Equal(map[string]string{"kubevirt.io/schedulable": "true"}))
			})

			When("the dedicated CPU option is defined", func() {
				BeforeEach(func() {
					nsr = NewNodeSelectorRenderer(emptySelectors(), emptySelectors(), "", WithDedicatedCPU())
				})

				It("must be scheduled on nodes featuring the `cpumanager` label", func() {
					Expect(nsr.Render()).To(HaveLabel("cpumanager"))
				})
			})

			When("the TSC timer option is defined", func() {
				var aFewHertzios int64

				BeforeEach(func() {
					aFewHertzios = 123
					nsr = NewNodeSelectorRenderer(emptySelectors(), emptySelectors(), "", WithTSCTimer(&aFewHertzios))
				})

				It("requires nodes to feature a particular TSC frequency", func() {
					Expect(nsr.Render()).To(HaveLabel("scheduling.node.kubevirt.io/tsc-frequency-123"))
				})
			})

			When("Hyper V is defined", func() {
				BeforeEach(func() {
					nsr = NewNodeSelectorRenderer(emptySelectors(), emptySelectors(), "", WithHyperv(hypervFeatures()))
				})

				It("must be scheduled on nodes with Intel processor", func() {
					Expect(nsr.Render()).To(HaveLabel("cpu-vendor.node.kubevirt.io/Intel"))
				})
			})

			When("Hyper V is defined, but the features are not correct", func() {
				BeforeEach(func() {
					nsr = NewNodeSelectorRenderer(emptySelectors(), emptySelectors(), "", WithHyperv(kvmFeatures()))
				})

				It("does not require a particular processor vendor", func() {
					Expect(nsr.Render()).To(Equal(map[string]string{"kubevirt.io/schedulable": "true"}))
				})
			})

			When("specific CPU model and features are requested", func() {
				const (
					feature1 = "a-feature"
					feature2 = "yet-another-feature"
					model    = "model-t"
				)

				BeforeEach(func() {
					nsr = NewNodeSelectorRenderer(
						emptySelectors(),
						emptySelectors(),
						"",
						WithModelAndFeatureLabels(model, feature1, feature2))
				})

				It("requires the node to feature those particular features", func() {
					Expect(nsr.Render()).To(
						Equal(map[string]string{
							"kubevirt.io/schedulable": "true",
							"model-t":                 "true",
							"yet-another-feature":     "true",
							"a-feature":               "true",
						}))
				})
			})

			When("architecture set on VMI", func() {

				BeforeEach(func() {
					nsr = NewNodeSelectorRenderer(
						emptySelectors(),
						emptySelectors(),
						runtime.GOARCH)
				})

				It("requires the renderer to have applied the architecture to the node selectors", func() {
					Expect(nsr.Render()).To(
						Equal(map[string]string{
							"kubevirt.io/schedulable": "true",
							"kubernetes.io/arch":      runtime.GOARCH,
						}))
				})
			})

		})

		When("user defined selectors are present", func() {
			BeforeEach(func() {
				nsr = NewNodeSelectorRenderer(selectors(selector{key: "blue-node", value: "true"}), emptySelectors(), "")
			})

			It("the node requires the user defined selector", func() {
				Expect(nsr.Render()).To(
					Equal(map[string]string{
						"kubevirt.io/schedulable": "true",
						"blue-node":               "true",
					}))
			})
		})

		When("cluster-wide selectors are present", func() {
			BeforeEach(func() {
				nsr = NewNodeSelectorRenderer(
					emptySelectors(),
					selectors(selector{key: "all-nodes", value: "must-work"}), "")
			})

			It("the node requires the user defined selector", func() {
				Expect(nsr.Render()).To(
					Equal(map[string]string{
						"kubevirt.io/schedulable": "true",
						"all-nodes":               "must-work",
					}))
			})
		})
	})
})

func hypervFeatures() *v1.Features {
	return &v1.Features{Hyperv: &v1.FeatureHyperv{EVMCS: &v1.FeatureState{}}}
}

func kvmFeatures() *v1.Features {
	return &v1.Features{KVM: &v1.FeatureKVM{}}
}

type selector struct {
	key   string
	value string
}

func emptySelectors() map[string]string {
	return map[string]string{}
}

func selectors(userSelectors ...selector) map[string]string {
	definedSelectors := map[string]string{}
	for _, s := range userSelectors {
		definedSelectors[s.key] = s.value
	}
	return definedSelectors
}

func HaveLabel(labelKey string) types.GomegaMatcher {
	return HaveKeyWithValue(labelKey, "true")
}

var _ = Describe("CPU feature labels of a VMI", func() {
	archCapabilitiesFeatures := []string{"arch-capabilities", "mds-no", "rdctl-no", "gds-no", "rfds-no", "skip-l1dfl-vmentry", "pschange-mc-no"}

	newVMI := func(machineType *string, features ...v1.CPUFeature) *v1.VirtualMachineInstance {
		vmi := &v1.VirtualMachineInstance{}
		vmi.Spec.Domain.CPU = &v1.CPU{Model: "qemu64", Features: features}
		if machineType != nil {
			vmi.Status.Machine = &v1.Machine{Type: *machineType}
		}
		return vmi
	}

	features := func(names []string, policy string) []v1.CPUFeature {
		result := make([]v1.CPUFeature, 0, len(names))
		for _, name := range names {
			result = append(result, v1.CPUFeature{Name: name, Policy: policy})
		}
		return result
	}

	label := func(name string) string { return v1.CPUFeatureLabel + name }

	DescribeTable("leaves the arch-capabilities features out only for a machine type that keeps them",
		func(machineType *string, expectArchLabels bool) {
			vmiFeatures := append(features(archCapabilitiesFeatures, "require"), v1.CPUFeature{Name: "avx2", Policy: "require"}, v1.CPUFeature{Name: "sse4.2"})
			labels := CPUFeatureLabelsFromCPUFeatures(newVMI(machineType, vmiFeatures...))

			Expect(labels).To(ContainElements(label("avx2"), label("sse4.2")))
			for _, name := range archCapabilitiesFeatures {
				if expectArchLabels {
					Expect(labels).To(ContainElement(label(name)))
				} else {
					Expect(labels).ToNot(ContainElement(label(name)))
				}
			}
		},
		Entry("pc-q35-9.2", machineTypePtr("pc-q35-9.2"), false),
		Entry("pc-q35-10.0", machineTypePtr("pc-q35-10.0"), false),
		Entry("pc-i440fx-8.2", machineTypePtr("pc-i440fx-8.2"), false),
		Entry("pc-q35-10.1", machineTypePtr("pc-q35-10.1"), true),
		Entry("pc-q35-10.2", machineTypePtr("pc-q35-10.2"), true),
		Entry("the q35 alias", machineTypePtr("q35"), true),
		Entry("a VMI that has no machine type yet", nil, true),
	)

	It("keeps ignoring the features whose policy is not require", func() {
		machineType := "pc-q35-9.2"
		vmiFeatures := append(features([]string{"arch-capabilities"}, "optional"), features([]string{"invtsc", "avx2"}, "optional")...)
		vmiFeatures = append(vmiFeatures, v1.CPUFeature{Name: "vmx", Policy: "disable"}, v1.CPUFeature{Name: "svm", Policy: "forbid"})

		Expect(CPUFeatureLabelsFromCPUFeatures(newVMI(&machineType, vmiFeatures...))).To(BeEmpty())
	})

	It("returns no labels for a VMI without CPU", func() {
		Expect(CPUFeatureLabelsFromCPUFeatures(&v1.VirtualMachineInstance{})).To(BeEmpty())
	})
})

func machineTypePtr(s string) *string { return &s }
