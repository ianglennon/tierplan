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

// setDefaults changes three of the instance defaults, as the settings page will.
func (f policyFix) setDefaults(t *testing.T, keepN, graceHours int, unusedNetworks bool) {
	t.Helper()
	if _, err := f.st.Write.ExecContext(testsupport.Context(t),
		`UPDATE janitor_defaults SET keep_n = ?, grace_hours = ?, remove_unused_networks = ? WHERE id = 1`, keepN, graceHours, unusedNetworks); err != nil {
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
	f.setDefaults(t, 4, 48, true)

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

// saveDefaults runs SetDefaults in a transaction of its own and commits it unless it failed.
func (f policyFix) saveDefaults(t *testing.T, p Policy, r Retention, version int64) (removed int64, changed bool, err error) {
	t.Helper()
	ctx := testsupport.Context(t)
	tx, err := f.st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	removed, changed, err = f.ps.SetDefaults(ctx, f.st.Q.WithTx(tx), p, r, version, "ada", time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		_ = tx.Rollback()
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return removed, changed, nil
}

// An instance admin's change of the defaults reaches the environment that follows the field and not the one
// that sets it (plan 11-21, D-04), and is stored with its version, author and time.
func TestPolicyStoreSetDefaults(t *testing.T) {
	t.Parallel()
	f := newPolicyFix(t)
	ctx := testsupport.Context(t)
	if _, err := f.save(t, "env-2", Overrides{KeepN: ptr(7)}, 0); err != nil {
		t.Fatal(err)
	}
	pol, _, version, err := f.ps.Defaults(ctx)
	if err != nil || version != 1 {
		t.Fatalf("Defaults = %v, v%d", err, version)
	}
	pol.KeepN, pol.Grace, pol.RemoveUnusedNetworks, pol.Schedule, pol.SchedulePreset = 0, 48*time.Hour, true, "0 4 * * 1", PresetCustom
	ret := Retention{Days: 30, Runs: 20}
	removed, changed, err := f.saveDefaults(t, pol, ret, 1)
	if err != nil || !changed || removed != 0 {
		t.Fatalf("SetDefaults = %d, %v, %v, want changed with nothing removed", removed, changed, err)
	}
	got, gotRet, version, err := f.ps.Defaults(ctx)
	if err != nil || got != pol || gotRet != ret || version != 2 {
		t.Fatalf("stored = %+v %+v v%d %v, want what was saved at version 2", got, gotRet, version, err)
	}
	var by string
	var at int64
	if err := f.st.Read.QueryRowContext(ctx, `SELECT updated_by, updated_at FROM janitor_defaults WHERE id = 1`).Scan(&by, &at); err != nil || by != "ada" || at == 0 {
		t.Errorf("updated_by %q updated_at %d (%v), want the author and the time", by, at, err)
	}
	if e := f.effective(t, "env-1"); e.KeepN != 0 || e.Grace != 48*time.Hour || !e.RemoveUnusedNetworks || e.Schedule != "0 4 * * 1" {
		t.Errorf("env-1 (follows) = %+v, want the new defaults", e)
	}
	if e := f.effective(t, "env-2"); e.KeepN != 7 || e.Grace != 48*time.Hour {
		t.Errorf("env-2 (keeps its own keep-N) = %+v, want keep 7 and the new grace", e)
	}
	if rt, err := f.ps.Retention(ctx); err != nil || rt != ret {
		t.Errorf("Retention = %+v, %v, want %+v", rt, err, ret)
	}
}

func TestPolicyStoreFollowers(t *testing.T) {
	t.Parallel()
	f := newPolicyFix(t)
	ctx := testsupport.Context(t)
	count := func() int {
		t.Helper()
		n, err := f.ps.Followers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 2 {
		t.Fatalf("followers with no policy row = %d, want both environments", n)
	}
	// A row that sets one field still follows the others.
	if _, err := f.save(t, "env-1", Overrides{KeepN: ptr(1)}, 0); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Errorf("followers with one override = %d, want 2", n)
	}
	// An environment that sets every field follows nothing.
	all := Overrides{
		Enabled: ptr(true), Schedule: ptr("0 5 * * *"), SchedulePreset: ptr(PresetCustom), KeepN: ptr(1), GraceHours: ptr(5),
		RemoveUnusedNetworks: ptr(false), RemoveStoppedContainers: ptr(false),
	}
	if _, err := f.save(t, "env-1", all, 1); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Errorf("followers with env-1 setting everything = %d, want 1", n)
	}
	// Taking one field back makes it follow again.
	all.GraceHours = nil
	if _, err := f.save(t, "env-1", all, 2); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Errorf("followers after env-1 follows the grace again = %d, want 2", n)
	}
}

func TestPolicyStoreSetDefaultsStaleNoOpInvalid(t *testing.T) {
	t.Parallel()
	f := newPolicyFix(t)
	ctx := testsupport.Context(t)
	pol, ret, _, err := f.ps.Defaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Saving what is stored writes nothing: the version and the time stay.
	if removed, changed, err := f.saveDefaults(t, pol, ret, 1); err != nil || changed || removed != 0 {
		t.Fatalf("identical save = %d, %v, %v, want changed false", removed, changed, err)
	}
	if _, _, v, _ := f.ps.Defaults(ctx); v != 1 {
		t.Errorf("version = %d after a no-op, want 1", v)
	}
	// A save from another version is refused, even one that would change nothing.
	changedPol := pol
	changedPol.KeepN = 9
	for _, version := range []int64{0, 2} {
		if _, changed, err := f.saveDefaults(t, changedPol, ret, version); !errors.Is(err, ErrPolicyVersion) || changed {
			t.Errorf("version %d: SetDefaults = %v, %v, want ErrPolicyVersion", version, changed, err)
		}
	}
	if _, _, err := f.saveDefaults(t, pol, ret, 5); !errors.Is(err, ErrPolicyVersion) {
		t.Errorf("a no-op from the wrong version = %v, want ErrPolicyVersion", err)
	}
	bad := func(mut func(*Policy, *Retention)) (Policy, Retention) {
		p, r := pol, ret
		mut(&p, &r)
		return p, r
	}
	for name, tc := range map[string]struct {
		mut  func(*Policy, *Retention)
		want error
	}{
		"keep 21":       {func(p *Policy, _ *Retention) { p.KeepN = 21 }, ErrOutOfRange},
		"keep -1":       {func(p *Policy, _ *Retention) { p.KeepN = -1 }, ErrOutOfRange},
		"grace 0":       {func(p *Policy, _ *Retention) { p.Grace = 0 }, ErrOutOfRange},
		"grace 90 mins": {func(p *Policy, _ *Retention) { p.Grace = 90 * time.Minute }, ErrOutOfRange},
		"every minute":  {func(p *Policy, _ *Retention) { p.Schedule = "* * * * *" }, ErrScheduleTooFrequent},
		"no schedule":   {func(p *Policy, _ *Retention) { p.Schedule = "" }, ErrInvalidSchedule},
		"no preset":     {func(p *Policy, _ *Retention) { p.SchedulePreset = "" }, ErrInvalidSchedule},
		"days 6":        {func(_ *Policy, r *Retention) { r.Days = 6 }, ErrOutOfRange},
		"days 3651":     {func(_ *Policy, r *Retention) { r.Days = 3651 }, ErrOutOfRange},
		"runs 9":        {func(_ *Policy, r *Retention) { r.Runs = 9 }, ErrOutOfRange},
		"runs 1001":     {func(_ *Policy, r *Retention) { r.Runs = 1001 }, ErrOutOfRange},
	} {
		p, r := bad(tc.mut)
		if _, changed, err := f.saveDefaults(t, p, r, 1); !errors.Is(err, tc.want) || changed {
			t.Errorf("%s: SetDefaults = %v, %v, want %v", name, changed, err, tc.want)
		}
	}
	if got, gotRet, v, _ := f.ps.Defaults(ctx); got != pol || gotRet != ret || v != 1 {
		t.Errorf("stored = %+v %+v v%d, want the seeded defaults untouched", got, gotRet, v)
	}
}
