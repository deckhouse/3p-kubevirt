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

package conntrack

import (
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1 "kubevirt.io/api/core/v1"
)

const (
	MaxInjectionTimeout = 200 * time.Millisecond
	ConntrackSyncPort   = 49154 // Proxy stream identifier (not actual TCP port)
)

type InjectionState int

const (
	InjectionPending InjectionState = iota
	InjectionInProgress
	InjectionDone
	InjectionFailed
	InjectionTimedOut
)

type CTPayload struct {
	Data    []byte
	Version byte
}

type SyncHandler interface {
	// Source side methods
	ExportAndSend(vmi *v1.VirtualMachineInstance, socketPath string) error
	HasSentCT(vmiUID types.UID) bool

	// Target side methods
	StartProxyListener(vmiUID types.UID, socketPath string) error
	StartHookListener(vmiUID types.UID, socketPath string) error
	OnHookSignal(vmiUID types.UID) error

	// Cleanup resets internal state for this VMI:
	// - Clears HasSentCT flag (enables retry)
	// - Stops proxy/hook listeners
	// Called when migration completes (success/failure/abort).
	// Cilium CT cleanup is automatic - not handled here.
	Cleanup(vmiUID types.UID)
}
