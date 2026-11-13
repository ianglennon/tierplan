// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// A certificate issued through the real server leaves one https.issued row in the audit log, written by the system
// principal that runs certificate work and saying that the new certificate is being served (Phase 17, SET-04). It proves
// the production wiring in wireACME: the manager's audit writer reaches the chain the audit page reads.
func TestACMERunAuditThroughTheServer(t *testing.T) {
	r := newACMERig(t, acmetest.Options{}, nil)
	r.opts = append(r.opts, WithTrustRoots(r.fake.IssuerPool()))
	r.startNew()

	client := r.client(r.mirrorPool())
	cookie := r.signIn(client)

	r.issue()

	// The certificate is served now, so the page's client trusts the fake CA.
	req, err := http.NewRequestWithContext(testsupport.Context(t), http.MethodGet, r.base+"/api/audit?action=https.issued", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := r.client(r.fake.IssuerPool()).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var list api.AuditListBody
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET /api/audit?action=https.issued = %d %s", resp.StatusCode, body)
	}
	if len(list.Items) != 1 {
		t.Fatalf("%d https.issued rows, want 1: %s", len(list.Items), body)
	}
	e := list.Items[0]
	if e.Actor != "system:acme" || e.ActorType != "system" || e.Result != "success" {
		t.Errorf("row = %+v, want actor system:acme (system), success", e)
	}
	if e.TargetType != "https" || e.Target != "https" || e.Env != nil {
		t.Errorf("target = %q/%q env %v, want https/https and no environment", e.TargetType, e.Target, e.Env)
	}
	if e.Details["served"] != "true" || e.Details["trigger"] != "admin" || e.Details["names"] != acmeHost {
		t.Errorf("details = %+v, want served true, trigger admin, names %s", e.Details, acmeHost)
	}
	if _, ok := e.Details["cancelled"]; ok {
		t.Errorf("a run nobody cancelled has cancelled = %q", e.Details["cancelled"])
	}
}

// The admin pressing Issue through the real server leaves one https.issue_requested row naming the signed-in admin and
// the job the press started, written by the handler and visible on the audit page (Phase 17, SET-04). It proves the
// production wiring of HTTPSDeps.Audit in the server.
func TestACMEIssueRequestAuditThroughTheServer(t *testing.T) {
	r := newACMERig(t, acmetest.Options{}, nil)
	r.opts = append(r.opts, WithTrustRoots(r.fake.IssuerPool()))
	r.startNew()

	selfSigned := r.client(r.mirrorPool())
	cookie := r.signIn(selfSigned)
	csrf := r.csrf(selfSigned, cookie)
	if _, err := r.srv.acme.Register(context.Background(), "", true); err != nil {
		t.Fatalf("Register: %v", err)
	}

	req, err := http.NewRequestWithContext(testsupport.Context(t), http.MethodPost, r.base+"/api/https/acme/issue", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("User-Agent", "tierplan-test-browser/1")
	req.AddCookie(cookie)
	resp, err := selfSigned.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var started api.HTTPSACMERunStartBody
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST /api/https/acme/issue = %d %s, want 202 with a job", resp.StatusCode, body)
	}
	r.waitJob(started.JobID)

	// The certificate is served now, so the page's client trusts the fake CA.
	req, err = http.NewRequestWithContext(testsupport.Context(t), http.MethodGet, r.base+"/api/audit?action=https.issue_requested", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err = r.client(r.fake.IssuerPool()).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var list api.AuditListBody
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET /api/audit?action=https.issue_requested = %d %s", resp.StatusCode, body)
	}
	if len(list.Items) != 1 {
		t.Fatalf("%d https.issue_requested rows, want 1: %s", len(list.Items), body)
	}
	e := list.Items[0]
	if e.ActorType != "user" || e.Actor != "admin" || e.Result != "success" {
		t.Errorf("row = %+v, want the signed-in admin, success", e)
	}
	if e.UserAgent != "tierplan-test-browser/1" || e.IP == "" {
		t.Errorf("user agent %q ip %q, want the request's browser and an address", e.UserAgent, e.IP)
	}
	if e.TargetType != "https" || e.Target != "https" || e.Env != nil {
		t.Errorf("target = %q/%q env %v, want https/https and no environment", e.TargetType, e.Target, e.Env)
	}
	if e.Details["job"] != started.JobID || e.Details["reused"] != "false" {
		t.Errorf("details = %+v, want job %s and reused false", e.Details, started.JobID)
	}
}
