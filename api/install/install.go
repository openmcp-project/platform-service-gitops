// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

// Package install registers all API group schemes.
package install

import (
	"k8s.io/apimachinery/pkg/runtime"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
)

// AddToScheme adds all registered API groups to the given scheme.
func AddToScheme(s *runtime.Scheme) error {
	if err := corev1alpha1.AddToScheme(s); err != nil {
		return err
	}
	return githubv1alpha1.AddToScheme(s)
}
