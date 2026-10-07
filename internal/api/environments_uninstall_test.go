// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// uninstallLine reads the next line the server wrote to an agent's control stream and
// decodes it as an uninstall.
func uninstallLine(t *testing.T, control io.Reader) wire.Uninstall {
	t.Helper()
	got := make(chan wire.Uninstall, 1)
	go func() {
		sc := bufio.NewScanner(control)
		if sc.Scan() {
			var u wire.Uninstall
			_ = json.Unmarshal(sc.Bytes(), &u)
			got <- u
		}
	}()
	select {
	case u := <-got:
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("the server wrote nothing to the agent's control stream")
		return wire.Uninstall{}
	}
}

// removeAsync sends DELETE and returns where the answer will arrive, so the test can
// play the agents while the server waits for them.
func (f *agentFix) removeAsync(t *testing.T, path string) <-chan *envRec {
	t.Helper()
	done := make(chan *envRec, 1)
	go func() {
		rec := f.call(t, authReq{method: http.MethodDelete, path: path, cookie: f.cookie, csrf: f.csrf})
		done <- &envRec{code: rec.Code, body: rec.Body.Bytes()}
	}()
	return done
}

func waitRemoval(t *testing.T, done <-chan *envRec) *envRec {
	t.Helper()
	select {
	case rec := <-done:
		return rec
	case <-time.After(10 * time.Second):
		t.Fatal("the removal did not finish")
		return nil
	}
}

// assertEnvironmentGoneAndAgentRefused checks that the environment cannot be read and
// that neither the agent's identity nor the old join token gets in any more.
func assertEnvironmentGoneAndAgentRefused(t *testing.T, f *agentFix, env tunneltest.Environment, a *agentConn) {
	t.Helper()
	if rec := f.req(t, http.MethodGet, "/api/env/"+env.ID+"/agents", ""); rec.code != http.StatusNotFound {
		t.Fatalf("the removed environment still answers: %d %s", rec.code, rec.body)
	}
	if c := f.srv.Hello(t, a.conn.Identity); c.Refuse == nil || c.Refuse.Code != wire.ReasonAuthRejected {
		t.Fatalf("a later hello from the agent: refuse = %+v, want auth_rejected", c.Refuse)
	}
	if c := f.srv.Dial(t, tunneltest.Agent{Token: env.Token}); c.Refuse == nil {
		t.Fatal("the old join token still enrols an agent")
	}
}

func TestRemoveEnvironmentWithUninstallOnASwarmTellsTheWorkerFirstThenTheManagerToRemoveTheServiceAndAuditsBothBeforeRevokingAndDeleting(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	mgr := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, swarmFacts("alpha", true, true))
	wrk := f.connect(t, env, dockertest.FakeConfig{Hostname: "bravo"}, swarmFacts("bravo", false, true))
	f.srv.Events.WaitLen(t, 2)

	done := f.removeAsync(t, "/api/environments/"+env.ID+"?uninstallAgents=true")
	uw := uninstallLine(t, wrk.control)
	if uw.T != wire.TypeUninstall || !uw.RemoveData || uw.Service || uw.ID == "" {
		t.Fatalf("the worker was sent %+v, want an uninstall that removes its data and not the service", uw)
	}
	// The manager is told last: removing the service ends every agent, so the others
	// must have started their helpers first. It is waiting for the worker's answer.
	select {
	case rec := <-done:
		t.Fatalf("the removal answered %d before the worker had", rec.code)
	case <-time.After(150 * time.Millisecond):
	}
	f.updater.HandleResult(wrk.id, wire.UpdateResult{T: wire.TypeUpdateResult, ID: uw.ID, OK: true})

	um := uninstallLine(t, mgr.control)
	if um.T != wire.TypeUninstall || !um.RemoveData || !um.Service || um.ID == "" {
		t.Fatalf("the manager was sent %+v, want an uninstall that removes its data and the service", um)
	}
	select {
	case rec := <-done:
		t.Fatalf("the removal answered %d before the manager had", rec.code)
	case <-time.After(150 * time.Millisecond):
	}
	if got := f.audits(t, audit.ActionEnvironmentRemoved); len(got) != 0 {
		t.Fatalf("the environment was removed before the agents answered: %+v", got)
	}
	f.updater.HandleResult(mgr.id, wire.UpdateResult{T: wire.TypeUpdateResult, ID: um.ID, OK: true})
	if rec := waitRemoval(t, done); rec.code != http.StatusNoContent {
		t.Fatalf("remove: %d %s, want 204", rec.code, rec.body)
	}

	if got := f.audits(t, audit.ActionAgentUninstalled); len(got) != 2 || got[0].ActorID != "id-admin" || got[1].ActorID != "id-admin" {
		t.Fatalf("agent.uninstalled = %+v, want one per confirming agent under the admin", got)
	}
	if got := f.audits(t, audit.ActionAgentRevoked); len(got) != 2 {
		t.Fatalf("agent.revoked = %+v, want both agents revoked with the environment", got)
	}
	if got := f.audits(t, audit.ActionEnvironmentRemoved); len(got) != 1 || got[0].TargetID != env.ID {
		t.Fatalf("environment.removed = %+v", got)
	}
	assertEnvironmentGoneAndAgentRefused(t, f, env, mgr)
}

