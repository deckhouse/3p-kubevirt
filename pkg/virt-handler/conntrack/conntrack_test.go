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
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("Conntrack Sync", func() {
	Describe("Protocol", func() {
		It("should encode and decode SyncMessage correctly", func() {
			ts := time.Now().UnixNano()
			original := &SyncMessage{
				Version:   2,
				Timestamp: ts,
				Data:      []byte("test conntrack data"),
			}

			encoded := original.Encode()
			Expect(encoded).To(HaveLen(1 + 8 + 4 + len(original.Data)))
			Expect(encoded[0]).To(Equal(byte(2)))

			decoded, err := DecodeSyncMessage(bytes.NewReader(encoded))
			Expect(err).ToNot(HaveOccurred())
			Expect(decoded.Version).To(Equal(original.Version))
			Expect(decoded.Timestamp).To(Equal(original.Timestamp))
			Expect(decoded.Data).To(Equal(original.Data))
		})

		It("should handle empty data", func() {
			original := &SyncMessage{
				Version:   1,
				Timestamp: time.Now().UnixNano(),
				Data:      []byte{},
			}

			encoded := original.Encode()
			Expect(encoded).To(HaveLen(1 + 8 + 4))

			decoded, err := DecodeSyncMessage(bytes.NewReader(encoded))
			Expect(err).ToNot(HaveOccurred())
			Expect(decoded.Version).To(Equal(byte(1)))
			Expect(decoded.Timestamp).To(Equal(original.Timestamp))
			Expect(decoded.Data).To(BeEmpty())
		})

		It("should handle large data", func() {
			largeData := make([]byte, 64*1024)
			for i := range largeData {
				largeData[i] = byte(i % 256)
			}

			original := &SyncMessage{
				Version:   3,
				Timestamp: time.Now().UnixNano(),
				Data:      largeData,
			}

			encoded := original.Encode()
			decoded, err := DecodeSyncMessage(bytes.NewReader(encoded))
			Expect(err).ToNot(HaveOccurred())
			Expect(decoded.Data).To(Equal(largeData))
		})

		It("should fail on truncated data", func() {
			// header: version(1) + timestamp(8) + data_len(4) = 13 bytes, then claims 10 bytes of data but only 3 present
			encoded := []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 10, 1, 2, 3}
			_, err := DecodeSyncMessage(bytes.NewReader(encoded))
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("TargetHandler", func() {
		var (
			ctrl      *gomock.Controller
			handler   *TargetHandler
			tmpDir    string
			vmiUID    types.UID
			cilClient *MockConntrackClient
		)

		BeforeEach(func() {
			var err error
			tmpDir, err = os.MkdirTemp("", "conntrack-test")
			Expect(err).ToNot(HaveOccurred())

			ctrl = gomock.NewController(GinkgoT())
			cilClient = NewMockConntrackClient(ctrl)
			handler = NewTargetHandler(cilClient)
			vmiUID = types.UID("test-vmi-uid")
		})

		AfterEach(func() {
			handler.Cleanup(vmiUID)
			os.RemoveAll(tmpDir)
			ctrl.Finish()
		})

		It("should start and stop proxy listener", func() {
			socketPath := filepath.Join(tmpDir, "proxy.sock")

			err := handler.StartProxyListener(vmiUID, socketPath)
			Expect(err).ToNot(HaveOccurred())

			_, err = os.Stat(socketPath)
			Expect(err).ToNot(HaveOccurred())

			handler.Cleanup(vmiUID)

			Eventually(func() error {
				_, err := net.Dial("unix", socketPath)
				return err
			}).Should(HaveOccurred())
		})

		It("should handle duplicate StartProxyListener calls", func() {
			socketPath := filepath.Join(tmpDir, "proxy.sock")

			err := handler.StartProxyListener(vmiUID, socketPath)
			Expect(err).ToNot(HaveOccurred())

			err = handler.StartProxyListener(vmiUID, socketPath)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should start and stop hook listener", func() {
			socketPath := filepath.Join(tmpDir, "hook.sock")

			err := handler.StartHookListener(vmiUID, socketPath)
			Expect(err).ToNot(HaveOccurred())

			_, err = os.Stat(socketPath)
			Expect(err).ToNot(HaveOccurred())

			handler.Cleanup(vmiUID)
		})

		It("should receive and import conntrack data", func() {
			socketPath := filepath.Join(tmpDir, "proxy.sock")

			importCalled := make(chan struct{})
			cilClient.EXPECT().
				ImportConntrack(gomock.Any(), []byte("test ct data"), byte(1)).
				DoAndReturn(func(ctx context.Context, data []byte, version byte) error {
					close(importCalled)
					return nil
				})

			err := handler.StartProxyListener(vmiUID, socketPath)
			Expect(err).ToNot(HaveOccurred())

			conn, err := net.Dial("unix", socketPath)
			Expect(err).ToNot(HaveOccurred())

			msg := &SyncMessage{
				Version:   1,
				Timestamp: time.Now().UnixNano(),
				Data:      []byte("test ct data"),
			}
			_, err = conn.Write(msg.Encode())
			Expect(err).ToNot(HaveOccurred())
			conn.Close()

			Eventually(importCalled, 2*time.Second).Should(BeClosed())
		})

		It("should respond to hook signal", func() {
			socketPath := filepath.Join(tmpDir, "hook.sock")

			err := handler.StartHookListener(vmiUID, socketPath)
			Expect(err).ToNot(HaveOccurred())

			conn, err := net.Dial("unix", socketPath)
			Expect(err).ToNot(HaveOccurred())
			defer conn.Close()

			_, err = conn.Write([]byte("wait\n"))
			Expect(err).ToNot(HaveOccurred())

			buf := make([]byte, 10)
			n, err := conn.Read(buf)
			Expect(err).ToNot(HaveOccurred())
			Expect(string(buf[:n])).To(Equal("ok\n"))
		})

		It("should timeout when CT data arrives with expired source timestamp", func() {
			payload := &CTPayload{
				Data:      []byte("test ct data"),
				Version:   1,
				SyncStart: time.Now().Add(-300 * time.Millisecond),
			}

			handler.onCTReceived(vmiUID, payload)

			handler.mu.Lock()
			state := handler.states[vmiUID]
			Expect(state.injectionState).To(Equal(InjectionTimedOut))
			handler.mu.Unlock()
		})

		It("should timeout import when remaining budget is exceeded", func() {
			cilClient.EXPECT().
				ImportConntrack(gomock.Any(), gomock.Any(), gomock.Any()).
				DoAndReturn(func(ctx context.Context, data []byte, version byte) error {
					<-ctx.Done()
					return ctx.Err()
				})

			payload := &CTPayload{
				Data:      []byte("test ct data"),
				Version:   1,
				SyncStart: time.Now().Add(-150 * time.Millisecond),
			}

			handler.onCTReceived(vmiUID, payload)

			Eventually(func() InjectionState {
				handler.mu.Lock()
				defer handler.mu.Unlock()
				return handler.states[vmiUID].injectionState
			}, 1*time.Second).Should(Equal(InjectionTimedOut))
		})

		It("should timeout when hook fires and no CT data arrives", func() {
			socketPath := filepath.Join(tmpDir, "hook.sock")

			err := handler.StartHookListener(vmiUID, socketPath)
			Expect(err).ToNot(HaveOccurred())

			start := time.Now()
			conn, err := net.Dial("unix", socketPath)
			Expect(err).ToNot(HaveOccurred())
			defer conn.Close()

			_, err = conn.Write([]byte("wait\n"))
			Expect(err).ToNot(HaveOccurred())

			buf := make([]byte, 10)
			n, err := conn.Read(buf)
			elapsed := time.Since(start)
			Expect(err).ToNot(HaveOccurred())
			Expect(string(buf[:n])).To(Equal("ok\n"))
			Expect(elapsed).To(BeNumerically(">=", MaxInjectionTimeout))
			Expect(elapsed).To(BeNumerically("<", MaxInjectionTimeout+100*time.Millisecond))

			handler.mu.Lock()
			state := handler.states[vmiUID]
			Expect(state.injectionState).To(Equal(InjectionTimedOut))
			handler.mu.Unlock()
		})

		It("should complete when hook fires before CT data and CT arrives in time", func() {
			proxyPath := filepath.Join(tmpDir, "proxy.sock")
			hookPath := filepath.Join(tmpDir, "hook.sock")

			cilClient.EXPECT().
				ImportConntrack(gomock.Any(), []byte("test ct data"), byte(1)).
				DoAndReturn(func(ctx context.Context, data []byte, version byte) error {
					return nil
				})

			err := handler.StartProxyListener(vmiUID, proxyPath)
			Expect(err).ToNot(HaveOccurred())
			err = handler.StartHookListener(vmiUID, hookPath)
			Expect(err).ToNot(HaveOccurred())

			// Hook fires first (no CT data yet)
			hookConn, err := net.Dial("unix", hookPath)
			Expect(err).ToNot(HaveOccurred())
			defer hookConn.Close()
			_, err = hookConn.Write([]byte("wait\n"))
			Expect(err).ToNot(HaveOccurred())

			// CT data arrives shortly after with recent timestamp
			time.Sleep(10 * time.Millisecond)
			proxyConn, err := net.Dial("unix", proxyPath)
			Expect(err).ToNot(HaveOccurred())
			msg := &SyncMessage{
				Version:   1,
				Timestamp: time.Now().Add(-20 * time.Millisecond).UnixNano(),
				Data:      []byte("test ct data"),
			}
			_, err = proxyConn.Write(msg.Encode())
			Expect(err).ToNot(HaveOccurred())
			proxyConn.Close()

			// Hook should respond after import completes
			buf := make([]byte, 10)
			n, err := hookConn.Read(buf)
			Expect(err).ToNot(HaveOccurred())
			Expect(string(buf[:n])).To(Equal("ok\n"))

			handler.mu.Lock()
			state := handler.states[vmiUID]
			Expect(state.injectionState).To(Equal(InjectionDone))
			handler.mu.Unlock()
		})
	})

	Describe("SourceHandler", func() {
		var (
			ctrl      *gomock.Controller
			handler   *SourceHandler
			cilClient *MockConntrackClient
		)

		BeforeEach(func() {
			ctrl = gomock.NewController(GinkgoT())
			cilClient = NewMockConntrackClient(ctrl)
			handler = NewSourceHandler(cilClient)
		})

		AfterEach(func() {
			ctrl.Finish()
		})

		It("should track sent VMIs", func() {
			vmiUID := types.UID("test-vmi")
			Expect(handler.HasSentCT(vmiUID)).To(BeFalse())

			handler.Cleanup(vmiUID)
			Expect(handler.HasSentCT(vmiUID)).To(BeFalse())
		})

		It("should cleanup sent state", func() {
			vmiUID := types.UID("test-vmi")

			handler.mu.Lock()
			handler.sentVMIs[vmiUID] = struct{}{}
			handler.mu.Unlock()

			Expect(handler.HasSentCT(vmiUID)).To(BeTrue())

			handler.Cleanup(vmiUID)
			Expect(handler.HasSentCT(vmiUID)).To(BeFalse())
		})
	})

	Describe("CiliumClient", func() {
		var (
			server     *httptest.Server
			socketPath string
			client     *CiliumClient
			tmpDir     string
		)

		BeforeEach(func() {
			var err error
			tmpDir, err = os.MkdirTemp("", "cilium-test")
			Expect(err).ToNot(HaveOccurred())

			socketPath = filepath.Join(tmpDir, "cilium.sock")

			listener, err := net.Listen("unix", socketPath)
			Expect(err).ToNot(HaveOccurred())

			mux := http.NewServeMux()
			mux.HandleFunc("/v1/conntrack/export", func(w http.ResponseWriter, r *http.Request) {
				ip := r.URL.Query().Get("ip4")
				if ip == "" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Cilium-Conntrack-Export-Version", "2")
				w.Write([]byte("conntrack-data-for-" + ip))
			})
			mux.HandleFunc("/v1/conntrack/import", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				w.WriteHeader(http.StatusOK)
			})

			server = &httptest.Server{
				Listener: listener,
				Config:   &http.Server{Handler: mux},
			}
			server.Start()

			client = NewCiliumClientWithSocket(socketPath)
		})

		AfterEach(func() {
			server.Close()
			os.RemoveAll(tmpDir)
		})

		It("should check availability", func() {
			Expect(client.IsAvailable()).To(BeTrue())

			badClient := NewCiliumClientWithSocket("/nonexistent/path")
			Expect(badClient.IsAvailable()).To(BeFalse())
		})

		It("should export conntrack entries", func() {
			ctx := context.Background()
			result, err := client.ExportConntrack(ctx, "10.0.0.1")
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Data).To(Equal([]byte("conntrack-data-for-10.0.0.1")))
			Expect(result.Version).To(Equal(byte(2)))
		})

		It("should import conntrack entries", func() {
			ctx := context.Background()
			err := client.ImportConntrack(ctx, []byte("import-data"), 1)
			Expect(err).ToNot(HaveOccurred())
		})
	})
})

