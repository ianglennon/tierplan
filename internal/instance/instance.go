// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package instance holds the instance settings every request reads: the display
// name (D-03) and the public URL that drives the Origin guard and the session
// cookie's Secure flag (D-04).
//
// The values live in the settings table. This package caches them in atomics so
// the Origin guard, which runs on every mutation, never touches the database,
// and so a change takes effect at once without a restart.
package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	neturl "net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// Settings keys.
const (
	SettingName      = "instance_name"
	SettingPublicURL = "public_url"
)

// ErrPublicURLLocked is returned by SetTx when BAYPLAN_PUBLIC_URL fixes the
// public URL and the caller asks for a different one (D-04).
var ErrPublicURLLocked = errors.New("the public URL is fixed by BAYPLAN_PUBLIC_URL")

// Instance caches the instance name and public URL.
type Instance struct {
	name   atomic.Pointer[string]
	url    atomic.Pointer[string]
	locked bool
}

// Load reads the stored settings. When BAYPLAN_PUBLIC_URL is set it wins over
// any stored value and the instance is locked: the stored value is neither read
// nor written, so removing the variable later falls back to what was stored.
func Load(ctx context.Context, s *store.Store, cfg config.Config) (*Instance, error) {
	i := &Instance{locked: cfg.PublicURL != ""}

	name, err := read(ctx, s, SettingName)
	if err != nil {
		return nil, err
	}
	i.name.Store(&name)

	url := cfg.PublicURL
	if !i.locked {
		if url, err = read(ctx, s, SettingPublicURL); err != nil {
			return nil, err
		}
	}
	i.url.Store(&url)
	return i, nil
}

func read(ctx context.Context, s *store.Store, key string) (string, error) {
	v, err := s.RQ.GetSetting(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read setting %s: %w", key, err)
	}
	return v, nil
}

// Name is the display name, empty before setup completes.
func (i *Instance) Name() string { return *i.name.Load() }

// PublicURL is the normalised public URL, empty when none is set.
func (i *Instance) PublicURL() string { return *i.url.Load() }

// PublicOrigin is the scheme, host and port of the public URL, or "" when none
// is set. httpx.Options.PublicOrigin reads it on every request.
func (i *Instance) PublicOrigin() string { return httpx.OriginOf(i.PublicURL()) }

// Secure reports whether the public URL is https, which decides the session
// cookie's Secure flag and name (D-12).
func (i *Instance) Secure() bool {
	return strings.HasPrefix(i.PublicOrigin(), "https://")
}

// BasePath is the escaped path of the public URL, for example "/bayplan" for
// https://example.com/bayplan, and "" when there is no public URL or it has no
// path. It is the single source of the base every link, asset URL and cookie
// scope derives from (D-50, D-51), and is read per request so a change takes
// effect at once. PublicURL is already free of a trailing slash.
func (i *Instance) BasePath() string {
	u, err := neturl.Parse(i.PublicURL())
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.EscapedPath(), "/")
}

// Locked reports whether BAYPLAN_PUBLIC_URL fixes the public URL.
func (i *Instance) Locked() bool { return i.locked }

// SetTx stores the name, and the public URL unless it is locked, inside the
// caller's write transaction. It does not touch the cache: call Apply after the
// transaction commits, so a rollback leaves the cache and the database in step.
// When the URL is locked, a publicURL other than the fixed one is refused with
// ErrPublicURLLocked and nothing is written.
func (i *Instance) SetTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, name, publicURL string) error {
	if i.locked && publicURL != i.PublicURL() {
		return ErrPublicURLLocked
	}
	now := time.Now().UTC().UnixNano()
	qt := q.WithTx(tx)
	if err := qt.UpsertSetting(ctx, sqlc.UpsertSettingParams{Key: SettingName, Value: name, UpdatedAt: now}); err != nil {
		return fmt.Errorf("store %s: %w", SettingName, err)
	}
	if i.locked {
		return nil
	}
	if err := qt.UpsertSetting(ctx, sqlc.UpsertSettingParams{Key: SettingPublicURL, Value: publicURL, UpdatedAt: now}); err != nil {
		return fmt.Errorf("store %s: %w", SettingPublicURL, err)
	}
	return nil
}

// Apply updates the cache after the transaction that stored the values has
// committed. A locked public URL is left as it is.
func (i *Instance) Apply(name, publicURL string) {
	i.name.Store(&name)
	if !i.locked {
		i.url.Store(&publicURL)
	}
}
