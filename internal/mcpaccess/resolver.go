// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

// Package mcpaccess resolves PropagateTarget entries from a GitRepository spec
// into concrete MCP cluster clients using the openmcp clusteraccess library.
//
// The access flow mirrors service-provider-crossplane's setupClusterAccess:
//  1. EnsureAccessRequest — create/update the AccessRequest on the platform cluster (non-blocking).
//  2. If still pending → caller returns RequeueAfter so the reconcile loop retries.
//  3. If granted → build a *clusters.Cluster from the AccessRequest's kubeconfig Secret.
package mcpaccess

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	clustersv1alpha1 "github.com/openmcp-project/openmcp-operator/api/clusters/v1alpha1"
	commonapi "github.com/openmcp-project/openmcp-operator/api/common"
	corev2alpha1 "github.com/openmcp-project/openmcp-operator/api/core/v2alpha1"
	"github.com/openmcp-project/openmcp-operator/lib/clusteraccess"
	libutils "github.com/openmcp-project/openmcp-operator/lib/utils"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	"github.com/openmcp-project/platform-service-gitops/internal/scheme"
)

const (
	controllerName = "gitops.open-control-plane.io"

	// PendingRequeue is the interval used when an AccessRequest exists but has
	// not been granted yet — mirrors crossplane's 30-second backoff.
	PendingRequeue = 30 * time.Second
)

// ResolvedTarget is the result of resolving one PropagateTarget entry.
type ResolvedTarget struct {
	// ControlPlaneName is the name of the ControlPlane on the onboarding cluster.
	ControlPlaneName string
	// Cluster is the MCP cluster — nil if access is still pending or failed.
	Cluster *clusters.Cluster
	// Pending is true when the AccessRequest exists but has not been granted yet.
	Pending bool
}

// Resolver resolves PropagateTarget entries to MCP cluster clients.
// The interface allows substituting a fake in tests.
type Resolver interface {
	// Resolve returns one ResolvedTarget per concrete ControlPlane derived from
	// the GitRepository's propagateTo list. Non-blocking: targets whose
	// AccessRequest is still pending are returned with Cluster==nil, Pending==true.
	Resolve(ctx context.Context, gitRepo *corev1alpha1.GitRepository, onboardingClient client.Client) ([]ResolvedTarget, error)

	// Cleanup deletes the AccessRequest on the platform cluster for a single
	// (GitRepository, ControlPlane) pair — called when a target is removed from
	// propagateTo or when the GitRepository itself is deleted.
	Cleanup(ctx context.Context, gitRepo *corev1alpha1.GitRepository, controlPlaneName string) error
}

type resolver struct {
	platformClient      client.Client
	controllerNamespace string
}

// NewResolver creates a Resolver.
// platformClient must be a client for the platform cluster where AccessRequests live.
// controllerNamespace is the namespace where AccessRequests are created.
func NewResolver(platformClient client.Client, controllerNamespace string) Resolver {
	return &resolver{
		platformClient:      platformClient,
		controllerNamespace: controllerNamespace,
	}
}

func (r *resolver) Resolve(ctx context.Context, gitRepo *corev1alpha1.GitRepository, onboardingClient client.Client) ([]ResolvedTarget, error) {
	names, err := resolveNames(ctx, gitRepo, onboardingClient)
	if err != nil {
		return nil, err
	}

	results := make([]ResolvedTarget, 0, len(names))
	for _, name := range names {
		rt, err := r.resolveOne(ctx, gitRepo, name)
		if err != nil {
			return nil, fmt.Errorf("resolving MCP %q: %w", name, err)
		}
		results = append(results, rt)
	}
	return results, nil
}

// resolveOne mirrors crossplane's setupClusterAccess:
//  1. Ensure the AccessRequest exists (create/update, non-blocking).
//  2. Check status — pending → Pending:true, denied → error, granted → build cluster.
func (r *resolver) resolveOne(ctx context.Context, gitRepo *corev1alpha1.GitRepository, controlPlaneName string) (ResolvedTarget, error) {
	result := ResolvedTarget{ControlPlaneName: controlPlaneName}

	mcpNamespace, err := libutils.StableMCPNamespace(controlPlaneName, gitRepo.Namespace)
	if err != nil {
		return result, fmt.Errorf("computing MCP namespace for %q: %w", controlPlaneName, err)
	}

	arName := clusteraccess.StableRequestNameFromLocalName(controllerName, AccessLocalName(gitRepo, controlPlaneName))
	ar := &clustersv1alpha1.AccessRequest{}

	// Step 1: Ensure the AccessRequest exists on the platform cluster.
	if err := r.ensureAccessRequest(ctx, arName, controlPlaneName, mcpNamespace); err != nil {
		return result, fmt.Errorf("ensuring AccessRequest for %q: %w", controlPlaneName, err)
	}

	// Step 2: Read current AccessRequest status (non-blocking).
	if err := r.platformClient.Get(ctx, client.ObjectKey{Name: arName, Namespace: r.controllerNamespace}, ar); err != nil {
		return result, fmt.Errorf("reading AccessRequest for %q: %w", controlPlaneName, err)
	}

	if ar.Status.IsPending() {
		result.Pending = true
		return result, nil
	}
	if ar.Status.IsDenied() {
		return result, fmt.Errorf("AccessRequest for %q was denied", controlPlaneName)
	}

	// Step 3: AccessRequest is granted — build the MCP cluster client from the kubeconfig Secret.
	cl, err := r.clusterFromAccessRequest(ctx, ar, controlPlaneName)
	if err != nil {
		return result, fmt.Errorf("building MCP client for %q: %w", controlPlaneName, err)
	}
	result.Cluster = cl
	return result, nil
}

