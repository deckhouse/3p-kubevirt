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
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/util/errors"

	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/util"

	"kubevirt.io/kubevirt/pkg/network/bpfbridge"
	"kubevirt.io/kubevirt/pkg/network/cache"
	"kubevirt.io/kubevirt/pkg/network/driver/nmstate"
	"kubevirt.io/kubevirt/pkg/network/driver/procsys"
	neterrors "kubevirt.io/kubevirt/pkg/network/errors"
	"kubevirt.io/kubevirt/pkg/network/link"
	"kubevirt.io/kubevirt/pkg/network/namescheme"
	"kubevirt.io/kubevirt/pkg/network/netmachinery"
	"kubevirt.io/kubevirt/pkg/network/setup/netpod/masquerade"
	"kubevirt.io/kubevirt/pkg/network/vmispec"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"

	"kubevirt.io/client-go/log"

	v1 "kubevirt.io/api/core/v1"
)

type nmstateAdapter interface {
	Apply(spec *nmstate.Spec) error
	Read() (*nmstate.Status, error)
}

type masqueradeAdapter interface {
	Setup(bridgeIfaceSpec, podIfaceSpec *nmstate.Interface, vmiIface v1.Interface) error
}

// bpfBridgeAdapter abstracts the BPF program load + TC attachment so the netpod
// logic is unit-testable without a real kernel/netlink. The production
// implementation delegates to pkg/network/bpfbridge.
type bpfBridgeAdapter interface {
	EnsureWiring(tapName, podIfaceName string) error
	Attach(objPath, tapName, podIfaceName string) error
}

type cacheCreator interface {
	New(filePath string) *cache.Cache
}

type NSExecutor interface {
	Do(func() error) error
}

type NetPod struct {
	vmiSpecIfaces    []v1.Interface
	vmiSpecNets      []v1.Network
	vmiIfaceStatuses []v1.VirtualMachineInstanceNetworkInterface
	vmiUID           string
	podPID           int
	ownerID          int
	queuesCapByIface map[string]int

	nmstateAdapter    nmstateAdapter
	masqueradeAdapter masqueradeAdapter
	bpfBridgeAdapter  bpfBridgeAdapter

	cacheCreator cacheCreator
	state        *State

	bindingPluginsByName map[string]v1.InterfaceBindingPlugin

	// externalTapProvisioning, when set, delegates secondary bpfbridge TAP creation
	// to an external service (SDN): nmstate does not create/manage the secondary TAP.
	// When false (default), bpfbridge provisions TAP devices natively via nmstate. The
	// BPF attach in setupBPFBridge always runs, whether the TAP is native or external.
	externalTapProvisioning bool

	// orphanedNetworks lists networks that still have leftovers (state cache,
	// TAP device) but are no longer part of the FULL VMI spec. Computed by the
	// caller against vmi.Spec.Networks: the networks passed to NewNetPod are a
	// filtered subset and must not be used as the comparison base.
	orphanedNetworks []string

	log *log.FilteredLogger
}

type option func(*NetPod)

func NewNetPod(vmiNetworks []v1.Network, vmiIfaces []v1.Interface, vmiUID string, podPID, ownerID, queuesCapacity int, state *State, opts ...option) NetPod {
	n := NetPod{
		vmiSpecIfaces: vmiIfaces,
		vmiSpecNets:   vmiNetworks,
		vmiUID:        vmiUID,
		podPID:        podPID,
		ownerID:       ownerID,
		state:         state,

		nmstateAdapter:    nmstate.New(),
		masqueradeAdapter: masquerade.New(),
		bpfBridgeAdapter:  defaultBpfBridgeAdapter{},

		cacheCreator:         cache.CacheCreator{},
		bindingPluginsByName: map[string]v1.InterfaceBindingPlugin{},

		log: log.Log,
	}
	for _, opt := range opts {
		opt(&n)
	}

	n.queuesCapByIface = calcQueuesCapByIface(queuesCapacity, n.vmiSpecIfaces, n.vmiIfaceStatuses)

	return n
}

func WithNMStateAdapter(h nmstateAdapter) option {
	return func(n *NetPod) {
		n.nmstateAdapter = h
	}
}

func WithMasqueradeAdapter(h masqueradeAdapter) option {
	return func(n *NetPod) {
		n.masqueradeAdapter = h
	}
}

func WithBpfBridgeAdapter(h bpfBridgeAdapter) option {
	return func(n *NetPod) {
		n.bpfBridgeAdapter = h
	}
}

// WithExternalTapProvisioning configures the bpfbridge binding to delegate
// secondary TAP provisioning to an external service (SDN) when true: nmstate does
// not create/manage the secondary TAP. When false (default behaviour), bpfbridge
// provisions TAP devices natively via nmstate. The BPF TC attach always runs so
// kubevirt owns the L2 bridge between the TAP and the pod interface.
func WithExternalTapProvisioning(external bool) option {
	return func(n *NetPod) {
		n.externalTapProvisioning = external
	}
}

func WithCacheCreator(c cacheCreator) option {
	return func(n *NetPod) {
		n.cacheCreator = c
	}
}

func WithBindingPlugins(bindings map[string]v1.InterfaceBindingPlugin) option {
	return func(n *NetPod) {
		n.bindingPluginsByName = bindings
	}
}

func WithLogger(logger *log.FilteredLogger) option {
	return func(n *NetPod) {
		n.log = logger
	}
}

func WithVMIIfaceStatuses(vmiIfaceStatuses []v1.VirtualMachineInstanceNetworkInterface) option {
	return func(n *NetPod) {
		n.vmiIfaceStatuses = vmiIfaceStatuses
	}
}

func WithOrphanedNetworks(networkNames []string) option {
	return func(n *NetPod) {
		n.orphanedNetworks = networkNames
	}
}

