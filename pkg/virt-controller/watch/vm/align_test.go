/*
Copyright The KubeVirt Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vm

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "kubevirt.io/api/core/v1"
)

var _ = Describe("alignLegacyIfaceFields", func() {
	bpfBridgeIface := func(acpiIndex int) v1.Interface {
		return v1.Interface{
			Name:      "default",
			Model:     "virtio",
			ACPIIndex: acpiIndex,
			Binding:   &v1.PluginBinding{Name: bpfBridgeBindingName},
		}
	}

	bridgeIface := func(acpiIndex int) v1.Interface {
		iface := v1.Interface{
			Name:      "default",
			Model:     "virtio",
			ACPIIndex: acpiIndex,
		}
		iface.Bridge = &v1.InterfaceBridge{}
		return iface
	}

	DescribeTable("aligns the interface with the desired one", func(iface, desiredIface v1.Interface) {
		ifaces := []v1.Interface{iface}

		alignLegacyIfaceFields(ifaces, []v1.Interface{desiredIface})

		Expect(ifaces).To(Equal([]v1.Interface{desiredIface}))
	},
		Entry("when the bridge binding method is replaced by the bpfbridge binding plugin",
			bridgeIface(1), bpfBridgeIface(1)),
		Entry("when an ACPI index is assigned to an interface that had none",
			bpfBridgeIface(0), bpfBridgeIface(1)),
		Entry("when both the binding and the ACPI index changed",
			bridgeIface(0), bpfBridgeIface(1)),
	)

	DescribeTable("keeps the interface as is", func(iface, desiredIface v1.Interface) {
		ifaces := []v1.Interface{iface}

		alignLegacyIfaceFields(ifaces, []v1.Interface{desiredIface})

		Expect(ifaces).To(Equal([]v1.Interface{iface}))
	},
		Entry("when the ACPI index is changed to another one",
			bpfBridgeIface(1), bpfBridgeIface(2)),
		Entry("when the ACPI index is removed",
			bpfBridgeIface(1), bpfBridgeIface(0)),
		Entry("when the bridge binding method is replaced by another binding plugin",
			bridgeIface(1), v1.Interface{
				Name:      "default",
				Model:     "virtio",
				ACPIIndex: 1,
				Binding:   &v1.PluginBinding{Name: "passt"},
			}),
		Entry("when no desired interface has the same name",
			bridgeIface(0), v1.Interface{Name: "other", Binding: &v1.PluginBinding{Name: bpfBridgeBindingName}, ACPIIndex: 1}),
	)
})

var _ = Describe("alignSVMCPUFeature", func() {
	svmOptional := v1.CPUFeature{Name: "svm", Policy: "optional"}
	svmRequire := v1.CPUFeature{Name: "svm", Policy: "require"}
	invtsc := v1.CPUFeature{Name: "invtsc", Policy: "optional"}

	DescribeTable("takes the desired features", func(features, desiredFeatures []v1.CPUFeature) {
		Expect(alignSVMCPUFeature(features, desiredFeatures)).To(Equal(desiredFeatures))
	},
		Entry("when svm was added to the desired features",
			[]v1.CPUFeature{invtsc}, []v1.CPUFeature{invtsc, svmOptional}),
		Entry("when svm is gone from the desired features",
			[]v1.CPUFeature{invtsc, svmOptional}, []v1.CPUFeature{invtsc}),
		Entry("when svm changed its policy",
			[]v1.CPUFeature{invtsc, svmOptional}, []v1.CPUFeature{invtsc, svmRequire}),
		Entry("when svm is the only desired feature",
			nil, []v1.CPUFeature{svmOptional}),
		Entry("when the features are equal",
			[]v1.CPUFeature{invtsc, svmOptional}, []v1.CPUFeature{invtsc, svmOptional}),
	)

	DescribeTable("keeps the features as they are", func(features, desiredFeatures []v1.CPUFeature) {
		Expect(alignSVMCPUFeature(features, desiredFeatures)).To(Equal(features))
	},
		Entry("when another feature was added next to svm",
			[]v1.CPUFeature{invtsc}, []v1.CPUFeature{invtsc, svmOptional, {Name: "vmx", Policy: "require"}}),
		Entry("when another feature changed its policy",
			[]v1.CPUFeature{invtsc, svmOptional}, []v1.CPUFeature{{Name: "invtsc", Policy: "require"}}),
		Entry("when the feature order changed",
			[]v1.CPUFeature{invtsc, {Name: "vmx"}}, []v1.CPUFeature{{Name: "vmx"}, invtsc}),
	)
})
