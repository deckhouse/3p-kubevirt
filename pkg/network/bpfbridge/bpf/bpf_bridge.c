#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <bpf/bpf_helpers.h>

// bridge_cfg maps an ingress ifindex to its peer ifindex in the same pod netns.
// For each VM we program two entries:
//   tap_ifindex -> pod_ifindex
//   pod_ifindex -> tap_ifindex
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 64);
	__type(key, __u32);
	__type(value, __u32);
} bridge_cfg SEC(".maps");

SEC("tc")
int tc_l2_proxy(struct __sk_buff *ctx)
{
	__u32 in_ifindex = ctx->ifindex;
	__u32 *peer_ifindex = bpf_map_lookup_elem(&bridge_cfg, &in_ifindex);
	if (!peer_ifindex || *peer_ifindex == 0)
		return TC_ACT_OK;

	return bpf_redirect(*peer_ifindex, 0);
}

char _license[] SEC("license") = "GPL";