func (n NetPod) Setup() error {
	// Not all network bindings are processed in the network setup.
	filteredNets, err := filterSupportedBindingNetworks(n.vmiSpecNets, n.vmiSpecIfaces)
	if err != nil {
		return err
	}

	pendingNets, startedNets, finishedNets, err := n.state.PendingStartedFinished(filteredNets)
	if err != nil {
		return err
	}
	if err := n.validateNoNetworkReconfigured(startedNets); err != nil {
		return err
	}

	unplugIfaces := n.unplugInterfaces(startedNets, finishedNets)

	// The pending networks should not include networks that are marked for removal.
	// Filter out such networks for the pending network list.
	pendingNets = vmispec.FilterNetworksSpec(pendingNets, func(net v1.Network) bool {
		iface := vmispec.LookupInterfaceByName(n.vmiSpecIfaces, net.Name)
		return iface != nil && iface.State != v1.InterfaceStateAbsent
	})

	if len(pendingNets) == 0 && len(unplugIfaces) == 0 {
		if len(n.orphanedNetworks) == 0 {
			return nil
		}
		return n.state.NSExec.Do(func() error {
			currentStatus, err := n.nmstateAdapter.Read()
			if err != nil {
				return err
			}
			// Cleaning up leftovers of already unplugged networks is housekeeping:
			// a failure must not fail the sync, it is retried on the next reconcile
			// (the leftovers stay recorded in the state cache).
			if cerr := n.cleanupOrphanedNetworks(currentStatus.Interfaces); cerr != nil {
				n.log.Reason(cerr).Warning("failed to clean up leftovers of unplugged networks")
			}
			return nil
		})
	}

	err = n.state.NSExec.Do(func() error {
		currentStatus, err := n.nmstateAdapter.Read()
		if err != nil {
			return err
		}

		currentStatusBytes, err := json.Marshal(currentStatus)
		if err != nil {
			return err
		}
		n.log.Infof("Current pod network: %s", currentStatusBytes)

		if derr := n.discover(currentStatus); derr != nil {
			return derr
		}

		if serr := n.state.SetStarted(pendingNets); serr != nil {
			return serr
		}

		if err = n.config(currentStatus); err != nil {
			log.Log.Reason(err).Errorf("failed to configure pod network")
			return neterrors.CreateCriticalNetworkError(err)
		}

		// Housekeeping only: a cleanup failure must not abort the setup, otherwise
		// the networks configured above would stay in the "started" state and the
		// next reconcile would report them as non-restartable (a critical network
		// error that fails the VMI).
		if cerr := n.cleanupOrphanedNetworks(currentStatus.Interfaces); cerr != nil {
			n.log.Reason(cerr).Warning("failed to clean up leftovers of unplugged networks")
		}

		return nil
	})
	if err != nil {
		return err
	}

	if serr := n.state.SetFinished(pendingNets); serr != nil {
		return serr
	}

	unplugNetworks := vmispec.FilterNetworksByInterfaces(n.vmiSpecNets, unplugIfaces)
	if serr := n.clearCache(unplugNetworks); serr != nil {
		return serr
	}

	return nil
}

func (n NetPod) validateNoNetworkReconfigured(startedNets []v1.Network) error {
	if len(startedNets) > 0 {
		for _, net := range startedNets {
			startedIface := vmispec.LookupInterfaceByName(n.vmiSpecIfaces, net.Name)
			if startedIface != nil && startedIface.State != v1.InterfaceStateAbsent {
				return neterrors.CreateCriticalNetworkError(
					fmt.Errorf("preparation for networks %v cannot be restarted", startedNets),
				)
			}
		}
	}
	return nil
}

func (n NetPod) config(currentStatus *nmstate.Status) error {
	desiredSpec, err := n.composeDesiredSpec(currentStatus)
	if err != nil {
		return err
	}

	desiredSpecBytes, err := json.Marshal(desiredSpec)
	if err != nil {
		return err
	}
	n.log.Infof("Desired pod network: %s", desiredSpecBytes)

	if err = n.nmstateAdapter.Apply(desiredSpec); err != nil {
		return err
	}

	if err = n.setupBPFBridge(currentStatus); err != nil {
		return err
	}

	// Configuring NAT (nftables) is temporary done outside nmstate.
	// This should be eventually embedded into the nmstate desired state and applied by it.
	return n.setupNAT(desiredSpec, currentStatus)
}

