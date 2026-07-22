// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1_test

import (
	"testing"

	"github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGitRepository_SpecFields(t *testing.T) {
	gr := v1alpha1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "my-infra", Namespace: "my-project"},
		Spec: v1alpha1.GitRepositorySpec{
			URL:  "https://github.com/my-org/my-infra",
			Ref:  v1alpha1.GitRef{Branch: "main"},
			Path: "./",
			CredentialRef: v1alpha1.CredentialRef{
				Name:  "my-github-connection",
				Kind:  "AppInstallation",
				Group: "github.gitops.open-control-plane.io",
			},
		},
	}
	assert.Equal(t, "https://github.com/my-org/my-infra", gr.Spec.URL)
	assert.Equal(t, "main", gr.Spec.Ref.Branch)
	assert.Equal(t, "AppInstallation", gr.Spec.CredentialRef.Kind)
	assert.Equal(t, "./", gr.Spec.Path)
}

func TestGitRepository_StatusConditions(t *testing.T) {
	gr := v1alpha1.GitRepository{}
	assert.Empty(t, gr.Status.Conditions)
	gr.Status.Conditions = append(gr.Status.Conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionFalse,
		Reason: "Reconciling",
	})
	assert.Len(t, gr.Status.Conditions, 1)
	assert.Equal(t, "Ready", gr.Status.Conditions[0].Type)
}
