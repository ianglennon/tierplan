// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"errors"
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
	// pollBackoffMax is the longest a failing stack waits for its next check. An interval longer than this is kept.
	pollBackoffMax = time.Hour
)

// outcomeFailed is what the Checked seam is told of a check that could not be recorded (Bayplan's own storage failed,
// or the stack was deleted while it waited), as opposed to a check that found the repository in error.
const outcomeFailed = "failed"

// PollerDeps are what RunPoller works with.
type PollerDeps struct {
	// Clock is the time the poller's timer, its due times and its cache run on. Nil is the wall clock.
	Clock stackops.Clock
	// Tick is how often due stacks are looked for. Zero is DefaultPollTick.
	Tick time.Duration
	// MaxFetches is how many repositories are read at once. Zero is DefaultMaxFetches.
	MaxFetches int
	// Rand returns a number in [0, 1): it spreads the first check of stacks the poller has not scheduled yet and adds
	// the jitter of each next check. Nil is math/rand/v2. A test passes its own, so that the spread is known.
	Rand func() float64
	// Checked, when set, is told what became of each stack the poller checked: the Outcome, or "failed". A test waits
	// on it instead of on a length of time. The last check of a read of a repository is told once the repository is
	// free again, so that a test which moves the clock on after it finds the next round able to read it.
	Checked func(stackID, outcome string)
}

// RunPoller checks every git stack with polling on, on its interval, until ctx ends (D-14). It is the one background
// check of repositories. A poll records what it sees (CheckWith); only a stack that has auto-deploy on goes on to deploy
// what it finds (autoDeploy), as its owner and through the same checks as a person's deploy.
//
// It is polite to the git host and to the server (Pitfalls 4 and 14):
//   - the stacks of one repository share one listing of its refs (ls-remote), kept for the shortest interval among them,
//     so a hundred stacks of one monorepo cost one advertisement request per interval;
//   - a stack whose ref still points at the commit it was last seen at is not fetched, only marked as checked;
//   - at most MaxFetches repositories are read at once and never two checks of one repository;
//   - the next check of a stack comes an interval later plus jitter, and after failures exponentially later, up to an
//     hour (an interval longer than that is kept);
//   - a stack the poller has not scheduled yet (new, or after a restart) first checks at a random moment of its first
//     interval, so a restart does not bring every check at once.
//
// Stacks due together are checked in the order of their next check time, then stack ID. RunPoller waits for the reads of
// repositories it started before it returns. The checks those reads asked for run apart from the poller, as every
// check does, and may outlive it by a moment; Shutdown ends and waits for them (WR-B01).
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
	p := &poller{
		s: s, d: d, slots: make(chan struct{}, d.MaxFetches), busy: map[string]bool{}, refs: map[string]refsEntry{},
	}
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

// refsEntry is the listing of a repository's refs and when it was made.
type refsEntry struct {
	list gitx.RefList
	at   time.Time
}

type poller struct {
	s *Service
	d PollerDeps
	// slots holds one token for each repository being read; dispatching a repository takes one.
	slots chan struct{}
	wg    sync.WaitGroup

	mu sync.Mutex
	// busy holds the repositories being read now, so that a slow one is never read twice at once.
	busy map[string]bool
	// refs is the last listing of each repository.
	refs map[string]refsEntry
}

// pollGroup is the due stacks of one repository, in the order they are checked in.
type pollGroup struct {
	repoID string
	stacks []stackops.PolledStack
	// ttl is how long a listing of the repository is good for: the shortest interval among all its polled stacks.
	ttl time.Duration
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

	// The stacks never scheduled spread over their first interval; they are not due yet. The list is by stack ID, so
	// the spread is given out in a fixed order.
	var due []stackops.PolledStack
	ttl := map[string]time.Duration{}
	for _, ps := range stacks {
		if t, ok := ttl[ps.RepoID]; !ok || ps.Interval < t {
			ttl[ps.RepoID] = ps.Interval
		}
		switch {
		case ps.NextCheckAt == nil:
			at := now.Add(time.Duration(p.d.Rand() * float64(ps.Interval)))
			if err := p.s.repo.SetNextCheck(ctx, ps.StackID, at); err != nil && ctx.Err() == nil && !errors.Is(err, stackops.ErrNoGit) {
				p.s.log.Error("could not schedule a git stack's first check", "stack", ps.StackID, "error", err)
			}
		case !ps.NextCheckAt.After(now):
			due = append(due, ps)
		}
	}
	if len(due) == 0 {
		return
	}
	slices.SortFunc(due, func(a, b stackops.PolledStack) int {
		if c := a.NextCheckAt.Compare(*b.NextCheckAt); c != 0 {
			return c
		}
		return strings.Compare(a.StackID, b.StackID)
	})
	var groups []*pollGroup
	byRepo := map[string]*pollGroup{}
	for _, ps := range due {
		g := byRepo[ps.RepoID]
		if g == nil {
			g = &pollGroup{repoID: ps.RepoID, ttl: ttl[ps.RepoID]}
			byRepo[ps.RepoID] = g
			groups = append(groups, g)
		}
		g.stacks = append(g.stacks, ps)
	}

	for _, g := range groups {
		if !p.begin(g.repoID) {
			// Still being read from an earlier round: its stacks stay due and are looked at on the next tick.
			continue
		}
		select {
		case p.slots <- struct{}{}:
		case <-ctx.Done():
			p.end(g.repoID)
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			var last *polledCheck
			defer func() { p.report(last) }()
			defer func() { <-p.slots }()
			defer p.end(g.repoID)
			last = p.readRepo(ctx, g)
		}()
	}
}

