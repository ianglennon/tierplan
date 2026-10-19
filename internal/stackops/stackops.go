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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/registry"
)

var (
	// ErrNameTaken means the environment already has a stack of that name.
	ErrNameTaken = errors.New("stackops: a stack of that name already exists on this environment")
	// ErrNoDraft means the stack has no draft: it is not found, or it was never written.
	ErrNoDraft = errors.New("stackops: the stack has no draft")
	// ErrRevisionNotFound means the stack has no revision with that number.
	ErrRevisionNotFound = errors.New("stackops: no such revision")
	// ErrStackNotFound means the environment has no stack of that ID.
	ErrStackNotFound = errors.New("stackops: no such stack")
	// ErrDraftConflict means the draft moved since the caller read it. The error is a
	// *DraftConflictError.
	ErrDraftConflict = errors.New("stackops: the draft was saved by someone else")
	// ErrTargetEngineMissing means the stack's target has no engine for the operation asked for in this
	// version: a standalone stack deploys with the Compose SDK, but rolling one back, and the other
	// operations of the later plans, wait for their plans.
	ErrTargetEngineMissing = errors.New("stackops: this version cannot deploy to that kind of environment yet")
	// ErrTargetChanged means the environment is no longer the kind of target the stack was created
	// for: the stack deploys to a swarm and the environment is a standalone host, or the reverse.
	ErrTargetChanged = errors.New("stackops: the environment is no longer the kind of target the stack was created for")
	// ErrNoPolicy means the host-escalation policy could not be applied: there is no checker or no
	// caller. It is never read as "allowed".
	ErrNoPolicy = errors.New("stackops: the host-escalation policy cannot be applied")
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
	// Clock is the time the deploy waits on for a service to settle. Nil is the wall clock.
	Clock Clock
	// ConvergeTimeout is how long a deploy waits for each service to settle. Zero is
	// DefaultConvergeTimeout.
	ConvergeTimeout time.Duration
	// Up runs the Compose SDK's up for a standalone deploy. Nil is (*dockerx.Client).ComposeUp, which needs a
	// real engine and is proved against one; a test sets it to feed a deploy a stream of events and an
	// outcome.
	Up func(ctx context.Context, c *dockerx.Client, p *types.Project, o dockerx.ComposeUpOptions, on func(dockerx.ComposeEvent)) error
	// Writable asks whether the node of a standalone host can be written to: it is the check step's first
	// question, so a host whose owner made it read-only (envs.ErrHostReadOnly) fails the deploy before it
	// asks the host or a registry for anything. Nil asks the aggregator with a write that does nothing
	// (WriteOnNode refuses a read-only host before it takes a client); a test sets it to play a host that
	// answers otherwise, which the aggregator proves for itself.
	Writable func(ctx context.Context, envID, nodeID string) error
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// up runs the Compose SDK's up on c, or the test's stand-in for it.
func (d Deps) up(ctx context.Context, c *dockerx.Client, p *types.Project, o dockerx.ComposeUpOptions, on func(dockerx.ComposeEvent)) error {
	if d.Up != nil {
		return d.Up(ctx, c, p, o, on)
	}
	return c.ComposeUp(ctx, p, o, on)
}

// writable asks whether the node can be written to (Deps.Writable).
func (d Deps) writable(ctx context.Context, envID, nodeID string) error {
	if d.Writable != nil {
		return d.Writable(ctx, envID, nodeID)
	}
	return d.Aggregator.WriteOnNode(ctx, envID, nodeID, func(*dockerx.Client) error { return nil })
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
