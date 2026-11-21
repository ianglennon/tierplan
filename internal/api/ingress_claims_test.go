// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// Every path that deploys a revision claims its hostnames as a manual deploy does, and nothing else touches the claims
// (plan 24-18, D-17, DEP-05, DEP-06). The seeding is that of plan 18-11 (seedIngressWorld); a stack that raced another
// for a name is staged through the BeforeJob seam, which runs after the gate and the lock and before the job.

// claimsOf are the hostnames a stack holds, sorted.
func claimsOf(t *testing.T, f *envFix, envID, stackID string) []string {
	t.Helper()
	world, err := f.stackDeps.Ingress.World(context.Background(), envID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var held []string
	for host, c := range world.Claims {
		if c.StackID == stackID {
			held = append(held, host)
		}
	}
	slices.Sort(held)
	return held
}

// configVersion is the config_version of the environment's ingress row.
func configVersion(t *testing.T, f *envFix, envID string) int64 {
	t.Helper()
	set, err := f.stackDeps.Ingress.Settings(context.Background(), envID)
	if err != nil {
		t.Fatal(err)
	}
	return set.ConfigVersion
}

// claimAudits counts the audit rows of claim writes.
func claimAudits(t *testing.T, f *envFix, action string) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// jobOf waits for the job and returns it whole, with the messages of its items.
func jobOf(t *testing.T, f *envFix, jobID string) jobs.Job {
	t.Helper()
	job, err := f.jobHub.Wait(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// jobItemOf is the item with the id, failing the test when the job has none.
func jobItemOf(t *testing.T, job jobs.Job, id string) jobs.Item {
	t.Helper()
	for _, it := range job.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("job has no item %q: %+v", id, job.Items)
	return jobs.Item{}
}

// deployRow is the state and code of the newest deploy row of the stack.
func deployRow(t *testing.T, f *envFix, stackID string) (state, code string) {
	t.Helper()
	if err := f.st.Read.QueryRow(`SELECT state, code FROM stack_deploys WHERE stack_id = ? ORDER BY started_at DESC, rowid DESC LIMIT 1`, stackID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	return state, code
}

// claimBeforeJob, set on the fixture before the server is built, makes the next deploy lose a race: just before its job
// starts, the stack in the second environment takes host, which the deploy's gate found free.
func claimBeforeJob(f *envFix, host string) {
	var once atomic.Bool
	f.stackBeforeJob = func(ctx context.Context) {
		if !once.CompareAndSwap(false, true) {
			return
		}
		decl := ingress.Declaration{Service: "web", Host: host, Label: "tierplan.ingress.host", Port: 80}
		if _, err := f.stackDeps.Ingress.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{EnvID: ingressOtherEnvID, StackID: "stack-in-b", Actor: "bob", Decls: []ingress.Declaration{decl}}); err != nil {
			panic("the competing claim was refused: " + err.Error())
		}
	}
}

func TestIngressClaimsOnManualDeploy(t *testing.T) {
	t.Parallel()

	t.Run("a deploy stores its claim and attaches nothing while ingress is off", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnv(t, nil)
		seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := newStack(t, admin, "claims-manual")

		f.deployGood(t, admin, st.ID, ingressCompose("manual.apps.example.com"))

		if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"manual.apps.example.com"}) {
			t.Errorf("claims = %v, want the host of the deployed revision", got)
		}
		if n := claimAudits(t, f, "ingress.claim_replaced"); n != 1 {
			t.Errorf("ingress.claim_replaced rows = %d, want 1", n)
		}
		creates := f.fake.ServiceCreates()
		if len(creates) == 0 {
			t.Fatal("no service was created")
		}
		for _, c := range creates {
			for _, n := range c.Spec.TaskTemplate.Networks {
				if n.Target == ingress.NetworkName {
					t.Errorf("service %s joined %s with ingress off, want nothing attached", c.Spec.Name, ingress.NetworkName)
				}
			}
		}
	})

	t.Run("a name another stack took since the gate refuses the job", func(t *testing.T) {
		t.Parallel()
		const host = "race.apps.example.com"
		f, e := stackEnvTuned(t, func(f *envFix) { claimBeforeJob(f, host) })
		seed := seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := newStack(t, admin, "claims-race")
		writes := len(f.fake.Writes())

		jobID := f.deployDraft(t, admin, st.ID, ingressCompose(host))
		job := jobOf(t, f, jobID)

		if job.State == "done" {
			t.Fatalf("job = %+v, want it failed", job)
		}
		check := jobItemOf(t, job, stackops.ItemCheck)
		if check.Code != stackops.CodeIngressRefused || !strings.HasPrefix(check.Message, stackops.CodeMessage(stackops.CodeIngressRefused)) {
			t.Errorf("check item = %+v, want ingress_refused with its sentence", check)
		}
		if strings.Contains(check.Message, seed.otherEnvID) {
			t.Errorf("message = %q, must not name the other environment's id", check.Message)
		}
		if state, code := deployRow(t, f, st.ID); state != "failed" || code != stackops.CodeIngressRefused {
			t.Errorf("deploy row = %s/%s, want failed/ingress_refused", state, code)
		}
		if n := len(f.fake.Writes()); n != writes {
			t.Errorf("writes went from %d to %d, a refused claim leaves the engine alone", writes, n)
		}
		if got := claimsOf(t, f, e.ID, st.ID); len(got) != 0 {
			t.Errorf("claims = %v, want none", got)
		}
		if got := claimsOf(t, f, seed.otherEnvID, seed.otherStackID); !slices.Equal(got, []string{host}) {
			t.Errorf("the other stack holds %v, want it left holding %s", got, host)
		}
	})

	t.Run("a standalone deploy claims the same way", func(t *testing.T) {
		t.Parallel()
		f, e, _ := standaloneEnv(t, nil)
		seedIngressWorld(t, f, e)
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := newStack(t, admin, "claims-standalone")
		v := saveCompose(t, admin, st.ID, ingressStandaloneCompose("stand.apps.example.com"), "[]")

		rec := admin.deploy(st.ID, `{"draftVersion":`+itoaT(v)+`}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("deploy: %d %.300s, want 202", rec.Code, rec.Body)
		}
		if job := f.awaitDeploy(t, answerOf[struct {
			JobID string `json:"jobId"`
		}](t, rec).JobID); job.State != "done" {
			t.Fatalf("job = %+v, want it done", job)
		}
		if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"stand.apps.example.com"}) {
			t.Errorf("claims = %v, want the host of the deployed revision", got)
		}
	})
}
