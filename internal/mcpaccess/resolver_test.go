// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package mcpaccess

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/openmcp-project/platform-service-gitops/api/core/v1alpha1"
)

// TestAccessLocalNameDistinguishesNamespaces verifies that the same ControlPlane
// name in two different namespaces yields two distinct AccessRequest local names,
// so their AccessRequests do not collide on the platform cluster.
func TestAccessLocalNameDistinguishesNamespaces(t *testing.T) {
	gr := &corev1alpha1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "my-infra", Namespace: "gitops"},
	}

	a := AccessLocalName(gr, "team-a", "my-mcp-prod")
	b := AccessLocalName(gr, "team-b", "my-mcp-prod")

	if a == b {
		t.Fatalf("expected distinct local names for same ControlPlane name in different namespaces, got %q for both", a)
	}

	// Same (namespace, name) must be stable.
	if got := AccessLocalName(gr, "team-a", "my-mcp-prod"); got != a {
		t.Fatalf("AccessLocalName not stable: %q != %q", got, a)
	}
}
