// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/engine"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

// The environment registry and monitor (plan 16, ENV-04, PLAT-06, D-19 to
// D-27, D-48). Docker is a FakeDaemon on a real unix socket, so the real
// client and probe run; the monitor's sleeps are injected so backoff is
// observed, not waited for.

var admin = audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "admin"}

// clock is a controllable time source that moves one second per reading, so
// two readings are never equal.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

// sleeper records every requested delay and waits at most 2ms of real time.
type sleeper struct {
	mu   sync.Mutex
	durs []time.Duration
}

func (s *sleeper) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.durs = append(s.durs, d)
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(min(d, 2*time.Millisecond)):
		return nil
	}
}

func (s *sleeper) delays() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.durs...)
}

// blockingSleep never lets a wait elapse: the monitor probes only at start and
// when kicked.
func blockingSleep(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type rig struct {
	st   *store.Store
	aud  *audit.Writer
	mon  *Monitor
	svc  *Service
	fake *dockertest.Fake
	clk  *clock
	slp  *sleeper
}

func newRig(t *testing.T, cfg dockertest.FakeConfig) *rig {
	t.Helper()
	r := &rig{st: storetest.New(t), clk: newClock(), slp: &sleeper{}}
	v, err := vault.LoadOrCreate(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r.aud = audit.NewWriter(r.st, v, slog.New(slog.DiscardHandler), r.clk.now)
	r.fake = dockertest.NewFake(t, cfg)
	r.mon = NewMonitor(slog.New(slog.DiscardHandler), r.clk.now, r.slp.sleep)
	t.Cleanup(r.mon.StopAll)
	r.svc = NewService(r.st, r.aud, r.mon, "unix://"+r.fake.SocketPath(), "self-id", r.clk.now)
	return r
}

type auditRow struct{ Action, ActorID, TargetID, EnvID, Result, Details string }

func (r *rig) audits(t *testing.T, action string) []auditRow {
	t.Helper()
	rows, err := r.st.Read.Query(`SELECT action, actor_id, target_id, COALESCE(env_id, ''), result, details FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		var details sql.NullString
		if err := rows.Scan(&a.Action, &a.ActorID, &a.TargetID, &a.EnvID, &a.Result, &details); err != nil {
			t.Fatal(err)
		}
		a.Details = details.String
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *rig) rowCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.st.Read.QueryRow(`SELECT COUNT(*) FROM environments`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (r *rig) add(t *testing.T) Environment {
	t.Helper()
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "203.0.113.9", "ua")
	if err != nil {
		t.Fatalf("AddLocal: %v", err)
	}
	return e
}

// --- AddLocal ---------------------------------------------------------------

func TestAddLocalStoresAnEnvironmentNamedAfterTheDaemonHostname(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{Hostname: "node-1"})
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "203.0.113.9", "ua/1")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "node-1" || e.Kind != "local" || !e.Enabled || e.ID == "" || e.SocketPath != r.fake.SocketPath() {
		t.Fatalf("environment = %+v", e)
	}
	got, err := r.svc.Get(context.Background(), e.ID)
	if err != nil || got.Name != "node-1" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	entries := r.audits(t, audit.ActionEnvironmentAdded)
	if len(entries) != 1 || entries[0].ActorID != "u-admin" || entries[0].TargetID != e.ID || entries[0].EnvID != e.ID || entries[0].Result != "success" {
		t.Fatalf("environment.added entries = %+v", entries)
	}
	if ok, err := r.svc.HasLocal(context.Background()); err != nil || !ok {
		t.Errorf("HasLocal = %v, %v, want true", ok, err)
	}
}

func TestAddLocalKeepsAnExplicitNameAndTrimsIt(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{Hostname: "node-1"})
	e, err := r.svc.AddLocal(context.Background(), admin, "  Home lab  ", false, "", "")
	if err != nil || e.Name != "Home lab" {
		t.Fatalf("got %+v, %v, want the trimmed explicit name", e, err)
	}
}

func TestAddLocalRefusesAnInvalidExplicitName(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	_, err := r.svc.AddLocal(context.Background(), admin, strings.Repeat("x", 65), false, "", "")
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("err = %v, want ErrInvalidName", err)
	}
	if r.rowCount(t) != 0 {
		t.Error("a refused add must store nothing")
	}
}

func TestAddLocalWithoutASocketReturnsErrSocketNotFound(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	svc := NewService(r.st, r.aud, r.mon, "unix:///nonexistent/dir/docker.sock", "", r.clk.now)
	_, err := svc.AddLocal(context.Background(), admin, "", false, "", "")
	if !errors.Is(err, ErrSocketNotFound) {
		t.Fatalf("err = %v, want ErrSocketNotFound", err)
	}
	if r.rowCount(t) != 0 || len(r.audits(t, audit.ActionEnvironmentAdded)) != 0 {
		t.Error("a refused add must store and audit nothing")
	}
}

func TestAddLocalRefusesAnEngineThatDoesNotAnswer(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	r.fake.Stop()
	// The socket file is gone with the daemon; a stale file would be a socket
	// that exists but is dead. Point at the stopped path anyway.
	_, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if !errors.Is(err, ErrSocketNotFound) && !errors.Is(err, ErrEngineUnreachable) {
		t.Fatalf("err = %v, want ErrSocketNotFound or ErrEngineUnreachable", err)
	}
}

func TestAddLocalRefusesAnUnsupportedEngineUnlessAllowed(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{APIVersion: "1.41", Version: "20.10.24", Hostname: "old-node"})
	_, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if !errors.Is(err, ErrUnsupportedEngine) {
		t.Fatalf("err = %v, want ErrUnsupportedEngine", err)
	}
	if r.rowCount(t) != 0 {
		t.Fatal("a refused add must store nothing")
	}

	e, err := r.svc.AddLocal(context.Background(), admin, "", true, "", "")
	if err != nil {
		t.Fatalf("allowUnsupported: %v", err)
	}
	if e.Status.State != StateUnsupported || e.Status.Reason != "api_too_old" {
		t.Errorf("status = %+v, want unsupported/api_too_old", e.Status)
	}
	eventually(t, "the monitor to classify the engine as unsupported", func() bool {
		return r.mon.Status(e.ID).State == StateUnsupported
	})
	if _, _, err := r.mon.Client(e.ID); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Client err = %v, want ErrUnsupported", err)
	}
}

func TestConcurrentAddLocalCreatesExactlyOneEnvironment(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	const n = 8
	errs := make(chan error, n)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for range n {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			_, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
			errs <- err
		}()
	}
	start.Done()
	done.Wait()
	close(errs)
	var ok, exists int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrLocalExists):
			exists++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || exists != n-1 {
		t.Fatalf("%d succeeded and %d got ErrLocalExists, want 1 and %d", ok, exists, n-1)
	}
	if r.rowCount(t) != 1 || len(r.audits(t, audit.ActionEnvironmentAdded)) != 1 {
		t.Errorf("rows = %d, audit entries = %d, want 1 and 1", r.rowCount(t), len(r.audits(t, audit.ActionEnvironmentAdded)))
	}
	if r.mon.loopCount() != 1 {
		t.Errorf("loopCount = %d, want 1", r.mon.loopCount())
	}
}

func TestNothingIsAddedAtStartup(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	if err := r.svc.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, err := r.svc.List(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("List = %v, %v, want empty", list, err)
	}
	if got := r.fake.Requests(); len(got) != 0 {
		t.Errorf("startup with no environment talked to Docker: %v", got)
	}
}

func TestStartAllResumesMonitoringStoredEnvironments(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	e := r.add(t)
	r.mon.StopAll() // a restart: the row survives, the monitor does not
	if r.mon.Status(e.ID).State != StateUnknown {
		t.Fatal("a stopped monitor should report unknown")
	}
	if err := r.svc.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the restarted monitor to see the engine", func() bool { return r.mon.Status(e.ID).State == StateOnline })
	list, err := r.svc.List(context.Background())
	if err != nil || len(list) != 1 || list[0].Status.State != StateOnline {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

// --- Rename and Remove ------------------------------------------------------

func TestRenameValidatesTheNameAndAudits(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{Hostname: "node-1"})
	e := r.add(t)
	ctx := context.Background()
	for _, bad := range []string{"", "   ", strings.Repeat("é", 65)} {
		if _, err := r.svc.Rename(ctx, admin, e.ID, bad, "", ""); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Rename(%q) err = %v, want ErrInvalidName", bad, err)
		}
	}
	if got, _ := r.svc.Get(ctx, e.ID); got.Name != "node-1" {
		t.Fatalf("a refused rename changed the name to %q", got.Name)
	}
	got, err := r.svc.Rename(ctx, admin, e.ID, strings.Repeat("é", 64), "", "")
	if err != nil || got.Name != strings.Repeat("é", 64) {
		t.Fatalf("Rename to 64 characters = %+v, %v", got, err)
	}
	got, err = r.svc.Rename(ctx, admin, e.ID, " Lab ", "", "")
	if err != nil || got.Name != "Lab" {
		t.Fatalf("Rename = %+v, %v", got, err)
	}
	entries := r.audits(t, audit.ActionEnvironmentRenamed)
	if len(entries) != 2 {
		t.Fatalf("environment.renamed entries = %d, want 2", len(entries))
	}
	last := entries[1]
	if last.TargetID != e.ID || !strings.Contains(last.Details, `"to":"Lab"`) || !strings.Contains(last.Details, `"from":"`+strings.Repeat("é", 64)+`"`) {
		t.Errorf("last rename entry = %+v, want details from and to", last)
	}
	if _, err := r.svc.Rename(ctx, admin, "no-such", "x", "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Rename of an unknown id err = %v, want ErrNotFound", err)
	}
}

func TestRemoveDeletesTheRowStopsTheMonitorAndChangesNothingInDocker(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	e := r.add(t)
	eventually(t, "online", func() bool { return r.mon.Status(e.ID).State == StateOnline })
	if err := r.svc.Remove(context.Background(), admin, e.ID, "203.0.113.9", "ua", false); err != nil {
		t.Fatal(err)
	}
	if r.rowCount(t) != 0 {
		t.Error("the row should be gone")
	}
	if r.mon.loopCount() != 0 || r.mon.Status(e.ID).State != StateUnknown {
		t.Errorf("loopCount = %d, state = %s, want 0 and unknown", r.mon.loopCount(), r.mon.Status(e.ID).State)
	}
	if entries := r.audits(t, audit.ActionEnvironmentRemoved); len(entries) != 1 || entries[0].TargetID != e.ID {
		t.Errorf("environment.removed entries = %+v", entries)
	}
	// Removing stops monitoring; it must not touch Docker. Only the read-only
	// probe endpoints were ever called.
	for _, p := range r.fake.Requests() {
		path := p
		if i := strings.Index(p, "/v1."); i == 0 {
			path = p[strings.Index(p[1:], "/")+1:]
		}
		if path != "/_ping" && path != "/version" && path != "/info" {
			t.Errorf("Docker was asked for %s", p)
		}
	}
	if err := r.svc.Remove(context.Background(), admin, e.ID, "", "", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Remove err = %v, want ErrNotFound", err)
	}
	// With the row gone, the local host can be opted in again.
	if _, err := r.svc.AddLocal(context.Background(), admin, "", false, "", ""); err != nil {
		t.Errorf("re-adding after removal: %v", err)
	}
}

// seedGrantsOn gives an environment what removing it must take with it: a direct
// and a team assignment (one with an extra) and an owned automation. It also adds an
// assignment and an owned row on another environment and an all-environments
// assignment, which must survive.
func seedGrantsOn(t *testing.T, st *store.Store, envID string) {
	t.Helper()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u1', 'alice', 'x', 1, 1, 1)`, nil},
		{`INSERT INTO teams (id, name, created_at, updated_at) VALUES ('t1', 'ops', 1, 1)`, nil},
		{`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES ('ra-user', 'user', 'u1', 'operator', ?, 1)`, []any{envID}},
		{`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES ('ra-team', 'team', 't1', 'viewer', ?, 1)`, []any{envID}},
		{`INSERT INTO assignment_extras (assignment_id, permission) VALUES ('ra-user', 'containers.exec')`, nil},
		{`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES ('ra-other', 'user', 'u1', 'viewer', 'some-other-env', 1)`, nil},
		{`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES ('ra-all', 'user', 'u1', 'viewer', NULL, 1)`, nil},
		{`INSERT INTO owned_resources (kind, resource_id, env_id, name, owner_id, created_at, updated_at) VALUES ('test.sync', 'r1', ?, 'Sync', 'u1', 1, 1)`, []any{envID}},
		{`INSERT INTO owned_resources (kind, resource_id, env_id, name, owner_id, created_at, updated_at) VALUES ('test.sync', 'r2', 'some-other-env', 'Sync', 'u1', 1, 1)`, nil},
	} {
		if _, err := st.Write.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
}

// grantRowsOn counts what is left of an environment's grants, extras and owned rows.
func grantRowsOn(t *testing.T, st *store.Store, envID string) (assignments, extras, owned int) {
	t.Helper()
	for q, dst := range map[string]*int{
		`SELECT COUNT(*) FROM role_assignments WHERE env_id = ?`:                                                           &assignments,
		`SELECT COUNT(*) FROM assignment_extras WHERE assignment_id IN (SELECT id FROM role_assignments WHERE env_id = ?)`: &extras,
		`SELECT COUNT(*) FROM owned_resources WHERE env_id = ?`:                                                            &owned,
	} {
		if err := st.Read.QueryRow(q, envID).Scan(dst); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return assignments, extras, owned
}

func TestRemoveDeletesTheEnvironmentsGrantsAndOwnedRowsInTheSameTransactionAndAuditsTheCount(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	e := r.add(t)
	seedGrantsOn(t, r.st, e.ID)

	if err := r.svc.Remove(context.Background(), admin, e.ID, "203.0.113.9", "ua", false); err != nil {
		t.Fatal(err)
	}
	if a, x, o := grantRowsOn(t, r.st, e.ID); a != 0 || x != 0 || o != 0 {
		t.Fatalf("left behind: %d assignments, %d extras, %d owned rows", a, x, o)
	}
	var elsewhere int
	if err := r.st.Read.QueryRow(`SELECT COUNT(*) FROM role_assignments WHERE id IN ('ra-other', 'ra-all')`).Scan(&elsewhere); err != nil || elsewhere != 2 {
		t.Fatalf("another environment's and the all-environments assignment = %d (%v), want both kept", elsewhere, err)
	}
	if n := countRows(t, r.st, `SELECT COUNT(*) FROM owned_resources WHERE env_id = 'some-other-env'`); n != 1 {
		t.Fatalf("another environment's owned row = %d, want kept", n)
	}
	entries := r.audits(t, audit.ActionEnvironmentRemoved)
	if len(entries) != 1 || !strings.Contains(entries[0].Details, `"grants":"2"`) {
		t.Fatalf("environment.removed entries = %+v, want one carrying grants 2", entries)
	}
}

func countRows(t *testing.T, st *store.Store, q string) int {
	t.Helper()
	var n int
	if err := st.Read.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// --- Resolve ----------------------------------------------------------------

func TestResolveDistinguishesUnknownDisabledAndEnabled(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{Hostname: "node-1"})
	ctx := context.Background()
	if _, err := r.svc.Resolve(ctx, "nope"); !errors.Is(err, httpx.ErrEnvNotFound) {
		t.Fatalf("unknown id err = %v, want ErrEnvNotFound", err)
	}
	e := r.add(t)
	got, err := r.svc.Resolve(ctx, e.ID)
	if err != nil || got.ID != e.ID || got.Name != "node-1" || !got.Enabled {
		t.Fatalf("Resolve = %+v, %v", got, err)
	}
	if _, err := r.st.Write.Exec(`UPDATE environments SET enabled = 0 WHERE id = ?`, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Resolve(ctx, e.ID); !errors.Is(err, httpx.ErrEnvDisabled) {
		t.Fatalf("disabled err = %v, want ErrEnvDisabled", err)
	}
}

// --- Monitor ----------------------------------------------------------------

func TestMonitorGoesOfflineBacksOffAndRecoversWithARenegotiatedAPI(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{APIVersion: "1.47"})
	e := r.add(t)
	eventually(t, "online", func() bool { return r.mon.Status(e.ID).State == StateOnline })
	if got := r.mon.Status(e.ID).Probe.NegotiatedAPI; got != "1.47" {
		t.Fatalf("NegotiatedAPI = %q, want 1.47", got)
	}

	r.fake.Stop()
	r.fake.SetAPIVersion("1.52") // upgraded while it was down
	eventually(t, "offline", func() bool { return r.mon.Status(e.ID).State == StateOffline })
	first := r.mon.Status(e.ID)
	if first.Since.IsZero() || first.Reason == "" {
		t.Fatalf("offline status = %+v, want Since and a reason", first)
	}
	// Keep failing: Since must stay at the first failure while LastAttempt moves on.
	eventually(t, "more failed probes", func() bool { return r.mon.Status(e.ID).LastAttempt.After(first.Since.Add(5 * time.Second)) })
	if got := r.mon.Status(e.ID).Since; !got.Equal(first.Since) {
		t.Errorf("Since moved from %v to %v while the daemon stayed down", first.Since, got)
	}
	if _, _, err := r.mon.Client(e.ID); !errors.Is(err, ErrOffline) {
		t.Errorf("Client err = %v, want ErrOffline", err)
	}

	eventually(t, "enough backoff delays", func() bool { return len(r.slp.delays()) >= 12 })
	var longest time.Duration
	for i, d := range r.slp.delays() {
		if d <= 0 || d > 30*time.Second {
			t.Errorf("delay %d = %v, want 0 < d <= 30s", i, d)
		}
		longest = max(longest, d)
	}
	if longest < 15*time.Second {
		t.Errorf("longest delay = %v, want the backoff to climb to about 30s", longest)
	}

	r.fake.Restart(t)
	eventually(t, "online again", func() bool { return r.mon.Status(e.ID).State == StateOnline })
	st := r.mon.Status(e.ID)
	if st.Probe.NegotiatedAPI != "1.52" {
		t.Errorf("NegotiatedAPI after the upgrade = %q, want 1.52", st.Probe.NegotiatedAPI)
	}
	if !st.Since.After(first.Since) {
		t.Errorf("online Since %v should be after the offline Since %v", st.Since, first.Since)
	}
	if _, _, err := r.mon.Client(e.ID); err != nil {
		t.Errorf("Client after recovery: %v", err)
	}
}

func TestMonitorFirstRetryAfterAFailureIsAboutOneSecond(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	e := r.add(t)
	eventually(t, "online", func() bool { return r.mon.Status(e.ID).State == StateOnline })
	r.slp.mu.Lock()
	r.slp.durs = nil
	r.slp.mu.Unlock()
	r.fake.Stop()
	eventually(t, "a recorded delay after the failure", func() bool {
		for _, d := range r.slp.delays() {
			if d <= time.Second {
				return true
			}
		}
		return false
	})
	// The online interval is 30s and is the only wait allowed to be that long
	// before the first failure; the first backoff is within [0.5s, 1s].
	var firstBackoff time.Duration
	for _, d := range r.slp.delays() {
		if d != 30*time.Second {
			firstBackoff = d
			break
		}
	}
	if firstBackoff < 500*time.Millisecond || firstBackoff > time.Second {
		t.Errorf("first backoff = %v, want within [500ms, 1s]", firstBackoff)
	}
}

func TestKickProbesNowAndOnlyOneLoopRunsPerEnvironment(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	m := NewMonitor(slog.New(slog.DiscardHandler), nil, blockingSleep)
	t.Cleanup(m.StopAll)
	ctx := context.Background()

	infoCalls := func() int {
		n := 0
		for _, p := range fake.Requests() {
			if strings.HasSuffix(p, "/info") {
				n++
			}
		}
		return n
	}

	// Many Starts of one environment, concurrently, still run one loop.
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Start(ctx, "env-1", fake)
		}()
	}
	wg.Wait()
	if m.loopCount() != 1 {
		t.Fatalf("loopCount = %d after 20 concurrent Starts, want 1", m.loopCount())
	}
	eventually(t, "the first probe", func() bool { return m.Status("env-1").State == StateOnline })
	before := infoCalls()

	// The wait is blocked forever, so only a kick can cause another probe.
	m.Kick("env-1")
	eventually(t, "a probe caused by the kick", func() bool { return infoCalls() > before })

	// Kicks and reads from many goroutines at once; run with -race.
	var reads atomic.Int64
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				m.Kick("env-1")
				if st := m.Status("env-1"); st.State == "" {
					t.Error("empty state")
				}
				if _, _, err := m.Client("env-1"); err != nil && !errors.Is(err, ErrOffline) {
					t.Errorf("Client: %v", err)
				}
				reads.Add(1)
			}
		}()
	}
	wg.Wait()
	if reads.Load() != 20*25 {
		t.Fatalf("reads = %d", reads.Load())
	}
	if m.loopCount() != 1 {
		t.Errorf("loopCount = %d after the kicks, want 1", m.loopCount())
	}
	m.Kick("no-such-env") // a kick for an unknown environment is a no-op
}

