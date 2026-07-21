// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

package crds

import "embed"

//go:embed manifests/*.yaml
var Manifests embed.FS
