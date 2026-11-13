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
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.  * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package synchronization

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	virtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/apimachinery/patch"
	"kubevirt.io/kubevirt/pkg/controller"

	context "golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"

	syncv1 "kubevirt.io/kubevirt/pkg/synchronizer-com/synchronization/v1"
	kvtls "kubevirt.io/kubevirt/pkg/util/tls"
)

const (
	defaultTimeout = 30

	MyPodIP = "MY_POD_IP"

	noSourceStatusErrorMsg                        = "must pass source status"
	noTargetStatusErrorMsg                        = "must pass target status"
	sourceUnableToLocateVMIMigrationIDErrorMsg    = "source: unable to locate VMI for migrationID %s"
	targetUnableToLocateVMIMigrationIDErrorMsg    = "target: unable to locate VMI for migrationID %s"
	sourceUnableToLocateVMIMigrationIDErrorMsgVMI = "source: unable to locate VMI for migrationID %s, vmi: %s"
	targetUnableToLocateVMIMigrationIDErrorMsgVMI = "target: unable to locate VMI for migrationID %s, vmi: %s"

	successMessage = "success"

	maxCloseRetries = 10

	SynchronizationFinalizer = "synchronization.kubevirt.io/migrationFinalizer"
)

type SynchronizationController struct {
	client   kubecli.KubevirtClient
	connChan chan io.ReadWriteCloser

	vmiInformer       cache.SharedIndexInformer
	migrationInformer cache.SharedIndexInformer

	listener    net.Listener
	bindAddress string
	bindPort    int
	ip          string
	timeout     int
	// identities holds the ephemeral TLS key of each decentralized migration by its ID.
	identities sync.Map

	queue     workqueue.TypedRateLimitingInterface[string]
	hasSynced func() bool

	syncOutboundConnectionMap  *sync.Map
	syncReceivingConnectionMap *sync.Map
	failedCloseConnections     *sync.Map
	grpcServer                 *grpc.Server
}

func NewSynchronizationController(
	client kubecli.KubevirtClient,
	vmiInformer cache.SharedIndexInformer,
	migrationInformer cache.SharedIndexInformer,
	bindAddress string,
	bindPort int,
	ip string,
) (*SynchronizationController, error) {
	syncController := &SynchronizationController{
		vmiInformer:       vmiInformer,
		migrationInformer: migrationInformer,
		timeout:           defaultTimeout,
		bindAddress:       bindAddress,
		bindPort:          bindPort,
		client:            client,
		ip:                ip,
	}

	// The peer refuses the connection until it learns the pin of this cluster, which takes a few seconds:
	// the default limiter would put the next attempt minutes away after a second of refusals.
	queue := workqueue.NewTypedRateLimitingQueueWithConfig[string](
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](5*time.Millisecond, 5*time.Second),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "sync-vmi-status"},
	)
	syncController.queue = queue

	syncController.hasSynced = func() bool {
		return vmiInformer.HasSynced() && migrationInformer.HasSynced()
	}

	syncController.syncOutboundConnectionMap = &sync.Map{}
	syncController.syncReceivingConnectionMap = &sync.Map{}
	syncController.failedCloseConnections = &sync.Map{}

	_, err := vmiInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    syncController.addVmiFunc,
		DeleteFunc: syncController.deleteVmiFunc,
		UpdateFunc: syncController.updateVmiFunc,
	})
	if err != nil {
		return nil, err
	}

	if err := syncController.migrationInformer.AddIndexers(map[string]cache.IndexFunc{
		"byUID":               indexByMigrationUID,
		"byActiveVMIName":     indexByActiveVmiName,
		"byTargetMigrationID": indexByTargetMigrationID,
		"bySourceMigrationID": indexBySourceMigrationID,
	}); err != nil {
		return nil, err
	}

	if _, err := syncController.migrationInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    syncController.addMigrationFunc,
		DeleteFunc: syncController.deleteMigrationFunc,
		UpdateFunc: syncController.updateMigrationFunc,
	}); err != nil {
		return nil, err
	}

	syncController.grpcServer = grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		GetConfigForClient: syncController.pinnedServerConfig,
	})))
	syncv1.RegisterSynchronizeServer(syncController.grpcServer, syncController)

	return syncController, nil
}

func (s *SynchronizationController) addVmiFunc(addObj interface{}) {
	s.enqueueVirtualMachineInstance(addObj)
}

func (s *SynchronizationController) deleteVmiFunc(addObj interface{}) {
	s.enqueueVirtualMachineInstance(addObj)
}

func (s *SynchronizationController) updateVmiFunc(_, curr interface{}) {
	s.enqueueVirtualMachineInstance(curr)
}

func (s *SynchronizationController) enqueueVirtualMachineInstance(obj interface{}) {
	vmi, ok := obj.(*virtv1.VirtualMachineInstance)
	if ok {
		key, err := controller.KeyFunc(vmi)
		if err != nil {
			log.Log.Object(vmi).Reason(err).Error("failed to extract key from virtualmachine.")
			return
		}
		s.queue.Add(key)
	}
}

func (s *SynchronizationController) addMigrationFunc(addObj interface{}) {
	s.enqueueVirtualMachineInstanceFromMigration(addObj)
}

func (s *SynchronizationController) deleteMigrationFunc(delObj interface{}) {
	// Clean up any synchronization connections in the map.
	s.enqueueVirtualMachineInstanceFromMigration(delObj)
	// Close any connections associated with this migration.
	migration, ok := delObj.(*virtv1.VirtualMachineInstanceMigration)
	if ok {
		if !migration.IsDecentralized() {
			return
		}
		// The key goes with the migration, not with the connection: a connection closed on the final state
		// may be reopened, and the peer holds the pin of this very key.
		if migration.Spec.Receive != nil {
			s.identities.Delete(migration.Spec.Receive.MigrationID)
			log.Log.V(4).Object(migration).Infof("closing receiving connection for migrationID %s", migration.Spec.Receive.MigrationID)
			if err := s.closeConnectionForMigrationID(s.syncReceivingConnectionMap, migration.Spec.Receive.MigrationID); err != nil {
				log.Log.Reason(err).Infof("unable to close connection for migrationID %s, possibly leaked connection", migration.Spec.Receive.MigrationID)
			}
		} else if migration.Spec.SendTo != nil {
			s.identities.Delete(migration.Spec.SendTo.MigrationID)
			log.Log.V(4).Object(migration).Infof("closing outbound connection for migrationID %s", migration.Spec.SendTo.MigrationID)
			if err := s.closeConnectionForMigrationID(s.syncOutboundConnectionMap, migration.Spec.SendTo.MigrationID); err != nil {
				log.Log.Reason(err).Infof("unable to close connection for migrationID %s, possibly leaked connection", migration.Spec.SendTo.MigrationID)
			}
		}
	}
}

// The peers of a decentralized migration share no CA: each synchronization controller
// generates a key for the migration and the peers verify each other by the pin of the
// public key. The own pin is published in the VMI, the pin of the peer arrives in the
// migration annotations, both handed over by the management layer out of band. The
// client sends the migration ID as SNI for the server to pick the matching key.

func (s *SynchronizationController) identity(migrationID string) (*kvtls.Identity, error) {
	if obj, ok := s.identities.Load(migrationID); ok {
		return obj.(*kvtls.Identity), nil
	}
	id, err := kvtls.NewEphemeralIdentity(migrationID)
	if err != nil {
		return nil, fmt.Errorf("generate the synchronization key for migration %s: %w", migrationID, err)
	}
	obj, _ := s.identities.LoadOrStore(migrationID, id)
	return obj.(*kvtls.Identity), nil
}

func (s *SynchronizationController) pinnedServerConfig(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	migrationID := hello.ServerName
	obj, ok := s.identities.Load(migrationID)
	if !ok {
		return nil, fmt.Errorf("no synchronization key for migration %q: unknown or over", migrationID)
	}
	config := obj.(*kvtls.Identity).ServerConfig(func() string { return s.peerSyncPin(migrationID) })
	config.NextProtos = []string{"h2"}
	return config, nil
}

