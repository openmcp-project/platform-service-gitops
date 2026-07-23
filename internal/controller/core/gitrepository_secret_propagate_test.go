// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"context"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openmcp-project/controller-utils/pkg/clusters"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	controller "github.com/openmcp-project/platform-service-gitops/internal/controller/core"
	"github.com/openmcp-project/platform-service-gitops/internal/credentials"
	"github.com/openmcp-project/platform-service-gitops/internal/mcpaccess"
	"github.com/openmcp-project/platform-service-gitops/internal/propagate"
)

// fakeMCPResolver resolves a single MCP target backed by an in-memory fake client,
// so the Secret-path propagation can be exercised without real cluster access.
type fakeMCPResolver struct {
	mcpClient client.Client
	cpName    string
}

const (
	fluxNS      = "flux-system"
	credName    = "git-creds"
	mcpCPName   = "test-mcp"
	finalizerNm = "gitops.open-control-plane.io/propagate"

	keyUsername = "username"
	keyPassword = "password"
)

func (f *fakeMCPResolver) Resolve(_ context.Context, _ *corev1alpha1.GitRepository, _ client.Client) ([]mcpaccess.ResolvedTarget, error) {
	return []mcpaccess.ResolvedTarget{{
		ControlPlaneName: f.cpName,
		Cluster:          clusters.NewTestClusterFromClient(f.cpName, f.mcpClient),
	}}, nil
}

func (*fakeMCPResolver) Cleanup(_ context.Context, _ *corev1alpha1.GitRepository, _ string) error {
	return nil
}

// secretGRWithPropagate builds a kind:Secret GitRepository targeting one MCP.
func secretGRWithPropagate() *corev1alpha1.GitRepository {
	gr := gitRepoWithCredential("Secret", credName)
	gr.Spec.PropagateToControlPlanes = []corev1alpha1.PropagateTarget{{
		Kind: "ControlPlane",
		Name: mcpCPName,
	}}
	return gr
}

func httpsCredSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credName, Namespace: grNamespace},
		Data: map[string][]byte{
			keyUsername: []byte("alice"),
			keyPassword: []byte("ghp_token"),
		},
	}
}

// reconcileSecretPropagate builds an onboarding fake client with the given objects,
// wires a fakeMCPResolver over mcpClient, stubs validateAccess to succeed, and runs
// the two-pass reconcile. Returns the updated GitRepository.
func reconcileSecretPropagate(objs []client.Object, mcpClient client.Client) *corev1alpha1.GitRepository {
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1alpha1.GitRepository:
			b = b.WithObjects(v).WithStatusSubresource(v)
		case *corev1.Secret:
			b = b.WithObjects(v)
		}
	}
	cl := b.Build()

	res := &fakeMCPResolver{mcpClient: mcpClient, cpName: mcpCPName}
	r := controller.NewGitRepositoryReconciler(cl, cl, res, "platform-service-gitops-system", fluxNS, 15*time.Minute).
		WithValidateAccess(func(_ context.Context, _ string, _ *credentials.Credential) error { return nil })

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: grName, Namespace: grNamespace}}
	_, err := r.Reconcile(context.Background(), req)
	Expect(err).NotTo(HaveOccurred())
	_, err = r.Reconcile(context.Background(), req)
	Expect(err).NotTo(HaveOccurred())

	out := &corev1alpha1.GitRepository{}
	Expect(cl.Get(context.Background(), req.NamespacedName, out)).To(Succeed())
	return out
}

