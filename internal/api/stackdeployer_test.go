// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"testing"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The StackDeployer starts a deploy for a principal that is not on a request (plan 09-03, D-20, RESEARCH
// Pitfall 9). It goes through the very code the deploy route does, so these tests check what is different:
// who the job and the audit row belong to.

// deployerOf is the deployer of the dependencies the fixture registered the stack routes with.
func deployerOf(f *envFix) *api.StackDeployer { return api.NewStackDeployer(f.stackDeps) }

// auditedDeploy is the stack.deployed audit rows of a stack, with the actor.
type auditedDeploy struct {
	ActorType, ActorID, ActorName string
	Result                        string
	Details                       map[string]string
}

func (f *envFix) deployAudits(t *testing.T, stackID string) []auditedDeploy {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT actor_type, actor_id, actor_name, result, details FROM audit_log WHERE action = 'stack.deployed' AND target_id = ? ORDER BY seq`, stackID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditedDeploy
	for rows.Next() {
		var a auditedDeploy
		var details string
		if err := rows.Scan(&a.ActorType, &a.ActorID, &a.ActorName, &a.Result, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &a.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, a)
	}
	return out
}

func TestStackDeployerDeploysOnBehalf(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	f.caller(t, e, "operator-1", "operator")
	const ownerID = "id-operator-1"
	st := newStack(t, admin, "auto")
	version := saveCompose(t, admin, st.ID, composeWeb127, `[]`)

	p := rbac.OnBehalf("system:gitsync", ownerID)
	started, err := deployerOf(f).DeployDraft(testsupport.Context(t), p, e.ID, st.ID, version, "poll")
	if err != nil {
		t.Fatalf("DeployDraft: %v", err)
	}
	if started.JobID == "" || started.Seq != 1 {
		t.Fatalf("started = %+v, want a job and revision 1", started)
	}
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("job = %+v, want it done", job)
	}

	// The job is the system's share of the owner's slots, not the owner's own and not another owner's.
	job, ok := f.jobHub.Get(started.JobID)
	if !ok || job.Owner != "system:gitsync:"+ownerID || job.Kind != "stack_deploy" || job.EnvID != e.ID {
		t.Errorf("job = %+v (found %v), want a stack_deploy of %s owned by system:gitsync:%s", job, ok, e.ID, ownerID)
	}

	// The deploy took a revision from the draft, as a manual one does.
	revs := f.revisions(t, st.ID)
	if len(revs) != 1 || revs[0].Seq != started.Seq || revs[0].Outcome != "good" {
		t.Errorf("revisions = %+v, want revision %d good", revs, started.Seq)
	}

	// The audit row names the system actor, the user it acted for and what started it.
	audits := f.deployAudits(t, st.ID)
	if len(audits) != 1 {
		t.Fatalf("stack.deployed rows = %+v, want one", audits)
	}
	a := audits[0]
	if a.ActorType != "system" || a.ActorID != "system:gitsync" || a.Result != "success" {
		t.Errorf("audit actor = %s %s result %s, want the system actor system:gitsync", a.ActorType, a.ActorID, a.Result)
	}
	if a.Details["trigger"] != "poll" || a.Details["onBehalfOf"] != ownerID || a.Details["mode"] != "deploy" || a.Details["revision"] != "1" {
		t.Errorf("audit details = %v, want trigger poll, onBehalfOf %s, mode deploy, revision 1", a.Details, ownerID)
	}
}
