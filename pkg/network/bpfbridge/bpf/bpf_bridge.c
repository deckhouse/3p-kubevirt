// SPDX-License-Identifier: GPL-2.0 OR MIT
#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <bpf/bpf_helpers.h>

// Per-attachment ifindexes, patched at load time by the Go loader via
// ebpf.CollectionSpec.RewriteConstants() before the program is handed to the
// kernel. The "volatile const" qualifiers are load-bearing:
//
//   - const    -> the symbol lands in the .rodata section, which is the
//                 read-only data map that libbpf-style loaders patch;
//   - volatile -> clang is forbidden from inlining the initialiser (0) into a
//                 `mov imm, X` instruction. Every read must remain a load from
//                 .rodata, otherwise our post-compile patch would have no
//                 effect (clang would have hard-coded 0 into the bytecode).
//
// Once .rodata is patched, the BPF verifier sees the values as immediates and
// can dead-code-eliminate the unused branches.
//
// This replaces the previous BPF_MAP_TYPE_HASH bridge_cfg lookup, which cost a
// helper call on every packet. With the ifindexes baked into the program,
// the fast path is now two compare-and-redirect instructions.
volatile const __u32 TAP_IFINDEX = 0;
volatile const __u32 POD_IFINDEX = 0;

SEC("tc")
int tc_l2_proxy(struct __sk_buff *ctx)
{
	if (TAP_IFINDEX == 0 || POD_IFINDEX == 0)
		return TC_ACT_OK;

	int in_ifindex = ctx->ifindex;

	// VM -> network: use an egress redirect (flags=0), NOT bpf_redirect_peer.
	// Egress redirect makes the frame actually leave eth0 (veth_xmit -> lxc rx),
	// which is exactly what a normal pod / Linux bridge does, so Cilium's
	// from-container path treats it as genuine container egress and rewrites the
	// L2 dst MAC on same-node delivery. bpf_redirect_peer skips eth0 egress and
	// jumps straight into lxc ingress, which makes Cilium omit that dmac rewrite
	// and the destination pod drops the frame as PACKET_OTHERHOST (SKB_DROP_REASON_OTHERHOST).
	if (in_ifindex == TAP_IFINDEX)
		return bpf_redirect(POD_IFINDEX, 0);

	if (in_ifindex == POD_IFINDEX)
		return bpf_redirect(TAP_IFINDEX, 0);

	return TC_ACT_OK;
}

char _license[] SEC("license") = "GPL";
