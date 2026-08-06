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

package network_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	kfs "kubevirt.io/kubevirt/pkg/os/fs"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	v1 "kubevirt.io/api/core/v1"

	dutils "kubevirt.io/kubevirt/pkg/ephemeral-disk-utils"
	"kubevirt.io/kubevirt/pkg/network/cache"
	netsetup "kubevirt.io/kubevirt/pkg/network/setup"
	"kubevirt.io/kubevirt/pkg/network/setup/netpod"
)

var _ = Describe("netconf", func() {
	const (
		testNetworkName = "default"
	)
	var (
		netConf  *netsetup.NetConf
		vmi      *v1.VirtualMachineInstance
		stateMap map[string]*netpod.State

		stateCache stateCacheStub
		ns         nsExecutorStub
	)

	const launcherPid = 0

	BeforeEach(func() {
		dutils.MockDefaultOwnershipManager()
		stateCache = newConfigStateCacheStub()
		ns = nsExecutorStub{}
		stateMap = map[string]*netpod.State{}
		netConf = netsetup.NewNetConfWithCustomFactoryAndConfigState(nsNoopFactory, &tempCacheCreator{}, stateMap, cConfigStub{}, nil)
		vmi = &v1.VirtualMachineInstance{ObjectMeta: metav1.ObjectMeta{UID: "123", Name: "vmi1"}}
	})

	It("runs setup successfully without networks", func() {
		Expect(netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())
	})

	Context("HasOrphanedNetworks with a real on-disk cache", func() {
		var cacheCreator tempCacheCreator

		AfterEach(func() { Expect(cacheCreator.New("").Delete()).To(Succeed()) })

		BeforeEach(func() {
			netConf = netsetup.NewNetConfWithCustomFactoryAndConfigState(nsNoopFactory, &cacheCreator, stateMap, cConfigStub{}, nil)
			vmi.Spec.Networks = []v1.Network{{
				Name:          testNetworkName,
				NetworkSource: v1.NetworkSource{Pod: &v1.PodNetwork{}},
			}}
		})

		It("is false for a VMI with no cache entries", func() {
			Expect(netConf.HasOrphanedNetworks(vmi)).To(BeFalse())
		})

		It("is false when only spec'd networks and the launcher-pid file are cached", func() {
			Expect(cache.WritePodInterfaceCache(&cacheCreator, string(vmi.UID), testNetworkName, &cache.PodIfaceCacheData{})).To(Succeed())
			// The pid file shares the per-UID directory; treating it as a network
			// would flag every VMI as orphaned and let cleanup destroy the pid,
			// breaking replaced-pod detection after a virt-handler restart.
			Expect(cache.NewLauncherPidCache(&cacheCreator, string(vmi.UID)).Write(4242)).To(Succeed())

			Expect(netConf.HasOrphanedNetworks(vmi)).To(BeFalse())
		})

		It("is true when a cached network is no longer in the spec", func() {
			Expect(cache.WritePodInterfaceCache(&cacheCreator, string(vmi.UID), testNetworkName, &cache.PodIfaceCacheData{})).To(Succeed())
			Expect(cache.WritePodInterfaceCache(&cacheCreator, string(vmi.UID), "veth_n5340036e", &cache.PodIfaceCacheData{})).To(Succeed())

			Expect(netConf.HasOrphanedNetworks(vmi)).To(BeTrue())
		})
	})

	It("runs setup successfully with networks", func() {
		stateMap[string(vmi.UID)] = netpod.NewState(stateCache, ns)
		Expect(stateCache.Write(testNetworkName, cache.PodIfaceNetworkPreparationFinished)).To(Succeed())

		vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{{
			Name:                   testNetworkName,
			InterfaceBindingMethod: v1.InterfaceBindingMethod{Masquerade: &v1.InterfaceMasquerade{}},
		}}
		vmi.Spec.Networks = []v1.Network{{
			Name:          testNetworkName,
			NetworkSource: v1.NetworkSource{Pod: &v1.PodNetwork{}},
		}}
		Expect(netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())
		Expect(stateCache.Read(testNetworkName)).To(Equal(cache.PodIfaceNetworkPreparationFinished))
	})

	DescribeTable("setup ignores specific network bindings", func(binding v1.InterfaceBindingMethod) {
		netConf = netsetup.NewNetConfWithCustomFactoryAndConfigState(nsFailureFactory, &tempCacheCreator{}, stateMap, cConfigStub{}, nil)

		stateMap[string(vmi.UID)] = netpod.NewState(stateCache, ns)

		vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{{
			Name:                   testNetworkName,
			InterfaceBindingMethod: binding,
		}}
		emptyBindingMethod := v1.InterfaceBindingMethod{}
		if binding == emptyBindingMethod {
			vmi.Spec.Domain.Devices.Interfaces[0].Binding = &v1.PluginBinding{}
		}
		vmi.Spec.Networks = []v1.Network{{
			Name:          testNetworkName,
			NetworkSource: v1.NetworkSource{Pod: &v1.PodNetwork{}},
		}}
		Expect(netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())
		Expect(stateCache.stateCache).To(BeEmpty())
	},
		Entry("SR-IOV", v1.InterfaceBindingMethod{SRIOV: &v1.InterfaceSRIOV{}}),

		// Macvtap is removed in v1.3. This scenario is tracking old VMIs that are still processed in the reconcile loop.
		Entry("macvtap", v1.InterfaceBindingMethod{DeprecatedMacvtap: &v1.DeprecatedInterfaceMacvtap{}}),
	)

	It("fails the setup run", func() {
		netConf := netsetup.NewNetConfWithCustomFactoryAndConfigState(nsFailureFactory, &tempCacheCreator{}, stateMap, cConfigStub{}, nil)
		vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{{
			Name:                   testNetworkName,
			InterfaceBindingMethod: v1.InterfaceBindingMethod{Masquerade: &v1.InterfaceMasquerade{}},
		}}
		vmi.Spec.Networks = []v1.Network{{
			Name:          testNetworkName,
			NetworkSource: v1.NetworkSource{Pod: &v1.PodNetwork{}},
		}}
		Expect(netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).NotTo(Succeed())
	})

	It("fails the teardown run", func() {
		netConf := netsetup.NewNetConfWithCustomFactoryAndConfigState(nil, failingCacheCreator{}, stateMap, cConfigStub{}, nil)
		Expect(netConf.Teardown(vmi)).NotTo(Succeed())
	})

	Context("tap provisioning mode", func() {
		const secondaryNetworkName = "secondary-net"

		var resolveCount int

		newNetConfWithResolver := func(resolver netsetup.TapProvisioningResolver) *netsetup.NetConf {
			return netsetup.NewNetConfWithCustomFactoryAndConfigState(nsNoopFactory, &tempCacheCreator{}, stateMap, cConfigStub{}, resolver)
		}

		countingResolver := func(external bool) netsetup.TapProvisioningResolver {
			return func(_ *v1.VirtualMachineInstance) (bool, error) {
				resolveCount++
				return external, nil
			}
		}

		BeforeEach(func() {
			resolveCount = 0
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{{
				Name:    secondaryNetworkName,
				Binding: &v1.PluginBinding{Name: "bpfbridge"},
			}}
			vmi.Spec.Networks = []v1.Network{{
				Name:          secondaryNetworkName,
				NetworkSource: v1.NetworkSource{Pod: &v1.PodNetwork{}},
			}}
		})

		It("fails the setup when the resolver fails, and succeeds on retry", func() {
			resolverErr := fmt.Errorf("node is unreachable")
			failOnce := func(_ *v1.VirtualMachineInstance) (bool, error) {
				if resolveCount == 0 {
					resolveCount++
					return false, resolverErr
				}
				return true, nil
			}
			netConf := newNetConfWithResolver(failOnce)

			err := netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)
			Expect(err).To(MatchError(ContainSubstring("failed to resolve tap provisioning mode")))
			Expect(err).To(MatchError(ContainSubstring(resolverErr.Error())))

			Expect(netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())
		})

		It("resolves the mode once per launcher pod", func() {
			netConf := newNetConfWithResolver(countingResolver(true))

			Expect(netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())
			Expect(netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())

			Expect(resolveCount).To(Equal(1))
		})

		It("re-resolves the mode for a replacement launcher pod", func() {
			netConf := newNetConfWithResolver(countingResolver(true))

			Expect(netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())
			const replacementLauncherPid = launcherPid + 1
			Expect(netConf.Setup(vmi, vmi.Spec.Networks, replacementLauncherPid)).To(Succeed())

			Expect(resolveCount).To(Equal(2))
		})

		It("does not consult the resolver for a VMI without secondary bpfbridge networks", func() {
			netConf := newNetConfWithResolver(func(_ *v1.VirtualMachineInstance) (bool, error) {
				return false, fmt.Errorf("must not be called")
			})
			vmi.Spec.Domain.Devices.Interfaces[0].Binding = nil
			vmi.Spec.Domain.Devices.Interfaces[0].InterfaceBindingMethod = v1.InterfaceBindingMethod{Masquerade: &v1.InterfaceMasquerade{}}

			Expect(netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())
		})

		newNetConfWithCacheCreator := func(creator *tempCacheCreator) *netsetup.NetConf {
			return netsetup.NewNetConfWithCustomFactoryAndConfigState(
				nsNoopFactory, creator, map[string]*netpod.State{}, cConfigStub{}, countingResolver(true))
		}

		It("reuses the persisted mode across a virt-handler restart", func() {
			sharedCacheCreator := &tempCacheCreator{}
			netConf := newNetConfWithCacheCreator(sharedCacheCreator)
			Expect(netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())
			Expect(resolveCount).To(Equal(1))

			// A restarted virt-handler loses the in-memory state but keeps the disk cache.
			restartedNetConf := newNetConfWithCacheCreator(sharedCacheCreator)
			Expect(restartedNetConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())

			Expect(resolveCount).To(Equal(1))
		})

		It("re-resolves the mode when the persisted file is corrupt", func() {
			sharedCacheCreator := &tempCacheCreator{}
			netConf := newNetConfWithCacheCreator(sharedCacheCreator)
			Expect(netConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())
			Expect(resolveCount).To(Equal(1))

			corruptFiles(sharedCacheCreator.tmpDir, ".tap-provision-mode")

			restartedNetConf := newNetConfWithCacheCreator(sharedCacheCreator)
			Expect(restartedNetConf.Setup(vmi, vmi.Spec.Networks, launcherPid)).To(Succeed())

			Expect(resolveCount).To(Equal(2))
		})
	})
})

