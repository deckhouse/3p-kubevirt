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

package netbinding

import (
	"fmt"

	k8sv1 "k8s.io/api/core/v1"
	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/hooks"
)

// defaultBindingPlugins provides hardcoded defaults for testing/development
// TODO: remove this when bpfbridge is properly registered via KubeVirt CR
var defaultBindingPlugins = map[string]v1.InterfaceBindingPlugin{
	"bpfbridge": {
		SidecarImage:         "dev-registry.deckhouse.io/sys/deckhouse-oss/modules/virtualization:latest",
		DomainAttachmentType: "tap",
	},
}

func NetBindingPluginSidecarList(vmi *v1.VirtualMachineInstance, config *v1.KubeVirtConfiguration) (hooks.HookSidecarList, error) {
	var pluginSidecars hooks.HookSidecarList

	netbindingPluginSidecars, err := netBindingPluginSidecar(vmi, config)
	if err != nil {
		return nil, err
	}
	pluginSidecars = append(pluginSidecars, netbindingPluginSidecars...)

	return pluginSidecars, nil
}

func netBindingPluginSidecar(vmi *v1.VirtualMachineInstance, config *v1.KubeVirtConfiguration) (hooks.HookSidecarList, error) {
	bindingByName := map[string]v1.InterfaceBindingPlugin{}

	// Start with defaults
	for name, binding := range defaultBindingPlugins {
		bindingByName[name] = binding
	}

	// Override with config bindings if provided
	if config.NetworkConfiguration != nil && config.NetworkConfiguration.Binding != nil {
		for name, binding := range config.NetworkConfiguration.Binding {
			bindingByName[name] = binding
		}
	}

	// Find bindings that are actually used by VMI interfaces
	var usedBindings = make(map[string]v1.InterfaceBindingPlugin)
	for _, iface := range vmi.Spec.Domain.Devices.Interfaces {
		if iface.Binding != nil && iface.Binding.Name != "" {
			if binding, exists := bindingByName[iface.Binding.Name]; exists {
				usedBindings[iface.Binding.Name] = binding
			} else {
				return nil, fmt.Errorf("couldn't find configuration for network binding: %s", iface.Binding.Name)
			}
		}
	}

	// Create sidecars for used bindings
	var pluginSidecars hooks.HookSidecarList
	for name, pluginInfo := range usedBindings {
		if pluginInfo.SidecarImage != "" {
			sidecar := hooks.HookSidecar{
				Image:           pluginInfo.SidecarImage,
				ImagePullPolicy: config.ImagePullPolicy,
				DownwardAPI:     pluginInfo.DownwardAPI,
			}
			if name == "bpfbridge" {
				sidecar.Command = []string{"/usr/bin/network-bpf-bridge-binding"}
				sidecar.Capabilities = &k8sv1.Capabilities{
					Add: []k8sv1.Capability{"NET_ADMIN", "NET_RAW", "SYS_ADMIN", "BPF"},
				}
				sidecar.Privileged = true
				sidecar.VolumeMounts = append(sidecar.VolumeMounts, k8sv1.VolumeMount{
					Name:      "bpffs",
					MountPath: "/sys/fs/bpf",
				})
				sidecar.Volumes = append(sidecar.Volumes, k8sv1.Volume{
					Name: "bpffs",
					VolumeSource: k8sv1.VolumeSource{
						HostPath: &k8sv1.HostPathVolumeSource{
							Path: "/sys/fs/bpf",
						},
					},
				})
			}
			pluginSidecars = append(pluginSidecars, sidecar)
		}
	}

	return pluginSidecars, nil
}

func ReadNetBindingPluginConfiguration(kvConfig *v1.KubeVirtConfiguration, pluginName string) *v1.InterfaceBindingPlugin {
	if kvConfig != nil && kvConfig.NetworkConfiguration != nil && kvConfig.NetworkConfiguration.Binding != nil {
		if plugin, exist := kvConfig.NetworkConfiguration.Binding[pluginName]; exist {
			return &plugin
		}
	}

	return nil
}