func (n NetPod) composeDesiredSpec(currentStatus *nmstate.Status) (*nmstate.Spec, error) {
	podIfaceStatusByName := ifaceStatusByName(currentStatus.Interfaces)

	podIfaceNameByVMINetwork := n.createNetworkNameScheme(currentStatus.Interfaces)

	spec := nmstate.Spec{Interfaces: []nmstate.Interface{}}

	for ifIndex, iface := range n.vmiSpecIfaces {
		if skipPodInterfaceIsNotDefault(iface.Name, n.vmiSpecNets) && !vmispec.IsBPFBridgeBinding(iface) {
			continue
		}

		var (
			ifacesSpec []nmstate.Interface
			err        error
		)
		podIfaceName := podIfaceNameByVMINetwork[iface.Name]

		switch {
		case iface.Bridge != nil:
			// A missing pod interface is not considered an error in case the interface is marked for removal.
			if _, exists := podIfaceStatusByName[podIfaceName]; !exists && iface.State != v1.InterfaceStateAbsent {
				return nil, fmt.Errorf("pod link (%s) is missing", podIfaceName)
			}
			ifacesSpec, err = n.bridgeBindingSpec(podIfaceName, ifIndex, podIfaceStatusByName)

			if nmstate.AnyInterface(ifacesSpec, hasIP4GlobalUnicast) {
				spec.LinuxStack.IPv4.ArpIgnore = pointer.P(procsys.ARPReplyMode1)
			}

			if iface.State == v1.InterfaceStateAbsent {
				var filteredIfacesSpec []nmstate.Interface
				for _, ifaceSpec := range ifacesSpec {
					// Interfaces with no type are not owned by kubevirt, therefore not removed.
					if ifaceSpec.TypeName != "" {
						ifaceSpec.State = nmstate.IfaceStateAbsent
						filteredIfacesSpec = append(filteredIfacesSpec, ifaceSpec)
					}
				}
				ifacesSpec = filteredIfacesSpec
			}

		case iface.Masquerade != nil:
			if _, exists := podIfaceStatusByName[podIfaceName]; !exists {
				return nil, fmt.Errorf("pod link (%s) is missing", podIfaceName)
			}
			ifacesSpec, err = n.masqueradeBindingSpec(podIfaceName, ifIndex, podIfaceStatusByName)

			if nmstate.AnyInterface(ifacesSpec, hasIP4GlobalUnicast) {
				spec.LinuxStack.IPv4.Forwarding = pointer.P(true)
			}
			if nmstate.AnyInterface(ifacesSpec, hasIP6GlobalUnicast) {
				spec.LinuxStack.IPv6.Forwarding = pointer.P(true)
			}
		case iface.SRIOV != nil:
		case iface.Binding != nil:
			bindingPlugin, exists := n.bindingPluginsByName[iface.Binding.Name]
			if exists && vmispec.IsBPFBridgeBinding(iface) {
				// An interface marked for removal must not fail the spec composition:
				// SDN tears down its side on its own, so both the SDN report and the
				// pod link may legitimately be gone already. The TAP, however, is ours
				// to delete — its name derives from the VMI network alone, so it can be
				// removed without resolving the (possibly gone) SDN pod interface. The
				// TC filters die with their devices, releasing the BPF program.
				if iface.State == v1.InterfaceStateAbsent {
					ifacesSpec = n.bpfBridgeAbsentSpec(ifIndex, currentStatus.Interfaces)
					break
				}
				if podIfaceName == "" {
					return nil, fmt.Errorf("SDN pod interface for network %s is not present yet", iface.Name)
				}
				if _, exists := podIfaceStatusByName[podIfaceName]; !exists {
					return nil, fmt.Errorf("pod link (%s) is missing", podIfaceName)
				}
				ifacesSpec, err = n.bpfBridgeSpec(podIfaceName, ifIndex, podIfaceStatusByName)
			} else if exists && bindingPlugin.DomainAttachmentType == v1.ManagedTap {
				if _, exists := podIfaceStatusByName[podIfaceName]; !exists {
					return nil, fmt.Errorf("pod link (%s) is missing", podIfaceName)
				}
				ifacesSpec, err = n.managedTapSpec(podIfaceName, ifIndex, podIfaceStatusByName)
				if nmstate.AnyInterface(ifacesSpec, hasIP4GlobalUnicast) {
					spec.LinuxStack.IPv4.ArpIgnore = pointer.P(procsys.ARPReplyMode1)
				}
			}

		// Passt is removed in v1.3. This scenario is tracking old VMIs that are still processed in the reconcile loop.
		case iface.DeprecatedPasst != nil:
			spec.LinuxStack.IPv4.PingGroupRange = []int{util.NonRootUID, util.NonRootUID}
			spec.LinuxStack.IPv4.UnprivilegedPortStart = pointer.P(0)
		// Macvtap is removed in v1.3. This scenario is tracking old VMIs that are still processed in the reconcile loop.
		case iface.DeprecatedMacvtap != nil:
		// SLIRP is removed in v1.3. This scenario is tracking old VMIs that are still processed in the reconcile loop.
		case iface.DeprecatedSlirp != nil:
		default:
			return nil, fmt.Errorf("undefined binding method: %v", iface)
		}
		if err != nil {
			return nil, err
		}
		spec.Interfaces = append(spec.Interfaces, ifacesSpec...)
	}

	return &spec, nil
}

func (n NetPod) bridgeBindingSpec(podIfaceName string, vmiIfaceIndex int, ifaceStatusByName map[string]nmstate.Interface) ([]nmstate.Interface, error) {
	const (
		bridgeFakeIPBase = "169.254.75.1"
		bridgeFakePrefix = 32
	)

	vmiNetworkName := n.vmiSpecIfaces[vmiIfaceIndex].Name
	vmiNetwork := vmispec.LookupNetworkByName(n.vmiSpecNets, vmiNetworkName)

	bridgeIface := nmstate.Interface{
		Name:     link.GenerateBridgeName(podIfaceName),
		TypeName: nmstate.TypeBridge,
		State:    nmstate.IfaceStateUp,
		Ethtool:  nmstate.Ethtool{Feature: nmstate.Feature{TxChecksum: pointer.P(false)}},
		Metadata: &nmstate.IfaceMetadata{NetworkName: vmiNetworkName},
	}

	podIfaceAlternativeName := link.GenerateNewBridgedVmiInterfaceName(podIfaceName)
	podStatusIface, exist := ifaceStatusByName[podIfaceAlternativeName]
	if !exist {
		podStatusIface = ifaceStatusByName[podIfaceName]
	}

	if hasIPGlobalUnicast(podStatusIface.IPv4) {
		bridgeIface.IPv4 = nmstate.IP{
			Enabled: pointer.P(true),
			Address: []nmstate.IPAddress{
				{
					IP:        bridgeFakeIPBase + strconv.Itoa(vmiIfaceIndex),
					PrefixLen: bridgeFakePrefix,
				},
			},
		}
	}

	podIface := nmstate.Interface{
		Index:       podStatusIface.Index,
		Name:        podIfaceAlternativeName,
		State:       nmstate.IfaceStateUp,
		CopyMacFrom: bridgeIface.Name,
		Controller:  bridgeIface.Name,
		IPv4:        nmstate.IP{Enabled: pointer.P(false)},
		IPv6:        nmstate.IP{Enabled: pointer.P(false)},
		LinuxStack:  nmstate.LinuxIfaceStack{PortLearning: pointer.P(false)},
		Metadata:    &nmstate.IfaceMetadata{NetworkName: vmiNetworkName},
	}

	tapIface := nmstate.Interface{
		Name:       link.GenerateTapDeviceName(podIfaceName, *vmiNetwork),
		TypeName:   nmstate.TypeTap,
		State:      nmstate.IfaceStateUp,
		MTU:        podStatusIface.MTU,
		Controller: bridgeIface.Name,
		Tap: &nmstate.TapDevice{
			Queues: n.networkQueues(vmiIfaceIndex),
			UID:    n.ownerID,
			GID:    n.ownerID,
		},
		Metadata: &nmstate.IfaceMetadata{Pid: n.podPID, NetworkName: vmiNetworkName},
	}

	dummyIface := nmstate.Interface{
		Name:       podIfaceName,
		TypeName:   nmstate.TypeDummy,
		MacAddress: podStatusIface.MacAddress,
		MTU:        podStatusIface.MTU,
		IPv4:       podStatusIface.IPv4,
		IPv6:       podStatusIface.IPv6,
		Metadata:   &nmstate.IfaceMetadata{NetworkName: vmiNetworkName},
	}

	return []nmstate.Interface{bridgeIface, podIface, tapIface, dummyIface}, nil
}

