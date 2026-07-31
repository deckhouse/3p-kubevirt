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
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"

	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/certificates/triple/cert"
	"kubevirt.io/kubevirt/pkg/virt-operator/resource/generate/components"
)

// minCertificateWakeup bounds the requeue delay of SyncCertificates. A rotation
// deadline in the past would otherwise reach workqueue.AddAfter, which turns any
// non-positive delay into an immediate Add. What keeps the reconciler from
// spinning is refusing to issue certificates at all in that situation; this is
// only the second line of defence, and it is the sole guaranteed requeue on the
// path where the caller reports no error.
const minCertificateWakeup = time.Minute

// ErrCANotUsableForRotation reports that the certificates cannot be renewed
// without the full sync rotating the CA first. It is not a failure of this
// component, but it does mean the certificates will expire if the full sync
// stays broken, so the caller is expected to surface it.
var ErrCANotUsableForRotation = errors.New("the CA cannot be used to issue certificates")

// SyncCertificates rotates the certificate secrets of the target strategy that
// already exist in the informer cache, signing them with the CA that is
// currently trusted by the kubevirt-ca bundle. Unlike Sync, it depends neither
// on the install strategy config map nor on any other component being
// reconcilable, so it keeps TLS alive while the full sync cannot run. Use it
// with the strategy from install.NewCertsOnlyStrategy.
//
// It never creates missing secrets and never rotates the CA: doing either
// requires updating the CA bundle config maps and the webhook caBundles in the
// right order, which stays in the full sync. Consequently it refuses to work
// once the CA is inside its own renewal window — the leaves cannot outlive the
// CA, so the only useful action there is to let the full sync recover.
func (r *Reconciler) SyncCertificates(queue workqueue.TypedRateLimitingInterface[string], caCert *tls.Certificate) error {
	selfSignedConfig := r.kv.Spec.CertificateRotationStrategy.SelfSigned
	duration := GetCertDuration(selfSignedConfig)
	renewBefore := GetCertRenewBefore(selfSignedConfig)
	caRenewBefore := GetCARenewBefore(selfSignedConfig)

	if err := r.checkCAUsableForRotation(caCert, caRenewBefore); err != nil {
		return fmt.Errorf("%w: %v", ErrCANotUsableForRotation, err)
	}

	var rotationErrors []error

	for _, secret := range r.targetStrategy.CertificateSecrets() {
		if secret.Name == components.KubeVirtCASecretName || secret.Name == components.KubeVirtExportCASecretName {
			continue
		}

		cachedSecret, err := r.cachedSecret(secret)
		if err != nil {
			rotationErrors = append(rotationErrors, err)
			continue
		}
		if cachedSecret == nil {
			continue
		}

		crt, err := r.rotateCertificateSecret(cachedSecret, secret, caCert, duration, renewBefore, caRenewBefore)
		if err != nil {
			// Keep going: an unwritable secret must not starve the rotation of
			// the remaining ones.
			rotationErrors = append(rotationErrors, err)
			continue
		}
		if crt == nil {
			continue
		}

		// we need to ensure that we revisit certificates before they expire
		wakeupDeadline := time.Until(components.NextRotationDeadline(crt, caCert, renewBefore, caRenewBefore))
		if wakeupDeadline <= 0 {
			// The certificate was just issued, so its deadline has to be in the
			// future. If it is not, the configured durations or the CA itself
			// make every reconcile rotate this secret again; say so once instead
			// of doing it quietly.
			rotationErrors = append(rotationErrors,
				fmt.Errorf("the certificate issued for secret %s is already due for rotation, refusing to keep reissuing it", secret.Name))
			continue
		}
		if wakeupDeadline < minCertificateWakeup {
			wakeupDeadline = minCertificateWakeup
		}
		queue.AddAfter(r.kvKey, wakeupDeadline)
	}

	return errors.Join(rotationErrors...)
}

