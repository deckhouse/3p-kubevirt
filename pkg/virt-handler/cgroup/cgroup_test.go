package cgroup

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	cgroups "github.com/opencontainers/cgroups"
	devices "github.com/opencontainers/cgroups/devices/config"
	"go.uber.org/mock/gomock"

	v1 "kubevirt.io/api/core/v1"

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

	Describe("parseDevicesList", func() {
		It("should parse devices.list content into allow rules", func() {
			// allow-all ("a") lines carry no device numbers and are skipped.
			rules, err := parseDevicesList(strings.NewReader("a *:* rwm\nb 8:0 rwm\nc 136:* rw\n"))
			Expect(err).ToNot(HaveOccurred())
			Expect(rules).To(Equal([]*devices.Rule{
				{Type: devices.BlockDevice, Major: 8, Minor: 0, Permissions: "rwm", Allow: true},
				{Type: devices.CharDevice, Major: 136, Minor: devices.Wildcard, Permissions: "rw", Allow: true},
			}))
		})

		DescribeTable("should reject malformed lines", func(list string) {
			_, err := parseDevicesList(strings.NewReader(list))
			Expect(err).To(HaveOccurred())
		},
			Entry("bad type", "x 8:0 rwm"),
			Entry("missing colon", "b 80 rwm"),
			Entry("non-numeric major", "b foo:0 rwm"),
			Entry("missing permissions", "b 8:0"),
		)
	})

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
