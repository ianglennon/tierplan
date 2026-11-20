// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ingressnode holds the integration tests of the tierplan-ingress image: the real image, run in a
// docker-in-docker node, enrolling with the Phase 22 hub (internal/ingresshub/ingresstest) and serving a
// bundle with a real Traefik. They cover a certificate applied without a Traefik restart, a bundle the node
// rejects, a killed Traefik, a revoked node, a node serving from its cache while TierPlan is stopped, and a
// node with nothing to serve.
//
// Every test file carries the build tag integration, so the package has no code without it. The tests run on
// the engine 29 leg only (traefiktest.SkipUnlessEngineLeg), and need `make ingress-image` first; `make
// test-int` builds the image.
package ingressnode
