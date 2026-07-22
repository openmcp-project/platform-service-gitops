// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package mcpclient_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openmcp-project/platform-service-gitops/internal/mcpclient"
)

func TestResolve_MissingSecret(t *testing.T) {
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	cl := fake.NewClientBuilder().WithScheme(sc).Build()

	_, err := mcpclient.Resolve(context.Background(), cl, "my-mcp")
	assert.ErrorContains(t, err, "kubeconfig Secret")
}

func TestResolve_MissingKey(t *testing.T) {
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	// control-plane-operator writes "flux-kubeconfig" in namespace "cp-<name>"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "flux-kubeconfig", Namespace: "cp-my-mcp"},
		Data:       map[string][]byte{},
	}
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(secret).Build()

	_, err := mcpclient.Resolve(context.Background(), cl, "my-mcp")
	assert.ErrorContains(t, err, `missing key "kubeconfig"`)
}

func TestResolve_InvalidKubeconfig(t *testing.T) {
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "flux-kubeconfig", Namespace: "cp-my-mcp"},
		Data:       map[string][]byte{"kubeconfig": []byte("not-valid-yaml")},
	}
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(secret).Build()

	_, err := mcpclient.Resolve(context.Background(), cl, "my-mcp")
	assert.ErrorContains(t, err, "building REST config")
}

func TestResolver_DoesNotCacheErrors(t *testing.T) {
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	// Invalid kubeconfig — Resolve will fail.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "flux-kubeconfig", Namespace: "cp-my-mcp"},
		Data:       map[string][]byte{"kubeconfig": []byte("not-valid")},
	}
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(secret).Build()
	resolver := mcpclient.NewResolver(cl)

	_, err1 := resolver.Resolve(context.Background(), "my-mcp")
	assert.Error(t, err1)

	// Second call must also return an error — the first failure must not be cached.
	_, err2 := resolver.Resolve(context.Background(), "my-mcp")
	assert.Error(t, err2)
}
