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

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/hooks"
	"kubevirt.io/kubevirt/pkg/network/vmispec"
)

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
	var userProvided map[string]v1.InterfaceBindingPlugin
	if config.NetworkConfiguration != nil && config.NetworkConfiguration.Binding != nil {
		userProvided = config.NetworkConfiguration.Binding
	}
	bindingByName := vmispec.MergeBindingPlugins(userProvided)

	// Find bindings that are actually used by VMI interfaces
	usedBindings := make(map[string]v1.InterfaceBindingPlugin)
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
	for _, pluginInfo := range usedBindings {
		if pluginInfo.SidecarImage != "" {
			sidecar := hooks.HookSidecar{
				Image:           pluginInfo.SidecarImage,
				ImagePullPolicy: config.ImagePullPolicy,
				DownwardAPI:     pluginInfo.DownwardAPI,
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
