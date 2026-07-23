// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"errors"
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
	"github.com/openmcp-project/platform-service-gitops/internal/mcpaccess"
	"github.com/openmcp-project/platform-service-gitops/internal/propagate"
)

const (
	condCredentialResolved = "CredentialResolved"
	condReady              = "Ready"

	reasonCredentialFound    = "CredentialResolved"
	reasonCredentialNotFound = "CredentialNotFound"
	reasonAppNotInstalled    = "AppNotInstalled"
	reasonUnsupportedKind    = "UnsupportedCredentialKind"
	reasonUnsupportedSecret  = "UnsupportedSecretFormat"
	reasonAuthFailed         = "AuthenticationFailed"
	reasonRepoUnreachable    = "RepositoryUnreachable"

	// Secret-path propagation reasons (per-MCP status). Distinct from the App
	// path's "Synced" so status readers can tell a copied user credential from a
	// minted token.
	reasonSecretSynced        = "SecretSynced"
	reasonSecretCopyFailed    = "SecretCopyFailed"
	reasonClusterAccessFailed = "ClusterAccessFailed"

	kindAppInstallation = "AppInstallation"
	kindSecret          = "Secret"

	appInstalledCondition = "AppInstalled"

	// validateAccessTimeout bounds the live ls-remote performed for the Secret
	// credential path so a slow or hung Git host cannot block a reconcile worker.
	validateAccessTimeout = 30 * time.Second

	finalizerPropagate = "gitops.open-control-plane.io/propagate"
)

// GitRepositoryReconciler resolves a GitRepository's credentialRef and reports
// readiness. For both credential kinds it additionally syncs a credential Secret
// and a Flux GitRepository into each MCP listed in propagateTo and keeps per-MCP
// status up to date: kind:AppInstallation mints scoped bearer tokens, while
// kind:Secret copies the user-supplied credential Secret in verbatim (validated
// against the repository on the onboarding cluster first).
//
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=appinstallations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=clusters.openmcp.cloud,resources=clusterrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clusters.openmcp.cloud,resources=accessrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch
type GitRepositoryReconciler struct {
	// onboardingClient reads GitRepository and AppInstallation from the onboarding cluster.
	onboardingClient client.Client
	// platformClient reads GitHubInstance and credential Secrets from the platform cluster.
	platformClient client.Client

	mcpResolver         mcpaccess.Resolver
	credentialNamespace string
	fluxNamespace       string
	tokenRenewBuffer    time.Duration

	// validateAccess verifies a resolved Secret credential against the repository
	// via a live ls-remote. It defaults to credentials.ValidateAccess and is
	// overridable in tests so the resolve+propagate path can be exercised without
	// network access.
	validateAccess func(ctx context.Context, url string, cred *credentials.Credential) error
}

// NewGitRepositoryReconciler creates a reconciler.
func NewGitRepositoryReconciler(
	onboardingClient client.Client,
	platformClient client.Client,
	mcpResolver mcpaccess.Resolver,
	credentialNamespace string,
	fluxNamespace string,
	tokenRenewBuffer time.Duration,
) *GitRepositoryReconciler {
	return &GitRepositoryReconciler{
		onboardingClient:    onboardingClient,
		platformClient:      platformClient,
		mcpResolver:         mcpResolver,
		credentialNamespace: credentialNamespace,
		fluxNamespace:       fluxNamespace,
		tokenRenewBuffer:    tokenRenewBuffer,
		validateAccess:      credentials.ValidateAccess,
	}
}

// WithValidateAccess overrides the credential-access validator. It exists so
// tests can exercise the resolve+propagate path without network access; the
// default (credentials.ValidateAccess) is used in production.
func (r *GitRepositoryReconciler) WithValidateAccess(fn func(ctx context.Context, url string, cred *credentials.Credential) error) *GitRepositoryReconciler {
	r.validateAccess = fn
	return r
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
// It watches AppInstallations so a GitRepository re-reconciles when the
// AppInstallation it references becomes installed (or changes), and Secrets so a
// GitRepository re-reconciles when a referenced credential Secret is rotated.
func (r *GitRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.GitRepository{}).
		Watches(&githubv1alpha1.AppInstallation{},
			handler.EnqueueRequestsFromMapFunc(r.mapAppInstallationToGitRepositories)).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToGitRepositories)).
		Complete(r)
}