var _ = Describe("GitRepositoryReconciler Secret propagation", func() {
	Context("when a valid Secret credential has propagateTo targets", func() {
		It("copies the Secret verbatim into the MCP and creates a Flux GitRepository", func() {
			mcp := fake.NewClientBuilder().WithScheme(scheme).Build()
			out := reconcileSecretPropagate([]client.Object{
				secretGRWithPropagate(),
				httpsCredSecret(),
			}, mcp)

			By("setting Ready=True")
			ready := findCondition(out.Status.Conditions, "Ready")
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))

			By("recording per-MCP status with reason SecretSynced and no token expiry")
			Expect(out.Status.Propagated).To(HaveLen(1))
			ps := out.Status.Propagated[0]
			Expect(ps.ControlPlaneName).To(Equal(mcpCPName))
			Expect(ps.Phase).To(Equal(corev1alpha1.PropagatePhaseReady))
			Expect(ps.Reason).To(Equal("SecretSynced"))
			Expect(ps.TokenExpiresAt).To(BeNil())

			By("copying the credential Secret verbatim into the MCP flux-system")
			gr := secretGRWithPropagate()
			copied := &corev1.Secret{}
			Expect(mcp.Get(context.Background(),
				types.NamespacedName{Name: propagate.CredentialsSecretName(gr), Namespace: fluxNS}, copied)).To(Succeed())
			Expect(string(copied.Data[keyUsername])).To(Equal("alice"))
			Expect(string(copied.Data[keyPassword])).To(Equal("ghp_token"))

			By("creating a Flux GitRepository referencing the copied Secret")
			flux := &sourcev1.GitRepository{}
			Expect(mcp.Get(context.Background(),
				types.NamespacedName{Name: grName, Namespace: fluxNS}, flux)).To(Succeed())
			Expect(flux.Spec.SecretRef).NotTo(BeNil())
			Expect(flux.Spec.SecretRef.Name).To(Equal(propagate.CredentialsSecretName(gr)))
		})
	})

	Context("when the onboarding Secret is rotated", func() {
		It("updates the copy in the MCP on the next reconcile", func() {
			mcp := fake.NewClientBuilder().WithScheme(scheme).Build()
			gr := secretGRWithPropagate()

			// First reconcile with the original password.
			reconcileSecretPropagate([]client.Object{gr.DeepCopy(), httpsCredSecret()}, mcp)

			// Second reconcile with a rotated password (fresh onboarding client, same MCP).
			rotated := httpsCredSecret()
			rotated.Data[keyPassword] = []byte("ghp_rotated")
			reconcileSecretPropagate([]client.Object{gr.DeepCopy(), rotated}, mcp)

			copied := &corev1.Secret{}
			Expect(mcp.Get(context.Background(),
				types.NamespacedName{Name: propagate.CredentialsSecretName(gr), Namespace: fluxNS}, copied)).To(Succeed())
			Expect(string(copied.Data[keyPassword])).To(Equal("ghp_rotated"))
		})
	})

	Context("when a Secret-propagated GitRepository is deleted", func() {
		It("cleans up the copied Secret and Flux GitRepository from the MCP", func() {
			mcp := fake.NewClientBuilder().WithScheme(scheme).Build()
			gr := secretGRWithPropagate()
			reconcileSecretPropagate([]client.Object{gr.DeepCopy(), httpsCredSecret()}, mcp)

			// Confirm resources exist, then run a delete reconcile.
			out := reconcileSecretDelete(gr, mcp)

			Expect(out.Finalizers).NotTo(ContainElement(finalizerNm))

			s := &corev1.Secret{}
			err := mcp.Get(context.Background(),
				types.NamespacedName{Name: propagate.CredentialsSecretName(gr), Namespace: fluxNS}, s)
			Expect(err).To(HaveOccurred(), "credential Secret should be gone from the MCP")

			flux := &sourcev1.GitRepository{}
			err = mcp.Get(context.Background(),
				types.NamespacedName{Name: grName, Namespace: fluxNS}, flux)
			Expect(err).To(HaveOccurred(), "Flux GitRepository should be gone from the MCP")
		})
	})
})

// reconcileSecretDelete seeds an onboarding client with a GitRepository carrying
// the propagate finalizer + a deletion timestamp, plus the credential Secret, and
// runs one reconcile so the deletion path (which does not call validateAccess)
// cleans up the MCP.
func reconcileSecretDelete(gr *corev1alpha1.GitRepository, mcp client.Client) *corev1alpha1.GitRepository {
	grDel := gr.DeepCopy()
	grDel.Finalizers = []string{finalizerNm}
	now := metav1.NewTime(time.Now())
	grDel.DeletionTimestamp = &now

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(grDel, httpsCredSecret()).
		WithStatusSubresource(grDel).Build()

	res := &fakeMCPResolver{mcpClient: mcp, cpName: mcpCPName}
	r := controller.NewGitRepositoryReconciler(cl, cl, res, "platform-service-gitops-system", fluxNS, 15*time.Minute).
		WithValidateAccess(func(_ context.Context, _ string, _ *credentials.Credential) error { return nil })

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: grName, Namespace: grNamespace}}
	_, err := r.Reconcile(context.Background(), req)
	Expect(err).NotTo(HaveOccurred())

	out := &corev1alpha1.GitRepository{}
	// After finalizer removal the object may be gone; tolerate NotFound.
	_ = cl.Get(context.Background(), req.NamespacedName, out)
	return out
}
