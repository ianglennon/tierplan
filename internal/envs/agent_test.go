// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// Agent environments (plan 02-01): the registry holds them next to the local
// host, but a monitor exists only for each connected agent's tunnel, never for
// the environment row itself.

func insertAgentEnv(t *testing.T, r *rig, id, name string) {
	t.Helper()
	now := r.clk.now().UnixNano()
	if err := r.st.Q.InsertAgentEnvironment(context.Background(), sqlc.InsertAgentEnvironmentParams{
		ID: id, Name: name, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStartAllMonitorsTheLocalHostAndNeverAnAgentEnvironmentRow(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	if _, err := r.svc.AddLocal(context.Background(), admin, "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	insertAgentEnv(t, r, "agent-env-1", "Remote")
	r.mon.StopAll()

	if err := r.svc.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if got := r.mon.loopCount(); got != 1 {
		t.Fatalf("%d monitors after StartAll, want 1 (the local host only)", got)
	}
	e, err := r.svc.Get(context.Background(), "agent-env-1")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != KindAgent || e.Status.State != StateUnknown || e.SocketPath != "" {
		t.Fatalf("agent environment = %+v, want kind agent, state unknown, no socket", e)
	}
}

func TestAnUnnamedAgentEnvironmentIsListedUnderAPlaceholderNameAndStaysBlankInTheDatabase(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	insertAgentEnv(t, r, "agent-env-1", "")
	e, err := r.svc.Get(context.Background(), "agent-env-1")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != pendingName {
		t.Fatalf("name = %q, want %q", e.Name, pendingName)
	}
	var stored string
	if err := r.st.Read.QueryRow(`SELECT name FROM environments WHERE id = 'agent-env-1'`).Scan(&stored); err != nil || stored != "" {
		t.Fatalf("stored name = %q, %v; want blank so the first agent can name it", stored, err)
	}
}

func TestRemovingAnAgentEnvironmentDeletesItsRowsAndAudits(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	insertAgentEnv(t, r, "agent-env-1", "Remote")
	if err := r.svc.Remove(context.Background(), admin, "agent-env-1", "203.0.113.5", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Get(context.Background(), "agent-env-1"); err == nil {
		t.Fatal("the environment is still there")
	}
	if got := r.audits(t, audit.ActionEnvironmentRemoved); len(got) != 1 || got[0].TargetID != "agent-env-1" {
		t.Fatalf("audit = %+v", got)
	}
}
