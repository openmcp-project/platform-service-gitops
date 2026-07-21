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
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`
	// +kubebuilder:default=""
	// +optional
	Group string `json:"group,omitempty"`
}

// PropagateTarget declares a ControlPlane that should receive scoped repository access.
type PropagateTarget struct {
	// Kind of target. Currently only ControlPlane is supported.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
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

	// PropagateToControlPlanes lists ControlPlanes that should receive a
	// scoped token and a Flux GitRepository resource for this source.
	// Behaviour implemented in a follow-up issue.
	// +optional
	PropagateToControlPlanes []PropagateTarget `json:"propagateTo,omitempty"`
}

// GitRepositoryStatus defines the observed state of GitRepository.
type GitRepositoryStatus struct {
	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions summarise the current state.
	// Known types: Ready, CredentialResolved.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
