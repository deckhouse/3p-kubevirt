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

package cache

import (
	"path/filepath"

	"kubevirt.io/kubevirt/pkg/util"
)

const tapProvisionModeFileName = ".tap-provision-mode"

// TapProvisionModeCache persists the tap provisioning mode resolved for the
// current virt-launcher pod, so that later Setup calls (NIC hotplug) and
// virt-handler restarts reuse the mode the pod was wired with without going
// back to the API.
type TapProvisionModeCache struct {
	cache *Cache
}

type TapProvisionModeData struct {
	External bool `json:"external"`
}

func NewTapProvisionModeCache(creator cacheCreator, uid string) TapProvisionModeCache {
	return TapProvisionModeCache{
		creator.New(filepath.Join(util.VirtPrivateDir, podIfaceCacheDirName, uid, tapProvisionModeFileName)),
	}
}

// Read reports the persisted mode. A missing file and an unreadable/corrupt one (e.g.
// truncated by a crash mid-write; Cache.Write is not atomic) are both reported as absent:
// the caller then re-resolves and re-persists the mode, whereas failing here would wedge
// the pod's network setup until the pod is replaced.
func (t TapProvisionModeCache) Read() (external, exists bool) {
	data := &TapProvisionModeData{}
	if _, err := t.cache.Read(data); err != nil {
		return false, false
	}
	return data.External, true
}

func (t TapProvisionModeCache) Write(external bool) error {
	return t.cache.Write(&TapProvisionModeData{External: external})
}

func (t TapProvisionModeCache) Remove() error {
	return t.cache.Delete()
}
