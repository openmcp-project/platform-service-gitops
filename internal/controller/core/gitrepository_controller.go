// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	clustersv1alpha1 "github.com/openmcp-project/openmcp-operator/api/clusters/v1alpha1"
	commonapi "github.com/openmcp-project/openmcp-operator/api/common"
	"github.com/openmcp-project/openmcp-operator/lib/clusteraccess/advanced"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	githubv1alpha1 "github.com/openmcp-project/platform-service-gitops/api/github/v1alpha1"
	"github.com/openmcp-project/platform-service-gitops/internal/controllerconst"
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

	kindAppInstallation = "AppInstallation"

	// appInstalledCondition is the condition on AppInstallation that must be True.
	appInstalledCondition = "AppInstalled"

	tokenRotationWindow = 15 * time.Minute
	fluxSecretNamespace = "flux-system"

	controllerName   = "platform-service-gitops.openmcp.cloud"
	finalizerMCPAccess = "platform-service-gitops.openmcp.cloud/mcp-access"
)

// mcpScheme is the scheme used when building clients for MCP clusters.
// Only corev1 is needed — we write Secrets there.
var mcpScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}()

// TokenMinter mints a GitHub App installation token.
type TokenMinter interface {
	MintInstallationToken(ctx context.Context, installationID int64) (string, error)
}

// GitRepositoryReconciler resolves a GitRepository's credentialRef to an
// AppInstallation and sets the CredentialResolved/Ready conditions based on the
// AppInstallation's verified status. When credentials are resolved and
// PropagateToControlPlanes is non-empty, it mints scoped GitHub App installation
// tokens and writes them as Secrets into each target MCP cluster via the
// AccessRequest protocol (openmcp-operator/lib/clusteraccess).
//
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gitops.open-control-plane.io,resources=gitrepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=github.gitops.open-control-plane.io,resources=appinstallations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=clusters.openmcp.cloud,resources=accessrequests;clusterrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clusters.openmcp.cloud,resources=accessrequests/finalizers;clusterrequests/finalizers,verbs=update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create
type GitRepositoryReconciler struct {
	platformCluster     *clusters.Cluster
	onboardingCluster   *clusters.Cluster
	credentialNamespace string
	newClient           func(githubapp.Credentials) (TokenMinter, error)
	clusterAccessRec    advanced.ClusterAccessReconciler
	resolveCredentials  func(ctx context.Context, ai *githubv1alpha1.AppInstallation) (githubapp.Credentials, error)
}

// NewGitRepositoryReconciler creates a reconciler with the given platform cluster
// (where AccessRequests are created) and onboarding cluster (where GitRepository
// CRs live), plus the credential namespace for GitHub App secrets.
func NewGitRepositoryReconciler(platform, onboarding *clusters.Cluster, credentialNamespace string) *GitRepositoryReconciler {
	r := &GitRepositoryReconciler{
		platformCluster:     platform,
		onboardingCluster:   onboarding,
		credentialNamespace: credentialNamespace,
		newClient: func(creds githubapp.Credentials) (TokenMinter, error) {
			return githubapp.NewClient(creds)
		},
	}
	r.resolveCredentials = func(ctx context.Context, ai *githubv1alpha1.AppInstallation) (githubapp.Credentials, error) {
		return credentials.Resolve(ctx, onboarding.Client(), ai.Spec.InstanceRef.Name, ai.Spec.CredentialName, credentialNamespace)
	}
	return r
}

// SetTokenMinterFactory replaces the factory used to build a TokenMinter from
// credentials. Intended for testing only.
func (r *GitRepositoryReconciler) SetTokenMinterFactory(f func(githubapp.Credentials) (TokenMinter, error)) {
	r.newClient = f
}

// SetClusterAccessReconciler replaces the ClusterAccessReconciler. Intended for testing only.
func (r *GitRepositoryReconciler) SetClusterAccessReconciler(rec advanced.ClusterAccessReconciler) {
	r.clusterAccessRec = rec
}

// SetCredentialResolver replaces the credential resolution function. Intended for testing only.
func (r *GitRepositoryReconciler) SetCredentialResolver(f func(ctx context.Context, ai *githubv1alpha1.AppInstallation) (githubapp.Credentials, error)) {
	r.resolveCredentials = f
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *GitRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.clusterAccessRec = advanced.NewClusterAccessReconciler(
		r.platformCluster.Client(),
		controllerName,
	).WithRetryInterval(10 * time.Second)

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.GitRepository{}).
		Watches(&githubv1alpha1.AppInstallation{}, handler.EnqueueRequestsFromMapFunc(r.mapAppInstallationToGitRepositories)).
		Complete(r)
}

