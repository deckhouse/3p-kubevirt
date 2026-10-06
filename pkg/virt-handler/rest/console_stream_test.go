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
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/emicklei/go-restful/v3"
	"github.com/gorilla/websocket"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "kubevirt.io/api/core/v1"
)

// waitFor bounds every step of an eviction; nothing here goes near the kernel, so a step
// that takes this long is a hang, not a slow host.
const waitFor = 10 * time.Second

// pipeAddr is the address of an in-memory connection.
type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// pipeListener feeds an HTTP server the server ends of in-memory pipes.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// dial opens a pipe and queues its server end for Accept.
func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// streamServer serves websocket sessions over in-memory pipes and hands every request to
// whatever session the test installed last. Loopback TCP is deliberately avoided: a stream
// that saturates it can lose packets on a loaded host, and the retransmission backoff that
// follows stalls the client for seconds. A pipe has no kernel in the way.
type streamServer struct {
	server   *http.Server
	listener *pipeListener
	mu       sync.Mutex
	serve    func(request *restful.Request, response *restful.Response)
}

func newStreamServer() *streamServer {
	s := &streamServer{listener: newPipeListener()}
	s.server = &http.Server{Handler: s}
	go s.server.Serve(s.listener)
	return s
}

func (s *streamServer) Close() {
	s.server.Close()
}

func (s *streamServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	serve := s.serve
	s.mu.Unlock()
	serve(restful.NewRequest(r), restful.NewResponse(w))
}

func (s *streamServer) install(serve func(request *restful.Request, response *restful.Response)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serve = serve
}

// dialer returns a websocket dialer whose connections land in this server's accept queue.
func (s *streamServer) dialer() *websocket.Dialer {
	return &websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return s.listener.dial(ctx)
		},
	}
}

// busyGuest returns a dialer that hands the handler one end of a pipe which never stops
// sending, like a VNC server pushing frames of a changing screen. Whatever the handler
// writes back is swallowed, and both loops exit once the handler closes its end.
func busyGuest() func() (net.Conn, error) {
	return func() (net.Conn, error) {
		handlerEnd, guestEnd := net.Pipe()
		go io.Copy(io.Discard, guestEnd)
		go func() {
			defer guestEnd.Close()
			frame := make([]byte, 1024)
			for {
				if _, err := guestEnd.Write(frame); err != nil {
					return
				}
			}
		}()
		return handlerEnd, nil
	}
}

// client connects to the server and drains everything it sends. closed resolves with the
// error that ended the read loop.
func connectClient(server *streamServer, firstFrame chan<- struct{}, closed chan<- error) *websocket.Conn {
	ws, _, err := server.dialer().Dial("ws://pipe/", nil)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	go func() {
		first := true
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				closed <- err
				return
			}
			if first {
				first = false
				close(firstFrame)
			}
		}
	}()
	return ws
}

func expectClosedGoingAway(closed <-chan error) {
	var err error
	EventuallyWithOffset(1, closed, waitFor).Should(Receive(&err))
	var closeErr *websocket.CloseError
	ExpectWithOffset(1, errors.As(err, &closeErr)).To(BeTrue(), "the client should be told why it was disconnected, got: %v", err)
	ExpectWithOffset(1, closeErr.Code).To(Equal(websocket.CloseGoingAway))
}

var _ = Describe("Evicting a streaming session", func() {
	const evictions = 200

	var (
		server *streamServer
		vmi    *v1.VirtualMachineInstance
	)

	BeforeEach(func() {
		server = newStreamServer()
		vmi = &v1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "ns", UID: types.UID("uid")},
		}
	})

	AfterEach(func() {
		server.Close()
	})

	It("does not race the close frame against a frame in flight", func() {
		h := NewConsoleHandler(nil, nil, nil)

		for i := 0; i < evictions; i++ {
			stopCh := make(chan struct{})
			done := make(chan struct{})
			server.install(func(request *restful.Request, response *restful.Response) {
				defer close(done)
				h.stream(vmi, request, response, busyGuest(), stopCh)
			})

			firstFrame := make(chan struct{})
			closed := make(chan error, 1)
			ws := connectClient(server, firstFrame, closed)
			Eventually(firstFrame, waitFor).Should(BeClosed(), "the session should be streaming before it is evicted")

			close(stopCh)
			Eventually(done, waitFor).Should(BeClosed(), "the evicted handler should return")
			expectClosedGoingAway(closed)
			ws.Close()
		}
	})

	It("does not race the close frame against a SPICE frame in flight", func() {
		h := NewConsoleHandler(nil, nil, nil)

		for i := 0; i < evictions; i++ {
			done := make(chan struct{})
			server.install(func(request *restful.Request, response *restful.Response) {
				defer close(done)
				h.streamSPICE(vmi, request, response, busyGuest())
			})

			firstFrame := make(chan struct{})
			closed := make(chan error, 1)
			ws := connectClient(server, firstFrame, closed)
			Expect(ws.WriteMessage(websocket.BinaryMessage, spiceLink(0))).To(Succeed())
			Eventually(firstFrame, waitFor).Should(BeClosed(), "the session should be streaming before it is evicted")

			// Another client starting a session is what evicts this one.
			h.acquireSpiceSession(vmi.GetUID(), 0, false)
			Eventually(done, waitFor).Should(BeClosed(), "the evicted handler should return")
			expectClosedGoingAway(closed)
			ws.Close()
		}
	})
})
