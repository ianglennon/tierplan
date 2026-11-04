// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// What a person asked TierPlan to stop offering image updates for (IMG-08, D-15, D-16). A skip passes over
// one remote version of one workload and lifts itself when the tag points at another digest; an ignore
// hides every update of an image in one environment or of one workload. Nothing is ever ignored across
// environments: every row belongs to one environment and is listed and matched there alone.

// The mode of an Ignore.
const (
	// ModeSkip passes over the remote digest the row holds, for one workload.
	ModeSkip = "skip"
	// ModeIgnore hides every update of what the row names.
	ModeIgnore = "ignore"
)

// The scope of an Ignore.
const (
	// ScopeImage is a repository and tag (host, repo, tag) in one environment.
	ScopeImage = "image"
	// ScopeWorkload is one service or container (kind, name) in one environment.
	ScopeWorkload = "workload"
	// ScopeLabel is the tierplan.updates=ignore label on the workload itself. It is never stored: Match
	// builds it from the workload's labels.
	ScopeLabel = "label"
)

// Ignore is one skip or ignore.
type Ignore struct {
	ID, EnvID string
	// Mode is ModeSkip or ModeIgnore; Scope is ScopeImage, ScopeWorkload or, for the label, ScopeLabel.
	Mode, Scope string
	// Host, Repo and Tag name the image of an image-scoped row, as an image check keys it.
	Host, Repo, Tag string
	// Kind ("service" or "container") and Name name the workload of a workload-scoped row.
	Kind, Name string
	// Digest is the remote digest a skip passes over; empty for an ignore.
	Digest string
	// SetBy is the user's ID; SetAt when it was set.
	SetBy string
	SetAt time.Time
	// ByLabel is true for the tierplan.updates=ignore label, which is not a stored row.
	ByLabel bool
}

// IgnoreStore keeps skips and ignores in SQLite. Q runs on the write connection W, like SQLStore.
type IgnoreStore struct {
	Q *sqlc.Queries
	W *sql.DB
}

// List returns the skips and ignores of one environment.
func (s IgnoreStore) List(ctx context.Context, envID string) (Ignores, error) {
	rows, err := s.Q.ListImageUpdateIgnores(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("read the image update ignores of an environment: %w", err)
	}
	out := make(Ignores, 0, len(rows))
	for _, r := range rows {
		out = append(out, Ignore{
			ID: r.ID, EnvID: r.EnvID, Mode: r.Mode, Scope: r.Scope, Host: r.Host, Repo: r.Repo, Tag: r.Tag,
			Kind: r.Kind, Name: r.Name, Digest: r.Digest, SetBy: r.SetBy, SetAt: fromNanos(r.SetAt),
		})
	}
	return out, nil
}

// Add saves a skip or an ignore. Setting the same one again replaces who set it, when and the digest a
// skip passes over. The ID is made here when ig has none.
func (s IgnoreStore) Add(ctx context.Context, ig Ignore) error {
	if ig.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("make an ID for an image update ignore: %w", err)
		}
		ig.ID = id.String()
	}
	if err := s.Q.InsertImageUpdateIgnore(ctx, sqlc.InsertImageUpdateIgnoreParams{
		ID: ig.ID, EnvID: ig.EnvID, Mode: ig.Mode, Scope: ig.Scope, Host: ig.Host, Repo: ig.Repo, Tag: ig.Tag,
		Kind: ig.Kind, Name: ig.Name, Digest: ig.Digest, SetBy: ig.SetBy, SetAt: ig.SetAt.UnixNano(),
	}); err != nil {
		return fmt.Errorf("save an image update %s: %w", ig.Mode, err)
	}
	return nil
}

// Remove deletes the skip or ignore of an environment that key names (its mode, scope and the image or
// workload it covers) and reports whether there was one.
func (s IgnoreStore) Remove(ctx context.Context, envID string, key Ignore) (bool, error) {
	n, err := s.Q.DeleteImageUpdateIgnore(ctx, sqlc.DeleteImageUpdateIgnoreParams{
		EnvID: envID, Mode: key.Mode, Scope: key.Scope, Host: key.Host, Repo: key.Repo, Tag: key.Tag,
		Kind: key.Kind, Name: key.Name,
	})
	if err != nil {
		return false, fmt.Errorf("remove an image update %s: %w", key.Mode, err)
	}
	return n > 0, nil
}

// Ignores are the skips and ignores of one environment, as List returns them.
type Ignores []Ignore

// Match answers the one rule that applies to a workload of this environment, nil when none does: an
// ignore of the workload (its kind and name), else an ignore of its image (host, repo and tag of w.Ref),
// else a skip of the workload whose digest equals remoteDigest. A skip stops applying as soon as the
// remote digest is another one. Rows of other environments are never in the list, and a row whose
// scope fields do not match the workload exactly never applies to it.
func (ig Ignores) Match(w Workload, remoteDigest string) *Ignore {
	var image, skip *Ignore
	for i := range ig {
		r := &ig[i]
		if r.EnvID != "" && w.EnvID != "" && r.EnvID != w.EnvID {
			continue
		}
		switch {
		case r.Mode == ModeIgnore && r.Scope == ScopeWorkload && r.Kind == w.Kind && r.Name == w.Name:
			return r
		case r.Mode == ModeIgnore && r.Scope == ScopeImage && w.Ref.Tag != "" &&
			r.Host == w.Ref.Domain && r.Repo == w.Ref.Path && r.Tag == w.Ref.Tag:
			if image == nil {
				image = r
			}
		case r.Mode == ModeSkip && r.Scope == ScopeWorkload && r.Kind == w.Kind && r.Name == w.Name &&
			remoteDigest != "" && r.Digest == remoteDigest:
			if skip == nil {
				skip = r
			}
		}
	}
	if image != nil {
		return image
	}
	return skip
}