// mapAppInstallationToGitRepositories returns reconcile requests for every
// GitRepository in the AppInstallation's namespace that references it.
func (r *GitRepositoryReconciler) mapAppInstallationToGitRepositories(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &corev1alpha1.GitRepositoryList{}
	if err := r.onboardingCluster.Client().List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
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
	if err := r.onboardingCluster.Client().Get(ctx, req.NamespacedName, gr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching GitRepository: %w", err)
	}

	// Deletion path: clean up AccessRequests then remove our finalizer.
	if !gr.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, req, gr)
	}

	// Ensure our finalizer is present so we can clean up AccessRequests on deletion.
	if !controllerutil.ContainsFinalizer(gr, finalizerMCPAccess) {
		controllerutil.AddFinalizer(gr, finalizerMCPAccess)
		if err := r.onboardingCluster.Client().Update(ctx, gr); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	patch := client.MergeFrom(gr.DeepCopy())
	resolved, reason, installationID := r.resolveCredential(ctx, gr)

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

	var requeueAfter time.Duration
	if len(gr.Spec.PropagateToControlPlanes) > 0 || len(gr.Status.PropagateStatus) > 0 {
		var requeue bool
		var err error
		requeueAfter, requeue, err = r.syncTokens(ctx, req, gr, resolved, installationID)
		if err != nil {
			return ctrl.Result{}, err
		}
		if requeue {
			// AccessRequests are still pending — patch status and requeue.
			gr.Status.ObservedGeneration = gr.Generation
			if pErr := r.onboardingCluster.Client().Status().Patch(ctx, gr, patch); pErr != nil {
				return ctrl.Result{}, fmt.Errorf("patching status: %w", pErr)
			}
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
	}

	gr.Status.ObservedGeneration = gr.Generation
	if err := r.onboardingCluster.Client().Status().Patch(ctx, gr, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
	}

	logger.Info("Reconciled GitRepository", "name", req.Name, "credentialResolved", resolved)
	if requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{RequeueAfter: controllerconst.RequeueInterval}, nil
}

func (r *GitRepositoryReconciler) reconcileDelete(ctx context.Context, req ctrl.Request, gr *corev1alpha1.GitRepository) (ctrl.Result, error) {
	// Register all known targets (spec + status) so ReconcileDelete cleans up all their AccessRequests.
	r.registerMCPs(gr)
	for _, st := range gr.Status.PropagateStatus {
		r.clusterAccessRec.Register(mcpClusterRegistration(st.Name))
	}

	res, err := r.clusterAccessRec.ReconcileDelete(ctx, req)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("deleting AccessRequests: %w", err)
	}
	if res.RequeueAfter > 0 {
		return ctrl.Result{RequeueAfter: res.RequeueAfter}, nil
	}

	controllerutil.RemoveFinalizer(gr, finalizerMCPAccess)
	if err := r.onboardingCluster.Client().Update(ctx, gr); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// resolveCredential resolves the credentialRef to an AppInstallation, verifies
// the App is installed, and returns whether resolution succeeded, the reason
// (used for the Ready condition), and the installation ID (non-zero on success).
func (r *GitRepositoryReconciler) resolveCredential(ctx context.Context, gr *corev1alpha1.GitRepository) (bool, string, int64) {
	ref := gr.Spec.CredentialRef

	if ref.Kind != kindAppInstallation {
		r.setResolved(gr, metav1.ConditionFalse, reasonUnsupportedKind,
			fmt.Sprintf("credentialRef.kind %q is not supported; only %q is.", ref.Kind, kindAppInstallation))
		return false, reasonUnsupportedKind, 0
	}

	ai := &githubv1alpha1.AppInstallation{}
	if err := r.onboardingCluster.Client().Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: gr.Namespace}, ai); err != nil {
		if apierrors.IsNotFound(err) {
			r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
				fmt.Sprintf("AppInstallation %s not found in namespace %s.", ref.Name, gr.Namespace))
			return false, reasonCredentialNotFound, 0
		}
		r.setResolved(gr, metav1.ConditionFalse, reasonCredentialNotFound,
			fmt.Sprintf("Fetching AppInstallation %s failed: %v.", ref.Name, err))
		return false, reasonCredentialNotFound, 0
	}

	// The AppInstallation controller already verified App installation and
	// access. Trusting its status avoids minting an installation token here:
	// tokens are rate-limited (1 per installation per hour on GHE) and would be
	// discarded, so minting on every reconcile would exhaust the quota. Token
	// minting happens only where a token is actually used (the propagateTo flow).
	if !meta.IsStatusConditionTrue(ai.Status.Conditions, appInstalledCondition) || ai.Status.InstallationID == 0 {
		r.setResolved(gr, metav1.ConditionFalse, reasonAppNotInstalled,
			fmt.Sprintf("AppInstallation %s is not ready (App not installed yet).", ref.Name))
		return false, reasonAppNotInstalled, 0
	}

	r.setResolved(gr, metav1.ConditionTrue, reasonCredentialFound,
		fmt.Sprintf("Resolved via AppInstallation %s (installation %d).", ref.Name, ai.Status.InstallationID))
	return true, reasonCredentialFound, ai.Status.InstallationID
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