func (r *GitRepositoryReconciler) mapAppInstallationToGitRepositories(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &corev1alpha1.GitRepositoryList{}
	if err := r.onboardingClient.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
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

// mapSecretToGitRepositories returns reconcile requests for every GitRepository
// in the Secret's namespace that references it via a kind:Secret credentialRef.
// This makes credential rotation (updating the Secret) take effect on the next
// reconcile without restarting the resource.
func (r *GitRepositoryReconciler) mapSecretToGitRepositories(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &corev1alpha1.GitRepositoryList{}
	if err := r.onboardingClient.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		gr := &list.Items[i]
		if gr.Spec.CredentialRef.Kind == kindSecret && gr.Spec.CredentialRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: gr.Name, Namespace: gr.Namespace}})
		}
	}
	return reqs
}

func (r *GitRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	gr := &corev1alpha1.GitRepository{}
	if err := r.onboardingClient.Get(ctx, req.NamespacedName, gr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching GitRepository: %w", err)
	}

	// Handle deletion.
	if !gr.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, gr)
	}

	// Ensure finalizer.
	if !controllerutil.ContainsFinalizer(gr, finalizerPropagate) {
		controllerutil.AddFinalizer(gr, finalizerPropagate)
		if err := r.onboardingClient.Update(ctx, gr); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	patch := client.MergeFrom(gr.DeepCopy())

	// Resolve credential.
	rc := r.resolveCredential(ctx, gr)

	if !rc.resolved {
		setCondition(&gr.Status.Conditions, metav1.Condition{
			Type:               condReady,
			Status:             metav1.ConditionFalse,
			Reason:             rc.reason,
			Message:            "Credentials are not resolved; see the CredentialResolved condition.",
			ObservedGeneration: gr.Generation,
		})
		gr.Status.ObservedGeneration = gr.Generation
		if err := r.onboardingClient.Status().Patch(ctx, gr, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
		}
		return ctrl.Result{RequeueAfter: controllerconst.RequeueInterval}, nil
	}

	// Propagate to MCPs. The AppInstallation path mints per-MCP scoped tokens; the
	// Secret path copies the user's credential Secret in verbatim. The Secret path
	// only propagates when propagateTo is set — otherwise it is onboarding-cluster
	// validation only, preserving prior behavior.
	var requeueAfter time.Duration
	readyMessage := "Credentials resolved; repository access verified."
	switch {
	case rc.appInstallation != nil:
		requeueAfter = r.reconcilePropagate(ctx, gr, rc.appInstallation, rc.installationID)
		readyMessage = "Credentials resolved; propagation in progress."
	case rc.userSecret != nil && len(gr.Spec.PropagateToControlPlanes) > 0:
		requeueAfter = r.reconcilePropagateSecret(ctx, gr, rc.userSecret)
		readyMessage = "Credentials resolved; propagation in progress."
	}

	setCondition(&gr.Status.Conditions, metav1.Condition{
		Type:               condReady,
		Status:             metav1.ConditionTrue,
		Reason:             reasonCredentialFound,
		Message:            readyMessage,
		ObservedGeneration: gr.Generation,
	})
	gr.Status.ObservedGeneration = gr.Generation
	if err := r.onboardingClient.Status().Patch(ctx, gr, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
	}

	logger.Info("Reconciled GitRepository", "name", req.Name, "requeueAfter", requeueAfter)
	if requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{RequeueAfter: controllerconst.RequeueInterval}, nil
}

