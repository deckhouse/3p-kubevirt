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

package virtwrap

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("usbfs placeholder", func() {
	It("leaves a non-empty usbfs root behind", func() {
		root := GinkgoT().TempDir()
		Expect(ensureUSBFSPlaceholder(root)).To(Succeed())
		entries, err := os.ReadDir(root)
		Expect(err).ToNot(HaveOccurred())
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].IsDir()).To(BeTrue())

		Expect(ensureUSBFSPlaceholder(root)).To(Succeed(), "must be idempotent")
		_, err = os.Stat(filepath.Join(root, usbfsPlaceholderBus))
		Expect(err).ToNot(HaveOccurred())
	})
})
