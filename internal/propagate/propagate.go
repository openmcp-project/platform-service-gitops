// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

// Package propagate implements the per-MCP reconciliation logic for a GitRepository:
// mint a scoped token, sync it as a Secret, and create/update the Flux GitRepository.
// It is intentionally decoupled from cluster access — it receives a ready client.Client
// for the target MCP and knows nothing about clusteraccess or multi-cluster wiring.
package propagate

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	"github.com/openmcp-project/platform-service-gitops/internal/githubapp"
)

const (
	// ManagedByAnnotation is set on every resource we own in the MCP so we can
	// distinguish controller-managed resources from manually-created ones.
	ManagedByAnnotation = "openmcp.cloud/managed-by"

	// TokenExpiresAtAnnotation stores the GitHub-reported token expiry on the Secret.
	TokenExpiresAtAnnotation = "openmcp.cloud/token-expires-at"

	// fluxInterval is the polling interval written into every Flux GitRepository.
	fluxInterval = 1 * time.Minute

	// secretTokenKey is the Flux-expected Secret key for bearer-token authentication.
	secretTokenKey = "bearerToken"
)

// TokenMinter creates scoped GitHub App installation tokens.
// The interface keeps the propagate package testable without a real GitHub connection.
type TokenMinter interface {
	MintScopedToken(ctx context.Context, installationID int64, repoName string) (githubapp.TokenResult, error)
}

// Result holds the outcome of a single MCP reconcile pass.
type Result struct {
	// TokenExpiresAt is the expiry reported by GitHub for the current token.
	TokenExpiresAt time.Time
	// Conflict is true when a Flux GitRepository with the same name already
	// exists in the MCP without our managed-by annotation.
	Conflict bool
}

// Reconcile ensures a scoped Secret and Flux GitRepository exist in the MCP for
// the given GitRepository CR. It mints a new token every call — callers are
// responsible for only calling when rotation is due or the Secret is absent.
func Reconcile(
	ctx context.Context,
	mcpClient client.Client,
	gitRepo *corev1alpha1.GitRepository,
	fluxNamespace string,
	installationID int64,
	minter TokenMinter,
) (Result, error) {
	// Mint an unscoped token for now — repo-scoped tokens require the GitHub App
	// to have explicit access to the specific repository, which may not always be
	// configured. An unscoped installation token still grants read access to all
	// repos the App installation covers.
	tok, err := minter.MintScopedToken(ctx, installationID, "")
	if err != nil {
		return Result{}, fmt.Errorf("minting token: %w", err)
	}

	secretName := secretName(gitRepo)
	if err := syncSecret(ctx, mcpClient, gitRepo, fluxNamespace, secretName, tok); err != nil {
		return Result{}, fmt.Errorf("syncing Secret: %w", err)
	}

	conflict, err := syncFluxGitRepository(ctx, mcpClient, gitRepo, fluxNamespace, secretName)
	if err != nil {
		return Result{}, fmt.Errorf("syncing Flux GitRepository: %w", err)
	}

	return Result{TokenExpiresAt: tok.ExpiresAt, Conflict: conflict}, nil
}

// Cleanup deletes the Secret and Flux GitRepository we own in the given MCP.
// Resources not carrying our managed-by annotation are left untouched.
func Cleanup(
	ctx context.Context,
	mcpClient client.Client,
	gitRepo *corev1alpha1.GitRepository,
	fluxNamespace string,
) error {
	managedBy := managedByValue(gitRepo)

	fluxGR := &sourcev1.GitRepository{}
	err := mcpClient.Get(ctx, types.NamespacedName{Name: gitRepo.Name, Namespace: fluxNamespace}, fluxGR)
	if err == nil && fluxGR.Annotations[ManagedByAnnotation] == managedBy {
		if err := mcpClient.Delete(ctx, fluxGR); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting Flux GitRepository: %w", err)
		}
	}

	secret := &corev1.Secret{}
	err = mcpClient.Get(ctx, types.NamespacedName{Name: secretName(gitRepo), Namespace: fluxNamespace}, secret)
	if err == nil && secret.Annotations[ManagedByAnnotation] == managedBy {
		if err := mcpClient.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting token Secret: %w", err)
		}
	}

	return nil
}

