// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretReference points at a Kubernetes Secret holding GitHub App credentials.
// The referenced Secret must contain the keys "appID", "privateKey" and "url".
type SecretReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace of the Secret. Defaults to the controller's credential namespace
	// when empty.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// GitHubInstanceSpec defines the desired state of GitHubInstance.
//
// A GitHubInstance is managed by the platform owner. It groups one or more
// credential Secrets that each describe a GitHub App on a GitHub instance
// (public github.com or a GitHub Enterprise Server). End users never handle
// these Secrets; they reference the GitHubInstance from an AppInstallation.
type GitHubInstanceSpec struct {
	// SecretRefs lists the credential Secrets that belong to this instance.
	// Each Secret holds the App ID, private key and instance URL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	SecretRefs []SecretReference `json:"secretRefs"`
}

// GitHubInstanceStatus defines the observed state of GitHubInstance.
type GitHubInstanceStatus struct {
	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions summarise the current state.
	// Known types: Ready, CredentialsValid.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories={gitops,openmcp}
// +kubebuilder:metadata:labels="openmcp.cloud/cluster=platform"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// GitHubInstance is the Schema for the githubinstances API.
type GitHubInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GitHubInstanceSpec   `json:"spec,omitempty"`
	Status GitHubInstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GitHubInstanceList contains a list of GitHubInstance.
type GitHubInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitHubInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitHubInstance{}, &GitHubInstanceList{})
}
