// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// The janitor policy of an environment (plan 11-19, JAN-01, D-04, D-15, D-21): each field follows the
// instance default until the environment sets it, and a save is checked, versioned and audited.

type policyBool struct {
	Value      bool `json:"value"`
	Overridden bool `json:"overridden"`
}

type policyInt struct {
	Value      int  `json:"value"`
	Overridden bool `json:"overridden"`
}

type janitorPolicyAnswer struct {
	Version  int64      `json:"version"`
	Timezone string     `json:"timezone"`
	Enabled  policyBool `json:"enabled"`
	Schedule struct {
		Value      string `json:"value"`
		Preset     string `json:"preset"`
		Overridden bool   `json:"overridden"`
	} `json:"schedule"`
	KeepN                   policyInt  `json:"keepN"`
	GraceHours              policyInt  `json:"graceHours"`
	RemoveUnusedNetworks    policyBool `json:"removeUnusedNetworks"`
	RemoveStoppedContainers policyBool `json:"removeStoppedContainers"`
	Defaults                struct {
		Enabled                 bool   `json:"enabled"`
		Schedule                string `json:"schedule"`
		SchedulePreset          string `json:"schedulePreset"`
		KeepN                   int    `json:"keepN"`
		GraceHours              int    `json:"graceHours"`
		RemoveUnusedNetworks    bool   `json:"removeUnusedNetworks"`
		RemoveStoppedContainers bool   `json:"removeStoppedContainers"`
	} `json:"defaults"`
}

// policyJSON is a save body that sets nothing but what is given: every other field is null, so it follows the default.
func policyJSON(version int64, fields map[string]any) string {
	body := map[string]any{
		"version": version, "enabled": nil, "schedule": nil, "schedulePreset": nil, "keepN": nil, "graceHours": nil,
		"removeUnusedNetworks": nil, "removeStoppedContainers": nil,
	}
	for k, v := range fields {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (f *envFix) putJanitorPolicy(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	return f.req(t, http.MethodPut, "/api/env/"+e.ID+"/janitor/policy", body)
}

func (f *envFix) readJanitorPolicy(t *testing.T, e envBody) janitorPolicyAnswer {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	rec := f.req(t, http.MethodGet, "/api/env/"+e.ID+"/janitor/policy", "")
	if rec.code != http.StatusOK {
		t.Fatalf("policy: %d %s", rec.code, rec.body)
	}
	return decode[janitorPolicyAnswer](t, rec)
}

type policyAudit struct {
	TargetType, TargetID, EnvID, Result string
	Details                             map[string]string
}

func (f *envFix) policyAudits(t *testing.T) []policyAudit {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT target_type, target_id, COALESCE(env_id, ''), result, COALESCE(details, '{}')
		FROM audit_log WHERE action = 'janitor.policy_changed' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []policyAudit
	for rows.Next() {
		var r policyAudit
		var details string
		if err := rows.Scan(&r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The tracer (plan 11-19, JAN-01, D-04, D-11): a stopped docker run container two days old is only
// reported while the environment follows the default, and turning the opt-in on through the API makes
// the next preview list it as removable. The save is one audit row.
func TestJanitorPolicyOptInChangesPreview(t *testing.T) {
	t.Parallel()
	cfg := orphanEngine()
	cfg.Containers = []dockertest.FakeContainer{standaloneBox("scratch", 48*time.Hour, nil)}
	f, e := dashEnv(t, cfg)

	before := f.previewJanitor(t, e)
	if len(before.Items) != 0 || len(before.NotRemoved) != 1 || before.NotRemoved[0].Class != "reported" || before.NotRemoved[0].Reason.Code != "opt_in_off_containers" {
		t.Fatalf("preview before = %+v notRemoved = %+v, want the container reported as opt_in_off_containers", before.Items, before.NotRemoved)
	}
	pol := f.readJanitorPolicy(t, e)
	if pol.Version != 0 || pol.RemoveStoppedContainers.Value || pol.RemoveStoppedContainers.Overridden || !pol.Enabled.Value || pol.KeepN.Value != 2 || pol.GraceHours.Value != 24 {
		t.Fatalf("policy = %+v, want the seeded defaults, nothing overridden, version 0", pol)
	}

	rec := f.putJanitorPolicy(t, e, policyJSON(0, map[string]any{"removeStoppedContainers": true}))
	if rec.code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.code, rec.body)
	}
	saved := decode[janitorPolicyAnswer](t, rec)
	if saved.Version != 1 || !saved.RemoveStoppedContainers.Value || !saved.RemoveStoppedContainers.Overridden || saved.KeepN.Overridden || saved.Enabled.Overridden || saved.Schedule.Overridden {
		t.Fatalf("saved = %+v, want version 1 with only the opt-in overridden", saved)
	}

	after := f.previewJanitor(t, e)
	if len(after.Items) != 1 || after.Summary.Containers != 1 || len(after.NotRemoved) != 0 {
		t.Fatalf("preview after = %+v notRemoved = %+v, want the container removable", after.Items, after.NotRemoved)
	}
	if it := after.Items[0]; it.Class != "removable" || it.Reason.Code != "stopped_standalone" || it.Ref != "scratch" {
		t.Errorf("item = %+v, want a removable stopped_standalone container", it)
	}

	rows := f.policyAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	if r := rows[0]; r.TargetType != "environment" || r.TargetID != e.ID || r.EnvID != e.ID || r.Result != "success" ||
		r.Details["fields"] != "removeStoppedContainers" || r.Details["version"] != "1" || len(r.Details) != 2 {
		t.Errorf("audit row = %+v, want the environment, success, fields removeStoppedContainers, version 1 and nothing else", r)
	}
}
