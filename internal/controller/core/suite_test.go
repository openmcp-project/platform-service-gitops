// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
)

var scheme *runtime.Scheme

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	scheme = runtime.NewScheme()
	Expect(corev1alpha1.AddToScheme(scheme)).To(Succeed())
})
