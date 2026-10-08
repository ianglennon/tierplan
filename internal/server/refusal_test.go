// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/server"
	"github.com/ianglennon/bayplan/internal/server/servertest"
)

// Plan 03-10, D-20: a signed-in user's identical refusals cost one full audit row
// now and one "refused xN" row when the minute closes.

// stepClock is a clock a test moves by hand.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestRepeatedRefusalsAreCoalescedIntoOneEntry(t *testing.T) {
	fl := pflag.NewFlagSet("refusal", pflag.ContinueOnError)
	config.BindFlags(fl)
	env := map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data"}
	cfg, err := config.Load(fl, func(k string) string { return env[k] }, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	// The bell ages bursts by the real clock, so the fake one starts there.
	clk := &stepClock{t: time.Now()}
	srv, err := server.Build(context.Background(), cfg, slog.New(slog.DiscardHandler), server.WithRefusalClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ids := servertest.SeedArchetypes(t, srv.Store(), envA, envB)
	h := srv.Handler()

	get := func(a servertest.Archetype, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
		req.Header.Set("User-Agent", "refusal-test")
		req.RemoteAddr = "192.0.2.77:40000"
		cookie, _ := servertest.SessionFor(t, srv, ids[a])
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	type row struct {
		actor, target, reason, env string
		details                    map[string]string
	}
	rows := func() []row {
		t.Helper()
		q, err := srv.Store().Read.Query(`SELECT actor_id, target_id, reason, COALESCE(env_id, ''), details FROM audit_log WHERE action = ? ORDER BY seq`, audit.ActionPermissionDenied)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = q.Close() }()
		var out []row
		for q.Next() {
			var r row
			var details string
			if err := q.Scan(&r.actor, &r.target, &r.reason, &r.env, &details); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal([]byte(details), &r.details)
			out = append(out, r)
		}
		return out
	}

	const path = "/api/warnings"
	for range 12 {
		if rec := get(servertest.ArchetypeViewerA, path); rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s as a Viewer: %d, want 403", path, rec.Code)
		}
	}
	first := rows()
	if len(first) != 1 {
		t.Fatalf("%d permission.denied rows after 12 identical refusals, want 1", len(first))
	}
	if first[0].actor != ids[servertest.ArchetypeViewerA] || first[0].target != "GET "+path || first[0].details["coalesced"] != "" {
		t.Fatalf("first row = %+v, want the Viewer's full refusal", first[0])
	}

	clk.advance(61 * time.Second)
	if err := srv.Refusals().Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := rows()
	if len(all) != 2 {
		t.Fatalf("%d rows after the window closed, want 2", len(all))
	}
	sum := all[1]
	if sum.details["count"] != "11" || sum.details["coalesced"] != "true" || sum.details["first"] == "" || sum.details["last"] == "" {
		t.Fatalf("summary details = %v, want count 11, first, last and coalesced=true", sum.details)
	}
	if sum.actor != first[0].actor || sum.target != first[0].target || sum.reason != first[0].reason || sum.env != first[0].env {
		t.Fatalf("summary %+v differs from the first refusal %+v in who, what or where", sum, first[0])
	}

	warnings := func(a servertest.Archetype) (int, []struct {
		ID      string `json:"id"`
		Subject string `json:"subject"`
		Count   int    `json:"count"`
	}) {
		t.Helper()
		rec := get(a, path)
		var body struct {
			Items []struct {
				ID      string `json:"id"`
				Subject string `json:"subject"`
				Count   int    `json:"count"`
			} `json:"items"`
		}
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
		}
		var bursts []struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
			Count   int    `json:"count"`
		}
		for _, it := range body.Items {
			if it.ID == "refusal_burst" {
				bursts = append(bursts, it)
			}
		}
		return rec.Code, bursts
	}
	code, items := warnings(servertest.ArchetypeInstanceAdmin)
	if code != http.StatusOK || len(items) != 1 || items[0].ID != "refusal_burst" ||
		items[0].Subject != ids[servertest.ArchetypeViewerA] || items[0].Count != 12 {
		t.Fatalf("admin warnings = %d %+v, want one refusal_burst naming the Viewer and 12", code, items)
	}
	if code, items := warnings(servertest.ArchetypeViewerA); code != http.StatusForbidden || len(items) != 0 {
		t.Fatalf("Viewer warnings = %d %+v, want 403 and no items", code, items)
	}
}