// NeedsRotation reports whether the token stored in the MCP Secret needs to be
// rotated. Returns true when the Secret does not exist, the expiry annotation is
// absent/invalid, or the token expires within renewBuffer. Transient API errors
// (network, rate-limit) are treated as "no rotation needed" to avoid burning
// GitHub token quota on every transient failure; the next reconcile will retry.
func NeedsRotation(ctx context.Context, mcpClient client.Client, gitRepo *corev1alpha1.GitRepository, fluxNamespace string, renewBuffer time.Duration) bool {
	secret := &corev1.Secret{}
	if err := mcpClient.Get(ctx, types.NamespacedName{Name: SecretName(gitRepo), Namespace: fluxNamespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return true
		}
		// Transient error — skip rotation, caller will requeue.
		return false
	}
	raw, ok := secret.Annotations[TokenExpiresAtAnnotation]
	if !ok {
		return true
	}
	expiresAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return true
	}
	return time.Until(expiresAt) <= renewBuffer
}

// syncSecret creates or updates the bearer-token Secret in the MCP.
func syncSecret(ctx context.Context, mcpClient client.Client, gitRepo *corev1alpha1.GitRepository, namespace, name string, tok githubapp.TokenResult) error {
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				ManagedByAnnotation:      managedByValue(gitRepo),
				TokenExpiresAtAnnotation: tok.ExpiresAt.UTC().Format(time.RFC3339),
			},
		},
		Data: map[string][]byte{
			secretTokenKey: []byte(tok.Token),
		},
		Type: corev1.SecretTypeOpaque,
	}

	existing := &corev1.Secret{}
	err := mcpClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing)
	if apierrors.IsNotFound(err) {
		return mcpClient.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting Secret: %w", err)
	}

	existing.Data = desired.Data
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[ManagedByAnnotation] = desired.Annotations[ManagedByAnnotation]
	existing.Annotations[TokenExpiresAtAnnotation] = desired.Annotations[TokenExpiresAtAnnotation]
	return mcpClient.Update(ctx, existing)
}

// syncFluxGitRepository creates or updates the Flux GitRepository in the MCP.
// Returns conflict=true when a resource with the same name exists but is not
// managed by this controller.
func syncFluxGitRepository(ctx context.Context, mcpClient client.Client, gitRepo *corev1alpha1.GitRepository, namespace, secretRefName string) (conflict bool, err error) {
	managedBy := managedByValue(gitRepo)

	existing := &sourcev1.GitRepository{}
	getErr := mcpClient.Get(ctx, types.NamespacedName{Name: gitRepo.Name, Namespace: namespace}, existing)

	if getErr == nil {
		// Resource exists — check ownership.
		if existing.Annotations[ManagedByAnnotation] != managedBy {
			return true, nil
		}
		// Owned by us — update.
		existing.Spec = fluxGitRepositorySpec(gitRepo, secretRefName)
		return false, mcpClient.Update(ctx, existing)
	}

	if !apierrors.IsNotFound(getErr) {
		return false, fmt.Errorf("getting Flux GitRepository: %w", getErr)
	}

	// Does not exist — create.
	desired := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gitRepo.Name,
			Namespace: namespace,
			Annotations: map[string]string{
				ManagedByAnnotation: managedBy,
			},
		},
		Spec: fluxGitRepositorySpec(gitRepo, secretRefName),
	}
	return false, mcpClient.Create(ctx, desired)
}

func fluxGitRepositorySpec(gitRepo *corev1alpha1.GitRepository, secretRefName string) sourcev1.GitRepositorySpec {
	spec := sourcev1.GitRepositorySpec{
		URL:       gitRepo.Spec.URL,
		Interval:  metav1.Duration{Duration: fluxInterval},
		SecretRef: &fluxmeta.LocalObjectReference{Name: secretRefName},
	}
	ref := gitRepo.Spec.Ref
	if ref.Branch != "" || ref.Tag != "" || ref.Commit != "" {
		spec.Reference = &sourcev1.GitRepositoryRef{
			Branch: ref.Branch,
			Tag:    ref.Tag,
			Commit: ref.Commit,
		}
	}
	return spec
}

// SecretName returns the name of the Secret written into the MCP for this GitRepository.
func SecretName(gitRepo *corev1alpha1.GitRepository) string {
	return gitRepo.Name + "-token"
}

func secretName(gitRepo *corev1alpha1.GitRepository) string {
	return SecretName(gitRepo)
}

// managedByValue returns the annotation value identifying the controller-managed
// resource: "<namespace>/<name>" of the owning GitRepository.
func managedByValue(gitRepo *corev1alpha1.GitRepository) string {
	return gitRepo.Namespace + "/" + gitRepo.Name
}