var _ = Describe("PodTapProvisioningResolver", func() {
	const (
		nodeName      = "node01"
		otherNodeName = "node02"
		namespace     = "default"
	)

	const (
		tapAnnotation      = netsetup.TapProvisionByDVPAnnotation
		networksStatusAnno = netsetup.NetworksStatusAnnotation
	)

	newPod := func(name string, uid types.UID, annotations map[string]string) *k8sv1.Pod {
		return &k8sv1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   namespace,
				UID:         uid,
				Labels:      map[string]string{v1.CreatedByLabel: "123"},
				Annotations: annotations,
			},
		}
	}

	newVMI := func(activePods map[types.UID]string) *v1.VirtualMachineInstance {
		return &v1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{UID: "123", Name: "vmi1", Namespace: namespace},
			Status:     v1.VirtualMachineInstanceStatus{ActivePods: activePods},
		}
	}

	resolveWith := func(vmi *v1.VirtualMachineInstance, pods ...*k8sv1.Pod) (bool, error) {
		objs := make([]runtime.Object, 0, len(pods))
		for _, pod := range pods {
			objs = append(objs, pod)
		}
		return netsetup.PodTapProvisioningResolver(k8sfake.NewSimpleClientset(objs...), nodeName)(vmi)
	}

	DescribeTable("resolves the mode from the frozen pod annotation",
		func(annotations map[string]string, expectedExternal bool) {
			pod := newPod("launcher", "pod-uid-1", annotations)
			vmi := newVMI(map[types.UID]string{"pod-uid-1": nodeName})
			external, err := resolveWith(vmi, pod)
			Expect(err).NotTo(HaveOccurred())
			Expect(external).To(Equal(expectedExternal))
		},
		Entry("native", map[string]string{networksStatusAnno: "[]", tapAnnotation: "true"}, false),
		Entry("external", map[string]string{networksStatusAnno: "[]"}, true),
	)

	It("fails while the SDN has not configured the pod yet", func() {
		pod := newPod("launcher", "pod-uid-1", map[string]string{tapAnnotation: "true"})
		vmi := newVMI(map[types.UID]string{"pod-uid-1": nodeName})
		_, err := resolveWith(vmi, pod)
		Expect(err).To(MatchError(ContainSubstring("has not configured")))
	})

	It("fails when the VMI has no active launcher pod on the node", func() {
		pod := newPod("launcher", "pod-uid-1", map[string]string{networksStatusAnno: "[]"})
		vmi := newVMI(map[types.UID]string{"pod-uid-1": otherNodeName})
		_, err := resolveWith(vmi, pod)
		Expect(err).To(MatchError(ContainSubstring("no active launcher pod")))
	})

	It("ignores a pod that is not active anymore", func() {
		stale := newPod("stale", "stale-uid", map[string]string{networksStatusAnno: "[]"})
		active := newPod("active", "pod-uid-1", map[string]string{networksStatusAnno: "[]", tapAnnotation: "true"})
		vmi := newVMI(map[types.UID]string{"pod-uid-1": nodeName})
		external, err := resolveWith(vmi, stale, active)
		Expect(err).NotTo(HaveOccurred())
		Expect(external).To(BeFalse())
	})

	It("fails on two active pods with disagreeing annotations", func() {
		one := newPod("one", "pod-uid-1", map[string]string{networksStatusAnno: "[]", tapAnnotation: "true"})
		two := newPod("two", "pod-uid-2", map[string]string{networksStatusAnno: "[]"})
		vmi := newVMI(map[types.UID]string{"pod-uid-1": nodeName, "pod-uid-2": nodeName})
		_, err := resolveWith(vmi, one, two)
		Expect(err).To(MatchError(ContainSubstring("disagree")))
	})

	It("resolves two active pods with agreeing annotations", func() {
		one := newPod("one", "pod-uid-1", map[string]string{networksStatusAnno: "[]"})
		two := newPod("two", "pod-uid-2", map[string]string{networksStatusAnno: "[]"})
		vmi := newVMI(map[types.UID]string{"pod-uid-1": nodeName, "pod-uid-2": nodeName})
		external, err := resolveWith(vmi, one, two)
		Expect(err).NotTo(HaveOccurred())
		Expect(external).To(BeTrue())
	})
})

