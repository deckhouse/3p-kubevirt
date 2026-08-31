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

package rest

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"sync"

	"github.com/emicklei/go-restful/v3"
	"github.com/mdlayher/vsock"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/certificate"

	v1 "kubevirt.io/api/core/v1"
	kvcorev1 "kubevirt.io/client-go/kubevirt/typed/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/virt-handler/isolation"

	"github.com/gorilla/websocket"
)

type ConsoleHandler struct {
	podIsolationDetector isolation.PodIsolationDetector
	serialStopChans      map[types.UID]chan struct{}
	vncStopChans         map[types.UID]chan struct{}
	spiceSessions        map[types.UID]*spiceSession
	serialLock           *sync.Mutex
	vncLock              *sync.Mutex
	spiceLock            *sync.Mutex
	vmiStore             cache.Store
	usbredir             map[types.UID]UsbredirHandlerVMI
	usbredirLock         *sync.Mutex
	certManager          certificate.Manager
}

type UsbredirHandlerVMI struct {
	stopChans map[int]chan struct{}
}

// spiceSession is one SPICE client, not one connection. A client opens a channel per
// function - main, display, inputs, cursor, playback, record and one per usbredir slot -
// and every one of them is a separate connection to the same socket. They share a stop
// channel so that evicting the client tears down the whole set at once, and refs counts
// how many are still open: the session is gone when the last channel closes, not the first.
type spiceSession struct {
	stopCh chan struct{}
	refs   int
}

const (
	// SpiceLinkHeader: magic, major, minor, size. SpiceLinkMess follows, and its first
	// field is the connection id, which is all this code needs.
	spiceLinkMagic        = "REDQ"
	spiceLinkHeaderSize   = 16
	spiceConnectionIDSize = 4
	spiceLinkPrefixSize   = spiceLinkHeaderSize + spiceConnectionIDSize
)

func NewConsoleHandler(podIsolationDetector isolation.PodIsolationDetector, vmiStore cache.Store, certManager certificate.Manager) *ConsoleHandler {
	return &ConsoleHandler{
		podIsolationDetector: podIsolationDetector,
		serialStopChans:      make(map[types.UID]chan struct{}),
		vncStopChans:         make(map[types.UID]chan struct{}),
		spiceSessions:        make(map[types.UID]*spiceSession),
		serialLock:           &sync.Mutex{},
		vncLock:              &sync.Mutex{},
		spiceLock:            &sync.Mutex{},
		usbredirLock:         &sync.Mutex{},
		vmiStore:             vmiStore,
		usbredir:             make(map[types.UID]UsbredirHandlerVMI),
		certManager:          certManager,
	}
}

func (t *ConsoleHandler) USBRedirHandler(request *restful.Request, response *restful.Response) {
	vmi, code, err := getVMI(request, t.vmiStore)
	if err != nil || vmi == nil {
		log.Log.Reason(err).Error(failedRetrieveVMI)
		response.WriteError(code, err)
		return
	}

	uid := vmi.GetUID()
	stopChan := make(chan struct{})
	var slotId int
	var unixSocketPath string
	ok := func() bool {
		// For simplicity, we handle one usbredir request at the time, for all VMIs
		// handled by virt-handler
		t.usbredirLock.Lock()
		defer t.usbredirLock.Unlock()

		if _, exists := t.usbredir[uid]; !exists {
			// Initialize
			t.usbredir[uid] = UsbredirHandlerVMI{
				stopChans: make(map[int]chan struct{}),
			}
		}

		usbHandler := t.usbredir[uid]
		// Find the first USB device slot available
		for slotId = 0; slotId < v1.UsbClientPassthroughMaxNumberOf; slotId++ {
			if _, inUse := usbHandler.stopChans[slotId]; !inUse {
				break
			}
		}

		if slotId == v1.UsbClientPassthroughMaxNumberOf {
			log.Log.Object(vmi).Reason(err).Errorf("All USB devices are in use.")
			response.WriteError(http.StatusServiceUnavailable, err)
			return false
		}

		unixSocketPath, err = t.getUnixSocketPath(vmi, fmt.Sprintf("virt-usbredir-%d", slotId))
		if err != nil {
			log.Log.Object(vmi).Reason(err).Error("Failed on finding unix socket for USBRedir")
			response.WriteError(http.StatusBadRequest, err)
			return false
		}

		usbHandler.stopChans[slotId] = stopChan
		return true
	}()

	if !ok {
		return
	}

	defer func() {
		t.usbredirLock.Lock()
		defer t.usbredirLock.Unlock()
		usbHandler := t.usbredir[uid]
		delete(usbHandler.stopChans, slotId)
	}()
	t.stream(vmi, request, response, unixSocketDialer(vmi, unixSocketPath), stopChan)
}

