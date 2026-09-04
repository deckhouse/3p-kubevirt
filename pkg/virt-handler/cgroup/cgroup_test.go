package cgroup

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	cgroups "github.com/opencontainers/cgroups"
	devices "github.com/opencontainers/cgroups/devices/config"
	"go.uber.org/mock/gomock"

	v1 "kubevirt.io/api/core/v1"

	k8sv1 "k8s.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/safepath"
	"kubevirt.io/kubevirt/pkg/virt-handler/isolation"
)

var _ = Describe("cgroup manager", func() {

	var (
		ctrl                  *gomock.Controller
		rulesDefined          []*devices.Rule
		v2DirPath             string
		subsystemPathsDefined map[string]string
	)

	newMockManagerFromCtrl := func(ctrl *gomock.Controller, version CgroupVersion) (Manager, error) {
		mockCgroupsManager := NewMockcgroupsManager(ctrl)
		mockCgroupsManager.EXPECT().GetPaths().DoAndReturn(func() map[string]string {
			paths := make(map[string]string)

			// See documentation here for more info: https://github.com/opencontainers/cgroups/blob/main/cgroups.go
			if version == V1 {
				paths["devices"] = "/sys/fs/cgroup/devices"
			} else {
				paths[""] = v2DirPath
			}

			return paths
		}).AnyTimes()

		execVirtChrootFunc := func(r *cgroups.Resources, subsystemPaths map[string]string, rootless bool, version CgroupVersion) error {
			rulesDefined = r.Devices
			subsystemPathsDefined = subsystemPaths
			return nil
		}

		getCurrentlyDefinedRulesFunc := func(cgManager cgroups.Manager) ([]*devices.Rule, error) {
			return rulesDefined, nil
		}

		if version == V1 {
			return newCustomizedV1Manager(mockCgroupsManager, false, execVirtChrootFunc, getCurrentlyDefinedRulesFunc)
		} else {
			return newCustomizedV2Manager(mockCgroupsManager, false, nil, execVirtChrootFunc)
		}
	}

	newMockManager := func(version CgroupVersion) (Manager, error) {
		return newMockManagerFromCtrl(ctrl, version)
	}

	newResourcesWithRule := func(rule *devices.Rule) *cgroups.Resources {
		return &cgroups.Resources{
			Devices: []*devices.Rule{
				rule,
			},
		}
	}

	newDeviceRule := func(UID int64) *devices.Rule {
		return &devices.Rule{
			Type:        'z',
			Major:       UID,
			Minor:       UID,
			Permissions: "fakePermissions",
			Allow:       true,
		}
	}

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		rulesDefined = make([]*devices.Rule, 0)
		v2DirPath = "/sys/fs/cgroup/"
	})

	AfterEach(func() {
		v2DirPath = ""
	})

	DescribeTable("ensure that default rules are added", func(version CgroupVersion) {
		manager, err := newMockManager(version)
		Expect(err).ShouldNot(HaveOccurred())

		fakeRule := newDeviceRule(123)

		err = manager.Set(newResourcesWithRule(fakeRule))
		Expect(err).ShouldNot(HaveOccurred())

		Expect(rulesDefined).To(ContainElement(fakeRule), "defined rule is expected to exist")

		defaultDeviceRules := GenerateDefaultDeviceRules()
		for _, defaultRule := range defaultDeviceRules {
			Expect(rulesDefined).To(ContainElement(defaultRule), "default rules are expected to be defined")
		}
		Expect(rulesDefined).To(HaveLen(len(defaultDeviceRules) + 1))
	},
		Entry("for v1", V1),
		Entry("for v2", V2),
	)

	DescribeTable("ensure that past rules are not overridden", func(version CgroupVersion) {
		manager, err := newMockManager(version)
		Expect(err).ShouldNot(HaveOccurred())

		fakeRule1 := newDeviceRule(123)
		fakeRule2 := newDeviceRule(456)

		err = manager.Set(newResourcesWithRule(fakeRule1))
		Expect(err).ShouldNot(HaveOccurred())

		err = manager.Set(newResourcesWithRule(fakeRule2))
		Expect(err).ShouldNot(HaveOccurred())

		Expect(rulesDefined).To(ContainElement(fakeRule1), "previous rule is expected to not be overridden")

	},
		Entry("for v1", V1),
		Entry("for v2", V2),
	)

	DescribeTable("ensure that past rules are overridden if explicitly set", func(version CgroupVersion) {
		manager, err := newMockManager(version)
		Expect(err).ShouldNot(HaveOccurred())

		fakeRule := newDeviceRule(123)
		fakeRule.Permissions = "fake-permissions-123"

		err = manager.Set(newResourcesWithRule(fakeRule))
		Expect(err).ShouldNot(HaveOccurred())
		Expect(rulesDefined).To(ContainElement(fakeRule), "defined rule is expected to exist")

		fakeRule.Permissions = "fake-permissions-456"
		Expect(rulesDefined).To(ContainElement(fakeRule), "rule needs to be overridden since explicitly re-set")

	},
		Entry("for v1", V1),
		Entry("for v2", V2),
	)

	DescribeTable("ensure that correct set of cgroups is configured", func(dirPath string, expectedPaths []string) {
		v2DirPath = dirPath
		manager, err := newMockManager(V2)
		Expect(err).ShouldNot(HaveOccurred())

		fakeRule := newDeviceRule(123)

		err = manager.Set(newResourcesWithRule(fakeRule))
		Expect(err).ShouldNot(HaveOccurred())

		Expect(rulesDefined).To(ContainElement(fakeRule), "defined rule is expected to exist")

		defaultDeviceRules := GenerateDefaultDeviceRules()
		for _, defaultRule := range defaultDeviceRules {
			Expect(rulesDefined).To(ContainElement(defaultRule), "default rules are expected to be defined")
		}
		Expect(rulesDefined).To(HaveLen(len(defaultDeviceRules) + 1))
		Expect(subsystemPathsDefined).To(ConsistOf(expectedPaths))
	},
		Entry("for crun installation",
			"/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/crio-456.scope/container",
			[]string{
				"/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/crio-456.scope/container",
				"/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/crio-456.scope",
			},
		),
		Entry("for runc installation",
			"/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/crio-456.scope",
			[]string{
				"/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/crio-456.scope",
			},
		),
	)
})

