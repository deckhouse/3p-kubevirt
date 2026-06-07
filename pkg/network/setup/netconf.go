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

package network

import (
	"fmt"
	"strconv"
	"sync"

	k8serrors "k8s.io/apimachinery/pkg/util/errors"

	"kubevirt.io/client-go/log"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter"

	"kubevirt.io/kubevirt/pkg/network/bpfbridge"
	"kubevirt.io/kubevirt/pkg/network/cache"
	netdriver "kubevirt.io/kubevirt/pkg/network/driver"
	"kubevirt.io/kubevirt/pkg/network/istio"
	"kubevirt.io/kubevirt/pkg/network/link"
	"kubevirt.io/kubevirt/pkg/network/namescheme"
	"kubevirt.io/kubevirt/pkg/network/netns"
	"kubevirt.io/kubevirt/pkg/network/setup/netpod"
	"kubevirt.io/kubevirt/pkg/network/setup/netpod/masquerade"
	"kubevirt.io/kubevirt/pkg/network/vmispec"
)

type cacheCreator interface {
	New(filePath string) *cache.Cache
}

type clusterConfigurer interface {
	GetNetworkBindings() map[string]v1.InterfaceBindingPlugin
}

type NetConf struct {
	cacheCreator     cacheCreator
	nsFactory        nsFactory
	state            map[string]*netpod.State
	statePid         map[string]int
	configStateMutex *sync.RWMutex

	clusterConfigurer clusterConfigurer
}

type nsFactory func(int) NSExecutor

type NSExecutor interface {
	Do(func() error) error
}

func NewNetConf(clusterConfigurer clusterConfigurer) *NetConf {
	var cacheFactory cache.CacheCreator
	return NewNetConfWithCustomFactoryAndConfigState(func(pid int) NSExecutor {
		return netns.New(pid)
	}, cacheFactory, map[string]*netpod.State{}, clusterConfigurer)
}

func NewNetConfWithCustomFactoryAndConfigState(nsFactory nsFactory, cacheCreator cacheCreator, state map[string]*netpod.State, clusterConfigurer clusterConfigurer) *NetConf {
	return &NetConf{
		state:             state,
		statePid:          map[string]int{},
		configStateMutex:  &sync.RWMutex{},
		cacheCreator:      cacheCreator,
		nsFactory:         nsFactory,
		clusterConfigurer: clusterConfigurer,
	}
}

// Setup applies (privilege) network related changes for an existing virt-launcher pod.
func (c *NetConf) Setup(vmi *v1.VirtualMachineInstance, networks []v1.Network, launcherPid int) error {
	c.configStateMutex.RLock()
	state, ok := c.state[string(vmi.UID)]
	cachedPid := c.statePid[string(vmi.UID)]
	c.configStateMutex.RUnlock()
	launcherPidCache := cache.NewLauncherPidCache(c.cacheCreator, string(vmi.UID))
	if ok && cachedPid != launcherPid {
		if err := c.Teardown(vmi); err != nil {
			return fmt.Errorf("netconf teardown for replaced launcher pod failed: %w", err)
		}
		ok = false
	} else if !ok {
		diskPid, err := launcherPidCache.Read()
		if err != nil {
			return err
		}
		if diskPid != launcherPid {
			if err := c.Teardown(vmi); err != nil {
				return fmt.Errorf("netconf teardown for replaced launcher pod failed: %w", err)
			}
		}
	}
	if !ok {
		if err := launcherPidCache.Write(launcherPid); err != nil {
			return err
		}
		stateCache := NewConfigStateCache(string(vmi.UID), c.cacheCreator)
		configStateCache, err := upgradeConfigStateCache(&stateCache, networks, c.cacheCreator, string(vmi.UID))
		if err != nil {
			return err
		}
		ns := c.nsFactory(launcherPid)
		state = netpod.NewState(configStateCache, ns)
		c.configStateMutex.Lock()
		c.state[string(vmi.UID)] = state
		c.statePid[string(vmi.UID)] = launcherPid
		c.configStateMutex.Unlock()
	}

	ownerID, _ := strconv.Atoi(netdriver.LibvirtUserAndGroupId)
	if util.IsNonRootVMI(vmi) {
		ownerID = util.NonRootUID
	}
	queuesCapacity := int(converter.NetworkQueuesCapacity(vmi))
	disableTapVethBridge := hasBPFBridgeBinding(vmi)
	netPod := netpod.NewNetPod(
		networks,
		vmispec.FilterInterfacesByNetworks(vmi.Spec.Domain.Devices.Interfaces, networks),
		string(vmi.UID),
		launcherPid,
		ownerID,
		queuesCapacity,
		state,
		netpod.WithMasqueradeAdapter(newMasqueradeAdapter(vmi)),
		netpod.WithCacheCreator(c.cacheCreator),
		netpod.WithBindingPlugins(c.clusterConfigurer.GetNetworkBindings()),
		netpod.WithDisableTapVethBridge(disableTapVethBridge),
		netpod.WithLogger(log.Log.Object(vmi)),
		netpod.WithVMIIfaceStatuses(vmi.Status.Interfaces),
	)

	if err := netPod.Setup(); err != nil {
		return fmt.Errorf("setup failed, err: %w", err)
	}
	return nil
}

