// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
	"github.com/openmcp-project/platform-service-gitops/internal/credentials"
	"github.com/openmcp-project/platform-service-gitops/internal/githubapp"
)

const (
	condAppInstalled    = "AppInstalled"
	condAccessVerified  = "AccessVerified"
	reasonAppFound      = "AppFound"
	reasonAppNotFound   = "AppNotInstalled"
	reasonAccessOK      = "AccessVerified"
	reasonAccessDenied  = "AccessDenied"
	reasonInvalidSpec   = "InvalidSpec"
	reasonInstanceError = "InstanceResolutionFailed"

	// requeueInterval re-checks installation state, which can change out of band.
	requeueInterval = 10 * time.Minute
)

// AppInstallationReconciler reconciles AppInstallation objects. It resolves the
// referenced GitHubInstance and its credential Secret, authenticates as the
// GitHub App, and reports whether the App is installed on the target org/user.
//
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=appinstallations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=appinstallations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=githubinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
type AppInstallationReconciler struct {
	client client.Client
	// credentialNamespace is the default namespace for credential Secrets whose
	// SecretReference does not specify one.
	credentialNamespace string
	// newClient builds a GitHub App client; overridable in tests.
	newClient func(githubapp.Credentials) (GitHubClient, error)
}

// GitHubClient is the subset of githubapp.Client used here, for test injection.
type GitHubClient interface {
	VerifyApp(ctx context.Context) (string, error)
	CheckOrgInstallation(ctx context.Context, org string) (githubapp.InstallationResult, error)
	CheckUserInstallation(ctx context.Context, user string) (githubapp.InstallationResult, error)
}

// GitHubClientFactory builds a GitHubClient from credentials. Exposed so tests
// can inject a fake instead of talking to a real GitHub instance.
type GitHubClientFactory func(githubapp.Credentials) (GitHubClient, error)

// NewAppInstallationReconciler creates a reconciler with the given client and
// default credential namespace.
func NewAppInstallationReconciler(c client.Client, credentialNamespace string) *AppInstallationReconciler {
	return &AppInstallationReconciler{
		client:              c,
		credentialNamespace: credentialNamespace,
		newClient: func(creds githubapp.Credentials) (GitHubClient, error) {
			return githubapp.NewClient(creds)
		},
	}
}

// SetGitHubClientFactory overrides the GitHub client factory. Intended for tests.
func (r *AppInstallationReconciler) SetGitHubClientFactory(f GitHubClientFactory) {
	r.newClient = f
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *AppInstallationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&githubv1alpha1.AppInstallation{}).
		Named("github-appinstallation").
		Complete(r)
}

func (r *AppInstallationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ai := &githubv1alpha1.AppInstallation{}
	if err := r.client.Get(ctx, req.NamespacedName, ai); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching AppInstallation: %w", err)
	}

	patch := client.MergeFrom(ai.DeepCopy())
	result := r.reconcile(ctx, ai)

	ai.Status.ObservedGeneration = ai.Generation
	ai.Status.LastChecked = &metav1.Time{Time: metav1.Now().Time}
	if err := r.client.Status().Patch(ctx, ai, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
	}

	logger.Info("Reconciled AppInstallation", "name", req.Name, "installed", ai.Status.InstallationID != 0)
	return result, nil
}

// reconcile performs the credential resolution and installation check, setting
// conditions on ai. Expected states (bad spec, App not installed, unreachable
// instance) are reported via conditions and a requeue rather than an error, so
// the controller never error-loops.
func (r *AppInstallationReconciler) reconcile(ctx context.Context, ai *githubv1alpha1.AppInstallation) ctrl.Result {
	// Validate spec: exactly one of org/user.
	if (ai.Spec.Org == "") == (ai.Spec.User == "") {
		r.setBoth(ai, reasonInvalidSpec, "Exactly one of spec.org or spec.user must be set.")
		return ctrl.Result{}
	}

	creds, err := r.resolveCredentials(ctx, ai)
	if err != nil {
		r.setBoth(ai, reasonInstanceError, err.Error())
		return ctrl.Result{RequeueAfter: requeueInterval}
	}

	gh, err := r.newClient(creds)
	if err != nil {
		r.setBoth(ai, reasonAccessDenied, fmt.Sprintf("Building GitHub client failed: %v.", err))
		return ctrl.Result{RequeueAfter: requeueInterval}
	}

	if _, err := gh.VerifyApp(ctx); err != nil {
		r.setBoth(ai, reasonAccessDenied, fmt.Sprintf("Authenticating as GitHub App failed: %v.", err))
		return ctrl.Result{RequeueAfter: requeueInterval}
	}

	var res githubapp.InstallationResult
	var target string
	if ai.Spec.Org != "" {
		target = "org " + ai.Spec.Org
		res, err = gh.CheckOrgInstallation(ctx, ai.Spec.Org)
	} else {
		target = "user " + ai.Spec.User
		res, err = gh.CheckUserInstallation(ctx, ai.Spec.User)
	}
	if err != nil {
		// AppInstalled unknown; App auth worked, so AccessVerified stays true.
		r.set(ai, condAccessVerified, metav1.ConditionTrue, reasonAccessOK, "GitHub App authenticated successfully.")
		r.set(ai, condAppInstalled, metav1.ConditionUnknown, reasonAppNotFound, fmt.Sprintf("Checking installation for %s failed: %v.", target, err))
		return ctrl.Result{RequeueAfter: requeueInterval}
	}

	r.set(ai, condAccessVerified, metav1.ConditionTrue, reasonAccessOK, "GitHub App authenticated successfully.")

	if !res.Installed {
		ai.Status.InstallationID = 0
		r.set(ai, condAppInstalled, metav1.ConditionFalse, reasonAppNotFound,
			fmt.Sprintf("GitHub App is not installed on %s. Install the App to grant access.", target))
		return ctrl.Result{RequeueAfter: requeueInterval}
	}

	ai.Status.InstallationID = res.InstallationID
	r.set(ai, condAppInstalled, metav1.ConditionTrue, reasonAppFound,
		fmt.Sprintf("GitHub App installation found for %s (installation %d).", target, res.InstallationID))
	return ctrl.Result{RequeueAfter: requeueInterval}
}

// resolveCredentials looks up the GitHubInstance, selects the credential Secret,
// and reads the App ID, private key and URL from it.
func (r *AppInstallationReconciler) resolveCredentials(ctx context.Context, ai *githubv1alpha1.AppInstallation) (githubapp.Credentials, error) {
	return credentials.Resolve(ctx, r.client, ai.Spec.InstanceRef.Name, ai.Spec.CredentialName, r.credentialNamespace)
}

// setBoth sets both conditions to False with the same reason/message. Used for
// failures that block the whole check (bad spec, unresolvable instance, denied
// access).
func (r *AppInstallationReconciler) setBoth(ai *githubv1alpha1.AppInstallation, reason, msg string) {
	r.set(ai, condAppInstalled, metav1.ConditionFalse, reason, msg)
	r.set(ai, condAccessVerified, metav1.ConditionFalse, reason, msg)
}

func (r *AppInstallationReconciler) set(ai *githubv1alpha1.AppInstallation, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&ai.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ai.Generation,
	})
}