func (n NetPod) networkQueues(vmiIfaceIndex int) int {
	iface := n.vmiSpecIfaces[vmiIfaceIndex]
	if ifaceModel := iface.Model; ifaceModel == "" || ifaceModel == v1.VirtIO {
		return n.queuesCapByIface[iface.Name]
	}

	return 0
}

func (n NetPod) masqueradeBindingSpec(podIfaceName string, vmiIfaceIndex int, ifaceStatusByName map[string]nmstate.Interface) ([]nmstate.Interface, error) {
	podIface := ifaceStatusByName[podIfaceName]

	vmiNetworkName := n.vmiSpecIfaces[vmiIfaceIndex].Name
	vmiNetwork := vmispec.LookupNetworkByName(n.vmiSpecNets, vmiNetworkName)

	bridgeIface := nmstate.Interface{
		Name:       link.GenerateBridgeName(podIfaceName),
		TypeName:   nmstate.TypeBridge,
		State:      nmstate.IfaceStateUp,
		MacAddress: link.StaticMasqueradeBridgeMAC,
		MTU:        podIface.MTU,
		Ethtool:    nmstate.Ethtool{Feature: nmstate.Feature{TxChecksum: pointer.P(false)}},
		IPv4:       nmstate.IP{Enabled: pointer.P(false)},
		IPv6:       nmstate.IP{Enabled: pointer.P(false)},
		Metadata:   &nmstate.IfaceMetadata{NetworkName: vmiNetwork.Name},
	}

	if hasIPGlobalUnicast(podIface.IPv4) {
		ip4GatewayAddress, err := gatewayIP(vmiNetwork.Pod.VMNetworkCIDR, api.DefaultVMCIDR)
		if err != nil {
			return nil, err
		}
		bridgeIface.IPv4 = nmstate.IP{
			Enabled: pointer.P(true),
			Address: []nmstate.IPAddress{ip4GatewayAddress},
		}
		bridgeIface.LinuxStack.IP4RouteLocalNet = pointer.P(true)
	}

	if hasIPGlobalUnicast(podIface.IPv6) {
		ip6GatewayAddress, err := gatewayIP(vmiNetwork.Pod.VMIPv6NetworkCIDR, api.DefaultVMIpv6CIDR)
		if err != nil {
			return nil, err
		}
		bridgeIface.IPv6 = nmstate.IP{
			Enabled: pointer.P(true),
			Address: []nmstate.IPAddress{ip6GatewayAddress},
		}
	}

	tapIface := nmstate.Interface{
		Name:       link.GenerateTapDeviceName(podIfaceName, *vmiNetwork),
		TypeName:   nmstate.TypeTap,
		State:      nmstate.IfaceStateUp,
		MTU:        podIface.MTU,
		Controller: bridgeIface.Name,
		Tap: &nmstate.TapDevice{
			Queues: n.networkQueues(vmiIfaceIndex),
			UID:    n.ownerID,
			GID:    n.ownerID,
		},
		Metadata: &nmstate.IfaceMetadata{Pid: n.podPID, NetworkName: vmiNetwork.Name},
	}

	return []nmstate.Interface{bridgeIface, tapIface}, nil
}

const primaryTapName = "tap0"

// tapIsDetached reports whether the TAP's operational state proves no guest is
// attached to it (netlink OperState "down"/"lower-layer-down").
func tapIsDetached(iface nmstate.Interface) bool {
	return iface.State == nmstate.IfaceStateDown || iface.State == "lower-layer-down"
}

// cleanupOrphanedNetworks removes what is left in the pod netns from networks
// that disappeared from the VMI spec: the KubeVirt-owned TAP (named after the
// network for secondary pod networks) and the per-network cache entries. The
// SDN-provisioned pod interface is not touched — its lifecycle belongs to SDN.
// TC filters die with their devices, releasing the BPF program.
//
// A TAP whose operational state is still up has the guest attached: the domain
// hot-unplug has not finished yet, so the device (and its cache entry, which
// keeps the network flagged as orphaned) is left for a later reconcile.
func (n NetPod) cleanupOrphanedNetworks(currentIfaces []nmstate.Interface) error {
	currentIfaceByName := ifaceStatusByName(currentIfaces)

	var absentIfaces []nmstate.Interface
	var cleanableNets []string
	for _, networkName := range n.orphanedNetworks {
		iface, exists := currentIfaceByName[networkName]
		// A network named like the primary TAP would match the default network's
		// device below; its cache entry is still cleanable, the device is not ours
		// to delete here.
		deviceIsLeftoverTap := exists && iface.TypeName == nmstate.TypeTap && networkName != primaryTapName
		if deviceIsLeftoverTap && !tapIsDetached(iface) {
			// Only a provably carrier-less TAP (guest detached by the domain
			// hot-unplug) is removed; "unknown" and other states postpone to a
			// later reconcile rather than risk racing the guest detach.
			n.log.Infof("Postponing leftover TAP removal for unplugged network %s: the guest is still attached", networkName)
			continue
		}
		if deviceIsLeftoverTap {
			absentIfaces = append(absentIfaces, nmstate.Interface{
				Name:     networkName,
				TypeName: nmstate.TypeTap,
				State:    nmstate.IfaceStateAbsent,
				Metadata: &nmstate.IfaceMetadata{NetworkName: networkName},
			})
		}
		cleanableNets = append(cleanableNets, networkName)
	}

	if len(absentIfaces) > 0 {
		n.log.Infof("Removing leftover TAP devices of unplugged networks: %v", cleanableNets)
		if err := n.nmstateAdapter.Apply(&nmstate.Spec{Interfaces: absentIfaces}); err != nil {
			return fmt.Errorf("failed to remove leftover TAP devices: %w", err)
		}
	}

	for _, networkName := range cleanableNets {
		if err := n.state.Delete([]v1.Network{{Name: networkName}}); err != nil {
			return err
		}
		delete(n.state.BPFBridgePodIfaceByNetwork, networkName)
		if err := cache.DeletePodInterfaceCache(n.cacheCreator, n.vmiUID, networkName); err != nil {
			n.log.Reason(err).Warningf("failed to delete pod interface cache for unplugged network %s", networkName)
		}
	}
	return nil
}

