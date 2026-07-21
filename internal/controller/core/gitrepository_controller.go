// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
)

const (
	condCredentialResolved   = "CredentialResolved"
	condReady                = "Ready"
	reasonCredentialNotFound = "CredentialNotFound"
	reasonCredentialFound    = "AppInstallationFound"
	reasonReconciling        = "Reconciling"
	reasonURLReachable       = "URLReachable"
)

// GitRepositoryReconciler reconciles GitRepository objects.
//
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories/status,verbs=get;update;patch
type GitRepositoryReconciler struct {
	client client.Client
}

// NewGitRepositoryReconciler creates a reconciler with the given client.
func NewGitRepositoryReconciler(c client.Client) *GitRepositoryReconciler {
	return &GitRepositoryReconciler{client: c}
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *GitRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.GitRepository{}).
		Complete(r)
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

	// Credential resolution is a placeholder until AppInstallation type lands (#538).
	// Always returns false so CredentialResolved=False is set, giving users clear status feedback.
	credResolved := false

	if credResolved {
		setCondition(&gr.Status.Conditions, metav1.Condition{
			Type:               condCredentialResolved,
			Status:             metav1.ConditionTrue,
			Reason:             reasonCredentialFound,
			Message:            fmt.Sprintf("%s %s resolved successfully.", gr.Spec.CredentialRef.Kind, gr.Spec.CredentialRef.Name),
			ObservedGeneration: gr.Generation,
		})
		setCondition(&gr.Status.Conditions, metav1.Condition{
			Type:               condReady,
			Status:             metav1.ConditionTrue,
			Reason:             reasonURLReachable,
			Message:            "Repository is reachable and credentials are valid.",
			ObservedGeneration: gr.Generation,
		})
	} else {
		setCondition(&gr.Status.Conditions, metav1.Condition{
			Type:               condCredentialResolved,
			Status:             metav1.ConditionFalse,
			Reason:             reasonCredentialNotFound,
			Message:            fmt.Sprintf("%s %s not found in namespace %s.", gr.Spec.CredentialRef.Kind, gr.Spec.CredentialRef.Name, gr.Namespace),
			ObservedGeneration: gr.Generation,
		})
		setCondition(&gr.Status.Conditions, metav1.Condition{
			Type:               condReady,
			Status:             metav1.ConditionFalse,
			Reason:             reasonReconciling,
			Message:            "Waiting for credential to be resolved.",
			ObservedGeneration: gr.Generation,
		})
	}

	gr.Status.ObservedGeneration = gr.Generation

	if err := r.client.Status().Patch(ctx, gr, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
	}

	logger.Info("reconciled GitRepository", "name", req.Name, "credentialResolved", credResolved)
	return ctrl.Result{}, nil
}

func setCondition(conditions *[]metav1.Condition, c metav1.Condition) {
	if c.LastTransitionTime.IsZero() {
		c.LastTransitionTime = metav1.Now()
	}
	meta.SetStatusCondition(conditions, c)
}
