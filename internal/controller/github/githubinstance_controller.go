// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
	"github.com/openmcp-project/platform-service-gitops/internal/controllerconst"
	"github.com/openmcp-project/platform-service-gitops/internal/credentials"
)

const (
	condReady             = "Ready"
	reasonSecretsValid    = "SecretsValid"
	reasonSecretNotFound  = "SecretNotFound"
	reasonSecretInvalid   = "SecretMalformed"
	reasonSecretReadError = "SecretReadError"
)

// GitHubInstanceReconciler validates that a GitHubInstance's credential Secrets
// exist and are well-formed, reporting the result via the Ready condition.
//
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=githubinstances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=githubinstances/status,verbs=get;update;patch
type GitHubInstanceReconciler struct {
	client              client.Client
	platformCluster     cluster.Cluster
	credentialNamespace string
}

// NewGitHubInstanceReconciler creates a reconciler. platformCluster is the
// controller-runtime Cluster for the platform cluster where GitHubInstance
// resources live; it is used to set up the watch.
func NewGitHubInstanceReconciler(c client.Client, platformCluster cluster.Cluster, credentialNamespace string) *GitHubInstanceReconciler {
	return &GitHubInstanceReconciler{client: c, platformCluster: platformCluster, credentialNamespace: credentialNamespace}
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
// GitHubInstance lives on the platform cluster, so we watch it via source.Kind
// backed by the platform cluster cache rather than the onboarding manager cache.
func (r *GitHubInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WatchesRawSource(source.Kind(
			r.platformCluster.GetCache(),
			&githubv1alpha1.GitHubInstance{},
			&handler.TypedEnqueueRequestForObject[*githubv1alpha1.GitHubInstance]{},
		)).
		Named("github-githubinstance").
		Complete(r)
}

func (r *GitHubInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	inst := &githubv1alpha1.GitHubInstance{}
	if err := r.client.Get(ctx, req.NamespacedName, inst); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching GitHubInstance: %w", err)
	}

	patch := client.MergeFrom(inst.DeepCopy())

	status, reason, msg := metav1.ConditionTrue, reasonSecretsValid, "Credential secret is present and well-formed."
	ref := inst.Spec.SecretRef
	ns := ref.Namespace
	if ns == "" {
		ns = r.credentialNamespace
	}
	secret := &corev1.Secret{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			status, reason, msg = metav1.ConditionFalse, reasonSecretNotFound, fmt.Sprintf("Credential Secret %s/%s not found.", ns, ref.Name)
		} else {
			status, reason, msg = metav1.ConditionUnknown, reasonSecretReadError, fmt.Sprintf("Reading Secret %s/%s failed: %v.", ns, ref.Name, err)
		}
	} else if _, err := credentials.FromSecret(secret); err != nil {
		status, reason, msg = metav1.ConditionFalse, reasonSecretInvalid, err.Error()
	}

	meta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
		Type:               condReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: inst.Generation,
	})
	inst.Status.ObservedGeneration = inst.Generation

	if err := r.client.Status().Patch(ctx, inst, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
	}

	logger.Info("Reconciled GitHubInstance", "name", req.Name, "ready", status == metav1.ConditionTrue)
	return ctrl.Result{RequeueAfter: controllerconst.RequeueInterval}, nil
}