// reconcilePropagate iterates over all propagateTo targets, resolves MCP access,
// syncs the token Secret + Flux GitRepository, updates per-target status, and
// returns the earliest token rotation deadline across all targets.
func (r *GitRepositoryReconciler) reconcilePropagate(ctx context.Context, gr *corev1alpha1.GitRepository, ai *githubv1alpha1.AppInstallation, installationID int64) time.Duration {
	logger := log.FromContext(ctx)

	targets, err := r.mcpResolver.Resolve(ctx, gr, r.onboardingClient)
	if err != nil {
		logger.Error(err, "failed to resolve propagateTo targets")
		return controllerconst.RequeueInterval
	}

	// Build creds + minter once for all targets (same AppInstallation for all).
	creds, err := r.resolveGitHubCreds(ctx, ai)
	if err != nil {
		logger.Error(err, "failed to resolve GitHub credentials for propagation")
		return controllerconst.RequeueInterval
	}
	ghClient, err := githubapp.NewClient(creds)
	if err != nil {
		logger.Error(err, "failed to build GitHub App client")
		return controllerconst.RequeueInterval
	}

	// Track the desired set of ControlPlanes (keyed by namespace/name) for status cleanup.
	desiredNames := map[string]struct{}{}
	for _, t := range targets {
		desiredNames[propagateKey(t.ControlPlaneNamespace, t.ControlPlaneName)] = struct{}{}
	}

	var earliest time.Duration
	for _, target := range targets {
		ps := r.reconcileTarget(ctx, gr, target, installationID, ghClient)
		setPropagateStatus(&gr.Status.Propagated, ps)

		if ps.TokenExpiresAt != nil {
			until := max(time.Until(ps.TokenExpiresAt.Time)-r.tokenRenewBuffer, 0)
			if earliest == 0 || until < earliest {
				earliest = until
			}
		}
	}

	// Remove status entries for targets no longer in propagateTo.
	gr.Status.Propagated = filterPropagateStatus(gr.Status.Propagated, desiredNames)

	return earliest
}

// reconcileTarget processes a single resolved MCP target and returns its status.
func (r *GitRepositoryReconciler) reconcileTarget(
	ctx context.Context,
	gr *corev1alpha1.GitRepository,
	target mcpaccess.ResolvedTarget,
	installationID int64,
	minter propagate.TokenMinter,
) corev1alpha1.PropagateStatus {
	ps := corev1alpha1.PropagateStatus{
		ControlPlaneNamespace: target.ControlPlaneNamespace,
		ControlPlaneName:      target.ControlPlaneName,
	}

	if target.Pending {
		ps.Phase = corev1alpha1.PropagatePhasePending
		ps.Reason = "AccessRequestPending"
		ps.Message = "Waiting for MCP cluster access to be granted."
		return ps
	}
	if target.Cluster == nil {
		ps.Phase = corev1alpha1.PropagatePhaseTokenFailed
		ps.Reason = "ClusterAccessFailed"
		ps.Message = "MCP cluster access could not be obtained."
		return ps
	}

	mcpClient := target.Cluster.Client()

	// Only mint a new token if rotation is due.
	if !propagate.NeedsRotation(ctx, mcpClient, gr, r.fluxNamespace, r.tokenRenewBuffer) {
		ps.Phase = corev1alpha1.PropagatePhaseReady
		ps.Reason = "Synced"
		ps.Message = "Token and Flux GitRepository are up to date."
		ps.TokenExpiresAt = currentTokenExpiry(ctx, mcpClient, gr, r.fluxNamespace)
		return ps
	}

	result, err := propagate.Reconcile(ctx, mcpClient, gr, r.fluxNamespace, installationID, minter)
	if err != nil {
		ps.Phase = corev1alpha1.PropagatePhaseTokenFailed
		ps.Reason = "ReconcileFailed"
		ps.Message = err.Error()
		return ps
	}

	if result.Conflict {
		ps.Phase = corev1alpha1.PropagatePhaseConflict
		ps.Reason = "FluxGitRepositoryConflict"
		ps.Message = fmt.Sprintf(
			"A Flux GitRepository named %q already exists in %s/%s without the managed-by annotation; will not overwrite.",
			gr.Name, r.fluxNamespace, gr.Name)
		return ps
	}

	expiresAt := metav1.NewTime(result.TokenExpiresAt)
	ps.Phase = corev1alpha1.PropagatePhaseReady
	ps.Reason = "Synced"
	ps.Message = "Token and Flux GitRepository are up to date."
	ps.TokenExpiresAt = &expiresAt
	return ps
}

