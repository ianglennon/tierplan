// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// What the sweep of the mirrors keeps (RESEARCH Open Question 3, D-08). A mirror is a cache of one repository: every
// check fetches before it reads, and a roll back is made from the stack's stored revisions, so removing a mirror loses
// nothing and the next use builds it again.
const (
	// DefaultGCEvery is how often the mirrors are swept.
	DefaultGCEvery = 24 * time.Hour
	// DefaultMirrorUnused is how long a mirror may go unused before the sweep removes it.
	DefaultMirrorUnused = 14 * 24 * time.Hour
	// DefaultMirrorMaxBytes is the size above which the sweep removes a mirror, used or not.
	DefaultMirrorMaxBytes int64 = 2 << 30
)

// Why the sweep removed a mirror.
const (
	MirrorGone     = "repository_gone"
	MirrorUnused   = "unused"
	MirrorTooLarge = "too_large"
)

// GCDeps are what RunMirrorGC works with. A zero field takes its default.
type GCDeps struct {
	// Clock is the time the sweep's timer and its judgement of idleness run on. Nil is the wall clock.
	Clock stackops.Clock
	// Every is the period of the sweep (24 hours).
	Every time.Duration
	// Unused is how long a mirror may go unused (14 days).
	Unused time.Duration
	// MaxBytes is the largest a mirror may be (2 GiB).
	MaxBytes int64
	// Swept, when set, is told what each pass removed (nothing is a nil slice). A test waits on it instead of on a
	// length of time.
	Swept func(removed []MirrorRemoval)
}

// MirrorRemoval is a mirror the sweep removed and why.
type MirrorRemoval struct {
	ID     gitx.MirrorID
	Reason string
}

func (d GCDeps) withDefaults() GCDeps {
	if d.Clock == nil {
		d.Clock = stackops.RealClock()
	}
	if d.Every <= 0 {
		d.Every = DefaultGCEvery
	}
	if d.Unused <= 0 {
		d.Unused = DefaultMirrorUnused
	}
	if d.MaxBytes <= 0 {
		d.MaxBytes = DefaultMirrorMaxBytes
	}
	return d
}

// RunMirrorGC sweeps the mirrors once and then every d.Every until ctx ends. A mirror is removed when its repository
// no longer exists, when nothing has fetched or read it for d.Unused, or when it is larger than d.MaxBytes. A service
// whose git client does not keep mirrors on disk does nothing.
func (s *Service) RunMirrorGC(ctx context.Context, d GCDeps) {
	d = d.withDefaults()
	if s.mirrors == nil {
		return
	}
	for {
		removed, err := s.SweepMirrors(ctx, d)
		if err != nil && ctx.Err() == nil {
			s.log.Error("could not sweep the git mirrors", "error", err)
		}
		if d.Swept != nil {
			d.Swept(removed)
		}
		select {
		case <-ctx.Done():
			return
		case <-d.Clock.After(d.Every):
		}
	}
}

// SweepMirrors makes one pass and answers the mirrors it removed, in ID order. A mirror that cannot be measured or
// removed is logged and left for the next pass.
func (s *Service) SweepMirrors(ctx context.Context, d GCDeps) ([]MirrorRemoval, error) {
	d = d.withDefaults()
	if s.mirrors == nil {
		return nil, nil
	}
	// The mirrors are listed before the repositories: a repository saved after the listing has no mirror in it, and
	// one whose mirror was made after it is saved is in the repository list read next.
	mirrors, err := s.mirrors.ListMirrors()
	if err != nil {
		return nil, fmt.Errorf("gitsync: list the mirrors: %w", err)
	}
	repos, err := s.conns.ListRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("gitsync: list the repositories: %w", err)
	}
	live := make(map[string]bool, len(repos))
	for _, r := range repos {
		live[r.ID] = true
	}
	slices.Sort(mirrors)
	now := d.Clock.Now()
	var removed []MirrorRemoval
	for _, m := range mirrors {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		reason := ""
		if !live[string(m)] {
			reason = MirrorGone
		} else {
			size, last, err := s.mirrors.MirrorStats(m)
			if err != nil {
				s.log.Warn("could not measure a git mirror", "mirror", string(m), "error", err)
				continue
			}
			switch {
			case size > d.MaxBytes:
				reason = MirrorTooLarge
			case now.Sub(last) >= d.Unused:
				reason = MirrorUnused
			}
		}
		if reason == "" {
			continue
		}
		if err := s.mirrors.RemoveMirror(m); err != nil {
			s.log.Warn("could not remove a git mirror", "mirror", string(m), "error", err)
			continue
		}
		removed = append(removed, MirrorRemoval{ID: m, Reason: reason})
	}
	return removed, nil
}
