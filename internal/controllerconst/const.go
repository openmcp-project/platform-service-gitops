// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and Open Control Plane contributors
// SPDX-License-Identifier: Apache-2.0

// Package controllerconst holds constants shared across controllers.
package controllerconst

import "time"

// RequeueInterval is the periodic re-check interval used by controllers whose
// state can change out of band (e.g. a referenced resource becoming ready).
// Single source of truth so the cadence stays consistent across controllers.
const RequeueInterval = 10 * time.Minute
