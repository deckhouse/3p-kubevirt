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

package virthandler

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/testutils"
)

var _ = Describe("Migration source sync-slot gate", func() {
	const sourceNode = "node01"

	newController := func(syncCap uint32) *MigrationSourceController {
		config, _, _ := testutils.NewFakeClusterConfigUsingKVConfig(&v1.KubeVirtConfiguration{
			MigrationConfiguration: &v1.MigrationConfiguration{
				ParallelSyncMigrationsPerNode:     pointer.P(syncCap),
				ParallelOutboundMigrationsPerNode: pointer.P(syncCap),
			},
		})
		return &MigrationSourceController{
			BaseController: &BaseController{
				host:          sourceNode,
				vmiStore:      cache.NewStore(cache.MetaNamespaceKeyFunc),
				clusterConfig: config,
			},
			syncSlotCache: make(map[types.UID]time.Time),
		}
	}

	sourceVMI := func(name string, migUID types.UID) *v1.VirtualMachineInstance {
		return &v1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID("uid-" + name)},
			Status: v1.VirtualMachineInstanceStatus{
				MigrationState: &v1.VirtualMachineInstanceMigrationState{
					MigrationUID: migUID,
					SourceNode:   sourceNode,
				},
			},
		}
	}

	It("gates a second migration once the per-node cap is full", func() {
		c := newController(1)
		a, b := sourceVMI("a", "mig-a"), sourceVMI("b", "mig-b")
		Expect(c.vmiStore.Add(a)).To(Succeed())
		Expect(c.vmiStore.Add(b)).To(Succeed())

		Expect(c.acquireSyncSlot(a)).To(BeTrue())
		Expect(c.acquireSyncSlot(b)).To(BeFalse())
	})

	It("admits up to the configured cap", func() {
		c := newController(2)
		a, b, d := sourceVMI("a", "mig-a"), sourceVMI("b", "mig-b"), sourceVMI("d", "mig-d")
		for _, vmi := range []*v1.VirtualMachineInstance{a, b, d} {
			Expect(c.vmiStore.Add(vmi)).To(Succeed())
		}
		Expect(c.acquireSyncSlot(a)).To(BeTrue())
		Expect(c.acquireSyncSlot(b)).To(BeTrue())
		Expect(c.acquireSyncSlot(d)).To(BeFalse())
	})

	It("is idempotent for the same attempt and consumes a single slot", func() {
		c := newController(1)
		a := sourceVMI("a", "mig-a")
		Expect(c.vmiStore.Add(a)).To(Succeed())

		Expect(c.acquireSyncSlot(a)).To(BeTrue())
		Expect(c.acquireSyncSlot(a)).To(BeTrue())
		Expect(c.syncSlotCache).To(HaveLen(1))
	})

	It("hands the reservation off to the migrating count once the transfer starts", func() {
		c := newController(1)
		a, b := sourceVMI("a", "mig-a"), sourceVMI("b", "mig-b")
		Expect(c.vmiStore.Add(a)).To(Succeed())
		Expect(c.vmiStore.Add(b)).To(Succeed())

		Expect(c.acquireSyncSlot(a)).To(BeTrue())

		// a starts transferring: its reservation is released but it still holds the slot via migrating
		a.Status.MigrationState.StartTimestamp = pointer.P(metav1.Now())
		Expect(c.acquireSyncSlot(b)).To(BeFalse())
		Expect(c.syncSlotCache).ToNot(HaveKey(types.UID("mig-a")))

		// a completes: the slot frees
		a.Status.MigrationState.Completed = true
		Expect(c.acquireSyncSlot(b)).To(BeTrue())
	})

	It("frees the slot on explicit release", func() {
		c := newController(1)
		a, b := sourceVMI("a", "mig-a"), sourceVMI("b", "mig-b")
		Expect(c.vmiStore.Add(a)).To(Succeed())
		Expect(c.vmiStore.Add(b)).To(Succeed())

		Expect(c.acquireSyncSlot(a)).To(BeTrue())
		Expect(c.acquireSyncSlot(b)).To(BeFalse())

		c.releaseSyncSlot(a)
		Expect(c.syncSlotCache).ToNot(HaveKey(types.UID("mig-a")))
		Expect(c.acquireSyncSlot(b)).To(BeTrue())
	})

	It("self-heals an orphaned reservation whose VMI is gone", func() {
		c := newController(1)
		a, b := sourceVMI("a", "mig-a"), sourceVMI("b", "mig-b")
		Expect(c.vmiStore.Add(a)).To(Succeed())
		Expect(c.vmiStore.Add(b)).To(Succeed())

		Expect(c.acquireSyncSlot(a)).To(BeTrue())

		Expect(c.vmiStore.Delete(a)).To(Succeed())
		Expect(c.acquireSyncSlot(b)).To(BeTrue())
		Expect(c.syncSlotCache).ToNot(HaveKey(types.UID("mig-a")))
	})

	It("does not let a new attempt inherit a previous attempt's reservation", func() {
		c := newController(1)
		a := sourceVMI("a", "mig-a")
		Expect(c.vmiStore.Add(a)).To(Succeed())
		Expect(c.acquireSyncSlot(a)).To(BeTrue())

		// a different migration takes the only slot by actually transferring
		other := sourceVMI("other", "mig-other")
		other.Status.MigrationState.StartTimestamp = pointer.P(metav1.Now())
		Expect(c.vmiStore.Add(other)).To(Succeed())

		// a's first attempt is replaced by a fresh one on the same VMI
		a.Status.MigrationState.MigrationUID = "mig-a2"

		// the fresh attempt must re-evaluate the cap, not ride the stale mig-a entry
		Expect(c.acquireSyncSlot(a)).To(BeFalse())
		Expect(c.syncSlotCache).ToNot(HaveKey(types.UID("mig-a")))
	})

	It("reclaims a reservation older than the TTL even if still pre-start", func() {
		c := newController(1)
		a, b := sourceVMI("a", "mig-a"), sourceVMI("b", "mig-b")
		Expect(c.vmiStore.Add(a)).To(Succeed())
		Expect(c.vmiStore.Add(b)).To(Succeed())

		// a is live and pre-start (only the TTL can release it); seed a stale reservation
		c.syncSlotCache["mig-a"] = time.Now().Add(-syncSlotReservationTTL - time.Minute)

		Expect(c.acquireSyncSlot(b)).To(BeTrue())
		Expect(c.syncSlotCache).ToNot(HaveKey(types.UID("mig-a")))
	})

	It("neither gates nor caches a migration with an empty UID", func() {
		c := newController(1)
		a := sourceVMI("a", "")
		Expect(c.vmiStore.Add(a)).To(Succeed())

		Expect(c.acquireSyncSlot(a)).To(BeTrue())
		Expect(c.syncSlotCache).To(BeEmpty())
	})
})