func (t *ConsoleHandler) VNCHandler(request *restful.Request, response *restful.Response) {
	vmi, code, err := getVMI(request, t.vmiStore)
	if err != nil || vmi == nil {
		log.Log.Reason(err).Error(failedRetrieveVMI)
		response.WriteError(code, err)
		return
	}
	unixSocketPath, err := t.getUnixSocketPath(vmi, "virt-vnc")
	if err != nil {
		log.Log.Object(vmi).Reason(err).Error("Failed finding unix socket for VNC console")
		response.WriteError(http.StatusBadRequest, err)
		return
	}
	uid := vmi.GetUID()
	stopChn := newStopChan(uid, t.vncLock, t.vncStopChans)
	defer deleteStopChan(uid, stopChn, t.vncLock, t.vncStopChans)
	t.stream(vmi, request, response, unixSocketDialer(vmi, unixSocketPath), stopChn)
}

// spiceConnectionID reads the connection id out of the SPICE link message that opens
// every channel. Zero means the client is starting a session and does not know its id
// yet; the server assigns one on the main channel and the client repeats it on all the
// others. That is the only thing in the protocol that tells a new client apart from
// another channel of the client already connected.
func spiceConnectionID(prefix []byte) (uint32, bool) {
	if len(prefix) < spiceLinkPrefixSize || string(prefix[:len(spiceLinkMagic)]) != spiceLinkMagic {
		return 0, false
	}
	return binary.LittleEndian.Uint32(prefix[spiceLinkHeaderSize:spiceLinkPrefixSize]), true
}

// acquireSpiceSession hands out the stop channel this connection belongs to. A connection
// id of zero starts a session and evicts the one before it, closing every channel that
// client had open; any other id joins the session in progress. An unreadable link message
// is treated as a new session: a client that does not speak SPICE gets no free pass.
func (t *ConsoleHandler) acquireSpiceSession(uid types.UID, connectionID uint32, newSession bool) chan struct{} {
	t.spiceLock.Lock()
	defer t.spiceLock.Unlock()

	current, ok := t.spiceSessions[uid]
	if ok && !newSession && connectionID != 0 {
		current.refs++
		return current.stopCh
	}

	if ok {
		close(current.stopCh)
	}
	session := &spiceSession{stopCh: make(chan struct{}), refs: 1}
	t.spiceSessions[uid] = session
	return session.stopCh
}

// releaseSpiceSession drops one channel. The session outlives its channels closing one by
// one - a client reconnects usbredir slots while it works - so it is only forgotten once
// nothing is left of it, and never when it has already been replaced by a newer one.
func (t *ConsoleHandler) releaseSpiceSession(uid types.UID, stopCh chan struct{}) {
	t.spiceLock.Lock()
	defer t.spiceLock.Unlock()

	current, ok := t.spiceSessions[uid]
	if !ok || current.stopCh != stopCh {
		return
	}
	current.refs--
	if current.refs <= 0 {
		delete(t.spiceSessions, uid)
	}
}

func (t *ConsoleHandler) SPICEHandler(request *restful.Request, response *restful.Response) {
	vmi, code, err := getVMI(request, t.vmiStore)
	if err != nil || vmi == nil {
		log.Log.Reason(err).Error(failedRetrieveVMI)
		response.WriteError(code, err)
		return
	}
	unixSocketPath, err := t.getUnixSocketPath(vmi, "virt-spice")
	if err != nil {
		log.Log.Object(vmi).Reason(err).Error("Failed finding unix socket for SPICE console")
		response.WriteError(http.StatusBadRequest, err)
		return
	}
	// The display is exclusive the way VNC is, but the unit of exclusivity is the client,
	// not the connection: SPICE opens 4-11 of them per session. Evicting per connection,
	// the way newStopChan does in VNCHandler, would tear down channels of the very same
	// client. Which one it is, is read off the link message - see acquireSpiceSession.
	t.streamSPICE(vmi, request, response, unixSocketDialer(vmi, unixSocketPath))
}

