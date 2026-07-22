// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

// Package credentials resolves GitHub App credentials from a GitHubInstance and
// its referenced Secret. It is shared by the AppInstallation and GitRepository
// controllers so the resolution logic lives in one place.
package credentials

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
	"github.com/openmcp-project/platform-service-gitops/internal/githubapp"
)

// Secret data keys holding the GitHub App credentials.
const (
	SecretKeyAppID      = "appID"
	SecretKeyPrivateKey = "privateKey"
	SecretKeyURL        = "url"
)

// Resolve looks up the GitHubInstance named by instanceName, selects the
// credential Secret (by credentialName, or the sole one when empty), and reads
// the App ID, private key and URL from it. defaultNamespace is used for secret
// refs that do not specify a namespace.
func Resolve(ctx context.Context, c client.Client, instanceName, credentialName, defaultNamespace string) (githubapp.Credentials, error) {
	inst := &githubv1alpha1.GitHubInstance{}
	if err := c.Get(ctx, types.NamespacedName{Name: instanceName}, inst); err != nil {
		if apierrors.IsNotFound(err) {
			return githubapp.Credentials{}, fmt.Errorf("GitHubInstance %q not found", instanceName)
		}
		return githubapp.Credentials{}, fmt.Errorf("fetching GitHubInstance %q: %w", instanceName, err)
	}

	ref, err := SelectSecretRef(inst.Spec.SecretRefs, credentialName)
	if err != nil {
		return githubapp.Credentials{}, err
	}

	ns := ref.Namespace
	if ns == "" {
		ns = defaultNamespace
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return githubapp.Credentials{}, fmt.Errorf("credential Secret %s/%s not found", ns, ref.Name)
		}
		return githubapp.Credentials{}, fmt.Errorf("fetching credential Secret %s/%s: %w", ns, ref.Name, err)
	}

	return FromSecret(secret)
}

// SelectSecretRef picks the secret ref named by credentialName, or the sole ref
// when credentialName is empty and there is exactly one.
func SelectSecretRef(refs []githubv1alpha1.SecretReference, credentialName string) (githubv1alpha1.SecretReference, error) {
	if credentialName == "" {
		if len(refs) == 1 {
			return refs[0], nil
		}
		return githubv1alpha1.SecretReference{}, fmt.Errorf("credentialName must be set: instance lists %d secrets", len(refs))
	}
	for _, ref := range refs {
		if ref.Name == credentialName {
			return ref, nil
		}
	}
	return githubv1alpha1.SecretReference{}, fmt.Errorf("credentialName %q not found in GitHubInstance secretRefs", credentialName)
}

// FromSecret reads GitHub App credentials from a Secret's data.
func FromSecret(secret *corev1.Secret) (githubapp.Credentials, error) {
	appIDRaw, ok := secret.Data[SecretKeyAppID]
	if !ok {
		return githubapp.Credentials{}, fmt.Errorf("secret %s/%s missing key %q", secret.Namespace, secret.Name, SecretKeyAppID)
	}
	appID, err := strconv.ParseInt(string(appIDRaw), 10, 64)
	if err != nil {
		return githubapp.Credentials{}, fmt.Errorf("secret %s/%s key %q is not a valid integer: %w", secret.Namespace, secret.Name, SecretKeyAppID, err)
	}
	pk, ok := secret.Data[SecretKeyPrivateKey]
	if !ok {
		return githubapp.Credentials{}, fmt.Errorf("secret %s/%s missing key %q", secret.Namespace, secret.Name, SecretKeyPrivateKey)
	}
	url, ok := secret.Data[SecretKeyURL]
	if !ok {
		return githubapp.Credentials{}, fmt.Errorf("secret %s/%s missing key %q", secret.Namespace, secret.Name, SecretKeyURL)
	}
	return githubapp.Credentials{AppID: appID, PrivateKey: pk, URL: string(url)}, nil
}