func TestStopEndsTheLoopAndForgetsTheEnvironment(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	m := NewMonitor(slog.New(slog.DiscardHandler), nil, blockingSleep)
	if err := m.Start(context.Background(), "e", fake); err != nil {
		t.Fatal(err)
	}
	eventually(t, "online", func() bool { return m.Status("e").State == StateOnline })
	m.Stop("e")
	if m.loopCount() != 0 || m.Status("e").State != StateUnknown {
		t.Errorf("loopCount = %d, state = %s after Stop", m.loopCount(), m.Status("e").State)
	}
	m.Stop("e") // twice is fine
	if _, _, err := m.Client("e"); err == nil {
		t.Error("Client of a stopped environment should fail")
	}
}

func TestACancelledContextEndsTheLoop(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	m := NewMonitor(slog.New(slog.DiscardHandler), nil, blockingSleep)
	t.Cleanup(m.StopAll)
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx, "e", fake); err != nil {
		t.Fatal(err)
	}
	cancel()
	eventually(t, "the loop to exit", func() bool { return m.loopCount() == 0 })
}

func TestAHungDaemonIsGivenUpOnAtTheProbeTimeout(t *testing.T) {
	// A socket that accepts and never answers.
	hung := newHungTransport(t)
	m := NewMonitor(slog.New(slog.DiscardHandler), nil, blockingSleep)
	m.probeTimeout = 100 * time.Millisecond
	t.Cleanup(m.StopAll)
	if err := m.Start(context.Background(), "e", hung); err != nil {
		t.Fatal(err)
	}
	eventually(t, "offline after the probe timeout", func() bool { return m.Status("e").State == StateOffline })
}

