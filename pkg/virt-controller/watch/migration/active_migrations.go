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
package migration

import (
	"fmt"
	"sync"
	"time"

	k8sv1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"

	virtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/pointer"
	migrationsutil "kubevirt.io/kubevirt/pkg/util/migrations"
)

// activeSlotRequeueDelay is how often a migration waiting for an active slot rechecks it. A
// finishing migration wakes the waiting ones; the recheck covers a missed event.
const activeSlotRequeueDelay = 5 * time.Second

// activeMigrationLoad is the number of permitted migrations transferring memory, by node and in
// the whole cluster.
type activeMigrationLoad struct {
	outbound map[string]int
	inbound  map[string]int
	cluster  int
}

// pendingPermits remembers the permits granted but not yet seen in the VMI informer, so two
// migrations permitted one after the other within the informer lag do not both take the last slot.
type pendingPermits struct {
	mu      sync.Mutex
	permits map[types.UID]struct{}
}

func newPendingPermits() *pendingPermits {
	return &pendingPermits{permits: map[types.UID]struct{}{}}
}

func (p *pendingPermits) add(uid types.UID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.permits[uid] = struct{}{}
}

func (p *pendingPermits) has(uid types.UID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.permits[uid]
	return ok
}

func (p *pendingPermits) forget(uid types.UID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.permits, uid)
}

// handleTransferPermission permits a migration that has prepared its target and waits for a
// permit to start transferring memory. With the active migration limits removed since the
// handoff, it is permitted right away; otherwise only when it fits every configured limit.
func (c *Controller) handleTransferPermission(key string, migration *virtv1.VirtualMachineInstanceMigration, vmi *virtv1.VirtualMachineInstance) error {
	state := vmi.Status.MigrationState
	if state == nil || state.MigrationUID != migration.UID || !state.TransferPermitRequired || state.TransferPermitted || state.StartTimestamp != nil {
		return nil
	}
	// The source waits for the external migration configuration as well; a permit granted
	// before it would hold a slot no data moves through. Filling it updates the VMI, which
	// enqueues the migration again.
	if state.MigrationConfiguration == nil {
		return nil
	}
	// A canceled volume migration is unwound in the same reconcile; it never transfers memory.
	if isVolumeMigrationCanceled(migration, vmi) {
		return nil
	}

	cfg := c.clusterConfig.GetMigrationConfiguration()
	if migrationsutil.ActiveMigrationLimitsConfigured(cfg) {
		c.migrationStartLock.Lock()
		defer c.migrationStartLock.Unlock()

		if reason, _ := activeLimitReasonAndMessage(cfg, c.activeMigrationLoad(), state.SourceNode, state.TargetNode); reason != "" {
			log.Log.Object(migration).V(4).Infof("Waiting for an active migration slot: %s", reason)
			c.Queue.AddWithOpts(priorityqueue.AddOpts{Priority: pointer.P(pendingPriority), After: activeSlotRequeueDelay}, key)
			return nil
		}
	}

	permittedVMI := vmi.DeepCopy()
	permittedVMI.Status.MigrationState.TransferPermitted = true
	if err := c.patchVMI(vmi, permittedVMI); err != nil {
		return err
	}
	c.pendingPermits.add(migration.UID)
	return nil
}

// migrationFinished forgets the permit of a finished or deleted migration and wakes the migrations
// with a prepared target whose slots it freed, so the next one starts without waiting for the
// recheck: those sharing a node with it, or all of them under a cluster limit.
func (c *Controller) migrationFinished(migration *virtv1.VirtualMachineInstanceMigration) {
	c.pendingPermits.forget(migration.UID)

	cfg := c.clusterConfig.GetMigrationConfiguration()
	if !migrationsutil.ActiveMigrationLimitsConfigured(cfg) {
		return
	}
	freed := c.migrationNodes(migration)
	for _, waiting := range migrationsutil.ListUnfinishedMigrations(c.migrationIndexer) {
		if waiting.Status.Phase != virtv1.MigrationTargetReady {
			continue
		}
		if cfg.ActiveMigrationsPerCluster != nil || len(freed) == 0 || sharesNode(freed, c.migrationNodes(waiting)) {
			c.enqueueMigration(waiting)
		}
	}
}

// migrationNodes returns the source and target nodes of a migration from its VMI, or nothing when
// the VMI no longer describes this migration.
func (c *Controller) migrationNodes(migration *virtv1.VirtualMachineInstanceMigration) []string {
	obj, exists, err := c.vmiStore.GetByKey(controller.NamespacedKey(migration.Namespace, migration.Spec.VMIName))
	if err != nil || !exists {
		return nil
	}
	state := obj.(*virtv1.VirtualMachineInstance).Status.MigrationState
	if state == nil || state.MigrationUID != migration.UID {
		return nil
	}
	return []string{state.SourceNode, state.TargetNode}
}

func sharesNode(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x != "" && x == y {
				return true
			}
		}
	}
	return false
}

