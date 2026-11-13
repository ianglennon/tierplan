// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// hstsAuditSink captures the rows a policy writes.
type hstsAuditSink struct {
	mu   sync.Mutex
	rows []audit.Entry
	err  error
}

func (s *hstsAuditSink) record(_ context.Context, e audit.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, e)
	return s.err
}

func (s *hstsAuditSink) all() []audit.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]audit.Entry(nil), s.rows...)
}

// audited is a policy over the rig's store with a capture sink already set.
func (g *hstsRig) audited() (*hstsPolicy, *hstsAuditSink) {
	p := g.policy(nil)
	s := &hstsAuditSink{}
	p.SetAudit(s.record)
	return p, s
}

// wantRows fails unless the sink holds exactly the actions given, in order.
func wantRows(t *testing.T, s *hstsAuditSink, actions ...string) []audit.Entry {
	t.Helper()
	rows := s.all()
	if len(rows) != len(actions) {
		t.Fatalf("audit rows = %d %v, want %v", len(rows), rowActions(rows), actions)
	}
	for i, a := range actions {
		if rows[i].Action != a {
			t.Fatalf("audit row %d = %q, want %q", i, rows[i].Action, a)
		}
	}
	return rows
}

func rowActions(rows []audit.Entry) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Action
	}
	return out
}

