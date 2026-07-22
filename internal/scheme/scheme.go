// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

// Package scheme holds the runtime schemes for the three cluster roles:
// Platform (where the controller runs), Onboarding (watched by the manager),
// and MCP (dynamically accessed per propagateTo target).
package scheme

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	clustersv1alpha1 "github.com/openmcp-project/openmcp-operator/api/clusters/v1alpha1"
	corev2alpha1 "github.com/openmcp-project/openmcp-operator/api/core/v2alpha1"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
)

var (
	// Platform is the scheme for the platform cluster (GitHubInstance, credential Secrets).
	Platform = runtime.NewScheme()

	// Onboarding is the scheme for the onboarding cluster (GitRepository, AppInstallation, etc.).
	// The manager watches this cluster.
	Onboarding = runtime.NewScheme()

	// MCP is the scheme for MCP clusters (Flux GitRepository, corev1 Secrets).
	MCP = runtime.NewScheme()
)

func init() {
	initPlatform()
	initOnboarding()
	initMCP()
}

func initPlatform() {
	utilruntime.Must(clientgoscheme.AddToScheme(Platform))
	utilruntime.Must(clustersv1alpha1.AddToScheme(Platform))
	utilruntime.Must(githubv1alpha1.AddToScheme(Platform))
}

func initOnboarding() {
	utilruntime.Must(clientgoscheme.AddToScheme(Onboarding))
	utilruntime.Must(corev1alpha1.AddToScheme(Onboarding))
	utilruntime.Must(githubv1alpha1.AddToScheme(Onboarding))
	utilruntime.Must(corev2alpha1.AddToScheme(Onboarding))
	utilruntime.Must(clustersv1alpha1.AddToScheme(Onboarding))
	utilruntime.Must(sourcev1.AddToScheme(Onboarding))
}

func initMCP() {
	utilruntime.Must(clientgoscheme.AddToScheme(MCP))
	utilruntime.Must(sourcev1.AddToScheme(MCP))
}
