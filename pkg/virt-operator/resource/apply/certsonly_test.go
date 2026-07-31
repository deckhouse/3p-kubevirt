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

package apply

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/certificates/triple"
	"kubevirt.io/kubevirt/pkg/certificates/triple/cert"
	"kubevirt.io/kubevirt/pkg/virt-operator/resource/generate/components"
	"kubevirt.io/kubevirt/pkg/virt-operator/resource/generate/install"
	"kubevirt.io/kubevirt/pkg/virt-operator/util"
)

// recordingQueue captures the delays SyncCertificates requeues itself with.
type recordingQueue struct {
	workqueue.TypedRateLimitingInterface[string]
	delays []time.Duration
}

func (q *recordingQueue) AddAfter(item string, duration time.Duration) {
	q.delays = append(q.delays, duration)
}

var _ = Describe("Certs-only sync", func() {
	const namespace = "kubevirt"

	var ctrl *gomock.Controller
	var clientset *kubecli.MockKubevirtClient
	var coreclientset *fake.Clientset
	var stores util.Stores
	var queue *recordingQueue
	var reconciler *Reconciler
	var caCert *tls.Certificate
	var secretPatches int

	// newCA mints a CA whose remaining lifetime is caLifetime.
	newCA := func(caLifetime time.Duration) *tls.Certificate {
		keyPair, err := triple.NewCA("kubevirt.io", caLifetime)
		Expect(err).ToNot(HaveOccurred())

		encodedCert := cert.EncodeCertPEM(keyPair.Cert)
		crt, err := tls.X509KeyPair(encodedCert, cert.EncodePrivateKeyPEM(keyPair.Key))
		Expect(err).ToNot(HaveOccurred())
		leaf, err := cert.ParseCertsPEM(encodedCert)
		Expect(err).ToNot(HaveOccurred())
		crt.Leaf = leaf[0]

		return &crt
	}

	trustCA := func(trusted *tls.Certificate) {
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      components.KubeVirtCASecretName,
				Namespace: namespace,
			},
			Data: map[string]string{
				components.CABundleKey: string(cert.EncodeCertPEM(trusted.Leaf)),
			},
		}
		Expect(stores.ConfigMapCache.Add(configMap)).To(Succeed())
	}

	addLeafSecrets := func(duration time.Duration) int {
		secrets := components.NewCertSecrets(namespace, namespace)
		for _, secret := range secrets {
			Expect(components.PopulateSecretWithCertificate(secret, caCert, &metav1.Duration{Duration: duration})).To(Succeed())
			Expect(stores.SecretCache.Add(secret)).To(Succeed())
		}
		return len(secrets)
	}

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		coreclientset = fake.NewSimpleClientset()
		secretPatches = 0

		coreclientset.Fake.PrependReactor("*", "*", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			Expect(action).To(BeNil())
			return true, nil, nil
		})
		coreclientset.Fake.PrependReactor("patch", "secrets", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			secretPatches++
			return true, &corev1.Secret{}, nil
		})

		clientset = kubecli.NewMockKubevirtClient(ctrl)
		clientset.EXPECT().CoreV1().Return(coreclientset.CoreV1()).AnyTimes()

		stores = util.Stores{}
		stores.SecretCache = cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)
		stores.ConfigMapCache = cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)

		queue = &recordingQueue{
			TypedRateLimitingInterface: workqueue.NewTypedRateLimitingQueue[string](workqueue.DefaultTypedControllerRateLimiter[string]()),
		}

		// a CA with plenty of life left, trusted by the bundle
		caCert = newCA(Duration7d)
		trustCA(caCert)

		kv := &v1.KubeVirt{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kubevirt",
				Namespace: namespace,
			},
		}

		reconciler = &Reconciler{
			kv:             kv,
			kvKey:          namespace + "/kubevirt",
			targetStrategy: install.NewCertsOnlyStrategy(namespace, namespace),
			stores:         stores,
			clientset:      clientset,
			expectations:   &util.Expectations{},
		}
	})

	It("should patch certificate secrets that need rotation", func() {
		// the duration annotation differs from the configured default, which
		// forces a rotation
		count := addLeafSecrets(12 * time.Hour)

		Expect(reconciler.SyncCertificates(queue, caCert)).To(Succeed())
		Expect(secretPatches).To(Equal(count))
	})

	It("should not write valid certificate secrets", func() {
		addLeafSecrets(Duration1d)

		Expect(reconciler.SyncCertificates(queue, caCert)).To(Succeed())
		Expect(secretPatches).To(BeZero())
	})

	It("should not create missing certificate secrets", func() {
		Expect(reconciler.SyncCertificates(queue, caCert)).To(Succeed())
		Expect(secretPatches).To(BeZero())
	})

	It("should never requeue itself without a delay", func() {
		// A rotation deadline in the past would make workqueue.AddAfter an
		// immediate Add, spinning the reconciler. Configure a lifetime whose
		// deadline lands just ahead of now, so the clamp is what keeps the
		// delay usable.
		reconciler.kv.Spec.CertificateRotationStrategy.SelfSigned = &v1.KubeVirtSelfSignConfiguration{
			CA: &v1.CertConfig{
				Duration:    &metav1.Duration{Duration: Duration7d},
				RenewBefore: &metav1.Duration{Duration: time.Hour},
			},
			Server: &v1.CertConfig{
				Duration:    &metav1.Duration{Duration: time.Minute},
				RenewBefore: &metav1.Duration{Duration: 59 * time.Second},
			},
		}
		addLeafSecrets(12 * time.Hour)

		Expect(reconciler.SyncCertificates(queue, caCert)).To(Succeed())

		Expect(queue.delays).ToNot(BeEmpty())
		for _, delay := range queue.delays {
			Expect(delay).To(Equal(minCertificateWakeup), "a deadline this close must be clamped")
		}
	})

	It("should refuse to keep reissuing a certificate that is due immediately", func() {
		// renewBefore equal to the duration is accepted by validation and makes
		// every freshly issued certificate due at once.
		reconciler.kv.Spec.CertificateRotationStrategy.SelfSigned = &v1.KubeVirtSelfSignConfiguration{
			Server: &v1.CertConfig{
				Duration:    &metav1.Duration{Duration: time.Hour},
				RenewBefore: &metav1.Duration{Duration: time.Hour},
			},
		}
		addLeafSecrets(12 * time.Hour)

		err := reconciler.SyncCertificates(queue, caCert)
		Expect(err).To(HaveOccurred())
		Expect(queue.delays).To(BeEmpty(), "a certificate due immediately must not be requeued")
	})

	DescribeTable("should not rotate anything when the CA cannot be used", func(prepare func()) {
		addLeafSecrets(12 * time.Hour)
		prepare()

		err := reconciler.SyncCertificates(queue, caCert)
		Expect(err).To(MatchError(ErrCANotUsableForRotation),
			"the caller has to be able to tell this state apart from a failure")
		Expect(secretPatches).To(BeZero())
		Expect(queue.delays).To(BeEmpty())
	},
		Entry("because the CA is inside its own renewal window", func() {
			// default caRenewBefore is 20% of the CA duration, so a CA with
			// less than that left may only be replaced by the full sync
			caCert = newCA(Duration7d / 10)
			trustCA(caCert)
		}),
		Entry("because the CA already expired", func() {
			caCert = newCA(-time.Hour)
			trustCA(caCert)
		}),
		Entry("because the CA is not trusted by the bundle yet", func() {
			// the full sync rotated the CA secret but did not get to the
			// bundle: signing here would produce certificates nobody trusts
			caCert = newCA(Duration7d)
		}),
		Entry("because the CA bundle config map is missing", func() {
			Expect(stores.ConfigMapCache.Replace(nil, "")).To(Succeed())
		}),
		Entry("because the CA private key is not an ECDSA key", func() {
			// components.PopulateSecretWithCertificate asserts the key type
			// without checking it, which would panic the operator.
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			Expect(err).ToNot(HaveOccurred())
			rsaCA := *caCert
			rsaCA.PrivateKey = key
			caCert = &rsaCA
		}),
	)

	It("should keep rotating the remaining secrets when one of them fails", func() {
		count := addLeafSecrets(12 * time.Hour)
		Expect(count).To(BeNumerically(">", 1))

		failed := components.VirtApiCertSecretName
		coreclientset.Fake.PrependReactor("patch", "secrets", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			if action.(testing.PatchAction).GetName() == failed {
				return true, nil, errors.NewInternalError(fmt.Errorf("the apiserver rejected the patch"))
			}
			secretPatches++
			return true, &corev1.Secret{}, nil
		})

		err := reconciler.SyncCertificates(queue, caCert)
		Expect(err).To(HaveOccurred())
		Expect(secretPatches).To(Equal(count-1), "every secret except the failing one should have been rotated")
	})

	It("should ignore a secret that disappeared while rotating", func() {
		addLeafSecrets(12 * time.Hour)

		coreclientset.Fake.PrependReactor("patch", "secrets", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			return true, nil, errors.NewNotFound(schema.GroupResource{Resource: "secrets"}, action.(testing.PatchAction).GetName())
		})

		Expect(reconciler.SyncCertificates(queue, caCert)).To(Succeed())
		Expect(queue.delays).To(BeEmpty(), "a secret that is gone must not be requeued")
	})

	It("should skip secrets that are being deleted", func() {
		secrets := components.NewCertSecrets(namespace, namespace)
		for _, secret := range secrets {
			Expect(components.PopulateSecretWithCertificate(secret, caCert, &metav1.Duration{Duration: 12 * time.Hour})).To(Succeed())
			deletionTimestamp := metav1.Now()
			secret.DeletionTimestamp = &deletionTimestamp
			Expect(stores.SecretCache.Add(secret)).To(Succeed())
		}

		Expect(reconciler.SyncCertificates(queue, caCert)).To(Succeed())
		Expect(secretPatches).To(BeZero())
	})
})
