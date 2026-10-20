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

package virthandler

import (
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"

	api2 "kubevirt.io/client-go/api"

	checksum_controller "kubevirt.io/kubevirt/pkg/virt-handler/checksum-controller"
	cmdclient "kubevirt.io/kubevirt/pkg/virt-handler/cmd-client"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

var _ = Describe("SyncVirtualMachine", func() {
	var (
		client     *cmdclient.MockLauncherClient
		controller *VirtualMachineController
	)

	BeforeEach(func() {
		client = cmdclient.NewMockLauncherClient(gomock.NewController(GinkgoT()))
		controller = &VirtualMachineController{checksumCtrl: checksum_controller.NewController(nil, nil)}
	})

	It("should return a disk expansion error when only the disk expansion failed", func() {
		launcherErr := fmt.Errorf("server error. command SyncVMI failed: %q", api.DiskExpansionFailed+": Cannot grow device files")
		client.EXPECT().SyncVirtualMachine(gomock.Any(), gomock.Any()).Return(launcherErr)

		err := controller.SyncVirtualMachine(client, api2.NewMinimalVMI("testvmi"), nil)

		var expansionErr *diskExpansionError
		Expect(errors.As(err, &expansionErr)).To(BeTrue())
		Expect(err).To(MatchError(launcherErr.Error()))
	})

	It("should return any other error as is", func() {
		launcherErr := errors.New("server error. command SyncVMI failed: \"attaching device\"")
		client.EXPECT().SyncVirtualMachine(gomock.Any(), gomock.Any()).Return(launcherErr)

		err := controller.SyncVirtualMachine(client, api2.NewMinimalVMI("testvmi"), nil)

		Expect(err).To(BeIdenticalTo(launcherErr))
	})
})
