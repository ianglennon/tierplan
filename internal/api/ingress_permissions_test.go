// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The permission matrix of the ingress API (Phase 27, UIX-05, D-14 to D-16, A-02). One row per ingress operation of the
// committed openapi.json, plus the job stream the document does not list. Each row names the permission its gate holds, and the
// test walks the roles through the real router: who is refused is read off the permission, so a row that is wrong, or an
// operation without a row, fails the build.

// ingressPerm is the permission a row's gate holds, which decides who passes it.
type ingressPerm string

const (
	permIngressRead    ingressPerm = "ingress.read"        // Viewer and above
	permIngressManage  ingressPerm = "ingress.manage"      // Admin of the environment
	permEnvRevoke      ingressPerm = "environments.revoke" // Admin of the environment (D-16)
	permSettingsRead   ingressPerm = "settings.read"       // global: instance admin only (D-16)
	permSettingsWrite  ingressPerm = "settings.write"      // global: instance admin only (D-16)
	ingressStreamRowOp             = "ingressJobStream"    // the raw route that is not in the document
)

// ingressPermRow is one ingress operation.
type ingressPermRow struct {
	op, method, path string
	perm             ingressPerm
	body             string
}

// ingressPermRows is the table. {env} stands for the first environment.
var ingressPermRows = []ingressPermRow{
	{op: "setIngressAccessLogs", method: http.MethodPut, path: "/api/env/{env}/ingress/access-logs", perm: permIngressManage, body: `{}`},
	{op: "listIngressCertificates", method: http.MethodGet, path: "/api/env/{env}/ingress/certificates", perm: permIngressRead},
	{op: "requestIngressCertificate", method: http.MethodPost, path: "/api/env/{env}/ingress/certificates", perm: permIngressManage, body: `{}`},
	{op: "getIngressCertificateBudget", method: http.MethodGet, path: "/api/env/{env}/ingress/certificates/budget", perm: permIngressRead},
	{op: "uploadIngressCertificate", method: http.MethodPost, path: "/api/env/{env}/ingress/certificates/upload", perm: permIngressManage, body: `{}`},
	{op: "switchIngressCertificateToACME", method: http.MethodPost, path: "/api/env/{env}/ingress/certificates/cert-x/acme", perm: permIngressManage, body: `{}`},
	{op: "retryIngressCertificate", method: http.MethodPost, path: "/api/env/{env}/ingress/certificates/cert-x/retry", perm: permIngressManage, body: `{}`},
	{op: "disableIngress", method: http.MethodPost, path: "/api/env/{env}/ingress/disable", perm: permIngressManage, body: `{}`},
	{op: "previewIngressDisable", method: http.MethodPost, path: "/api/env/{env}/ingress/disable/preview", perm: permIngressManage, body: `{}`},
	{op: "listIngressDomains", method: http.MethodGet, path: "/api/env/{env}/ingress/domains", perm: permIngressRead},
	{op: "createIngressDomain", method: http.MethodPost, path: "/api/env/{env}/ingress/domains", perm: permIngressManage, body: `{}`},
	{op: "deleteIngressDomain", method: http.MethodDelete, path: "/api/env/{env}/ingress/domains/dom-x", perm: permIngressManage},
	{op: "previewIngressDomainIssuer", method: http.MethodGet, path: "/api/env/{env}/ingress/domains/dom-x/issuer", perm: permIngressManage},
	{op: "changeIngressDomainIssuer", method: http.MethodPut, path: "/api/env/{env}/ingress/domains/dom-x/issuer", perm: permIngressManage, body: `{}`},
	{op: "reserveIngressHostname", method: http.MethodPost, path: "/api/env/{env}/ingress/domains/dom-x/reserved", perm: permIngressManage, body: `{}`},
	{op: "unreserveIngressHostname", method: http.MethodDelete, path: "/api/env/{env}/ingress/domains/dom-x/reserved/app.example.com", perm: permIngressManage},
	{op: "replaceIngressDomainToken", method: http.MethodPut, path: "/api/env/{env}/ingress/domains/dom-x/token", perm: permIngressManage, body: `{}`},
	{op: "enableIngress", method: http.MethodPost, path: "/api/env/{env}/ingress/enable", perm: permIngressManage, body: `{}`},
	{op: "previewIngressEnable", method: http.MethodPost, path: "/api/env/{env}/ingress/enable/preview", perm: permIngressManage, body: `{}`},
	{op: "getIngressJob", method: http.MethodGet, path: "/api/env/{env}/ingress/jobs/job-x", perm: permIngressManage},
	{op: ingressStreamRowOp, method: http.MethodGet, path: "/api/env/{env}/ingress/jobs/job-x/stream", perm: permIngressManage},
	{op: "disableIngressJoinToken", method: http.MethodDelete, path: "/api/env/{env}/ingress/join-token", perm: permIngressManage},
	{op: "getIngressJoinToken", method: http.MethodGet, path: "/api/env/{env}/ingress/join-token", perm: permIngressRead},
	{op: "issueIngressJoinToken", method: http.MethodPost, path: "/api/env/{env}/ingress/join-token", perm: permIngressManage, body: `{}`},
	{op: "listIngressNodes", method: http.MethodGet, path: "/api/env/{env}/ingress/nodes", perm: permIngressRead},
	{op: "revokeIngressNode", method: http.MethodPost, path: "/api/env/{env}/ingress/nodes/node-x/revoke", perm: permEnvRevoke, body: `{}`},
	{op: "resumeIngress", method: http.MethodPost, path: "/api/env/{env}/ingress/resume", perm: permIngressManage, body: `{}`},
	{op: "listIngressRoutes", method: http.MethodGet, path: "/api/env/{env}/ingress/routes", perm: permIngressRead},
	{op: "getIngressStatus", method: http.MethodGet, path: "/api/env/{env}/ingress/status", perm: permIngressRead},
	{op: "disableIngressVIP", method: http.MethodDelete, path: "/api/env/{env}/ingress/vip", perm: permIngressManage},
	{op: "getIngressVIP", method: http.MethodGet, path: "/api/env/{env}/ingress/vip", perm: permIngressRead},
	{op: "enableIngressVIP", method: http.MethodPost, path: "/api/env/{env}/ingress/vip", perm: permIngressManage, body: `{}`},
	{op: "changeIngressVIP", method: http.MethodPut, path: "/api/env/{env}/ingress/vip", perm: permIngressManage, body: `{}`},
	{op: "previewIngressVIPDisable", method: http.MethodPost, path: "/api/env/{env}/ingress/vip/disable/preview", perm: permIngressManage, body: `{}`},
	{op: "listIngressVIPMoves", method: http.MethodGet, path: "/api/env/{env}/ingress/vip/moves", perm: permIngressRead},
	{op: "previewIngressVIP", method: http.MethodPost, path: "/api/env/{env}/ingress/vip/preview", perm: permIngressManage, body: `{}`},
	{op: "getIngressCertificateAccount", method: http.MethodGet, path: "/api/ingress/certificates/account", perm: permSettingsRead},
	{op: "acceptIngressCertificateTerms", method: http.MethodPut, path: "/api/ingress/certificates/account", perm: permSettingsWrite, body: `{}`},
	{op: "resumeIngressCertificates", method: http.MethodPost, path: "/api/ingress/certificates/resume", perm: permSettingsWrite, body: `{}`},
}