var _ = Describe("generateDeviceRulesForVMI", func() {
	var (
		ctrl    *gomock.Controller
		tempDir string
	)

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		var err error
		tempDir, err = os.MkdirTemp("", "cgroup-device-rules")
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(os.RemoveAll, tempDir)
	})

	newMockIsolationWithMountRoot := func() *isolation.MockIsolationResult {
		mountRoot, err := safepath.JoinAndResolveWithRelativeRoot(tempDir)
		Expect(err).ToNot(HaveOccurred())
		isolationRes := isolation.NewMockIsolationResult(ctrl)
		isolationRes.EXPECT().MountRoot().Return(mountRoot, nil).AnyTimes()
		return isolationRes
	}

	It("should not fail when /dev/vfio does not exist", func() {
		rules, err := generateDeviceRulesForVMI(&v1.VirtualMachineInstance{}, newMockIsolationWithMountRoot(), "")
		Expect(err).ToNot(HaveOccurred())
		Expect(rules).To(BeEmpty())
	})

	It("should not fail when /dev/vfio exists but is empty", func() {
		Expect(os.MkdirAll(filepath.Join(tempDir, "dev", "vfio"), 0755)).To(Succeed())
		rules, err := generateDeviceRulesForVMI(&v1.VirtualMachineInstance{}, newMockIsolationWithMountRoot(), "")
		Expect(err).ToNot(HaveOccurred())
		Expect(rules).To(BeEmpty())
	})

	It("should not fail when /dev/bus/usb exists but is empty", func() {
		Expect(os.MkdirAll(filepath.Join(tempDir, "dev", "bus", "usb"), 0755)).To(Succeed())
		rules, err := generateDeviceRulesForVMI(&v1.VirtualMachineInstance{}, newMockIsolationWithMountRoot(), "")
		Expect(err).ToNot(HaveOccurred())
		Expect(rules).To(BeEmpty())
	})
})

var _ = Describe("generateDeviceRulesForAttachedHotplugDevices", func() {
	const volumeName = "hotplug-vol"

	var (
		ctrl    *gomock.Controller
		tempDir string
		vmi     *v1.VirtualMachineInstance
	)

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		var err error
		tempDir, err = os.MkdirTemp("", "cgroup-hotplug-device-rules")
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(os.RemoveAll, tempDir)

		// Mimic a launcher rootfs where /var/run is a symlink to /run, as injected by some container runtimes.
		Expect(os.MkdirAll(filepath.Join(tempDir, "run", "kubevirt", "hotplug-disks"), 0755)).To(Succeed())
		Expect(os.MkdirAll(filepath.Join(tempDir, "var"), 0755)).To(Succeed())
		Expect(os.Symlink("../run", filepath.Join(tempDir, "var", "run"))).To(Succeed())

		blockMode := k8sv1.PersistentVolumeBlock
		vmi = &v1.VirtualMachineInstance{
			Status: v1.VirtualMachineInstanceStatus{
				VolumeStatus: []v1.VolumeStatus{{
					Name:                      volumeName,
					HotplugVolume:             &v1.HotplugVolumeStatus{},
					PersistentVolumeClaimInfo: &v1.PersistentVolumeClaimInfo{VolumeMode: &blockMode},
				}},
			},
		}
	})

	newMockIsolationWithMountRoot := func() *isolation.MockIsolationResult {
		mountRoot, err := safepath.JoinAndResolveWithRelativeRoot(tempDir)
		Expect(err).ToNot(HaveOccurred())
		isolationRes := isolation.NewMockIsolationResult(ctrl)
		isolationRes.EXPECT().MountRoot().Return(mountRoot, nil).AnyTimes()
		return isolationRes
	}

	It("should skip a hotplug volume that is not attached yet behind a symlinked /var/run", func() {
		rules, err := generateDeviceRulesForAttachedHotplugDevices(vmi, newMockIsolationWithMountRoot())
		Expect(err).ToNot(HaveOccurred())
		Expect(rules).To(BeEmpty())
	})

	It("should resolve the hotplug volume path behind a symlinked /var/run", func() {
		// A regular file is not a device node, so no rule is expected, but the path must be traversed without error.
		Expect(os.WriteFile(filepath.Join(tempDir, "run", "kubevirt", "hotplug-disks", volumeName), nil, 0644)).To(Succeed())
		rules, err := generateDeviceRulesForAttachedHotplugDevices(vmi, newMockIsolationWithMountRoot())
		Expect(err).ToNot(HaveOccurred())
		Expect(rules).To(BeEmpty())
	})
})
