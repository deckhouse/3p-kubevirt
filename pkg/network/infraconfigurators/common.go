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
 * Copyright 2021 Red Hat, Inc.
 *
 */

//go:generate mockgen -source $GOFILE -package=$GOPACKAGE -destination=generated_mock_$GOFILE

package infraconfigurators

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/devices"
	"github.com/vishvananda/netlink"
	"kubevirt.io/client-go/log"
	"kubevirt.io/kubevirt/pkg/network/cache"
	"kubevirt.io/kubevirt/pkg/virt-handler/cgroup"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"

	v1 "kubevirt.io/api/core/v1"
	netdriver "kubevirt.io/kubevirt/pkg/network/driver"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter"
)

type PodNetworkInfraConfigurator interface {
	DiscoverPodNetworkInterface(podIfaceName string) error
	PreparePodNetworkInterface() error
	GenerateNonRecoverableDomainIfaceSpec() *api.Interface
	// The method should return dhcp configuration that cannot be calculated in virt-launcher's phase2
	GenerateNonRecoverableDHCPConfig() *cache.DHCPConfig
}

func createAndBindTapToBridge(handler netdriver.NetworkHandler, deviceName string, parentName string, launcherPID int, mtu int, tapOwner string, vmi *v1.VirtualMachineInstance) error {
	//err := handler.CreateTapDevice(deviceName, parentName, calculateNetworkQueues(vmi), launcherPID, mtu, tapOwner)
	//if err != nil {
	//	return err
	//}

	m, err := netlink.LinkByName(parentName)
	if err != nil {
		return fmt.Errorf("failed to lookup lowerDevice %q: %v", parentName, err)
	}

	// Create a macvtap
	tapDevice := &netlink.Macvtap{
		Macvlan: netlink.Macvlan{
			LinkAttrs: netlink.LinkAttrs{
				Name:        deviceName,
				ParentIndex: m.Attrs().Index,
				// we had crashes if we did not set txqlen to some value
				TxQLen: m.Attrs().TxQLen,
			},
			Mode: netlink.MACVLAN_MODE_BRIDGE,
		},
	}

	if err := netlink.LinkAdd(tapDevice); err != nil {
		return fmt.Errorf("failed to create tap device named %s. Reason: %v", deviceName, err)
	}
	if err := netlink.LinkSetMTU(tapDevice, mtu); err != nil {
		return fmt.Errorf("failed to set MTU on tap device named %s. Reason: %v", deviceName, err)
	}

	// TODO: separate function

	tap, err := netlink.LinkByName(deviceName)
	log.Log.V(4).Infof("Looking for tap device: %s", deviceName)
	if err != nil {
		return fmt.Errorf("could not find tap device %s; %v", deviceName, err)
	}

	err = netlink.LinkSetUp(tap)
	if err != nil {
		return fmt.Errorf("failed to set tap device %s up; %v", deviceName, err)
	}

	// TODO: separate function

	// fix permissions
	manager, _ := cgroup.NewManagerFromPid(launcherPID)

	tapSysPath := filepath.Join("/sys/class/net", deviceName, "macvtap")
	// dirContent, err := ioutil.ReadDir(tapSysPath)
	cmd := exec.Command("/usr/bin/nsenter", "-t", strconv.Itoa(launcherPID), "-n", "/bin/sh", "-c", "ls -1 "+tapSysPath)
	out, err := cmd.Output()
	if err != nil {
		log.Log.Infof("command failed: %+v", []string{"/usr/bin/nsenter", "-t", strconv.Itoa(launcherPID), "-n", "/bin/sh", "-c", "ls -1 " + tapSysPath})
		time.Sleep(10 * time.Minute)
		return fmt.Errorf("failed to open directory %s. error: %v", tapSysPath, err)
	}

	// devName := dirContent[0].Name()
	devName := string(out)

	devSysPath := filepath.Join(tapSysPath, devName, "dev")
	cmd = exec.Command("/usr/bin/nsenter", "-t", strconv.Itoa(launcherPID), "-n", "/bin/sh", "-c", "cat "+devSysPath)

	// devString, err := ioutil.ReadFile(devSysPath)
	out, err = cmd.Output()
	if err != nil {
		return fmt.Errorf("unable to read file %s. error: %v", devSysPath, err)
	}
	devString := string(out)

	s := strings.Split(strings.TrimSuffix(string(devString), "\n"), ":")
	major, err := strconv.Atoi(s[0])
	if err != nil {
		return fmt.Errorf("unable to convert major %s. error: %v", s[0], err)
	}
	minor, err := strconv.Atoi(s[1])
	if err != nil {
		return fmt.Errorf("unable to convert minor %s. error: %v", s[1], err)
	}

	deviceRule := &devices.Rule{
		Type:        devices.CharDevice,
		Major:       int64(major),
		Minor:       int64(minor),
		Permissions: "rwm",
		Allow:       true,
	}

	err = manager.Set(&configs.Resources{
		Devices: []*devices.Rule{deviceRule},
	})

	if err != nil {
		return fmt.Errorf("cgroup %s had failed to set device rule. error: %v. rule: %+v", manager.GetCgroupVersion(), err, *deviceRule)
	} else {
		log.Log.Infof("cgroup %s device rule is set successfully. rule: %+v", manager.GetCgroupVersion(), *deviceRule)
	}

	log.Log.Infof("Successfully configured tap device: %s", deviceName)
	return nil
}

func calculateNetworkQueues(vmi *v1.VirtualMachineInstance) uint32 {
	if isMultiqueue(vmi) {
		return converter.CalculateNetworkQueues(vmi)
	}
	return 0
}

func isMultiqueue(vmi *v1.VirtualMachineInstance) bool {
	return (vmi.Spec.Domain.Devices.NetworkInterfaceMultiQueue != nil) &&
		(*vmi.Spec.Domain.Devices.NetworkInterfaceMultiQueue)
}
