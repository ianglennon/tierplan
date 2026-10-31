// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/audit"
)

// The address-space settings (plan 10-21, NET-06, D-21, D-07 as amended). Docker is a fake daemon;
// the router, policy, store and audit log are the real ones.

type settingsBody struct {
	SmallSubnets struct {
		Enabled bool `json:"enabled"`
		Mask    int  `json:"mask"`
	} `json:"smallSubnets"`
	Pools []struct {
		Base string `json:"base"`
		Size int    `json:"size"`
	} `json:"pools"`
	Version   int64  `json:"version"`
	ChangedBy string `json:"changedBy"`
}

func (f *envFix) settings(t *testing.T, e envBody) settingsBody {
	t.Helper()
	rec := f.get(t, e, "address-space/settings")
	if rec.code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.code, rec.body)
	}
	return decode[settingsBody](t, rec)
}

func (f *envFix) putSettings(t *testing.T, e envBody, what, body string) *envRec {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	return f.req(t, http.MethodPut, "/api/env/"+e.ID+"/address-space/"+what, body)
}

func smallBody(enabled bool, mask int, version int64) string {
	return fmt.Sprintf(`{"enabled":%v,"mask":%d,"version":%d}`, enabled, mask, version)
}

func TestSmallSubnetsSaveAndRead(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	got := f.settings(t, e)
	if got.SmallSubnets.Enabled || got.SmallSubnets.Mask != 26 || len(got.Pools) != 0 || got.Version != 0 || got.ChangedBy != "" {
		t.Fatalf("defaults = %+v, want off at /26, no pools, version 0 and nobody", got)
	}
	rec := f.putSettings(t, e, "small-subnets", smallBody(true, 27, 0))
	if rec.code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.code, rec.body)
	}
	saved := decode[settingsBody](t, rec)
	if !saved.SmallSubnets.Enabled || saved.SmallSubnets.Mask != 27 || saved.Version != 1 || saved.ChangedBy != "admin" {
		t.Errorf("answer = %+v, want on at /27, version 1, changed by admin", saved)
	}
	if read := f.settings(t, e); read.SmallSubnets.Mask != 27 || !read.SmallSubnets.Enabled || read.Version != 1 {
		t.Errorf("read back = %+v", read)
	}
	rows := f.auditRows(t, audit.ActionEnvironmentSmallSubnetsChanged)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one environment.small_subnets_changed", rows)
	}
	r := rows[0]
	if r.TargetType != "environment" || r.TargetID != e.ID || r.EnvID != e.ID || r.Result != "success" ||
		r.Details["enabled"] != "true" || r.Details["mask"] != "27" || r.Details["previous_enabled"] != "false" || r.Details["previous_mask"] != "26" {
		t.Errorf("audit row = %+v", r)
	}
}

func TestSmallSubnetsNeedsEnvAdmin(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	op := loopAs(t, f, e, "operator", "operator")
	if rec := op(http.MethodPut, "address-space/small-subnets", smallBody(true, 27, 0)); rec.code != http.StatusForbidden {
		t.Errorf("PUT as an Operator: %d %s, want 403", rec.code, rec.body)
	}
	viewer := loopAs(t, f, e, "viewer", "viewer")
	if rec := viewer(http.MethodPut, "address-space/small-subnets", smallBody(true, 27, 0)); rec.code != http.StatusForbidden {
		t.Errorf("PUT as a Viewer: %d, want 403", rec.code)
	}
	if rec := viewer(http.MethodGet, "address-space/settings", ""); rec.code != http.StatusOK {
		t.Errorf("GET as a Viewer: %d %s, want 200 (networks.read)", rec.code, rec.body)
	}
	if got := f.settings(t, e); got.SmallSubnets.Enabled || got.Version != 0 {
		t.Errorf("settings = %+v: a refused save changed them", got)
	}
	if rows := f.auditRows(t, audit.ActionEnvironmentSmallSubnetsChanged); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none from refused saves", rows)
	}
	admin := loopAs(t, f, e, "envadmin", "admin")
	if rec := admin(http.MethodPut, "address-space/small-subnets", smallBody(true, 28, 0)); rec.code != http.StatusOK {
		t.Errorf("PUT as an Env Admin: %d %s, want 200", rec.code, rec.body)
	}
}

func TestSmallSubnetsMaskRange(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	for _, mask := range []int{24, 29} {
		if rec := f.putSettings(t, e, "small-subnets", smallBody(true, mask, 0)); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("mask %d: %d %s, want 422", mask, rec.code, rec.body)
		}
	}
	version := int64(0)
	for _, mask := range []int{25, 28} {
		rec := f.putSettings(t, e, "small-subnets", smallBody(true, mask, version))
		if rec.code != http.StatusOK {
			t.Fatalf("mask %d: %d %s, want 200", mask, rec.code, rec.body)
		}
		version = decode[settingsBody](t, rec).Version
	}
	if got := f.settings(t, e); got.SmallSubnets.Mask != 28 || got.Version != 2 {
		t.Errorf("settings = %+v, want /28 at version 2: the refused masks must not have been stored", got)
	}
}

