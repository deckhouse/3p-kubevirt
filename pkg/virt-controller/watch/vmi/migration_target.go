package vmi

import (
	"encoding/json"
	"strings"

	k8sv1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	virtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/util/affinity"
	"kubevirt.io/kubevirt/pkg/util/migrations"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
)

const (
	migrationTargetAvailableMessage = "The cluster has a node the VirtualMachine can be migrated to"
	noNodeMatchesPlacementMessage   = "No other node matches the node placement rules of the VirtualMachine"
	noNodeMatchesPodAffinityMessage = "All the nodes matching the node placement rules of the VirtualMachine are rejected by its pod affinity rules"
	noNodeAvailableMessage          = "The nodes matching the node placement rules of the VirtualMachine are not available at the moment: they are excluded from scheduling or do not run the virtualization"

	// migrationNodeAffinityTermsAnn is set on the VirtualMachine by the virtualization controller.
	// It holds, as a JSON array of NodeSelectorTerm, the required node affinity terms that keep
	// holding once the VirtualMachineInstance has been migrated. See
	// vmiWithMigrationNodeAffinityTerms.
	migrationNodeAffinityTermsAnn = "virtualization.deckhouse.io/migration-node-affinity-terms"
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
	// Only the placement of the pod is rendered: the containers, the volumes and the resources of the
	// whole manifest have no say in which node can take it, and this runs on every reconcile of every
	// running VirtualMachineInstance.
	//
	// The placement is rendered from a VirtualMachineInstance whose node affinity holds the terms
	// published by the virtualization controller, so that the requirements the render adds on its own
	// keep being applied on top of them, the way they are for the pod of a real migration.
	templatePod := services.RenderPodPlacement(c.clusterConfig, c.vmiWithMigrationNodeAffinityTerms(vmi))
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

// vmiWithMigrationNodeAffinityTerms returns the VirtualMachineInstance the placement of a migration
// target is rendered from: the one given, or a copy of it whose required node affinity terms are
// the ones the VirtualMachine carries, for a VirtualMachineInstance whose only obstacle to a live
// migration is its volumes.
//
// The node affinity of such a VirtualMachineInstance carries the node its volumes live on, so that
// the Pod lands where the data is. A migration of the storage lifts that pin by moving the volumes
// along, so the pin says nothing about where the machine may go, and reading it as a rule of the
// machine reports every machine with a local volume as having nowhere to migrate to.
//
// The pin cannot be told from a rule of the machine here: the two are merged into one set of terms
// by the time they reach the VirtualMachineInstance, and this controller has no PersistentVolume
// store to derive the pin from. The virtualization controller has both, and publishes the terms
// that survive a migration in an annotation of the VirtualMachine.
//
// The terms are replaced before the render rather than in the rendered Pod: the render adds
// requirements of its own to every term, the obsolete host model and the forbidden CPU features
// among them, and those hold after a migration as much as before it.
func (c *Controller) vmiWithMigrationNodeAffinityTerms(vmi *virtv1.VirtualMachineInstance) *virtv1.VirtualMachineInstance {
	terms, found := c.migrationNodeAffinityTerms(vmi)
	if !found {
		return vmi
	}

	// The VirtualMachineInstance belongs to the informer store, so the terms are replaced in a copy
	// of it, and its affinity is copied instead of being modified in place.
	vmiCopy := *vmi
	if vmi.Spec.Affinity == nil {
		vmiCopy.Spec.Affinity = &k8sv1.Affinity{}
	} else {
		vmiCopy.Spec.Affinity = vmi.Spec.Affinity.DeepCopy()
	}
	if vmiCopy.Spec.Affinity.NodeAffinity == nil {
		vmiCopy.Spec.Affinity.NodeAffinity = &k8sv1.NodeAffinity{}
	}
	if len(terms) == 0 {
		// The machine has no node affinity rules of its own.
		vmiCopy.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
	} else {
		vmiCopy.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &k8sv1.NodeSelector{
			NodeSelectorTerms: terms,
		}
	}
	return &vmiCopy
}

// migrationNodeAffinityTerms returns the required node affinity terms the VirtualMachine publishes
// for a migration of the VirtualMachineInstance, and whether they were found at all.
//
// Anything that cannot be read leaves the answer as it is: a VirtualMachineInstance whose volumes
// are not the only obstacle to a migration, a missing VirtualMachine, a missing annotation, a value
// that does not parse. Only an annotation that is there and holds terms replaces them, an empty
// array included.
func (c *Controller) migrationNodeAffinityTerms(vmi *virtv1.VirtualMachineInstance) ([]k8sv1.NodeSelectorTerm, bool) {
	if !storageIsTheOnlyMigrationObstacle(vmi) {
		return nil, false
	}

	obj, exists, err := c.vmStore.GetByKey(controller.NamespacedKey(vmi.Namespace, vmi.Name))
	if err != nil || !exists {
		return nil, false
	}
	vm, ok := obj.(*virtv1.VirtualMachine)
	if !ok {
		return nil, false
	}
	value, found := vm.Annotations[migrationNodeAffinityTermsAnn]
	if !found {
		return nil, false
	}

	var terms []k8sv1.NodeSelectorTerm
	if err := json.Unmarshal([]byte(value), &terms); err != nil {
		log.Log.Object(vmi).Reason(err).Errorf("failed to parse the %s annotation of the VirtualMachine", migrationNodeAffinityTermsAnn)
		return nil, false
	}

	return terms, true
}

// storageIsTheOnlyMigrationObstacle reports whether the VirtualMachineInstance would be live
// migratable if its storage moved along with it. Both conditions are calculated by virt-handler from
// the same list of obstacles, StorageLiveMigratable being the one that leaves the volumes out of it.
func storageIsTheOnlyMigrationObstacle(vmi *virtv1.VirtualMachineInstance) bool {
	condManager := controller.NewVirtualMachineInstanceConditionManager()
	return condManager.HasConditionWithStatus(vmi, virtv1.VirtualMachineInstanceIsStorageLiveMigratable, k8sv1.ConditionTrue) &&
		condManager.HasConditionWithStatusAndReason(vmi, virtv1.VirtualMachineInstanceIsMigratable,
			k8sv1.ConditionFalse, virtv1.VirtualMachineInstanceReasonDisksNotMigratable)
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