// waitsForTransferPermission reports whether the source of vmi's migration holds back the transfer
// until virt-controller permits it.
func waitsForTransferPermission(vmi *virtv1.VirtualMachineInstance) bool {
	return vmi.Status.MigrationState.TransferPermitRequired && !vmi.Status.MigrationState.TransferPermitted
}

// activeMigrationLoad counts the unfinished local migrations permitted to transfer memory.
func (c *Controller) activeMigrationLoad() activeMigrationLoad {
	load := activeMigrationLoad{outbound: map[string]int{}, inbound: map[string]int{}}

	for _, migration := range migrationsutil.ListUnfinishedMigrations(c.migrationIndexer) {
		if migration.IsDecentralized() {
			continue
		}
		obj, exists, err := c.vmiStore.GetByKey(controller.NamespacedKey(migration.Namespace, migration.Spec.VMIName))
		if err != nil || !exists {
			continue
		}
		state := obj.(*virtv1.VirtualMachineInstance).Status.MigrationState
		if state == nil || state.MigrationUID != migration.UID {
			continue
		}
		// A migration transferring without a permit was started before the limits were
		// configured or before an upgrade; it takes its slots all the same.
		if state.TransferPermitted || state.StartTimestamp != nil {
			c.pendingPermits.forget(migration.UID)
		} else if !c.pendingPermits.has(migration.UID) {
			continue
		}
		load.outbound[state.SourceNode]++
		load.inbound[state.TargetNode]++
		load.cluster++
	}
	return load
}

// activeLimitReasonAndMessage returns the reason a migration from sourceNode to targetNode may not
// start transferring memory yet, or empty strings when it fits every configured active limit.
func activeLimitReasonAndMessage(cfg *virtv1.MigrationConfiguration, load activeMigrationLoad, sourceNode, targetNode string) (string, string) {
	if cfg.ActiveMigrationsPerCluster != nil && load.cluster >= int(*cfg.ActiveMigrationsPerCluster) {
		return virtv1.VirtualMachineInstanceMigrationConcurrencyLimitReachedReasonActiveCluster,
			"The cluster limit of live migrations transferring memory is reached."
	}

	if cfg.ActiveMigrationsPerNode != nil {
		limit := int(*cfg.ActiveMigrationsPerNode)
		for _, node := range []string{sourceNode, targetNode} {
			if load.outbound[node]+load.inbound[node] >= limit {
				return virtv1.VirtualMachineInstanceMigrationConcurrencyLimitReachedReasonActiveNode,
					fmt.Sprintf("The node %q already transfers the maximum number of live migrations.", node)
			}
		}
		return "", ""
	}

	if cfg.ActiveOutboundMigrationsPerNode != nil && load.outbound[sourceNode] >= int(*cfg.ActiveOutboundMigrationsPerNode) {
		return virtv1.VirtualMachineInstanceMigrationConcurrencyLimitReachedReasonActiveOutboundNode,
			fmt.Sprintf("The source node %q already sends the maximum number of live migrations.", sourceNode)
	}
	if cfg.ActiveInboundMigrationsPerNode != nil && load.inbound[targetNode] >= int(*cfg.ActiveInboundMigrationsPerNode) {
		return virtv1.VirtualMachineInstanceMigrationConcurrencyLimitReachedReasonActiveInboundNode,
			fmt.Sprintf("The target node %q already receives the maximum number of live migrations.", targetNode)
	}
	return "", ""
}

// reconcileActiveLimitCondition reports on a migration with a prepared target which active limit
// keeps it from transferring memory, and clears the condition once it is permitted.
func (c *Controller) reconcileActiveLimitCondition(cm *controller.VirtualMachineInstanceMigrationConditionManager, migrationCopy *virtv1.VirtualMachineInstanceMigration, vmi *virtv1.VirtualMachineInstance) {
	reason, message := "", ""
	cfg := c.clusterConfig.GetMigrationConfiguration()
	// A permit granted in this reconcile is not in the cached VMI yet; it is not a wait.
	if state := vmi.Status.MigrationState; state != nil && state.MigrationUID == migrationCopy.UID &&
		state.TransferPermitRequired && !state.TransferPermitted && state.StartTimestamp == nil &&
		!c.pendingPermits.has(migrationCopy.UID) && migrationsutil.ActiveMigrationLimitsConfigured(cfg) {
		reason, message = activeLimitReasonAndMessage(cfg, c.activeMigrationLoad(), state.SourceNode, state.TargetNode)
	}

	if message != "" {
		cm.UpdateCondition(migrationCopy, &virtv1.VirtualMachineInstanceMigrationCondition{
			Type:          virtv1.VirtualMachineInstanceMigrationConcurrencyLimitReached,
			Status:        k8sv1.ConditionTrue,
			Reason:        reason,
			Message:       message,
			LastProbeTime: v1.Now(),
		})
		return
	}
	if cm.HasCondition(migrationCopy, virtv1.VirtualMachineInstanceMigrationConcurrencyLimitReached) {
		cm.RemoveCondition(migrationCopy, virtv1.VirtualMachineInstanceMigrationConcurrencyLimitReached)
	}
}