func TestSmallSubnetsNoChangeWritesNothing(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	if rec := f.putSettings(t, e, "small-subnets", smallBody(false, 26, 0)); rec.code != http.StatusOK {
		t.Fatalf("save of the stored values: %d %s", rec.code, rec.body)
	}
	if rows := f.auditRows(t, audit.ActionEnvironmentSmallSubnetsChanged); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none", rows)
	}
	if got := f.settings(t, e); got.Version != 0 || got.ChangedBy != "" {
		t.Errorf("settings = %+v, want version 0 and nobody: nothing was changed", got)
	}
	if rec := f.putSettings(t, e, "small-subnets", smallBody(true, 26, 0)); rec.code != http.StatusOK {
		t.Fatalf("save: %d", rec.code)
	}
	if rec := f.putSettings(t, e, "small-subnets", smallBody(true, 26, 1)); rec.code != http.StatusOK {
		t.Fatalf("second save of the same values: %d %s", rec.code, rec.body)
	}
	if rows := f.auditRows(t, audit.ActionEnvironmentSmallSubnetsChanged); len(rows) != 1 {
		t.Errorf("audit rows = %d, want one: the repeat changed nothing", len(rows))
	}
	if got := f.settings(t, e); got.Version != 1 {
		t.Errorf("version = %d, want 1", got.Version)
	}
}

func poolsBody(version int64, pools ...string) string {
	list := ""
	for i, p := range pools {
		if i > 0 {
			list += ","
		}
		list += p
	}
	return fmt.Sprintf(`{"pools":[%s],"version":%d}`, list, version)
}

func pool(base string, size int) string { return fmt.Sprintf(`{"base":%q,"size":%d}`, base, size) }

func TestAddressPoolsSaveAndRead(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	rec := f.putSettings(t, e, "pools", poolsBody(0, pool("10.20.0.0/16", 24)))
	if rec.code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.code, rec.body)
	}
	if got := f.settings(t, e); len(got.Pools) != 1 || got.Pools[0].Base != "10.20.0.0/16" || got.Pools[0].Size != 24 || got.Version != 1 || got.ChangedBy != "admin" {
		t.Fatalf("settings = %+v, want the one pool at version 1 saved by admin", got)
	}
	// The standalone read now answers the entered pool, estimated, and not the engine's own.
	awaitOnline(t, f.mon, e.ID)
	space := decode[addressSpace](t, f.get(t, e, "address-space"))
	if len(space.Pools) != 1 || space.Pools[0].CIDR != "10.20.0.0/16" || space.Pools[0].Mask != 24 || space.Pools[0].Source != "entered" || !space.Pools[0].Estimated {
		t.Fatalf("pools = %+v, want the entered pool, source entered, estimated", space.Pools)
	}
	rows := f.auditRows(t, audit.ActionEnvironmentAddressPoolsSaved)
	if len(rows) != 1 || rows[0].Details["pools"] != "10.20.0.0/16/24" || rows[0].Details["previous_pools"] != "" || rows[0].EnvID != e.ID {
		t.Fatalf("audit rows = %+v, want one environment.address_pools_saved", rows)
	}
	// Clearing the pools goes back to Docker's defaults, at once.
	if rec := f.putSettings(t, e, "pools", poolsBody(1)); rec.code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.code, rec.body)
	}
	space = decode[addressSpace](t, f.get(t, e, "address-space"))
	if len(space.Pools) < 2 || space.Pools[0].Source != "defaults" || !space.Pools[0].Estimated {
		t.Errorf("pools = %+v after clearing, want Docker's defaults", space.Pools)
	}
	rows = f.auditRows(t, audit.ActionEnvironmentAddressPoolsSaved)
	if len(rows) != 2 || rows[1].Details["pools"] != "" || rows[1].Details["previous_pools"] != "10.20.0.0/16/24" {
		t.Errorf("audit rows = %+v, want the clear with the pools it replaced", rows)
	}
	// Saving what is stored writes nothing.
	if rec := f.putSettings(t, e, "pools", poolsBody(2)); rec.code != http.StatusOK {
		t.Fatalf("repeat: %d", rec.code)
	}
	if n := len(f.auditRows(t, audit.ActionEnvironmentAddressPoolsSaved)); n != 2 {
		t.Errorf("audit rows = %d, want 2: the repeat changed nothing", n)
	}
}

