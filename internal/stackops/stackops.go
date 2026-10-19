// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package stackops is the orchestration of a compose stack: its stored definition (the draft and
// the revisions a deploy takes), and the deploy that turns a converted plan into swarm objects
// (STK-01, STK-02, STK-05). It imports no HTTP type and no huma; it takes the aggregator, the
// credential resolver and the caller through its input structs, so it is tested against the
// fake daemon with injected faults. Later plans add the review, the standalone engine, the
// lifecycle and the lock to the same package.
//
// Writing a draft never touches an engine: only a deploy does, and every engine write goes
// through Aggregator.WriteOnManager, once, never retried (P2 D-42).
package stackops

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/registry"
)

var (
	// ErrNameTaken means the environment already has a stack of that name.
	ErrNameTaken = errors.New("stackops: a stack of that name already exists on this environment")
	// ErrNoDraft means the stack has no draft: it is not found, or it was never written.
	ErrNoDraft = errors.New("stackops: the stack has no draft")
	// ErrStackNotFound means the environment has no stack of that ID.
	ErrStackNotFound = errors.New("stackops: no such stack")
	// ErrDraftConflict means the draft moved since the caller read it. The error is a
	// *DraftConflictError.
	ErrDraftConflict = errors.New("stackops: the draft was saved by someone else")
	// ErrTargetEngineMissing means the stack's target has no deploy engine in this version: a
	// standalone stack waits for the Compose engine (plan 07-20).
	ErrTargetEngineMissing = errors.New("stackops: this version cannot deploy to that kind of environment yet")
)

// DraftConflictError is the refusal of a save made against a draft that has moved: who saved it
// last, when, and the version it is at now.
type DraftConflictError struct {
	SavedBy string
	SavedAt time.Time
	Version int64
}

func (e *DraftConflictError) Error() string {
	return fmt.Sprintf("stackops: the draft was saved by %s at version %d", e.SavedBy, e.Version)
}

// Is makes errors.Is(err, ErrDraftConflict) true.
func (e *DraftConflictError) Is(target error) bool { return target == ErrDraftConflict }

// Deps are what the deploy needs. Zero fields a given operation does not use may be left nil.
type Deps struct {
	// Aggregator is the one write path of a cluster-scoped change: WriteOnManager.
	Aggregator *envs.Aggregator
	// Clusters is the cluster reader the deploy reads what a stack owns from.
	Clusters *envs.Clusters
	// Resolver chooses the registry credential each service is sent with.
	Resolver *registry.Resolver
	// Checker is the permission checker the later plans' policy checks use.
	Checker *rbac.Checker
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

func (d Deps) log() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

// Caller is who asks. Actor is the display name recorded as the person who saved or deployed.
type Caller struct {
	Principal *rbac.Principal
	Actor     string
}
