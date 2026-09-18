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
	"path/filepath"

	kutil "kubevirt.io/kubevirt/pkg/util"
)

const (
	usbfsRoot = "/dev/bus/usb"
	// usbfsPlaceholderBus is an empty bus directory that makes libusb accept
	// usbfsRoot: libusb treats a directory without a single entry as "no usbfs"
	// and refuses to initialise, which kills QEMU on every usb-host device.
	usbfsPlaceholderBus = "001"
)

// ensureUSBFSPlaceholder keeps root usable for libusb even when no USB device
// node has been created in the pod yet. The pod's /dev/bus/usb is an emptyDir:
// on a migration target the hotplugged USB devices arrive only after the
// migration, so the domain starts with usb-host devices libvirt marked missing
// and QEMU must still be able to initialise libusb. Hotplugged devices are not
// part of the converted domain, so the placeholder is created for every domain.
func ensureUSBFSPlaceholder(root string) error {
	return kutil.MkdirAllWithNosec(filepath.Join(root, usbfsPlaceholderBus))
}
