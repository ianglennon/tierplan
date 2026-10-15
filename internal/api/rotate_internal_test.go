// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

// A rotate updates services, so the secrets.manage or configs.manage that reaches the route is not
// enough: the handler checks services.control too, and a refusal is audited as permission.denied
// (plan 06-28, T-06-81). An Admin always holds both, so the second check is a guard against a role
// matrix that one day lets them part.
func TestRotateNeedsServicesControlBesideTheObjectsPermissionAndAuditsARefusal(t *testing.T) {
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	d := ServicesDeps{Audit: audit.NewWriter(st, v, log, nil), Checker: rbac.NewChecker(st), Log: log}
	for i, ra := range [][2]string{{"viewer-1", "viewer"}, {"operator-1", "operator"}} {
		if _, err := st.Write.ExecContext(context.Background(),
			`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES (?, ?, 'x', 1, 1, 1)`, ra[0], ra[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Write.ExecContext(context.Background(),
			`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES (?, 'user', ?, ?, 'env-1', ?)`,
			"ra-"+ra[0], ra[0], ra[1], i+1); err != nil {
			t.Fatal(err)
		}
	}
	who := func(id string) *rbac.Principal { return &rbac.Principal{Type: rbac.PrincipalUser, ID: id, Name: id} }
	route := "POST /api/env/{envId}/secrets/{secretId}/rotate"

	if err := requireServicesControl(context.Background(), d, who("operator-1"), "env-1", "agent", route); err != nil {
		t.Errorf("an Operator holds services.control: %v", err)
	}
	status, ptype := problemOf(t, requireServicesControl(context.Background(), d, who("viewer-1"), "env-1", "agent", route))
	if status != http.StatusForbidden || ptype != "forbidden" {
		t.Errorf("a Viewer = %d %s, want 403 forbidden", status, ptype)
	}
	var target, perm string
	if err := st.Read.QueryRow(`SELECT target_id, COALESCE(json_extract(details, '$.permission'), '') FROM audit_log WHERE action = ?`,
		audit.ActionPermissionDenied).Scan(&target, &perm); err != nil {
		t.Fatalf("the refusal was not audited: %v", err)
	}
	if target != route || perm != string(rbac.PermServicesControl) {
		t.Errorf("audit row = %q %q, want the route and services.control", target, perm)
	}
	d.Checker = nil
	if status, _ := problemOf(t, requireServicesControl(context.Background(), d, who("operator-1"), "env-1", "agent", route)); status != http.StatusInternalServerError {
		t.Errorf("no checker = %d, want 500", status)
	}
}
