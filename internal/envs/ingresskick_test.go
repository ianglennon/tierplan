package envs

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// recordingIngress records what Remove asked of the ingress sessions, and counts the
// environment's rows while it is asked: the kick must come after the removal committed.
type recordingIngress struct {
	r       *rig
	calls   [][2]string
	envRows int
}

func (f *recordingIngress) KickEnv(envID, code string) {
	f.calls = append(f.calls, [2]string{envID, code})
	_ = f.r.st.Read.QueryRow(`SELECT COUNT(*) FROM environments WHERE id = ?`, envID).Scan(&f.envRows)
}

func TestIngressKickOnEnvironmentRemoval(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	insertAgentEnv(t, r, "agent-env-1", "Remote")
	k := &recordingIngress{r: r, envRows: -1}
	r.svc.WithIngressSessions(k)

	if err := r.svc.Remove(context.Background(), admin, "agent-env-1", "203.0.113.5", "test", false); err != nil {
		t.Fatal(err)
	}
	if len(k.calls) != 1 || k.calls[0] != [2]string{"agent-env-1", "removed"} {
		t.Fatalf("KickEnv calls = %v, want exactly one for agent-env-1 with code removed", k.calls)
	}
	if k.envRows != 0 {
		t.Fatalf("%d environment rows when the sessions were kicked, want 0: the kick follows the commit", k.envRows)
	}
}

func TestIngressKickOnLocalEnvironmentRemoval(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	e := r.add(t)
	k := &recordingIngress{r: r, envRows: -1}
	r.svc.WithIngressSessions(k)

	if err := r.svc.Remove(context.Background(), admin, e.ID, "203.0.113.5", "test", false); err != nil {
		t.Fatal(err)
	}
	if len(k.calls) != 1 || k.calls[0] != [2]string{e.ID, "removed"} {
		t.Fatalf("KickEnv calls = %v, want exactly one for %s with code removed", k.calls, e.ID)
	}
	if k.envRows != 0 {
		t.Fatalf("%d environment rows when the sessions were kicked, want 0", k.envRows)
	}
}

func TestIngressKickWithoutASourceStillRemoves(t *testing.T) {
	r := newRig(t, dockertest.FakeConfig{})
	insertAgentEnv(t, r, "agent-env-1", "Remote")

	if err := r.svc.Remove(context.Background(), admin, "agent-env-1", "203.0.113.5", "test", false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Get(context.Background(), "agent-env-1"); err == nil {
		t.Fatal("the environment is still there")
	}
	if got := r.audits(t, audit.ActionEnvironmentRemoved); len(got) != 1 || got[0].TargetID != "agent-env-1" {
		t.Fatalf("audit = %+v, want one environment.removed row for agent-env-1", got)
	}
}
