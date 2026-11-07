// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package instance

import (
	"context"
	"log/slog"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
)

// SettingsPollInterval is how often the running server re-reads the settings table
// to see a change another process made (D-05). Three seconds is the longest an
// operator waits for `tierplan https disable` to reach a running server.
const SettingsPollInterval = 3 * time.Second

// Clock is the time the watcher waits on. It is declared here so this package does
// not import stackops; stackops.RealClock() satisfies it.
type Clock interface {
	// Now is the current time.
	Now() time.Time
	// After delivers the time once d has passed.
	After(d time.Duration) <-chan time.Time
}

// Watcher re-reads the instance settings on a clock, so a value another process
// wrote reaches the running server without a restart (D-05). It pushes nothing to
// agents: connected wss agents stay valid, and agents accept only wss pushes.
//
// Polling the cached rows converges rather than locks: a poll that raced an API
// write may apply a stale value for one interval, and the next poll restores the
// database's (RESEARCH Pitfall 7).
type Watcher struct {
	Inst  *Instance
	Store *store.Store
	Clock Clock
	// Every is the poll interval; zero means SettingsPollInterval.
	Every time.Duration
	// Log defaults to slog.Default().
	Log *slog.Logger
}

// Poll re-reads the settings once and returns the keys that changed. A change is
// logged at Info with the keys only, never the values; a failed read is logged at
// Warn and applies nothing, so the server keeps serving the last good values.
func (w *Watcher) Poll(ctx context.Context) ([]string, error) {
	changed, err := w.Inst.Reload(ctx, w.Store)
	if err != nil {
		w.log().Warn("settings poll failed; keeping the current values", "error", err)
		return nil, err
	}
	if len(changed) > 0 {
		w.log().Info("settings changed outside this process; applied", "keys", changed)
	}
	return changed, nil
}

// Run polls each time the clock fires and returns when ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	every := w.Every
	if every <= 0 {
		every = SettingsPollInterval
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.Clock.After(every):
			_, _ = w.Poll(ctx)
		}
	}
}

func (w *Watcher) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}