// streamSPICE proxies one SPICE channel. It does what stream does, with one thing in
// between: the link message that opens the channel is read first, because the stop channel
// this connection gets depends on whether it starts a session or joins one. The bytes are
// handed over to the socket afterwards, so the server sees the stream it expects.
func (t *ConsoleHandler) streamSPICE(vmi *v1.VirtualMachineInstance, request *restful.Request, response *restful.Response, dial func() (net.Conn, error)) {
	var upgrader = kvcorev1.NewUpgrader()
	clientSocket, err := upgrader.Upgrade(response.ResponseWriter, request.Request, nil)
	if err != nil {
		log.Log.Object(vmi).Reason(err).Error("Failed to upgrade client websocket connection")
		response.WriteError(http.StatusInternalServerError, err)
		return
	}
	defer clientSocket.Close()

	conn, err := dial()
	if err != nil {
		response.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	prefix, err := readSpiceLinkPrefix(clientSocket)
	if err != nil {
		log.Log.Object(vmi).Reason(err).Error("Failed reading the SPICE link message")
		return
	}
	connectionID, parsed := spiceConnectionID(prefix)
	stopCh := t.acquireSpiceSession(vmi.GetUID(), connectionID, !parsed)
	defer t.releaseSpiceSession(vmi.GetUID(), stopCh)

	if _, err := conn.Write(prefix); err != nil {
		log.Log.Object(vmi).Reason(err).Error("Failed forwarding the SPICE link message")
		return
	}

	errCh := make(chan error, 2)
	go func() {
		_, err := kvcorev1.CopyTo(clientSocket, conn)
		errCh <- err
	}()
	go func() {
		_, err := kvcorev1.CopyFrom(conn, clientSocket)
		errCh <- err
	}()

	select {
	case <-stopCh:
		clientSocket.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "close by another connection"))
	case err := <-errCh:
		if err != nil && err != io.EOF {
			log.Log.Object(vmi).Reason(err).Error("Error in proxing websocket and unix socket")
			response.WriteHeader(http.StatusInternalServerError)
		}
	}
}

// readSpiceLinkPrefix collects the first bytes of a channel up to the connection id.
// A websocket frame boundary has nothing to do with the protocol's own framing, so the
// message can arrive split; whatever is read is returned in full and forwarded as is.
func readSpiceLinkPrefix(clientSocket *websocket.Conn) ([]byte, error) {
	var prefix []byte
	for len(prefix) < spiceLinkPrefixSize {
		_, data, err := clientSocket.ReadMessage()
		if err != nil {
			return prefix, err
		}
		prefix = append(prefix, data...)
	}
	return prefix, nil
}

func (t *ConsoleHandler) SerialHandler(request *restful.Request, response *restful.Response) {
	vmi, code, err := getVMI(request, t.vmiStore)
	if err != nil || vmi == nil {
		log.Log.Reason(err).Error(failedRetrieveVMI)
		response.WriteError(code, err)
		return
	}
	unixSocketPath, err := t.getUnixSocketPath(vmi, "virt-serial0")
	if err != nil {
		log.Log.Object(vmi).Reason(err).Error("Failed finding unix socket for serial console")
		response.WriteError(http.StatusBadRequest, err)
		return
	}
	uid := vmi.GetUID()
	stopCh := newStopChan(uid, t.serialLock, t.serialStopChans)
	defer deleteStopChan(uid, stopCh, t.serialLock, t.serialStopChans)
	t.stream(vmi, request, response, unixSocketDialer(vmi, unixSocketPath), stopCh)
}

func (t *ConsoleHandler) VSOCKHandler(request *restful.Request, response *restful.Response) {
	vmi, code, err := getVMI(request, t.vmiStore)
	if err != nil || vmi == nil {
		log.Log.Reason(err).Error(failedRetrieveVMI)
		response.WriteError(code, err)
		return
	}
	log.Log.Object(vmi).Info("In VSOCKHandler")
	if !util.IsAutoAttachVSOCK(vmi) {
		response.WriteError(http.StatusBadRequest, errors.New("VM doesn't have VSOCK enabled"))
		return
	}
	if vmi.Status.VSOCKCID == nil {
		// This should not happen.
		err := errors.New("VSOCK CID is nil")
		log.Log.Object(vmi).Error(err.Error())
		response.WriteError(http.StatusInternalServerError, err)
		return
	}
	portParam := request.QueryParameter("port")
	tlsParam := request.QueryParameter("tls")
	if tlsParam == "" {
		tlsParam = "false"
	}
	port, err := strconv.ParseUint(portParam, 10, 32)
	if err != nil {
		log.Log.Object(vmi).Reason(err).Errorf("Failed parsing the path parameter port %s", portParam)
		response.WriteError(http.StatusBadRequest, err)
		return
	}
	useTLS, err := strconv.ParseBool(tlsParam)
	if err != nil {
		log.Log.Object(vmi).Reason(err).Errorf("Failed parsing the path parameter useTLS %s", tlsParam)
		response.WriteError(http.StatusBadRequest, err)
		return
	}
	cid := *vmi.Status.VSOCKCID
	t.stream(vmi, request, response, func() (net.Conn, error) {
		log.Log.Object(vmi).Infof("Connecting to %d:%d", cid, port)
		conn, err := vsock.Dial(cid, uint32(port), &vsock.Config{})
		if err != nil {
			log.Log.Object(vmi).Reason(err).Errorf("failed to dial vsock %d:%d", cid, port)
			return nil, err
		}
		if !useTLS {
			log.Log.Object(vmi).Infof("Connected to %d:%d", cid, port)
			return conn, nil
		}
		tlsConn := tls.Client(conn, &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS13,
			GetClientCertificate: func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
				return t.certManager.Current(), nil
			},
		})
		if err := tlsConn.Handshake(); err != nil {
			log.Log.Object(vmi).Reason(err).Errorf("Failed to connect to %d:%d over TLS", cid, port)
			return nil, err
		}
		log.Log.Object(vmi).Infof("Connected to %d:%d over TLS", cid, port)
		return tlsConn, nil
	}, make(chan struct{})) // It is legitimate and up to the guest-application to accept multiple connections.
}

