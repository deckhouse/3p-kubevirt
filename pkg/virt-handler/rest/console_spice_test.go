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
	"encoding/binary"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"
)

func spiceLink(connectionID uint32) []byte {
	prefix := make([]byte, spiceLinkPrefixSize)
	copy(prefix, spiceLinkMagic)
	binary.LittleEndian.PutUint32(prefix[spiceLinkHeaderSize:], connectionID)
	return prefix
}

func newSpiceHandler() *ConsoleHandler {
	return &ConsoleHandler{
		spiceSessions: make(map[types.UID]*spiceSession),
		spiceLock:     &sync.Mutex{},
	}
}

var _ = Describe("SPICE link message", func() {
	It("reads the connection id a client sends", func() {
		id, ok := spiceConnectionID(spiceLink(42))

		Expect(ok).To(BeTrue())
		Expect(id).To(Equal(uint32(42)))
	})

	It("rejects a message that does not start with the SPICE magic", func() {
		prefix := spiceLink(1)
		copy(prefix, "HTTP")

		_, ok := spiceConnectionID(prefix)

		Expect(ok).To(BeFalse())
	})

	It("rejects a message cut short of the connection id", func() {
		_, ok := spiceConnectionID(spiceLink(1)[:spiceLinkPrefixSize-1])

		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("SPICE session tracking", func() {
	const uid = types.UID("vmi-uid")

	It("keeps the channels of one client together", func() {
		h := newSpiceHandler()

		main := h.acquireSpiceSession(uid, 0, false)
		display := h.acquireSpiceSession(uid, 7, false)
		cursor := h.acquireSpiceSession(uid, 7, false)

		Expect(display).To(Equal(main), "a channel of the same session shares its stop channel")
		Expect(cursor).To(Equal(main))
		Expect(main).NotTo(BeClosed(), "nothing evicts a client that is alone")
	})

	It("evicts the previous client when a new one starts a session", func() {
		h := newSpiceHandler()
		first := h.acquireSpiceSession(uid, 0, false)
		h.acquireSpiceSession(uid, 3, false)

		second := h.acquireSpiceSession(uid, 0, false)

		Expect(first).To(BeClosed(), "every channel of the first client is torn down")
		Expect(second).NotTo(BeClosed())
	})

	It("treats a link message it cannot read as a new client", func() {
		h := newSpiceHandler()
		first := h.acquireSpiceSession(uid, 0, false)

		second := h.acquireSpiceSession(uid, 9, true)

		Expect(first).To(BeClosed(), "a connection that does not speak SPICE gets no free pass")
		Expect(second).NotTo(BeClosed())
	})

	It("forgets a session only when its last channel closes", func() {
		h := newSpiceHandler()
		main := h.acquireSpiceSession(uid, 0, false)
		h.acquireSpiceSession(uid, 5, false)

		h.releaseSpiceSession(uid, main)
		Expect(h.spiceSessions).To(HaveKey(uid), "one channel left, the client is still there")

		h.releaseSpiceSession(uid, main)
		Expect(h.spiceSessions).NotTo(HaveKey(uid))
	})

	It("ignores a channel of a session that has already been evicted", func() {
		h := newSpiceHandler()
		first := h.acquireSpiceSession(uid, 0, false)
		second := h.acquireSpiceSession(uid, 0, false)

		h.releaseSpiceSession(uid, first)

		Expect(h.spiceSessions[uid].stopCh).To(Equal(second), "the newer client keeps its session")
	})

	It("keeps sessions of different virtual machines apart", func() {
		h := newSpiceHandler()
		other := types.UID("another-vmi")

		first := h.acquireSpiceSession(uid, 0, false)
		h.acquireSpiceSession(other, 0, false)

		Expect(first).NotTo(BeClosed())
	})
})
