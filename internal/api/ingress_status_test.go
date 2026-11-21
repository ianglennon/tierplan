// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The ingress status, access-log and resume routes (Phase 24, D-10, D-30, D-42).

type statusResp struct {
	Enabled      bool `json:"enabled"`
	Paused       bool `json:"paused"`
	AccessLogs   bool `json:"accessLogs"`
	ObjectsKnown bool `json:"objectsKnown"`
	Objects      struct {
		Network, Service, Secret bool
	} `json:"objects"`
	Nodes struct {
		Expected, Enrolled, Ready int
	} `json:"nodes"`
	LastEnsure struct {
		At, Class string
	} `json:"lastEnsure"`
	PendingWipes        int  `json:"pendingWipes"`
	DisablingIncomplete bool `json:"disablingIncomplete"`
	TokenDisabled       bool `json:"tokenDisabled"`
	Redeploy            []struct {
		StackID, StackName string
		HoldsClaims        bool
	} `json:"redeploy"`
}

// enableViaRoute switches ingress on through the route and waits for the job.
func (f *deployFix) enableViaRoute(t *testing.T) {
	t.Helper()
	rec := f.credCall(t, f.adminA, http.MethodPost, "/api/env/"+f.envA+"/ingress/enable", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	start := decodeBody[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	if _, err := f.hub.Wait(testsupport.Context(t), start.JobID); err != nil {
		t.Fatalf("wait for the enable job: %v", err)
	}
}

// TestIngressStatusRoute: an Admin reads the status of a new environment and of an enabled one, the body carries no token
// or secret field, an Operator is refused and a standalone environment is a 409.
func TestIngressStatusRoute(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, false)
	path := "/api/env/" + f.envA + "/ingress/status"

	rec := f.credCall(t, f.adminA, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	st := decodeBody[statusResp](t, rec)
	if st.Enabled || st.Paused || st.AccessLogs || !st.ObjectsKnown || st.Objects.Network || st.Objects.Service || st.Objects.Secret || st.DisablingIncomplete {
		t.Errorf("a new environment's status = %+v, want everything off (accessLogs false by default)", st)
	}
	if st.Nodes.Expected != 1 || st.Redeploy == nil {
		t.Errorf("nodes = %+v, redeploy = %v; want one expected node and an empty list", st.Nodes, st.Redeploy)
	}

	f.enableViaRoute(t)
	rec = f.credCall(t, f.adminA, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status after enable: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	st = decodeBody[statusResp](t, rec)
	if !st.Enabled || !st.Objects.Network || !st.Objects.Service || !st.Objects.Secret || st.TokenDisabled {
		t.Errorf("enabled status = %+v, want on with all three objects", st)
	}
	for _, banned := range []string{"\"token\":", "\"secretName\"", "\"secretId\"", "\"password\"", "\"joinToken\""} {
		if strings.Contains(body, banned) {
			t.Errorf("the status body carries %s: %s", banned, body)
		}
	}

	if rec := f.credCall(t, f.operatorA, http.MethodGet, path, ""); rec.Code != http.StatusForbidden {
		t.Errorf("an Operator's status read = %d, want 403", rec.Code)
	}
	if rec := f.credCall(t, f.adminB, http.MethodGet, path, ""); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("another environment's Admin's status read = %d, want 403 or 404", rec.Code)
	}

	sa := newDeployFix(t, true)
	rec = sa.credCall(t, sa.adminA, http.MethodGet, "/api/env/"+sa.envA+"/ingress/status", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("standalone status: %d %s, want 409", rec.Code, rec.Body)
	}
	if typ, _ := deployProblem(t, rec.Body.Bytes()); !strings.HasSuffix(typ, "ingress_standalone_unavailable") {
		t.Errorf("standalone problem type = %q, want ingress_standalone_unavailable", typ)
	}
}
