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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	clustersv1alpha1 "github.com/openmcp-project/openmcp-operator/api/clusters/v1alpha1"
	commonapi "github.com/openmcp-project/openmcp-operator/api/common"
	"github.com/openmcp-project/openmcp-operator/lib/clusteraccess/advanced"

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

// newTestClusters builds platform + onboarding clusters backed by the same fake client.
func newTestClusters(cl client.Client) (*clusters.Cluster, *clusters.Cluster) {
	platform := clusters.NewTestClusterFromClient("platform", cl)
	onboarding := clusters.NewTestClusterFromClient("onboarding", cl)
	return platform, onboarding
}

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

// reconcileGR runs a single reconcile for credential-only tests (no propagateTo).
// It uses two clusters backed by the same fake client.
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
	platform, onboarding := newTestClusters(cl)
	r := controller.NewGitRepositoryReconciler(platform, onboarding, "platform-service-gitops-system")

	// No propagateTo in these tests — inject a no-op clusterAccessRec.
	r.SetClusterAccessReconciler(advanced.NewClusterAccessReconciler(cl, "test"))

	// Reconcile twice: first pass adds the finalizer (returns Requeue:true), second does the work.
	for range 2 {
		_, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: grName, Namespace: grNamespace},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	out := &corev1alpha1.GitRepository{}
	Expect(cl.Get(context.Background(), types.NamespacedName{Name: grName, Namespace: grNamespace}, out)).To(Succeed())
	return out
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

// reconcileGRWithClusterAccess sets up a reconciler with a clusterAccessRec whose
// Access() method returns fake clients from the provided map.
// It auto-grants AccessRequests via FakeAccessRequestReadiness faking callbacks.
func reconcileGRWithClusterAccess(
	objs []client.Object,
	mcpClients map[string]client.Client,
	minter *fakeMinter,
) *corev1alpha1.GitRepository {
	// Platform client needs clustersv1alpha1 registered (for AccessRequest CRDs).
	platformScheme := runtime.NewScheme()
	_ = clustersv1alpha1.AddToScheme(platformScheme)
	_ = corev1.AddToScheme(platformScheme)
	platformCl := fake.NewClientBuilder().WithScheme(platformScheme).
		WithStatusSubresource(&clustersv1alpha1.AccessRequest{}).
		Build()

	onboardingScheme := scheme
	b := fake.NewClientBuilder().WithScheme(onboardingScheme)
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
	onboardingCl := b.Build()

	// Seed status separately since WithObjects doesn't set status subresource fields.
	for _, o := range objs {
		if gr, ok := o.(*corev1alpha1.GitRepository); ok && len(gr.Status.PropagateStatus) > 0 {
			Expect(onboardingCl.Status().Update(context.Background(), gr)).To(Succeed())
		}
	}

	platform := clusters.NewTestClusterFromClient("platform", platformCl)
	onboarding := clusters.NewTestClusterFromClient("onboarding", onboardingCl)

	r := controller.NewGitRepositoryReconciler(platform, onboarding, "platform-service-gitops-system")

	// Build the clusterAccessRec with fake client generator and auto-grant callbacks.
	mcpScheme := runtime.NewScheme()
	_ = corev1.AddToScheme(mcpScheme)

	caRec := advanced.NewClusterAccessReconciler(platformCl, "test").
		WithRetryInterval(10*time.Millisecond).
		// FakeClientGenerator: kubeconfig bytes are written as the MCP name by our custom callback.
		WithFakeClientGenerator(func(_ context.Context, kcfgData []byte, _ *runtime.Scheme, _ ...any) (client.Client, error) {
			mcpName := string(kcfgData)
			cl, ok := mcpClients[mcpName]
			if !ok {
				return nil, fmt.Errorf("no fake client for MCP %q", mcpName)
			}
			return cl, nil
		}).
		// Custom faking callback: grants the AR and writes the registration ID (MCP name)
		// as the kubeconfig bytes so FakeClientGenerator can look it up.
		WithFakingCallback(
			advanced.FakingCallback_WaitingForAccessRequestReadiness,
			grantARWithMCPNameKubeconfig(platformCl),
		)

	r.SetClusterAccessReconciler(caRec)
	r.SetCredentialResolver(func(_ context.Context, _ *githubv1alpha1.AppInstallation) (githubapp.Credentials, error) {
		return githubapp.Credentials{}, nil
	})
	if minter != nil {
		r.SetTokenMinterFactory(func(_ githubapp.Credentials) (controller.TokenMinter, error) {
			return minter, nil
		})
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: grName, Namespace: grNamespace}}

	// First reconcile adds the finalizer and requeues.
	_, err := r.Reconcile(context.Background(), req)
	Expect(err).NotTo(HaveOccurred())

	// Up to 4 more reconcile passes:
	// - Pass 2: creates AccessRequests; faking callback fires but RequeueAfter is still set
	//   (status update not yet observed in this pass).
	// - Pass 3: AccessRequests now granted; arResult.RequeueAfter==0; syncTokens runs.
	// - Pass 4: stable (token already synced, no re-mint needed unless rotation window).
	for range 3 {
		_, err = r.Reconcile(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
	}

	out := &corev1alpha1.GitRepository{}
	Expect(onboardingCl.Get(context.Background(), types.NamespacedName{Name: grName, Namespace: grNamespace}, out)).To(Succeed())
	return out
}

