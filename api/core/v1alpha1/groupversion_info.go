// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains API Schema definitions for the gitops.open-control-plane.io/v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=gitops.open-control-plane.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is group version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: "gitops.open-control-plane.io", Version: "v1alpha1"}

// SchemeGroupVersion is an alias for GroupVersion for backward compatibility with
// generated and scaffolded code that references it directly. New code should prefer GroupVersion.
var SchemeGroupVersion = GroupVersion

// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck

// AddToScheme adds the types in this group-version to the given scheme.
var AddToScheme = SchemeBuilder.AddToScheme
