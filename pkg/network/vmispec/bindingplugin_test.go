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

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "kubevirt.io/api/core/v1"
)

var _ = Describe("Default binding plugins", func() {
	It("bpfbridge is registered as a migratable native tap attachment", func() {
		defs := DefaultBindingPlugins()
		Expect(defs).To(HaveKey("bpfbridge"))
		Expect(defs["bpfbridge"].DomainAttachmentType).To(Equal(v1.Tap))
		Expect(defs["bpfbridge"].Migration).ToNot(BeNil())
		Expect(defs["bpfbridge"].Migration.Method).To(Equal(v1.LinkRefresh))
	})

	DescribeTable("MergeBindingPlugins",
		func(userProvided map[string]v1.InterfaceBindingPlugin, expectedBpfbridge v1.InterfaceBindingPlugin, hasUser bool) {
			merged := MergeBindingPlugins(userProvided)

			Expect(merged).To(HaveKey("bpfbridge"))
			if hasUser {
				Expect(merged["bpfbridge"]).To(Equal(expectedBpfbridge))
			} else {
				Expect(merged["bpfbridge"]).To(Equal(v1.InterfaceBindingPlugin{
					DomainAttachmentType: v1.Tap,
					Migration:            &v1.InterfaceBindingMigration{Method: v1.LinkRefresh},
				}))
			}
		},
		Entry("nil userProvided keeps the default bpfbridge", nil, v1.InterfaceBindingPlugin{}, false),
		Entry("empty userProvided keeps the default bpfbridge", map[string]v1.InterfaceBindingPlugin{}, v1.InterfaceBindingPlugin{}, false),
		Entry("userProvided bpfbridge overrides the default",
			map[string]v1.InterfaceBindingPlugin{
				"bpfbridge": {DomainAttachmentType: v1.ManagedTap, SidecarImage: "custom:latest"},
			},
			v1.InterfaceBindingPlugin{DomainAttachmentType: v1.ManagedTap, SidecarImage: "custom:latest"},
			true,
		),
	)

	It("merges extra user-provided plugins alongside the defaults", func() {
		merged := MergeBindingPlugins(map[string]v1.InterfaceBindingPlugin{
			"custom": {DomainAttachmentType: v1.Tap, SidecarImage: "custom:latest"},
		})
		Expect(merged).To(HaveKey("bpfbridge"))
		Expect(merged).To(HaveKey("custom"))
	})

	It("returns a non-nil map for nil input so callers can range safely", func() {
		Expect(MergeBindingPlugins(nil)).ToNot(BeNil())
	})
})
