// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
	controller "github.com/openmcp-project/platform-service-gitops/internal/controller/core"
	"github.com/openmcp-project/platform-service-gitops/internal/mcpaccess"
)

const (
	grName       = "my-infra"
	grNamespace  = "my-project"
	instanceName = "sap-ghe"
)

// noopResolver is a test double that never resolves any MCP targets.
type noopResolver struct{}

func (noopResolver) Resolve(_ context.Context, _ *corev1alpha1.GitRepository, _ client.Client) ([]mcpaccess.ResolvedTarget, error) {
	return nil, nil
}

func (noopResolver) Cleanup(_ context.Context, _ *corev1alpha1.GitRepository, _ string, _ string) error {
	return nil
}

func newTestReconciler(cl client.Client) *controller.GitRepositoryReconciler {
	return controller.NewGitRepositoryReconciler(
		cl, // onboardingClient
		cl, // platformClient (same fake in unit tests)
		noopResolver{},
		"platform-service-gitops-system",
		"flux-system",
		15*time.Minute,
	)
}

func gitRepo(credName string) *corev1alpha1.GitRepository {
	return &corev1alpha1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: grName, Namespace: grNamespace},
		Spec: corev1alpha1.GitRepositorySpec{
			URL: "https://github.tools.sap/my-org/my-infra",
			Ref: corev1alpha1.GitRef{Branch: "main"},
			CredentialRef: corev1alpha1.CredentialRef{
				Name:  credName,
				Kind:  "AppInstallation",
				Group: "github.gitops.open-control-plane.io",
			},
		},
	}
}

func installedAppInstallation() *githubv1alpha1.AppInstallation {
	return &githubv1alpha1.AppInstallation{
		ObjectMeta: metav1.ObjectMeta{Name: "my-connection", Namespace: grNamespace},
		Spec: githubv1alpha1.AppInstallationSpec{
			InstanceRef: githubv1alpha1.InstanceReference{Name: instanceName},
			Org:         "cloud-orchestration",
		},
		Status: githubv1alpha1.AppInstallationStatus{
			InstallationID: 16282,
			Conditions: []metav1.Condition{{
				Type:   "AppInstalled",
				Status: metav1.ConditionTrue,
				Reason: "AppFound",
			}},
		},
	}
}

func reconcileGR(objs []client.Object) *corev1alpha1.GitRepository {
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1alpha1.GitRepository:
			b = b.WithObjects(v).WithStatusSubresource(v)
		case *githubv1alpha1.AppInstallation:
			b = b.WithObjects(v).WithStatusSubresource(v)
		}
	}
	cl := b.Build()
	r := newTestReconciler(cl)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: grName, Namespace: grNamespace}}

	// First reconcile adds the finalizer and requeues; second reconcile runs the full logic.
	_, err := r.Reconcile(context.Background(), req)
	Expect(err).NotTo(HaveOccurred())
	_, err = r.Reconcile(context.Background(), req)
	Expect(err).NotTo(HaveOccurred())

	out := &corev1alpha1.GitRepository{}
	Expect(cl.Get(context.Background(), types.NamespacedName{Name: grName, Namespace: grNamespace}, out)).To(Succeed())
	return out
}

var _ = Describe("GitRepositoryReconciler", func() {
	Context("when the GitRepository does not exist", func() {
		It("returns no error", func() {
			cl := fake.NewClientBuilder().WithScheme(scheme).Build()
			r := newTestReconciler(cl)
			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "nope", Namespace: grNamespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("when the referenced AppInstallation is missing", func() {
		It("sets CredentialResolved=False / CredentialNotFound and Ready=False", func() {
			out := reconcileGR([]client.Object{gitRepo("my-connection")})
			cred := findCondition(out.Status.Conditions, "CredentialResolved")
			Expect(cred.Status).To(Equal(metav1.ConditionFalse))
			Expect(cred.Reason).To(Equal("CredentialNotFound"))
			ready := findCondition(out.Status.Conditions, "Ready")
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		})
	})

	Context("when the AppInstallation exists but the App is not installed", func() {
		It("sets CredentialResolved=False / AppNotInstalled", func() {
			ai := installedAppInstallation()
			ai.Status.InstallationID = 0
			ai.Status.Conditions[0].Status = metav1.ConditionFalse
			out := reconcileGR([]client.Object{gitRepo("my-connection"), ai})
			cred := findCondition(out.Status.Conditions, "CredentialResolved")
			Expect(cred.Status).To(Equal(metav1.ConditionFalse))
			Expect(cred.Reason).To(Equal("AppNotInstalled"))
		})
	})

	Context("when the AppInstallation reports the App as installed", func() {
		It("sets CredentialResolved=True and Ready=True", func() {
			out := reconcileGR([]client.Object{gitRepo("my-connection"), installedAppInstallation()})
			cred := findCondition(out.Status.Conditions, "CredentialResolved")
			Expect(cred.Status).To(Equal(metav1.ConditionTrue))
			Expect(cred.Reason).To(Equal("CredentialResolved"))
			ready := findCondition(out.Status.Conditions, "Ready")
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
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
