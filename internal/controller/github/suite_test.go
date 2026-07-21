// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
)

var scheme *runtime.Scheme

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GitHub Controller Suite")
}

var _ = BeforeSuite(func() {
	scheme = runtime.NewScheme()
	Expect(githubv1alpha1.AddToScheme(scheme)).To(Succeed())
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
})
