// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ianglennon/coaming/internal/auth"
	"github.com/ianglennon/coaming/internal/config"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/store/sqlc"
)

// Settings keys.
const (
	SettingName      = "instance_name"
	SettingPublicURL = "public_url"
	SettingAgentURL  = "agent_url"

	// SettingShellIdleMinutes and SettingShellMaxPerUser bound the browser shells
	// (D-29, D-31): how long one may sit unused, and how many one person may hold.
	SettingShellIdleMinutes = "shell_idle_minutes"
	SettingShellMaxPerUser  = "shell_max_per_user"

	// SettingStackRevisionsKept is how many revisions of each stack Coaming keeps (D-01): the
	// newest N, plus the deployed one and any still deploying.
	SettingStackRevisionsKept = "stack_revisions_kept"
)

// The browser-shell limits: the defaults, and the ranges a stored or submitted value
// must lie in. A stored value outside its range is read as the default.
const (
	DefaultShellIdleMinutes = 30
	MinShellIdleMinutes     = 5
	MaxShellIdleMinutes     = 240
	DefaultShellMaxPerUser  = 10
	MinShellMaxPerUser      = 1
	MaxShellMaxPerUser      = 50

	// The stack-history retention: how many revisions of each stack are kept.
	DefaultStackRevisionsKept = 20
	MinStackRevisionsKept     = 5
	MaxStackRevisionsKept     = 200
)

// ErrPublicURLLocked is returned by SetTx when COAMING_PUBLIC_URL fixes the
// public URL and the caller asks for a different one (D-04).
var ErrPublicURLLocked = errors.New("the public URL is fixed by COAMING_PUBLIC_URL")

// ErrAgentURLLocked is returned by SetAgentURLTx when COAMING_AGENT_URL fixes the
// agent URL and the caller asks for a different one (D-18).
var ErrAgentURLLocked = errors.New("the agent URL is fixed by COAMING_AGENT_URL")

// Instance caches the instance name, the public URL and the agent URL.
type Instance struct {
	name     atomic.Pointer[string]
	url      atomic.Pointer[string]
	agentURL atomic.Pointer[string]
	locked   bool
	// agentLocked is separate from locked: each URL is fixed, or not, by its own
	// environment variable.
	agentLocked bool

	shellIdle atomic.Int64 // minutes
	shellCap  atomic.Int64

	revisionsKept atomic.Int64
}

// Load reads the stored settings. When COAMING_PUBLIC_URL is set it wins over
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

	idle, err := readInt(ctx, s, SettingShellIdleMinutes, DefaultShellIdleMinutes, MinShellIdleMinutes, MaxShellIdleMinutes)
	if err != nil {
		return nil, err
	}
	capN, err := readInt(ctx, s, SettingShellMaxPerUser, DefaultShellMaxPerUser, MinShellMaxPerUser, MaxShellMaxPerUser)
	if err != nil {
		return nil, err
	}
	i.ApplyShellLimits(idle, capN)

	kept, err := readInt(ctx, s, SettingStackRevisionsKept, DefaultStackRevisionsKept, MinStackRevisionsKept, MaxStackRevisionsKept)
	if err != nil {
		return nil, err
	}
	i.ApplyStackRevisionsKept(kept)
	return i, nil
}

