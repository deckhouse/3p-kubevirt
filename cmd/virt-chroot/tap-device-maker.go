package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/devices"
	"github.com/spf13/cobra"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"kubevirt.io/client-go/log"
	"kubevirt.io/kubevirt/pkg/virt-handler/cgroup"
)

func createTapDevice(name string, parentName string, owner uint, group uint, queueNumber int, mtu int) error {
	var tapDevice netlink.Link

	if parentName == "" {
		tapDevice = &netlink.Tuntap{
			LinkAttrs:  netlink.LinkAttrs{Name: name},
			Mode:       unix.IFF_TAP,
			NonPersist: false,
			Queues:     queueNumber,
			Owner:      uint32(owner),
			Group:      uint32(group),
		}

		// // when netlink receives a request for a tap device with 1 queue, it uses
		// // the MULTI_QUEUE flag, which differs from libvirt; as such, we need to
		// // manually request the single queue flags, enabling libvirt to consume
		// // the tap device.
		// // See https://github.com/vishvananda/netlink/issues/574
		// if queueNumber == 1 {
		// 	tapDevice.Flags = netlink.TUNTAP_DEFAULTS
		// }

	} else {
		m, err := netlink.LinkByName(parentName)
		if err != nil {
			return fmt.Errorf("failed to lookup lowerDevice %q: %v", parentName, err)
		}

		// Create a macvtap
		tapDevice = &netlink.Macvtap{
			Macvlan: netlink.Macvlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:        name,
					ParentIndex: m.Attrs().Index,
					// we had crashes if we did not set txqlen to some value
					TxQLen: m.Attrs().TxQLen,
				},
				Mode: netlink.MACVLAN_MODE_BRIDGE,
			},
		}
	}

	// Device creation is retried due to https://bugzilla.redhat.com/1933627
	// which has been observed on multiple occasions on CI runs.
	const retryAttempts = 5
	attempt, err := retry(retryAttempts, func() error {
		return netlink.LinkAdd(tapDevice)
	})
	if err != nil {
		return fmt.Errorf("failed to create tap device named %s. Reason: %v", name, err)
	}

	if err := netlink.LinkSetMTU(tapDevice, mtu); err != nil {
		return fmt.Errorf("failed to set MTU on tap device named %s. Reason: %v", name, err)
	}

	// Create /dev/tapX device
	if parentName != "" {
		dev, err := getMacvtapDevice(name)
		if err != nil {
			return fmt.Errorf("failed to get device for %s. error: %v", name, err)
		}

		// fix permissions
		manager, err := cgroup.NewManagerFromPid(1)
		if err != nil {
			return fmt.Errorf("failed to create cgroup manager. error: %v", err)
		}

		deviceRule := &devices.Rule{
			Type:        devices.CharDevice,
			Major:       int64(dev.Major),
			Minor:       int64(dev.Minor),
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
		// fix permissions END

		tapDevPath := filepath.Join("/dev", dev.Name)
		if err := syscall.Mknod(tapDevPath, syscall.S_IFCHR|uint32(os.FileMode(0644)), int(unix.Mkdev(uint32(dev.Major), uint32(dev.Minor)))); err != nil {
			return fmt.Errorf("failed to create characted device %s. error: %v", tapDevPath, err)
		}
	}

	fmt.Printf("Successfully created tap device %s, attempt %d\n", name, attempt)

	return nil
}

type macvtapDevice struct {
	Name  string
	Major int
	Minor int
}

func getMacvtapDevice(name string) (macvtapDevice, error) {
	tapSysPath := filepath.Join("/sys/class/net", name, "macvtap")
	dirContent, err := ioutil.ReadDir(tapSysPath)
	var dev macvtapDevice
	if err != nil {
		return dev, fmt.Errorf("failed to open directory %s. error: %v", tapSysPath, err)
	}

	for _, dir := range dirContent {
		if strings.HasPrefix(dir.Name(), "tap") {
			dev.Name = dir.Name()
			break
		}
	}
	if dev.Name == "" {
		return dev, fmt.Errorf("failed to find tap device number for %s.", name)
	}

	devSysPath := filepath.Join(tapSysPath, dev.Name, "dev")
	devString, err := ioutil.ReadFile(devSysPath)

	if err != nil {
		return dev, fmt.Errorf("unable to read file %s. error: %v", devSysPath, err)
	}

	m := strings.Split(strings.TrimSuffix(string(devString), "\n"), ":")
	dev.Major, err = strconv.Atoi(m[0])
	if err != nil {
		return dev, fmt.Errorf("unable to convert major %s. error: %v", m[0], err)
	}
	dev.Minor, err = strconv.Atoi(m[1])
	if err != nil {
		return dev, fmt.Errorf("unable to convert minor %s. error: %v", m[1], err)
	}
	return dev, nil
}

func NewCreateTapCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "create-tap",
		Short: "create a tap device in a given PID net ns",
		RunE: func(cmd *cobra.Command, args []string) error {
			tapName := cmd.Flag("tap-name").Value.String()
			parentName := cmd.Flag("parent-name").Value.String()
			uidStr := cmd.Flag("uid").Value.String()
			gidStr := cmd.Flag("gid").Value.String()
			queueNumber, err := cmd.Flags().GetUint32("queue-number")
			if err != nil {
				return fmt.Errorf("could not access queue-number parameter: %v", err)
			}
			mtu, err := cmd.Flags().GetUint32("mtu")
			if err != nil {
				return fmt.Errorf("could not access mtu parameter: %v", err)
			}

			uid, err := strconv.ParseUint(uidStr, 10, 32)
			if err != nil {
				return fmt.Errorf("could not parse tap device owner: %v", err)
			}
			gid, err := strconv.ParseUint(gidStr, 10, 32)
			if err != nil {
				return fmt.Errorf("could not parse tap device group: %v", err)
			}

			return createTapDevice(tapName, parentName, uint(uid), uint(gid), int(queueNumber), int(mtu))
		},
	}
}

func retry(retryAttempts uint, f func() error) (uint, error) {
	var errorsString []string
	for attemptID := uint(0); attemptID < retryAttempts; attemptID++ {
		if err := f(); err != nil {
			errorsString = append(errorsString, fmt.Sprintf("[%d]: %v", attemptID, err))
			time.Sleep(time.Second)
		} else {
			if len(errorsString) > 0 {
				fmt.Printf("warning: Tap device creation has been retried: %v", strings.Join(errorsString, "\n"))
			}
			return attemptID, nil
		}
	}

	return retryAttempts, fmt.Errorf(strings.Join(errorsString, "\n"))
}