// bpfBridgeAbsentSpec marks the bpfbridge TAP of an unplugged interface for
// removal. Only a device that actually exists and is KubeVirt-owned is emitted,
// so a repeated reconcile after the deletion stays a no-op and a foreign device
// that happens to share the name is left alone.
func (n NetPod) bpfBridgeAbsentSpec(vmiIfaceIndex int, currentIfaces []nmstate.Interface) []nmstate.Interface {
	vmiNetworkName := n.vmiSpecIfaces[vmiIfaceIndex].Name
	vmiNetwork := vmispec.LookupNetworkByName(n.vmiSpecNets, vmiNetworkName)
	if vmiNetwork == nil {
		return nil
	}
	// Only secondary pod networks get their TAP named after the network; a multus
	// TAP name derives from the (here unresolved) pod interface name, so it is
	// left alone rather than mis-targeted.
	if vmispec.IsSecondaryMultusNetwork(*vmiNetwork) {
		return nil
	}

	tapName := link.GenerateTapDeviceName(vmiNetworkName, *vmiNetwork)
	for _, currentIface := range currentIfaces {
		if currentIface.Name == tapName && currentIface.TypeName == nmstate.TypeTap {
			return []nmstate.Interface{{
				Name:     tapName,
				TypeName: nmstate.TypeTap,
				State:    nmstate.IfaceStateAbsent,
				Metadata: &nmstate.IfaceMetadata{NetworkName: vmiNetworkName},
			}}
		}
	}
	return nil
}

func (n NetPod) bpfBridgeSpec(podIfaceName string, vmiIfaceIndex int, ifaceStatusByName map[string]nmstate.Interface) ([]nmstate.Interface, error) {
	podIface := ifaceStatusByName[podIfaceName]
	vmiNetworkName := n.vmiSpecIfaces[vmiIfaceIndex].Name
	vmiNetwork := vmispec.LookupNetworkByName(n.vmiSpecNets, vmiNetworkName)
	if vmiNetwork == nil {
		return nil, fmt.Errorf("network %s not found for bpfbridge interface", vmiNetworkName)
	}

	// When external TAP provisioning is enabled (external service provisions TAPs),
	// nmstate must not create/manage the secondary TAP device; skip emitting it in
	// the desired spec. The default pod network TAP is always created natively.
	if n.externalTapProvisioning && vmispec.IsSecondaryPodNetwork(*vmiNetwork) {
		n.log.Infof("bpfbridge setup: skipping native TAP creation for secondary pod interface %q (external provisioning)", podIfaceName)
		return nil, nil
	}

	tapIface := nmstate.Interface{
		Name:     link.GenerateTapDeviceName(podIfaceName, *vmiNetwork),
		TypeName: nmstate.TypeTap,
		State:    nmstate.IfaceStateUp,
		MTU:      podIface.MTU,
		Tap: &nmstate.TapDevice{
			Queues: n.networkQueues(vmiIfaceIndex),
			UID:    n.ownerID,
			GID:    n.ownerID,
		},
		Metadata: &nmstate.IfaceMetadata{Pid: n.podPID, NetworkName: vmiNetworkName},
	}

	return []nmstate.Interface{tapIface}, nil
}

func (n NetPod) managedTapSpec(podIfaceName string, vmiIfaceIndex int, ifaceStatusByName map[string]nmstate.Interface) ([]nmstate.Interface, error) {

	vmiNetworkName := n.vmiSpecIfaces[vmiIfaceIndex].Name
	vmiNetwork := vmispec.LookupNetworkByName(n.vmiSpecNets, vmiNetworkName)

	podIfaceAlternativeName := link.GenerateNewBridgedVmiInterfaceName(podIfaceName)
	podStatusIface, exist := ifaceStatusByName[podIfaceAlternativeName]
	if !exist {
		podStatusIface = ifaceStatusByName[podIfaceName]
	}

	bridgeIface := nmstate.Interface{
		Name:     link.GenerateBridgeName(podIfaceName),
		TypeName: nmstate.TypeBridge,
		State:    nmstate.IfaceStateUp,
		Ethtool:  nmstate.Ethtool{Feature: nmstate.Feature{TxChecksum: pointer.P(false)}},
		Metadata: &nmstate.IfaceMetadata{NetworkName: vmiNetworkName},
	}

	podIface := nmstate.Interface{
		Index:       podStatusIface.Index,
		Name:        podIfaceAlternativeName,
		State:       nmstate.IfaceStateUp,
		CopyMacFrom: bridgeIface.Name,
		Controller:  bridgeIface.Name,
		IPv4:        nmstate.IP{Enabled: pointer.P(false)},
		IPv6:        nmstate.IP{Enabled: pointer.P(false)},
		LinuxStack:  nmstate.LinuxIfaceStack{PortLearning: pointer.P(false)},
		Metadata:    &nmstate.IfaceMetadata{NetworkName: vmiNetworkName},
	}

	tapIface := nmstate.Interface{
		Name:       link.GenerateTapDeviceName(podIfaceName, *vmiNetwork),
		TypeName:   nmstate.TypeTap,
		State:      nmstate.IfaceStateUp,
		MTU:        podStatusIface.MTU,
		Controller: bridgeIface.Name,
		Tap: &nmstate.TapDevice{
			Queues: n.networkQueues(vmiIfaceIndex),
			UID:    n.ownerID,
			GID:    n.ownerID,
		},
		Metadata: &nmstate.IfaceMetadata{Pid: n.podPID, NetworkName: vmiNetworkName},
	}

	dummyIface := nmstate.Interface{
		Name:       podIfaceName,
		TypeName:   nmstate.TypeDummy,
		MacAddress: podStatusIface.MacAddress,
		MTU:        podStatusIface.MTU,
		IPv4:       podStatusIface.IPv4,
		IPv6:       podStatusIface.IPv6,
		Metadata:   &nmstate.IfaceMetadata{NetworkName: vmiNetworkName},
	}

	return []nmstate.Interface{bridgeIface, podIface, tapIface, dummyIface}, nil
}

