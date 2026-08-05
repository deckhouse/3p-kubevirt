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

package netpod

import (
	"fmt"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/network/cache"
	neterrors "kubevirt.io/kubevirt/pkg/network/errors"
	"kubevirt.io/kubevirt/pkg/network/namescheme"
)

type stateCacheReaderWriterDeleter interface {
	Read(networkName string) (cache.PodIfaceState, error)
	Write(networkName string, state cache.PodIfaceState) error
	Delete(networkName string) error
	Keys() ([]string, error)
}

type State struct {
	cache stateCacheReaderWriterDeleter

	NSExec NSExecutor

	// BPFBridgePodIfaceByNetwork maps a VMI network name to the resolved pod
	// interface name for bpfbridge-bound interfaces, as computed during Setup.
	// Teardown reuses this mapping to detach BPF resources from the exact devices
	// that Setup attached them to, instead of re-resolving with divergent logic.
	BPFBridgePodIfaceByNetwork map[string]string
}

func NewState(cache stateCacheReaderWriterDeleter, ns NSExecutor) *State {
	return &State{cache: cache, NSExec: ns, BPFBridgePodIfaceByNetwork: map[string]string{}}
}

func (s *State) PendingStartedFinished(nets []v1.Network) ([]v1.Network, []v1.Network, []v1.Network, error) {
	var pendingNets []v1.Network
	var startedNets []v1.Network
	var finishedNets []v1.Network
	for _, net := range nets {
		state, err := s.cache.Read(net.Name)
		if err != nil {
			return nil, nil, nil, err
		}

		switch state {
		case cache.PodIfaceNetworkPreparationPending:
			pendingNets = append(pendingNets, net)
		case cache.PodIfaceNetworkPreparationStarted:
			startedNets = append(startedNets, net)
		case cache.PodIfaceNetworkPreparationFinished:
			finishedNets = append(finishedNets, net)
		}
	}
	return pendingNets, startedNets, finishedNets, nil
}

func (s *State) SetStarted(nets []v1.Network) error {
	for _, net := range nets {
		if werr := s.cache.Write(net.Name, cache.PodIfaceNetworkPreparationStarted); werr != nil {
			return fmt.Errorf("failed to mark configuration as started for %s: %v", net.Name, werr)
		}
	}
	return nil
}

func (s *State) SetFinished(nets []v1.Network) error {
	for _, net := range nets {
		if werr := s.cache.Write(net.Name, cache.PodIfaceNetworkPreparationFinished); werr != nil {
			return neterrors.CreateCriticalNetworkError(
				fmt.Errorf("failed to mark configuration as finished for %s: %w", net.Name, werr),
			)
		}
	}
	return nil
}

// OrphanedNetworks returns cached network names that are no longer part of the
// VMI spec. This happens when an interface is hot-unplugged by removing it from
// the spec outright (without the Absent phase): the spec no longer mentions the
// network, but the state cache and the pod netns still hold its leftovers.
func (s *State) OrphanedNetworks(specNets []v1.Network) ([]string, error) {
	cachedNames, err := s.cache.Keys()
	if err != nil {
		return nil, err
	}
	specNames := map[string]struct{}{}
	for _, net := range specNets {
		specNames[net.Name] = struct{}{}
	}
	var orphaned []string
	for _, name := range cachedNames {
		if _, inSpec := specNames[name]; inSpec {
			continue
		}
		// Legacy cache keys of the ordinal/primary pod-interface naming are not
		// network names; they are migrated by upgradeConfigStateCache and must
		// not be destroyed as orphans before that happens.
		if name == namescheme.PrimaryPodInterfaceName || namescheme.OrdinalSecondaryInterfaceName(name) {
			continue
		}
		orphaned = append(orphaned, name)
	}
	return orphaned, nil
}

func (s *State) Delete(nets []v1.Network) error {
	for _, net := range nets {
		if err := s.cache.Delete(net.Name); err != nil {
			return fmt.Errorf("failed to clear state cache for %s: %w", net.Name, err)
		}
	}
	return nil
}
