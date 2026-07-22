// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
	"github.com/openmcp-project/platform-service-gitops/internal/controllerconst"
	"github.com/openmcp-project/platform-service-gitops/internal/credentials"
	"github.com/openmcp-project/platform-service-gitops/internal/githubapp"
	"github.com/openmcp-project/platform-service-gitops/internal/mcpclient"
)

const (
	condCredentialResolved = "CredentialResolved"
	condReady              = "Ready"

	reasonCredentialFound    = "CredentialResolved"
	reasonCredentialNotFound = "CredentialNotFound"
	reasonAppNotInstalled    = "AppNotInstalled"
	reasonUnsupportedKind    = "UnsupportedCredentialKind"

	kindAppInstallation = "AppInstallation"

	// appInstalledCondition is the condition on AppInstallation that must be True.
	appInstalledCondition = "AppInstalled"

	tokenRotationWindow = 15 * time.Minute
	fluxSecretNamespace = "flux-system"
)

// TokenMinter mints a GitHub App installation token.
type TokenMinter interface {
	MintInstallationToken(ctx context.Context, installationID int64) (string, error)
}

// MCPClientResolver resolves a PropagateTarget name to a client.Client for that cluster.
type MCPClientResolver interface {
	Resolve(ctx context.Context, namespace, mcpName string) (client.Client, error)
}

type mcpClientResolverFunc func(ctx context.Context, namespace, mcpName string) (client.Client, error)

func (f mcpClientResolverFunc) Resolve(ctx context.Context, namespace, mcpName string) (client.Client, error) {
	return f(ctx, namespace, mcpName)
}

// GitRepositoryReconciler resolves a GitRepository's credentialRef to an
// AppInstallation and sets the CredentialResolved/Ready conditions based on the
// AppInstallation's verified status. When credentials are resolved and
// PropagateToControlPlanes is non-empty, it mints scoped GitHub App installation
// tokens and writes them as Secrets into each target MCP cluster.
//
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=appinstallations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
type GitRepositoryReconciler struct {
	client              client.Client
	credentialNamespace string
	newClient           func(githubapp.Credentials) (TokenMinter, error)
	mcpResolver         MCPClientResolver
	resolveCredentials  func(ctx context.Context, ai *githubv1alpha1.AppInstallation) (githubapp.Credentials, error)
}

// NewGitRepositoryReconciler creates a reconciler with the given client and
// credential namespace (used to resolve GitHubInstance credential Secrets).
func NewGitRepositoryReconciler(c client.Client, credentialNamespace string) *GitRepositoryReconciler {
	r := &GitRepositoryReconciler{
		client:              c,
		credentialNamespace: credentialNamespace,
		newClient: func(creds githubapp.Credentials) (TokenMinter, error) {
			return githubapp.NewClient(creds)
		},
	}
	r.mcpResolver = mcpClientResolverFunc(func(ctx context.Context, ns, name string) (client.Client, error) {
		return mcpclient.Resolve(ctx, c, ns, name)
	})
	r.resolveCredentials = func(ctx context.Context, ai *githubv1alpha1.AppInstallation) (githubapp.Credentials, error) {
		return credentials.Resolve(ctx, c, ai.Spec.InstanceRef.Name, ai.Spec.CredentialName, credentialNamespace)
	}
	return r
}

// SetTokenMinterFactory replaces the factory used to build a TokenMinter from
// credentials. Intended for testing only.
func (r *GitRepositoryReconciler) SetTokenMinterFactory(f func(githubapp.Credentials) (TokenMinter, error)) {
	r.newClient = f
}

// SetMCPClientResolver replaces the MCPClientResolver. Intended for testing only.
func (r *GitRepositoryReconciler) SetMCPClientResolver(res MCPClientResolver) {
	r.mcpResolver = res
}

