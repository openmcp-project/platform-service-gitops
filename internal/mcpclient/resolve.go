// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

// Package mcpclient resolves a target MCP name to a controller-runtime client
// for that cluster.
//
// The control-plane-operator (github.com/openmcp-project/control-plane-operator)
// creates a dedicated hub namespace "cp-<controlplane-name>" for each ControlPlane
// CR and writes a Secret named "flux-kubeconfig" into it. That Secret holds a
// service-account kubeconfig (key: "kubeconfig") scoped for Flux. We read it here
// to build a cross-cluster client for the target MCP.
package mcpclient

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

var mcpScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}()

const (
	// fluxKubeconfigSecretName is the name used by control-plane-operator for the
	// Flux service-account kubeconfig Secret it writes into each ControlPlane namespace.
	fluxKubeconfigSecretName = "flux-kubeconfig"

	// cpNamespacePrefix is the hub namespace prefix applied by control-plane-operator
	// when it creates a dedicated namespace for each ControlPlane CR.
	cpNamespacePrefix = "cp-"
)

// Resolve returns a client.Client for the target MCP cluster.
//
// The control-plane-operator creates a hub namespace "cp-<mcpName>" for each
// ControlPlane and writes a Secret "flux-kubeconfig" into it with a Flux-scoped
// service-account kubeconfig (data key: "kubeconfig"). This function reads that
// Secret to build the cross-cluster client.
//
// Each call creates a new HTTP transport and REST mapper — callers should cache
// the returned client rather than calling Resolve on every reconcile.
func Resolve(ctx context.Context, c client.Client, mcpName string) (client.Client, error) {
	namespace := cpNamespacePrefix + mcpName
	secretName := fluxKubeconfigSecretName
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("kubeconfig Secret %s/%s not found: %w", namespace, secretName, err)
		}
		return nil, fmt.Errorf("fetching kubeconfig Secret %s/%s: %w", namespace, secretName, err)
	}

	kubeconfigBytes, ok := secret.Data["kubeconfig"]
	if !ok {
		return nil, fmt.Errorf("kubeconfig Secret %s/%s missing key \"kubeconfig\"", namespace, secretName)
	}

	restCfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("building REST config from kubeconfig Secret %s/%s: %w", namespace, secretName, err)
	}

	httpClient, err := rest.HTTPClientFor(restCfg)
	if err != nil {
		return nil, fmt.Errorf("building HTTP client for MCP %q: %w", mcpName, err)
	}

	mapper, err := apiutil.NewDynamicRESTMapper(restCfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("building REST mapper for MCP %q: %w", mcpName, err)
	}

	mcpClient, err := client.New(restCfg, client.Options{Mapper: mapper, Scheme: mcpScheme})
	if err != nil {
		return nil, fmt.Errorf("building client for MCP %q: %w", mcpName, err)
	}
	return mcpClient, nil
}

// Resolver is a caching wrapper around Resolve. It builds one client.Client per
// MCP name and reuses it across reconcile loops, avoiding repeated TLS handshakes
// and REST mapper discovery calls.
type Resolver struct {
	hubClient client.Client
	cache     sync.Map // key: string (mcpName), value: client.Client
}

// NewResolver returns a Resolver backed by the given hub-cluster client.
func NewResolver(hubClient client.Client) *Resolver {
	return &Resolver{hubClient: hubClient}
}

// Resolve returns a cached client.Client for the named MCP, building one on
// first access. Errors are not cached — a failed call is retried on the next
// invocation.
func (r *Resolver) Resolve(ctx context.Context, mcpName string) (client.Client, error) {
	if v, ok := r.cache.Load(mcpName); ok {
		return v.(client.Client), nil
	}
	cl, err := Resolve(ctx, r.hubClient, mcpName)
	if err != nil {
		return nil, err
	}
	// LoadOrStore is race-safe: if two goroutines built a client concurrently,
	// we use whichever was stored first and discard the other.
	actual, _ := r.cache.LoadOrStore(mcpName, cl)
	return actual.(client.Client), nil
}