// corruptFiles truncates every file with the given base name under root to invalid JSON,
// emulating a crash mid-write.
func corruptFiles(root, baseName string) {
	GinkgoHelper()
	var matches []string
	Expect(filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Base(path) == baseName {
			matches = append(matches, path)
		}
		return nil
	})).To(Succeed())
	Expect(matches).NotTo(BeEmpty(), "no %s file found under %s", baseName, root)
	for _, path := range matches {
		Expect(os.WriteFile(path, []byte("{trunc"), 0o600)).To(Succeed())
	}
}

type netnsStub struct {
	shouldFail bool
}

func (n netnsStub) Do(func() error) error {
	if n.shouldFail {
		return fmt.Errorf("do-netns failure")
	}
	return nil
}
func nsNoopFactory(_ int) netsetup.NSExecutor    { return netnsStub{} }
func nsFailureFactory(_ int) netsetup.NSExecutor { return netnsStub{shouldFail: true} }

type tempCacheCreator struct {
	once   sync.Once
	tmpDir string
}

func (c *tempCacheCreator) New(filePath string) *cache.Cache {
	c.once.Do(func() {
		tmpDir, err := os.MkdirTemp("", "temp-cache")
		if err != nil {
			panic("Unable to create temp cache directory")
		}
		c.tmpDir = tmpDir
	})
	return cache.NewCustomCache(filePath, kfs.NewWithRootPath(c.tmpDir))
}