// SetCredentialResolver replaces the credential resolution function. Intended for testing only.
func (r *GitRepositoryReconciler) SetCredentialResolver(f func(ctx context.Context, ai *githubv1alpha1.AppInstallation) (githubapp.Credentials, error)) {
	r.resolveCredentials = f
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
// It also watches AppInstallations so a GitRepository re-reconciles when the
// AppInstallation it references becomes installed (or changes).
func (r *GitRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.GitRepository{}).
		Watches(&githubv1alpha1.AppInstallation{}, handler.EnqueueRequestsFromMapFunc(r.mapAppInstallationToGitRepositories)).
		Complete(r)
}

// mapAppInstallationToGitRepositories returns reconcile requests for every
// GitRepository in the AppInstallation's namespace that references it.
func (r *GitRepositoryReconciler) mapAppInstallationToGitRepositories(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &corev1alpha1.GitRepositoryList{}
	if err := r.client.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		gr := &list.Items[i]
		if gr.Spec.CredentialRef.Kind == kindAppInstallation && gr.Spec.CredentialRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: gr.Name, Namespace: gr.Namespace}})
		}
	}
	return reqs
}

func (r *GitRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	gr := &corev1alpha1.GitRepository{}
	if err := r.client.Get(ctx, req.NamespacedName, gr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching GitRepository: %w", err)
	}

	patch := client.MergeFrom(gr.DeepCopy())
	resolved, reason, installationID := r.resolveCredential(ctx, gr)

	if resolved {
		setCondition(&gr.Status.Conditions, metav1.Condition{
			Type:               condReady,
			Status:             metav1.ConditionTrue,
			Reason:             reasonCredentialFound,
			Message:            "Credentials resolved; repository access verified.",
			ObservedGeneration: gr.Generation,
		})
	} else {
		setCondition(&gr.Status.Conditions, metav1.Condition{
			Type:               condReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            "Credentials are not resolved; see the CredentialResolved condition.",
			ObservedGeneration: gr.Generation,
		})
	}

	var requeueAfter time.Duration
	if len(gr.Spec.PropagateToControlPlanes) > 0 || len(gr.Status.PropagateStatus) > 0 {
		nextRotation, err := r.syncTokens(ctx, gr, resolved, installationID)
		if err != nil {
			return ctrl.Result{}, err
		}
		requeueAfter = nextRotation
	}

	gr.Status.ObservedGeneration = gr.Generation
	if err := r.client.Status().Patch(ctx, gr, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
	}

	logger.Info("Reconciled GitRepository", "name", req.Name, "credentialResolved", resolved)
	if requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{RequeueAfter: controllerconst.RequeueInterval}, nil
}

// resolveCredential resolves the credentialRef to an AppInstallation, verifies
// the App is installed, and returns whether resolution succeeded, the reason
// (used for the Ready condition), and the installation ID (non-zero on success).
func (r *GitRepositoryReconciler) resolveCredential(ctx context.Context, gr *corev1alpha1.GitRepository) (bool, string, int64) {
	ref := gr.Spec.CredentialRef

	if ref.Kind != kindAppInstallation {
		r.setResolved(gr, metav1.ConditionFalse, reasonUnsupportedKind,
			fmt.Sprintf("credentialRef.kind %q is not supported; only %q is.", ref.Kind, kindAppInstallation))
		return false, reasonUnsupportedKind, 0
	}

	// AppInstallation is referenced by name in the same namespace as the GitRepository.
	ai := &githubv1alpha1.AppInstallation{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: gr.Namespace}, ai); err != nil {
		if apierrors.IsNotFound(err) {
			r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
				fmt.Sprintf("AppInstallation %s not found in namespace %s.", ref.Name, gr.Namespace))
			return false, reasonCredentialNotFound, 0
		}
		r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
			fmt.Sprintf("Fetching AppInstallation %s failed: %v.", ref.Name, err))
		return false, reasonCredentialNotFound, 0
	}

	// The AppInstallation controller already verified App installation and
	// access. Trusting its status avoids minting an installation token here:
	// tokens are rate-limited (1 per installation per hour on GHE) and would be
	// discarded, so minting on every reconcile would exhaust the quota. Token
	// minting happens only where a token is actually used (the propagateTo flow).
	if !meta.IsStatusConditionTrue(ai.Status.Conditions, appInstalledCondition) || ai.Status.InstallationID == 0 {
		r.setResolved(gr, metav1.ConditionFalse, reasonAppNotInstalled,
			fmt.Sprintf("AppInstallation %s is not ready (App not installed yet).", ref.Name))
		return false, reasonAppNotInstalled, 0
	}

	r.setResolved(gr, metav1.ConditionTrue, reasonCredentialFound,
		fmt.Sprintf("Resolved via AppInstallation %s (installation %d).", ref.Name, ai.Status.InstallationID))
	return true, reasonCredentialFound, ai.Status.InstallationID
}