func TestRemoveEnvironmentWithUninstallOnAStandaloneHostTellsItToRemoveItsOwnContainerAndVolume(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, selfUpdating("alpha"))
	f.srv.Events.WaitLen(t, 1)

	done := f.removeAsync(t, "/api/environments/"+env.ID+"?uninstallAgents=true")
	u := uninstallLine(t, a.control)
	if u.T != wire.TypeUninstall || !u.RemoveData || u.Service || u.ID == "" {
		t.Fatalf("the agent was sent %+v", u)
	}
	f.updater.HandleResult(a.id, wire.UpdateResult{T: wire.TypeUpdateResult, ID: u.ID, OK: true})
	if rec := waitRemoval(t, done); rec.code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.code, rec.body)
	}
	if got := f.audits(t, audit.ActionAgentUninstalled); len(got) != 1 || got[0].TargetID != a.id || got[0].ActorID != "id-admin" {
		t.Fatalf("agent.uninstalled = %+v", got)
	}
	assertEnvironmentGoneAndAgentRefused(t, f, env, a)
}

func TestRemoveEnvironmentWithoutUninstallOnlyRevokesAndDeletesAndSendsNoUninstall(t *testing.T) {
	for name, q := range map[string]string{"false": "?uninstallAgents=false", "absent": ""} {
		t.Run(name, func(t *testing.T) {
			f := newAgentFix(t)
			env := f.srv.NewEnvironment(t, "Production")
			a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, selfUpdating("alpha"))
			f.srv.Events.WaitLen(t, 1)

			if rec := f.req(t, http.MethodDelete, "/api/environments/"+env.ID+q, ""); rec.code != http.StatusNoContent {
				t.Fatalf("remove: %d %s, want 204", rec.code, rec.body)
			}
			if got := f.audits(t, audit.ActionAgentUninstalled); len(got) != 0 {
				t.Fatalf("agent.uninstalled = %+v, want none", got)
			}
			if got := f.audits(t, audit.ActionAgentRevoked); len(got) != 1 {
				t.Fatalf("agent.revoked = %+v, want the agent revoked", got)
			}
			assertEnvironmentGoneAndAgentRefused(t, f, env, a)
		})
	}
}

func TestRemoveEnvironmentWithUninstallAnAgentThatNeverAnswersDoesNotStopTheRemovalAndIsNotRecorded(t *testing.T) {
	f := newAgentFix(t)
	f.envSvc.WithUninstallWait(900 * time.Millisecond)
	env := f.srv.NewEnvironment(t, "Production")
	mgr := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, swarmFacts("alpha", true, true))
	wrk := f.connect(t, env, dockertest.FakeConfig{Hostname: "bravo"}, swarmFacts("bravo", false, true))
	f.srv.Events.WaitLen(t, 2)

	done := f.removeAsync(t, "/api/environments/"+env.ID+"?uninstallAgents=true")
	_ = uninstallLine(t, wrk.control) // the worker reads it and never answers
	um := uninstallLine(t, mgr.control)
	f.updater.HandleResult(mgr.id, wire.UpdateResult{T: wire.TypeUpdateResult, ID: um.ID, OK: true})

	if rec := waitRemoval(t, done); rec.code != http.StatusNoContent {
		t.Fatalf("remove: %d %s, want 204 although the worker never answered", rec.code, rec.body)
	}
	got := f.audits(t, audit.ActionAgentUninstalled)
	if len(got) != 1 || got[0].TargetID != mgr.id {
		t.Fatalf("agent.uninstalled = %+v, want only the manager, who confirmed", got)
	}
	assertEnvironmentGoneAndAgentRefused(t, f, env, wrk)
}

func TestRemoveEnvironmentWithUninstallAnAgentThatRefusesIsNotRecordedAsUninstalled(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, selfUpdating("alpha"))
	f.srv.Events.WaitLen(t, 1)

	done := f.removeAsync(t, "/api/environments/"+env.ID+"?uninstallAgents=true")
	ua := uninstallLine(t, a.control)
	f.updater.HandleResult(a.id, wire.UpdateResult{T: wire.TypeUpdateResult, ID: ua.ID, OK: false, Error: wire.UpdateErrNotImplemented})
	if rec := waitRemoval(t, done); rec.code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.code, rec.body)
	}
	if got := f.audits(t, audit.ActionAgentUninstalled); len(got) != 0 {
		t.Fatalf("agent.uninstalled = %+v, want none", got)
	}
	assertEnvironmentGoneAndAgentRefused(t, f, env, a)
}