// ensureAccessRequest creates or updates the AccessRequest on the platform cluster.
func (r *resolver) ensureAccessRequest(ctx context.Context, arName, controlPlaneName, mcpNamespace string) error {
	ar := &clustersv1alpha1.AccessRequest{}
	err := r.platformClient.Get(ctx, client.ObjectKey{Name: arName, Namespace: r.controllerNamespace}, ar)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting AccessRequest: %w", err)
	}

	desired := &clustersv1alpha1.AccessRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      arName,
			Namespace: r.controllerNamespace,
		},
		Spec: clustersv1alpha1.AccessRequestSpec{
			// RequestRef points to the ClusterRequest that the openmcp operator
			// creates for each MCP — the same pattern used by service-provider-crossplane.
			RequestRef: &commonapi.ObjectReference{
				Name:      controlPlaneName,
				Namespace: mcpNamespace,
			},
			Token: &clustersv1alpha1.TokenConfig{
				Permissions: mcpPermissions(),
			},
		},
	}

	if apierrors.IsNotFound(err) {
		return r.platformClient.Create(ctx, desired)
	}

	// Already exists — update spec in case permissions changed.
	ar.Spec = desired.Spec
	return r.platformClient.Update(ctx, ar)
}

// clusterFromAccessRequest builds a *clusters.Cluster from the kubeconfig Secret
// referenced by the granted AccessRequest — mirrors crossplane's createClusterForAccessRequest.
func (r *resolver) clusterFromAccessRequest(ctx context.Context, ar *clustersv1alpha1.AccessRequest, clusterName string) (*clusters.Cluster, error) {
	if ar.Status.SecretRef == nil {
		return nil, fmt.Errorf("AccessRequest %q has no SecretRef", ar.Name)
	}

	secret := &corev1.Secret{}
	if err := r.platformClient.Get(ctx, client.ObjectKey{
		Name:      ar.Status.SecretRef.Name,
		Namespace: ar.Namespace,
	}, secret); err != nil {
		return nil, fmt.Errorf("reading kubeconfig Secret: %w", err)
	}

	kubeconfigBytes, ok := secret.Data["kubeconfig"]
	if !ok {
		return nil, fmt.Errorf("kubeconfig key not found in Secret %s/%s", ar.Namespace, ar.Status.SecretRef.Name)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}

	cl := clusters.New(clusterName).WithRESTConfig(restConfig)
	if err := cl.InitializeClient(scheme.MCP); err != nil {
		return nil, fmt.Errorf("initializing MCP client for %q: %w", clusterName, err)
	}
	return cl, nil
}

func (r *resolver) Cleanup(ctx context.Context, gitRepo *corev1alpha1.GitRepository, controlPlaneName string) error {
	arName := clusteraccess.StableRequestNameFromLocalName(controllerName, AccessLocalName(gitRepo, controlPlaneName))
	ar := &clustersv1alpha1.AccessRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      arName,
			Namespace: r.controllerNamespace,
		},
	}
	if err := r.platformClient.Delete(ctx, ar); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting AccessRequest %s/%s: %w", r.controllerNamespace, arName, err)
	}
	return nil
}

// resolveNames expands all PropagateTarget entries into concrete ControlPlane names,
// deduplicating across entries.
func resolveNames(ctx context.Context, gitRepo *corev1alpha1.GitRepository, onboardingClient client.Client) ([]string, error) {
	seen := map[string]struct{}{}
	var names []string

	for _, target := range gitRepo.Spec.PropagateToControlPlanes {
		if target.Kind != corev1alpha1.PropagateTargetKindControlPlane {
			continue
		}

		if target.Name != "" {
			if _, ok := seen[target.Name]; !ok {
				seen[target.Name] = struct{}{}
				names = append(names, target.Name)
			}
			continue
		}

		list := &corev2alpha1.ControlPlaneList{}
		if err := onboardingClient.List(ctx, list,
			client.InNamespace(gitRepo.Namespace),
			client.MatchingLabels(target.MatchLabels),
		); err != nil {
			return nil, fmt.Errorf("listing ControlPlanes for matchLabels: %w", err)
		}
		for i := range list.Items {
			n := list.Items[i].Name
			if _, ok := seen[n]; !ok {
				seen[n] = struct{}{}
				names = append(names, n)
			}
		}
	}
	return names, nil
}

// AccessLocalName returns the stable local name used to derive the AccessRequest
// name for a given (GitRepository, ControlPlane) pair.
func AccessLocalName(gitRepo *corev1alpha1.GitRepository, controlPlaneName string) string {
	return fmt.Sprintf("%s--%s--%s", gitRepo.Namespace, gitRepo.Name, controlPlaneName)
}

// mcpPermissions returns the RBAC permissions the controller needs in each MCP.
func mcpPermissions() []clustersv1alpha1.PermissionsRequest {
	return []clustersv1alpha1.PermissionsRequest{
		{
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups: []string{corev1.SchemeGroupVersion.Group},
					Resources: []string{"secrets"},
					Verbs:     []string{"get", "list", "create", "update", "patch", "delete"},
				},
				{
					APIGroups: []string{"source.toolkit.fluxcd.io"},
					Resources: []string{"gitrepositories"},
					Verbs:     []string{"get", "list", "create", "update", "patch", "delete"},
				},
			},
		},
	}
}
