package usb

import (
	"context"
	"fmt"
	"strings"

	expect "github.com/google/goexpect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	pkgUtil "kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/virt-config/featuregate"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"

	"kubevirt.io/kubevirt/tests/console"
	"kubevirt.io/kubevirt/tests/decorators"
	"kubevirt.io/kubevirt/tests/framework/kubevirt"
	"kubevirt.io/kubevirt/tests/libdomain"
	"kubevirt.io/kubevirt/tests/libkubevirt"
	kvconfig "kubevirt.io/kubevirt/tests/libkubevirt/config"
	"kubevirt.io/kubevirt/tests/libnode"
	"kubevirt.io/kubevirt/tests/libvmifact"
	"kubevirt.io/kubevirt/tests/libwait"
	"kubevirt.io/kubevirt/tests/testsuite"
)

const (
	failedDeleteVMI = "Failed to delete VMI"
	cmdNumberUSBs   = "dmesg | grep -c idVendor=46f4"
)

var _ = Describe("[sig-compute][USB] host USB Passthrough", Serial, decorators.SigCompute, decorators.USB, func() {
	var virtClient kubecli.KubevirtClient
	var config v1.KubeVirtConfiguration
	var vmi *v1.VirtualMachineInstance

	BeforeEach(func() {
		virtClient = kubevirt.Client()
		kv := libkubevirt.GetCurrentKv(virtClient)
		config = kv.Spec.Configuration

		nodeName := libnode.GetNodeNameWithHandler()
		Expect(nodeName).ToNot(BeEmpty())

		// Emulated USB devices only on c9s providers. Remove this when sig-compute 1.26 is the
		// oldest sig-compute with test with.
		// See: https://github.com/kubevirt/project-infra/pull/2922
		stdout, err := libnode.ExecuteCommandInVirtHandlerPod(nodeName, []string{"dmesg"})
		Expect(err).ToNot(HaveOccurred())
		if strings.Count(stdout, "idVendor=46f4") == 0 {
			Skip("No emulated USB devices present for functional test.")
		}

		vmi = libvmifact.NewCirros()
	})

	AfterEach(func() {
		// Make sure to delete the VMI before ending the test otherwise a device could still be taken
		const deleteTimeout = 180
		err := virtClient.VirtualMachineInstance(
			testsuite.NamespaceTestDefault).Delete(context.Background(), vmi.ObjectMeta.Name, metav1.DeleteOptions{})
		Expect(err).ToNot(HaveOccurred(), failedDeleteVMI)

		libwait.WaitForVirtualMachineToDisappearWithTimeout(vmi, deleteTimeout)
	})

	Context("with usb storage", func() {
		DescribeTable("with emulated USB devices", func(deviceNames []string) {
			const resourceName = "kubevirt.io/usb-storage"

			By("Adding the emulated USB device to the permitted host devices")
			config.DeveloperConfiguration = &v1.DeveloperConfiguration{
				FeatureGates: []string{featuregate.HostDevicesGate},
			}
			config.PermittedHostDevices = &v1.PermittedHostDevices{
				USB: []v1.USBHostDevice{
					{
						ResourceName: resourceName,
						Selectors: []v1.USBSelector{
							{
								Vendor:  "46f4",
								Product: "0001",
							},
						},
					},
				},
			}
			kvconfig.UpdateKubeVirtConfigValueAndWait(config)

			By("Creating a Fedora VMI with the usb host device")
			hostDevs := []v1.HostDevice{}
			for i, name := range deviceNames {
				hostDevs = append(hostDevs, v1.HostDevice{
					Name:       fmt.Sprintf("usb-%d-%s", i, name),
					DeviceName: resourceName,
				})
			}

			var err error
			vmi.Spec.Domain.Devices.HostDevices = hostDevs
			vmi, err = virtClient.VirtualMachineInstance(testsuite.NamespaceTestDefault).Create(context.Background(), vmi, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			vmi = libwait.WaitForSuccessfulVMIStart(vmi)
			Expect(console.LoginToCirros(vmi)).To(Succeed())

			By("Making sure the usb is present inside the VMI")
			const expectTimeout = 15
			Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
				&expect.BSnd{S: fmt.Sprintf("%s\n", cmdNumberUSBs)},
				&expect.BExp{R: console.RetValue(fmt.Sprintf("%d", len(deviceNames)))},
			}, expectTimeout)).To(Succeed(), "Device not found")

			By("Verifying ownership is properly set in the host")
			domainXML, err := libdomain.GetRunningVMIDomainSpec(vmi)
			Expect(err).ToNot(HaveOccurred())

			for _, hostDevice := range domainXML.Devices.HostDevices {
				if hostDevice.Type != api.HostDeviceUSB {
					continue
				}
				addr := hostDevice.Source.Address
				path := fmt.Sprintf("%sdev/bus/usb/00%s/00%s", pkgUtil.HostRootMount, addr.Bus, addr.Device)
				cmd := []string{"stat", "--printf", `"%u %g"`, path}
				stdout, err := libnode.ExecuteCommandInVirtHandlerPod(vmi.Status.NodeName, cmd)
				Expect(err).ToNot(HaveOccurred())
				Expect(stdout).Should(Equal(`"107 107"`))
			}
		},
			Entry("Should successfully passthrough 1 emulated USB device", []string{"slow-storage"}),
			Entry("Should successfully passthrough 2 emulated USB devices", []string{"fast-storage", "low-storage"}),
		)
	})
})
