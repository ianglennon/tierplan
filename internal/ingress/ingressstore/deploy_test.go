// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// newNotifiedStore is newTestStore with a Notifier and one subscriber, so a test can count the wake-ups.
func newNotifiedStore(t *testing.T) (*Store, *store.Store, *ingress.Subscription) {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	n := ingress.NewNotifier()
	sub := n.Subscribe()
	t.Cleanup(sub.Close)
	return New(Deps{St: st, Vault: v, Now: func() time.Time { return fixedNow }, Notifier: n}), st, sub
}

// TestSetEnabledIsIdempotent pins the tracer: the first switch on creates the row, raises the version
// once and notifies once; the same call again changes nothing and tells nobody.
func TestSetEnabledIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, st, sub := newNotifiedStore(t)
	seedEnv(t, st, "env-a", "Alpha")

	hooks := 0
	hook := func(context.Context, *sql.Tx) error { hooks++; return nil }

	changed, err := s.SetEnabled(ctx, "env-a", true, "admin", hook)
	if err != nil || !changed {
		t.Fatalf("first SetEnabled = %v, %v; want true, nil", changed, err)
	}
	if v := configVersion(t, st, "env-a"); v != 1 {
		t.Fatalf("config_version after the first switch = %d, want 1", v)
	}
	if got := sub.Pending(); len(got) != 1 || got[0] != "env-a" {
		t.Fatalf("notified %v after the first switch, want [env-a]", got)
	}
	set, err := s.Settings(ctx, "env-a")
	if err != nil || !set.Exists || !set.Enabled || set.ConfigVersion != 1 || set.UpdatedBy != "admin" {
		t.Fatalf("Settings = %+v, %v; want an enabled row at version 1 by admin", set, err)
	}

	changed, err = s.SetEnabled(ctx, "env-a", true, "admin", hook)
	if err != nil || changed {
		t.Fatalf("repeated SetEnabled = %v, %v; want false, nil", changed, err)
	}
	if v := configVersion(t, st, "env-a"); v != 1 {
		t.Fatalf("config_version after a repeat = %d, want it to stay 1", v)
	}
	if got := sub.Pending(); len(got) != 0 {
		t.Fatalf("a repeat notified %v, want nobody", got)
	}
	if hooks != 1 {
		t.Fatalf("the hook ran %d times, want once (only for the change)", hooks)
	}

	// Off is a change too; off again is not.
	if changed, err := s.SetEnabled(ctx, "env-a", false, "admin", hook); err != nil || !changed {
		t.Fatalf("switch off = %v, %v; want true, nil", changed, err)
	}
	if v := configVersion(t, st, "env-a"); v != 2 {
		t.Fatalf("config_version after off = %d, want 2", v)
	}
	sub.Pending()
	if changed, err := s.SetEnabled(ctx, "env-a", false, "admin", hook); err != nil || changed {
		t.Fatalf("switch off again = %v, %v; want false, nil", changed, err)
	}

	// An environment that never had a row, asked for off, gets none.
	seedEnv(t, st, "env-b", "Beta")
	if changed, err := s.SetEnabled(ctx, "env-b", false, "admin", hook); err != nil || changed {
		t.Fatalf("off on a row-less environment = %v, %v; want false, nil", changed, err)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_env WHERE env_id = 'env-b'`); n != 0 {
		t.Fatalf("a no-op off created %d ingress_env rows, want 0", n)
	}

	// A failing hook rolls the switch back.
	boom := errors.New("audit failed")
	if _, err := s.SetEnabled(ctx, "env-b", true, "admin", func(context.Context, *sql.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("SetEnabled with a failing hook = %v, want the hook's error", err)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_env WHERE env_id = 'env-b'`); n != 0 {
		t.Fatalf("a rolled-back switch left %d rows", n)
	}
}

