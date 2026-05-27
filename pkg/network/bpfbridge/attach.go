package bpfbridge

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	DefaultPodIface = "eth0"
)

// EnsureWiring validates and normalizes TAP wiring in the current netns, mirroring
// the old sidecar runtime behavior: lookup pod iface, ensure tap exists, align MTU,
// and set TAP link up before BPF attachment.
func EnsureWiring(tapName, podIface string) error {
	if podIface == "" {
		podIface = DefaultPodIface
	}

	pod, err := netlink.LinkByName(podIface)
	if err != nil {
		return fmt.Errorf("lookup pod interface %q: %w", podIface, err)
	}

	tap, err := netlink.LinkByName(tapName)
	if err != nil {
		return fmt.Errorf("lookup tap %q: %w", tapName, err)
	}

	if podMTU := pod.Attrs().MTU; podMTU > 0 && tap.Attrs().MTU != podMTU {
		if err := netlink.LinkSetMTU(tap, podMTU); err != nil {
			return fmt.Errorf("set %q mtu %d: %w", tapName, podMTU, err)
		}
	}

	if err := netlink.LinkSetUp(tap); err != nil {
		return fmt.Errorf("set %q up: %w", tapName, err)
	}

	return nil
}

// Attach loads bpf_bridge.o, writes tap/pod ifindexes into bridge_cfg,
// and attaches the TC program on both interfaces in the current network namespace.
func Attach(objPath, tapName, podName string) error {
	tap, err := netlink.LinkByName(tapName)
	if err != nil {
		return fmt.Errorf("lookup tap %s: %w", tapName, err)
	}
	pod, err := netlink.LinkByName(podName)
	if err != nil {
		return fmt.Errorf("lookup pod iface %s: %w", podName, err)
	}

	spec, err := ebpf.LoadCollectionSpec(objPath)
	if err != nil {
		return fmt.Errorf("load BPF spec: %w", err)
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("load BPF collection: %w", err)
	}
	defer coll.Close()

	cfgMap, ok := coll.Maps["bridge_cfg"]
	if !ok {
		return fmt.Errorf("BPF object missing bridge_cfg map")
	}
	prog, ok := coll.Programs["tc_l2_proxy"]
	if !ok {
		return fmt.Errorf("BPF object missing tc_l2_proxy program")
	}

	tapKey := uint32(tap.Attrs().Index)
	podKey := uint32(pod.Attrs().Index)
	podVal := uint32(pod.Attrs().Index)
	tapVal := uint32(tap.Attrs().Index)
	if err := cfgMap.Update(tapKey, podVal, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update bridge_cfg tap->pod: %w", err)
	}
	if err := cfgMap.Update(podKey, tapVal, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update bridge_cfg pod->tap: %w", err)
	}

	for _, dev := range []string{tapName, podName} {
		if err := ensurePromisc(dev); err != nil {
			return err
		}
		if err := ensureClsact(dev); err != nil {
			return err
		}
		if err := replaceIngressBPF(dev, prog); err != nil {
			return err
		}
	}
	return nil
}

// ensurePromisc puts the device into promiscuous mode.
// The call is idempotent: we skip it when the kernel reports the device is already in
// promiscuous mode, so we do not bump the in-kernel IFF_PROMISC reference count on every
// reconcile.
func ensurePromisc(dev string) error {
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return fmt.Errorf("lookup link %s: %w", dev, err)
	}
	if link.Attrs().Promisc != 0 {
		return nil
	}
	if err := netlink.SetPromiscOn(link); err != nil {
		return fmt.Errorf("set %q promisc on: %w", dev, err)
	}
	return nil
}

func ensureClsact(dev string) error {
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return fmt.Errorf("lookup link %s: %w", dev, err)
	}
	attrs := netlink.QdiscAttrs{
		LinkIndex: link.Attrs().Index,
		Handle:    netlink.MakeHandle(0xffff, 0),
		Parent:    netlink.HANDLE_CLSACT,
	}
	qdisc := &netlink.Clsact{QdiscAttrs: attrs}
	if err := netlink.QdiscReplace(qdisc); err != nil {
		return fmt.Errorf("qdisc replace dev %s clsact: %w", dev, err)
	}
	return nil
}

func replaceIngressBPF(dev string, prog *ebpf.Program) error {
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return fmt.Errorf("lookup link %s: %w", dev, err)
	}

	_ = netlink.FilterDel(&netlink.BpfFilter{FilterAttrs: netlink.FilterAttrs{LinkIndex: link.Attrs().Index, Parent: netlink.HANDLE_MIN_INGRESS}})

	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Handle:    1,
			Priority:  1,
			Protocol:  unix.ETH_P_ALL,
		},
		Fd:           prog.FD(),
		Name:         "tc_l2_proxy",
		DirectAction: true,
	}
	if err := netlink.FilterReplace(filter); err != nil {
		return fmt.Errorf("attach bpf filter dev %s ingress: %w", dev, err)
	}
	return nil
}
