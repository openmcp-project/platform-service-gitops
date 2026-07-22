// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
	"github.com/openmcp-project/platform-service-gitops/internal/controllerconst"
	"github.com/openmcp-project/platform-service-gitops/internal/credentials"
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

	kindAppInstallation = "AppInstallation"
	kindSecret          = "Secret"

	// appInstalledCondition is the condition on AppInstallation that must be True.
	appInstalledCondition = "AppInstalled"

	// validateAccessTimeout bounds the live ls-remote performed for the Secret
	// credential path so a slow or hung Git host cannot block a reconcile worker.
	validateAccessTimeout = 30 * time.Second
)

// GitRepositoryReconciler resolves a GitRepository's credentialRef to an
// AppInstallation and sets the CredentialResolved/Ready conditions based on the
// AppInstallation's verified status. It does not mint tokens; token minting
// happens only in the propagateTo flow where a token is actually used.
//
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=appinstallations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
type GitRepositoryReconciler struct {
	client client.Client
}

// NewGitRepositoryReconciler creates a reconciler with the given client.
func NewGitRepositoryReconciler(c client.Client) *GitRepositoryReconciler {
	return &GitRepositoryReconciler{client: c}
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
// It watches AppInstallations so a GitRepository re-reconciles when the
// AppInstallation it references becomes installed (or changes), and Secrets so a
// GitRepository re-reconciles when a referenced credential Secret is rotated.
func (r *GitRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.GitRepository{}).
		Watches(&githubv1alpha1.AppInstallation{}, handler.EnqueueRequestsFromMapFunc(r.mapAppInstallationToGitRepositories)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToGitRepositories)).
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

// mapSecretToGitRepositories returns reconcile requests for every GitRepository
// in the Secret's namespace that references it via a kind:Secret credentialRef.
// This makes credential rotation (updating the Secret) take effect on the next
// reconcile without restarting the resource.
func (r *GitRepositoryReconciler) mapSecretToGitRepositories(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &corev1alpha1.GitRepositoryList{}
	if err := r.client.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
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
	if err := r.client.Get(ctx, req.NamespacedName, gr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching GitRepository: %w", err)
	}

	patch := client.MergeFrom(gr.DeepCopy())
	resolved, reason := r.resolveCredential(ctx, gr)

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

	gr.Status.ObservedGeneration = gr.Generation
	if err := r.client.Status().Patch(ctx, gr, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
	}

	logger.Info("Reconciled GitRepository", "name", req.Name, "credentialResolved", resolved)
	return ctrl.Result{RequeueAfter: controllerconst.RequeueInterval}, nil
}

// resolveCredential dispatches on credentialRef.kind. It sets the
// CredentialResolved condition and returns whether resolution succeeded plus the
// reason (used for the Ready condition).
func (r *GitRepositoryReconciler) resolveCredential(ctx context.Context, gr *corev1alpha1.GitRepository) (bool, string) {
	switch gr.Spec.CredentialRef.Kind {
	case kindAppInstallation:
		return r.resolveAppInstallation(ctx, gr)
	case kindSecret:
		return r.resolveSecret(ctx, gr)
	default:
		r.setResolved(gr, metav1.ConditionFalse, reasonUnsupportedKind,
			fmt.Sprintf("credentialRef.kind %q is not supported; supported kinds are %q and %q.",
				gr.Spec.CredentialRef.Kind, kindAppInstallation, kindSecret))
		return false, reasonUnsupportedKind
	}
}

// resolveAppInstallation resolves the credentialRef to an AppInstallation and
// verifies (via its status) that the App is installed.
func (r *GitRepositoryReconciler) resolveAppInstallation(ctx context.Context, gr *corev1alpha1.GitRepository) (bool, string) {
	ref := gr.Spec.CredentialRef

	// AppInstallation is referenced by name in the same namespace as the GitRepository.
	ai := &githubv1alpha1.AppInstallation{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: gr.Namespace}, ai); err != nil {
		if apierrors.IsNotFound(err) {
			r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
				fmt.Sprintf("AppInstallation %s not found in namespace %s.", ref.Name, gr.Namespace))
			return false, reasonCredentialNotFound
		}
		r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
			fmt.Sprintf("Fetching AppInstallation %s failed: %v.", ref.Name, err))
		return false, reasonCredentialNotFound
	}

	// The AppInstallation controller already verified App installation and
	// access. Trusting its status avoids minting an installation token here:
	// tokens are rate-limited (1 per installation per hour on GHE) and would be
	// discarded, so minting on every reconcile would exhaust the quota. Token
	// minting happens only where a token is actually used (the propagateTo flow).
	if !meta.IsStatusConditionTrue(ai.Status.Conditions, appInstalledCondition) || ai.Status.InstallationID == 0 {
		r.setResolved(gr, metav1.ConditionFalse, reasonAppNotInstalled,
			fmt.Sprintf("AppInstallation %s is not ready (App not installed yet).", ref.Name))
		return false, reasonAppNotInstalled
	}

	r.setResolved(gr, metav1.ConditionTrue, reasonCredentialFound,
		fmt.Sprintf("Resolved via AppInstallation %s (installation %d).", ref.Name, ai.Status.InstallationID))
	return true, reasonCredentialFound
}

// resolveSecret resolves a user-supplied credential Secret (PAT or SSH) from the
// GitRepository's own namespace and verifies it by performing a live ls-remote
// against the repository. Unlike the AppInstallation path there is no upstream
// controller vouching for the credential, so access is validated here directly.
func (r *GitRepositoryReconciler) resolveSecret(ctx context.Context, gr *corev1alpha1.GitRepository) (bool, string) {
	ref := gr.Spec.CredentialRef

	// The Secret must live in the GitRepository's own namespace. Reading Secrets
	// from other namespaces would allow privilege escalation.
	secret := &corev1.Secret{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: gr.Namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
				fmt.Sprintf("Secret %s not found in namespace %s.", ref.Name, gr.Namespace))
			return false, reasonCredentialNotFound
		}
		r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
			fmt.Sprintf("Fetching Secret %s failed: %v.", ref.Name, err))
		return false, reasonCredentialNotFound
	}

	cred, err := credentials.ResolveSecret(secret)
	if err != nil {
		// Error messages here describe the Secret shape, never its contents.
		r.setResolved(gr, metav1.ConditionFalse, reasonUnsupportedSecret,
			fmt.Sprintf("Secret %s: %v.", ref.Name, err))
		return false, reasonUnsupportedSecret
	}

	// Bound the live ls-remote so a slow or hung host cannot block the worker.
	validateCtx, cancel := context.WithTimeout(ctx, validateAccessTimeout)
	defer cancel()
	if err := credentials.ValidateAccess(validateCtx, gr.Spec.URL, cred); err != nil {
		// credentials errors are classified and carry no secret material.
		reason := reasonRepoUnreachable
		if errors.Is(err, credentials.ErrAuthFailed) {
			reason = reasonAuthFailed
		}
		r.setResolved(gr, metav1.ConditionFalse, reason,
			fmt.Sprintf("Secret %s: %v.", ref.Name, err))
		return false, reason
	}

	r.setResolved(gr, metav1.ConditionTrue, reasonCredentialFound,
		fmt.Sprintf("Resolved via Secret %s; repository access verified.", ref.Name))
	return true, reasonCredentialFound
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
