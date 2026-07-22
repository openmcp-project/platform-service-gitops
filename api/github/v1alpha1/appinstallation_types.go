// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InstanceReference selects the GitHubInstance that provides credentials.
type InstanceReference struct {
	// Name of the (cluster-scoped) GitHubInstance.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// AppInstallationSpec defines the desired state of AppInstallation.
//
// An AppInstallation is created by an end user in a Project namespace. It links
// the Project to a GitHub App installation on the user's own GitHub org (or
// user account). Credentials are never supplied here — they are resolved from
// the referenced GitHubInstance, which the platform owner manages.
type AppInstallationSpec struct {
	// InstanceRef selects the GitHubInstance that holds the credentials.
	// +kubebuilder:validation:Required
	InstanceRef InstanceReference `json:"instanceRef"`

	// CredentialName selects which Secret from the GitHubInstance's secretRefs
	// to use. Optional when the instance lists exactly one Secret.
	// +optional
	CredentialName string `json:"credentialName,omitempty"`

	// Org is the GitHub organization to check the App installation for.
	// Exactly one of org or user must be set.
	// +optional
	Org string `json:"org,omitempty"`

	// User is the GitHub user account to check the App installation for.
	// Exactly one of org or user must be set.
	// +optional
	User string `json:"user,omitempty"`
}

// AppInstallationStatus defines the observed state of AppInstallation.
type AppInstallationStatus struct {
	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// InstallationID is the numeric GitHub App installation ID discovered for
	// the target org or user. Zero when the App is not installed.
	// +optional
	InstallationID int64 `json:"installationID,omitempty"`

	// LastChecked is the time the installation state was last verified.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`

	// Conditions summarise the current state.
	// Known types: AppInstalled, AccessVerified.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={gitops,openmcp}
// +kubebuilder:printcolumn:name="INSTANCE",type="string",JSONPath=".spec.instanceRef.name"
// +kubebuilder:printcolumn:name="ORG",type="string",JSONPath=".spec.org"
// +kubebuilder:printcolumn:name="INSTALLED",type="string",JSONPath=".status.conditions[?(@.type=='AppInstalled')].status"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// AppInstallation is the Schema for the appinstallations API.
type AppInstallation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppInstallationSpec   `json:"spec,omitempty"`
	Status AppInstallationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AppInstallationList contains a list of AppInstallation.
type AppInstallationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppInstallation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AppInstallation{}, &AppInstallationList{})
}