func upgradeConfigStateCache(stateCache *ConfigStateCache, networks []v1.Network, cacheCreator cacheCreator, vmiUID string) (*ConfigStateCache, error) {
	for networkName, podIfaceName := range namescheme.CreateOrdinalNetworkNameScheme(networks) {
		exists, err := stateCache.Exists(podIfaceName)
		if err != nil {
			return nil, err
		}
		if exists {
			data, rErr := stateCache.Read(podIfaceName)
			if rErr != nil {
				return nil, rErr
			}
			if wErr := stateCache.Write(networkName, data); wErr != nil {
				return nil, wErr
			}
			if dErr := stateCache.Delete(podIfaceName); dErr != nil {
				log.Log.Reason(dErr).Errorf("failed to delete pod interface (%s) state from cache", podIfaceName)
			}
			if dErr := cache.DeletePodInterfaceCache(cacheCreator, vmiUID, podIfaceName); dErr != nil {
				log.Log.Reason(dErr).Errorf("failed to delete pod interface (%s) from cache", podIfaceName)
			}
		}
	}
	return stateCache, nil
}

func (c *NetConf) Teardown(vmi *v1.VirtualMachineInstance) error {
	// Snapshot the per-VMI state under the same critical section that removes it from
	// the map. teardownBPFBridge needs the cached NSExecutor (carrying the launcher
	// PID's /proc/<pid>/ns/net path) to detach BPF resources inside the pod-netns
	// *before* we forget about this VMI.
	c.configStateMutex.Lock()
	state := c.state[string(vmi.UID)]
	delete(c.state, string(vmi.UID))
	delete(c.statePid, string(vmi.UID))
	c.configStateMutex.Unlock()

	var errs []error
	if err := c.teardownBPFBridge(vmi, state); err != nil {
		errs = append(errs, fmt.Errorf("bpfbridge teardown failed: %w", err))
	}

	podCache := cache.NewPodInterfaceCache(c.cacheCreator, string(vmi.UID))
	if err := podCache.Remove(); err != nil {
		errs = append(errs, fmt.Errorf("pod cache teardown failed: %w", err))
	}

	if err := k8serrors.NewAggregate(errs); err != nil {
		return fmt.Errorf("teardown failed, err: %w", err)
	}
	return nil
}

