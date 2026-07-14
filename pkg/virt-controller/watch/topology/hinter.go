package topology

//go:generate mockgen -source $GOFILE -package=$GOPACKAGE -destination=generated_mock_$GOFILE

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"kubevirt.io/kubevirt/pkg/pointer"

	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"

	k6tv1 "kubevirt.io/api/core/v1"
)

type Hinter interface {
	TopologyHintsForVMI(vmi *k6tv1.VirtualMachineInstance) (hints *k6tv1.TopologyHints, requirement TscFrequencyRequirementType, err error)
	IsTscFrequencyRequired(vmi *k6tv1.VirtualMachineInstance) bool
	TSCFrequenciesInUse() []int64
}

// PodNodeSelectorsFunc returns the node selectors the virt-launcher pod of the
// given VMI will be scheduled with (CPU model/features, hyperv, machine type,
// VMI and cluster-wide node selectors).
type PodNodeSelectorsFunc func(vmi *k6tv1.VirtualMachineInstance) map[string]string

type topologyHinter struct {
	clusterConfig        *virtconfig.ClusterConfig
	nodeStore            cache.Store
	vmiStore             cache.Store
	podNodeSelectorsFunc PodNodeSelectorsFunc
}

func (t *topologyHinter) IsTscFrequencyRequired(vmi *k6tv1.VirtualMachineInstance) bool {
	return vmi.Spec.Architecture == "amd64" && GetTscFrequencyRequirement(vmi).Type != NotRequired
}

func (t *topologyHinter) TopologyHintsForVMI(vmi *k6tv1.VirtualMachineInstance) (hints *k6tv1.TopologyHints, requirement TscFrequencyRequirementType, err error) {
	requirement = GetTscFrequencyRequirement(vmi).Type
	if requirement == NotRequired || vmi.Spec.Architecture != "amd64" {
		return
	}

	// The tsc-frequency scheduling label is intersected with the rest of the
	// pod node selectors, so the frequency must come from nodes the VMI can
	// actually be scheduled to: a frequency taken from a foreign node (e.g. a
	// node filtered out by the VirtualMachineClass CPU feature selectors)
	// would make the virt-launcher pod unschedulable.
	candidateNodes := t.candidateNodesForVMI(vmi)

	freq, err := t.lowestTSCFrequency(candidateNodes)
	if err != nil {
		return nil, requirement, fmt.Errorf("failed to determine the lowest tsc frequency for vmi %s/%s: %v", vmi.Namespace, vmi.Name, err)
	}
	if freq == 0 {
		return nil, requirement, fmt.Errorf("no schedulable node matching the node placement of vmi %s/%s exposes an invariant tsc frequency", vmi.Namespace, vmi.Name)
	}

	stableFreq := pickStableBaselineTSCFrequency(freq, t.TSCFrequenciesInUse(), TSCFrequenciesFromNodes(candidateNodes))
	hints = &k6tv1.TopologyHints{TSCFrequency: pointer.P(stableFreq)}
	return
}

// candidateNodesForVMI returns schedulable nodes exposing an invariant TSC
// frequency that also satisfy the VMI's scheduling constraints: the node
// selectors of the future virt-launcher pod and the required node affinity.
func (t *topologyHinter) candidateNodesForVMI(vmi *k6tv1.VirtualMachineInstance) []*v1.Node {
	nodeSelectors := vmi.Spec.NodeSelector
	if t.podNodeSelectorsFunc != nil {
		nodeSelectors = t.podNodeSelectorsFunc(vmi)
	}

	return FilterNodesFromCache(t.nodeStore.List(),
		HasInvTSCFrequency,
		IsSchedulable,
		MatchesLabels(nodeSelectors),
		MatchesRequiredNodeAffinity(vmi),
	)
}

// lowestTSCFrequency returns the configured minimum cluster TSC frequency if
// set, otherwise the lowest frequency among the given nodes.
func (t *topologyHinter) lowestTSCFrequency(nodes []*v1.Node) (int64, error) {
	if t.clusterConfig != nil {
		if configTSCFrequency := t.clusterConfig.GetMinimumClusterTSCFrequency(); configTSCFrequency != nil {
			if *configTSCFrequency > 0 {
				return *configTSCFrequency, nil
			}
			return 0, fmt.Errorf("the configured minimumClusterTSCFrequency must be greater 0, but got %d", *configTSCFrequency)
		}
	}
	return LowestTSCFrequency(nodes), nil
}

func pickStableBaselineTSCFrequency(clusterMin int64, frequenciesInUse []int64, frequenciesOnNodes []int64) int64 {
	var selected int64

	// First, try to pick minimal frequency that already in use by VMIs
	// and compatible with cluster wide minimal frequency.
	for _, freq := range frequenciesInUse {
		if !IsTSCFrequencyCompatible(clusterMin, false, freq) {
			continue
		}
		if selected == 0 || freq < selected {
			selected = freq
		}
	}
	if selected > 0 {
		return selected
	}

	// Next, get frequencies from all nodes and count that compatible with cluster wide minimal frequency.
	compatibleCounts := map[int64]int{}
	for _, freq := range frequenciesOnNodes {
		if !IsTSCFrequencyCompatible(clusterMin, false, freq) {
			continue
		}
		compatibleCounts[freq]++
	}

	// Try to pick frequency that present on at least 2 nodes.
	// This is a nice trick to overcome frequency drifting noise
	// and increase chances for VM to be able to live migrate in the future.
	for freq, count := range compatibleCounts {
		if count < 2 {
			continue
		}
		if selected == 0 || freq < selected {
			selected = freq
		}
	}
	if selected > 0 {
		return selected
	}

	// Fallback to cluster wide minimal if more stable baseline frequency is not found.
	return clusterMin
}

func (t *topologyHinter) TSCFrequenciesInUse() []int64 {
	frequencyMap := map[int64]struct{}{}
	for _, obj := range t.vmiStore.List() {
		vmi := obj.(*k6tv1.VirtualMachineInstance)
		if AreTSCFrequencyTopologyHintsDefined(vmi) {
			frequencyMap[*vmi.Status.TopologyHints.TSCFrequency] = struct{}{}
		}
	}
	frequencies := []int64{}
	for freq := range frequencyMap {
		frequencies = append(frequencies, freq)
	}
	return frequencies
}

func NewTopologyHinter(nodeStore cache.Store, vmiStore cache.Store, clusterConfig *virtconfig.ClusterConfig, podNodeSelectorsFunc PodNodeSelectorsFunc) *topologyHinter {
	return &topologyHinter{nodeStore: nodeStore, vmiStore: vmiStore, clusterConfig: clusterConfig, podNodeSelectorsFunc: podNodeSelectorsFunc}
}
