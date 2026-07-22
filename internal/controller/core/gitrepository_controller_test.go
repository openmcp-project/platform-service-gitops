// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
	controller "github.com/openmcp-project/platform-service-gitops/internal/controller/core"
	githubapp "github.com/openmcp-project/platform-service-gitops/internal/githubapp"
)

const (
	grName              = "my-infra"
	grNamespace         = "my-project"
	instanceName        = "sap-ghe"
	testMCPName         = "my-mcp"
	testCredName        = "my-connection"
	fluxSecretNamespace = "flux-system"
)

func gitRepo() *corev1alpha1.GitRepository {
	return &corev1alpha1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: grName, Namespace: grNamespace},
		Spec: corev1alpha1.GitRepositorySpec{
			URL: "https://github.tools.sap/my-org/my-infra",
			Ref: corev1alpha1.GitRef{Branch: "main"},
			CredentialRef: corev1alpha1.CredentialRef{
				Name:  testCredName,
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
	r := controller.NewGitRepositoryReconciler(cl, "platform-service-gitops-system")
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: grName, Namespace: grNamespace},
	})
	Expect(err).NotTo(HaveOccurred())

	out := &corev1alpha1.GitRepository{}
	Expect(cl.Get(context.Background(), types.NamespacedName{Name: grName, Namespace: grNamespace}, out)).To(Succeed())
	return out
}

// fakeMCPResolver returns a pre-built fake client for named MCPs.
type fakeMCPResolver struct {
	clients map[string]client.Client
}

func (f *fakeMCPResolver) Resolve(_ context.Context, mcpName string) (client.Client, error) {
	cl, ok := f.clients[mcpName]
	if !ok {
		return nil, fmt.Errorf("kubeconfig Secret for MCP %q not found", mcpName)
	}
	return cl, nil
}

// fakeMinter records call count and returns a configured token.
type fakeMinter struct {
	token     string
	callCount int
	err       error
}

func (f *fakeMinter) MintInstallationToken(_ context.Context, _ int64) (string, error) {
	f.callCount++
	return f.token, f.err
}

func newMCPFakeClient() client.Client {
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	return fake.NewClientBuilder().WithScheme(sc).Build()
}

func gitRepoWithPropagate(mcpNames ...string) *corev1alpha1.GitRepository {
	gr := gitRepo()
	for _, n := range mcpNames {
		gr.Spec.PropagateToControlPlanes = append(gr.Spec.PropagateToControlPlanes,
			corev1alpha1.PropagateTarget{Kind: "ControlPlane", Name: n})
	}
	return gr
}

func reconcileGRWithMCPResolver(
	objs []client.Object,
	resolver controller.MCPClientResolver,
	minter *fakeMinter,
) *corev1alpha1.GitRepository {
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1alpha1.GitRepository:
			b = b.WithObjects(v).WithStatusSubresource(v)
		case *githubv1alpha1.AppInstallation:
			b = b.WithObjects(v).WithStatusSubresource(v)
		default:
			b = b.WithObjects(v)
		}
	}
	cl := b.Build()

	// Seed status separately since WithObjects doesn't set status subresource fields
	for _, o := range objs {
		if gr, ok := o.(*corev1alpha1.GitRepository); ok && len(gr.Status.PropagateStatus) > 0 {
			if err := cl.Status().Update(context.Background(), gr); err != nil {
				Expect(err).NotTo(HaveOccurred())
			}
		}
	}

	r := controller.NewGitRepositoryReconciler(cl, "platform-service-gitops-system")
	r.SetMCPClientResolver(resolver)
	// Bypass GitHubInstance + Secret lookup — credentials are not exercised in these tests.
	r.SetCredentialResolver(func(_ context.Context, _ *githubv1alpha1.AppInstallation) (githubapp.Credentials, error) {
		return githubapp.Credentials{}, nil
	})
	if minter != nil {
		r.SetTokenMinterFactory(func(_ githubapp.Credentials) (controller.TokenMinter, error) {
			return minter, nil
		})
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: grName, Namespace: grNamespace},
	})
	Expect(err).NotTo(HaveOccurred())

	out := &corev1alpha1.GitRepository{}
	Expect(cl.Get(context.Background(), types.NamespacedName{Name: grName, Namespace: grNamespace}, out)).To(Succeed())
	return out
}