var _ = Describe("GitRepositoryReconciler", func() {
	Context("when the GitRepository does not exist", func() {
		It("returns no error", func() {
			cl := fake.NewClientBuilder().WithScheme(scheme).Build()
			platform, onboarding := newTestClusters(cl)
			r := controller.NewGitRepositoryReconciler(platform, onboarding, "platform-service-gitops-system")
			r.SetClusterAccessReconciler(advanced.NewClusterAccessReconciler(cl, "test"))
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

			out := reconcileGRWithClusterAccess(
				[]client.Object{gitRepoWithPropagate(testMCPName), installedAppInstallation()},
				map[string]client.Client{testMCPName: mcpFake},
				minter,
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

			gr := gitRepoWithPropagate(testMCPName)
			validExpiry := metav1.NewTime(time.Now().Add(30 * time.Minute))
			gr.Status.PropagateStatus = []corev1alpha1.MCPPropagateState{
				{Name: testMCPName, Phase: corev1alpha1.TokenSyncPhaseTokenSynced, TokenExpiresAt: &validExpiry},
			}

			reconcileGRWithClusterAccess(
				[]client.Object{gr, installedAppInstallation()},
				map[string]client.Client{testMCPName: mcpFake},
				minter,
			)

			Expect(minter.callCount).To(Equal(0))
		})

		It("rotates the token when expiry is within tokenRotationWindow", func() {
			minter := &fakeMinter{token: "ghs_rotated"}
			mcpFake := newMCPFakeClient()

			gr := gitRepoWithPropagate(testMCPName)
			expiringSoon := metav1.NewTime(time.Now().Add(10 * time.Minute))
			gr.Status.PropagateStatus = []corev1alpha1.MCPPropagateState{
				{Name: testMCPName, Phase: corev1alpha1.TokenSyncPhaseTokenSynced, TokenExpiresAt: &expiringSoon},
			}

			reconcileGRWithClusterAccess(
				[]client.Object{gr, installedAppInstallation()},
				map[string]client.Client{testMCPName: mcpFake},
				minter,
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

			// Pre-create the token Secret in the MCP cluster.
			existingSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("gitrepository-%s-%s", grNamespace, grName),
					Namespace: fluxSecretNamespace,
				},
			}
			Expect(mcpFake.Create(context.Background(), existingSecret)).To(Succeed())

			// GitRepository has no propagateTo but status still references old-mcp.
			gr := gitRepo()
			gr.Status.PropagateStatus = []corev1alpha1.MCPPropagateState{
				{Name: "old-mcp", Phase: corev1alpha1.TokenSyncPhaseTokenSynced},
			}

			reconcileGRWithClusterAccess(
				[]client.Object{gr, installedAppInstallation()},
				map[string]client.Client{"old-mcp": mcpFake},
				nil,
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

			wrongType := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("gitrepository-%s-%s", grNamespace, grName),
					Namespace: fluxSecretNamespace,
				},
				Type: corev1.SecretTypeOpaque,
			}
			Expect(mcpFake.Create(context.Background(), wrongType)).To(Succeed())

			reconcileGRWithClusterAccess(
				[]client.Object{gitRepoWithPropagate(testMCPName), installedAppInstallation()},
				map[string]client.Client{testMCPName: mcpFake},
				minter,
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
			minter := &fakeMinter{token: "irrelevant"}

			// Pass empty map — no fake client for "unreachable-mcp", so Access() will fail.
			out := reconcileGRWithClusterAccess(
				[]client.Object{gitRepoWithPropagate("unreachable-mcp"), installedAppInstallation()},
				map[string]client.Client{},
				minter,
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

// grantARWithMCPNameKubeconfig is a FakingCallback that grants the AccessRequest and
// writes the registration ID (MCP name) as the kubeconfig bytes in the Secret.
// The registration ID is the last "--"-separated segment of the AR name
// (e.g. "test--my-infra--my-mcp" → "my-mcp"), which matches the map key in mcpClients.
func grantARWithMCPNameKubeconfig(platformCl client.Client) advanced.FakingCallback {
	return func(ctx context.Context, _ client.Client, _ string, req *reconcile.Request,
		_ *clustersv1alpha1.ClusterRequest, ar *clustersv1alpha1.AccessRequest,
		_ *clustersv1alpha1.Cluster, _ *clusters.Cluster) error {
		if ar == nil {
			return nil
		}
		if ar.Status.IsGranted() && ar.Status.ObservedGeneration == ar.Generation {
			return nil
		}

		// Extract MCP name = last "--"-separated segment of the AR name.
		mcpName := ar.Name
		for i := len(ar.Name) - 1; i >= 0; i-- {
			if i+2 <= len(ar.Name) && ar.Name[i] == '-' && ar.Name[i+1] == '-' {
				mcpName = ar.Name[i+2:]
				break
			}
		}

		// Create the kubeconfig Secret with the MCP name as content.
		sec := &corev1.Secret{}
		sec.Name = ar.Name
		sec.Namespace = ar.Namespace
		if _, err := controllerutil.CreateOrUpdate(ctx, platformCl, sec, func() error {
			sec.Data = map[string][]byte{
				clustersv1alpha1.SecretKeyKubeconfig: []byte(mcpName),
			}
			return nil
		}); err != nil {
			return fmt.Errorf("creating kubeconfig Secret: %w", err)
		}

		// Grant the AR.
		old := ar.DeepCopy()
		ar.Status.SecretRef = &commonapi.LocalObjectReference{Name: sec.Name}
		ar.Status.Phase = clustersv1alpha1.REQUEST_GRANTED
		ar.Status.ObservedGeneration = ar.Generation
		return platformCl.Status().Patch(ctx, ar, client.MergeFrom(old))
	}
}
