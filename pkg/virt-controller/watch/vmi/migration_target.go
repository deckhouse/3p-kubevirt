package vmi

import (
	"fmt"
	"strings"

	k8sv1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	virtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/util/affinity"
	"kubevirt.io/kubevirt/pkg/util/migrations"
)

const (
	migrationTargetAvailableMessage = "The cluster has a node the VirtualMachine can be migrated to"
	noNodeMatchesPlacementMessage   = "No other node matches the node placement rules of the VirtualMachine"
	noNodeMatchesPodAffinityMessage = "All the nodes matching the node placement rules of the VirtualMachine are rejected by its pod affinity rules"
	noNodeAvailableMessage          = "The nodes matching the node placement rules of the VirtualMachine are not available at the moment: they are excluded from scheduling or do not run the virtualization"
)

// The taints Kubernetes puts on a node on its own describe the state of the node at this moment —
// not ready, unreachable, cordoned, under pressure — and not the placement rules of the
// VirtualMachine. Both namespaces are reserved by Kubernetes for exactly that.
var nodeStateTaintPrefixes = []string{"node.kubernetes.io/", "node.cloudprovider.kubernetes.io/"}

// syncMigrationTargetAvailableCondition updates the MigrationTargetAvailable condition of the
// VirtualMachineInstance.
//
// The condition is not updated while the VirtualMachineInstance is migrating: the nodes it occupies
// are not migration targets, so the calculated result would be misleading for the migration which
// is already in progress.
func (c *Controller) syncMigrationTargetAvailableCondition(vmi *virtv1.VirtualMachineInstance) error {
	if migrations.IsMigrating(vmi) {
		return nil
	}

	reason, message, err := c.findMigrationTarget(vmi)
	if err != nil {
		return err
	}

	condition := virtv1.VirtualMachineInstanceCondition{
		Type:               virtv1.VirtualMachineInstanceMigrationTargetAvailable,
		Status:             k8sv1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: v1.Now(),
	}
	if reason == virtv1.VirtualMachineInstanceReasonMigrationTargetAvailable {
		condition.Status = k8sv1.ConditionTrue
	}
	controller.NewVirtualMachineInstanceConditionManager().UpdateCondition(vmi, &condition)
	return nil
}

// findMigrationTarget looks for a node the VirtualMachineInstance can be migrated to and returns
// the reason and the message describing the outcome.
//
// Two negative outcomes are told apart: no node of the cluster fits the VirtualMachine at all, and
// the nodes that fit it are not available right now. The first one is a property of the
// VirtualMachine and holds until its placement rules or the cluster change; the second one is the
// state of the cluster around a cordon, a reboot or a maintenance, and clears up on its own.
//
// Only the placement rules are taken into account: the node selector, the node affinity, the taints
// of the nodes and the pod affinity rules. The free resources of the nodes are not evaluated, it is
// up to the scheduler once the migration target pod is created.
func (c *Controller) findMigrationTarget(vmi *virtv1.VirtualMachineInstance) (string, string, error) {
	templatePod, err := c.templateService.RenderLaunchManifest(vmi)
	if err != nil {
		return "", "", fmt.Errorf("failed to render pod manifest: %w", err)
	}
	// The pod affinity terms of the template pod are resolved against the namespace of the pod.
	templatePod.Namespace = vmi.Namespace
	// kubevirt.io/schedulable is managed by virt-controller and tells whether the node runs a
	// responsive virt-handler at the moment, so it belongs to the availability of the node rather
	// than to the placement rules of the VirtualMachine.
	delete(templatePod.Spec.NodeSelector, virtv1.NodeSchedulable)

	// The pods of a node have to be listed only when the VirtualMachine defines its own pod affinity
	// rules, the rule of the migration target pod is already covered by skipping the node the
	// VirtualMachineInstance runs on.
	hasPodAffinityRules := len(affinity.GetPodAffinityTerms(templatePod.Spec.Affinity))+
		len(affinity.GetPodAntiAffinityTerms(templatePod.Spec.Affinity)) > 0

	nodeMatchedFound := false
	fittingNodeFound := false

	for _, obj := range c.nodeIndexer.List() {
		node, ok := obj.(*k8sv1.Node)
		if !ok {
			continue
		}
		// The node the VirtualMachineInstance runs on is not a migration target: the migration
		// controller renders the target pod with the pod anti-affinity rule against the pods of the
		// VirtualMachineInstance. The rest of Status.ActivePods is deliberately left out: around a
		// migration that map holds both its source and its target, and skipping their nodes reports
		// a cluster of two suitable nodes as having nowhere to migrate to.
		if node.Name == vmi.Status.NodeName {
			continue
		}
		if !affinity.ToleratesTaints(placementTaintsOf(node), templatePod) {
			continue
		}

		matched, err := nodeAffinityIsMatched(node, templatePod)
		if err != nil {
			return "", "", err
		}
		if !matched {
			continue
		}
		nodeMatchedFound = true

		if hasPodAffinityRules {
			matched, err = c.podAffinityIsMatched(node.Name, templatePod)
			if err != nil {
				return "", "", err
			}
			if !matched {
				continue
			}
		}
		fittingNodeFound = true

		if nodeIsAvailable(node, templatePod) {
			return virtv1.VirtualMachineInstanceReasonMigrationTargetAvailable, migrationTargetAvailableMessage, nil
		}
	}

	switch {
	case fittingNodeFound:
		return virtv1.VirtualMachineInstanceReasonMigrationTargetUnavailable, noNodeAvailableMessage, nil
	case nodeMatchedFound:
		return virtv1.VirtualMachineInstanceReasonNoMigrationTarget, noNodeMatchesPodAffinityMessage, nil
	default:
		return virtv1.VirtualMachineInstanceReasonNoMigrationTarget, noNodeMatchesPlacementMessage, nil
	}
}

// nodeIsAvailable reports whether a node that fits the VirtualMachine can take a pod right now.
// Everything checked here is a transient state of the node: it is going away, it is excluded from
// scheduling, its virt-handler stopped reporting, or Kubernetes taints it by its own condition.
func nodeIsAvailable(node *k8sv1.Node, templatePod *k8sv1.Pod) bool {
	if node.DeletionTimestamp != nil || node.Spec.Unschedulable {
		return false
	}
	if node.Labels[virtv1.NodeSchedulable] != "true" {
		return false
	}
	return affinity.ToleratesTaints(stateTaintsOf(node), templatePod)
}

func placementTaintsOf(node *k8sv1.Node) []k8sv1.Taint {
	return filterTaints(node.Spec.Taints, false)
}

func stateTaintsOf(node *k8sv1.Node) []k8sv1.Taint {
	return filterTaints(node.Spec.Taints, true)
}

func filterTaints(taints []k8sv1.Taint, stateOnly bool) []k8sv1.Taint {
	filtered := make([]k8sv1.Taint, 0, len(taints))
	for _, taint := range taints {
		if isNodeStateTaint(taint) == stateOnly {
			filtered = append(filtered, taint)
		}
	}
	return filtered
}

func isNodeStateTaint(taint k8sv1.Taint) bool {
	for _, prefix := range nodeStateTaintPrefixes {
		if strings.HasPrefix(taint.Key, prefix) {
			return true
		}
	}
	return false
}
