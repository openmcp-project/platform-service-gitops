// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/openmcp-project/controller-utils/pkg/clusters"

	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
	controller "github.com/openmcp-project/platform-service-gitops/internal/controller/github"
)

func reconcileInstance(objs []any) *githubv1alpha1.GitHubInstance {
	cl := fake_ClientBuilder(objs).Build()
	// In unit tests the platform cluster cache is not used (no SetupWithManager call),
	// so we supply a no-op fake cluster for the constructor.
	fakePlatformCluster := clusters.NewTestClusterFromClient("platform", cl)
	r := controller.NewGitHubInstanceReconciler(cl, fakePlatformCluster.Cluster(), credNamespace)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: instanceName},
	})
	Expect(err).NotTo(HaveOccurred())

	out := &githubv1alpha1.GitHubInstance{}
	Expect(cl.Get(context.Background(), types.NamespacedName{Name: instanceName}, out)).To(Succeed())
	return out
}

var _ = Describe("GitHubInstanceReconciler", func() {
	instance := &githubv1alpha1.GitHubInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instanceName},
		Spec: githubv1alpha1.GitHubInstanceSpec{
			SecretRef: githubv1alpha1.SecretReference{Name: instanceName},
		},
	}

	Context("when all referenced secrets are present and valid", func() {
		It("sets Ready=True", func() {
			out := reconcileInstance([]any{instance.DeepCopy(), newSecret(validSecretData())})
			c := meta.FindStatusCondition(out.Status.Conditions, "Ready")
			Expect(c).NotTo(BeNil())
			Expect(c.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("when a referenced secret is missing", func() {
		It("sets Ready=False with SecretNotFound", func() {
			out := reconcileInstance([]any{instance.DeepCopy()})
			c := meta.FindStatusCondition(out.Status.Conditions, "Ready")
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal("SecretNotFound"))
		})
	})

	Context("when a referenced secret is malformed", func() {
		It("sets Ready=False with SecretMalformed", func() {
			bad := map[string]string{"appID": "not-a-number", "privateKey": "x", "url": "https://github.com"}
			out := reconcileInstance([]any{instance.DeepCopy(), newSecret(bad)})
			c := meta.FindStatusCondition(out.Status.Conditions, "Ready")
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal("SecretMalformed"))
		})
	})
})