// teardownBPFBridge removes the TC ingress BPF filter and clsact qdisc that
// bpfbridge.Attach installed on the TAP and pod-side interfaces of every VMI
// interface bound to the "bpfbridge" plugin.
//
// The bulk of the work runs inside the pod-netns via state.NSExec.Do, because the
// devices we are detaching from ("eth0", "tap0", ...) only exist by that name there;
// running netlink calls from the host netns would either fail or — worse — touch
// the wrong devices.
//
// If no NSExecutor was ever cached for this VMI (e.g. virt-handler restarted between
// Setup and Teardown), we cannot enter the right netns and we bail with a warning.
// That is safe: the pod-netns is about to die with the pod and the kernel reclaims
// the TC filters, clsact qdiscs and BPF program objects on its own.
//
// Per-interface errors are accumulated and returned as a single aggregate; a failure
// to enter the netns is returned separately so the caller can distinguish it from
// per-device cleanup failures.
func (c *NetConf) teardownBPFBridge(vmi *v1.VirtualMachineInstance, state *netpod.State) error {
	if !hasBPFBridgeBinding(vmi) {
		return nil
	}
	if state == nil || state.NSExec == nil {
		log.Log.Object(vmi).Warning("bpfbridge teardown: no cached pod-netns executor, skipping detach")
		return nil
	}

	podIfaceNameByVMINetwork := bpfBridgeTeardownNetworkNameScheme(vmi.Spec.Networks)

	var errs []error
	nsErr := state.NSExec.Do(func() error {
		for _, iface := range vmi.Spec.Domain.Devices.Interfaces {
			if iface.Binding == nil || iface.Binding.Name != "bpfbridge" {
				continue
			}

			podIfaceName, exists := podIfaceNameByVMINetwork[iface.Name]
			if !exists {
				err := fmt.Errorf("pod interface name not found for network %q", iface.Name)
				log.Log.Object(vmi).Reason(err).Warning("bpfbridge teardown skipped")
				errs = append(errs, err)
				continue
			}

			vmiNetwork := vmispec.LookupNetworkByName(vmi.Spec.Networks, iface.Name)
			if vmiNetwork == nil {
				err := fmt.Errorf("network %q not found", iface.Name)
				log.Log.Object(vmi).Reason(err).Warning("bpfbridge teardown skipped")
				errs = append(errs, err)
				continue
			}

			tapName := link.GenerateTapDeviceName(podIfaceName, *vmiNetwork)
			if err := bpfbridge.Detach(tapName, podIfaceName); err != nil {
				log.Log.Object(vmi).Reason(err).Warningf("bpfbridge teardown failed for network %q", iface.Name)
				errs = append(errs, fmt.Errorf("network %q: %w", iface.Name, err))
			}
		}
		// NSExec.Do is used purely as a netns switcher here; we drain errs in the
		// outer scope so a single device failure does not short-circuit the rest of
		// the cleanup.
		return nil
	})
	if nsErr != nil {
		log.Log.Object(vmi).Reason(nsErr).Warning("bpfbridge teardown: failed to enter pod netns, skipping detach")
		return fmt.Errorf("enter pod netns: %w", nsErr)
	}

	return k8serrors.NewAggregate(errs)
}

func bpfBridgeTeardownNetworkNameScheme(networks []v1.Network) map[string]string {
	podIfaceNamesByNetworkName := namescheme.CreateOrdinalNetworkNameScheme(networks)
	for _, network := range networks {
		if network.Pod != nil && network.Name != "default" {
			podIfaceNamesByNetworkName[network.Name] = network.Name
		}
	}
	return podIfaceNamesByNetworkName
}

func hasBPFBridgeBinding(vmi *v1.VirtualMachineInstance) bool {
	for _, iface := range vmi.Spec.Domain.Devices.Interfaces {
		if iface.Binding != nil && iface.Binding.Name == "bpfbridge" {
			return true
		}
	}
	return false
}

func newMasqueradeAdapter(vmi *v1.VirtualMachineInstance) masquerade.MasqPod {
	if vmi.Status.MigrationTransport == v1.MigrationTransportUnix {
		return masquerade.New(masquerade.WithIstio(istio.ProxyInjectionEnabled(vmi)))
	} else {
		return masquerade.New(
			masquerade.WithIstio(istio.ProxyInjectionEnabled(vmi)),
			masquerade.WithLegacyMigrationPorts(),
		)
	}
}
