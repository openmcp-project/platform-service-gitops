// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitRef specifies which revision of a repository to use.
type GitRef struct {
	// +optional
	Branch string `json:"branch,omitempty"`
	// +optional
	Tag string `json:"tag,omitempty"`
	// +optional
	Commit string `json:"commit,omitempty"`
}

// CredentialRef identifies the credential provider. Mirrors cert-manager's issuerRef pattern.
type CredentialRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// Kind of credential provider. Supported values: AppInstallation (platform
	// GitHub App) and Secret (user-provided PAT or SSH key).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=AppInstallation;Secret
	Kind string `json:"kind"`
	// +kubebuilder:default=""
	// +optional
	Group string `json:"group,omitempty"`
}

// PropagateTargetKind is the kind of resource a propagateTo entry refers to.
// +kubebuilder:validation:Enum=ControlPlane
type PropagateTargetKind string

const (
	// PropagateTargetKindControlPlane targets a ManagedControlPlane (MCP).
	PropagateTargetKindControlPlane PropagateTargetKind = "ControlPlane"
)

// PropagateTarget declares a target (ControlPlane or future Workspace) that should
// receive a scoped repository token and a Flux GitRepository resource.
// Exactly one of name or matchLabels must be set — setting both is undefined behaviour.
type PropagateTarget struct {
	// Kind of target. Currently only ControlPlane is supported.
	// +kubebuilder:validation:Required
	Kind PropagateTargetKind `json:"kind"`

	// Name is the explicit name of the target ControlPlane.
	// Mutually exclusive with matchLabels.
	// +optional
	Name string `json:"name,omitempty"`

	// MatchLabels selects ControlPlanes by label. All matching ControlPlanes in the
	// GitRepository's namespace receive a token and Flux GitRepository.
	// Mutually exclusive with name.
	// +optional
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

// PropagatePhase describes the reconciliation state of a single propagateTo target.
// +kubebuilder:validation:Enum=Pending;Ready;TokenFailed;FluxFailed;Conflict
type PropagatePhase string

const (
	PropagatePhasePending     PropagatePhase = "Pending"
	PropagatePhaseReady       PropagatePhase = "Ready"
	PropagatePhaseTokenFailed PropagatePhase = "TokenFailed"
	PropagatePhaseFluxFailed  PropagatePhase = "FluxFailed"
	// PropagatePhaseConflict means a Flux GitRepository with the same name already
	// exists in the MCP without our managed-by annotation; we will not overwrite it.
	PropagatePhaseConflict PropagatePhase = "Conflict"
)

// PropagateStatus holds the per-MCP reconciliation state for one resolved propagateTo target.
type PropagateStatus struct {
	// ControlPlaneName is the name of the ControlPlane this entry refers to.
	// +kubebuilder:validation:Required
	ControlPlaneName string `json:"controlPlaneName"`

	// Phase summarises the current reconciliation state for this target.
	// +optional
	Phase PropagatePhase `json:"phase,omitempty"`

	// Reason is a machine-readable reason code for the current phase.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is a human-readable description of the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// TokenExpiresAt is the expiry time of the currently active installation token.
	// The controller rotates the token before this time based on the configured renew buffer.
	// +optional
	TokenExpiresAt *metav1.Time `json:"tokenExpiresAt,omitempty"`
}

// GitRepositorySpec defines the desired state of GitRepository.
type GitRepositorySpec struct {
	// URL is the HTTPS URL of the Git repository.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://.*`
	URL string `json:"url"`

	// Ref specifies the branch, tag, or commit to track.
	// +kubebuilder:validation:Required
	Ref GitRef `json:"ref"`

	// Path within the repository to use as the source root.
	// +kubebuilder:default="./"
	// +optional
	Path string `json:"path,omitempty"`

	// CredentialRef references the credential provider for repository access.
	// +kubebuilder:validation:Required
	CredentialRef CredentialRef `json:"credentialRef"`

	// PropagateToControlPlanes lists ControlPlanes (or future Workspaces) that should
	// receive a scoped token and a Flux GitRepository resource for this source.
	// Each entry may specify an explicit name or a matchLabels selector, but not both.
	// +optional
	PropagateToControlPlanes []PropagateTarget `json:"propagateTo,omitempty"`
}

// GitRepositoryStatus defines the observed state of GitRepository.
type GitRepositoryStatus struct {
	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions summarise the current state.
	// Known condition types: Ready, CredentialResolved.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Propagated holds per-MCP status for each resolved entry in spec.propagateTo.
	// +optional
	// +listType=map
	// +listMapKey=controlPlaneName
	Propagated []PropagateStatus `json:"propagated,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={gitops,openmcp}
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".spec.url"
// +kubebuilder:printcolumn:name="BRANCH",type="string",JSONPath=".spec.ref.branch"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// GitRepository is the Schema for the gitrepositories API.
type GitRepository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GitRepositorySpec   `json:"spec,omitempty"`
	Status GitRepositoryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GitRepositoryList contains a list of GitRepository.
type GitRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitRepository `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitRepository{}, &GitRepositoryList{})
}