var _ engine.Transport = (*hungTransport)(nil)

// --- DetectSelfContainerID --------------------------------------------------

func TestDetectSelfContainerID(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const other = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	hostnameLine := "710 700 8:1 /var/lib/docker/containers/" + id + "/hostname /etc/hostname rw,relatime - ext4 /dev/sda1 rw\n"
	resolvLine := "711 700 8:1 /var/lib/docker/containers/" + id + "/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/sda1 rw\n"
	noise := "25 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n26 25 0:5 / /proc rw - proc proc rw\n"
	// Another container's directory mounted for log shipping, listed first.
	foreign := "700 690 8:1 /var/lib/docker/containers/" + other + " /var/log/containers ro - ext4 /dev/sda1 rw\n"

	tests := []struct {
		name, mountinfo, hostname, want string
	}{
		{"hostname file mount", noise + hostnameLine, "whatever", id},
		{"resolv.conf mount only", noise + resolvLine, "", id},
		{"prefers the identity mounts over a foreign directory", foreign + hostnameLine, "", id},
		{"a foreign directory alone is not Bayplan's own", foreign, "", ""},
		{"a foreign directory alone falls back to a short hostname", foreign, "0123456789ab", "0123456789ab"},
		{"no match and a 12 hex hostname", noise, "0123456789ab", "0123456789ab"},
		{"no match and a hostname that is not a short ID", noise, "bayplan", ""},
		{"no match and a 12 character non-hex hostname", noise, "zzzzzzzzzzzz", ""},
		{"no match and an upper-case hex hostname", noise, "0123456789AB", ""},
		{"a 63 character directory is not an ID", "1 2 8:1 /var/lib/docker/containers/" + id[:63] + "/hostname /etc/hostname rw - ext4 x rw\n", "", ""},
		{"an empty mountinfo and no hostname", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectSelfContainerID(strings.NewReader(tc.mountinfo), tc.hostname); got != tc.want {
				t.Errorf("DetectSelfContainerID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestASlowSnapshotNeitherFlapsTheEngineNorDelaysStop(t *testing.T) {
	// WR-06 (D-26): a snapshot holding the client lock made the next probe wait
	// past its timeout, so a healthy engine with a slow container list went
	// offline, and Stop waited for the snapshot to finish.
	// The delay outlasts the sampling window, so Stop meets a snapshot that is
	// still running.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{ListDelay: 3 * time.Second})
	m := NewMonitor(slog.New(slog.DiscardHandler), nil, func(context.Context, time.Duration) error { return nil })
	// Well under the 3s list delay, and well over a slowed ping (BAYPLAN_TEST_PROBE_DELAY
	// sweeps), so only a probe that waits for the snapshot can time out.
	m.probeTimeout = time.Second
	if err := m.Start(context.Background(), "e", fake); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.StopAll)
	eventually(t, "online", func() bool { return m.Status("e").State == StateOnline })

	cli, _, err := m.Client("e")
	if err != nil {
		t.Fatal(err)
	}
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	go func() { _, _ = cli.Snapshot(sctx, false) }()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if st := m.Status("e").State; st != StateOnline {
			t.Fatalf("state = %s while a slow snapshot ran, want online", st)
		}
		time.Sleep(50 * time.Millisecond)
	}

	start := time.Now()
	m.Stop("e")
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("Stop took %s with a snapshot in flight, want under 500ms", d)
	}
}

func TestCheckNameAppliesTheRenameRuleWithoutWriting(t *testing.T) {
	for _, bad := range []string{"", "   ", strings.Repeat("x", 65)} {
		if err := CheckName(bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("CheckName(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
	for _, good := range []string{"a", " Lab ", strings.Repeat("x", 64)} {
		if err := CheckName(good); err != nil {
			t.Errorf("CheckName(%q) = %v, want nil", good, err)
		}
	}
}