func (n NetPod) setupBPFBridge(currentStatus *nmstate.Status) error {
	podIfaceNameByVMINetwork := n.createNetworkNameScheme(currentStatus.Interfaces)
	for ifIndex, iface := range n.vmiSpecIfaces {
		if !vmispec.IsBPFBridgeBinding(iface) {
			continue
		}
		// Never attach for an interface that is being unplugged: with its own SDN
		// veth already gone, the resolved name could point at another network's
		// device and Attach would replace that device's ingress filter.
		if iface.State == v1.InterfaceStateAbsent {
			continue
		}

		podIfaceName := podIfaceNameByVMINetwork[iface.Name]
		vmiNetwork := vmispec.LookupNetworkByName(n.vmiSpecNets, iface.Name)
		if vmiNetwork == nil {
			return fmt.Errorf("network %s not found for bpfbridge interface", iface.Name)
		}
		// Persist the resolved pod interface name so Teardown detaches from the exact
		// same device, without re-resolving the name scheme with divergent logic.
		if n.state != nil && n.state.BPFBridgePodIfaceByNetwork != nil {
			n.state.BPFBridgePodIfaceByNetwork[iface.Name] = podIfaceName
		}
		tapName := link.GenerateTapDeviceName(podIfaceName, *vmiNetwork)
		objPath := filepath.Join("/usr", "share", "network-bpf-bridge-binding", "bpf_bridge.o")
		if err := n.bpfBridgeAdapter.EnsureWiring(tapName, podIfaceName); err != nil {
			return fmt.Errorf("bpfbridge wiring failed for iface %s (index %d): %w", iface.Name, ifIndex, err)
		}
		if err := n.bpfBridgeAdapter.Attach(objPath, tapName, podIfaceName); err != nil {
			return fmt.Errorf("bpfbridge attach failed for iface %s (index %d): %w", iface.Name, ifIndex, err)
		}
	}
	return nil
}

// defaultBpfBridgeAdapter is the production implementation of bpfBridgeAdapter;
// it delegates to pkg/network/bpfbridge which talks to netlink + libbpf.
type defaultBpfBridgeAdapter struct{}

func (defaultBpfBridgeAdapter) EnsureWiring(tapName, podIfaceName string) error {
	return bpfbridge.EnsureWiring(tapName, podIfaceName)
}

func (defaultBpfBridgeAdapter) Attach(objPath, tapName, podIfaceName string) error {
	return bpfbridge.Attach(objPath, tapName, podIfaceName)
}

func (n NetPod) setupNAT(desiredSpec *nmstate.Spec, currentStatus *nmstate.Status) error {
	bridgeIfaceSpec := n.lookupMasquradeBridge(desiredSpec.Interfaces)
	if bridgeIfaceSpec == nil {
		return nil
	}
	podIfaceNameByVMINetwork := n.createNetworkNameScheme(currentStatus.Interfaces)
	podIfaceName := podIfaceNameByVMINetwork[bridgeIfaceSpec.Metadata.NetworkName]
	podIfaceSpec := nmstate.LookupInterface(currentStatus.Interfaces, func(i nmstate.Interface) bool {
		return i.Name == podIfaceName
	})
	if podIfaceSpec == nil {
		return fmt.Errorf("setup-nat: pod link (%s) is missing", podIfaceName)
	}
	vmiIface := vmispec.FilterInterfacesSpec(n.vmiSpecIfaces, func(i v1.Interface) bool {
		return i.Name == bridgeIfaceSpec.Metadata.NetworkName
	})
	return n.masqueradeAdapter.Setup(bridgeIfaceSpec, podIfaceSpec, vmiIface[0])
}

func (n NetPod) lookupMasquradeBridge(desiredIfacesSpec []nmstate.Interface) *nmstate.Interface {
	masqueradeIfaces := vmispec.FilterInterfacesSpec(n.vmiSpecIfaces, func(i v1.Interface) bool {
		return i.Masquerade != nil
	})
	if len(masqueradeIfaces) > 0 {
		vmiMasqIface := masqueradeIfaces[0]
		bridgeIfaceSpec := nmstate.LookupInterface(desiredIfacesSpec, func(i nmstate.Interface) bool {
			return i.Metadata != nil && i.Metadata.NetworkName == vmiMasqIface.Name && i.TypeName == nmstate.TypeBridge
		})

		return bridgeIfaceSpec
	}
	return nil
}

func calcQueuesCapByIface(desiredQueueCount int,
	ifaces []v1.Interface,
	ifaceStatuses []v1.VirtualMachineInstanceNetworkInterface) map[string]int {

	hasDomainInfoSource := func(ifaceStatus v1.VirtualMachineInstanceNetworkInterface) bool {
		return vmispec.ContainsInfoSource(ifaceStatus.InfoSource, vmispec.InfoSourceDomain)
	}

	ifaceStatusesInDomainByName := vmispec.IndexInterfaceStatusByName(ifaceStatuses, hasDomainInfoSource)

	queuesCapByIface := map[string]int{}
	for _, iface := range ifaces {
		if iface.SRIOV != nil {
			continue
		}

		ifaceStatus, existsInDomain := ifaceStatusesInDomainByName[iface.Name]
		if existsInDomain {
			queuesCapByIface[iface.Name] = int(ifaceStatus.QueueCount)
		} else {
			queuesCapByIface[iface.Name] = desiredQueueCount
		}
	}

	return queuesCapByIface
}

