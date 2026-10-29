// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// The pace of the background poller (D-14, RESEARCH Pitfalls 4 and 14).
const (
	// DefaultPollTick is how often the poller looks for stacks that are due. A stack is checked on its own interval
	// (5 minutes unless set); the tick only decides how late a due check can start.
	DefaultPollTick = 15 * time.Second
	// DefaultMaxFetches is how many repositories are talked to at once.
	DefaultMaxFetches = 4

	// pollJitterMax caps the random delay added to a stack's next check, so the stacks of one round drift apart.
	pollJitterMax = 60 * time.Second
)

// outcomeFailed is what the Checked seam is told of a check that could not be recorded (Bayplan's own storage failed,
// or the stack was deleted while it waited), as opposed to a check that found the repository in error.
const outcomeFailed = "failed"

// PollerDeps are what RunPoller works with.
type PollerDeps struct {
	// Clock is the time the poller's timer and its due times run on. Nil is the wall clock.
	Clock stackops.Clock
	// Tick is how often due stacks are looked for. Zero is DefaultPollTick.
	Tick time.Duration
	// MaxFetches is how many repositories are read at once. Zero is DefaultMaxFetches.
	MaxFetches int
	// Rand returns a number in [0, 1): it adds the jitter of each next check. Nil is math/rand/v2. A test passes its
	// own, so that the jitter is known.
	Rand func() float64
	// Checked, when set, is told what became of each stack the poller checked: the Outcome, or "failed". A test waits
	// on it instead of on a length of time.
	Checked func(stackID, outcome string)
}

// RunPoller checks every git stack with polling on, on its interval, until ctx ends (D-14). It is the one background
// check of repositories, and it only records what it sees (CheckWith): a poll never starts a deploy.
//
// The stacks of one repository are checked together, so that the repository is asked for its refs once (ls-remote) and
// every stack follows the answer. Stacks due together are checked in the order of their next check time, then stack ID.
// RunPoller waits for the checks it started before it returns.
func (s *Service) RunPoller(ctx context.Context, d PollerDeps) {
	if d.Clock == nil {
		d.Clock = stackops.RealClock()
	}
	if d.Tick <= 0 {
		d.Tick = DefaultPollTick
	}
	if d.MaxFetches <= 0 {
		d.MaxFetches = DefaultMaxFetches
	}
	if d.Rand == nil {
		d.Rand = rand.Float64
	}
	p := &poller{s: s, d: d}
	defer p.wg.Wait()
	for {
		p.round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-d.Clock.After(d.Tick):
		}
	}
}

type poller struct {
	s  *Service
	d  PollerDeps
	wg sync.WaitGroup
}

// pollGroup is the due stacks of one repository, in the order they are checked in.
type pollGroup struct {
	repoID string
	stacks []stackops.PolledStack
}

func (p *poller) round(ctx context.Context) {
	stacks, err := p.s.repo.PolledStacks(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.s.log.Error("could not list the git stacks to poll", "error", err)
		}
		return
	}
	now := p.d.Clock.Now()
	var due []stackops.PolledStack
	for _, ps := range stacks {
		if ps.NextCheckAt == nil || !ps.NextCheckAt.After(now) {
			due = append(due, ps)
		}
	}
	slices.SortFunc(due, func(a, b stackops.PolledStack) int {
		an, bn := now, now
		if a.NextCheckAt != nil {
			an = *a.NextCheckAt
		}
		if b.NextCheckAt != nil {
			bn = *b.NextCheckAt
		}
		if c := an.Compare(bn); c != 0 {
			return c
		}
		return strings.Compare(a.StackID, b.StackID)
	})
	var groups []*pollGroup
	byRepo := map[string]*pollGroup{}
	for _, ps := range due {
		g := byRepo[ps.RepoID]
		if g == nil {
			g = &pollGroup{repoID: ps.RepoID}
			byRepo[ps.RepoID] = g
			groups = append(groups, g)
		}
		g.stacks = append(g.stacks, ps)
	}
	for _, g := range groups {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.readRepo(ctx, g)
		}()
	}
}

// readRepo checks the due stacks of one repository one after the other, with one listing of its refs between them.
func (p *poller) readRepo(ctx context.Context, g *pollGroup) {
	list, err := p.s.listRefs(ctx, g.repoID)
	listed := err == nil
	for _, ps := range g.stacks {
		if ctx.Err() != nil {
			return
		}
		head := ""
		if listed {
			head = advertisedHead(list, ps)
		}
		out, err := p.s.CheckWith(ctx, ps.EnvID, ps.StackID, CheckOptions{Trigger: TriggerPoll, RemoteHead: head})
		if ctx.Err() != nil {
			return
		}
		result := string(out)
		if err != nil {
			result = outcomeFailed
			p.s.log.Warn("could not check a git stack", "stack", ps.StackID, "error", err)
		}
		next := p.d.Clock.Now().Add(p.delay(ps.Interval))
		if serr := p.s.repo.SetNextCheck(ctx, ps.StackID, next); serr != nil && ctx.Err() == nil {
			p.s.log.Warn("could not schedule the next check of a git stack", "stack", ps.StackID, "error", serr)
		}
		if p.d.Checked != nil {
			p.d.Checked(ps.StackID, result)
		}
	}
}

// delay is how long after a check the next one is: the interval plus jitter of up to a tenth of it and at most a minute.
func (p *poller) delay(interval time.Duration) time.Duration {
	spread := min(interval/10, pollJitterMax)
	return interval + time.Duration(p.d.Rand()*float64(spread))
}

// advertisedHead is the commit the repository advertises for the ref the stack follows, or "" when it does not
// advertise it (the stack is then checked in full, which names a branch that is gone).
func advertisedHead(list gitx.RefList, ps stackops.PolledStack) string {
	refs := list.Branches
	if gitx.RefKind(ps.RefKind) == gitx.RefTag {
		refs = list.Tags
	}
	for _, r := range refs {
		if r.Name == ps.Ref {
			return r.Commit
		}
	}
	return ""
}

// listRefs asks the repository for its branches and tags (ls-remote), with the repository's credential. It is one
// request to the git host whatever the number of stacks that follow the repository.
func (s *Service) listRefs(ctx context.Context, repoID string) (gitx.RefList, error) {
	repo, err := s.conns.GetRepo(ctx, repoID)
	if err != nil {
		return gitx.RefList{}, err
	}
	rem, err := s.conns.Remote(ctx, repo)
	if err != nil {
		return gitx.RefList{}, err
	}
	list, err := s.git.LsRemote(ctx, rem)
	if ctx.Err() == nil {
		s.recordUse(ctx, repo, err)
	}
	return list, err
}
