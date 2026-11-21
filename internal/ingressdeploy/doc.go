// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ingressdeploy deploys the swarm objects of an environment's ingress: the proxy service, its
// overlay network, its certificate cache volume and the swarm secret that carries its join token. It
// owns enable, ensure and disable.
//
// The objects are built by pure functions in spec.go, which make no engine call, so one builder keeps
// the labels every consumer reads on every object and keeps the spec independent of any node (one
// global service). Every write goes through the manager or node write path and is never retried: a
// repeated create could leave a second object behind.
package ingressdeploy
