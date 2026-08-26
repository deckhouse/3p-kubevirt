package vmi

import (
	"fmt"

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
)

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

	available, message, err := c.hasMigrationTargetNode(vmi)
	if err != nil {
		return err
	}

	condition := virtv1.VirtualMachineInstanceCondition{
		Type:               virtv1.VirtualMachineInstanceMigrationTargetAvailable,
		Status:             k8sv1.ConditionTrue,
		Reason:             virtv1.VirtualMachineInstanceReasonMigrationTargetAvailable,
		Message:            migrationTargetAvailableMessage,
		LastTransitionTime: v1.Now(),
	}
	if !available {
		condition.Status = k8sv1.ConditionFalse
		condition.Reason = virtv1.VirtualMachineInstanceReasonNoMigrationTarget
		condition.Message = message
	}
	controller.NewVirtualMachineInstanceConditionManager().UpdateCondition(vmi, &condition)
	return nil
}

// hasMigrationTargetNode reports whether the cluster has a node the VirtualMachineInstance can be
// migrated to and the message describing why it has not, if it has not.
//
// Only the placement rules are taken into account: the node selector, the node affinity, the taints
// of the nodes and the pod affinity rules. The free resources of the nodes are not evaluated, it is
// up to the scheduler once the migration target pod is created.
func (c *Controller) hasMigrationTargetNode(vmi *virtv1.VirtualMachineInstance) (bool, string, error) {
	templatePod, err := c.templateService.RenderLaunchManifest(vmi)
	if err != nil {
		return false, "", fmt.Errorf("failed to render pod manifest: %w", err)
	}
	// The pod affinity terms of the template pod are resolved against the namespace of the pod.
	templatePod.Namespace = vmi.Namespace

	// The pods of a node have to be listed only when the VirtualMachine defines its own pod affinity
	// rules, the rule of the migration target pod is already covered by skipping the node the
	// VirtualMachineInstance runs on.
	hasPodAffinityRules := len(affinity.GetPodAffinityTerms(templatePod.Spec.Affinity))+
		len(affinity.GetPodAntiAffinityTerms(templatePod.Spec.Affinity)) > 0

	nodeMatchedFound := false

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
		if node.DeletionTimestamp != nil || node.Spec.Unschedulable {
			continue
		}
		if !affinity.ToleratesNodeTaints(node, templatePod) {
			continue
		}

		matched, err := nodeAffinityIsMatched(node, templatePod)
		if err != nil {
			return false, "", err
		}
		if !matched {
			continue
		}
		nodeMatchedFound = true

		if !hasPodAffinityRules {
			return true, "", nil
		}

		matched, err = c.podAffinityIsMatched(node.Name, templatePod)
		if err != nil {
			return false, "", err
		}
		if matched {
			return true, "", nil
		}
	}

	if nodeMatchedFound {
		return false, noNodeMatchesPodAffinityMessage, nil
	}
	return false, noNodeMatchesPlacementMessage, nil
}
