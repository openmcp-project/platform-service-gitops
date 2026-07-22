// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SourceRef identifies the openmcp GitRepository that this Kustomization pulls from.
type SourceRef struct {
	// Kind must be GitRepository.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=GitRepository
	Kind string `json:"kind"`

	// Name of the GitRepository in the same namespace.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// KustomizationSpec defines the desired state of Kustomization.
type KustomizationSpec struct {
	// SourceRef points to the openmcp GitRepository to use as the source.
	// Only kind: GitRepository (gitops.open-control-plane.io) is accepted.
	// +kubebuilder:validation:Required
	SourceRef SourceRef `json:"sourceRef"`

	// Path within the repository to reconcile.
	// +kubebuilder:default="./"
	// +optional
	Path string `json:"path,omitempty"`

	// Interval at which to reconcile the Kustomization.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`
	Interval metav1.Duration `json:"interval"`

	// Prune enables garbage collection of removed manifests.
	// +optional
	Prune bool `json:"prune,omitempty"`
}

// KustomizationStatus defines the observed state of Kustomization.
type KustomizationStatus struct {
	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions summarise the current state.
	// Known types: Ready, SourceInvalid.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastAppliedRevision is the revision of the last successful reconciliation
	// as reported by the backing Flux Kustomization.
	// +optional
	LastAppliedRevision string `json:"lastAppliedRevision,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={gitops,openmcp}
// +kubebuilder:printcolumn:name="SOURCE",type="string",JSONPath=".spec.sourceRef.name"
// +kubebuilder:printcolumn:name="PATH",type="string",JSONPath=".spec.path"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// Kustomization is the Schema for the kustomizations API.
type Kustomization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KustomizationSpec   `json:"spec,omitempty"`
	Status KustomizationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KustomizationList contains a list of Kustomization.
type KustomizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Kustomization `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Kustomization{}, &KustomizationList{})
}
