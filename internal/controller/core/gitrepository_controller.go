// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
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

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
	"github.com/openmcp-project/platform-service-gitops/internal/credentials"
	"github.com/openmcp-project/platform-service-gitops/internal/githubapp"
)

const (
	condCredentialResolved = "CredentialResolved"
	condReady              = "Ready"

	reasonCredentialFound    = "CredentialResolved"
	reasonCredentialNotFound = "CredentialNotFound"
	reasonAppNotInstalled    = "AppNotInstalled"
	reasonUnsupportedKind    = "UnsupportedCredentialKind"
	reasonAccessDenied       = "AccessDenied"

	kindAppInstallation = "AppInstallation"

	// appInstalledCondition is the condition on AppInstallation that must be True.
	appInstalledCondition = "AppInstalled"

	// requeueInterval re-checks resolution periodically, since it depends on the
	// referenced AppInstallation's state which can change out of band.
	requeueInterval = 10 * time.Minute
)

// GitRepositoryReconciler resolves a GitRepository's credentialRef to an
// AppInstallation, mints a scoped installation token to prove access, and sets
// the CredentialResolved/Ready conditions. The token is not persisted here;
// pushing a scoped token to target MCPs is handled by the propagateTo flow.
//
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=appinstallations,verbs=get;list;watch
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=githubinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
type GitRepositoryReconciler struct {
	client              client.Client
	credentialNamespace string
	newClient           func(githubapp.Credentials) (TokenMinter, error)
}

// TokenMinter is the subset of githubapp.Client used here, for test injection.
type TokenMinter interface {
	MintInstallationToken(ctx context.Context, installationID int64) (string, error)
}

// NewGitRepositoryReconciler creates a reconciler with the given client and
// default credential namespace.
func NewGitRepositoryReconciler(c client.Client, credentialNamespace string) *GitRepositoryReconciler {
	return &GitRepositoryReconciler{
		client:              c,
		credentialNamespace: credentialNamespace,
		newClient: func(creds githubapp.Credentials) (TokenMinter, error) {
			return githubapp.NewClient(creds)
		},
	}
}

// SetTokenMinterFactory overrides the token-minter factory. Intended for tests.
func (r *GitRepositoryReconciler) SetTokenMinterFactory(f func(githubapp.Credentials) (TokenMinter, error)) {
	r.newClient = f
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
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// resolveCredential resolves the credentialRef to an AppInstallation, verifies
// the App is installed, and mints a scoped token to prove access. It sets the
// CredentialResolved condition and returns whether resolution succeeded plus the
// reason (used for the Ready condition).
func (r *GitRepositoryReconciler) resolveCredential(ctx context.Context, gr *corev1alpha1.GitRepository) (bool, string) {
	ref := gr.Spec.CredentialRef

	if ref.Kind != kindAppInstallation {
		r.setResolved(gr, metav1.ConditionFalse, reasonUnsupportedKind,
			fmt.Sprintf("credentialRef.kind %q is not supported; only %q is.", ref.Kind, kindAppInstallation))
		return false, reasonUnsupportedKind
	}

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

	// The AppInstallation must report the App as installed and expose an ID.
	if !meta.IsStatusConditionTrue(ai.Status.Conditions, appInstalledCondition) || ai.Status.InstallationID == 0 {
		r.setResolved(gr, metav1.ConditionFalse, reasonAppNotInstalled,
			fmt.Sprintf("AppInstallation %s is not ready (App not installed yet).", ref.Name))
		return false, reasonAppNotInstalled
	}

	// Resolve credentials from the AppInstallation's GitHubInstance + secret.
	creds, err := credentials.Resolve(ctx, r.client, ai.Spec.InstanceRef.Name, ai.Spec.CredentialName, r.credentialNamespace)
	if err != nil {
		r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
			fmt.Sprintf("Resolving credentials for AppInstallation %s failed: %v.", ref.Name, err))
		return false, reasonCredentialNotFound
	}

	// Mint a scoped installation token to prove access. The token is not stored
	// or used here; propagation to MCPs is a separate flow.
	gh, err := r.newClient(creds)
	if err != nil {
		r.setResolved(gr, metav1.ConditionFalse, reasonAccessDenied,
			fmt.Sprintf("Building GitHub client failed: %v.", err))
		return false, reasonAccessDenied
	}
	if _, err := gh.MintInstallationToken(ctx, ai.Status.InstallationID); err != nil {
		r.setResolved(gr, metav1.ConditionFalse, reasonAccessDenied,
			fmt.Sprintf("Minting installation token failed: %v.", err))
		return false, reasonAccessDenied
	}

	r.setResolved(gr, metav1.ConditionTrue, reasonCredentialFound,
		fmt.Sprintf("Resolved via AppInstallation %s (installation %d); scoped token minted.", ref.Name, ai.Status.InstallationID))
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
