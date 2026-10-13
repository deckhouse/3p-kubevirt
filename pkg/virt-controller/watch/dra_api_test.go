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

package watch

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"

	"kubevirt.io/client-go/kubecli"
)

var _ = Describe("resource.k8s.io/v1 discovery", func() {
	DescribeTable("should report whether the cluster serves resource.k8s.io/v1",
		func(groupVersions []string, expected bool) {
			resources := make([]*metav1.APIResourceList, 0, len(groupVersions))
			for _, gv := range groupVersions {
				resources = append(resources, &metav1.APIResourceList{GroupVersion: gv})
			}
			client := kubecli.NewMockKubevirtClient(gomock.NewController(GinkgoT()))
			client.EXPECT().DiscoveryClient().Return(&fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{Resources: resources}})

			served, err := isResourceV1Served(client)
			Expect(err).ToNot(HaveOccurred())
			Expect(served).To(Equal(expected))
		},
		Entry("Kubernetes 1.34+", []string{"v1", "resource.k8s.io/v1", "resource.k8s.io/v1beta2"}, true),
		Entry("Kubernetes 1.33", []string{"v1", "resource.k8s.io/v1beta1", "resource.k8s.io/v1beta2"}, false),
		Entry("no DRA API at all", []string{"v1", "apps/v1"}, false),
	)
})