func TestRemoveEnvironmentWithUninstallIsForInstanceAdminsAndTellsNoAgentWithout(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "alpha"}, selfUpdating("alpha"))
	f.srv.Events.WaitLen(t, 1)

	rec := f.asAllEnvAdmin(t, "manager", http.MethodDelete, "/api/environments/"+env.ID+"?uninstallAgents=true", ``)
	if rec.code != http.StatusForbidden {
		t.Fatalf("remove as an admin of every environment: %d %s, want 403", rec.code, rec.body)
	}
	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(a.control)
		if sc.Scan() {
			got <- sc.Text()
		}
	}()
	select {
	case line := <-got:
		t.Fatalf("a refused request still told the agent something: %s", line)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := f.envSvc.Get(t.Context(), env.ID); err != nil {
		t.Fatalf("the environment was removed: %v", err)
	}
}

func TestRemoveEnvironmentRejectsAQueryValueThatIsNotABoolean(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	rec := f.req(t, http.MethodDelete, "/api/environments/"+env.ID+"?uninstallAgents=maybe", "")
	if rec.code != http.StatusUnprocessableEntity && rec.code != http.StatusBadRequest {
		t.Fatalf("a bad uninstallAgents: %d %s, want 422", rec.code, rec.body)
	}
	if !strings.Contains(string(rec.body), "uninstallAgents") {
		t.Fatalf("the problem does not name the parameter: %s", rec.body)
	}
}

func TestTheAgentListGivesTheManualCleanupCommandsWithTheCurrentSecretName(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	read := func() (swarm, standalone string) {
		rec := f.req(t, http.MethodGet, "/api/env/"+env.ID+"/agents", "")
		var body struct {
			ManualCleanup struct {
				Swarm      string `json:"swarm"`
				Standalone string `json:"standalone"`
			} `json:"manualCleanup"`
		}
		if rec.code != http.StatusOK || json.Unmarshal(rec.body, &body) != nil {
			t.Fatalf("list agents: %d %s", rec.code, rec.body)
		}
		return body.ManualCleanup.Swarm, body.ManualCleanup.Standalone
	}
	swarm, standalone := read()
	for _, want := range []string{"docker service rm bayplan-agent", "docker secret rm bayplan-agent-token-g1", "docker volume rm bayplan-agent"} {
		if !strings.Contains(swarm, want) {
			t.Errorf("the swarm cleanup lacks %q:\n%s", want, swarm)
		}
	}
	if !strings.Contains(standalone, "docker rm -f bayplan-agent") || !strings.Contains(standalone, "docker volume rm bayplan-agent") {
		t.Errorf("the standalone cleanup = %q", standalone)
	}
	if strings.Contains(swarm+standalone, "bpjt_") {
		t.Fatal("a cleanup command carries a join token")
	}

	// A rotation moves the secret name on.
	f.inst.Apply("Bayplan", "https://bayplan.example")
	if rec := f.req(t, http.MethodPost, "/api/environments/"+env.ID+"/token", ""); rec.code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.code, rec.body)
	}
	if swarm, _ := read(); !strings.Contains(swarm, "docker secret rm bayplan-agent-token-g2") {
		t.Fatalf("after a rotation the swarm cleanup still names the old secret:\n%s", swarm)
	}
}

// Removing an environment frees what it held: the host that was enrolled in it can be
// enrolled in another environment at once (plan 02-19's engine, cluster and host bindings).
func TestRemoveEnvironmentFreesTheHostToEnrolInAnotherEnvironmentAtOnce(t *testing.T) {
	f := newAgentFix(t)
	first := f.srv.NewEnvironment(t, "First")
	facts := selfUpdating("alpha")
	a := f.connect(t, first, dockertest.FakeConfig{Hostname: "alpha"}, facts)
	f.srv.Events.WaitLen(t, 1)

	// While the first environment holds the host, a second one cannot enrol it.
	second := f.srv.NewEnvironment(t, "Second")
	if c := f.srv.Dial(t, tunneltest.Agent{Token: second.Token, Facts: facts}); c.Refuse == nil {
		t.Fatal("a host enrolled in one environment was enrolled in another as well")
	}

	done := f.removeAsync(t, "/api/environments/"+first.ID+"?uninstallAgents=true")
	u := uninstallLine(t, a.control)
	f.updater.HandleResult(a.id, wire.UpdateResult{T: wire.TypeUpdateResult, ID: u.ID, OK: true})
	if rec := waitRemoval(t, done); rec.code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.code, rec.body)
	}

	c := f.srv.Dial(t, tunneltest.Agent{Token: second.Token, Facts: facts})
	if c.Welcome == nil {
		t.Fatalf("the host could not enrol in another environment after its environment was removed: %+v", c.Refuse)
	}
}
