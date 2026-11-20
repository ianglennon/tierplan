// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import "time"

// TraefikGraceTimeout is how long Traefik lets in-flight requests finish after it is told to stop
// (D-11). It equals graceTimeOut on the web and websecure entrypoints in static/traefik.yml, which
// TestStaticConfig in internal/ingressrender proves, so the file and this constant cannot drift.
const TraefikGraceTimeout = 10 * time.Second
