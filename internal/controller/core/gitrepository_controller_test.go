// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	controller "github.com/openmcp-project/platform-service-gitops/internal/controller/core"
)

var _ = Describe("GitRepositoryReconciler", func() {
	Context("when the GitRepository does not exist", func() {
		It("returns no error", func() {
			cl := fake.NewClientBuilder().WithScheme(scheme).Build()
			r := controller.NewGitRepositoryReconciler(cl)

			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: "default"},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("when a new GitRepository references a missing credential", func() {
		It("sets CredentialResolved=False and Ready=False", func() {
			gr := &corev1alpha1.GitRepository{
				ObjectMeta: metav1.ObjectMeta{Name: "my-infra", Namespace: "my-project"},
				Spec: corev1alpha1.GitRepositorySpec{
					URL: "https://github.com/my-org/my-infra",
					Ref: corev1alpha1.GitRef{Branch: "main"},
					CredentialRef: corev1alpha1.CredentialRef{
						Name:  "missing-connection",
						Kind:  "AppInstallation",
						Group: "github.gitops.open-control-plane.io",
					},
				},
			}
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(gr).
				WithObjects(gr).
				Build()
			r := controller.NewGitRepositoryReconciler(cl)

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "my-infra", Namespace: "my-project"},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &corev1alpha1.GitRepository{}
			Expect(cl.Get(context.Background(),
				types.NamespacedName{Name: "my-infra", Namespace: "my-project"},
				updated)).To(Succeed())

			credCond := findCondition(updated.Status.Conditions, "CredentialResolved")
			Expect(credCond).NotTo(BeNil(), "expected CredentialResolved condition")
			Expect(credCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(credCond.Reason).To(Equal("CredentialNotFound"))

			readyCond := findCondition(updated.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil(), "expected Ready condition")
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal("Reconciling"))
		})
	})
})

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