// ingressOperationsOfSpec reads every operationId tagged ingress from the committed openapi.json.
func ingressOperationsOfSpec(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../openapi.json")
	if err != nil {
		t.Fatalf("read openapi.json: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string   `json:"operationId"`
			Tags        []string `json:"tags"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode openapi.json: %v", err)
	}
	var ops []string
	for _, methods := range doc.Paths {
		for _, o := range methods {
			for _, tag := range o.Tags {
				if tag == "ingress" {
					ops = append(ops, o.OperationID)
				}
			}
		}
	}
	sort.Strings(ops)
	return ops
}

// ingressPermitted reports whether a role passes the gate of a permission. The environment's Admin holds the two environment
// permissions but not a global one, which is the instance admin's alone.
func ingressPermitted(perm ingressPerm, role string) bool {
	switch role {
	case "viewer", "operator":
		return perm == permIngressRead
	case "admin":
		return perm == permIngressRead || perm == permIngressManage || perm == permEnvRevoke
	case "instance":
		return true
	}
	return false
}

// TestIngressOperationsByRole: every ingress operation by every role. A Viewer and an Operator pass the ingress.read rows and
// are 403 on every other; the environment's Admin passes the environment rows and not the global ones; the instance admin passes
// the global ones; a user with no role on the environment is refused on every row, and an anonymous caller gets 401 on every
// row. A 404 or a 409 or a 422 past the gate counts as passed. The table is checked against openapi.json first.
func TestIngressOperationsByRole(t *testing.T) {
	t.Parallel()

	t.Run("the table covers every operation", func(t *testing.T) {
		t.Parallel()
		inTable := map[string]bool{}
		for _, r := range ingressPermRows {
			if inTable[r.op] {
				t.Errorf("operation %s has two rows", r.op)
			}
			inTable[r.op] = true
		}
		inSpec := map[string]bool{ingressStreamRowOp: true}
		for _, op := range ingressOperationsOfSpec(t) {
			inSpec[op] = true
			if !inTable[op] {
				t.Errorf("ingress operation %s has no row in the permission table: add it with the permission its gate holds", op)
			}
		}
		for op := range inTable {
			if !inSpec[op] {
				t.Errorf("the table has a row for %s, which openapi.json does not list as an ingress operation", op)
			}
		}
		if len(ingressPermRows) < 39 {
			t.Errorf("the table has %d rows, want at least 39", len(ingressPermRows))
		}
	})

	f := newDeployFix(t, false)
	viewer := f.asRoleOn(t, "ing-viewer-a", f.envA, "viewer")
	f.addUser(t, "ing-nobody", false)
	nobodyCookie, nobodyCSRF := f.signIn(t, "ing-nobody", false)
	nobody := credentialUser{nobodyCookie, nobodyCSRF}

	refused := func(code int) bool { return code == http.StatusForbidden || code == http.StatusNotFound }
	gate := func(code int) bool { return code == http.StatusUnauthorized || code == http.StatusForbidden }

	// The refusals first, then the callers who pass; a caller who passes may change the fixture, so those come last.
	for _, rt := range ingressPermRows {
		path := strings.ReplaceAll(rt.path, "{env}", f.envA)
		if rec := f.call(t, authReq{method: rt.method, path: path, body: rt.body}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: an anonymous caller got %d %s, want 401", rt.op, rec.Code, rec.Body)
		}
		if rec := f.credCall(t, nobody, rt.method, path, rt.body); !refused(rec.Code) {
			t.Errorf("%s: a user with no role got %d %s, want 403 or 404", rt.op, rec.Code, rec.Body)
		}
		if rec := f.credCall(t, f.adminB, rt.method, path, rt.body); !refused(rec.Code) {
			t.Errorf("%s: an Admin of another environment got %d %s, want 403 or 404", rt.op, rec.Code, rec.Body)
		}
		for role, u := range map[string]credentialUser{"viewer": viewer, "operator": f.operatorA} {
			rec := f.credCall(t, u, rt.method, path, rt.body)
			if ingressPermitted(rt.perm, role) == gate(rec.Code) {
				t.Errorf("%s (%s): a %s got %d %s, want the gate %s", rt.op, rt.perm, role, rec.Code, rec.Body, wantGate(ingressPermitted(rt.perm, role)))
			}
		}
	}
	for _, rt := range ingressPermRows {
		path := strings.ReplaceAll(rt.path, "{env}", f.envA)
		rec := f.credCall(t, f.adminA, rt.method, path, rt.body)
		if ingressPermitted(rt.perm, "admin") == gate(rec.Code) {
			t.Errorf("%s (%s): the Admin got %d %s, want the gate %s", rt.op, rt.perm, rec.Code, rec.Body, wantGate(ingressPermitted(rt.perm, "admin")))
		}
	}
	for _, rt := range ingressPermRows {
		if rt.perm != permSettingsRead && rt.perm != permSettingsWrite {
			continue
		}
		rec := f.credCall(t, f.admin, rt.method, rt.path, rt.body)
		if gate(rec.Code) {
			t.Errorf("%s (%s): the instance admin got %d %s, want the gate passed", rt.op, rt.perm, rec.Code, rec.Body)
		}
	}
}

func wantGate(passed bool) string {
	if passed {
		return "passed"
	}
	return "closed (403)"
}

// TestIngressJobsStayWithManagers: a job the Admin started is neither readable nor followable by a Viewer or an Operator of the
// environment (A-02): the job read and the stream are ingress.manage, so both are 403 before the job is looked up.
func TestIngressJobsStayWithManagers(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, false)
	viewer := f.asRoleOn(t, "ing-viewer-a", f.envA, "viewer")
	base := "/api/env/" + f.envA + "/ingress"

	rec := f.credCall(t, f.adminA, http.MethodPost, base+"/enable", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	start := decodeBody[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	if _, err := f.hub.Wait(testsupport.Context(t), start.JobID); err != nil {
		t.Fatalf("wait for the job: %v", err)
	}

	for _, rest := range []string{"/jobs/" + start.JobID, "/jobs/" + start.JobID + "/stream"} {
		for name, u := range map[string]credentialUser{"Viewer": viewer, "Operator": f.operatorA} {
			if rec := f.credCall(t, u, http.MethodGet, base+rest, ""); rec.Code != http.StatusForbidden {
				t.Errorf("a %s read %s: %d %s, want 403", name, rest, rec.Code, rec.Body)
			}
		}
	}
	// The Admin who started it still reads it.
	rec = f.credCall(t, f.adminA, http.MethodGet, base+"/jobs/"+start.JobID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the Admin reads the job: %d %s", rec.Code, rec.Body)
	}
	if job := decodeBody[jobResp](t, rec); job.State != jobs.JobDone {
		t.Errorf("job state = %q, want done", job.State)
	}
}

// TestIngressReadFollowsRoleChanges: the checker reads access on every request, so a role change takes effect on the next
// request without a restart (T-27-44).
func TestIngressReadFollowsRoleChanges(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, false)
	reader := f.asRoleOn(t, "ing-reader-a", f.envA, "viewer")
	path := "/api/env/" + f.envA + "/ingress/routes"

	if rec := f.credCall(t, reader, http.MethodGet, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("a Viewer reads the routes: %d %s, want 200", rec.Code, rec.Body)
	}
	if _, err := f.st.Write.Exec(`DELETE FROM role_assignments WHERE principal_id = ?`, "id-ing-reader-a"); err != nil {
		t.Fatalf("remove the assignment: %v", err)
	}
	if rec := f.credCall(t, reader, http.MethodGet, path, ""); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("after the assignment is removed: %d %s, want 403 or 404", rec.Code, rec.Body)
	}
	f.addEnvRole(t, "ing-reader-a", f.envA, "viewer")
	if rec := f.credCall(t, reader, http.MethodGet, path, ""); rec.Code != http.StatusOK {
		t.Errorf("after the role is given again: %d %s, want 200", rec.Code, rec.Body)
	}
}