func (s *SynchronizationController) peerSyncPin(migrationID string) string {
	migration, _ := s.findTargetMigrationFromMigrationID(migrationID)
	if migration != nil {
		if pin := migration.Annotations[virtv1.MigrationPeerSyncPinAnnotation]; pin != "" {
			return pin
		}
	}
	// When both migrations are local, the sender can already have the pin needed to publish source state.
	migration, _ = s.findSourceMigrationFromMigrationID(migrationID)
	if migration == nil {
		return ""
	}
	return migration.Annotations[virtv1.MigrationPeerSyncPinAnnotation]
}

func syncEndpointStateField(vmi *virtv1.VirtualMachineInstance, state *virtv1.VirtualMachineInstanceCommonMigrationState) (string, error) {
	ms := vmi.Status.MigrationState
	if ms != nil && ms.SourceState != nil && state == &ms.SourceState.VirtualMachineInstanceCommonMigrationState {
		return "sourceState", nil
	}
	if ms != nil && ms.TargetState != nil && state == &ms.TargetState.VirtualMachineInstanceCommonMigrationState {
		return "targetState", nil
	}
	return "", fmt.Errorf("synchronization endpoint has no local migration state")
}

// publishSyncEndpoint refreshes the local address and key together so retries recover after a controller restart.
func (s *SynchronizationController) publishSyncEndpoint(
	vmi *virtv1.VirtualMachineInstance, migrationID string, migrationUID types.UID,
	state *virtv1.VirtualMachineInstanceCommonMigrationState,
) error {
	if state.MigrationUID != migrationUID || migrationUID == "" {
		return fmt.Errorf("synchronization endpoint belongs to migration %s, expected %s", state.MigrationUID, migrationUID)
	}
	id, err := s.identity(migrationID)
	if err != nil {
		return err
	}
	address, err := s.getLocalSynchronizationAddress()
	if err != nil {
		return err
	}
	if state.SyncPin == id.Pin && state.SyncAddress != nil && *state.SyncAddress == address {
		return nil
	}
	field, err := syncEndpointStateField(vmi, state)
	if err != nil {
		return err
	}
	ms := vmi.Status.MigrationState
	// Transport identity may rotate after Completed, until the final state is delivered.
	// Patch only our address and pin; the final outcome and the peer's state are immutable here.
	patchSet := patch.New()
	if vmi.UID != "" {
		patchSet.AddOption(patch.WithTest("/metadata/uid", vmi.UID))
	}
	if ms.MigrationUID != "" {
		patchSet.AddOption(patch.WithTest("/status/migrationState/migrationUid", ms.MigrationUID))
	} else {
		patchSet.AddOption(patch.WithTest("/status/migrationState", ms))
	}
	path := "/status/migrationState/" + field
	if state.MigrationUID != "" {
		patchSet.AddOption(patch.WithTest(path+"/migrationUID", state.MigrationUID))
	}
	patchSet.AddOption(
		patch.WithAdd(path+"/syncAddress", address),
		patch.WithAdd(path+"/syncPin", id.Pin),
	)
	payload, err := patchSet.GeneratePayload()
	if err != nil {
		return err
	}
	_, err = s.client.VirtualMachineInstance(vmi.Namespace).Patch(
		context.Background(), vmi.Name, types.JSONPatchType, payload, metav1.PatchOptions{},
	)
	if err != nil {
		return err
	}
	state.SyncPin = id.Pin
	state.SyncAddress = &address
	log.Log.Object(vmi).Infof("published the synchronization endpoint for migration %s", migrationID)
	return nil
}

func (s *SynchronizationController) closeConnectionForMigrationID(syncMap *sync.Map, migrationID string) error {
	obj, loaded := syncMap.LoadAndDelete(migrationID)
	if loaded {
		log.Log.V(4).Infof("closing connection associated with migrationID %s", migrationID)
		outboundConnection, ok := obj.(*SynchronizationConnection)
		if ok {
			if err := outboundConnection.Close(); err != nil {
				log.Log.Warningf("unable to close connection for migrationID %s, %v", migrationID, err)
				s.failedCloseConnections.Store(outboundConnection, 0)
				return err
			}
		} else {
			log.Log.Warningf("unable to close connection for migrationID %s, type is %v", migrationID, obj)
			return fmt.Errorf("unknown type %v", obj)
		}
	}
	return nil
}

func (s *SynchronizationController) updateMigrationFunc(_, curr interface{}) {
	s.enqueueVirtualMachineInstanceFromMigration(curr)
}

func (s *SynchronizationController) enqueueVirtualMachineInstanceFromMigration(obj interface{}) {
	migration, ok := obj.(*virtv1.VirtualMachineInstanceMigration)
	if ok {
		key := controller.NamespacedKey(migration.Namespace, migration.Spec.VMIName)
		s.queue.Add(key)
	}
}

func (s *SynchronizationController) Run(threadiness int, stopCh <-chan struct{}) error {
	defer controller.HandlePanic()
	defer s.queue.ShutDown()
	defer s.closeConnections()

	log.Log.Info("starting vmi status synchronization controller.")

	// Wait for cache sync before we start the pod controller
	cache.WaitForCacheSync(stopCh, s.hasSynced)

	// Start the actual work
	for i := 0; i < threadiness; i++ {
		go wait.Until(s.runWorker, time.Second, stopCh)
	}
	go wait.Until(s.runConnectionCleanup, 5*time.Second, stopCh)

	conn, err := s.createTcpListener()
	if err != nil {
		log.Log.Criticalf("received error %v, exiting", err)
		return err
	} else {
		go func() {
			s.grpcServer.Serve(conn)
		}()
	}
	if err := s.rebuildConnectionsAndUpdateSyncAddress(); err != nil {
		return err
	}

	log.Log.Info("waiting on stop signal")
	<-stopCh
	log.Log.Info("normally stopping vmi status synchronization controller.")
	return nil
}

func (s *SynchronizationController) closeConnections() {
	log.Log.V(1).Info("closing listener and grpcserver")
	if s.listener != nil {
		s.listener.Close()
	}
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
	log.Log.V(1).Infof("closing outbound connections")
	s.syncOutboundConnectionMap.Range(closeMapConnections)
	log.Log.V(1).Infof("closing inbound connections")
	s.syncReceivingConnectionMap.Range(closeMapConnections)
}

func closeMapConnections(k, obj interface{}) bool {
	outboundConnection, ok := obj.(*SynchronizationConnection)
	if ok && outboundConnection != nil {
		log.Log.V(1).Infof("closing connection for migration ID: %s", outboundConnection.migrationID)
		if err := outboundConnection.Close(); err != nil {
			log.Log.Warningf("unable to close connection for VMI %s during shutdown, %v", k, err)
		}
	} else {
		log.Log.Warningf("unable to close connection for VMI %s during shutdown", k)
	}
	return true
}

func (s *SynchronizationController) runWorker() {
	for s.Execute() {
	}
}

func (s *SynchronizationController) Execute() bool {
	key, quit := s.queue.Get()
	if quit {
		return false
	}

	defer s.queue.Done(key)
	err := s.execute(key)

	if err != nil {
		log.Log.Reason(err).Infof("reenqueuing VirtualMachineInstance %v", key)
		s.queue.AddRateLimited(key)
	} else {
		log.Log.V(4).Infof("processed VirtualMachineInstance %v", key)
		s.queue.Forget(key)
	}
	return true
}

func (s *SynchronizationController) execute(key string) error {
	// A VMI may have finished cleanup, started another migration, or disappeared.
	// None of these may strand the finalizer on its previous final migration.
	if err := s.removeFinalizerFromFinalMigrations(key); err != nil {
		return err
	}
	// Fetch the latest VMI state from cache
	obj, exists, _ := s.vmiInformer.GetStore().GetByKey(key)
	if !exists {
		return nil
	}
	vmi := obj.(*virtv1.VirtualMachineInstance)

	migration, err := s.getMigrationForVMI(vmi)
	if err != nil {
		return err
	}
	if migration != nil && migration.IsDecentralized() {
		if err := s.handleMigrationFinalizer(migration); err != nil {
			return err
		}
		if migration.IsDecentralizedSource() {
			if migration.DeletionTimestamp != nil {
				log.Log.V(2).Object(migration).Infof("migration is being deleted, informing the target that the migration is canceled")
				// migration is being deleted, inform the target that the migration is canceled
				if err := s.cancelTargetRemoteMigration(vmi, migration); err != nil {
					return err
				}
				return nil
			}
			if err := s.handleSourceState(vmi.DeepCopy(), migration); err != nil {
				return err
			}
		}
		if migration.IsDecentralizedTarget() {
			if migration.DeletionTimestamp != nil {
				log.Log.V(2).Object(migration).Infof("migration is being deleted, informing the source that the migration is canceled")
				// migration is being deleted, inform the target that the migration is canceled
				if err := s.cancelSourceRemoteMigration(vmi, migration); err != nil {
					return err
				}
				return nil
			}
			return s.handleTargetState(vmi.DeepCopy(), migration)
		}
		return nil
	} else {
		// No migration found don't do anything
		log.Log.Object(vmi).V(2).Info("no active decentralized migration found for VMI")
		return nil
	}
}

