package netsetup

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

const (
	DefaultTapName  = "kvbpf0"
	DefaultPodIface = "eth0"
)

// EnsureWiring creates a persistent TAP in the current (pod) network namespace and
// returns the ifindexes of both the TAP and the pod-side interface (e.g. eth0)
// to be wired together by the BPF L2 proxy program.
//
// The caller is expected to attach the BPF program to TC ingress on both devices
// in the same netns; no veth pair and no host-side wiring is created here.
func EnsureWiring(tapName, podIface string) (tapIdx, podIdx int, err error) {
	if tapName == "" {
		tapName = DefaultTapName
	}
	if podIface == "" {
		podIface = DefaultPodIface
	}

	pod, err := netlink.LinkByName(podIface)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup pod interface %q: %w", podIface, err)
	}

	if err := ensureTAP(tapName); err != nil {
		return 0, 0, err
	}
	tap, err := netlink.LinkByName(tapName)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup tap %q: %w", tapName, err)
	}

	// Match TAP MTU to the pod-side interface so frames are not truncated when
	// redirected between them by the BPF program.
	if podMTU := pod.Attrs().MTU; podMTU > 0 && tap.Attrs().MTU != podMTU {
		if err := netlink.LinkSetMTU(tap, podMTU); err != nil {
			return 0, 0, fmt.Errorf("set %q mtu %d: %w", tapName, podMTU, err)
		}
	}

	if err := netlink.LinkSetUp(tap); err != nil {
		return 0, 0, fmt.Errorf("set %q up: %w", tapName, err)
	}

	return tap.Attrs().Index, pod.Attrs().Index, nil
}

func ensureTAP(name string) error {
	if _, err := netlink.LinkByName(name); err == nil {
		return nil
	}
	la := netlink.NewLinkAttrs()
	la.Name = name
	tap := &netlink.Tuntap{
		Mode:      netlink.TUNTAP_MODE_TAP,
		Flags:     netlink.TUNTAP_DEFAULTS,
		LinkAttrs: la,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("add tap %q: %w", name, err)
	}
	return nil
}
