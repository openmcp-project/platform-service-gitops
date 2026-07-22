// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
	controller "github.com/openmcp-project/platform-service-gitops/internal/controller/github"
	"github.com/openmcp-project/platform-service-gitops/internal/githubapp"
)

// fakeGitHub is an injectable GitHub client for tests.
type fakeGitHub struct {
	verifyErr error
	result    githubapp.InstallationResult
	checkErr  error
}

func (f *fakeGitHub) VerifyApp(_ context.Context) (string, error) { return "test-app", f.verifyErr }
func (f *fakeGitHub) CheckOrgInstallation(_ context.Context, _ string) (githubapp.InstallationResult, error) {
	return f.result, f.checkErr
}
func (f *fakeGitHub) CheckUserInstallation(_ context.Context, _ string) (githubapp.InstallationResult, error) {
	return f.result, f.checkErr
}

const (
	credNamespace = "platform-service-gitops-system"
	instanceName  = "sap-ghe"
	aiName        = "my-connection"
	aiNamespace   = "my-project"
	testOrg       = "cloud-orchestration"
)

func newSecret(data map[string]string) *corev1.Secret {
	d := map[string][]byte{}
	for k, v := range data {
		d[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: instanceName, Namespace: credNamespace},
		Data:       d,
	}
}

func validSecretData() map[string]string {
	return map[string]string{
		"appID":      "1436",
		"privateKey": "-----BEGIN RSA PRIVATE KEY-----\nMIIB\n-----END RSA PRIVATE KEY-----",
		"url":        "https://github.tools.sap",
	}
}

func condStatus(conds []metav1.Condition, t string) metav1.ConditionStatus {
	c := meta.FindStatusCondition(conds, t)
	if c == nil {
		return "Missing"
	}
	return c.Status
}

func reconcileAI(objs []any, ghc *fakeGitHub) *githubv1alpha1.AppInstallation {
	builder := fake_ClientBuilder(objs)
	cl := builder.Build()
	r := controller.NewAppInstallationReconciler(cl, cl, credNamespace)
	if ghc != nil {
		r.SetGitHubClientFactory(func(githubapp.Credentials) (controller.GitHubClient, error) {
			return ghc, nil
		})
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: aiName, Namespace: aiNamespace},
	})
	Expect(err).NotTo(HaveOccurred())

	out := &githubv1alpha1.AppInstallation{}
	Expect(cl.Get(context.Background(), types.NamespacedName{Name: aiName, Namespace: aiNamespace}, out)).To(Succeed())
	return out
}

