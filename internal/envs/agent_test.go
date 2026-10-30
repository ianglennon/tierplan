// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/store/sqlc"
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
	if err := r.svc.Remove(context.Background(), admin, "agent-env-1", "203.0.113.5", "test", false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Get(context.Background(), "agent-env-1"); err == nil {
		t.Fatal("the environment is still there")
	}
	if got := r.audits(t, audit.ActionEnvironmentRemoved); len(got) != 1 || got[0].TargetID != "agent-env-1" {
		t.Fatalf("audit = %+v", got)
	}
}

func TestRenamingAnEnvironmentThatWaitsForAnAutomaticNameStopsTheAutomaticNaming(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	insertAgentEnv(t, r, "agent-env-1", pendingName)
	if _, err := r.st.Write.Exec(`UPDATE environments SET name_auto = 1 WHERE id = 'agent-env-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Rename(context.Background(), admin, "agent-env-1", "Remote", "203.0.113.5", "test"); err != nil {
		t.Fatal(err)
	}
	var auto int
	if err := r.st.Read.QueryRow(`SELECT name_auto FROM environments WHERE id = 'agent-env-1'`).Scan(&auto); err != nil || auto != 0 {
		t.Fatalf("name_auto = %d, %v; want 0 so no host renames it again", auto, err)
	}
}

// fakeUninstaller records what Remove asked of it and looks at the database while it
// is asked, to prove that the agents are told before anything is revoked.
type fakeUninstaller struct {
	r         *rig
	calls     []uninstallCall
	confirmed []string
	envRows   int
	audited   int
}

type uninstallCall struct {
	actor        audit.Actor
	envID, ip, u string
	timeout      time.Duration
}

func (f *fakeUninstaller) UninstallAll(_ context.Context, actor audit.Actor, envID, ip, ua string, timeout time.Duration) []string {
	f.calls = append(f.calls, uninstallCall{actor, envID, ip, ua, timeout})
	_ = f.r.st.Read.QueryRow(`SELECT COUNT(*) FROM environments WHERE id = ?`, envID).Scan(&f.envRows)
	_ = f.r.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE env_id = ? AND action IN ('environment.removed', 'agent.revoked')`, envID).Scan(&f.audited)
	return f.confirmed
}

func TestRemovingAnAgentEnvironmentWithUninstallTellsTheAgentsFirstAndThenRevokesAndDeletesAsAlways(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	insertAgentEnv(t, r, "agent-env-1", "Remote")
	un := &fakeUninstaller{r: r}
	r.svc.WithUninstaller(un)

	if err := r.svc.Remove(context.Background(), admin, "agent-env-1", "203.0.113.5", "test", true); err != nil {
		t.Fatal(err)
	}
	if len(un.calls) != 1 {
		t.Fatalf("the uninstaller was asked %d times, want once", len(un.calls))
	}
	c := un.calls[0]
	if c.actor != admin || c.envID != "agent-env-1" || c.ip != "203.0.113.5" || c.u != "test" || c.timeout != UninstallWait {
		t.Fatalf("call = %+v, want the admin, the environment, the caller's address and agent, and the %v wait", c, UninstallWait)
	}
	if un.envRows != 1 || un.audited != 0 {
		t.Fatalf("while the agents were being told: %d environment rows and %d removal or revocation audit rows, want 1 and 0: nothing may be revoked or deleted first", un.envRows, un.audited)
	}
	if _, err := r.svc.Get(context.Background(), "agent-env-1"); err == nil {
		t.Fatal("the environment is still there")
	}
}

func TestRemovingAnEnvironmentWithoutUninstallNeverAsksTheUninstaller(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	insertAgentEnv(t, r, "agent-env-1", "Remote")
	un := &fakeUninstaller{r: r}
	r.svc.WithUninstaller(un)
	if err := r.svc.Remove(context.Background(), admin, "agent-env-1", "", "", false); err != nil {
		t.Fatal(err)
	}
	if len(un.calls) != 0 {
		t.Fatalf("the uninstaller was asked: %+v", un.calls)
	}
}

func TestRemovingTheLocalHostWithUninstallAsksNoOneAndAnUnknownEnvironmentIsNotFound(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	e := r.add(t)
	un := &fakeUninstaller{r: r}
	r.svc.WithUninstaller(un)
	if err := r.svc.Remove(context.Background(), admin, "nope", "", "", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown environment: %v, want ErrNotFound", err)
	}
	if err := r.svc.Remove(context.Background(), admin, e.ID, "", "", true); err != nil {
		t.Fatal(err)
	}
	if len(un.calls) != 0 {
		t.Fatalf("the uninstaller was asked about the local host or an unknown environment: %+v", un.calls)
	}
}

func TestRemovingWithUninstallStillRemovesWhenNothingConfirms(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	insertAgentEnv(t, r, "agent-env-1", "Remote")
	r.svc.WithUninstaller(&fakeUninstaller{r: r})
	if err := r.svc.Remove(context.Background(), admin, "agent-env-1", "", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Get(context.Background(), "agent-env-1"); err == nil {
		t.Fatal("an environment whose agents did not answer was not removed")
	}
}

func TestRemovingWithUninstallAndNoUninstallerConfiguredJustRemoves(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	insertAgentEnv(t, r, "agent-env-1", "Remote")
	if err := r.svc.Remove(context.Background(), admin, "agent-env-1", "", "", true); err != nil {
		t.Fatal(err)
	}
}
