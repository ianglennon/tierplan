// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package janitor decides what the cleanup of an environment would remove, and why.
//
// The package is split into a pure planner and an impure shell. The planner (Build and the
// classifiers beside it) takes a Snapshot of what Docker holds, the inspect facts of the few
// containers that need them, a Policy and a clock, and answers with a Plan: every object it
// would remove, and every object it looked at and left, each with a class and a reason. Nothing
// in the planner is a Docker type or reads a clock or a file, so the order of its rules is a
// table test. The shell reads Docker through the Engine port, which the server implements.
//
// Safety is the shape of this package. It never removes a volume, never calls a prune endpoint of
// the engine (a prune judges by its own rules and cannot be previewed or audited object by
// object), and never imports internal/api: the server hands it ports. A guard test fails the
// build the day a call to a volume removal or a prune appears in a file of this package.
package janitor