func newStopChan(uid types.UID, lock *sync.Mutex, stopChans map[types.UID]chan struct{}) chan struct{} {
	lock.Lock()
	defer lock.Unlock()
	// close current connection, if exists
	if c, ok := stopChans[uid]; ok {
		delete(stopChans, uid)
		close(c)
	}
	// create a stop channel for the new connection
	stopCh := make(chan struct{})
	stopChans[uid] = stopCh
	return stopCh
}

func deleteStopChan(uid types.UID, stopChn chan struct{}, lock *sync.Mutex, stopChans map[types.UID]chan struct{}) {
	lock.Lock()
	defer lock.Unlock()
	// delete the stop channel from the cache if needed
	if c, ok := stopChans[uid]; ok && c == stopChn {
		delete(stopChans, uid)
	}
}

func (t *ConsoleHandler) getUnixSocketPath(vmi *v1.VirtualMachineInstance, socketName string) (string, error) {
	result, err := t.podIsolationDetector.Detect(vmi)
	if err != nil {
		return "", err
	}
	socketDir := path.Join("/proc", strconv.Itoa(result.Pid()), "root", "var", "run", "kubevirt-private", string(vmi.GetUID()))
	socketPath := path.Join(socketDir, socketName)
	if _, err = os.Stat(socketPath); errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	return socketPath, nil
}

func unixSocketDialer(vmi *v1.VirtualMachineInstance, unixSocketPath string) func() (net.Conn, error) {
	return func() (net.Conn, error) {
		log.Log.Object(vmi).Infof("Connecting to %s", unixSocketPath)
		fd, err := net.Dial("unix", unixSocketPath)
		if err != nil {
			log.Log.Object(vmi).Reason(err).Errorf("failed to dial unix socket %s", unixSocketPath)
			return nil, err
		}
		log.Log.Object(vmi).Infof("Connected to %s", unixSocketPath)
		return fd, nil
	}
}

func (t *ConsoleHandler) stream(vmi *v1.VirtualMachineInstance, request *restful.Request, response *restful.Response, dial func() (net.Conn, error), stopCh chan struct{}) {
	var upgrader = kvcorev1.NewUpgrader()
	clientSocket, err := upgrader.Upgrade(response.ResponseWriter, request.Request, nil)
	if err != nil {
		log.Log.Object(vmi).Reason(err).Error("Failed to upgrade client websocket connection")
		response.WriteError(http.StatusInternalServerError, err)
		return
	}
	defer clientSocket.Close()

	log.Log.Object(vmi).Infof("Websocket connection upgraded")

	conn, err := dial()
	if err != nil {
		response.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	errCh := make(chan error, 2)
	go func() {
		_, err := kvcorev1.CopyTo(clientSocket, conn)
		log.Log.Object(vmi).Reason(err).Error("error encountered reading from unix socket")
		errCh <- err
	}()

	go func() {
		_, err := kvcorev1.CopyFrom(conn, clientSocket)
		log.Log.Object(vmi).Reason(err).Error("error encountered reading from client (virt-api) websocket")
		errCh <- err
	}()

	select {
	case <-stopCh:
		clientSocket.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "close by another connection"))
	case err := <-errCh:
		if err != nil && err != io.EOF {
			log.Log.Object(vmi).Reason(err).Error("Error in proxing websocket and unix socket")
			response.WriteHeader(http.StatusInternalServerError)
		}
	}
}
