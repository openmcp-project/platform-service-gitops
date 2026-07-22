// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package crds

import (
	"embed"

	crdutil "github.com/openmcp-project/controller-utils/pkg/crds"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

//go:embed manifests
var CRDFS embed.FS

// CRDs returns all CRD definitions embedded in this package.
// Each CRD carries an openmcp.cloud/cluster label that the CRDManager uses
// to route it to the correct cluster (platform or onboarding).
func CRDs() ([]*apiextv1.CustomResourceDefinition, error) {
	return crdutil.CRDsFromFileSystem(CRDFS, "manifests")
}
