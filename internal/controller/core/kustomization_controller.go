// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"fmt"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
)

const (
	condSourceInvalid           = "SourceInvalid"
	reasonGitRepositoryNotFound = "GitRepositoryNotFound"
	reasonGitRepositoryFound    = "GitRepositoryFound"
	reasonInvalidSourceKind     = "InvalidSourceKind"
	reasonFluxKsCreated         = "FluxKustomizationCreated"
)

// KustomizationReconciler reconciles Kustomization objects.
//
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=kustomizations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=kustomizations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch;create;update;patch;delete
type KustomizationReconciler struct {
	client client.Client
}

// NewKustomizationReconciler creates a reconciler with the given client.
func NewKustomizationReconciler(c client.Client) *KustomizationReconciler {
	return &KustomizationReconciler{client: c}
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *KustomizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Kustomization{}).
		Owns(&kustomizev1.Kustomization{}).
		Complete(r)
}

func (r *KustomizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ks := &corev1alpha1.Kustomization{}
	if err := r.client.Get(ctx, req.NamespacedName, ks); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching Kustomization: %w", err)
	}

	patch := client.MergeFrom(ks.DeepCopy())

	if ks.Spec.SourceRef.Kind != "GitRepository" {
		setCondition(&ks.Status.Conditions, metav1.Condition{
			Type:               condSourceInvalid,
			Status:             metav1.ConditionTrue,
			Reason:             reasonInvalidSourceKind,
			Message:            fmt.Sprintf("sourceRef.kind %q is not supported; only GitRepository is allowed.", ks.Spec.SourceRef.Kind),
			ObservedGeneration: ks.Generation,
		})
		setCondition(&ks.Status.Conditions, metav1.Condition{
			Type:               condReady,
			Status:             metav1.ConditionFalse,
			Reason:             reasonInvalidSourceKind,
			Message:            "Invalid sourceRef kind.",
			ObservedGeneration: ks.Generation,
		})
		ks.Status.ObservedGeneration = ks.Generation
		if err := r.client.Status().Patch(ctx, ks, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	gr := &corev1alpha1.GitRepository{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: ks.Spec.SourceRef.Name, Namespace: ks.Namespace}, gr); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("fetching GitRepository: %w", err)
		}
		setCondition(&ks.Status.Conditions, metav1.Condition{
			Type:               condSourceInvalid,
			Status:             metav1.ConditionTrue,
			Reason:             reasonGitRepositoryNotFound,
			Message:            fmt.Sprintf("GitRepository %q not found in namespace %q.", ks.Spec.SourceRef.Name, ks.Namespace),
			ObservedGeneration: ks.Generation,
		})
		setCondition(&ks.Status.Conditions, metav1.Condition{
			Type:               condReady,
			Status:             metav1.ConditionFalse,
			Reason:             reasonGitRepositoryNotFound,
			Message:            "Waiting for referenced GitRepository to exist.",
			ObservedGeneration: ks.Generation,
		})
		ks.Status.ObservedGeneration = ks.Generation
		if err := r.client.Status().Patch(ctx, ks, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	fluxKs := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ks.Name,
			Namespace: ks.Namespace,
		},
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.client, fluxKs, func() error {
		if err := controllerutil.SetControllerReference(ks, fluxKs, r.client.Scheme()); err != nil {
			return fmt.Errorf("setting owner reference: %w", err)
		}
		fluxKs.Spec = kustomizev1.KustomizationSpec{
			Interval: ks.Spec.Interval,
			Path:     ks.Spec.Path,
			Prune:    ks.Spec.Prune,
			SourceRef: kustomizev1.CrossNamespaceSourceReference{
				Kind:      "GitRepository",
				Name:      ks.Spec.SourceRef.Name,
				Namespace: ks.Namespace,
			},
		}
		return nil
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling Flux Kustomization: %w", err)
	}

	// Mirror Flux Kustomization status back to our status.
	ks.Status.LastAppliedRevision = fluxKs.Status.LastAppliedRevision

	readyStatus := metav1.ConditionTrue
	readyReason := reasonFluxKsCreated
	readyMsg := "Flux Kustomization created and managed."
	for _, c := range fluxKs.Status.Conditions {
		if c.Type == condReady {
			readyStatus = metav1.ConditionStatus(c.Status)
			readyReason = c.Reason
			readyMsg = c.Message
			break
		}
	}

	setCondition(&ks.Status.Conditions, metav1.Condition{
		Type:               condSourceInvalid,
		Status:             metav1.ConditionFalse,
		Reason:             reasonGitRepositoryFound,
		Message:            fmt.Sprintf("GitRepository %q found.", ks.Spec.SourceRef.Name),
		ObservedGeneration: ks.Generation,
	})
	setCondition(&ks.Status.Conditions, metav1.Condition{
		Type:               condReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMsg,
		ObservedGeneration: ks.Generation,
	})
	ks.Status.ObservedGeneration = ks.Generation

	if err := r.client.Status().Patch(ctx, ks, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
	}

	logger.Info("reconciled Kustomization", "name", req.Name, "fluxKustomization", fluxKs.Name)
	return ctrl.Result{}, nil
}
