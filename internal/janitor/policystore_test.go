// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"errors"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The policy store on a real SQLite store (plan 11-17, D-04): the three levels, the seeded
// defaults, and a save that is versioned and validated.

type policyFix struct {
	st *store.Store
	ps PolicyStore
}

func newPolicyFix(t *testing.T) policyFix {
	t.Helper()
	st := storetest.New(t)
	if _, err := st.Write.ExecContext(testsupport.Context(t),
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'agent', 'e', '', 1, 1, 1), ('env-2', 'agent', 'f', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	return policyFix{st: st, ps: PolicyStore{St: st}}
}

// save runs SetEnv in a transaction of its own and commits it unless it failed.
func (f policyFix) save(t *testing.T, env string, o Overrides, version int64) (bool, error) {
	t.Helper()
	ctx := testsupport.Context(t)
	tx, err := f.st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := f.ps.SetEnv(ctx, f.st.Q.WithTx(tx), env, o, version, "ada", time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return changed, nil
}

func (f policyFix) effective(t *testing.T, env string) Policy {
	t.Helper()
	p, err := f.ps.Effective(testsupport.Context(t), env)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f policyFix) setDefaults(t *testing.T, set string) {
	t.Helper()
	if _, err := f.st.Write.ExecContext(testsupport.Context(t), `UPDATE janitor_defaults SET `+set+` WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyStoreSeededDefaults(t *testing.T) {
	t.Parallel()
	f := newPolicyFix(t)
	ctx := testsupport.Context(t)
	pol, ret, version, err := f.ps.Defaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pol != DefaultPolicy() || ret != DefaultRetention || version != 1 {
		t.Errorf("seeded defaults = %+v %+v v%d, want DefaultPolicy, DefaultRetention and version 1", pol, ret, version)
	}
	if got := f.effective(t, "env-1"); got != DefaultPolicy() {
		t.Errorf("an environment with no row = %+v, want the defaults", got)
	}
	o, v, err := f.ps.Env(ctx, "env-1")
	if err != nil || o != (Overrides{}) || v != 0 {
		t.Errorf("Env with no row = %+v v%d %v, want no overrides at version 0", o, v, err)
	}
}

func TestPolicyStoreSetEnvRoundTrip(t *testing.T) {
	t.Parallel()
	f := newPolicyFix(t)
	ctx := testsupport.Context(t)
	changed, err := f.save(t, "env-1", Overrides{KeepN: ptr(5), RemoveStoppedContainers: ptr(true)}, 0)
	if err != nil || !changed {
		t.Fatalf("first save = %v, %v, want changed", changed, err)
	}
	o, v, err := f.ps.Env(ctx, "env-1")
	if err != nil || v != 1 || o.KeepN == nil || *o.KeepN != 5 || o.RemoveStoppedContainers == nil || !*o.RemoveStoppedContainers || o.Enabled != nil || o.Schedule != nil || o.GraceHours != nil {
		t.Fatalf("Env = %+v v%d %v, want keep 5 and the stopped-container opt-in only, at version 1", o, v, err)
	}
	want := DefaultPolicy()
	want.KeepN, want.RemoveStoppedContainers = 5, true
	if got := f.effective(t, "env-1"); got != want {
		t.Errorf("effective = %+v, want %+v", got, want)
	}
	if got := f.effective(t, "env-2"); got != DefaultPolicy() {
		t.Errorf("another environment = %+v, want the defaults", got)
	}

	// keepN nil returns it to the default; the opt-in stays.
	changed, err = f.save(t, "env-1", Overrides{RemoveStoppedContainers: ptr(true)}, 1)
	if err != nil || !changed {
		t.Fatalf("second save = %v, %v, want changed", changed, err)
	}
	if _, v, _ := f.ps.Env(ctx, "env-1"); v != 2 {
		t.Errorf("version = %d, want 2", v)
	}
	want.KeepN = DefaultKeepN
	if got := f.effective(t, "env-1"); got != want {
		t.Errorf("after unsetting keep-N = %+v, want %+v", got, want)
	}
}

func TestDefaultChangeFollows(t *testing.T) {
	t.Parallel()
	f := newPolicyFix(t)
	if _, err := f.save(t, "env-2", Overrides{KeepN: ptr(7)}, 0); err != nil {
		t.Fatal(err)
	}
	f.setDefaults(t, `keep_n = 4, grace_hours = 48, remove_unused_networks = 1`)

	one := f.effective(t, "env-1") // has no row: follows everything
	if one.KeepN != 4 || one.Grace != 48*time.Hour || !one.RemoveUnusedNetworks {
		t.Errorf("an environment that follows = %+v, want the changed defaults", one)
	}
	two := f.effective(t, "env-2") // overrides keep-N only
	if two.KeepN != 7 || two.Grace != 48*time.Hour || !two.RemoveUnusedNetworks {
		t.Errorf("an environment that overrides keep-N = %+v, want keep 7 and the other defaults changed", two)
	}
	// Nothing was copied into the row: it still follows the rest.
	if o, _, _ := f.ps.Env(testsupport.Context(t), "env-2"); o.GraceHours != nil || o.RemoveUnusedNetworks != nil {
		t.Errorf("env row = %+v, want only keep-N stored", o)
	}
}

func TestPolicyStoreNodes(t *testing.T) {
	t.Parallel()
	f := newPolicyFix(t)
	ctx := testsupport.Context(t)
	if _, err := f.st.Write.ExecContext(ctx, `INSERT INTO janitor_node_policy (env_id, node_id, enabled, keep_n) VALUES ('env-1', 'node-a', 0, 4), ('env-1', 'node-b', NULL, NULL), ('env-2', 'node-c', 1, NULL)`); err != nil {
		t.Fatal(err)
	}
	got, err := f.ps.Nodes(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["node-a"].Enabled == nil || *got["node-a"].Enabled || got["node-a"].KeepN == nil || *got["node-a"].KeepN != 4 || got["node-b"] != (NodeOverrides{}) {
		t.Errorf("Nodes = %+v, want node-a (disabled, keep 4) and node-b (follows) of env-1 only", got)
	}
}

func TestPolicyStoreSetEnvStaleVersion(t *testing.T) {
	t.Parallel()
	f := newPolicyFix(t)
	ctx := testsupport.Context(t)
	if _, err := f.save(t, "env-1", Overrides{KeepN: ptr(5)}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.save(t, "env-1", Overrides{KeepN: ptr(6)}, 1); err != nil {
		t.Fatal(err)
	}
	for name, version := range map[string]int64{"a version already used": 1, "the version of no row": 0, "a version not reached": 9} {
		changed, err := f.save(t, "env-1", Overrides{KeepN: ptr(10)}, version)
		if !errors.Is(err, ErrPolicyVersion) || changed {
			t.Errorf("%s: SetEnv = %v, %v, want ErrPolicyVersion", name, changed, err)
		}
	}
	// An environment with no row refuses a version it never had.
	if changed, err := f.save(t, "env-2", Overrides{KeepN: ptr(10)}, 3); !errors.Is(err, ErrPolicyVersion) || changed {
		t.Errorf("no row, version 3: SetEnv = %v, %v, want ErrPolicyVersion", changed, err)
	}
	o, v, err := f.ps.Env(ctx, "env-1")
	if err != nil || v != 2 || o.KeepN == nil || *o.KeepN != 6 {
		t.Errorf("stored = %+v v%d %v, want the second save untouched (keep 6, version 2)", o, v, err)
	}
	if _, v, _ := f.ps.Env(ctx, "env-2"); v != 0 {
		t.Errorf("env-2 version = %d, want no row written", v)
	}
	// With the version it read, the same save goes through.
	if changed, err := f.save(t, "env-1", Overrides{KeepN: ptr(10)}, 2); err != nil || !changed {
		t.Errorf("save at the current version = %v, %v, want changed", changed, err)
	}
}

func TestPolicyStoreSetEnvNoOp(t *testing.T) {
	t.Parallel()
	f := newPolicyFix(t)
	ctx := testsupport.Context(t)
	same := Overrides{KeepN: ptr(5), Schedule: ptr("0 4 * * *"), SchedulePreset: ptr(PresetCustom)}
	if _, err := f.save(t, "env-1", same, 0); err != nil {
		t.Fatal(err)
	}
	var at0 int64
	if err := f.st.Read.QueryRowContext(ctx, `SELECT updated_at FROM janitor_env_policy WHERE env_id = 'env-1'`).Scan(&at0); err != nil {
		t.Fatal(err)
	}
	changed, err := f.save(t, "env-1", same, 1)
	if err != nil || changed {
		t.Fatalf("identical save = %v, %v, want changed false", changed, err)
	}
	var v, at1 int64
	if err := f.st.Read.QueryRowContext(ctx, `SELECT version, updated_at FROM janitor_env_policy WHERE env_id = 'env-1'`).Scan(&v, &at1); err != nil {
		t.Fatal(err)
	}
	if v != 1 || at1 != at0 {
		t.Errorf("version %d updated_at %d (was %d), want the row untouched", v, at1, at0)
	}
	// Saving nothing for an environment with no row creates none.
	if changed, err := f.save(t, "env-2", Overrides{}, 0); err != nil || changed {
		t.Errorf("empty save with no row = %v, %v, want changed false", changed, err)
	}
	if _, v, _ := f.ps.Env(ctx, "env-2"); v != 0 {
		t.Errorf("env-2 version = %d, want no row", v)
	}
}

func TestPolicyStoreSetEnvInvalid(t *testing.T) {
	t.Parallel()
	f := newPolicyFix(t)
	ctx := testsupport.Context(t)
	for name, tc := range map[string]struct {
		o    Overrides
		want error
	}{
		"keep 21":         {Overrides{KeepN: ptr(21)}, ErrOutOfRange},
		"grace 0":         {Overrides{GraceHours: ptr(0)}, ErrOutOfRange},
		"grace 721":       {Overrides{GraceHours: ptr(721)}, ErrOutOfRange},
		"every minute":    {Overrides{Schedule: ptr("* * * * *")}, ErrScheduleTooFrequent},
		"not a schedule":  {Overrides{Schedule: ptr("nonsense")}, ErrInvalidSchedule},
		"one bad of many": {Overrides{KeepN: ptr(3), GraceHours: ptr(900)}, ErrOutOfRange},
	} {
		changed, err := f.save(t, "env-1", tc.o, 0)
		if !errors.Is(err, tc.want) || changed {
			t.Errorf("%s: SetEnv = %v, %v, want %v", name, changed, err, tc.want)
		}
	}
	if _, v, _ := f.ps.Env(ctx, "env-1"); v != 0 {
		t.Errorf("version = %d, want nothing written", v)
	}
	// The CHECK constraints are the second line: a value past them cannot be stored by any path.
	if _, err := f.st.Write.ExecContext(ctx, `INSERT INTO janitor_env_policy (env_id, keep_n) VALUES ('env-1', 99)`); err == nil {
		t.Error("the database accepted keep_n 99")
	}
	if _, err := f.st.Write.ExecContext(ctx, `UPDATE janitor_defaults SET grace_hours = 0 WHERE id = 1`); err == nil {
		t.Error("the database accepted grace_hours 0")
	}
}