// reconcilePropagateSecret copies a user-supplied credential Secret into every
// propagateTo target MCP and ensures a Flux GitRepository references it. It is the
// Secret-path analogue of reconcilePropagate: no token minting, so no per-target
// rotation deadline — it always returns 0 and the caller requeues on the normal
// interval.
func (r *GitRepositoryReconciler) reconcilePropagateSecret(ctx context.Context, gr *corev1alpha1.GitRepository, userSecret *corev1.Secret) time.Duration {
	logger := log.FromContext(ctx)

	targets, err := r.mcpResolver.Resolve(ctx, gr, r.onboardingClient)
	if err != nil {
		logger.Error(err, "failed to resolve propagateTo targets")
		return controllerconst.RequeueInterval
	}

	desiredNames := map[string]struct{}{}
	for _, t := range targets {
		desiredNames[propagateKey(t.ControlPlaneNamespace, t.ControlPlaneName)] = struct{}{}
	}

	for _, target := range targets {
		ps := r.reconcileTargetSecret(ctx, gr, target, userSecret)
		setPropagateStatus(&gr.Status.Propagated, ps)
	}

	gr.Status.Propagated = filterPropagateStatus(gr.Status.Propagated, desiredNames)

	return 0
}

// reconcileTargetSecret processes a single MCP target for the Secret path and
// returns its status. There is no token, so TokenExpiresAt is left nil and copy
// failures surface as FluxFailed.
func (r *GitRepositoryReconciler) reconcileTargetSecret(
	ctx context.Context,
	gr *corev1alpha1.GitRepository,
	target mcpaccess.ResolvedTarget,
	userSecret *corev1.Secret,
) corev1alpha1.PropagateStatus {
	ps := corev1alpha1.PropagateStatus{
		ControlPlaneNamespace: target.ControlPlaneNamespace,
		ControlPlaneName:      target.ControlPlaneName,
	}

	if target.Pending {
		ps.Phase = corev1alpha1.PropagatePhasePending
		ps.Reason = "AccessRequestPending"
		ps.Message = "Waiting for MCP cluster access to be granted."
		return ps
	}
	if target.Cluster == nil {
		ps.Phase = corev1alpha1.PropagatePhaseFluxFailed
		ps.Reason = reasonClusterAccessFailed
		ps.Message = "MCP cluster access could not be obtained."
		return ps
	}

	result, err := propagate.ReconcileSecret(ctx, target.Cluster.Client(), gr, r.fluxNamespace, userSecret)
	if err != nil {
		ps.Phase = corev1alpha1.PropagatePhaseFluxFailed
		ps.Reason = reasonSecretCopyFailed
		ps.Message = err.Error()
		return ps
	}

	if result.Conflict {
		ps.Phase = corev1alpha1.PropagatePhaseConflict
		ps.Reason = "FluxGitRepositoryConflict"
		ps.Message = fmt.Sprintf(
			"A Flux GitRepository named %q already exists in %s/%s without the managed-by annotation; will not overwrite.",
			gr.Name, r.fluxNamespace, gr.Name)
		return ps
	}

	ps.Phase = corev1alpha1.PropagatePhaseReady
	ps.Reason = reasonSecretSynced
	ps.Message = "Credential Secret and Flux GitRepository are up to date."
	return ps
}

// reconcileDelete removes owned resources from all MCPs and then removes the finalizer.
func (r *GitRepositoryReconciler) reconcileDelete(ctx context.Context, gr *corev1alpha1.GitRepository) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(gr, finalizerPropagate) {
		return ctrl.Result{}, nil
	}

	targets, err := r.mcpResolver.Resolve(ctx, gr, r.onboardingClient)
	if err != nil {
		// Do not remove the finalizer — requeue and retry cleanup once access is available.
		logger.Error(err, "failed to resolve targets during deletion; will retry")
		return ctrl.Result{RequeueAfter: controllerconst.RequeueInterval}, fmt.Errorf("resolving targets for cleanup: %w", err)
	}

	var cleanupErrs []error
	for _, target := range targets {
		// Always clean up the AccessRequest, regardless of whether cluster access was granted.
		if err := r.mcpResolver.Cleanup(ctx, gr, target.ControlPlaneNamespace, target.ControlPlaneName); err != nil {
			logger.Error(err, "AccessRequest cleanup failed", "controlPlaneNamespace", target.ControlPlaneNamespace, "controlPlane", target.ControlPlaneName)
			cleanupErrs = append(cleanupErrs, err)
		}
		if target.Cluster == nil {
			continue
		}
		if err := propagate.Cleanup(ctx, target.Cluster.Client(), gr, r.fluxNamespace); err != nil {
			logger.Error(err, "MCP resource cleanup failed", "controlPlane", target.ControlPlaneName)
			cleanupErrs = append(cleanupErrs, err)
		}
	}

	if len(cleanupErrs) > 0 {
		return ctrl.Result{RequeueAfter: controllerconst.RequeueInterval},
			fmt.Errorf("cleanup incomplete (%d errors), will retry", len(cleanupErrs))
	}

	controllerutil.RemoveFinalizer(gr, finalizerPropagate)
	if err := r.onboardingClient.Update(ctx, gr); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// resolvedCredential carries the outcome of credential resolution across both