func (r *GitRepositoryReconciler) setResolved(gr *corev1alpha1.GitRepository, status metav1.ConditionStatus, reason, msg string) {
	setCondition(&gr.Status.Conditions, metav1.Condition{
		Type:               condCredentialResolved,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: gr.Generation,
	})
}

func setCondition(conditions *[]metav1.Condition, c metav1.Condition) {
	if c.LastTransitionTime.IsZero() {
		c.LastTransitionTime = metav1.Now()
	}
	meta.SetStatusCondition(conditions, c)
}

// syncTokens ensures a scoped installation token Secret exists and is fresh in
// every target MCP cluster. It also cleans up Secrets for MCPs that have been
// removed from the spec. It returns the duration until the earliest token
// rotation is needed (so the reconciler can requeue before expiry).
func (r *GitRepositoryReconciler) syncTokens(
	ctx context.Context,
	gr *corev1alpha1.GitRepository,
	resolved bool,
	installationID int64,
) (time.Duration, error) {
	logger := log.FromContext(ctx)

	statusByName := make(map[string]*corev1alpha1.MCPPropagateState, len(gr.Status.PropagateStatus))
	for i := range gr.Status.PropagateStatus {
		statusByName[gr.Status.PropagateStatus[i].Name] = &gr.Status.PropagateStatus[i]
	}

	specNames := make(map[string]bool, len(gr.Spec.PropagateToControlPlanes))
	for _, t := range gr.Spec.PropagateToControlPlanes {
		specNames[t.Name] = true
	}

	// Clean up Secrets for MCPs that are no longer in the spec.
	for _, st := range gr.Status.PropagateStatus {
		if !specNames[st.Name] {
			if err := r.deleteTokenSecret(ctx, gr, st.Name); err != nil {
				logger.Error(err, "failed to delete token Secret for removed MCP", "mcp", st.Name)
			}
		}
	}

	// When credentials are not resolved we can only clean up — do not mint new tokens.
	if !resolved {
		gr.Status.PropagateStatus = nil
		return 0, nil
	}

	var newStatus []corev1alpha1.MCPPropagateState
	var earliestExpiry time.Duration

	for _, target := range gr.Spec.PropagateToControlPlanes {
		existing := statusByName[target.Name]
		st := r.syncOneMCP(ctx, gr, target, existing, installationID)
		newStatus = append(newStatus, st)

		if st.TokenExpiresAt != nil {
			remaining := time.Until(st.TokenExpiresAt.Time) - tokenRotationWindow
			if remaining > 0 && (earliestExpiry == 0 || remaining < earliestExpiry) {
				earliestExpiry = remaining
			}
		}
	}

	gr.Status.PropagateStatus = newStatus
	return earliestExpiry, nil
}

