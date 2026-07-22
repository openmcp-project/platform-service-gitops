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
	"sigs.k8s.io/controller-runtime/pkg/log"

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
	credentialNamespace string
}

// NewGitHubInstanceReconciler creates a reconciler with the given client and
// default credential namespace.
func NewGitHubInstanceReconciler(c client.Client, credentialNamespace string) *GitHubInstanceReconciler {
	return &GitHubInstanceReconciler{client: c, credentialNamespace: credentialNamespace}
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *GitHubInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&githubv1alpha1.GitHubInstance{}).
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

	status, reason, msg := metav1.ConditionTrue, reasonSecretsValid, fmt.Sprintf("All %d credential secrets are present and well-formed.", len(inst.Spec.SecretRefs))
	for _, ref := range inst.Spec.SecretRefs {
		ns := ref.Namespace
		if ns == "" {
			ns = r.credentialNamespace
		}
		secret := &corev1.Secret{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				status, reason, msg = metav1.ConditionFalse, reasonSecretNotFound, fmt.Sprintf("Credential Secret %s/%s not found.", ns, ref.Name)
				break
			}
			// Transient error: surface it as Unknown and requeue rather than
			// error-looping and dropping the status update.
			status, reason, msg = metav1.ConditionUnknown, reasonSecretReadError, fmt.Sprintf("Reading Secret %s/%s failed: %v.", ns, ref.Name, err)
			break
		}
		if _, err := credentials.FromSecret(secret); err != nil {
			status, reason, msg = metav1.ConditionFalse, reasonSecretInvalid, err.Error()
			break
		}
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
