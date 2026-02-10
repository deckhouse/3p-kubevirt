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
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"kubevirt.io/client-go/log"
)

const MaxInjectionTimeout = 200 * time.Millisecond

type InjectionState int

const (
	InjectionPending InjectionState = iota
	InjectionInProgress
	InjectionDone
	InjectionFailed
	InjectionTimedOut
)

type CTPayload struct {
	Data      []byte
	Version   byte
	SyncStart time.Time
}

type targetState struct {
	proxyListener  net.Listener
	hookListener   net.Listener
	injectionState InjectionState
	injectionStart time.Time
	injectionDone  *sync.Cond
	cancel         context.CancelFunc
}

type TargetHandler struct {
	ciliumClient ConntrackClient
	mu           sync.Mutex
	states       map[types.UID]*targetState
}

func NewTargetHandler(ciliumClient ConntrackClient) *TargetHandler {
	return &TargetHandler{
		ciliumClient: ciliumClient,
		states:       make(map[types.UID]*targetState),
	}
}

func (h *TargetHandler) StartProxyListener(vmiUID types.UID, socketPath string) error {
	h.mu.Lock()
	state := h.getOrCreateState(vmiUID)
	if state.proxyListener != nil {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}

	h.mu.Lock()
	state.proxyListener = listener
	h.mu.Unlock()

	go h.handleProxyConnections(vmiUID, listener)

	log.Log.V(3).Infof("Conntrack sync: started proxy listener at %s for VMI %s", socketPath, vmiUID)
	return nil
}

func (h *TargetHandler) handleProxyConnections(vmiUID types.UID, listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Log.Warningf("Conntrack sync: proxy accept error for VMI %s: %v", vmiUID, err)
			}
			return
		}

		go h.handleProxyConnection(vmiUID, conn)
	}
}

func (h *TargetHandler) handleProxyConnection(vmiUID types.UID, conn net.Conn) {
	defer conn.Close()

	msg, err := DecodeSyncMessage(conn)
	if err != nil {
		log.Log.Warningf("Conntrack sync: failed to decode message for VMI %s: %v", vmiUID, err)
		return
	}

	log.Log.V(3).Infof("Conntrack sync: received %d bytes for VMI %s", len(msg.Data), vmiUID)

	h.onCTReceived(vmiUID, &CTPayload{
		Data:      msg.Data,
		Version:   msg.Version,
		SyncStart: time.Unix(0, msg.Timestamp),
	})
}

func (h *TargetHandler) onCTReceived(vmiUID types.UID, payload *CTPayload) {
	h.mu.Lock()
	state := h.getOrCreateState(vmiUID)
	state.injectionStart = payload.SyncStart
	remaining := MaxInjectionTimeout - time.Since(payload.SyncStart)
	if remaining <= 0 {
		state.injectionState = InjectionTimedOut
		log.Log.Warningf("Conntrack sync: CT data arrived after timeout for VMI %s (elapsed: %v)", vmiUID, time.Since(payload.SyncStart))
		if state.injectionDone != nil {
			state.injectionDone.Broadcast()
		}
		h.mu.Unlock()
		return
	}

	state.injectionState = InjectionInProgress
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	state.cancel = cancel
	h.mu.Unlock()

	go func() {
		err := h.ciliumClient.ImportConntrack(ctx, payload.Data, payload.Version)

		h.mu.Lock()
		defer h.mu.Unlock()

		state := h.states[vmiUID]
		if state == nil {
			return
		}

		if state.injectionState == InjectionTimedOut {
			return
		}

		if ctx.Err() != nil {
			state.injectionState = InjectionTimedOut
			log.Log.Warningf("Conntrack sync: import timed out for VMI %s (elapsed: %v)", vmiUID, time.Since(state.injectionStart))
		} else if err != nil {
			state.injectionState = InjectionFailed
			log.Log.Warningf("Conntrack sync: import failed for VMI %s: %v", vmiUID, err)
		} else {
			state.injectionState = InjectionDone
			log.Log.V(3).Infof("Conntrack sync: import completed for VMI %s in %v", vmiUID, time.Since(state.injectionStart))
		}

		if state.injectionDone != nil {
			state.injectionDone.Broadcast()
		}
	}()
}

func (h *TargetHandler) StartHookListener(vmiUID types.UID, socketPath string) error {
	h.mu.Lock()
	state := h.getOrCreateState(vmiUID)
	if state.hookListener != nil {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}

	h.mu.Lock()
	state.hookListener = listener
	h.mu.Unlock()

	go h.handleHookConnections(vmiUID, listener)

	log.Log.V(3).Infof("Conntrack sync: started hook listener at %s for VMI %s", socketPath, vmiUID)
	return nil
}

func (h *TargetHandler) handleHookConnections(vmiUID types.UID, listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Log.Warningf("Conntrack sync: hook accept error for VMI %s: %v", vmiUID, err)
			}
			return
		}

		go h.handleHookConnection(vmiUID, conn)
	}
}

func (h *TargetHandler) handleHookConnection(vmiUID types.UID, conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "wait") {
			h.onHookSignal(vmiUID)
		}
	}

	conn.Write([]byte("ok\n"))
}

func (h *TargetHandler) onHookSignal(vmiUID types.UID) error {
	h.mu.Lock()

	state, exists := h.states[vmiUID]
	if !exists || state.injectionState == InjectionPending {
		h.mu.Unlock()
		return nil
	}

	if state.injectionState == InjectionDone ||
		state.injectionState == InjectionFailed ||
		state.injectionState == InjectionTimedOut {
		h.mu.Unlock()
		return nil
	}

	elapsed := time.Since(state.injectionStart)
	remaining := MaxInjectionTimeout - elapsed
	if remaining <= 0 {
		if state.cancel != nil {
			state.cancel()
		}
		h.mu.Unlock()
		return nil
	}

	cond := state.injectionDone
	if cond == nil {
		cond = sync.NewCond(&h.mu)
		state.injectionDone = cond
	}

	done := make(chan struct{})

	go func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for state.injectionState == InjectionInProgress {
			cond.Wait()
		}
		close(done)
	}()

	h.mu.Unlock()

	select {
	case <-done:
	case <-time.After(remaining):
		h.mu.Lock()
		if state.cancel != nil {
			state.cancel()
		}
		h.mu.Unlock()
		log.Log.Warningf("Conntrack sync: hook timeout for VMI %s", vmiUID)
	}

	return nil
}

func (h *TargetHandler) Cleanup(vmiUID types.UID) {
	h.mu.Lock()
	defer h.mu.Unlock()

	state, exists := h.states[vmiUID]
	if !exists {
		return
	}

	if state.proxyListener != nil {
		state.proxyListener.Close()
	}
	if state.hookListener != nil {
		state.hookListener.Close()
	}
	if state.cancel != nil {
		state.cancel()
	}

	delete(h.states, vmiUID)

	log.Log.V(3).Infof("Conntrack sync: cleaned up target state for VMI %s", vmiUID)
}

func (h *TargetHandler) getOrCreateState(vmiUID types.UID) *targetState {
	state, exists := h.states[vmiUID]
	if !exists {
		state = &targetState{
			injectionState: InjectionPending,
		}
		h.states[vmiUID] = state
	}
	return state
}
