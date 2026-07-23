// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package propagate_test

import (
	"context"
	"testing"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	"github.com/openmcp-project/platform-service-gitops/internal/propagate"
)

const (
	grName    = "my-infra"
	fluxNS    = "flux-system"
	managedBy = "openmcp.cloud/managed-by"

	keyUsername = "username"
	keyPassword = "password"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	if err := sourcev1.AddToScheme(s); err != nil {
		t.Fatalf("adding sourcev1 to scheme: %v", err)
	}
	return s
}

func gitRepo() *corev1alpha1.GitRepository {
	return &corev1alpha1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: grName, Namespace: "my-project"},
		Spec: corev1alpha1.GitRepositorySpec{
			URL: "https://github.tools.sap/my-org/my-infra",
			Ref: corev1alpha1.GitRef{Branch: "main"},
		},
	}
}

func httpsSecret() *corev1.Secret {
	return &corev1.Secret{
		Data: map[string][]byte{
			keyUsername: []byte("alice"),
			keyPassword: []byte("ghp_token"),
		},
	}
}

func sshSecret() *corev1.Secret {
	return &corev1.Secret{
		Data: map[string][]byte{
			"identity":    []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nkey\n-----END OPENSSH PRIVATE KEY-----"),
			"known_hosts": []byte("github.tools.sap ssh-ed25519 AAAA..."),
		},
	}
}

func getCredSecret(t *testing.T, cl client.Client) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	name := propagate.CredentialsSecretName(gitRepo())
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: fluxNS}, s); err != nil {
		t.Fatalf("getting credential Secret %s: %v", name, err)
	}
	return s
}

func TestReconcileSecret_CopiesHTTPSKeysVerbatim(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	gr := gitRepo()
	user := httpsSecret()

	res, err := propagate.ReconcileSecret(context.Background(), cl, gr, fluxNS, user)
	if err != nil {
		t.Fatalf("ReconcileSecret: %v", err)
	}
	if res.Conflict {
		t.Errorf("unexpected conflict")
	}
	if !res.TokenExpiresAt.IsZero() {
		t.Errorf("TokenExpiresAt = %v, want zero (Secret path has no token)", res.TokenExpiresAt)
	}

	got := getCredSecret(t, cl)
	if string(got.Data[keyUsername]) != "alice" || string(got.Data[keyPassword]) != "ghp_token" {
		t.Errorf("data not copied verbatim: %v", got.Data)
	}
	if len(got.Data) != 2 {
		t.Errorf("unexpected keys copied: %v", got.Data)
	}
	if got.Annotations[managedBy] != "my-project/my-infra" {
		t.Errorf("managed-by = %q, want %q", got.Annotations[managedBy], "my-project/my-infra")
	}
	if _, ok := got.Annotations["openmcp.cloud/token-expires-at"]; ok {
		t.Errorf("Secret path must not set token-expires-at annotation")
	}
}

func TestReconcileSecret_CopiesSSHKeysVerbatim(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	user := sshSecret()

	if _, err := propagate.ReconcileSecret(context.Background(), cl, gitRepo(), fluxNS, user); err != nil {
		t.Fatalf("ReconcileSecret: %v", err)
	}

	got := getCredSecret(t, cl)
	if string(got.Data["identity"]) != string(user.Data["identity"]) {
		t.Errorf("identity not copied verbatim")
	}
	if string(got.Data["known_hosts"]) != string(user.Data["known_hosts"]) {
		t.Errorf("known_hosts not copied verbatim")
	}
}

func TestReconcileSecret_CreatesFluxGitRepositoryReferencingCredSecret(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	gr := gitRepo()

	if _, err := propagate.ReconcileSecret(context.Background(), cl, gr, fluxNS, httpsSecret()); err != nil {
		t.Fatalf("ReconcileSecret: %v", err)
	}

	flux := &sourcev1.GitRepository{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: grName, Namespace: fluxNS}, flux); err != nil {
		t.Fatalf("getting Flux GitRepository: %v", err)
	}
	wantRef := propagate.CredentialsSecretName(gr)
	if flux.Spec.SecretRef == nil || flux.Spec.SecretRef.Name != wantRef {
		t.Errorf("Flux secretRef = %v, want %q", flux.Spec.SecretRef, wantRef)
	}
	if flux.Spec.URL != gr.Spec.URL {
		t.Errorf("Flux URL = %q, want %q", flux.Spec.URL, gr.Spec.URL)
	}
}

func TestReconcileSecret_OverwritesOnRotate(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	gr := gitRepo()

	if _, err := propagate.ReconcileSecret(context.Background(), cl, gr, fluxNS, httpsSecret()); err != nil {
		t.Fatalf("first ReconcileSecret: %v", err)
	}

	rotated := &corev1.Secret{Data: map[string][]byte{
		keyUsername: []byte("alice"),
		keyPassword: []byte("ghp_rotated"),
	}}
	if _, err := propagate.ReconcileSecret(context.Background(), cl, gr, fluxNS, rotated); err != nil {
		t.Fatalf("second ReconcileSecret: %v", err)
	}

	got := getCredSecret(t, cl)
	if string(got.Data["password"]) != "ghp_rotated" {
		t.Errorf("password not updated on rotate: %q", string(got.Data["password"]))
	}
}

func TestReconcileSecret_ConflictOnUnmanagedFluxGitRepository(t *testing.T) {
	preexisting := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: grName, Namespace: fluxNS}, // no managed-by annotation
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(preexisting).Build()

	res, err := propagate.ReconcileSecret(context.Background(), cl, gitRepo(), fluxNS, httpsSecret())
	if err != nil {
		t.Fatalf("ReconcileSecret: %v", err)
	}
	if !res.Conflict {
		t.Errorf("expected conflict on unmanaged Flux GitRepository")
	}
}

func TestCleanup_RemovesManagedCredentialSecretAndFluxGitRepository(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	gr := gitRepo()
	if _, err := propagate.ReconcileSecret(context.Background(), cl, gr, fluxNS, httpsSecret()); err != nil {
		t.Fatalf("ReconcileSecret: %v", err)
	}

	if err := propagate.Cleanup(context.Background(), cl, gr, fluxNS); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	s := &corev1.Secret{}
	err := cl.Get(context.Background(), types.NamespacedName{Name: propagate.CredentialsSecretName(gr), Namespace: fluxNS}, s)
	if !apierrors.IsNotFound(err) {
		t.Errorf("credential Secret should be deleted, got err=%v", err)
	}
	flux := &sourcev1.GitRepository{}
	err = cl.Get(context.Background(), types.NamespacedName{Name: grName, Namespace: fluxNS}, flux)
	if !apierrors.IsNotFound(err) {
		t.Errorf("Flux GitRepository should be deleted, got err=%v", err)
	}
}

func TestCleanup_LeavesUnmanagedSecretUntouched(t *testing.T) {
	unmanaged := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: propagate.CredentialsSecretName(gitRepo()), Namespace: fluxNS}, // no managed-by
		Data:       map[string][]byte{keyUsername: []byte("someone-elses")},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(unmanaged).Build()

	if err := propagate.Cleanup(context.Background(), cl, gitRepo(), fluxNS); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	s := &corev1.Secret{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: propagate.CredentialsSecretName(gitRepo()), Namespace: fluxNS}, s); err != nil {
		t.Errorf("unmanaged Secret must not be deleted, got err=%v", err)
	}
}