// checkCAUsableForRotation reports why the cached CA must not be used to issue
// new leaf certificates.
func (r *Reconciler) checkCAUsableForRotation(caCert *tls.Certificate, caRenewBefore *metav1.Duration) error {
	if caCert == nil || caCert.Leaf == nil {
		return fmt.Errorf("the CA certificate is not loaded")
	}

	if _, ok := caCert.PrivateKey.(*ecdsa.PrivateKey); !ok {
		// components.PopulateSecretWithCertificate asserts this type without
		// checking it, which would panic the operator.
		return fmt.Errorf("the CA private key is not an ECDSA key")
	}

	// Once the CA is inside its own renewal window it is the CA that has to be
	// rotated, and only the full sync can do that together with the bundles and
	// the webhook caBundles. Issuing leaves here would also be pointless: their
	// rotation deadline is derived from the CA lifetime and would already be in
	// the past, so every reconcile would rotate them again. It is not the only
	// way to end up with a deadline in the past, which is why the issued
	// certificate is checked again afterwards.
	if caRenewBefore == nil {
		return fmt.Errorf("the CA renewal interval is not set")
	}
	renewAt := caCert.Leaf.NotAfter.Add(-caRenewBefore.Duration)
	if !time.Now().Before(renewAt) {
		return fmt.Errorf("the CA expires at %v, which is inside its own renewal window", caCert.Leaf.NotAfter)
	}

	// The leaves are validated by clients against the kubevirt-ca bundle. If
	// the bundle does not carry this CA yet, signing with it would produce
	// certificates that nobody trusts.
	if err := r.checkCATrustedByBundle(caCert); err != nil {
		return err
	}

	return nil
}

func (r *Reconciler) checkCATrustedByBundle(caCert *tls.Certificate) error {
	obj, exists, err := r.stores.ConfigMapCache.GetByKey(r.kv.Namespace + "/" + components.KubeVirtCASecretName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the %s CA bundle config map does not exist yet", components.KubeVirtCASecretName)
	}

	configMap, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return fmt.Errorf("the %s CA bundle config map has an unexpected type", components.KubeVirtCASecretName)
	}

	bundle, err := cert.ParseCertsPEM([]byte(configMap.Data[components.CABundleKey]))
	if err != nil {
		return fmt.Errorf("unable to parse the %s CA bundle: %v", components.KubeVirtCASecretName, err)
	}

	for _, bundled := range bundle {
		if bundled.Equal(caCert.Leaf) {
			return nil
		}
	}

	return fmt.Errorf("the CA from the %s secret is not present in the CA bundle config map", components.KubeVirtCASecretName)
}

// cachedSecret returns the secret from the informer cache, or nil when it does
// not exist or is being deleted. Creating missing secrets is left to the full
// sync.
func (r *Reconciler) cachedSecret(secret *corev1.Secret) (*corev1.Secret, error) {
	obj, exists, err := r.stores.SecretCache.GetByKey(secret.Namespace + "/" + secret.Name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	cachedSecret, ok := obj.(*corev1.Secret)
	if !ok || cachedSecret.DeletionTimestamp != nil {
		return nil, nil
	}

	return cachedSecret, nil
}

func (r *Reconciler) rotateCertificateSecret(cachedSecret *corev1.Secret, secret *corev1.Secret, caCert *tls.Certificate, duration *metav1.Duration, renewBefore *metav1.Duration, caRenewBefore *metav1.Duration) (*tls.Certificate, error) {
	if !certificationNeedsRotation(cachedSecret, duration, caCert, renewBefore, caRenewBefore) {
		return components.LoadCertificates(cachedSecret)
	}

	secret = secret.DeepCopy()
	version, imageRegistry, id := getTargetVersionRegistryID(r.kv)
	injectOperatorMetadata(r.kv, &secret.ObjectMeta, version, imageRegistry, id, true)

	if err := components.PopulateSecretWithCertificate(secret, caCert, duration); err != nil {
		return nil, err
	}

	crt, err := components.LoadCertificates(secret)
	if err != nil {
		return nil, fmt.Errorf("unable to load the certificate generated for secret %s: %v", secret.Name, err)
	}

	patchBytes, err := createSecretPatch(secret)
	if err != nil {
		return nil, err
	}

	_, err = r.clientset.CoreV1().Secrets(secret.Namespace).Patch(context.Background(), secret.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The full sync recreates it.
			return nil, nil
		}
		return nil, fmt.Errorf("unable to patch secret %s: %v", secret.Name, err)
	}

	log.Log.V(2).Infof("secret %v rotated ahead of the full sync", secret.GetName())

	return crt, nil
}
