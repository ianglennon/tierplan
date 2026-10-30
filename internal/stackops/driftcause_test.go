// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/vault"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// Drift causes (plan 08-22, STK-14, D-24, D-55): the audit log says why a service differs from the deployed revision.

var causeEpoch = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// auditRow is one audit row to write: who did what to which service of which environment, when, with what result.
type auditRow struct {
	at      time.Time
	actor   string
	action  string
	target  string
	env     string
	result  audit.Result
	details map[string]string
}

// record writes the rows into the audit log of the rig's store with the times they carry.
func (r *driftRig) record(t *testing.T, rows ...auditRow) {
	t.Helper()
	v, err := vault.New(bytes.Repeat([]byte{5}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		w := audit.NewWriter(r.swarmRig.store, v, slog.New(slog.DiscardHandler), func() time.Time { return row.at })
		result := row.result
		if result == "" {
			result = audit.ResultSuccess
		}
		reason := ""
		if result != audit.ResultSuccess {
			reason = "scale_failed"
		}
		if err := w.Record(testsupport.Context(t), audit.Entry{
			Actor:  audit.Actor{Type: audit.ActorUser, ID: "user-" + row.actor, Name: row.actor},
			Action: row.action, TargetType: "service", TargetID: row.target, EnvID: row.env,
			Result: result, Reason: reason, Details: row.details,
		}); err != nil {
			t.Fatalf("record %s: %v", row.action, err)
		}
	}
}

// serviceID is the swarm ID of a deployed service of the rig's stack.
func (r *driftRig) serviceID(t *testing.T, service string) string {
	t.Helper()
	svc, ok := r.ownedNow(t, r.st)[r.st.Name+"_"+service]
	if !ok {
		t.Fatalf("the swarm holds no service %s", service)
	}
	return svc.ID
}

// driftWithCauses is the drift check with the audit log wired in, as the route and the worker make it.
func (r *driftRig) driftWithCauses(t *testing.T) stackops.DriftResult {
	t.Helper()
	in := r.input()
	in.Audit = r.swarmRig.store
	res, err := r.deps.Drift(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	return res
}

func scaleRow(at time.Time, actor, target, env, previous, replicas string) auditRow {
	return auditRow{at: at, actor: actor, action: audit.ActionServiceScaled, target: target, env: env,
		details: map[string]string{"name": "drift_web", "replicas": replicas, "previous": previous, "code": "done"}}
}

func replicasOf(n uint64) func(*swarm.ServiceSpec) {
	return func(s *swarm.ServiceSpec) { s.Mode.Replicated.Replicas = &n }
}

func serviceNamed(t *testing.T, res stackops.DriftResult, short string) stackops.DriftService {
	t.Helper()
	for _, s := range res.Services {
		if s.Short == short {
			return s
		}
	}
	t.Fatalf("drift lists no service %s: %v", short, driftNames(res))
	return stackops.DriftService{}
}

func TestDriftCauseFromAScale(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", replicasOf(3))
	at := causeEpoch.Add(time.Hour)
	rig.record(t, scaleRow(at, "alice", rig.serviceID(t, "web"), rig.envID, "2", "3"))

	res := rig.driftWithCauses(t)

	web := serviceNamed(t, res, "web")
	want := stackops.DriftCause{Action: stackops.CauseScaled, Service: "web", By: "alice", At: at, From: "2", To: "3"}
	if web.Cause == nil || *web.Cause != want {
		t.Fatalf("web cause = %+v, want %+v", web.Cause, want)
	}
	if res.Latest == nil || *res.Latest != want || res.Causes != 1 {
		t.Errorf("latest = %+v, causes = %d, want the scale and 1", res.Latest, res.Causes)
	}

	// Without the audit log wired in nothing is known, and the drift is the same.
	plain := rig.drift(t)
	if plain.State != stackops.DriftDrifted || plain.Latest != nil || plain.Causes != 0 || serviceNamed(t, plain, "web").Cause != nil {
		t.Errorf("drift without the audit log = %+v, want drifted with no cause", plain)
	}
}

func TestDriftCauseIgnoresOldFailedAndOtherEnvRows(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", replicasOf(3))
	deployed := causeEpoch
	rig.store.rev.CreatedAt = deployed
	id := rig.serviceID(t, "web")
	unchanged := scaleRow(deployed.Add(time.Hour), "dave", id, rig.envID, "3", "3")
	unchanged.details["code"] = "unchanged"
	failed := scaleRow(deployed.Add(2*time.Hour), "carol", id, rig.envID, "2", "3")
	failed.result = audit.ResultFailure
	rig.record(t,
		scaleRow(deployed.Add(-time.Hour), "alice", id, rig.envID, "2", "3"), // before the revision was taken
		failed, // did not happen
		scaleRow(deployed.Add(3*time.Hour), "bob", id, "another-environment", "2", "3"), // somewhere else
		scaleRow(deployed.Add(4*time.Hour), "erin", "another-service", rig.envID, "2", "3"),
		unchanged, // asked for the replicas the service had
	)

	res := rig.driftWithCauses(t)

	if res.State != stackops.DriftDrifted {
		t.Fatalf("drift = %s %v, want drifted", res.State, driftNames(res))
	}
	if web := serviceNamed(t, res, "web"); web.Cause != nil || res.Latest != nil || res.Causes != 0 {
		t.Errorf("cause = %+v, latest = %+v, causes = %d, want none: not one of the rows counts", web.Cause, res.Latest, res.Causes)
	}
}

func TestDriftCauseIgnoresForceUpdate(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", replicasOf(3))
	id := rig.serviceID(t, "web")
	rig.record(t, auditRow{
		at: causeEpoch.Add(time.Hour), actor: "alice", action: audit.ActionServiceForceUpdated, target: id, env: rig.envID,
		details: map[string]string{"name": "drift_web"},
	})

	res := rig.driftWithCauses(t)

	if web := serviceNamed(t, res, "web"); web.Cause != nil || res.Latest != nil {
		t.Errorf("cause = %+v, latest = %+v, want none: a force update is never a cause", web.Cause, res.Latest)
	}
}

func TestDriftCauseNewestWins(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", replicasOf(4))
	rig.editLive(t, "api", func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Image = "nginx:1.30-alpine" })
	web, api := rig.serviceID(t, "web"), rig.serviceID(t, "api")
	first, second, third := causeEpoch.Add(time.Hour), causeEpoch.Add(2*time.Hour), causeEpoch.Add(3*time.Hour)
	rig.record(t,
		scaleRow(first, "alice", web, rig.envID, "2", "3"),
		scaleRow(second, "bob", web, rig.envID, "3", "4"),
		auditRow{at: third, actor: "carol", action: audit.ActionServiceUpdated, target: api, env: rig.envID, details: map[string]string{"name": "drift_api", "code": "done"}},
	)

	res := rig.driftWithCauses(t)

	w := serviceNamed(t, res, "web")
	if w.Cause == nil || w.Cause.By != "bob" || !w.Cause.At.Equal(second) || w.Cause.From != "3" || w.Cause.To != "4" {
		t.Errorf("web cause = %+v, want bob's scale from 3 to 4: the newest of two", w.Cause)
	}
	a := serviceNamed(t, res, "api")
	if a.Cause == nil || a.Cause.Action != stackops.CauseEdited || a.Cause.By != "carol" || a.Cause.From != "" || a.Cause.To != "" {
		t.Errorf("api cause = %+v, want carol's edit, with no replica counts", a.Cause)
	}
	if res.Causes != 2 || res.Latest == nil || res.Latest.By != "carol" || res.Latest.Service != "api" {
		t.Errorf("latest = %+v, causes = %d, want 2 changes, the latest carol's edit of api", res.Latest, res.Causes)
	}
}

func TestDriftCauseAdoptionGap(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", replicasOf(3))
	rig.editLive(t, "api", func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Image = "nginx:1.30-alpine" })
	rig.st.Source = stacks.Source{Kind: stacks.SourceAdopted, Ref: "drift"}
	rig.st.Adoption = stacks.Adoption{Pending: true, Gaps: map[string][]string{
		"drift_web": {"Mode.Replicated.Replicas"},
		"drift_api": {"TaskTemplate.ContainerSpec.Env[OTHER]"}, // recorded, but not where api differs now
	}}

	res := rig.driftWithCauses(t)

	web := serviceNamed(t, res, "web")
	if web.Cause == nil || web.Cause.Action != stackops.CauseAdopted || web.Cause.Service != "web" || web.Cause.By != "" || !web.Cause.At.IsZero() {
		t.Errorf("web cause = %+v, want the adoption's, with no actor or time", web.Cause)
	}
	if api := serviceNamed(t, res, "api"); api.Cause != nil {
		t.Errorf("api cause = %+v, want none: its image was not a recorded gap", api.Cause)
	}
	if res.Latest == nil || res.Latest.Action != stackops.CauseAdopted || res.Causes != 0 {
		t.Errorf("latest = %+v, causes = %d, want the adoption's and no change of Coaming's own", res.Latest, res.Causes)
	}

	// Once the first deploy has applied the rebuilt definition the adoption is no longer pending and explains nothing.
	rig.st.Adoption.Pending = false
	if done := rig.driftWithCauses(t); serviceNamed(t, done, "web").Cause != nil || done.Latest != nil {
		t.Errorf("after the first deploy: latest = %+v, want none", done.Latest)
	}
}

func TestDriftScaleIsNotTheCauseOfAnImageChange(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "api", func(s *swarm.ServiceSpec) { s.TaskTemplate.ContainerSpec.Image = "nginx:1.30-alpine" })
	rig.record(t, scaleRow(causeEpoch.Add(time.Hour), "alice", rig.serviceID(t, "api"), rig.envID, "1", "1"))

	res := rig.driftWithCauses(t)

	if api := serviceNamed(t, res, "api"); api.Cause != nil || res.Latest != nil {
		t.Errorf("cause = %+v, want none: the replicas are not what differs", api.Cause)
	}
}