// mcpClusterRegistration builds a ClusterRegistration for a single MCP by name.
func mcpClusterRegistration(name string) advanced.ClusterRegistration {
	return advanced.ExistingClusterRequest(name, name, func(req reconcile.Request, _ ...any) (*commonapi.ObjectReference, error) {
		return &commonapi.ObjectReference{Name: req.Name, Namespace: req.Namespace}, nil
	}).
		WithNamespaceGenerator(advanced.DefaultNamespaceGeneratorForMCP).
		WithTokenAccess(&clustersv1alpha1.TokenConfig{
			RoleRefs: []commonapi.RoleRef{{Kind: "ClusterRole", Name: "cluster-admin"}},
		}).
		WithScheme(mcpScheme).
		Build()
}

// registerMCPs registers each propagateTo target with the clusterAccessRec.
func (r *GitRepositoryReconciler) registerMCPs(gr *corev1alpha1.GitRepository) {
	for _, target := range gr.Spec.PropagateToControlPlanes {
		r.clusterAccessRec.Register(mcpClusterRegistration(target.Name))
	}
}

// syncTokens ensures a scoped installation token Secret exists and is fresh in
// every target MCP cluster. It returns the duration until the earliest token
// rotation is needed, a bool indicating whether AccessRequests are still
// pending (caller should requeue), and any error.
func (r *GitRepositoryReconciler) syncTokens(
	ctx context.Context,
	req ctrl.Request,
	gr *corev1alpha1.GitRepository,
	resolved bool,
	installationID int64,
) (time.Duration, bool, error) {
	logger := log.FromContext(ctx)

	statusByName := make(map[string]*corev1alpha1.MCPPropagateState, len(gr.Status.PropagateStatus))
	for i := range gr.Status.PropagateStatus {
		statusByName[gr.Status.PropagateStatus[i].Name] = &gr.Status.PropagateStatus[i]
	}

	specNames := make(map[string]bool, len(gr.Spec.PropagateToControlPlanes))
	for _, t := range gr.Spec.PropagateToControlPlanes {
		specNames[t.Name] = true
	}

	// When credentials are not resolved we can only clean up — do not mint new tokens.
	if !resolved {
		gr.Status.PropagateStatus = nil
		return 0, false, nil
	}

	// Register all current spec targets plus any removed ones still in status
	// (so their AccessRequests can be used to delete Secrets before cleanup).
	r.registerMCPs(gr)
	for _, st := range gr.Status.PropagateStatus {
		if !specNames[st.Name] {
			r.clusterAccessRec.Register(mcpClusterRegistration(st.Name))
		}
	}

	// Drive AccessRequest lifecycle for all registered MCPs (spec + removed).
	arResult, err := r.clusterAccessRec.Reconcile(ctx, reconcile.Request(req))
	if err != nil {
		return 0, false, fmt.Errorf("reconciling AccessRequests: %w", err)
	}
	if arResult.RequeueAfter > 0 {
		// Some AccessRequests are still pending — requeue and wait.
		return arResult.RequeueAfter, true, nil
	}

	// All AccessRequests are granted. Delete Secrets for removed MCPs, then unregister.
	for _, st := range gr.Status.PropagateStatus {
		if !specNames[st.Name] {
			if err := r.deleteTokenSecret(ctx, req, st.Name); err != nil {
				logger.Error(err, "failed to delete token Secret for removed MCP", "mcp", st.Name)
			}
			r.clusterAccessRec.Unregister(st.Name)
		}
	}

	var newStatus []corev1alpha1.MCPPropagateState
	var earliestExpiry time.Duration

	for _, target := range gr.Spec.PropagateToControlPlanes {
		existing := statusByName[target.Name]
		st := r.syncOneMCP(ctx, req, gr, target, existing, installationID)
		newStatus = append(newStatus, st)

		if st.TokenExpiresAt != nil {
			remaining := time.Until(st.TokenExpiresAt.Time) - tokenRotationWindow
			if remaining > 0 && (earliestExpiry == 0 || remaining < earliestExpiry) {
				earliestExpiry = remaining
			}
		}
	}

	gr.Status.PropagateStatus = newStatus
	return earliestExpiry, false, nil
}