// T-03-19: PUT /api/me/recent-environments/{envId} answers the D-08 404 itself (it is
// self-service, so the policy gate does not see the environment). It must audit that
// refusal through the same coalescer as the gate, so probing IDs through it is
// neither free nor a way to flood the log.
func TestRecentEnvironmentProbesAreAuditedThroughTheCoalescerWithABoundedEnvID(t *testing.T) {
	fl := pflag.NewFlagSet("refusal", pflag.ContinueOnError)
	config.BindFlags(fl)
	env := map[string]string{"BAYPLAN_DATA_DIR": t.TempDir() + "/data"}
	cfg, err := config.Load(fl, func(k string) string { return env[k] }, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	clk := &stepClock{t: time.Now()}
	srv, err := server.Build(context.Background(), cfg, slog.New(slog.DiscardHandler), server.WithRefusalClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ids := servertest.SeedArchetypes(t, srv.Store(), "env-a", "env-b")
	h := srv.Handler()

	put := func(envID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/api/me/recent-environments/"+envID, nil)
		req.Header.Set("User-Agent", "recent-probe")
		req.Header.Set("Origin", "http://example.com")
		req.RemoteAddr = "192.0.2.78:40000"
		cookie, csrf := servertest.SessionFor(t, srv, ids[servertest.ArchetypeNoGrant])
		req.AddCookie(cookie)
		req.Header.Set("X-CSRF-Token", csrf)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	type row struct{ actor, target, reason, env string }
	rows := func() []row {
		t.Helper()
		q, err := srv.Store().Read.Query(`SELECT actor_id, target_id, reason, COALESCE(env_id, '') FROM audit_log WHERE action = ? ORDER BY seq`, audit.ActionPermissionDenied)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = q.Close() }()
		var out []row
		for q.Next() {
			var r row
			if err := q.Scan(&r.actor, &r.target, &r.reason, &r.env); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}

	const same = "00000000-0000-7000-8000-000000000000"
	var firstBody string
	for i := range 20 {
		rec := put(same)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("PUT = %d %s, want 404", rec.Code, rec.Body)
		}
		if i == 0 {
			firstBody = rec.Body.String()
		}
	}
	got := rows()
	if len(got) != 1 {
		t.Fatalf("%d permission.denied rows after 20 identical probes, want 1 (coalesced)", len(got))
	}
	want := row{ids[servertest.ArchetypeNoGrant], "PUT /api/me/recent-environments/{envId}", "environment_not_visible", same}
	if got[0] != want {
		t.Fatalf("row = %+v, want %+v", got[0], want)
	}

	// The 404 reads the same whatever the ID, and the log stays small however many
	// different IDs are tried and however long they are.
	long := strings.Repeat("a", 400)
	for i := range 300 {
		rec := put(strconv.Itoa(i) + long)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("probe %d = %d, want 404", i, rec.Code)
		}
		if rec.Body.String() != firstBody {
			t.Fatalf("probe %d answered %s, want the bytes of the first 404 %s", i, rec.Body, firstBody)
		}
	}
	got = rows()
	if len(got) > 40 {
		t.Fatalf("%d permission.denied rows after 300 distinct probes, want the per-user cap to hold them to a few dozen", len(got))
	}
	for _, r := range got {
		if len(r.env) > 128 {
			t.Fatalf("a row records an environment ID of %d bytes, want at most 128", len(r.env))
		}
	}
}
