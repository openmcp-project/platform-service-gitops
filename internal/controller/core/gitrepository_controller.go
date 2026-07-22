// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
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

	kindAppInstallation = "AppInstallation"

	appInstalledCondition = "AppInstalled"

	finalizerPropagate = "gitops.open-control-plane.io/propagate"
)

// GitRepositoryReconciler resolves a GitRepository's credentialRef, syncs scoped
// tokens and Flux GitRepository resources into each MCP listed in propagateTo,
// and keeps per-MCP status up to date.
//
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=appinstallations,verbs=get;list;watch
type GitRepositoryReconciler struct {
	// onboardingClient reads GitRepository and AppInstallation from the onboarding cluster.
	onboardingClient client.Client
	// platformClient reads GitHubInstance and credential Secrets from the platform cluster.
	platformClient client.Client

	mcpResolver         mcpaccess.Resolver
	credentialNamespace string
	fluxNamespace       string
	tokenRenewBuffer    time.Duration
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
	}
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *GitRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.GitRepository{}).
		Watches(&githubv1alpha1.AppInstallation{},
			handler.EnqueueRequestsFromMapFunc(r.mapAppInstallationToGitRepositories)).
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
	installationID, resolved, reason := r.resolveCredential(ctx, gr)

	if !resolved {
		setCondition(&gr.Status.Conditions, metav1.Condition{
			Type:               condReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            "Credentials are not resolved; see the CredentialResolved condition.",
			ObservedGeneration: gr.Generation,
		})
		gr.Status.ObservedGeneration = gr.Generation
		if err := r.onboardingClient.Status().Patch(ctx, gr, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
		}
		return ctrl.Result{RequeueAfter: controllerconst.RequeueInterval}, nil
	}

	// Propagate to MCPs.
	requeueAfter := r.reconcilePropagate(ctx, gr, installationID)

	setCondition(&gr.Status.Conditions, metav1.Condition{
		Type:               condReady,
		Status:             metav1.ConditionTrue,
		Reason:             reasonCredentialFound,
		Message:            "Credentials resolved; propagation in progress.",
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
func (r *GitRepositoryReconciler) reconcilePropagate(ctx context.Context, gr *corev1alpha1.GitRepository, installationID int64) time.Duration {
	logger := log.FromContext(ctx)

	targets, err := r.mcpResolver.Resolve(ctx, gr, r.onboardingClient)
	if err != nil {
		logger.Error(err, "failed to resolve propagateTo targets")
		return controllerconst.RequeueInterval
	}

	// Build creds + minter once for all targets (same AppInstallation for all).
	creds, err := r.resolveGitHubCreds(ctx, gr)
	if err != nil {
		logger.Error(err, "failed to resolve GitHub credentials for propagation")
		return controllerconst.RequeueInterval
	}
	ghClient, err := githubapp.NewClient(creds)
	if err != nil {
		logger.Error(err, "failed to build GitHub App client")
		return controllerconst.RequeueInterval
	}

	// Track the desired set of ControlPlane names for status cleanup.
	desiredNames := map[string]struct{}{}
	for _, t := range targets {
		desiredNames[t.ControlPlaneName] = struct{}{}
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
	ps := corev1alpha1.PropagateStatus{ControlPlaneName: target.ControlPlaneName}

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

	for _, target := range targets {
		// Always clean up the AccessRequest, regardless of whether cluster access was granted.
		if err := r.mcpResolver.Cleanup(ctx, gr, target.ControlPlaneName); err != nil {
			logger.Error(err, "AccessRequest cleanup failed", "controlPlane", target.ControlPlaneName)
		}
		if target.Cluster == nil {
			continue
		}
		if err := propagate.Cleanup(ctx, target.Cluster.Client(), gr, r.fluxNamespace); err != nil {
			logger.Error(err, "MCP resource cleanup failed", "controlPlane", target.ControlPlaneName)
		}
	}

	controllerutil.RemoveFinalizer(gr, finalizerPropagate)
	if err := r.onboardingClient.Update(ctx, gr); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// resolveCredential resolves the credentialRef to an AppInstallation and returns
// the installation ID, whether resolution succeeded, and a reason code.
func (r *GitRepositoryReconciler) resolveCredential(ctx context.Context, gr *corev1alpha1.GitRepository) (int64, bool, string) {
	ref := gr.Spec.CredentialRef

	if ref.Kind != kindAppInstallation {
		r.setResolved(gr, metav1.ConditionFalse, reasonUnsupportedKind,
			fmt.Sprintf("credentialRef.kind %q is not supported; only %q is.", ref.Kind, kindAppInstallation))
		return 0, false, reasonUnsupportedKind
	}

	ai := &githubv1alpha1.AppInstallation{}
	if err := r.onboardingClient.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: gr.Namespace}, ai); err != nil {
		if apierrors.IsNotFound(err) {
			r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
				fmt.Sprintf("AppInstallation %s not found in namespace %s.", ref.Name, gr.Namespace))
			return 0, false, reasonCredentialNotFound
		}
		r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
			fmt.Sprintf("Fetching AppInstallation %s failed: %v.", ref.Name, err))
		return 0, false, reasonCredentialNotFound
	}

	if !meta.IsStatusConditionTrue(ai.Status.Conditions, appInstalledCondition) || ai.Status.InstallationID == 0 {
		r.setResolved(gr, metav1.ConditionFalse, reasonAppNotInstalled,
			fmt.Sprintf("AppInstallation %s is not ready (App not installed yet).", ref.Name))
		return 0, false, reasonAppNotInstalled
	}

	r.setResolved(gr, metav1.ConditionTrue, reasonCredentialFound,
		fmt.Sprintf("Resolved via AppInstallation %s (installation %d).", ref.Name, ai.Status.InstallationID))
	return ai.Status.InstallationID, true, reasonCredentialFound
}

// resolveGitHubCreds resolves the full GitHub App credentials for token minting.
func (r *GitRepositoryReconciler) resolveGitHubCreds(ctx context.Context, gr *corev1alpha1.GitRepository) (githubapp.Credentials, error) {
	ref := gr.Spec.CredentialRef
	ai := &githubv1alpha1.AppInstallation{}
	if err := r.onboardingClient.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: gr.Namespace}, ai); err != nil {
		return githubapp.Credentials{}, fmt.Errorf("fetching AppInstallation: %w", err)
	}
	return credentials.Resolve(ctx, r.platformClient, ai.Spec.InstanceRef.Name, "", r.credentialNamespace)
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

// setPropagateStatus upserts a PropagateStatus entry by ControlPlaneName.
func setPropagateStatus(list *[]corev1alpha1.PropagateStatus, ps corev1alpha1.PropagateStatus) {
	for i := range *list {
		if (*list)[i].ControlPlaneName == ps.ControlPlaneName {
			(*list)[i] = ps
			return
		}
	}
	*list = append(*list, ps)
}

// filterPropagateStatus removes entries whose ControlPlaneName is not in desired.
func filterPropagateStatus(list []corev1alpha1.PropagateStatus, desired map[string]struct{}) []corev1alpha1.PropagateStatus {
	out := make([]corev1alpha1.PropagateStatus, 0, len(list))
	for _, ps := range list {
		if _, ok := desired[ps.ControlPlaneName]; ok {
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