func (r *GitRepositoryReconciler) syncOneMCP(
	ctx context.Context,
	gr *corev1alpha1.GitRepository,
	target corev1alpha1.PropagateTarget,
	existing *corev1alpha1.MCPPropagateState,
	installationID int64,
) corev1alpha1.MCPPropagateState {
	logger := log.FromContext(ctx)

	needsToken := existing == nil ||
		existing.Phase != corev1alpha1.TokenSyncPhaseTokenSynced ||
		existing.TokenExpiresAt == nil ||
		time.Until(existing.TokenExpiresAt.Time) <= tokenRotationWindow

	if !needsToken {
		return *existing
	}

	mcpClient, err := r.mcpResolver.Resolve(ctx, r.credentialNamespace, target.Name)
	if err != nil {
		logger.Error(err, "failed to resolve MCP client", "mcp", target.Name)
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("resolving MCP client failed: %v", err),
		}
	}

	ai := &githubv1alpha1.AppInstallation{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: gr.Spec.CredentialRef.Name, Namespace: gr.Namespace}, ai); err != nil {
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("re-fetching AppInstallation failed: %v", err),
		}
	}

	creds, err := r.resolveCredentials(ctx, ai)
	if err != nil {
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("resolving credentials failed: %v", err),
		}
	}

	gh, err := r.newClient(creds)
	if err != nil {
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("building GitHub client failed: %v", err),
		}
	}

	token, err := gh.MintInstallationToken(ctx, installationID)
	if err != nil {
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("minting token failed: %v", err),
		}
	}

	expiry := metav1.NewTime(time.Now().Add(1 * time.Hour))
	secretName := tokenSecretName(gr)

	if err := r.writeTokenSecret(ctx, mcpClient, secretName, gr, token); err != nil {
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("writing token Secret failed: %v", err),
		}
	}

	logger.Info("synced token to MCP", "mcp", target.Name, "secret", secretName)
	return corev1alpha1.MCPPropagateState{
		Name:           target.Name,
		Phase:          corev1alpha1.TokenSyncPhaseTokenSynced,
		Message:        fmt.Sprintf("Token synced to Secret %s/%s.", fluxSecretNamespace, secretName),
		TokenExpiresAt: &expiry,
	}
}

func tokenSecretName(gr *corev1alpha1.GitRepository) string {
	name := fmt.Sprintf("gitrepository-%s-%s", gr.Namespace, gr.Name)
	if len(name) <= 253 {
		return name
	}
	// Namespace+name combination exceeds the 253-char Kubernetes name limit.
	// Fall back to a deterministic hash so the name is always valid.
	h := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	return "gitrepository-" + h[:16]
}

func (r *GitRepositoryReconciler) writeTokenSecret(
	ctx context.Context,
	mcpClient client.Client,
	name string,
	gr *corev1alpha1.GitRepository,
	token string,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: fluxSecretNamespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, mcpClient, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = make(map[string]string)
		}
		secret.Labels["gitops.open-control-plane.io/source-namespace"] = gr.Namespace
		secret.Labels["gitops.open-control-plane.io/source-name"] = gr.Name
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data["username"] = []byte("x-access-token")
		secret.Data["password"] = []byte(token)
		// Secret.Type is immutable once set. If the existing Secret has a different
		// type (e.g. Opaque), delete it and let CreateOrUpdate recreate it.
		if secret.Type != "" && secret.Type != corev1.SecretTypeBasicAuth {
			if delErr := mcpClient.Delete(ctx, secret); delErr != nil && !apierrors.IsNotFound(delErr) {
				return fmt.Errorf("deleting Secret with wrong type: %w", delErr)
			}
			secret.ResourceVersion = ""
		}
		secret.Type = corev1.SecretTypeBasicAuth
		return nil
	})
	return err
}

func (r *GitRepositoryReconciler) deleteTokenSecret(
	ctx context.Context,
	gr *corev1alpha1.GitRepository,
	mcpName string,
) error {
	mcpClient, err := r.mcpResolver.Resolve(ctx, r.credentialNamespace, mcpName)
	if err != nil {
		return fmt.Errorf("resolving MCP client: %w", err)
	}
	secret := &corev1.Secret{}
	name := tokenSecretName(gr)
	if err := mcpClient.Get(ctx, types.NamespacedName{Name: name, Namespace: fluxSecretNamespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("fetching Secret %s/%s: %w", fluxSecretNamespace, name, err)
	}
	return mcpClient.Delete(ctx, secret)
}