func (p *poller) begin(repoID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy[repoID] {
		return false
	}
	p.busy[repoID] = true
	return true
}

func (p *poller) end(repoID string) {
	p.mu.Lock()
	delete(p.busy, repoID)
	p.mu.Unlock()
}

// report tells the Checked seam what became of a stack, when there is one to tell.
func (p *poller) report(c *polledCheck) {
	if c != nil && p.d.Checked != nil {
		p.d.Checked(c.stackID, c.outcome)
	}
}

// polledCheck is what became of one stack the poller checked.
type polledCheck struct{ stackID, outcome string }

// readRepo checks the due stacks of one repository one after the other, with one listing of its refs between them. It
// returns the last check it made without reporting it: the caller reports it once the repository is free.
func (p *poller) readRepo(ctx context.Context, g *pollGroup) (last *polledCheck) {
	list, listed := p.listRefs(ctx, g)
	for _, ps := range g.stacks {
		if ctx.Err() != nil {
			return last
		}
		head := ""
		if listed {
			head = advertisedHead(list, ps)
		}
		out, err := p.s.CheckWith(ctx, ps.EnvID, ps.StackID, CheckOptions{Trigger: TriggerPoll, RemoteHead: head})
		if ctx.Err() != nil {
			return last
		}
		result := string(out)
		failures := ps.ConsecutiveFailures
		switch {
		case err != nil:
			result = outcomeFailed
			p.s.log.Warn("could not check a git stack", "stack", ps.StackID, "error", err)
		case out == OutcomeError:
			// The run of failures was counted by the check; read what it stored.
			if st, serr := p.s.repo.GitState(ctx, ps.StackID); serr == nil {
				failures = st.ConsecutiveFailures
				if repositoryAnswered(ReasonCode(st.ReasonCode)) {
					// The repository answered; what waits is a person. A fix pushed meanwhile is deployed at the next
					// interval and not an hour later.
					failures = 0
				}
			} else {
				failures++
			}
		default:
			failures = 0
		}
		next := p.d.Clock.Now().Add(p.delay(ps.Interval, failures))
		if serr := p.s.repo.SetNextCheck(ctx, ps.StackID, next); serr != nil && ctx.Err() == nil && !errors.Is(serr, stackops.ErrNoGit) {
			p.s.log.Warn("could not schedule the next check of a git stack", "stack", ps.StackID, "error", serr)
		}
		p.report(last)
		last = &polledCheck{ps.StackID, result}
		if out == OutcomeError && !listed {
			// The repository could not even be listed and a fetch failed too: it is down, and trying the other stacks
			// of it now would only repeat the same failure. They stay due and are tried one by one on the next ticks.
			return last
		}
	}
	return last
}

// listRefs is the listing of a repository's refs: the one made less than the group's ttl ago, or a new one. listed is
// false when the listing could not be made; the stacks are then checked in full.
func (p *poller) listRefs(ctx context.Context, g *pollGroup) (gitx.RefList, bool) {
	now := p.d.Clock.Now()
	p.mu.Lock()
	e, ok := p.refs[g.repoID]
	p.mu.Unlock()
	if ok && now.Sub(e.at) < g.ttl && !now.Before(e.at) {
		return e.list, true
	}
	list, err := p.s.listRefs(ctx, g.repoID)
	if err != nil {
		if ctx.Err() == nil {
			p.s.log.Debug("could not list the refs of a repository to poll", "repository", g.repoID, "error", err)
		}
		p.mu.Lock()
		delete(p.refs, g.repoID)
		p.mu.Unlock()
		return gitx.RefList{}, false
	}
	p.mu.Lock()
	p.refs[g.repoID] = refsEntry{list: list, at: now}
	p.mu.Unlock()
	return list, true
}

// delay is how long after a check the next one is: the interval, doubled for each failure after the first up to an
// hour (an interval longer than an hour is kept as it is), plus jitter of up to a tenth of the interval and at most a
// minute.
func (p *poller) delay(interval time.Duration, failures int) time.Duration {
	wait := interval
	for i := 1; i < failures && wait < pollBackoffMax; i++ {
		wait *= 2
	}
	if wait > pollBackoffMax {
		wait = max(pollBackoffMax, interval)
	}
	spread := min(interval/10, pollJitterMax)
	return wait + time.Duration(p.d.Rand()*float64(spread))
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
