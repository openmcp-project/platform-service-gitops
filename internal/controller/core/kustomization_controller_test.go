// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"context"
	"time"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	controller "github.com/openmcp-project/platform-service-gitops/internal/controller/core"
)

var _ = Describe("KustomizationReconciler", func() {
	const (
		namespace = "my-project"
		name      = "my-workspaces"
		srcName   = "my-infra"
	)

	newKustomization := func() *corev1alpha1.Kustomization {
		return &corev1alpha1.Kustomization{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: corev1alpha1.KustomizationSpec{
				SourceRef: corev1alpha1.SourceRef{Kind: "GitRepository", Name: srcName},
				Path:      "./projects/my-project",
				Interval:  metav1.Duration{Duration: 5 * time.Minute},
				Prune:     true,
			},
		}
	}

	newGitRepository := func() *corev1alpha1.GitRepository {
		return &corev1alpha1.GitRepository{
			ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: namespace},
			Spec: corev1alpha1.GitRepositorySpec{
				URL: "https://github.com/my-org/my-infra",
				Ref: corev1alpha1.GitRef{Branch: "main"},
				CredentialRef: corev1alpha1.CredentialRef{
					Name:  "my-cred",
					Kind:  "AppInstallation",
					Group: "github.gitops.open-control-plane.io",
				},
			},
		}
	}

	Context("when the Kustomization does not exist", func() {
		It("returns no error", func() {
			cl := fake.NewClientBuilder().WithScheme(scheme).Build()
			r := controller.NewKustomizationReconciler(cl)

			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: namespace},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("when sourceRef points to a non-existent GitRepository", func() {
		It("sets SourceInvalid=True and Ready=False, does not create Flux Kustomization", func() {
			ks := newKustomization()
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(ks).
				WithObjects(ks).
				Build()
			r := controller.NewKustomizationReconciler(cl)

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &corev1alpha1.Kustomization{}
			Expect(cl.Get(context.Background(),
				types.NamespacedName{Name: name, Namespace: namespace},
				updated)).To(Succeed())

			srcCond := findCondition(updated.Status.Conditions, "SourceInvalid")
			Expect(srcCond).NotTo(BeNil(), "expected SourceInvalid condition")
			Expect(srcCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(srcCond.Reason).To(Equal("GitRepositoryNotFound"))

			readyCond := findCondition(updated.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil(), "expected Ready condition")
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))

			fluxList := &kustomizev1.KustomizationList{}
			Expect(cl.List(context.Background(), fluxList)).To(Succeed())
			Expect(fluxList.Items).To(BeEmpty())
		})
	})

	Context("when sourceRef points to a valid GitRepository", func() {
		It("creates the backing Flux Kustomization with correct spec", func() {
			ks := newKustomization()
			gr := newGitRepository()
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(ks).
				WithObjects(ks, gr).
				Build()
			r := controller.NewKustomizationReconciler(cl)

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			fluxKs := &kustomizev1.Kustomization{}
			Expect(cl.Get(context.Background(),
				types.NamespacedName{Name: name, Namespace: namespace},
				fluxKs)).To(Succeed())

			Expect(fluxKs.Spec.Path).To(Equal("./projects/my-project"))
			Expect(fluxKs.Spec.Interval.Duration).To(Equal(5 * time.Minute))
			Expect(fluxKs.Spec.Prune).To(BeTrue())
			Expect(fluxKs.Spec.SourceRef.Kind).To(Equal("GitRepository"))
			Expect(fluxKs.Spec.SourceRef.Name).To(Equal(srcName))
			Expect(fluxKs.Spec.SourceRef.Namespace).To(Equal(namespace))

			// Owner reference must be set so deletion cascades
			Expect(fluxKs.OwnerReferences).To(HaveLen(1))
			Expect(fluxKs.OwnerReferences[0].Name).To(Equal(name))
		})

		It("sets SourceInvalid=False and Ready=True", func() {
			ks := newKustomization()
			gr := newGitRepository()
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(ks).
				WithObjects(ks, gr).
				Build()
			r := controller.NewKustomizationReconciler(cl)

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &corev1alpha1.Kustomization{}
			Expect(cl.Get(context.Background(),
				types.NamespacedName{Name: name, Namespace: namespace},
				updated)).To(Succeed())

			srcCond := findCondition(updated.Status.Conditions, "SourceInvalid")
			Expect(srcCond).NotTo(BeNil())
			Expect(srcCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(srcCond.Reason).To(Equal("GitRepositoryFound"))

			readyCond := findCondition(updated.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("does not recreate the Flux Kustomization if it already exists (idempotent)", func() {
			ks := newKustomization()
			gr := newGitRepository()
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(ks).
				WithObjects(ks, gr).
				Build()
			r := controller.NewKustomizationReconciler(cl)

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
			_, err := r.Reconcile(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile must not error
			_, err = r.Reconcile(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())

			fluxList := &kustomizev1.KustomizationList{}
			Expect(cl.List(context.Background(), fluxList)).To(Succeed())
			Expect(fluxList.Items).To(HaveLen(1))
		})
	})

	Context("when sourceRef kind is not GitRepository", func() {
		It("sets SourceInvalid=True with reason InvalidSourceKind", func() {
			ks := &corev1alpha1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: corev1alpha1.KustomizationSpec{
					SourceRef: corev1alpha1.SourceRef{Kind: "HelmRepository", Name: srcName},
					Interval:  metav1.Duration{Duration: 5 * time.Minute},
				},
			}
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(ks).
				WithObjects(ks).
				Build()
			r := controller.NewKustomizationReconciler(cl)

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &corev1alpha1.Kustomization{}
			Expect(cl.Get(context.Background(),
				types.NamespacedName{Name: name, Namespace: namespace},
				updated)).To(Succeed())

			srcCond := findCondition(updated.Status.Conditions, "SourceInvalid")
			Expect(srcCond).NotTo(BeNil())
			Expect(srcCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(srcCond.Reason).To(Equal("InvalidSourceKind"))
		})
	})
})
