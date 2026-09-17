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
 */

package network

import (
	"fmt"
	"runtime"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vishvananda/netlink"
	"go.uber.org/mock/gomock"

	v1 "kubevirt.io/api/core/v1"

	dutils "kubevirt.io/kubevirt/pkg/ephemeral-disk-utils"
	"kubevirt.io/kubevirt/pkg/network/cache"
	"kubevirt.io/kubevirt/pkg/network/dhcp"
	netdriver "kubevirt.io/kubevirt/pkg/network/driver"
	"kubevirt.io/kubevirt/pkg/network/namescheme"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

var _ = Describe("podNIC", func() {
	var (
		mockNetwork          *netdriver.MockNetworkHandler
		baseCacheCreator     tempCacheCreator
		mockDHCPConfigurator *dhcp.MockConfigurator
		ctrl                 *gomock.Controller
	)

	newPhase2PodNICWithMocks := func(vmi *v1.VirtualMachineInstance) (*podNIC, error) {
		podnic, err := newPodNIC(vmi, &vmi.Spec.Networks[0], &vmi.Spec.Domain.Devices.Interfaces[0], mockNetwork, &baseCacheCreator, nil)
		if err != nil {
			return nil, err
		}
		podnic.dhcpConfigurator = mockDHCPConfigurator
		return podnic, nil
	}
	BeforeEach(func() {
		dutils.MockDefaultOwnershipManager()

		ctrl = gomock.NewController(GinkgoT())
		mockNetwork = netdriver.NewMockNetworkHandler(ctrl)
		mockDHCPConfigurator = dhcp.NewMockConfigurator(ctrl)
	})
	AfterEach(func() {
		Expect(baseCacheCreator.New("").Delete()).To(Succeed())
	})

	When("DHCP config is correctly read", func() {
		var (
			podnic *podNIC
			domain *api.Domain
			vmi    *v1.VirtualMachineInstance
		)
		BeforeEach(func() {
			var err error
			domain = NewDomainWithBridgeInterface()
			vmi = newVMIBridgeInterface("testnamespace", "testVmName")
			api.NewDefaulter(runtime.GOARCH).SetObjectDefaults_Domain(domain)
			podnic, err = newPhase2PodNICWithMocks(vmi)
			Expect(err).ToNot(HaveOccurred())
			Expect(
				cache.WriteDHCPInterfaceCache(
					podnic.cacheCreator,
					getPIDString(podnic.launcherPID),
					podnic.podInterfaceName,
					&cache.DHCPConfig{Name: podnic.podInterfaceName},
				),
			).To(Succeed())
			Expect(
				cache.WriteDomainInterfaceCache(
					podnic.cacheCreator,
					getPIDString(podnic.launcherPID),
					podnic.vmiSpecIface.Name,
					&domain.Spec.Devices.Interfaces[0],
				),
			).To(Succeed())
		})
		Context("and starting the DHCP server fails", func() {
			BeforeEach(func() {
				mockDHCPConfigurator.EXPECT().Generate().Return(&cache.DHCPConfig{}, nil)
				mockDHCPConfigurator.EXPECT().EnsureDHCPServerStarted(gomock.Any(), gomock.Any(), gomock.Any()).Return(fmt.Errorf("Fake EnsureDHCPServerStarted failure"))
				podnic.domainGenerator = &fakeLibvirtSpecGenerator{
					shouldGenerateFail: false,
				}
			})
			It("phase2 should panic", func() {
				Expect(func() { _ = podnic.PlugPhase2(domain) }).To(Panic())
			})
		})
		Context("and the libvirt spec generator fails", func() {
			BeforeEach(func() {
				podnic.domainGenerator = &fakeLibvirtSpecGenerator{
					shouldGenerateFail: true,
				}
			})
			It("phase2 should report the error instead of killing virt-launcher", func() {
				Expect(podnic.PlugPhase2(domain)).To(
					MatchError(ContainSubstring("Fake LibvirtSpecGenerator.Generate failure")))
			})
		})
		Context("and starting the DHCP server succeed", func() {
			BeforeEach(func() {
				dhcpConfig := &cache.DHCPConfig{}
				mockDHCPConfigurator.EXPECT().Generate().Return(dhcpConfig, nil)
				mockDHCPConfigurator.EXPECT().EnsureDHCPServerStarted(namescheme.PrimaryPodInterfaceName, *dhcpConfig, vmi.Spec.Domain.Devices.Interfaces[0].DHCPOptions).Return(nil)
				podnic.domainGenerator = &fakeLibvirtSpecGenerator{
					shouldGenerateFail: false,
				}
				podnic.podInterfaceName = namescheme.PrimaryPodInterfaceName
			})
			It("phase2 should succeed", func() {
				Expect(podnic.PlugPhase2(domain)).To(Succeed())
			})

		})
	})

	When("a secondary pod network is plugged", func() {
		const secondaryNetworkName = "veth_n80b42d9f"

		var (
			vmi    *v1.VirtualMachineInstance
			domain *api.Domain
		)

		BeforeEach(func() {
			domain = NewDomainWithBridgeInterface()
			vmi = newVMIBridgeInterface("testnamespace", "testVmName")
			vmi.Spec.Networks = append(vmi.Spec.Networks, v1.Network{
				Name:          secondaryNetworkName,
				NetworkSource: v1.NetworkSource{Pod: &v1.PodNetwork{}},
			})
			vmi.Spec.Domain.Devices.Interfaces = append(vmi.Spec.Domain.Devices.Interfaces, v1.Interface{
				Name:    secondaryNetworkName,
				Binding: &v1.PluginBinding{Name: "bpfbridge"},
			})
		})

		newSecondaryPhase2PodNIC := func() (*podNIC, error) {
			return newPhase2PodNIC(
				vmi,
				&vmi.Spec.Networks[1],
				&vmi.Spec.Domain.Devices.Interfaces[1],
				mockNetwork,
				&baseCacheCreator,
				domain,
				string(v1.Tap),
			)
		}

		It("should fail when the pod interface is not in the pod yet", func() {
			mockNetwork.EXPECT().LinkByName(secondaryNetworkName).Return(nil, netlink.LinkNotFoundError{})

			_, err := newSecondaryPhase2PodNIC()
			Expect(err).To(MatchError(ContainSubstring(secondaryNetworkName)))
		})

		It("should resolve the pod interface name when the link is in the pod", func() {
			mockNetwork.EXPECT().LinkByName(secondaryNetworkName).Return(
				&netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: secondaryNetworkName}}, nil)

			podnic, err := newSecondaryPhase2PodNIC()
			Expect(err).ToNot(HaveOccurred())
			Expect(podnic.podInterfaceName).To(Equal(secondaryNetworkName))
		})
	})
})

type fakeLibvirtSpecGenerator struct {
	shouldGenerateFail bool
}

func (b *fakeLibvirtSpecGenerator) Generate() error {
	if b.shouldGenerateFail {
		return fmt.Errorf("Fake LibvirtSpecGenerator.Generate failure")
	}
	return nil

}
