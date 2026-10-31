// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"fmt"
	"net/http"
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