// TestDeployStateRoundTrip reads every deploy column back after writing it.
func TestDeployStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, st, sub := newNotifiedStore(t)
	seedEnv(t, st, "env-a", "Alpha")

	if got, err := s.DeployState(ctx, "env-a"); err != nil || got != (DeployState{}) {
		t.Fatalf("DeployState of an environment with no row = %+v, %v; want the zero value", got, err)
	}

	if _, err := s.SetEnabled(ctx, "env-a", true, "admin", nil); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.SetAccessLogs(ctx, "env-a", true, "admin", nil); err != nil || !changed {
		t.Fatalf("SetAccessLogs on = %v, %v", changed, err)
	}
	if changed, err := s.SetAccessLogs(ctx, "env-a", true, "admin", nil); err != nil || changed {
		t.Fatalf("SetAccessLogs repeated = %v, %v; want false", changed, err)
	}
	versionBeforePause := configVersion(t, st, "env-a")
	sub.Pending()
	if changed, err := s.SetPaused(ctx, "env-a", true, "admin", nil); err != nil || !changed {
		t.Fatalf("SetPaused on = %v, %v", changed, err)
	}
	if v := configVersion(t, st, "env-a"); v != versionBeforePause {
		t.Fatalf("SetPaused moved config_version %d -> %d; it changes no route", versionBeforePause, v)
	}
	if got := sub.Pending(); len(got) != 0 {
		t.Fatalf("SetPaused notified %v, want nobody", got)
	}
	at := time.Date(2026, time.November, 16, 9, 30, 0, 0, time.UTC)
	if err := s.RecordEnsure(ctx, "env-a", at, "engine_unreachable"); err != nil {
		t.Fatal(err)
	}
	if v := configVersion(t, st, "env-a"); v != versionBeforePause {
		t.Fatalf("RecordEnsure moved config_version to %d", v)
	}
	if got := sub.Pending(); len(got) != 0 {
		t.Fatalf("RecordEnsure notified %v, want nobody", got)
	}

	want := DeployState{Enabled: true, AccessLogs: true, Paused: true, EnsureAt: at, EnsureError: "engine_unreachable"}
	got, err := s.DeployState(ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("DeployState = %+v, want %+v", got, want)
	}

	// Settings is untouched by the new columns.
	set, err := s.Settings(ctx, "env-a")
	if err != nil || !set.Enabled || set.ConfigVersion != versionBeforePause {
		t.Fatalf("Settings = %+v, %v", set, err)
	}

	// The columns turn back off and the ensure state clears.
	if _, err := s.SetPaused(ctx, "env-a", false, "admin", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAccessLogs(ctx, "env-a", false, "admin", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEnsure(ctx, "env-a", at.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.DeployState(ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	want = DeployState{Enabled: true, EnsureAt: at.Add(time.Minute)}
	if got != want {
		t.Fatalf("DeployState after clearing = %+v, want %+v", got, want)
	}

	// RecordEnsure on an environment with no row creates it, switched off.
	seedEnv(t, st, "env-b", "Beta")
	if err := s.RecordEnsure(ctx, "env-b", at, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DeployState(ctx, "env-b"); err != nil || got.Enabled || got.EnsureAt != at {
		t.Fatalf("DeployState of an ensured environment = %+v, %v", got, err)
	}

	envs, err := s.EnabledEnvs(ctx)
	if err != nil || len(envs) != 1 || envs[0] != "env-a" {
		t.Fatalf("EnabledEnvs = %v, %v; want [env-a]", envs, err)
	}
}

// TestPendingWipesRoundTrip queues, lists, dedups, clears and cascades a pending wipe.
func TestPendingWipesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	t1 := fixedNow
	t2 := fixedNow.Add(time.Minute)

	if err := s.AddPendingWipe(ctx, nil, "env-a", "node-2", ingress.VolumeName, t2); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPendingWipe(ctx, nil, "env-a", "node-1", ingress.VolumeName, t1); err != nil {
		t.Fatal(err)
	}
	// The same wipe again is one row and the first time stands.
	if err := s.AddPendingWipe(ctx, nil, "env-a", "node-1", ingress.VolumeName, t2); err != nil {
		t.Fatal(err)
	}
	// Inside a caller's transaction.
	tx, err := st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddPendingWipe(ctx, tx, "env-b", "node-9", ingress.VolumeName, t1); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got, err := s.PendingWipes(ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SwarmNodeID != "node-1" || !got[0].CreatedAt.Equal(t1) || got[1].SwarmNodeID != "node-2" {
		t.Fatalf("PendingWipes(env-a) = %+v, want node-1 (first time kept) then node-2", got)
	}
	all, err := s.AllPendingWipes(ctx)
	if err != nil || len(all) != 3 || all[0].EnvID != "env-a" || all[2].EnvID != "env-b" {
		t.Fatalf("AllPendingWipes = %+v, %v; want three rows, env-a first", all, err)
	}

	if ok, err := s.ClearPendingWipe(ctx, "env-a", "node-1", ingress.VolumeName); err != nil || !ok {
		t.Fatalf("ClearPendingWipe = %v, %v; want true", ok, err)
	}
	if ok, err := s.ClearPendingWipe(ctx, "env-a", "node-1", ingress.VolumeName); err != nil || ok {
		t.Fatalf("a second ClearPendingWipe = %v, %v; want false", ok, err)
	}
	if got, _ := s.PendingWipes(ctx, "env-a"); len(got) != 1 {
		t.Fatalf("PendingWipes(env-a) after a clear = %+v, want one", got)
	}

	// The environment's removal takes its wipes with it.
	if _, err := st.Write.Exec(`DELETE FROM environments WHERE id = 'env-b'`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_pending_wipes WHERE env_id = 'env-b'`); n != 0 {
		t.Fatalf("%d wipes of a removed environment remain", n)
	}
}

// TestLiveRoutesOrderAndScope lists the routes of one environment in hostname then path order, keeps
// a stopped stack's claim in, and never lists another environment's.
func TestLiveRoutesOrderAndScope(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	seedStack(t, st, "stack-1", "env-a", "web")
	seedStack(t, st, "stack-2", "env-a", "api")
	seedStack(t, st, "stack-3", "env-b", "other")
	if _, err := st.Write.Exec(`UPDATE stacks SET stopped = 1 WHERE id = 'stack-2'`); err != nil {
		t.Fatalf("stop stack-2: %v", err)
	}
	domA := registerDomain(t, s, "env-a", "apps.example.com")
	domB := registerDomain(t, s, "env-b", "other.example.org")

	hostname := func(host, envID, domainID, stackID string) {
		t.Helper()
		if _, err := st.Write.Exec(
			`INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at) VALUES (?, ?, ?, 'stack', ?, 'alice', 1)`,
			host, envID, domainID, stackID); err != nil {
			t.Fatalf("claim %s: %v", host, err)
		}
	}
	route := func(id, envID, host, path string) {
		t.Helper()
		if _, err := st.Write.Exec(
			`INSERT INTO ingress_routes (id, env_id, hostname, name, path_prefix, service, port, created_at, updated_at) VALUES (?, ?, ?, '', ?, 'web', 8080, 1, 1)`,
			id, envID, host, path); err != nil {
			t.Fatalf("route %s%s: %v", host, path, err)
		}
	}
	hostname("b.apps.example.com", "env-a", domA.ID, "stack-1")
	hostname("a.apps.example.com", "env-a", domA.ID, "stack-2")
	hostname("x.other.example.org", "env-b", domB.ID, "stack-3")
	route("r1", "env-a", "b.apps.example.com", "/")
	route("r2", "env-a", "b.apps.example.com", "/api")
	route("r3", "env-a", "a.apps.example.com", "/z")
	route("r4", "env-a", "a.apps.example.com", "/a")
	route("r5", "env-b", "x.other.example.org", "/")

	got, err := s.LiveRoutes(ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	want := []LiveRoute{
		{Hostname: "a.apps.example.com", PathPrefix: "/a", StackID: "stack-2", StackName: "api"},
		{Hostname: "a.apps.example.com", PathPrefix: "/z", StackID: "stack-2", StackName: "api"},
		{Hostname: "b.apps.example.com", PathPrefix: "/", StackID: "stack-1", StackName: "web"},
		{Hostname: "b.apps.example.com", PathPrefix: "/api", StackID: "stack-1", StackName: "web"},
	}
	if len(got) != len(want) {
		t.Fatalf("LiveRoutes(env-a) = %+v, want %d routes", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("LiveRoutes(env-a)[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	other, err := s.LiveRoutes(ctx, "env-b")
	if err != nil || len(other) != 1 || other[0].Hostname != "x.other.example.org" || other[0].StackName != "other" {
		t.Fatalf("LiveRoutes(env-b) = %+v, %v; want only its own route", other, err)
	}
	if none, err := s.LiveRoutes(ctx, "env-none"); err != nil || len(none) != 0 {
		t.Fatalf("LiveRoutes of an unknown environment = %+v, %v; want none", none, err)
	}
}

// TestDeleteIngressNodesByEnv removes every node row of one environment, active or revoked, returns
// them, and leaves the other environment's rows alone.
func TestDeleteIngressNodesByEnv(t *testing.T) {
	ctx := context.Background()
	_, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	key := make([]byte, 32)
	insert := func(id, envID, swarmNode, status string) {
		t.Helper()
		var node any
		if swarmNode != "" {
			node = swarmNode
		}
		if _, err := st.Write.Exec(
			`INSERT INTO ingress_nodes (id, env_id, swarm_node_id, public_key, status, created_at) VALUES (?, ?, ?, ?, ?, 1)`,
			id, envID, node, key, status); err != nil {
			t.Fatalf("insert node %s: %v", id, err)
		}
	}
	insert("n1", "env-a", "sn-1", "active")
	insert("n2", "env-a", "sn-2", "revoked")
	insert("n3", "env-b", "sn-1", "active")

	rows, err := st.Q.DeleteIngressNodesByEnv(ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("DeleteIngressNodesByEnv returned %d rows, want 2", len(rows))
	}
	ids := map[string]string{}
	for _, r := range rows {
		ids[r.ID] = r.Status
	}
	if ids["n1"] != "active" || ids["n2"] != "revoked" {
		t.Fatalf("returned rows = %v, want n1 active and n2 revoked", ids)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_nodes WHERE env_id = 'env-a'`); n != 0 {
		t.Fatalf("%d nodes of env-a remain", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_nodes WHERE env_id = 'env-b'`); n != 1 {
		t.Fatalf("env-b has %d nodes, want 1", n)
	}
	if again, err := st.Q.DeleteIngressNodesByEnv(ctx, "env-a"); err != nil || len(again) != 0 {
		t.Fatalf("a second delete = %d rows, %v; want none", len(again), err)
	}
}

// TestQueueAndCompleteWipeRunTheirHookInTheTransaction: the hook of a queued wipe commits with the wipe and a hook that fails
// takes the wipe with it; completing a wipe runs its hook only when there was a wipe to complete.
func TestQueueAndCompleteWipeRunTheirHookInTheTransaction(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	boom := errors.New("the audit row could not be written")

	hooked := 0
	count := func(context.Context, *sql.Tx) error { hooked++; return nil }
	if err := s.QueueWipe(ctx, "env-a", "node-1", ingress.VolumeName, fixedNow, func(context.Context, *sql.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("QueueWipe with a failing hook = %v, want the hook's error", err)
	}
	if got, _ := s.PendingWipes(ctx, "env-a"); len(got) != 0 {
		t.Fatalf("a wipe whose hook failed was queued: %+v", got)
	}
	if err := s.QueueWipe(ctx, "env-a", "node-1", ingress.VolumeName, fixedNow, count); err != nil || hooked != 1 {
		t.Fatalf("QueueWipe = %v with %d hook runs, want one", err, hooked)
	}
	if got, _ := s.PendingWipes(ctx, "env-a"); len(got) != 1 {
		t.Fatalf("PendingWipes = %+v, want the queued wipe", got)
	}

	if _, err := s.CompleteWipe(ctx, "env-a", "node-1", ingress.VolumeName, func(context.Context, *sql.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("CompleteWipe with a failing hook = %v, want the hook's error", err)
	}
	if got, _ := s.PendingWipes(ctx, "env-a"); len(got) != 1 {
		t.Fatalf("a completion whose hook failed cleared the wipe: %+v", got)
	}
	if ok, err := s.CompleteWipe(ctx, "env-a", "node-1", ingress.VolumeName, count); err != nil || !ok || hooked != 2 {
		t.Fatalf("CompleteWipe = %v, %v with %d hook runs, want true and a second run", ok, err, hooked)
	}
	if ok, err := s.CompleteWipe(ctx, "env-a", "node-1", ingress.VolumeName, count); err != nil || ok || hooked != 2 {
		t.Fatalf("a second CompleteWipe = %v, %v with %d hook runs, want false and no hook", ok, err, hooked)
	}
}
