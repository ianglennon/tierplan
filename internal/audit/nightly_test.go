// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

func TestRunNightlyPrunesOnTheTimerAndStopsWithItsContext(t *testing.T) {
	s := storetest.New(t)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	v, err := vault.LoadOrCreate(key, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cur := now.Add(-100 * 24 * time.Hour)
	w := NewWriter(s, v, slog.New(slog.DiscardHandler), func() time.Time { return cur })
	if err := w.Record(context.Background(), Entry{
		Actor: Actor{Type: ActorUser, ID: "u", Name: "alice"}, Action: ActionAuthLogin, Result: ResultSuccess,
	}); err != nil {
		t.Fatal(err)
	}
	cur = now

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runNightly(ctx, w, 30, slog.New(slog.DiscardHandler), 20*time.Millisecond, time.Hour)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := s.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'audit.pruned'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the nightly prune did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runNightly did not stop when its context was cancelled")
	}
}