// credential kinds. Exactly one of appInstallation / userSecret is non-nil on
// success: appInstallation drives the token-minting propagation path, userSecret
// drives the verbatim copy-in propagation path.
type resolvedCredential struct {
	resolved bool
	reason   string

	// AppInstallation path.
	appInstallation *githubv1alpha1.AppInstallation
	installationID  int64

	// Secret path: the user-supplied credential Secret, copied verbatim into MCPs.
	userSecret *corev1.Secret
}

// resolveCredential dispatches on credentialRef.kind and sets the
// CredentialResolved condition. On success it returns a resolvedCredential whose
// appInstallation (AppInstallation path) or userSecret (Secret path) is set; that
// is what the reconcile loop propagates to the MCPs named in propagateTo.
func (r *GitRepositoryReconciler) resolveCredential(ctx context.Context, gr *corev1alpha1.GitRepository) resolvedCredential {
	switch gr.Spec.CredentialRef.Kind {
	case kindAppInstallation:
		return r.resolveAppInstallation(ctx, gr)
	case kindSecret:
		return r.resolveSecret(ctx, gr)
	default:
		r.setResolved(gr, metav1.ConditionFalse, reasonUnsupportedKind,
			fmt.Sprintf("credentialRef.kind %q is not supported; supported kinds are %q and %q.",
				gr.Spec.CredentialRef.Kind, kindAppInstallation, kindSecret))
		return resolvedCredential{reason: reasonUnsupportedKind}
	}
}

// resolveAppInstallation resolves the credentialRef to an AppInstallation and
// verifies (via its status) that the App is installed.
func (r *GitRepositoryReconciler) resolveAppInstallation(ctx context.Context, gr *corev1alpha1.GitRepository) resolvedCredential {
	ref := gr.Spec.CredentialRef

	ai := &githubv1alpha1.AppInstallation{}
	if err := r.onboardingClient.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: gr.Namespace}, ai); err != nil {
		if apierrors.IsNotFound(err) {
			r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
				fmt.Sprintf("AppInstallation %s not found in namespace %s.", ref.Name, gr.Namespace))
			return resolvedCredential{reason: reasonCredentialNotFound}
		}
		r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
			fmt.Sprintf("Fetching AppInstallation %s failed: %v.", ref.Name, err))
		return resolvedCredential{reason: reasonCredentialNotFound}
	}

	if !meta.IsStatusConditionTrue(ai.Status.Conditions, appInstalledCondition) || ai.Status.InstallationID == 0 {
		r.setResolved(gr, metav1.ConditionFalse, reasonAppNotInstalled,
			fmt.Sprintf("AppInstallation %s is not ready (App not installed yet).", ref.Name))
		return resolvedCredential{reason: reasonAppNotInstalled}
	}

	r.setResolved(gr, metav1.ConditionTrue, reasonCredentialFound,
		fmt.Sprintf("Resolved via AppInstallation %s (installation %d).", ref.Name, ai.Status.InstallationID))
	return resolvedCredential{
		resolved:        true,
		reason:          reasonCredentialFound,
		appInstallation: ai,
		installationID:  ai.Status.InstallationID,
	}
}

// resolveGitHubCreds resolves the full GitHub App credentials for token minting.
// It reuses the already-fetched AppInstallation to avoid a second fetch (TOCTOU).
func (r *GitRepositoryReconciler) resolveGitHubCreds(ctx context.Context, ai *githubv1alpha1.AppInstallation) (githubapp.Credentials, error) {
	return credentials.Resolve(ctx, r.platformClient, ai.Spec.InstanceRef.Name, "", r.credentialNamespace)
}