func (r *GitRepositoryReconciler) syncOneMCP(
	ctx context.Context,
	req ctrl.Request,
	gr *corev1alpha1.GitRepository,
	target corev1alpha1.PropagateTarget,
	existing *corev1alpha1.MCPPropagateState,
	installationID int64,
) corev1alpha1.MCPPropagateState {
	logger := log.FromContext(ctx)

	needsToken := existing == nil ||
		existing.Phase != corev1alpha1.TokenSyncPhaseTokenSynced ||
		existing.TokenExpiresAt == nil ||
		time.Until(existing.TokenExpiresAt.Time) <= tokenRotationWindow

	if !needsToken {
		return *existing
	}

	mcpCluster, err := r.clusterAccessRec.Access(ctx, reconcile.Request(req), target.Name)
	if err != nil {
		logger.Error(err, "failed to get MCP cluster access", "mcp", target.Name)
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("resolving MCP client failed: %v", err),
		}
	}

	ai := &githubv1alpha1.AppInstallation{}
	if err := r.onboardingCluster.Client().Get(ctx, types.NamespacedName{Name: gr.Spec.CredentialRef.Name, Namespace: gr.Namespace}, ai); err != nil {
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("re-fetching AppInstallation failed: %v", err),
		}
	}

	creds, err := r.resolveCredentials(ctx, ai)
	if err != nil {
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("resolving credentials failed: %v", err),
		}
	}

	gh, err := r.newClient(creds)
	if err != nil {
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("building GitHub client failed: %v", err),
		}
	}

	token, err := gh.MintInstallationToken(ctx, installationID)
	if err != nil {
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("minting token failed: %v", err),
		}
	}

	expiry := metav1.NewTime(time.Now().Add(1 * time.Hour))
	secretName := tokenSecretName(gr)

	if err := r.writeTokenSecret(ctx, mcpCluster.Client(), secretName, gr, token); err != nil {
		return corev1alpha1.MCPPropagateState{
			Name:    target.Name,
			Phase:   corev1alpha1.TokenSyncPhaseError,
			Message: fmt.Sprintf("writing token Secret failed: %v", err),
		}
	}

	logger.Info("synced token to MCP", "mcp", target.Name, "secret", secretName)
	return corev1alpha1.MCPPropagateState{
		Name:           target.Name,
		Phase:          corev1alpha1.TokenSyncPhaseTokenSynced,
		Message:        fmt.Sprintf("Token synced to Secret %s/%s.", fluxSecretNamespace, secretName),
		TokenExpiresAt: &expiry,
	}
}

func tokenSecretName(gr *corev1alpha1.GitRepository) string {
	name := fmt.Sprintf("gitrepository-%s-%s", gr.Namespace, gr.Name)
	if len(name) <= 253 {
		return name
	}
	// Namespace+name combination exceeds the 253-char Kubernetes name limit.
	// Fall back to a deterministic hash so the name is always valid.
	h := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	return "gitrepository-" + h[:16]
}

func (r *GitRepositoryReconciler) writeTokenSecret(
	ctx context.Context,
	mcpClient client.Client,
	name string,
	gr *corev1alpha1.GitRepository,
	token string,
) error {
	// Pre-flight: if a Secret with the wrong type already exists, delete it before
	// calling CreateOrUpdate. Secret.Type is immutable once set, so CreateOrUpdate
	// cannot fix a type mismatch — it must be deleted and recreated.
	existing := &corev1.Secret{}
	if err := mcpClient.Get(ctx, types.NamespacedName{Name: name, Namespace: fluxSecretNamespace}, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("fetching Secret %s/%s: %w", fluxSecretNamespace, name, err)
		}
	} else if existing.Type != "" && existing.Type != corev1.SecretTypeBasicAuth {
		if delErr := mcpClient.Delete(ctx, existing); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("deleting Secret with wrong type: %w", delErr)
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: fluxSecretNamespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, mcpClient, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = make(map[string]string)
		}
		secret.Labels["gitops.open-control-plane.io/source-namespace"] = gr.Namespace
		secret.Labels["gitops.open-control-plane.io/source-name"] = gr.Name
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data["username"] = []byte("x-access-token")
		secret.Data["password"] = []byte(token)
		secret.Type = corev1.SecretTypeBasicAuth
		return nil
	})
	return err
}

func (r *GitRepositoryReconciler) deleteTokenSecret(
	ctx context.Context,
	req ctrl.Request,
	mcpName string,
) error {
	mcpCluster, err := r.clusterAccessRec.Access(ctx, reconcile.Request(req), mcpName)
	if err != nil {
		return fmt.Errorf("resolving MCP client: %w", err)
	}
	secret := &corev1.Secret{}
	name := tokenSecretName(&corev1alpha1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: req.Namespace},
	})
	if err := mcpCluster.Client().Get(ctx, types.NamespacedName{Name: name, Namespace: fluxSecretNamespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("fetching Secret %s/%s: %w", fluxSecretNamespace, name, err)
	}
	return mcpCluster.Client().Delete(ctx, secret)
}