var _ = Describe("AppInstallationReconciler", func() {
	baseAI := func(spec githubv1alpha1.AppInstallationSpec) *githubv1alpha1.AppInstallation {
		return &githubv1alpha1.AppInstallation{
			ObjectMeta: metav1.ObjectMeta{Name: aiName, Namespace: aiNamespace},
			Spec:       spec,
		}
	}
	instance := func() *githubv1alpha1.GitHubInstance {
		return &githubv1alpha1.GitHubInstance{
			ObjectMeta: metav1.ObjectMeta{Name: instanceName},
			Spec: githubv1alpha1.GitHubInstanceSpec{
				SecretRefs: []githubv1alpha1.SecretReference{{Name: instanceName}},
			},
		}
	}

	Context("when the AppInstallation does not exist", func() {
		It("returns no error", func() {
			cl := fake_ClientBuilder(nil).Build()
			r := controller.NewAppInstallationReconciler(cl, cl, credNamespace)
			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "nope", Namespace: aiNamespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("when neither org nor user is set", func() {
		It("sets both conditions False with InvalidSpec", func() {
			ai := baseAI(githubv1alpha1.AppInstallationSpec{
				InstanceRef: githubv1alpha1.InstanceReference{Name: instanceName},
			})
			out := reconcileAI([]any{ai, instance(), newSecret(validSecretData())}, nil)
			Expect(condStatus(out.Status.Conditions, "AppInstalled")).To(Equal(metav1.ConditionFalse))
			Expect(meta.FindStatusCondition(out.Status.Conditions, "AppInstalled").Reason).To(Equal("InvalidSpec"))
		})
	})

	Context("when the referenced GitHubInstance is missing", func() {
		It("sets conditions False with InstanceResolutionFailed", func() {
			ai := baseAI(githubv1alpha1.AppInstallationSpec{
				InstanceRef: githubv1alpha1.InstanceReference{Name: instanceName},
				Org:         testOrg,
			})
			out := reconcileAI([]any{ai}, nil)
			Expect(meta.FindStatusCondition(out.Status.Conditions, "AppInstalled").Reason).To(Equal("InstanceResolutionFailed"))
		})
	})

	Context("when the App is installed on the org", func() {
		It("sets AppInstalled=True, AccessVerified=True and records the installation ID", func() {
			ai := baseAI(githubv1alpha1.AppInstallationSpec{
				InstanceRef: githubv1alpha1.InstanceReference{Name: instanceName},
				Org:         testOrg,
			})
			fk := &fakeGitHub{result: githubapp.InstallationResult{Installed: true, InstallationID: 16282, Account: testOrg}}
			out := reconcileAI([]any{ai, instance(), newSecret(validSecretData())}, fk)
			Expect(condStatus(out.Status.Conditions, "AppInstalled")).To(Equal(metav1.ConditionTrue))
			Expect(condStatus(out.Status.Conditions, "AccessVerified")).To(Equal(metav1.ConditionTrue))
			Expect(out.Status.InstallationID).To(Equal(int64(16282)))
		})
	})

	Context("when the App is not installed on the org", func() {
		It("sets AppInstalled=False but AccessVerified=True, no error loop", func() {
			ai := baseAI(githubv1alpha1.AppInstallationSpec{
				InstanceRef: githubv1alpha1.InstanceReference{Name: instanceName},
				Org:         "some-org",
			})
			fk := &fakeGitHub{result: githubapp.InstallationResult{Installed: false}}
			out := reconcileAI([]any{ai, instance(), newSecret(validSecretData())}, fk)
			Expect(condStatus(out.Status.Conditions, "AppInstalled")).To(Equal(metav1.ConditionFalse))
			Expect(meta.FindStatusCondition(out.Status.Conditions, "AppInstalled").Reason).To(Equal("AppNotInstalled"))
			Expect(condStatus(out.Status.Conditions, "AccessVerified")).To(Equal(metav1.ConditionTrue))
			Expect(out.Status.InstallationID).To(Equal(int64(0)))
		})
	})

	Context("when App authentication fails", func() {
		It("sets both conditions False with AccessDenied", func() {
			ai := baseAI(githubv1alpha1.AppInstallationSpec{
				InstanceRef: githubv1alpha1.InstanceReference{Name: instanceName},
				Org:         testOrg,
			})
			fk := &fakeGitHub{verifyErr: errors.New("401 bad jwt")}
			out := reconcileAI([]any{ai, instance(), newSecret(validSecretData())}, fk)
			Expect(condStatus(out.Status.Conditions, "AccessVerified")).To(Equal(metav1.ConditionFalse))
			Expect(meta.FindStatusCondition(out.Status.Conditions, "AccessVerified").Reason).To(Equal("AccessDenied"))
		})
	})
})

// fake_ClientBuilder builds a fake client with the scheme and status subresource
// for our CRDs, seeded with the given objects.
func fake_ClientBuilder(objs []any) *fake.ClientBuilder {
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		switch v := o.(type) {
		case *githubv1alpha1.AppInstallation:
			b = b.WithObjects(v).WithStatusSubresource(v)
		case *githubv1alpha1.GitHubInstance:
			b = b.WithObjects(v).WithStatusSubresource(v)
		case *corev1.Secret:
			b = b.WithObjects(v)
		}
	}
	return b
}