func ifaceStatusByName(interfaces []nmstate.Interface) map[string]nmstate.Interface {
	ifaceByName := map[string]nmstate.Interface{}
	for _, iface := range interfaces {
		ifaceByName[iface.Name] = iface
	}
	return ifaceByName
}

func gatewayIP(cidr, defaultCIDR string) (nmstate.IPAddress, error) {
	if cidr == "" {
		cidr = defaultCIDR
	}
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nmstate.IPAddress{}, fmt.Errorf("failed to parse VM CIDR: %s, %v", cidr, err)
	}
	const minMaskBitsForHostAddresses = 2
	if prefixLen, maxPrefixLen := ipNet.Mask.Size(); prefixLen > maxPrefixLen-minMaskBitsForHostAddresses {
		return nmstate.IPAddress{}, fmt.Errorf("VM CIDR subnet is too small, at least 2 host addresses are required: %s", cidr)
	}
	netmachinery.NextIP(ipNet.IP)

	gatewayAddress := ipNet.IP.String()
	ipGatewayPrefixLen, _ := ipNet.Mask.Size()

	return nmstate.IPAddress{
		IP:        gatewayAddress,
		PrefixLen: ipGatewayPrefixLen,
	}, nil
}

func hasIP4GlobalUnicast(iface nmstate.Interface) bool {
	return hasIPGlobalUnicast(iface.IPv4)
}

func hasIP6GlobalUnicast(iface nmstate.Interface) bool {
	return hasIPGlobalUnicast(iface.IPv6)
}

func hasIPGlobalUnicast(ip nmstate.IP) bool {
	return firstIPGlobalUnicast(ip) != nil
}

func firstIPGlobalUnicast(ip nmstate.IP) *nmstate.IPAddress {
	if ip.Enabled != nil && *ip.Enabled {
		for _, addr := range ip.Address {
			if net.ParseIP(addr.IP).IsGlobalUnicast() {
				address := addr
				return &address
			}
		}
	}
	return nil
}

func (n NetPod) createNetworkNameScheme(currentIfaces []nmstate.Interface) map[string]string {
	var podIfaceNamesByNetworkName map[string]string

	if includesOrdinalNames(currentIfaces) {
		podIfaceNamesByNetworkName = namescheme.CreateOrdinalNetworkNameScheme(n.vmiSpecNets)
	} else {
		podIfaceNamesByNetworkName = namescheme.CreateHashedNetworkNameScheme(n.vmiSpecNets)
	}

	podIfaceNamesByNetworkName = namescheme.UpdatePrimaryPodIfaceNameFromVMIStatus(podIfaceNamesByNetworkName, n.vmiSpecNets, n.vmiIfaceStatuses)
	return updateNonDefaultPodInterfaceNamesFromCurrentStatus(podIfaceNamesByNetworkName, n.vmiSpecNets, n.vmiIfaceStatuses, currentIfaces)
}

// sdnAltNamePrefix marks the pod-side interface the SDN agent provisioned for a
// VM network. The full altname is "d8-sdn-veth-in-<networkName>-<requestedIfName>",
// where the requested ifName equals the VMI network name and never contains
// dashes, so it is parsed from the last "-veth_" occurrence.
const sdnAltNamePrefix = "d8-sdn-veth-in-"

// sdnPodIfaceNamesFromAltNames derives the authoritative VMI-network -> pod
// interface mapping from the SDN altname contract present on the links in the
// pod netns.
func sdnPodIfaceNamesFromAltNames(currentIfaces []nmstate.Interface) map[string]string {
	podIfaceByNetwork := map[string]string{}
	for _, iface := range currentIfaces {
		for _, altName := range iface.AltNames {
			if !strings.HasPrefix(altName, sdnAltNamePrefix) {
				continue
			}
			idx := strings.LastIndex(altName, "-veth_")
			if idx < 0 {
				continue
			}
			podIfaceByNetwork[altName[idx+1:]] = iface.Name
		}
	}
	return podIfaceByNetwork
}