var _ = Describe("GitRepositoryReconciler", func() {
	Context("when the GitRepository does not exist", func() {
		It("returns no error", func() {
			cl := fake.NewClientBuilder().WithScheme(scheme).Build()
			r := controller.NewGitRepositoryReconciler(cl, "platform-service-gitops-system")
			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "nope", Namespace: grNamespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("when the referenced AppInstallation is missing", func() {
		It("sets CredentialResolved=False / CredentialNotFound and Ready=False", func() {
			out := reconcileGR([]client.Object{gitRepo()})
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
			out := reconcileGR([]client.Object{gitRepo(), ai})
			cred := findCondition(out.Status.Conditions, "CredentialResolved")
			Expect(cred.Status).To(Equal(metav1.ConditionFalse))
			Expect(cred.Reason).To(Equal("AppNotInstalled"))
		})
	})

	Context("when the AppInstallation reports the App as installed", func() {
		It("sets CredentialResolved=True and Ready=True", func() {
			out := reconcileGR([]client.Object{gitRepo(), installedAppInstallation()})
			cred := findCondition(out.Status.Conditions, "CredentialResolved")
			Expect(cred.Status).To(Equal(metav1.ConditionTrue))
			Expect(cred.Reason).To(Equal("CredentialResolved"))
			ready := findCondition(out.Status.Conditions, "Ready")
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("token sync via propagateTo", func() {
		It("writes a BasicAuth Secret into the MCP cluster", func() {
			minter := &fakeMinter{token: "ghs_test_token"}
			mcpFake := newMCPFakeClient()
			resolver := &fakeMCPResolver{clients: map[string]client.Client{testMCPName: mcpFake}}

			out := reconcileGRWithMCPResolver(
				[]client.Object{gitRepoWithPropagate(testMCPName), installedAppInstallation()},
				resolver, minter,
			)

			secret := &corev1.Secret{}
			Expect(mcpFake.Get(context.Background(),
				types.NamespacedName{
					Name:      fmt.Sprintf("gitrepository-%s-%s", grNamespace, grName),
					Namespace: fluxSecretNamespace,
				},
				secret)).To(Succeed())
			Expect(secret.Data["password"]).To(Equal([]byte("ghs_test_token")))
			Expect(secret.Data["username"]).To(Equal([]byte("x-access-token")))
			Expect(secret.Type).To(Equal(corev1.SecretTypeBasicAuth))

			Expect(out.Status.PropagateStatus).To(HaveLen(1))
			Expect(out.Status.PropagateStatus[0].Phase).To(Equal(corev1alpha1.TokenSyncPhaseTokenSynced))
			Expect(out.Status.PropagateStatus[0].TokenExpiresAt).NotTo(BeNil())
		})

		It("does not re-mint when the existing token is still valid", func() {
			minter := &fakeMinter{token: "ghs_fresh"}
			mcpFake := newMCPFakeClient()
			resolver := &fakeMCPResolver{clients: map[string]client.Client{testMCPName: mcpFake}}

			gr := gitRepoWithPropagate(testMCPName)
			validExpiry := metav1.NewTime(time.Now().Add(30 * time.Minute))
			gr.Status.PropagateStatus = []corev1alpha1.MCPPropagateState{
				{Name: testMCPName, Phase: corev1alpha1.TokenSyncPhaseTokenSynced, TokenExpiresAt: &validExpiry},
			}

			reconcileGRWithMCPResolver(
				[]client.Object{gr, installedAppInstallation()},
				resolver, minter,
			)

			Expect(minter.callCount).To(Equal(0))
		})

		It("rotates the token when expiry is within tokenRotationWindow", func() {
			minter := &fakeMinter{token: "ghs_rotated"}
			mcpFake := newMCPFakeClient()
			resolver := &fakeMCPResolver{clients: map[string]client.Client{testMCPName: mcpFake}}

			gr := gitRepoWithPropagate(testMCPName)
			expiringSoon := metav1.NewTime(time.Now().Add(10 * time.Minute))
			gr.Status.PropagateStatus = []corev1alpha1.MCPPropagateState{
				{Name: testMCPName, Phase: corev1alpha1.TokenSyncPhaseTokenSynced, TokenExpiresAt: &expiringSoon},
			}

			reconcileGRWithMCPResolver(
				[]client.Object{gr, installedAppInstallation()},
				resolver, minter,
			)

			Expect(minter.callCount).To(Equal(1))

			secret := &corev1.Secret{}
			Expect(mcpFake.Get(context.Background(),
				types.NamespacedName{
					Name:      fmt.Sprintf("gitrepository-%s-%s", grNamespace, grName),
					Namespace: fluxSecretNamespace,
				},
				secret)).To(Succeed())
			Expect(secret.Data["password"]).To(Equal([]byte("ghs_rotated")))
		})

		It("deletes the token Secret when the MCP is removed from propagateTo", func() {
			mcpFake := newMCPFakeClient()
			resolver := &fakeMCPResolver{clients: map[string]client.Client{"old-mcp": mcpFake}}

			existingSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("gitrepository-%s-%s", grNamespace, grName),
					Namespace: fluxSecretNamespace,
				},
			}
			Expect(mcpFake.Create(context.Background(), existingSecret)).To(Succeed())

			gr := gitRepo()
			gr.Status.PropagateStatus = []corev1alpha1.MCPPropagateState{
				{Name: "old-mcp", Phase: corev1alpha1.TokenSyncPhaseTokenSynced},
			}

			reconcileGRWithMCPResolver(
				[]client.Object{gr, installedAppInstallation()},
				resolver, nil,
			)

			deleted := &corev1.Secret{}
			err := mcpFake.Get(context.Background(),
				types.NamespacedName{
					Name:      fmt.Sprintf("gitrepository-%s-%s", grNamespace, grName),
					Namespace: fluxSecretNamespace,
				},
				deleted)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("recreates the Secret when an existing one has the wrong type", func() {
			minter := &fakeMinter{token: "ghs_retyped"}
			mcpFake := newMCPFakeClient()
			resolver := &fakeMCPResolver{clients: map[string]client.Client{testMCPName: mcpFake}}

			wrongType := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("gitrepository-%s-%s", grNamespace, grName),
					Namespace: fluxSecretNamespace,
				},
				Type: corev1.SecretTypeOpaque,
			}
			Expect(mcpFake.Create(context.Background(), wrongType)).To(Succeed())

			reconcileGRWithMCPResolver(
				[]client.Object{gitRepoWithPropagate(testMCPName), installedAppInstallation()},
				resolver, minter,
			)

			secret := &corev1.Secret{}
			Expect(mcpFake.Get(context.Background(),
				types.NamespacedName{
					Name:      fmt.Sprintf("gitrepository-%s-%s", grNamespace, grName),
					Namespace: fluxSecretNamespace,
				},
				secret)).To(Succeed())
			Expect(secret.Type).To(Equal(corev1.SecretTypeBasicAuth))
			Expect(secret.Data["password"]).To(Equal([]byte("ghs_retyped")))
		})

		It("sets Error phase when MCP cannot be resolved", func() {
			resolver := &fakeMCPResolver{clients: map[string]client.Client{}}
			minter := &fakeMinter{token: "irrelevant"}

			out := reconcileGRWithMCPResolver(
				[]client.Object{gitRepoWithPropagate("unreachable-mcp"), installedAppInstallation()},
				resolver, minter,
			)

			Expect(out.Status.PropagateStatus).To(HaveLen(1))
			Expect(out.Status.PropagateStatus[0].Name).To(Equal("unreachable-mcp"))
			Expect(out.Status.PropagateStatus[0].Phase).To(Equal(corev1alpha1.TokenSyncPhaseError))
			Expect(out.Status.PropagateStatus[0].Message).To(ContainSubstring("resolving MCP client failed"))
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
