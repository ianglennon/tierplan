// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package updates is TierPlan's own knowledge about image updates: what each registry last answered
// when asked for the digest of a tag, and the verdict that answer gives for each workload that runs
// the image. Docker stays the source of truth (D-04): nothing here is a copy of what runs. A stored
// check is matched to the digest a workload runs live, every time a verdict is asked for.
//
// The package is split as the janitor is. Judge is pure: a workload, a stored check, a clock and a
// bound go in, a Verdict comes out, so the order of its rules is a table test. The Checker asks the
// registries through the Heads port and saves what they answer through the Store port. The Service
// ties both to the environments' own reads.
//
// A check is keyed by registry host, repository, tag and the credential that asked (D-19), so an
// environment with a credential of its own never answers for one without, and with one instance
// credential the whole fleet shares one row. A row holds no credential and none of the registry's own
// words: a failure is stored as a Reason mapped from the type of the error.
package updates

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// Key names one check: the registry, the repository below it, the tag, and the registry credential the
// check ran with, "" for an anonymous one.
type Key struct {
	Host, Repo, Tag, CredID string
}

// Check is what a registry last answered for a Key.
type Check struct {
	Key
	// Digest is the digest the tag pointed at when the last check succeeded, "" before any did.
	Digest string
	// Index is true when Digest names a multi-platform index; Platforms then maps each platform
	// (linux/amd64) to its manifest digest.
	Index     bool
	Platforms map[string]string
	// CheckedAt is when the last check ran, whatever came of it; SuccessAt when the last successful
	// one did, zero before any did.
	CheckedAt, SuccessAt time.Time
	// Error is why the last check failed, "" when it succeeded.
	Error Reason
}

// Store keeps checks.
type Store interface {
	// Checks returns the stored checks of keys. A key with no check is absent from the answer.
	Checks(ctx context.Context, keys []Key) (map[Key]Check, error)
	// SaveSuccess stores a check that got an answer, clearing any earlier error.
	SaveSuccess(ctx context.Context, c Check) error
	// SaveFailure stores that the check of key failed at the time given, keeping the digest, platforms
	// and success time of the last good answer.
	SaveFailure(ctx context.Context, key Key, at time.Time, r Reason) error
}

// SQLStore is the Store over SQLite. Q runs on the write connection W: single statements go through it,
// and a later plan that writes several rows together begins its transaction on W.
type SQLStore struct {
	Q *sqlc.Queries
	W *sql.DB
}

type hostRepo struct{ host, repo string }

// Checks implements Store. It reads each repository once and keeps the keys asked for, because a list
// of composite keys cannot be bound in a query.
func (s SQLStore) Checks(ctx context.Context, keys []Key) (map[Key]Check, error) {
	want := make(map[Key]bool, len(keys))
	repos := map[hostRepo]bool{}
	for _, k := range keys {
		want[k] = true
		repos[hostRepo{k.Host, k.Repo}] = true
	}
	out := make(map[Key]Check, len(keys))
	for hr := range repos {
		rows, err := s.Q.GetImageChecks(ctx, sqlc.GetImageChecksParams{Host: hr.host, Repo: hr.repo})
		if err != nil {
			return nil, fmt.Errorf("read the image checks of %s/%s: %w", hr.host, hr.repo, err)
		}
		for _, r := range rows {
			k := Key{Host: r.Host, Repo: r.Repo, Tag: r.Tag, CredID: r.CredID}
			if !want[k] {
				continue
			}
			out[k] = Check{
				Key: k, Digest: r.Digest, Index: r.IsIndex != 0, Platforms: decodePlatforms(r.Platforms),
				CheckedAt: fromNanos(r.CheckedAt), SuccessAt: fromNanos(r.SuccessAt), Error: Reason(r.ErrorCode),
			}
		}
	}
	return out, nil
}

// SaveSuccess implements Store.
func (s SQLStore) SaveSuccess(ctx context.Context, c Check) error {
	platforms, err := json.Marshal(c.Platforms)
	if err != nil || c.Platforms == nil {
		platforms = []byte("{}")
	}
	var index int64
	if c.Index {
		index = 1
	}
	if err := s.Q.UpsertImageCheckSuccess(ctx, sqlc.UpsertImageCheckSuccessParams{
		Host: c.Host, Repo: c.Repo, Tag: c.Tag, CredID: c.CredID, Digest: c.Digest, IsIndex: index,
		Platforms: string(platforms), CheckedAt: c.CheckedAt.UnixNano(), SuccessAt: c.SuccessAt.UnixNano(),
	}); err != nil {
		return fmt.Errorf("save the image check of %s/%s:%s: %w", c.Host, c.Repo, c.Tag, err)
	}
	return nil
}

// SaveFailure implements Store.
func (s SQLStore) SaveFailure(ctx context.Context, key Key, at time.Time, r Reason) error {
	if err := s.Q.UpsertImageCheckFailure(ctx, sqlc.UpsertImageCheckFailureParams{
		Host: key.Host, Repo: key.Repo, Tag: key.Tag, CredID: key.CredID, CheckedAt: at.UnixNano(), ErrorCode: string(r),
	}); err != nil {
		return fmt.Errorf("save the failed image check of %s/%s:%s: %w", key.Host, key.Repo, key.Tag, err)
	}
	return nil
}

// decodePlatforms reads the platform map of a row. A map that cannot be read is empty: it is only ever
// written by SaveSuccess, and an empty one makes a verdict fall back to the tag's own digest.
func decodePlatforms(s string) map[string]string {
	m := map[string]string{}
	if err := json.Unmarshal([]byte(s), &m); err != nil || m == nil {
		return map[string]string{}
	}
	return m
}

// fromNanos turns stored UTC unix nanoseconds into a time, the zero time for 0.
func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}