type failingCacheCreator struct{}

func (c failingCacheCreator) New(path string) *cache.Cache {
	return cache.NewCustomCache(path, stubFS{failRemove: true})
}

type stubFS struct{ failRemove bool }

func (f stubFS) Stat(name string) (os.FileInfo, error)                          { return nil, nil }
func (f stubFS) MkdirAll(path string, perm os.FileMode) error                   { return nil }
func (f stubFS) ReadFile(filename string) ([]byte, error)                       { return nil, nil }
func (f stubFS) WriteFile(filename string, data []byte, perm fs.FileMode) error { return nil }
func (f stubFS) RemoveAll(path string) error {
	if f.failRemove {
		return fmt.Errorf("remove failed")
	}
	return nil
}
func (f stubFS) Walk(root string, walkFn filepath.WalkFunc) error { return nil }

type stateCacheStub struct {
	stateCache map[string]cache.PodIfaceState
}

func newConfigStateCacheStub() stateCacheStub {
	return stateCacheStub{map[string]cache.PodIfaceState{}}
}

func (c stateCacheStub) Read(key string) (cache.PodIfaceState, error) {
	return c.stateCache[key], nil
}

func (c stateCacheStub) Keys() ([]string, error) {
	var keys []string
	for k := range c.stateCache {
		keys = append(keys, k)
	}
	return keys, nil
}

func (c stateCacheStub) Write(key string, state cache.PodIfaceState) error {
	c.stateCache[key] = state
	return nil
}

func (c stateCacheStub) Delete(key string) error {
	delete(c.stateCache, key)
	return nil
}

type nsExecutorStub struct {
	shouldNotBeExecuted bool
}

func (n nsExecutorStub) Do(f func() error) error {
	Expect(n.shouldNotBeExecuted).To(BeFalse(), "The namespace executor shouldn't be invoked")
	return f()
}

type cConfigStub struct{}

func (c cConfigStub) GetNetworkBindings() map[string]v1.InterfaceBindingPlugin {
	return map[string]v1.InterfaceBindingPlugin{}
}