func TestAddressPoolsValidation(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	cases := []struct {
		name, body, ptyp, loc string
	}{
		{"host bits in the base", poolsBody(0, pool("10.20.0.1/16", 24)), "pool_base_invalid", "body.pools[0].base"},
		{"not a range", poolsBody(0, pool("ten", 24)), "pool_base_invalid", "body.pools[0].base"},
		{"an IPv6 base", poolsBody(0, pool("fd00::/48", 64)), "pool_base_invalid", "body.pools[0].base"},
		{"size narrower than the base", poolsBody(0, pool("10.20.0.0/16", 12)), "pool_size_invalid", "body.pools[0].size"},
		{"size over 30", poolsBody(0, pool("10.20.0.0/16", 31)), "pool_size_invalid", "body.pools[0].size"},
		{"the bad row is named", poolsBody(0, pool("10.20.0.0/16", 24), pool("10.30.0.0/16", 8)), "pool_size_invalid", "body.pools[1].size"},
		{"overlapping pools", poolsBody(0, pool("10.20.0.0/16", 24), pool("10.20.128.0/17", 26)), "pool_overlap", "body.pools[1].base"},
	}
	for _, tc := range cases {
		rec := f.putSettings(t, e, "pools", tc.body)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != tc.ptyp || !strings.Contains(string(rec.body), tc.loc) {
			t.Errorf("%s: %d %q %s, want 422 %s at %s", tc.name, rec.code, rec.ptyp, rec.body, tc.ptyp, tc.loc)
		}
	}
	var many []string
	for i := range 17 {
		many = append(many, pool(fmt.Sprintf("10.%d.0.0/16", i), 24))
	}
	if rec := f.putSettings(t, e, "pools", poolsBody(0, many...)); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("17 pools: %d %s, want 422", rec.code, rec.body)
	}
	if rec := f.putSettings(t, e, "pools", poolsBody(0, many[:16]...)); rec.code != http.StatusOK {
		t.Errorf("16 pools: %d %s, want 200", rec.code, rec.body)
	}
	if got := f.settings(t, e); len(got.Pools) != 16 || got.Version != 1 {
		t.Errorf("settings = %d pools at version %d: a refused list must store nothing", len(got.Pools), got.Version)
	}
	if rows := f.auditRows(t, audit.ActionEnvironmentAddressPoolsSaved); len(rows) != 1 {
		t.Errorf("audit rows = %d, want only the accepted save", len(rows))
	}
}

func TestAddressPoolsSwarmRefused(t *testing.T) {
	t.Parallel()
	s := newSwarmFixWith(t, nil)
	raw := s.call(t, authReq{
		method: http.MethodPut, path: "/api/env/" + s.env.ID + "/address-space/pools",
		body: poolsBody(0, pool("10.20.0.0/16", 24)), cookie: s.cookie, csrf: s.csrf,
	})
	if raw.Code != http.StatusConflict || problemType(t, raw) != "not_standalone" {
		t.Fatalf("swarm: %d %s, want 409 not_standalone", raw.Code, raw.Body)
	}
	var n int
	if err := s.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'environment.address_pools_saved'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("audit rows = %d (%v), want none", n, err)
	}
}

func TestAddressSettingsVersionConflict(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	for i := range 3 {
		mask := 26 + i%2 + 1
		if rec := f.putSettings(t, e, "small-subnets", smallBody(true, mask, int64(i))); rec.code != http.StatusOK {
			t.Fatalf("save %d: %d %s", i, rec.code, rec.body)
		}
	}
	// Two admins read version 3.
	second := loopAs(t, f, e, "second", "admin")
	if got := f.settings(t, e); got.Version != 3 {
		t.Fatalf("version = %d, want 3", got.Version)
	}
	if rec := f.putSettings(t, e, "pools", poolsBody(3, pool("10.20.0.0/16", 24))); rec.code != http.StatusOK {
		t.Fatalf("first admin's save: %d %s", rec.code, rec.body)
	}
	for what, body := range map[string]string{
		"pools":         poolsBody(3, pool("10.30.0.0/16", 24)),
		"small-subnets": smallBody(false, 28, 3),
	} {
		rec := second(http.MethodPut, "address-space/"+what, body)
		if rec.code != http.StatusConflict || rec.ptyp != "settings_changed" || !strings.Contains(string(rec.body), "admin") {
			t.Errorf("second admin's %s save: %d %q %s, want 409 settings_changed naming admin", what, rec.code, rec.ptyp, rec.body)
		}
	}
	got := f.settings(t, e)
	if len(got.Pools) != 1 || got.Pools[0].Base != "10.20.0.0/16" || got.Version != 4 {
		t.Errorf("settings = %+v: the refused saves must not have changed anything", got)
	}
}