// readInt reads a whole-number setting, and gives def when it is not stored, is not
// a number, or lies outside lo..hi.
func readInt(ctx context.Context, s *store.Store, key string, def, lo, hi int) (int, error) {
	v, err := read(ctx, s, key)
	if err != nil {
		return 0, err
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(v))
	if convErr != nil || n < lo || n > hi {
		return def, nil
	}
	return n, nil
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

// BasePath is the escaped path of the public URL, for example "/coaming" for
// https://example.com/coaming, and "" when there is no public URL or it has no
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

// Locked reports whether COAMING_PUBLIC_URL fixes the public URL.
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

// AgentURLLocked reports whether COAMING_AGENT_URL fixes the agent URL.
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
// the commit. When COAMING_AGENT_URL fixes the URL a different value is refused
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

// ShellIdle is how long a browser shell may go without input or output before it is
// closed (D-29). It is read at every use, so a change applies to open shells at their
// next idle check.
func (i *Instance) ShellIdle() time.Duration {
	return time.Duration(i.shellIdle.Load()) * time.Minute
}

// ShellCap is the most open or pending shells one person may hold across all
// environments (D-31).
func (i *Instance) ShellCap() int { return int(i.shellCap.Load()) }

// SetShellLimitsTx stores the idle limit in minutes and the per-person cap inside the
// caller's write transaction. Like SetTx it leaves the cache alone: call
// ApplyShellLimits after the commit. A value outside its range is refused.
func (i *Instance) SetShellLimitsTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, idleMinutes, maxPerUser int) error {
	if idleMinutes < MinShellIdleMinutes || idleMinutes > MaxShellIdleMinutes {
		return fmt.Errorf("shell idle minutes %d is outside %d to %d", idleMinutes, MinShellIdleMinutes, MaxShellIdleMinutes)
	}
	if maxPerUser < MinShellMaxPerUser || maxPerUser > MaxShellMaxPerUser {
		return fmt.Errorf("shells per user %d is outside %d to %d", maxPerUser, MinShellMaxPerUser, MaxShellMaxPerUser)
	}
	now := time.Now().UTC().UnixNano()
	qt := q.WithTx(tx)
	if err := qt.UpsertSetting(ctx, sqlc.UpsertSettingParams{Key: SettingShellIdleMinutes, Value: strconv.Itoa(idleMinutes), UpdatedAt: now}); err != nil {
		return fmt.Errorf("store %s: %w", SettingShellIdleMinutes, err)
	}
	if err := qt.UpsertSetting(ctx, sqlc.UpsertSettingParams{Key: SettingShellMaxPerUser, Value: strconv.Itoa(maxPerUser), UpdatedAt: now}); err != nil {
		return fmt.Errorf("store %s: %w", SettingShellMaxPerUser, err)
	}
	return nil
}

// ApplyShellLimits updates the cache after the transaction that stored the limits has
// committed. A value outside its range is ignored, so the cache never holds one.
func (i *Instance) ApplyShellLimits(idleMinutes, maxPerUser int) {
	if idleMinutes >= MinShellIdleMinutes && idleMinutes <= MaxShellIdleMinutes {
		i.shellIdle.Store(int64(idleMinutes))
	}
	if maxPerUser >= MinShellMaxPerUser && maxPerUser <= MaxShellMaxPerUser {
		i.shellCap.Store(int64(maxPerUser))
	}
}

// StackRevisionsKept is how many revisions of each stack are kept (D-01). It is read at every
// prune, so a change applies to the next deploy that ends. A zero Instance reads the default.
func (i *Instance) StackRevisionsKept() int {
	if n := int(i.revisionsKept.Load()); n != 0 {
		return n
	}
	return DefaultStackRevisionsKept
}

// SetStackRevisionsKeptTx stores how many revisions of each stack are kept inside the caller's
// write transaction. Like SetTx it leaves the cache alone: call ApplyStackRevisionsKept after the
// commit. A value outside its range is refused.
func (i *Instance) SetStackRevisionsKeptTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, n int) error {
	if n < MinStackRevisionsKept || n > MaxStackRevisionsKept {
		return fmt.Errorf("revisions kept %d is outside %d to %d", n, MinStackRevisionsKept, MaxStackRevisionsKept)
	}
	err := q.WithTx(tx).UpsertSetting(ctx, sqlc.UpsertSettingParams{
		Key: SettingStackRevisionsKept, Value: strconv.Itoa(n), UpdatedAt: time.Now().UTC().UnixNano(),
	})
	if err != nil {
		return fmt.Errorf("store %s: %w", SettingStackRevisionsKept, err)
	}
	return nil
}

// ApplyStackRevisionsKept updates the cache after the transaction that stored the value has
// committed. A value outside its range is ignored, so the cache never holds one.
func (i *Instance) ApplyStackRevisionsKept(n int) {
	if n >= MinStackRevisionsKept && n <= MaxStackRevisionsKept {
		i.revisionsKept.Store(int64(n))
	}
}
