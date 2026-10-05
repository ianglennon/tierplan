// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package instance holds the instance settings every request reads: the display
// name (D-03), the public URL that drives the Origin guard and the session
// cookie's Secure flag (D-04), and the agent URL that tells agents where to dial
// when it is not the public one (D-18).
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

	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// Settings keys.
const (
	SettingName      = "instance_name"
	SettingPublicURL = "public_url"
	SettingAgentURL  = "agent_url"
)

// ErrPublicURLLocked is returned by SetTx when BAYPLAN_PUBLIC_URL fixes the
// public URL and the caller asks for a different one (D-04).
var ErrPublicURLLocked = errors.New("the public URL is fixed by BAYPLAN_PUBLIC_URL")

// ErrAgentURLLocked is returned by SetAgentURLTx when BAYPLAN_AGENT_URL fixes the
// agent URL and the caller asks for a different one (D-18).
var ErrAgentURLLocked = errors.New("the agent URL is fixed by BAYPLAN_AGENT_URL")

// Instance caches the instance name, the public URL and the agent URL.
type Instance struct {
	name     atomic.Pointer[string]
	url      atomic.Pointer[string]
	agentURL atomic.Pointer[string]
	locked   bool
	// agentLocked is separate from locked: each URL is fixed, or not, by its own
	// environment variable.
	agentLocked bool
}

// Load reads the stored settings. When BAYPLAN_PUBLIC_URL is set it wins over
// any stored value and the instance is locked: the stored value is neither read
// nor written, so removing the variable later falls back to what was stored.
func Load(ctx context.Context, s *store.Store, cfg config.Config) (*Instance, error) {
	i := &Instance{locked: cfg.PublicURL != "", agentLocked: cfg.AgentURL != ""}

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

	agent := cfg.AgentURL
	if !i.agentLocked {
		if agent, err = read(ctx, s, SettingAgentURL); err != nil {
			return nil, err
		}
	}
	i.agentURL.Store(&agent)
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

// CookieScope is the scope the session cookie is named and scoped for: whether
// the public URL is https and the path it is served under (D-50).
func (i *Instance) CookieScope() auth.CookieScope {
	return auth.CookieScope{Secure: i.Secure(), BasePath: i.BasePath()}
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

// AgentURL is the normalised agent URL, empty when none is set and agents dial the
// public URL.
func (i *Instance) AgentURL() string { return *i.agentURL.Load() }

// AgentURLLocked reports whether BAYPLAN_AGENT_URL fixes the agent URL.
func (i *Instance) AgentURLLocked() bool { return i.agentLocked }

// AgentBase is the URL agents are told to dial: the agent URL when one is set,
// else the public URL. The tunnel checks every handshake's audience against its
// host, so it must be the one the install commands point at.
func (i *Instance) AgentBase() string {
	if u := i.AgentURL(); u != "" {
		return u
	}
	return i.PublicURL()
}

// AgentEndpoint is the address an agent dials: wss:// (ws:// for an http URL), the
// host and path of AgentBase, and /agent/connect. It is "" when neither URL is
// set. It never reads a request header (D-51), so a forged Host cannot point an
// agent, and its join token, somewhere else.
func (i *Instance) AgentEndpoint() string {
	base := i.AgentBase()
	if base == "" {
		return ""
	}
	e, err := config.AgentEndpoint(base)
	if err != nil {
		return ""
	}
	return e
}

// SetAgentURLTx stores the agent URL inside the caller's write transaction, with
// "" clearing it. Like SetTx it leaves the cache alone: call ApplyAgentURL after
// the commit. When BAYPLAN_AGENT_URL fixes the URL a different value is refused
// with ErrAgentURLLocked, and the fixed one is never copied into the settings
// table, so removing the variable later falls back to what was stored.
func (i *Instance) SetAgentURLTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, agentURL string) error {
	if i.agentLocked {
		if agentURL != i.AgentURL() {
			return ErrAgentURLLocked
		}
		return nil
	}
	err := q.WithTx(tx).UpsertSetting(ctx, sqlc.UpsertSettingParams{
		Key: SettingAgentURL, Value: agentURL, UpdatedAt: time.Now().UTC().UnixNano(),
	})
	if err != nil {
		return fmt.Errorf("store %s: %w", SettingAgentURL, err)
	}
	return nil
}

// ApplyAgentURL updates the cache after the transaction that stored the value has
// committed. A locked agent URL is left as it is.
func (i *Instance) ApplyAgentURL(agentURL string) {
	if !i.agentLocked {
		i.agentURL.Store(&agentURL)
	}
}