func updateNonDefaultPodInterfaceNamesFromCurrentStatus(
	podIfaceNamesByNetworkName map[string]string,
	networks []v1.Network,
	ifaceStatuses []v1.VirtualMachineInstanceNetworkInterface,
	currentIfaces []nmstate.Interface,
) map[string]string {
	currentIfaceByName := map[string]nmstate.Interface{}
	for _, iface := range currentIfaces {
		currentIfaceByName[iface.Name] = iface
	}

	primaryPodIfaceName := podIfaceNamesByNetworkName["default"]
	if primaryPodIfaceName == "" {
		primaryPodIfaceName = namescheme.PrimaryPodInterfaceName
	}

	var secondaryPodNetworks []v1.Network
	for _, network := range networks {
		if network.Pod != nil && network.Name != "default" {
			secondaryPodNetworks = append(secondaryPodNetworks, network)
		}
	}
	if len(secondaryPodNetworks) == 0 {
		return podIfaceNamesByNetworkName
	}

	usedCandidateNames := map[string]struct{}{primaryPodIfaceName: {}}
	// The SDN altname contract is authoritative. When ANY link in the netns
	// carries it, the environment is SDN-managed: every secondary pod network is
	// resolved strictly by altname, and a network whose interface is not present
	// yet gets an empty name so the setup fails with a retriable error instead of
	// guessing a device heuristically — with several networks a guess can
	// cross-wire one network's traffic into another.
	sdnPodIfaceNamesByNetwork := sdnPodIfaceNamesFromAltNames(currentIfaces)
	sdnManagedNetns := len(sdnPodIfaceNamesByNetwork) > 0
	sdnResolvedNetworks := map[string]struct{}{}
	for _, network := range secondaryPodNetworks {
		if sdnManagedNetns {
			podIfaceName := sdnPodIfaceNamesByNetwork[network.Name]
			podIfaceNamesByNetworkName[network.Name] = podIfaceName
			if podIfaceName != "" {
				usedCandidateNames[podIfaceName] = struct{}{}
			}
			sdnResolvedNetworks[network.Name] = struct{}{}
			continue
		}
		// A current interface named exactly like the network is only the pod
		// interface when it is not a KubeVirt-owned device. The managed tap is
		// named after the network (see link.GenerateTapDeviceName), so without
		// this guard the tap would be picked as the pod interface, collapsing
		// tapName == podIfaceName and baking POD_IFINDEX == TAP_IFINDEX into the
		// bpfbridge program (which then redirects the tap back onto itself).
		if iface, exists := currentIfaceByName[network.Name]; exists && !isKubeVirtOwnedInterface(iface) && network.Name != primaryPodIfaceName {
			podIfaceNamesByNetworkName[network.Name] = network.Name
			usedCandidateNames[network.Name] = struct{}{}
			continue
		}
		if ifaceStatus := vmispec.LookupInterfaceStatusByName(ifaceStatuses, network.Name); ifaceStatus != nil && ifaceStatus.PodInterfaceName != "" {
			// Two guards on the reported PodInterfaceName:
			//  - it can be the network name (hence the managed tap) — refuse a
			//    KubeVirt-owned device;
			//  - it can be a stale "eth0" written by an older resolution — a
			//    secondary network must never claim the primary pod interface,
			//    otherwise it binds to eth0 (POD_IFINDEX=eth0) instead of its CNI
			//    veth. Reject primaryPodIfaceName so it falls through to the
			//    candidate loop and picks the real CNI veth.
			if iface, exists := currentIfaceByName[ifaceStatus.PodInterfaceName]; exists && !isKubeVirtOwnedInterface(iface) && ifaceStatus.PodInterfaceName != primaryPodIfaceName {
				podIfaceNamesByNetworkName[network.Name] = ifaceStatus.PodInterfaceName
				usedCandidateNames[ifaceStatus.PodInterfaceName] = struct{}{}
			}
		}
	}

	var candidateNames []string
	for _, iface := range currentIfaces {
		if _, used := usedCandidateNames[iface.Name]; used {
			continue
		}
		if isKubeVirtOwnedInterface(iface) || iface.Name == "lo" {
			continue
		}
		candidateNames = append(candidateNames, iface.Name)
	}

	candidateIndex := 0
	for _, network := range secondaryPodNetworks {
		if _, sdnResolved := sdnResolvedNetworks[network.Name]; sdnResolved {
			continue
		}
		if podIfaceName, exists := podIfaceNamesByNetworkName[network.Name]; exists {
			if _, linkExists := currentIfaceByName[podIfaceName]; linkExists {
				continue
			}
		}
		if candidateIndex >= len(candidateNames) {
			continue
		}
		podIfaceNamesByNetworkName[network.Name] = candidateNames[candidateIndex]
		candidateIndex++
	}

	return podIfaceNamesByNetworkName
}

func isKubeVirtOwnedInterface(iface nmstate.Interface) bool {
	return iface.TypeName == nmstate.TypeTap || iface.TypeName == nmstate.TypeBridge || iface.TypeName == nmstate.TypeDummy || strings.HasPrefix(iface.Name, "tap") || strings.HasPrefix(iface.Name, "k6t-")
}

func skipPodInterfaceIsNotDefault(name string, networks []v1.Network) bool {
	if name == "default" {
		return false
	}
	for _, network := range networks {
		if network.Name == name && network.Pod != nil {
			return true
		}
	}
	return false
}

func includesOrdinalNames(ifaces []nmstate.Interface) bool {
	for _, iface := range ifaces {
		if namescheme.OrdinalSecondaryInterfaceName(iface.Name) {
			return true
		}
	}
	return false
}

func filterSupportedBindingNetworks(specNetworks []v1.Network, specInterfaces []v1.Interface) ([]v1.Network, error) {
	var networks []v1.Network
	for _, network := range specNetworks {
		iface := vmispec.LookupInterfaceByName(specInterfaces, network.Name)
		if iface == nil {
			return nil, fmt.Errorf("no iface matching with network %s", network.Name)
		}

		// Macvtap is removed in v1.3. This scenario is tracking old VMIs that are still processed in the reconcile loop.
		if iface.SRIOV != nil || iface.DeprecatedMacvtap != nil {
			continue
		}

		networks = append(networks, network)
	}

	return networks, nil
}

func (n NetPod) unplugInterfaces(startedNets, finishedNets []v1.Network) []v1.Interface {
	nonPendingNetworks := append(startedNets, finishedNets...)
	nonPendingNetsByName := vmispec.IndexNetworkSpecByName(nonPendingNetworks)
	unplugIfaces := vmispec.FilterInterfacesSpec(n.vmiSpecIfaces, func(iface v1.Interface) bool {
		_, netExists := nonPendingNetsByName[iface.Name]
		return iface.State == v1.InterfaceStateAbsent && netExists
	})
	return unplugIfaces
}

func (n NetPod) clearCache(nets []v1.Network) error {
	var unplugErrors []error
	for _, net := range nets {
		err := cache.DeleteDomainInterfaceCache(n.cacheCreator, strconv.Itoa(n.podPID), net.Name)
		if err != nil {
			unplugErrors = append(unplugErrors, err)
		}

		podInterfaceName := namescheme.HashedPodInterfaceName(net, n.vmiIfaceStatuses)
		err = cache.DeleteDHCPInterfaceCache(n.cacheCreator, strconv.Itoa(n.podPID), podInterfaceName)
		if err != nil {
			unplugErrors = append(unplugErrors, err)
		}

		// the PodInterface cache should be the last one to be cleaned.
		// It should be cleaned as the last step of the cleanup, since it is the indicator the cleanup should be done/not over yet.
		if len(unplugErrors) == 0 {
			err = cache.DeletePodInterfaceCache(n.cacheCreator, n.vmiUID, net.Name)
			if err != nil {
				unplugErrors = append(unplugErrors, err)
			}
		}
	}

	if len(unplugErrors) > 0 {
		return k8serrors.NewAggregate(unplugErrors)
	}
	return n.state.Delete(nets)
}
