package bpfbridge

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	k8serrors "k8s.io/apimachinery/pkg/util/errors"
)

const (
	DefaultPodIface = "eth0"
	programName     = "tc_l2_proxy"
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

// Attach loads bpf_bridge.o, patches the tap/pod ifindexes directly into the program
// via .rodata rewriting (no BPF map involved) and attaches the resulting TC program
// on both interfaces in the current network namespace.
//
// The configuration values land in the program's .rodata section through the
// "volatile const" symbols declared in bpf_bridge.c. Because each Attach materialises
// a fresh CollectionSpec from disk and rewrites its private copy of .rodata before
// the program reaches the kernel, two VMIs on the same node get two independent
// programs with their own baked-in ifindexes; there is no shared map to corrupt.
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

	// Rewrite .rodata-backed TAP_IFINDEX / POD_IFINDEX BEFORE the collection is
	// uploaded to the kernel: the variable specs resolve the named symbols through
	// BTF and patch the raw bytes of the backing map's initial contents. From the
	// verifier's point of view the values then look like immediates, so the unused
	// redirect branch is dead-code eliminated and the fast path is two cmp+jmp
	// instructions plus the redirect.
	tapIdx := uint32(tap.Attrs().Index)
	podIdx := uint32(pod.Attrs().Index)
	if err := setConstant(spec, "TAP_IFINDEX", tapIdx); err != nil {
		return err
	}
	if err := setConstant(spec, "POD_IFINDEX", podIdx); err != nil {
		return err
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("load BPF collection: %w", err)
	}
	defer coll.Close()

	prog, ok := coll.Programs[programName]
	if !ok {
		return fmt.Errorf("BPF object missing %s program", programName)
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

func setConstant(spec *ebpf.CollectionSpec, name string, value uint32) error {
	v, ok := spec.Variables[name]
	if !ok {
		return fmt.Errorf("BPF object missing %s constant", name)
	}
	if !v.Constant() {
		return fmt.Errorf("BPF variable %s is not a constant", name)
	}
	if err := v.Set(value); err != nil {
		return fmt.Errorf("rewrite BPF constant %s=%d: %w", name, value, err)
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
		Name:         programName,
		DirectAction: true,
	}
	if err := netlink.FilterReplace(filter); err != nil {
		return fmt.Errorf("attach bpf filter dev %s ingress: %w", dev, err)
	}
	return nil
}

// Detach removes the TC ingress BPF filter and the clsact qdisc that Attach installed
// on each of the given devices. It MUST be invoked inside the same network namespace
// where Attach ran (i.e. the pod-netns); calling it from host netns will silently miss
// the devices because the names "eth0"/"tap0" either do not exist there or refer to
// unrelated devices.
//
// Detach is best-effort. Errors are aggregated per device into a single returned error
// via k8serrors.NewAggregate so that one broken device does not abort cleanup for the
// rest. "Ignorable" races against a pod that is already being torn down (LinkNotFound,
// ENOENT, ENODEV, ESRCH on the netlink calls) are silently dropped — they simply mean
// the kernel has already reclaimed what we were going to delete.
//
// What is NOT undone here:
//
//   - The promiscuous flag we set in ensurePromisc — IFF_PROMISC is kernel ref-counted
//     and we never recorded whether we were the ones who flipped it on, so calling
//     SetPromiscOff might decrement someone else's reference or, worse, turn off
//     promisc someone else was relying on. The pod-netns dies with the pod and the
//     kernel handles this for us.
//   - The BPF program object itself. We do not hold an FD here; the TC filter held the
//     kernel-side reference and FilterDel above drops it. With .rodata-based
//     configuration there is no pinned map or program left to unpin.
func Detach(devices ...string) error {
	var errs []error

	for _, dev := range devices {
		if dev == "" {
			continue
		}

		link, err := netlink.LinkByName(dev)
		if err != nil {
			if isIgnorableDetachError(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("lookup link %s: %w", dev, err))
			continue
		}

		if err := netlink.FilterDel(&netlink.BpfFilter{FilterAttrs: netlink.FilterAttrs{LinkIndex: link.Attrs().Index, Parent: netlink.HANDLE_MIN_INGRESS}}); err != nil && !isIgnorableDetachError(err) {
			errs = append(errs, fmt.Errorf("delete ingress filter dev %s: %w", dev, err))
		}
		if err := netlink.QdiscDel(&netlink.Clsact{QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: netlink.MakeHandle(0xffff, 0), Parent: netlink.HANDLE_CLSACT}}); err != nil && !isIgnorableDetachError(err) {
			errs = append(errs, fmt.Errorf("delete clsact qdisc dev %s: %w", dev, err))
		}
	}

	return k8serrors.NewAggregate(errs)
}

// isIgnorableDetachError classifies "the thing is already gone" netlink failures so
// Detach can swallow them. ENOENT/ENODEV/ESRCH cover the race where the pod's netns
// has started tearing down beneath us; LinkNotFoundError is the netlink-Go-typed
// flavour of the same condition surfaced by LinkByName.
func isIgnorableDetachError(err error) bool {
	var linkNotFoundErr netlink.LinkNotFoundError
	if errors.As(err, &linkNotFoundErr) {
		return true
	}
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ESRCH)
}