// resolveSecret resolves a user-supplied credential Secret (PAT or SSH) from the
// GitRepository's own namespace and verifies it by performing a live ls-remote
// against the repository. Unlike the AppInstallation path there is no upstream
// controller vouching for the credential, so access is validated here directly.
// On success it returns the raw Secret so the reconcile loop can copy it verbatim
// into each MCP named in propagateTo.
func (r *GitRepositoryReconciler) resolveSecret(ctx context.Context, gr *corev1alpha1.GitRepository) resolvedCredential {
	ref := gr.Spec.CredentialRef

	// The Secret must live in the GitRepository's own namespace. Reading Secrets
	// from other namespaces would allow privilege escalation.
	secret := &corev1.Secret{}
	if err := r.onboardingClient.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: gr.Namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
				fmt.Sprintf("Secret %s not found in namespace %s.", ref.Name, gr.Namespace))
			return resolvedCredential{reason: reasonCredentialNotFound}
		}
		r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
			fmt.Sprintf("Fetching Secret %s failed: %v.", ref.Name, err))
		return resolvedCredential{reason: reasonCredentialNotFound}
	}

	cred, err := credentials.ResolveSecret(secret)
	if err != nil {
		// Error messages here describe the Secret shape, never its contents.
		r.setResolved(gr, metav1.ConditionFalse, reasonUnsupportedSecret,
			fmt.Sprintf("Secret %s: %v.", ref.Name, err))
		return resolvedCredential{reason: reasonUnsupportedSecret}
	}

	// Bound the live ls-remote so a slow or hung host cannot block the worker.
	validateCtx, cancel := context.WithTimeout(ctx, validateAccessTimeout)
	defer cancel()
	if err := r.validateAccess(validateCtx, gr.Spec.URL, cred); err != nil {
		// credentials errors are classified and carry no secret material.
		reason := reasonRepoUnreachable
		if errors.Is(err, credentials.ErrAuthFailed) {
			reason = reasonAuthFailed
		}
		r.setResolved(gr, metav1.ConditionFalse, reason,
			fmt.Sprintf("Secret %s: %v.", ref.Name, err))
		return resolvedCredential{reason: reason}
	}

	r.setResolved(gr, metav1.ConditionTrue, reasonCredentialFound,
		fmt.Sprintf("Resolved via Secret %s; repository access verified.", ref.Name))
	return resolvedCredential{
		resolved:   true,
		reason:     reasonCredentialFound,
		userSecret: secret,
	}
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

// propagateKey is the composite (namespace, name) identity of a propagate target.
func propagateKey(namespace, name string) string {
	return namespace + "/" + name
}

// setPropagateStatus upserts a PropagateStatus entry by (ControlPlaneNamespace, ControlPlaneName).
func setPropagateStatus(list *[]corev1alpha1.PropagateStatus, ps corev1alpha1.PropagateStatus) {
	for i := range *list {
		if (*list)[i].ControlPlaneNamespace == ps.ControlPlaneNamespace &&
			(*list)[i].ControlPlaneName == ps.ControlPlaneName {
			(*list)[i] = ps
			return
		}
	}
	*list = append(*list, ps)
}

// filterPropagateStatus removes entries whose (namespace, name) is not in desired.
func filterPropagateStatus(list []corev1alpha1.PropagateStatus, desired map[string]struct{}) []corev1alpha1.PropagateStatus {
	out := make([]corev1alpha1.PropagateStatus, 0, len(list))
	for _, ps := range list {
		if _, ok := desired[propagateKey(ps.ControlPlaneNamespace, ps.ControlPlaneName)]; ok {
			out = append(out, ps)
		}
	}
	return out
}

// currentTokenExpiry reads the token expiry from the MCP Secret annotation.
func currentTokenExpiry(ctx context.Context, mcpClient client.Client, gr *corev1alpha1.GitRepository, fluxNamespace string) *metav1.Time {
	secret := &corev1.Secret{}
	if err := mcpClient.Get(ctx, types.NamespacedName{
		Name:      propagate.SecretName(gr),
		Namespace: fluxNamespace,
	}, secret); err != nil {
		return nil
	}
	raw, ok := secret.Annotations[propagate.TokenExpiresAtAnnotation]
	if !ok {
		return nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	mt := metav1.NewTime(t)
	return &mt
}