// TestHSTSAudit: the policy's own withdrawal ahead of a certificate's end, and its return after a
// renewal, are one system:acme row each, and nothing else it does is a row (D-10, Pitfall 8).
func TestHSTSAudit(t *testing.T) {

	// A 90-day certificate with 30 days left: the window opens 23 days on.
	const (
		left   = 30 * 24 * time.Hour
		window = 23 * 24 * time.Hour
	)
	sendingOver := func(g *hstsRig) (*hstsPolicy, *hstsAuditSink, time.Time) {
		notAfter := hstsStart.Add(left)
		g.serve(tlsKindACME, g.leaf(notAfter.Add(-90*24*time.Hour), notAfter, hstsHost))
		g.setStored(true)
		p, s := g.audited()
		if got := g.header(p, hstsHost); got != "max-age=300" {
			t.Fatalf("header = %q, want max-age=300", got)
		}
		return p, s, notAfter
	}

	t.Run("withdrawn once", func(t *testing.T) {
		g := newHSTSRig(t)
		p, s, notAfter := sendingOver(g)
		wantRows(t, s)

		g.clock.Advance(window)
		if got := g.header(p, hstsHost); got != "max-age=0" {
			t.Fatalf("header in the window = %q, want max-age=0", got)
		}
		rows := wantRows(t, s, audit.ActionHTTPSHSTSWithdrawn)
		r := rows[0]
		if r.Actor != audit.SystemACME || r.TargetType != "https" || r.TargetID != "https" || r.Result != audit.ResultSuccess {
			t.Errorf("row = %+v, want system:acme, target https, success", r)
		}
		if r.Details["source"] != "lets_encrypt" || r.Details["not_after"] != notAfter.UTC().Format(time.RFC3339) || len(r.Details) != 2 {
			t.Errorf("details = %v, want not_after %s and source lets_encrypt only", r.Details, notAfter.UTC().Format(time.RFC3339))
		}

		// A second and a third recompute inside the window add nothing.
		g.clock.Advance(time.Hour)
		g.header(p, hstsHost)
		p.Recompute(testsupport.Context(t))
		wantRows(t, s, audit.ActionHTTPSHSTSWithdrawn)
	})

	t.Run("restored by a renewed certificate", func(t *testing.T) {
		g := newHSTSRig(t)
		p, s, _ := sendingOver(g)
		g.clock.Advance(window)
		g.header(p, hstsHost)

		renewed := g.clock.Now().Add(90 * 24 * time.Hour)
		g.serve(tlsKindACME, g.leaf(g.clock.Now(), renewed, hstsHost))
		if got := g.header(p, hstsHost); got != "max-age=300" {
			t.Fatalf("header after the renewal = %q, want max-age=300", got)
		}
		rows := wantRows(t, s, audit.ActionHTTPSHSTSWithdrawn, audit.ActionHTTPSHSTSRestored)
		r := rows[1]
		if r.Actor != audit.SystemACME || r.Result != audit.ResultSuccess {
			t.Errorf("row = %+v, want system:acme, success", r)
		}
		if r.Details["not_after"] != renewed.UTC().Format(time.RFC3339) || r.Details["source"] != "lets_encrypt" {
			t.Errorf("details = %v, want the renewed not_after %s", r.Details, renewed.UTC().Format(time.RFC3339))
		}
	})

	t.Run("a restart inside the window writes nothing", func(t *testing.T) {
		g := newHSTSRig(t)
		p, s, _ := sendingOver(g)
		g.clock.Advance(window)
		g.header(p, hstsHost)
		wantRows(t, s, audit.ActionHTTPSHSTSWithdrawn)

		// A new policy over the same database is a restart: its first decision has nothing to compare with.
		p2, s2 := g.audited()
		if got := g.header(p2, hstsHost); got != "max-age=0" {
			t.Fatalf("header after the restart = %q, want max-age=0", got)
		}
		g.header(p2, hstsHost)
		wantRows(t, s2)

		// Its first return is a transition and is recorded.
		g.serve(tlsKindACME, g.leaf(g.clock.Now(), g.clock.Now().Add(90*24*time.Hour), hstsHost))
		g.header(p2, hstsHost)
		wantRows(t, s2, audit.ActionHTTPSHSTSRestored)
	})

	t.Run("ramp steps write nothing", func(t *testing.T) {
		g := newHSTSRig(t)
		notAfter := hstsStart.Add(365 * 24 * time.Hour)
		g.serve(tlsKindACME, g.leaf(hstsStart.Add(-24*time.Hour), notAfter, hstsHost))
		g.setStored(true)
		p, s := g.audited()
		for _, want := range []string{"max-age=300", "max-age=604800", "max-age=2592000", "max-age=31536000"} {
			if got := g.header(p, hstsHost); got != want {
				t.Fatalf("header = %q, want %q", got, want)
			}
			g.clock.Advance(31 * 24 * time.Hour)
		}
		wantRows(t, s)
	})

	t.Run("the admin's toggle off writes nothing", func(t *testing.T) {
		g := newHSTSRig(t)
		p, s, _ := sendingOver(g)
		if err := p.SetEnabled(testsupport.Context(t), false); err != nil {
			t.Fatal(err)
		}
		if d := p.snap.Load(); d.kind != hstsWithdraw || d.reason != "toggle_off" {
			t.Fatalf("decision = %+v, want a toggle_off withdrawal", d)
		}
		wantRows(t, s)
		if err := p.SetEnabled(testsupport.Context(t), true); err != nil {
			t.Fatal(err)
		}
		wantRows(t, s)
	})

	t.Run("on inside the window writes nothing", func(t *testing.T) {
		g := newHSTSRig(t)
		notAfter := hstsStart.Add(5 * 24 * time.Hour)
		g.serve(tlsKindACME, g.leaf(notAfter.Add(-90*24*time.Hour), notAfter, hstsHost))
		p, s := g.audited()
		if got := g.header(p, hstsHost); got != "max-age=0" {
			t.Fatalf("header with the toggle off = %q, want max-age=0", got)
		}
		if err := p.SetEnabled(testsupport.Context(t), true); err != nil {
			t.Fatal(err)
		}
		if d := p.snap.Load(); d.kind != hstsWithdraw || d.reason != "withdrawn" {
			t.Fatalf("decision = %+v, want a withdrawn decision", d)
		}
		// It was never being sent, so nothing was withdrawn.
		wantRows(t, s)
	})

	t.Run("a failed write changes no decision", func(t *testing.T) {
		g := newHSTSRig(t)
		p, s, _ := sendingOver(g)
		s.err = errors.New("the audit log is read-only")
		g.clock.Advance(window)
		if got := g.header(p, hstsHost); got != "max-age=0" {
			t.Fatalf("header = %q, want max-age=0 although the row could not be written", got)
		}
		if len(s.all()) != 1 {
			t.Errorf("rows tried = %d, want 1", len(s.all()))
		}
	})

	t.Run("no sink writes nothing and does not panic", func(t *testing.T) {
		g := newHSTSRig(t)
		notAfter := hstsStart.Add(left)
		g.serve(tlsKindACME, g.leaf(notAfter.Add(-90*24*time.Hour), notAfter, hstsHost))
		g.setStored(true)
		p := g.policy(nil)
		g.header(p, hstsHost)
		g.clock.Advance(window)
		if got := g.header(p, hstsHost); got != "max-age=0" {
			t.Fatalf("header = %q, want max-age=0", got)
		}
	})
}
