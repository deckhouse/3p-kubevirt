/*
Copyright The KubeVirt Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vm

import (
	"context"
	"fmt"
	"sync"
	"time"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	"kubevirt.io/kubevirt/pkg/apimachinery/patch"
)

type constructor func(vmInformer cache.SharedIndexInformer, clientSet kubecli.KubevirtClient, logger *log.FilteredLogger) (Migration, error)

var newMigrations = []constructor{
	newFirmwareUUIDMigration,
}

type Migration interface {
	Name() string
	Migrate(ctx context.Context) error
}

type MigrationController struct {
	logger     *log.FilteredLogger
	migrations []Migration
}

func NewMigrationController(vmInformer cache.SharedIndexInformer, clientSet kubecli.KubevirtClient, logger *log.FilteredLogger) (*MigrationController, error) {
	migrations := make([]Migration, len(newMigrations))
	for i, newMigration := range newMigrations {
		m, err := newMigration(vmInformer, clientSet, logger)
		if err != nil {
			return nil, err
		}
		migrations[i] = m
	}

	return &MigrationController{
		logger:     logger,
		migrations: migrations,
	}, nil
}

func (c *MigrationController) Run(ctx context.Context) {
	wg := &sync.WaitGroup{}

	c.run(ctx, wg, c.migrations)
	wg.Wait()
}

func (c *MigrationController) run(ctx context.Context, wg *sync.WaitGroup, migrations []Migration) {
	for _, m := range migrations {
		wg.Add(1)
		lg := c.logger.With("name", m.Name())
		lg.Info("Running migration")

		go func() {
			defer wg.Done()

			for {
				select {
				case <-ctx.Done():
					lg.Info("Cancelled migration")
					return
				default:
					if err := m.Migrate(ctx); err != nil {
						lg.Errorf("Failed to run migration, retry after 5s: %v", err)
						time.Sleep(5 * time.Second)
						continue
					}
					lg.Info("Finished migration")
					return
				}
			}
		}()
	}
}

type firmwareUUIDMigration struct {
	vmInformer cache.SharedIndexInformer
	clientSet  kubecli.KubevirtClient
	logger     *log.FilteredLogger
}

func newFirmwareUUIDMigration(vmInformer cache.SharedIndexInformer, clientSet kubecli.KubevirtClient, logger *log.FilteredLogger) (Migration, error) {
	return &firmwareUUIDMigration{
		vmInformer: vmInformer,
		clientSet:  clientSet,
		logger:     logger,
	}, nil
}

func (m *firmwareUUIDMigration) Name() string {
	return "vm-firmware-uuid"
}

func (m *firmwareUUIDMigration) Migrate(ctx context.Context) error {
	m.logger.Info("Starting firmware UUID migration for all VMs")

	// Get all VMs from informer store
	vmStore := m.vmInformer.GetStore()
	vms := vmStore.List()

	for _, obj := range vms {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		kvvm, ok := obj.(*v1.VirtualMachine)
		if !ok {
			continue
		}

		// Check if firmware UUID is already set
		firmware := kvvm.Spec.Template.Spec.Domain.Firmware
		if firmware != nil && firmware.UUID != "" {
			continue
		}

		// Generate new firmware with UUID
		if firmware == nil {
			firmware = &v1.Firmware{}
		}
		firmware = firmware.DeepCopy()
		firmware.UUID = CalculateLegacyUUID(kvvm.Name)

		// Create patch
		patchBytes, err := patch.New(
			patch.WithTest("/spec/template/spec/domain/firmware", kvvm.Spec.Template.Spec.Domain.Firmware),
			patch.WithAdd("/spec/template/spec/domain/firmware", firmware),
		).GeneratePayload()
		if err != nil {
			return fmt.Errorf("failed to generate patch for VM %s/%s: %w", kvvm.Namespace, kvvm.Name, err)
		}

		m.logger.Infof("Patching firmware UUID for VM %s/%s", kvvm.Namespace, kvvm.Name)

		// Apply patch
		_, err = m.clientSet.GeneratedKubeVirtClient().KubevirtV1().
			VirtualMachines(kvvm.Namespace).
			Patch(ctx, kvvm.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("failed to patch VM %s/%s: %w", kvvm.Namespace, kvvm.Name, err)
		}

		m.logger.V(4).Infof("Successfully patched firmware UUID for VM %s/%s", kvvm.Namespace, kvvm.Name)
	}

	m.logger.Info("Completed firmware UUID migration for all VMs")
	return nil
}
