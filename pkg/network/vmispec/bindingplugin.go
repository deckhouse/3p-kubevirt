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

// BindingBPFBridge is the name of the bpfbridge network binding plugin.
const BindingBPFBridge = "bpfbridge"

// IsBPFBridgeBinding reports whether the interface is bound to the bpfbridge plugin.
func IsBPFBridgeBinding(iface v1.Interface) bool {
	return iface.Binding != nil && iface.Binding.Name == BindingBPFBridge
}

// HasBPFBridgeBinding reports whether any of the interfaces is bound to the bpfbridge plugin.
func HasBPFBridgeBinding(ifaces []v1.Interface) bool {
	for _, iface := range ifaces {
		if IsBPFBridgeBinding(iface) {
			return true
		}
	}
	return false
}

// DefaultBindingPlugins is the single source of truth for binding plugins that
// KubeVirt ships natively (i.e. without a user-provided sidecarImage). Entries
// here are merged under any user-provided networkConfiguration.binding entries
// so that both the domain attachment (libvirt XML) and the sidecar wiring paths
// agree on the same plugin metadata.
//
// Adding a plugin here makes it:
//   - resolvable by ClusterConfig.GetNetworkBindings() (domain XML converter,
//     migration checks, passt repair, ...), and
//   - resolvable by NetBindingPluginSidecarList() (sidecar container generation),
//
// without requiring the cluster admin to register it in the KubeVirt CR.
// A user-provided CR entry for the same name always takes precedence.
func DefaultBindingPlugins() map[string]v1.InterfaceBindingPlugin {
	return map[string]v1.InterfaceBindingPlugin{
		BindingBPFBridge: {
			// bpfbridge attaches the pod-facing and TAP interfaces with a TC BPF
			// program; from the domain (libvirt) perspective it is a plain tap
			// attachment.
			DomainAttachmentType: v1.Tap,
			// The guest gets its IP/gateway from the pod network via DHCP
			// (storeBridgeBindingDHCPInterfaceData). On live migration the target
			// pod has a different IP, so the guest must renew its lease: link-refresh
			// toggles the NIC down/up to trigger a fresh DHCP request.
			Migration: &v1.InterfaceBindingMigration{Method: v1.LinkRefresh},
		},
	}
}

// MergeBindingPlugins returns a new map starting from defaultBindingPlugins and
// overlaying userProvided on top. A nil userProvided is treated as empty. The
// returned map is always non-nil so callers can range over it safely.
func MergeBindingPlugins(userProvided map[string]v1.InterfaceBindingPlugin) map[string]v1.InterfaceBindingPlugin {
	merged := DefaultBindingPlugins()
	for name, binding := range userProvided {
		merged[name] = binding
	}
	return merged
}
