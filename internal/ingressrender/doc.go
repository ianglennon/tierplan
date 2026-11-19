// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ingressrender turns an environment's ingress snapshot and its servable certificates into a
// versioned Traefik bundle: one dynamic configuration file, the certificate files it names, and a
// manifest that fixes the order a node applies them in.
//
// The package is pure. It holds no store, vault, clock or network handle: the snapshot, the opened
// certificates and the current time are inputs, so the same inputs always render the same bytes.
//
// It also owns the fixed static Traefik file that the Phase 23 proxy image ships. The file is embedded
// (see Static), is not rendered per environment, and is the same file the real-Traefik test harness
// starts, so the image and the tests cannot drift apart.
package ingressrender