func (s *SynchronizationController) removeFinalizerFromFinalMigrations(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	objects, err := s.migrationInformer.GetIndexer().ByIndex("byActiveVMIName", name)
	if err != nil {
		return err
	}
	for _, obj := range objects {
		migration := obj.(*virtv1.VirtualMachineInstanceMigration)
		if migration.Namespace == namespace && migration.IsDecentralized() && migration.IsFinal() {
			if err := s.handleMigrationFinalizer(migration); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *SynchronizationController) handleMigrationFinalizer(migration *virtv1.VirtualMachineInstanceMigration) error {
	originalMigration := migration
	migration = migration.DeepCopy()
	if !migration.IsFinal() {
		controller.AddFinalizer(migration, SynchronizationFinalizer)
	} else {
		controller.RemoveFinalizer(migration, SynchronizationFinalizer)
	}
	if !apiequality.Semantic.DeepEqual(originalMigration.ObjectMeta, migration.ObjectMeta) {
		log.Log.V(4).Object(migration).Infof("adding or removing finalizer to migration, %v", migration.Finalizers)
		patchSet := patch.New()
		patchSet.AddOption(
			patch.WithReplace("/metadata/finalizers", migration.Finalizers),
		)
		patchBytes, err := patchSet.GeneratePayload()
		if err != nil {
			return err
		}
		if _, err := s.client.VirtualMachineInstanceMigration(migration.Namespace).Patch(context.Background(), migration.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func (s *SynchronizationController) cancelSourceRemoteMigration(vmi *virtv1.VirtualMachineInstance, migration *virtv1.VirtualMachineInstanceMigration) error {
	if vmi == nil || vmi.Status.MigrationState == nil || vmi.Status.MigrationState.SourceState == nil || vmi.Status.MigrationState.SourceState.MigrationUID == "" {
		return nil
	}
	if migration.UID == vmi.Status.MigrationState.SourceState.MigrationUID {
		return fmt.Errorf("source migration UID %s is the same as the VMI's migration UID %s", migration.UID, vmi.Status.MigrationState.SourceState.MigrationUID)
	}
	log.Log.V(4).Object(migration).Infof("cancelling source remote migration for VMI %s/%s", vmi.Namespace, vmi.Name)
	return s.cancelRemoteMigration(vmi.Status.MigrationState.SourceState.MigrationUID, migration.Spec.Receive.MigrationID, s.syncOutboundConnectionMap)
}

func (s *SynchronizationController) cancelTargetRemoteMigration(vmi *virtv1.VirtualMachineInstance, migration *virtv1.VirtualMachineInstanceMigration) error {
	if vmi == nil || vmi.Status.MigrationState == nil || vmi.Status.MigrationState.TargetState == nil || vmi.Status.MigrationState.TargetState.MigrationUID == "" {
		return nil
	}
	if migration.UID == vmi.Status.MigrationState.TargetState.MigrationUID {
		return fmt.Errorf("target migration UID %s is the same as the VMI's migration UID %s", migration.UID, vmi.Status.MigrationState.TargetState.MigrationUID)
	}
	log.Log.V(4).Object(migration).Infof("cancelling target remote migration for VMI %s/%s", vmi.Namespace, vmi.Name)
	return s.cancelRemoteMigration(vmi.Status.MigrationState.TargetState.MigrationUID, migration.Spec.SendTo.MigrationID, s.syncReceivingConnectionMap)
}

func (s *SynchronizationController) cancelRemoteMigration(migrationUID types.UID, migrationID string, connectionMap *sync.Map) error {
	obj, ok := connectionMap.Load(migrationID)
	if !ok {
		// No connection found, don't do anything
		return nil
	}
	conn, ok := obj.(*SynchronizationConnection)
	if !ok {
		return fmt.Errorf("found unknown object in outbound connection cache %#v", conn)
	}
	client := syncv1.NewSynchronizeClient(conn.grpcClientConnection)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.timeout)*time.Second)
	defer cancel()
	_, err := client.CancelMigration(ctx, &syncv1.MigrationCancelRequest{
		MigrationUID: string(migrationUID),
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *SynchronizationController) getMigrationIDFromUID(migrationUID types.UID) (string, error) {
	objs, err := s.migrationInformer.GetIndexer().ByIndex("byUID", string(migrationUID))
	if err != nil {
		return "", err
	}
	if len(objs) > 1 {
		return "", fmt.Errorf("found more than one migration with same UID")
	}
	if len(objs) == 0 {
		return "", nil
	}
	migration, ok := objs[0].(*virtv1.VirtualMachineInstanceMigration)
	if !ok {
		return "", fmt.Errorf("found unknown object in migration cache")
	}
	var migrationID string
	if migration.Spec.Receive != nil {
		migrationID = migration.Spec.Receive.MigrationID
	}
	if migration.Spec.SendTo != nil {
		migrationID = migration.Spec.SendTo.MigrationID
	}
	return migrationID, nil
}

// getOutboundMigrationConnection prefers the migration annotation over the address exchanged in VMI status.
func (s *SynchronizationController) getOutboundMigrationConnection(
	vmi *virtv1.VirtualMachineInstance,
	state *virtv1.VirtualMachineInstanceMigrationState,
	migration *virtv1.VirtualMachineInstanceMigration,
) (*SynchronizationConnection, error) {
	if state == nil {
		return nil, nil
	}
	var localState, remoteState *virtv1.VirtualMachineInstanceCommonMigrationState
	connections := s.syncOutboundConnectionMap
	switch {
	case migration.Spec.SendTo != nil && state.SourceState != nil:
		localState = &state.SourceState.VirtualMachineInstanceCommonMigrationState
		if state.TargetState != nil {
			remoteState = &state.TargetState.VirtualMachineInstanceCommonMigrationState
		}
	case migration.Spec.Receive != nil && state.TargetState != nil:
		localState = &state.TargetState.VirtualMachineInstanceCommonMigrationState
		if state.SourceState != nil {
			remoteState = &state.SourceState.VirtualMachineInstanceCommonMigrationState
		}
		connections = s.syncReceivingConnectionMap
	default:
		return nil, nil
	}
	address := migration.Annotations[virtv1.MigrationPeerSyncAddressAnnotation]
	if address == "" && remoteState != nil && remoteState.SyncAddress != nil {
		address = *remoteState.SyncAddress
	}
	if address == "" {
		return nil, nil
	}
	return s.getOutboundConnection(vmi, localState.MigrationUID, address, connections)
}

func (s *SynchronizationController) getOutboundConnection(vmi *virtv1.VirtualMachineInstance, migrationUID types.UID, syncAddress string, connectionMap *sync.Map) (*SynchronizationConnection, error) {
	if migrationUID == "" {
		return nil, nil
	}
	migrationID, err := s.getMigrationIDFromUID(migrationUID)
	if err != nil {
		return nil, err
	}
	log.Log.Object(vmi).V(4).Infof("found migration ID %s", migrationID)
	obj, ok := connectionMap.Load(migrationID)
	if ok {
		current, valid := obj.(*SynchronizationConnection)
		if valid && current.grpcClientConnection != nil && current.grpcClientConnection.Target() != syncAddress {
			if err := current.Close(); err != nil {
				return nil, err
			}
			connectionMap.Delete(migrationID)
			ok = false
		}
	}
	if !ok {
		if s.peerSyncPin(migrationID) == "" {
			log.Log.Object(vmi).V(4).Infof("the pin of the peer synchronization controller for migration %s is not known yet", migrationID)
			return nil, nil
		}
		id, err := s.identity(migrationID)
		if err != nil {
			return nil, err
		}
		grpcClientConnection, err := s.createOutboundConnection(syncAddress, id.ClientConfig(func() string { return s.peerSyncPin(migrationID) }, migrationID))
		if err != nil {
			return nil, err
		}
		conn := &SynchronizationConnection{
			migrationID:          migrationID,
			grpcClientConnection: grpcClientConnection,
		}
		connectionMap.Store(migrationID, conn)
		return conn, nil
	}
	outboundSyncConnection, ok := obj.(*SynchronizationConnection)
	if !ok {
		return nil, fmt.Errorf("found unknown object in outbound connection cache %#v", outboundSyncConnection)
	}
	return outboundSyncConnection, nil
}

func (s *SynchronizationController) handleSourceState(vmi *virtv1.VirtualMachineInstance, migration *virtv1.VirtualMachineInstanceMigration) error {
	var outboundConnection *SynchronizationConnection
	var err error
	if vmi.Status.MigrationState == nil {
		// No migration state, don't do anything
		return nil
	}
	if vmi.Status.MigrationState.SourceState == nil || vmi.Status.MigrationState.SourceState.MigrationUID == "" {
		log.Log.Object(vmi).V(2).Infof("source: no source state yet for migration %s", migration.Spec.SendTo.MigrationID)
		return nil
	}
	// The own pin goes out before anything of the peer is known: the peer waits for it to start.
	sourceState := vmi.Status.MigrationState.SourceState
	err = s.publishSyncEndpoint(vmi, migration.Spec.SendTo.MigrationID, migration.UID,
		&sourceState.VirtualMachineInstanceCommonMigrationState)
	if err != nil {
		return err
	}
	if vmi.Status.MigrationState.TargetState == nil {
		log.Log.Object(vmi).V(2).Infof("source: waiting for the target state of migration %s", migration.Spec.SendTo.MigrationID)
		return nil
	}
	if sourceState.SyncAddress == nil || *sourceState.SyncAddress == "" {
		syncAddress, err := s.getLocalSynchronizationAddress()
		if err != nil {
			return err
		}
		sourceState.SyncAddress = &syncAddress
	}
	targetState := vmi.Status.MigrationState.TargetState
	if (targetState.SyncAddress != nil || migration.Annotations[virtv1.MigrationPeerSyncAddressAnnotation] != "") && sourceState.MigrationUID != "" {
		if outboundConnection, err = s.getOutboundMigrationConnection(vmi, vmi.Status.MigrationState, migration); err != nil {
			return err
		}
	}
	if outboundConnection == nil {
		log.Log.Object(vmi).V(4).Info("no synchronization connection found for source, doing nothing")
		return nil
	}
	vmiStatusJson, err := json.Marshal(vmi.Status)
	if err != nil {
		return err
	}
	client := syncv1.NewSynchronizeClient(outboundConnection.grpcClientConnection)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.timeout)*time.Second)
	defer cancel()

	if _, err := client.SyncSourceMigrationStatus(ctx, &syncv1.VMIStatusRequest{
		MigrationID: outboundConnection.migrationID,
		VmiStatus: &syncv1.VMIStatus{
			VmiStatusJson: vmiStatusJson,
		},
	}); err != nil {
		return err
	}
	if migration.IsFinal() {
		if migration.Spec.SendTo != nil {
			log.Log.Object(migration).Infof("completed migration for VMI %s/%s, closing outbound connections", migration.Namespace, migration.Spec.VMIName)
			s.closeConnectionForMigrationID(s.syncOutboundConnectionMap, migration.Spec.SendTo.MigrationID)
		}
	}

	return nil
}

func (s *SynchronizationController) handleTargetState(vmi *virtv1.VirtualMachineInstance, migration *virtv1.VirtualMachineInstanceMigration) error {
	if vmi.Status.MigrationState == nil {
		// No migration state, don't do anything
		return nil
	}
	if vmi.Status.MigrationState.TargetState == nil || vmi.Status.MigrationState.TargetState.MigrationUID == "" {
		log.Log.Object(vmi).V(2).Infof("target: no target state yet for migration %s", migration.Spec.Receive.MigrationID)
		return nil
	}
	// The own pin goes out before anything of the source is known: the source state arrives through
	// the synchronization, which the source starts only once it holds this pin.
	targetState := vmi.Status.MigrationState.TargetState
	if err := s.publishSyncEndpoint(vmi, migration.Spec.Receive.MigrationID, migration.UID,
		&targetState.VirtualMachineInstanceCommonMigrationState); err != nil {
		return err
	}
	// Endpoint publication has updated the API; final cleanup tests this snapshot.
	originalState := vmi.Status.MigrationState.DeepCopy()
	if vmi.Status.MigrationState.SourceState == nil {
		log.Log.Object(vmi).V(2).Infof("target: waiting for the source state of migration %s", migration.Spec.Receive.MigrationID)
		return nil
	}

	var outboundConnection *SynchronizationConnection
	var err error
	sourceState := vmi.Status.MigrationState.SourceState
	if targetState.SyncAddress == nil || *targetState.SyncAddress == "" {
		syncAddress, err := s.getLocalSynchronizationAddress()
		if err != nil {
			return err
		}
		targetState.SyncAddress = &syncAddress
	}

	if (sourceState.SyncAddress != nil || migration.Annotations[virtv1.MigrationPeerSyncAddressAnnotation] != "") && targetState.MigrationUID != "" {
		if outboundConnection, err = s.getOutboundMigrationConnection(vmi, vmi.Status.MigrationState, migration); err != nil {
			return err
		}
	}
	if outboundConnection == nil {
		log.Log.Object(vmi).V(4).Info("no synchronization connection found for target, doing nothing")
		return nil
	}

	vmiStatusJson, err := json.Marshal(vmi.Status)
	if err != nil {
		return err
	}
	client := syncv1.NewSynchronizeClient(outboundConnection.grpcClientConnection)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.timeout)*time.Second)
	defer cancel()

	_, err = client.SyncTargetMigrationStatus(ctx, &syncv1.VMIStatusRequest{
		MigrationID: outboundConnection.migrationID,
		VmiStatus: &syncv1.VMIStatus{
			VmiStatusJson: vmiStatusJson,
		},
	})
	if err != nil {
		return err
	}
	if migration.IsFinal() {
		if migration.Spec.Receive != nil {
			log.Log.Object(migration).Infof("completed migration for VMI %s/%s, closing receiving connections", migration.Namespace, migration.Spec.VMIName)
			s.closeConnectionForMigrationID(s.syncReceivingConnectionMap, migration.Spec.Receive.MigrationID)
		}
	}

	// Only a successful final publication may discard the peer identity. The
	// target handler must finish its own cleanup first (it removes the annotation).
	if migration.IsFinal() && vmi.Status.MigrationState.Completed && !vmi.Status.MigrationState.Failed && !vmi.IsMigrationTarget() {
		payload, err := patch.New(
			patch.WithTest("/status/migrationState", originalState),
			patch.WithRemove("/status/migrationState/sourceState"),
			patch.WithRemove("/status/migrationState/targetState"),
		).GeneratePayload()
		if err != nil {
			return err
		}
		_, err = s.client.VirtualMachineInstance(vmi.Namespace).Patch(ctx, vmi.Name, types.JSONPatchType, payload, metav1.PatchOptions{})
		return err
	}
	return nil
}

func (s *SynchronizationController) getMigrationForVMI(vmi *virtv1.VirtualMachineInstance) (*virtv1.VirtualMachineInstanceMigration, error) {
	objects, err := s.migrationInformer.GetIndexer().ByIndex("byActiveVMIName", vmi.Name)
	if err != nil {
		return nil, err
	}
	if len(objects) > 0 {
		count := 0
		var res *virtv1.VirtualMachineInstanceMigration
		for _, migrationObj := range objects {
			migration, ok := migrationObj.(*virtv1.VirtualMachineInstanceMigration)
			if !ok {
				return nil, fmt.Errorf("not a virtual machine instance migration")
			}
			if migration.Namespace == vmi.Namespace {
				if migration.IsDecentralizedSource() {
					if vmi.Status.MigrationState != nil && vmi.Status.MigrationState.SourceState != nil && migration.UID == vmi.Status.MigrationState.SourceState.MigrationUID {
						count++
						res = migration
					}
				} else if migration.IsDecentralizedTarget() {
					if vmi.Status.MigrationState != nil && vmi.Status.MigrationState.TargetState != nil && migration.UID == vmi.Status.MigrationState.TargetState.MigrationUID {
						count++
						res = migration
					}
				}
			}
		}
		if count > 1 {
			return nil, fmt.Errorf("found more than one migration pointing to same VMI")
		} else if count == 0 {
			return nil, nil
		}
		return res, nil
	}
	return nil, nil
}

func (s *SynchronizationController) rebuildConnectionsAndUpdateSyncAddress() error {
	// Go and find all active migration resources, if they are decentralized rebuild either
	// the incoming or outbound connections, and call sync to update the remote with the new
	// address.
	objs := s.migrationInformer.GetStore().List()
	log.Log.V(4).Infof("rebuilding any connections, and updating remote VMIs, found %d migrations", len(objs))
	for _, obj := range objs {
		migration, ok := obj.(*virtv1.VirtualMachineInstanceMigration)
		if !ok {
			return fmt.Errorf("unknown object in migration store %v", obj)
		}
		if isOnGoingMigration(migration) {
			vmi, err := s.getVMIFromMigration(migration)
			if err != nil {
				s.requeueAfterRebuildError(migration, err)
				continue
			}
			if vmi == nil {
				// No VMI found, can't update it, so skip it.
				continue
			}
			// ongoing migration.
			if migration.Spec.Receive != nil {
				// We are the target
				log.Log.Object(migration).Object(vmi).Info("found ongoing target migration for vmi, rebuilding connection")
				if err := s.rebuildTargetConnection(migration, vmi); err != nil {
					s.requeueAfterRebuildError(migration, err)
				}
			} else if migration.Spec.SendTo != nil {
				// We are the source
				log.Log.Object(migration).Object(vmi).Info("found ongoing source migration for vmi, rebuilding connection")
				if err := s.rebuildSourceConnection(migration, vmi); err != nil {
					s.requeueAfterRebuildError(migration, err)
				}
			}
		}
	}
	return nil
}

func (s *SynchronizationController) requeueAfterRebuildError(migration *virtv1.VirtualMachineInstanceMigration, err error) {
	log.Log.Object(migration).Reason(err).Error("unable to restore the synchronization connection; retrying")
	s.queue.AddRateLimited(controller.NamespacedKey(migration.Namespace, migration.Spec.VMIName))
}

func isOnGoingMigration(migration *virtv1.VirtualMachineInstanceMigration) bool {
	return migration.IsDecentralized() && migration.Status.Phase != virtv1.MigrationFailed && migration.Status.Phase != virtv1.MigrationSucceeded
}

func (s *SynchronizationController) rebuildTargetConnection(migration *virtv1.VirtualMachineInstanceMigration, vmi *virtv1.VirtualMachineInstance) error {
	if vmi.Status.MigrationState != nil && vmi.Status.MigrationState.TargetState != nil {
		if err := s.publishSyncEndpoint(vmi, migration.Spec.Receive.MigrationID, migration.UID,
			&vmi.Status.MigrationState.TargetState.VirtualMachineInstanceCommonMigrationState); err != nil {
			return err
		}
	}
	conn, err := s.getOutboundMigrationConnection(vmi, vmi.Status.MigrationState, migration)
	if err != nil {
		return err
	}
	if conn != nil {
		s.syncReceivingConnectionMap.Store(migration.Spec.Receive.MigrationID, conn)
	}
	return nil
}

func (s *SynchronizationController) rebuildSourceConnection(migration *virtv1.VirtualMachineInstanceMigration, vmi *virtv1.VirtualMachineInstance) error {
	if vmi.Status.MigrationState != nil && vmi.Status.MigrationState.SourceState != nil {
		if err := s.publishSyncEndpoint(vmi, migration.Spec.SendTo.MigrationID, migration.UID,
			&vmi.Status.MigrationState.SourceState.VirtualMachineInstanceCommonMigrationState); err != nil {
			return err
		}
	}
	conn, err := s.getOutboundMigrationConnection(vmi, vmi.Status.MigrationState, migration)
	if err != nil {
		return err
	}
	if conn != nil {
		s.syncOutboundConnectionMap.Store(migration.Spec.SendTo.MigrationID, conn)
	}
	return nil
}

func (s *SynchronizationController) getVMIFromMigration(migration *virtv1.VirtualMachineInstanceMigration) (*virtv1.VirtualMachineInstance, error) {
	key := controller.NamespacedKey(migration.Namespace, migration.Spec.VMIName)
	obj, exists, err := s.vmiInformer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return obj.(*virtv1.VirtualMachineInstance).DeepCopy(), nil
}

func (s *SynchronizationController) getLocalSynchronizationAddress() (string, error) {
	if s.ip != "" {
		return fmt.Sprintf("%s:%d", s.ip, s.bindPort), nil
	}
	// TODO figure out how to get my URL with or without submariner (url changes based on export)
	return s.listener.Addr().String(), nil
}

func (s *SynchronizationController) createOutboundConnection(connectionURL string, tlsConfig *tls.Config) (*grpc.ClientConn, error) {
	logger := log.Log.With("outbound", connectionURL)
	logger.Info("creating new synchronization grpc connection")

	client, err := grpc.NewClient(connectionURL, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	return client, err
}

func (s *SynchronizationController) createTcpListener() (net.Listener, error) {
	if s.listener != nil {
		return s.listener, nil
	}
	var ln net.Listener
	var err error
	addr := net.JoinHostPort(s.bindAddress, strconv.Itoa(s.bindPort))
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		log.Log.Reason(err).Error("failed to create tcp listener")
		return nil, err
	}
	s.listener = ln
	return ln, nil
}

func (s *SynchronizationController) findTargetMigrationFromMigrationID(migrationID string) (*virtv1.VirtualMachineInstanceMigration, error) {
	return s.findMigrationFromMigrationIDByIndex("byTargetMigrationID", migrationID)
}

func (s *SynchronizationController) findSourceMigrationFromMigrationID(migrationID string) (*virtv1.VirtualMachineInstanceMigration, error) {
	return s.findMigrationFromMigrationIDByIndex("bySourceMigrationID", migrationID)
}

func (s *SynchronizationController) findMigrationFromMigrationIDByIndex(indexName, migrationID string) (*virtv1.VirtualMachineInstanceMigration, error) {
	objs, err := s.migrationInformer.GetIndexer().ByIndex(indexName, migrationID)
	if err != nil {
		return nil, err
	}
	if len(objs) > 1 {
		log.Log.Warningf("found multiple migrations for migrationID %s, picking first one", migrationID)
	}
	for _, obj := range objs {
		migration, _ := obj.(*virtv1.VirtualMachineInstanceMigration)
		return migration, nil
	}
	return nil, nil
}

func (s *SynchronizationController) SyncSourceMigrationStatus(ctx context.Context, request *syncv1.VMIStatusRequest) (*syncv1.VMIStatusResponse, error) {
	if err := authorizeMigration(ctx, request.MigrationID); err != nil {
		return nil, err
	}
	if request.VmiStatus == nil || len(request.VmiStatus.VmiStatusJson) == 0 {
		return &syncv1.VMIStatusResponse{
			Message: noSourceStatusErrorMsg,
		}, fmt.Errorf(noSourceStatusErrorMsg)
	}
	migration, err := s.findTargetMigrationFromMigrationID(request.MigrationID)
	if migration == nil {
		return &syncv1.VMIStatusResponse{
			Message: fmt.Sprintf(sourceUnableToLocateVMIMigrationIDErrorMsg, request.MigrationID),
		}, fmt.Errorf(sourceUnableToLocateVMIMigrationIDErrorMsg, request.MigrationID)
	}
	key := controller.NamespacedKey(migration.Namespace, migration.Spec.VMIName)
	log.Log.Object(migration).V(5).Infof("looking up VMI %s", key)
	obj, exists, err := s.vmiInformer.GetStore().GetByKey(key)
	if err != nil || !exists {
		if err == nil {
			err = fmt.Errorf(sourceUnableToLocateVMIMigrationIDErrorMsgVMI, request.MigrationID, key)
		}
		return &syncv1.VMIStatusResponse{
			Message: fmt.Sprintf(sourceUnableToLocateVMIMigrationIDErrorMsgVMI, request.MigrationID, key),
		}, err
	}
	vmi := obj.(*virtv1.VirtualMachineInstance)

	remoteStatus := &virtv1.VirtualMachineInstanceStatus{}
	if err := json.Unmarshal(request.VmiStatus.VmiStatusJson, remoteStatus); err != nil {
		return &syncv1.VMIStatusResponse{
			Message: fmt.Sprintf("unable to unmarshal vmistatus for migrationID %s", request.MigrationID),
		}, err
	}
	if remoteStatus.MigrationState == nil || remoteStatus.MigrationState.SourceState == nil {
		return &syncv1.VMIStatusResponse{
			Message: noSourceStatusErrorMsg,
		}, fmt.Errorf(noSourceStatusErrorMsg)
	}
	newVMI := vmi.DeepCopy()
	if newVMI.Status.MigrationState == nil {
		newVMI.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{}
	}

	// Only update SourceState if this migration is still active and matches the VMI's current migration.
	// This prevents stale updates from a completed decentralized migration from interfering with a new compute migration.
	if migration.IsFinal() {
		log.Log.Object(migration).Infof("Migration is final, ignoring source state update for VMI %s/%s", vmi.Namespace, vmi.Name)
		return &syncv1.VMIStatusResponse{
			Message: successMessage,
		}, nil
	}

	// Check if the VMI's current migration matches this migration
	if newVMI.Status.MigrationState.MigrationUID != "" && newVMI.Status.MigrationState.MigrationUID != migration.UID {
		log.Log.Object(migration).Warningf("VMI %s/%s has different migration UID %s, ignoring source state update for migration %s",
			vmi.Namespace, vmi.Name, newVMI.Status.MigrationState.MigrationUID, migration.UID)
		return &syncv1.VMIStatusResponse{
			Message: successMessage,
		}, nil
	}

	log.Log.Object(newVMI).V(5).Infof("vmi migration source state: %#v", newVMI.Status.MigrationState.SourceState)
	log.Log.Object(newVMI).V(5).Infof("remote migration source state: %#v", remoteStatus.MigrationState.SourceState)
	newVMI.Status.MigrationState.SourceState = remoteStatus.MigrationState.SourceState.DeepCopy()
	copyLegacySourceFields(newVMI, remoteStatus.MigrationState)
	newVMI.Status.MigratedVolumes = remoteStatus.MigratedVolumes
	newVMI.Status.MigrationMethod = remoteStatus.MigrationMethod
	if !apiequality.Semantic.DeepEqual(vmi.Status, newVMI.Status) {
		if err := s.patchVMI(ctx, vmi, newVMI); err != nil {
			return &syncv1.VMIStatusResponse{
				Message: fmt.Sprintf("unable to synchronize VMI for migrationID %s", request.MigrationID),
			}, err
		}
		log.Log.Object(newVMI).With("MigrationID", request.MigrationID).V(5).Info("successfully patched VMI with source state")
	}
	log.Log.Object(newVMI).V(5).Info("returning success to grpc caller, source")
	return &syncv1.VMIStatusResponse{
		Message: successMessage,
	}, nil
}

func (s *SynchronizationController) SyncTargetMigrationStatus(ctx context.Context, request *syncv1.VMIStatusRequest) (*syncv1.VMIStatusResponse, error) {
	if err := authorizeMigration(ctx, request.MigrationID); err != nil {
		return nil, err
	}
	if request.VmiStatus == nil || len(request.VmiStatus.VmiStatusJson) == 0 {
		return &syncv1.VMIStatusResponse{
			Message: noTargetStatusErrorMsg,
		}, fmt.Errorf(noTargetStatusErrorMsg)
	}

	migration, err := s.findSourceMigrationFromMigrationID(request.MigrationID)
	if migration == nil {
		return &syncv1.VMIStatusResponse{
			Message: fmt.Sprintf(targetUnableToLocateVMIMigrationIDErrorMsg, request.MigrationID),
		}, fmt.Errorf(targetUnableToLocateVMIMigrationIDErrorMsg, request.MigrationID)
	}

	key := controller.NamespacedKey(migration.Namespace, migration.Spec.VMIName)
	obj, exists, err := s.vmiInformer.GetStore().GetByKey(key)
	if err != nil || !exists {
		if err == nil {
			err = fmt.Errorf(targetUnableToLocateVMIMigrationIDErrorMsgVMI, request.MigrationID, key)
		}
		return &syncv1.VMIStatusResponse{
			Message: fmt.Sprintf(targetUnableToLocateVMIMigrationIDErrorMsgVMI, request.MigrationID, key),
		}, err
	}
	vmi := obj.(*virtv1.VirtualMachineInstance)
	remoteStatus := &virtv1.VirtualMachineInstanceStatus{}
	if err := json.Unmarshal(request.VmiStatus.VmiStatusJson, remoteStatus); err != nil {
		return &syncv1.VMIStatusResponse{
			Message: fmt.Sprintf("unable to unmarshal vmistatus for migrationID %s", request.MigrationID),
		}, err
	}
	if remoteStatus.MigrationState == nil || remoteStatus.MigrationState.TargetState == nil {
		return &syncv1.VMIStatusResponse{
			Message: noTargetStatusErrorMsg,
		}, fmt.Errorf(noTargetStatusErrorMsg)
	}
	newVMI := vmi.DeepCopy()
	if newVMI.Status.MigrationState == nil {
		newVMI.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{}
	}

	// Only update TargetState if this migration is still active and matches the VMI's current migration.
	// This prevents stale updates from a completed decentralized migration from interfering with a new compute migration.
	if migration.IsFinal() {
		log.Log.Object(migration).Infof("Migration is final, ignoring target state update for VMI %s/%s", vmi.Namespace, vmi.Name)
		return &syncv1.VMIStatusResponse{
			Message: successMessage,
		}, nil
	}

	// Check if the VMI's current migration matches this migration
	if newVMI.Status.MigrationState.MigrationUID != "" && newVMI.Status.MigrationState.MigrationUID != migration.UID {
		log.Log.Object(migration).Warningf("VMI %s/%s has different migration UID %s, ignoring target state update for migration %s",
			vmi.Namespace, vmi.Name, newVMI.Status.MigrationState.MigrationUID, migration.UID)
		return &syncv1.VMIStatusResponse{
			Message: successMessage,
		}, nil
	}

	log.Log.Object(newVMI).V(5).Infof("vmi migration target state: %#v", newVMI.Status.MigrationState.TargetState)
	log.Log.Object(newVMI).V(5).Infof("remote migration target state: %#v", remoteStatus.MigrationState.TargetState)
	newVMI.Status.MigrationState.TargetState = remoteStatus.MigrationState.TargetState.DeepCopy()
	copyLegacyTargetFields(newVMI, remoteStatus.MigrationState)
	if !apiequality.Semantic.DeepEqual(vmi.Status.MigrationState, newVMI.Status.MigrationState) {
		if err := s.patchVMI(ctx, vmi, newVMI); err != nil {
			return &syncv1.VMIStatusResponse{
				Message: fmt.Sprintf("unable to synchronize VMI for migrationID %s", request.MigrationID),
			}, err
		}
		log.Log.Object(newVMI).With("MigrationID", request.MigrationID).V(5).Info("successfully patched VMI with target state")
	}
	log.Log.Object(newVMI).V(5).Info("returning success to grpc caller, target")
	return &syncv1.VMIStatusResponse{
		Message: successMessage,
	}, nil
}

func (s *SynchronizationController) patchVMI(ctx context.Context, origVMI, newVMI *virtv1.VirtualMachineInstance) error {
	if origVMI.Status.MigrationState != nil &&
		origVMI.Status.MigrationState.Completed {
		log.Log.Object(origVMI).V(3).Infof("VMI is completed, skipping patch")
		return nil
	}
	patchSet := patch.New()

	if !apiequality.Semantic.DeepEqual(origVMI.Labels, newVMI.Labels) {
		if len(origVMI.Labels) == 0 {
			patchSet.AddOption(
				patch.WithAdd("/metadata/labels", newVMI.Labels))
		} else {
			patchSet.AddOption(
				patch.WithTest("/metadata/labels", origVMI.Labels),
				patch.WithReplace("/metadata/labels", newVMI.Labels),
			)
		}
	}

	if !apiequality.Semantic.DeepEqual(origVMI.Status.MigrationMethod, newVMI.Status.MigrationMethod) {
		if origVMI.Status.MigrationMethod == "" {
			patchSet.AddOption(
				patch.WithAdd("/status/migrationMethod", newVMI.Status.MigrationMethod))
		} else {
			patchSet.AddOption(
				patch.WithTest("/status/migrationMethod", origVMI.Status.MigrationMethod),
				patch.WithReplace("/status/migrationMethod", newVMI.Status.MigrationMethod),
			)
		}
	}

	if !apiequality.Semantic.DeepEqual(origVMI.Status.MigratedVolumes, newVMI.Status.MigratedVolumes) {
		if origVMI.Status.MigratedVolumes == nil {
			patchSet.AddOption(
				patch.WithAdd("/status/migratedVolumes", newVMI.Status.MigratedVolumes))
		} else {
			patchSet.AddOption(
				patch.WithTest("/status/migratedVolumes", origVMI.Status.MigratedVolumes),
				patch.WithReplace("/status/migratedVolumes", newVMI.Status.MigratedVolumes),
			)
		}
	}

	if !apiequality.Semantic.DeepEqual(origVMI.Status.MigrationState, newVMI.Status.MigrationState) {
		if origVMI.Status.MigrationState == nil {
			patchSet.AddOption(
				patch.WithAdd("/status/migrationState", newVMI.Status.MigrationState))
		} else {
			addMigrationStateFieldPatches(patchSet, origVMI.Status.MigrationState, newVMI.Status.MigrationState)
		}
	}

	if !patchSet.IsEmpty() {
		patchBytes, err := patchSet.GeneratePayload()
		if err != nil {
			return err
		}
		log.Log.Object(origVMI).V(5).Infof("patch VMI with %s", string(patchBytes))
		if _, err := s.client.VirtualMachineInstance(origVMI.Namespace).Patch(ctx, origVMI.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// addMigrationStateFieldPatches generates individual JSON Patch
// operations for each changed field in MigrationState, rather than a
// single test+replace of the entire object. This prevents spurious
// patch failures when another controller concurrently modifies
// unrelated MigrationState fields (e.g. the virt-handler setting
// ports while the sync controller sets SourceState).
func addMigrationStateFieldPatches(patchSet *patch.PatchSet, origMS, newMS *virtv1.VirtualMachineInstanceMigrationState) {
	if apiequality.Semantic.DeepEqual(origMS, newMS) {
		return
	}
	if newMS == nil {
		patchSet.AddOption(
			patch.WithTest("/status/migrationState", origMS),
			patch.WithRemove("/status/migrationState"),
		)
		return
	}

	// Reject a patch if a new migration took over after the informer snapshot.
	if origMS.MigrationUID != "" && origMS.MigrationUID == newMS.MigrationUID {
		patchSet.AddOption(patch.WithTest("/status/migrationState/migrationUid", origMS.MigrationUID))
	} else {
		// Early receives have only a nested UID. Test the snapshot so that a
		// subsequently installed migration cannot inherit this stale patch.
		patchSet.AddOption(patch.WithTest("/status/migrationState", origMS))
	}

	patchMigrationStateField(patchSet, "startTimestamp", origMS.StartTimestamp, newMS.StartTimestamp)
	patchMigrationStateField(patchSet, "endTimestamp", origMS.EndTimestamp, newMS.EndTimestamp)
	patchMigrationStateField(patchSet, "targetNodeDomainReadyTimestamp",
		origMS.TargetNodeDomainReadyTimestamp, newMS.TargetNodeDomainReadyTimestamp,
	)
	patchMigrationStateField(patchSet, "targetNodeDomainDetected", origMS.TargetNodeDomainDetected, newMS.TargetNodeDomainDetected)
	patchMigrationStateField(patchSet, "targetNodeAddress", origMS.TargetNodeAddress, newMS.TargetNodeAddress)
	patchMigrationStateField(patchSet, "targetDirectMigrationNodePorts",
		origMS.TargetDirectMigrationNodePorts, newMS.TargetDirectMigrationNodePorts,
	)
	patchMigrationStateField(patchSet, "targetNode", origMS.TargetNode, newMS.TargetNode)
	patchMigrationStateField(patchSet, "targetPod", origMS.TargetPod, newMS.TargetPod)
	patchMigrationStateField(patchSet, "targetAttachmentPodUID", origMS.TargetAttachmentPodUID, newMS.TargetAttachmentPodUID)
	patchMigrationStateField(patchSet, "sourceNode", origMS.SourceNode, newMS.SourceNode)
	patchMigrationStateField(patchSet, "sourcePod", origMS.SourcePod, newMS.SourcePod)
	patchMigrationStateField(patchSet, "completed", origMS.Completed, newMS.Completed)
	patchMigrationStateField(patchSet, "failed", origMS.Failed, newMS.Failed)
	patchMigrationStateField(patchSet, "abortRequested", origMS.AbortRequested, newMS.AbortRequested)
	patchMigrationStateField(patchSet, "abortStatus", origMS.AbortStatus, newMS.AbortStatus)
	patchMigrationStateField(patchSet, "failureReason", origMS.FailureReason, newMS.FailureReason)
	patchMigrationStateField(patchSet, "migrationUid", origMS.MigrationUID, newMS.MigrationUID)
	patchMigrationStateField(patchSet, "mode", origMS.Mode, newMS.Mode)
	patchMigrationStateField(patchSet, "migrationPolicyName", origMS.MigrationPolicyName, newMS.MigrationPolicyName)
	patchMigrationStateField(patchSet, "migrationConfiguration", origMS.MigrationConfiguration, newMS.MigrationConfiguration)
	patchMigrationStateField(patchSet, "targetCPUSet", origMS.TargetCPUSet, newMS.TargetCPUSet)
	patchMigrationStateField(patchSet, "targetNodeTopology", origMS.TargetNodeTopology, newMS.TargetNodeTopology)
	patchMigrationStateField(patchSet, "sourcePersistentStatePVCName", origMS.SourcePersistentStatePVCName, newMS.SourcePersistentStatePVCName)
	patchMigrationStateField(patchSet, "targetPersistentStatePVCName", origMS.TargetPersistentStatePVCName, newMS.TargetPersistentStatePVCName)
	patchMigrationStateField(patchSet, "sourceState", origMS.SourceState, newMS.SourceState)
	patchMigrationStateField(patchSet, "targetState", origMS.TargetState, newMS.TargetState)
	patchMigrationStateField(patchSet, "migrationNetworkType", origMS.MigrationNetworkType, newMS.MigrationNetworkType)
	// Preserve the additional migration state maintained by this fork.
	patchMigrationStateField(patchSet, "transferStatus", origMS.TransferStatus, newMS.TransferStatus)
	patchMigrationStateField(patchSet, "transferPermitRequired", origMS.TransferPermitRequired, newMS.TransferPermitRequired)
	patchMigrationStateField(patchSet, "transferPermitted", origMS.TransferPermitted, newMS.TransferPermitted)
	patchMigrationStateField(patchSet, "downtimeMilliseconds", origMS.DowntimeMilliseconds, newMS.DowntimeMilliseconds)
	patchMigrationStateField(patchSet, "downtimeWithoutNetworkMilliseconds",
		origMS.DowntimeWithoutNetworkMilliseconds, newMS.DowntimeWithoutNetworkMilliseconds,
	)
}

// patchMigrationStateField generates an add or test+replace JSON
// Patch operation for a single MigrationState field (all of which are
// omitempty). Unchanged fields are skipped. Zero originals use add
// (field absent from JSON), non-zero use test+replace for conflict
// detection.
func patchMigrationStateField[T any](patchSet *patch.PatchSet, field string, orig, changed T) {
	if apiequality.Semantic.DeepEqual(orig, changed) {
		return
	}
	path := "/status/migrationState/" + field
	var zero T
	if apiequality.Semantic.DeepEqual(orig, zero) {
		patchSet.AddOption(patch.WithAdd(path, changed))
	} else {
		patchSet.AddOption(
			patch.WithTest(path, orig),
			patch.WithReplace(path, changed),
		)
	}
}

func indexByMigrationUID(obj interface{}) ([]string, error) {
	migration, ok := obj.(*virtv1.VirtualMachineInstanceMigration)
	if !ok {
		return nil, nil
	}
	return []string{string(migration.UID)}, nil
}

func indexByActiveVmiName(obj interface{}) ([]string, error) {
	migration, ok := obj.(*virtv1.VirtualMachineInstanceMigration)
	if !ok {
		return nil, nil
	}
	return []string{migration.Spec.VMIName}, nil
}

func indexByTargetMigrationID(obj interface{}) ([]string, error) {
	migration, ok := obj.(*virtv1.VirtualMachineInstanceMigration)
	if !ok {
		return nil, nil
	}
	if migration.Spec.Receive != nil {
		return []string{migration.Spec.Receive.MigrationID}, nil
	}
	return []string{}, nil
}

func indexBySourceMigrationID(obj interface{}) ([]string, error) {
	migration, ok := obj.(*virtv1.VirtualMachineInstanceMigration)
	if !ok {
		return nil, nil
	}
	if migration.Spec.SendTo != nil {
		return []string{migration.Spec.SendTo.MigrationID}, nil
	}
	return []string{}, nil
}

func copyLegacyTargetFields(vmi *virtv1.VirtualMachineInstance, migrationState *virtv1.VirtualMachineInstanceMigrationState) {
	targetState := migrationState.TargetState
	vmi.Status.MigrationState.TargetNode = targetState.Node
	if targetState.AttachmentPodUID != nil {
		vmi.Status.MigrationState.TargetAttachmentPodUID = *targetState.AttachmentPodUID
	}
	vmi.Status.MigrationState.TargetCPUSet = targetState.CPUSet
	vmi.Status.MigrationState.TargetDirectMigrationNodePorts = targetState.DirectMigrationNodePorts
	if targetState.NodeAddress != nil {
		vmi.Status.MigrationState.TargetNodeAddress = *targetState.NodeAddress
	}
	vmi.Status.MigrationState.TargetNodeDomainDetected = targetState.DomainDetected
	vmi.Status.MigrationState.TargetNodeDomainReadyTimestamp = targetState.DomainReadyTimestamp
	if targetState.NodeTopology != nil {
		vmi.Status.MigrationState.TargetNodeTopology = *targetState.NodeTopology
	}
	if targetState.PersistentStatePVCName != nil {
		vmi.Status.MigrationState.TargetPersistentStatePVCName = *targetState.PersistentStatePVCName
	}
	vmi.Status.MigrationState.TargetPod = targetState.Pod
	copyCommonLegacyFields(vmi.Status.MigrationState, migrationState)
	// Completion and failure are final within one migration UID. A delayed target
	// update must not clear an outcome already established by the source controller.
	vmi.Status.MigrationState.Completed = vmi.Status.MigrationState.Completed || migrationState.Completed
	vmi.Status.MigrationState.Failed = vmi.Status.MigrationState.Failed || migrationState.Failed
}

func copyLegacySourceFields(vmi *virtv1.VirtualMachineInstance, migrationState *virtv1.VirtualMachineInstanceMigrationState) {
	vmi.Status.MigrationState.SourceNode = migrationState.SourceState.Node
	if migrationState.SourceState.PersistentStatePVCName != nil {
		vmi.Status.MigrationState.SourcePersistentStatePVCName = *migrationState.SourceState.PersistentStatePVCName
	}
	vmi.Status.MigrationState.SourcePod = migrationState.SourceState.Pod
	copyCommonLegacyFields(vmi.Status.MigrationState, migrationState)
	// A failure the source reports ends the migration on the target too. Without it the target sees the
	// end timestamp the source sets on the failure, takes the migration for finished and completes it.
	if migrationState.Failed {
		vmi.Status.MigrationState.Failed = true
		if vmi.Status.MigrationState.FailureReason == "" {
			vmi.Status.MigrationState.FailureReason = migrationState.FailureReason
		}
	}
}

func copyCommonLegacyFields(targetMigrationState, sourceMigrationState *virtv1.VirtualMachineInstanceMigrationState) {
	// Copy regular fields.
	if sourceMigrationState.MigrationPolicyName != nil {
		targetMigrationState.MigrationPolicyName = sourceMigrationState.MigrationPolicyName
	}
	if sourceMigrationState.MigrationConfiguration != nil {
		targetMigrationState.MigrationConfiguration = sourceMigrationState.MigrationConfiguration
	}
	if sourceMigrationState.StartTimestamp != nil {
		targetMigrationState.StartTimestamp = sourceMigrationState.StartTimestamp
	}
	if sourceMigrationState.EndTimestamp != nil {
		targetMigrationState.EndTimestamp = sourceMigrationState.EndTimestamp
	}
}

func (s *SynchronizationController) runConnectionCleanup() {
	s.failedCloseConnections.Range(func(k, v interface{}) bool {
		retryCount, ok := v.(int)
		if !ok {
			log.Log.Warningf("invalid retry count type during connection cleanup: %v", v)
			s.failedCloseConnections.Delete(k)
			return true
		}
		if retryCount >= maxCloseRetries {
			log.Log.Warningf("connection for migrationID %s failed to close after %d retries, not attempting to close again", k, retryCount)
			s.failedCloseConnections.Delete(k)
		}
		outboundConnection, ok := k.(*SynchronizationConnection)
		if !ok {
			log.Log.Warningf("invalid outbound connection type during connection cleanup: %v", k)
			s.failedCloseConnections.Delete(k)
			return true
		}
		if err := outboundConnection.Close(); err != nil {
			log.Log.Warningf("unable to close connection for migrationID, trying again: %s, %v", outboundConnection.migrationID, err)
			s.failedCloseConnections.Store(outboundConnection, retryCount+1)
		} else {
			s.failedCloseConnections.Delete(k)
		}
		return true
	})
}

func (s *SynchronizationController) CancelMigration(ctx context.Context, request *syncv1.MigrationCancelRequest) (*syncv1.MigrationCancelResponse, error) {
	authenticatedID, err := authenticatedMigrationID(ctx)
	if err != nil {
		return nil, err
	}
	migrationUID := request.MigrationUID

	migration, err := s.findMigrationFromMigrationIDByIndex("byUID", migrationUID)
	if err != nil {
		return &syncv1.MigrationCancelResponse{
			Message: fmt.Sprintf("unable to find migration to cancel for migrationUID %s", migrationUID),
		}, err
	}
	if migration == nil {
		return nil, status.Error(codes.NotFound, "migration not found")
	}
	if (migration.Spec.SendTo == nil && migration.Spec.Receive == nil) ||
		(migration.Spec.SendTo != nil && migration.Spec.SendTo.MigrationID != authenticatedID) ||
		(migration.Spec.Receive != nil && migration.Spec.Receive.MigrationID != authenticatedID) {
		return nil, status.Error(codes.PermissionDenied, "TLS migration identity does not match the migration to cancel")
	}
	log.Log.V(2).Object(migration).Infof("found migration to cancel for migrationUID %s", migrationUID)
	if err := s.client.VirtualMachineInstanceMigration(migration.Namespace).Delete(ctx, migration.Name, metav1.DeleteOptions{}); err != nil {
		return &syncv1.MigrationCancelResponse{
			Message: fmt.Sprintf("unable to cancel migration for migrationUID %s", migrationUID),
		}, err
	}
	log.Log.V(2).Object(migration).Infof("successfully deleted migration %s/%s for migrationUID %s", migration.Namespace, migration.Name, migrationUID)
	return &syncv1.MigrationCancelResponse{
		Message: "migration canceled",
	}, nil
}

func authenticatedMigrationID(ctx context.Context) (string, error) {
	remote, ok := peer.FromContext(ctx)
	if !ok || remote == nil {
		return "", status.Error(codes.Unauthenticated, "TLS migration identity is required")
	}
	info, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok || info.State.ServerName == "" {
		return "", status.Error(codes.Unauthenticated, "TLS migration identity is required")
	}
	return info.State.ServerName, nil
}

func authorizeMigration(ctx context.Context, migrationID string) error {
	authenticatedID, err := authenticatedMigrationID(ctx)
	if err != nil {
		return err
	}
	if migrationID != authenticatedID {
		return status.Error(codes.PermissionDenied, "TLS migration identity does not match the requested migration")
	}
	return nil
}
