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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/network/namescheme"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter"

	"kubevirt.io/kubevirt/pkg/network/cache"
	netdriver "kubevirt.io/kubevirt/pkg/network/driver"
	"kubevirt.io/kubevirt/pkg/network/istio"
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
	log.Log.Object(vmi).Infof("DEBUG-NETCONF: enter ok=%v cachedPid=%d launcherPid=%d", ok, cachedPid, launcherPid)
	if ok && cachedPid != launcherPid {
		log.Log.Object(vmi).Infof("DEBUG-NETCONF: in-memory PID mismatch, Teardown")
		if err := c.Teardown(vmi); err != nil {
			return fmt.Errorf("netconf teardown for replaced launcher pod failed: %w", err)
		}
		ok = false
	}
	if !ok {
		diskPid, err := readPersistedLauncherPid(string(vmi.UID))
		if err != nil {
			log.Log.Object(vmi).Reason(err).Errorf("DEBUG-NETCONF: readPersistedLauncherPid failed")
			return err
		}
		log.Log.Object(vmi).Infof("DEBUG-NETCONF: !ok branch diskPid=%d launcherPid=%d", diskPid, launcherPid)
		if diskPid != launcherPid {
			log.Log.Object(vmi).Infof("DEBUG-NETCONF: disk PID mismatch, Teardown")
			if err := c.Teardown(vmi); err != nil {
				return fmt.Errorf("netconf teardown for replaced launcher pod failed: %w", err)
			}
		}
		if err := writePersistedLauncherPid(string(vmi.UID), launcherPid); err != nil {
			log.Log.Object(vmi).Reason(err).Errorf("DEBUG-NETCONF: writePersistedLauncherPid failed")
			return err
		}
		log.Log.Object(vmi).Infof("DEBUG-NETCONF: writePersistedLauncherPid done")
		cache := NewConfigStateCache(string(vmi.UID), c.cacheCreator)
		configStateCache, err := upgradeConfigStateCache(&cache, networks, c.cacheCreator, string(vmi.UID))
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
	netpod := netpod.NewNetPod(
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
		netpod.WithLogger(log.Log.Object(vmi)),
		netpod.WithVMIIfaceStatuses(vmi.Status.Interfaces),
	)

	log.Log.Object(vmi).Infof("DEBUG-NETCONF: calling netpod.Setup")
	if err := netpod.Setup(); err != nil {
		log.Log.Object(vmi).Reason(err).Errorf("DEBUG-NETCONF: netpod.Setup failed")
		return fmt.Errorf("setup failed, err: %w", err)
	}
	log.Log.Object(vmi).Infof("DEBUG-NETCONF: netpod.Setup returned nil")
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
	c.configStateMutex.Lock()
	delete(c.state, string(vmi.UID))
	delete(c.statePid, string(vmi.UID))
	c.configStateMutex.Unlock()
	podCache := cache.NewPodInterfaceCache(c.cacheCreator, string(vmi.UID))
	if err := podCache.Remove(); err != nil {
		return fmt.Errorf("teardown failed, err: %w", err)
	}
	_ = removePersistedLauncherPid(string(vmi.UID))

	return nil
}

func persistedLauncherPidPath(vmiUID string) string {
	return filepath.Join(util.VirtPrivateDir, "network-info-cache", vmiUID, ".launcher-pid")
}

func readPersistedLauncherPid(vmiUID string) (int, error) {
	data, err := os.ReadFile(persistedLauncherPidPath(vmiUID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, nil
	}
	return pid, nil
}

func writePersistedLauncherPid(vmiUID string, launcherPid int) error {
	path := persistedLauncherPidPath(vmiUID)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(launcherPid)), 0640)
}

func removePersistedLauncherPid(vmiUID string) error {
	err := os.Remove(persistedLauncherPidPath(vmiUID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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
