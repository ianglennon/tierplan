// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package gitsync reads a stack's files from a git repository and, in the plans that follow, keeps the stack in
// step with it. It imports gitx (the one place go-git is used), gitconn (repositories and their credentials),
// stacks (the stack model) and stackops (where a stack is stored); stackops never imports gitx.
package gitsync

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitconn"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// Deps is what a Service needs.
type Deps struct {
	// Git reads the repository and its mirror.
	Git gitx.Client
	// Mirrors lists, measures and removes the mirrors for the sweep (RunMirrorGC). Nil takes the Git client when that
	// keeps mirrors on disk (gitx.Mirrors), else the sweep does nothing.
	Mirrors gitx.Mirrors
	// Conns holds the repositories and their credentials.
	Conns *gitconn.Store
	// Repo stores the stacks.
	Repo stackops.Repo
	Log  *slog.Logger
	// Audit records a stack's sync going into error. Nil records nothing (a test that does not look at the log).
	Audit *audit.Writer
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Access rechecks the owner of a stack's automatic deploys before each one (access.Service.RunAs). Nil, or no
	// Deployer, means no stack deploys by itself: a check only records.
	Access *access.Service
	// Deployer starts the deploy of an automatic deploy. SetDeployer sets it after the service is made, when it cannot
	// be built before.
	Deployer Deployer
	// Joined, when set, is called each time a caller starts or joins the check of a stack, with how many callers wait on
	// it now. A test uses it to know that every caller has joined before it lets the check finish.
	Joined func(stackID string, waiting int)
	// Triggered, when set, is called after each check a Trigger ran, with its outcome and whether another (trailing)
	// check follows at once. A test uses it to know that the triggered checks are over.
	Triggered func(stackID string, outcome Outcome, err error, more bool)
}

// Service imports the files of a stack from a repository folder.
type Service struct {
	git     gitx.Client
	mirrors gitx.Mirrors
	conns   *gitconn.Store
	repo    stackops.Repo
	log     *slog.Logger
	audit   *audit.Writer
	now     func() time.Time

	access *access.Service

	joined    func(stackID string, waiting int)
	triggered func(stackID string, outcome Outcome, err error, more bool)
	mu        sync.Mutex
	// deployer starts an automatic deploy; it is guarded by mu, since SetDeployer may run after checks have begun.
	deployer Deployer
	// checking holds the sync checks in flight, by stack ID: a second request for the same stack joins the first.
	checking map[string]*checkCall
	// triggers holds the stacks a Trigger is working on: a trigger while one runs only marks it to run once more.
	triggers map[string]*triggerRun
	// stateLocks holds the sync-state locks that are held or waited for (lockState).
	stateLocks map[string]*stateLock
	// life ends when Shutdown begins: the checks that run detached from their callers end with it, and no new work
	// starts once closing is set (guarded by mu). work counts the detached checks and triggers Shutdown waits for.
	life     context.Context
	stopLife context.CancelFunc
	closing  bool
	work     sync.WaitGroup
}

// ErrShuttingDown is the answer to a check asked for after Shutdown began.
var ErrShuttingDown = errors.New("gitsync: shutting down")

// Shutdown ends the work that runs apart from the callers that asked for it: the checks that CheckWith and Trigger
// start on a context of their own (those of the poller and of webhook deliveries among them), and the automatic deploys
// that follow them. No new check or trigger starts from now on. The running ones are told to stop, an automatic deploy
// that is already starting its deploy finishes that step, and Shutdown waits until all of them have returned or ctx
// ends, whichever is first (the error is then ctx's). The server calls it after the poller has stopped and before the
// database and the jobs close, so nothing writes to either afterwards.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.stopLife()
	done := make(chan struct{})
	go func() {
		s.work.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shuttingDown says whether Shutdown has begun.
func (s *Service) shuttingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// New returns a Service.
func New(d Deps) *Service {
	s := &Service{git: d.Git, conns: d.Conns, repo: d.Repo, log: d.Log, audit: d.Audit, now: d.Now, access: d.Access, deployer: d.Deployer, joined: d.Joined, triggered: d.Triggered, checking: map[string]*checkCall{}, triggers: map[string]*triggerRun{}, stateLocks: map[string]*stateLock{}}
	s.life, s.stopLife = context.WithCancel(context.Background())
	if s.mirrors = d.Mirrors; s.mirrors == nil {
		s.mirrors, _ = d.Git.(gitx.Mirrors)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}
