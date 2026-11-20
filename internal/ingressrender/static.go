// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import "github.com/ianglennon/tierplan/proto/ingressbundle"

// Static is the fixed static Traefik configuration, as a copy the caller may change. The proxy image
// ships these bytes and the real-Traefik harness starts with them. The bytes come from
// proto/ingressbundle, the one home of the file (D-10), so the supervisor in its own module reads the
// same file.
func Static() []byte { return ingressbundle.StaticTraefik() }
