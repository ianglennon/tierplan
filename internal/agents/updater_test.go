// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package agents_test

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/proto/wire"
)

// fakeUpdateHub is the part of the tunnel hub the updater uses: which agents of an
// environment are connected, and a place to write an update to.
type fakeUpdateHub struct {
	mu      sync.Mutex
	agents  map[string][]agents.Agent // by environment
	sent    []sentUpdate
	sendErr error

	uninstalls  []sentUninstall
	onUninstall func(agentID string, msg wire.Uninstall)
	goneAgents  map[string]bool
}

type sentUpdate struct {
	AgentID string
	Msg     wire.Update
}

type sentUninstall struct {
	AgentID string
	Msg     wire.Uninstall
}

// SendUninstall records the message and then runs onUninstall, which a test uses to
// answer it, outside the hub's lock.
func (h *fakeUpdateHub) SendUninstall(agentID string, msg wire.Uninstall) error {
	h.mu.Lock()
	if h.sendErr != nil {
		h.mu.Unlock()
		return h.sendErr
	}
	if h.goneAgents[agentID] {
		h.mu.Unlock()
		return tunnel.ErrAgentGone
	}
	h.uninstalls = append(h.uninstalls, sentUninstall{agentID, msg})
	hook := h.onUninstall
	h.mu.Unlock()
	if hook != nil {
		hook(agentID, msg)
	}
	return nil
}

func (h *fakeUpdateHub) SendUpdate(agentID string, msg wire.Update) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sendErr != nil {
		return h.sendErr
	}
	h.sent = append(h.sent, sentUpdate{agentID, msg})
	return nil
}

func (h *fakeUpdateHub) Connected(envID string) []agents.Agent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]agents.Agent(nil), h.agents[envID]...)
}

func (h *fakeUpdateHub) set(envID string, as ...agents.Agent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents[envID] = as
}

func (h *fakeUpdateHub) sentTo() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, s := range h.sent {
		out = append(out, s.AgentID)
	}
	return out
}

func (h *fakeUpdateHub) last(t *testing.T) sentUpdate {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sent) == 0 {
		t.Fatal("nothing was sent to any agent")
	}
	return h.sent[len(h.sent)-1]
}

type updRig struct {
	*rig
	hub   *fakeUpdateHub
	upd   *agents.Updater
	env   string
	token string
	mgr   map[string]string // the leader's agent ID by environment
	mgrMu sync.Mutex
}

// newUpdRig is a rig with an agent environment and an updater of a server at version.
func newUpdRig(t *testing.T, version string) *updRig {
	t.Helper()
	r := &updRig{rig: newRig(t), hub: &fakeUpdateHub{agents: map[string][]agents.Agent{}}, mgr: map[string]string{}}
	created := r.create(t, "Production")
	r.env, r.token = created.EnvID, created.JoinToken
	r.upd = agents.NewUpdater(r.svc, r.hub, func(envID string) (string, bool) {
		r.mgrMu.Lock()
		defer r.mgrMu.Unlock()
		id := r.mgr[envID]
		return id, id != ""
	}, version, func() time.Time { return r.clock }).WithTimeouts(150*time.Millisecond, 150*time.Millisecond, 150*time.Millisecond)
	t.Cleanup(r.upd.Close)
	return r
}

// enrol enrols one standalone host in the environment, so the database has an agent
// for the updater to find, and returns its ID.
func (r *updRig) enrol(t *testing.T, host string) string {
	t.Helper()
	req := newProof(t, "bayplan.example").request(r.token)
	req.Facts.Hostname = host
	req.Facts.EngineID = "ENG:" + host
	res, err := r.svc.Enrol(r.ctx, req)
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	return res.AgentID
}

func (r *updRig) leader(id string) {
	r.mgrMu.Lock()
	defer r.mgrMu.Unlock()
	r.mgr[r.env] = id
}

func (r *updRig) standalone(id, version string, selfUpdate bool) agents.Agent {
	return agents.Agent{ID: id, EnvID: r.env, Node: "node-" + id, Version: version, Role: "standalone", SelfUpdate: selfUpdate}
}

func (r *updRig) manager(id, version string) agents.Agent {
	return agents.Agent{ID: id, EnvID: r.env, Node: "node-" + id, Version: version, Role: "manager", SelfUpdate: true}
}

func (r *updRig) worker(id, version string) agents.Agent {
	return agents.Agent{ID: id, EnvID: r.env, Node: "node-" + id, Version: version, Role: "worker", SelfUpdate: true}
}

// connect makes the agents the connected ones and tells the updater each connected.
func (r *updRig) connect(as ...agents.Agent) {
	r.hub.set(r.env, as...)
	for _, a := range as {
		r.upd.OnConnected(a)
	}
}

func (r *updRig) auditRows(t *testing.T, action string) []map[string]string {
	t.Helper()
	rows, err := r.st.Read.Query(`SELECT actor_id, target_id, result, reason, details FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]string
	for rows.Next() {
		var actor, target, result, reason, details string
		if err := rows.Scan(&actor, &target, &result, &reason, &details); err != nil {
			t.Fatal(err)
		}
		m := map[string]string{"actor": actor, "target": target, "result": result, "reason": reason}
		var d map[string]string
		if json.Unmarshal([]byte(details), &d) == nil {
			for k, v := range d {
				m["details."+k] = v
			}
		}
		out = append(out, m)
	}
	return out
}

func (r *updRig) state(id string) string {
	s := r.upd.State(id)
	if s.Reason != "" {
		return s.State + ":" + s.Reason
	}
	return s.State
}

// awaitFailed waits for the nth agent.update_failed audit entry, which the updater writes
// after it has set the failed state, and then asserts the state of the agent.
func (r *updRig) awaitFailed(t *testing.T, id, want string, n int) {
	t.Helper()
	r.awaitAudit(t, audit.ActionAgentUpdateFailed, n)
	if got := r.state(id); got != want {
		t.Fatalf("update state of %s = %q, want %q", id, got, want)
	}
}

const ownRepo = "ghcr.io/ianglennon/bayplan-agent"

func TestUpdaterAReleaseServerUpdatesAnOlderConnectedStandaloneAgentOnceAndAuditsItAsTheUpdater(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.connect(r.standalone("a1", "0.2.0", true))

	if got := r.hub.sentTo(); len(got) != 1 || got[0] != "a1" {
		t.Fatalf("updates sent to %v, want exactly one to a1", got)
	}
	msg := r.hub.last(t).Msg
	if msg.Image != ownRepo+":0.2.1" {
		t.Errorf("image = %q, want %s:0.2.1", msg.Image, ownRepo)
	}
	if msg.ID == "" || len(msg.ID) > wire.MaxMessageIDLen {
		t.Errorf("id = %q, want a non-empty id of at most %d bytes", msg.ID, wire.MaxMessageIDLen)
	}
	rows := r.auditRows(t, audit.ActionAgentUpdated)
	if len(rows) != 1 || rows[0]["actor"] != "system:agent-update" || rows[0]["target"] != "a1" || rows[0]["result"] != "success" {
		t.Fatalf("agent.updated rows = %v, want one by system:agent-update about a1", rows)
	}
	if rows[0]["details.from"] != "0.2.0" || rows[0]["details.image"] != ownRepo+":0.2.1" || rows[0]["details.mode"] != "auto" {
		t.Errorf("agent.updated details = %v", rows[0])
	}
	if s := r.state("a1"); s != "updating" {
		t.Errorf("state after the send = %q, want updating", s)
	}

	// The agent answers, then reconnects as 0.2.1: the update is done.
	r.upd.HandleResult("a1", wire.UpdateResult{T: wire.TypeUpdateResult, ID: msg.ID, OK: true})
	if s := r.state("a1"); s != "updating" {
		t.Errorf("state after ok:true = %q, an accepted update is not a finished one", s)
	}
	r.connect(r.standalone("a1", "0.2.1", true))
	if s := r.state("a1"); s != "idle" {
		t.Errorf("state after reconnecting with the new version = %q, want idle", s)
	}
	if got := r.hub.sentTo(); len(got) != 1 {
		t.Errorf("updates sent = %v: an updated agent is not updated again", got)
	}
}

func TestUpdaterNoAutomaticUpdateFromADevEdgeOrUnversionedServer(t *testing.T) {
	for _, v := range []string{"dev (abc1234)", "edge", "", "unknown"} {
		r := newUpdRig(t, v)
		r.connect(r.standalone("a1", "0.2.0", true))
		if got := r.hub.sentTo(); len(got) != 0 {
			t.Errorf("a %q server sent updates to %v (RESEARCH Pitfall 23)", v, got)
		}
	}
}

func TestUpdaterNoAutomaticUpdateWhenTheAgentIsNotBehindIsNotAReleaseOrPredatesSelfUpdate(t *testing.T) {
	cases := []struct{ name, server, agent string }{
		{"same version", "v0.2.1", "0.2.1"},
		{"same version with a prerelease suffix", "v0.2.1", "0.2.1-rc.1"},
		{"newer agent", "v0.2.1", "0.2.2"},
		{"agent is a dev build", "v0.2.1", "dev (abc1234)"},
		{"agent predates self-update", "v0.1.5", "0.0.9"},
		{"agent outside the window", "v0.3.0", "0.1.0"},
	}
	for _, c := range cases {
		r := newUpdRig(t, c.server)
		r.connect(r.standalone("a1", c.agent, true))
		if got := r.hub.sentTo(); len(got) != 0 {
			t.Errorf("%s: updates sent to %v, want none", c.name, got)
		}
		if s := r.state("a1"); s != "idle" {
			t.Errorf("%s: state = %q, want idle", c.name, s)
		}
	}
}

func TestUpdaterNoUpdateWhenTheEnvironmentSwitchedAutoUpdateOffOrTheHostRefuses(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	if err := r.svc.SetAutoUpdate(r.ctx, audit.SystemCLI, r.env, false, "203.0.113.9", "test"); err != nil {
		t.Fatal(err)
	}
	r.connect(r.standalone("a1", "0.2.0", true))
	if got := r.hub.sentTo(); len(got) != 0 {
		t.Fatalf("auto-update off: updates sent to %v", got)
	}

	r2 := newUpdRig(t, "v0.2.1")
	r2.connect(r2.standalone("a1", "0.2.0", false))
	if got := r2.hub.sentTo(); len(got) != 0 {
		t.Fatalf("the host set BAYPLAN_AGENT_SELF_UPDATE=false: updates sent to %v", got)
	}
}

func TestUpdaterSwitchingAutoUpdateOnUpdatesTheAgentsThatAreAlreadyConnected(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	if err := r.svc.SetAutoUpdate(r.ctx, audit.SystemCLI, r.env, false, "", ""); err != nil {
		t.Fatal(err)
	}
	r.connect(r.standalone("a1", "0.2.0", true))
	if err := r.svc.SetAutoUpdate(r.ctx, audit.SystemCLI, r.env, true, "", ""); err != nil {
		t.Fatal(err)
	}
	r.upd.Sweep(r.env)
	if got := r.hub.sentTo(); len(got) != 1 {
		t.Fatalf("after switching it on, updates sent to %v, want one", got)
	}
}

func TestUpdaterASwarmWithThreeManagersGetsExactlyOneUpdateThroughTheLeader(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.leader("m2")
	all := []agents.Agent{r.manager("m1", "0.2.0"), r.manager("m2", "0.2.0"), r.manager("m3", "0.2.0"), r.worker("w1", "0.2.0")}
	r.connect(all...)

	if got := r.hub.sentTo(); len(got) != 1 || got[0] != "m2" {
		t.Fatalf("updates sent to %v, want exactly one, to the leader m2", got)
	}
	for _, id := range []string{"m1", "m2", "m3", "w1"} {
		if s := r.state(id); s != "updating" {
			t.Errorf("state of %s = %q: one message rolls every agent of the service, so all show updating", id, s)
		}
	}
	rows := r.auditRows(t, audit.ActionAgentUpdated)
	if len(rows) != 1 || rows[0]["target"] != "m2" {
		t.Fatalf("agent.updated rows = %v, want one, about the leader", rows)
	}

	// Each agent comes back as 0.2.1 as its node's task is replaced.
	for _, id := range []string{"m1", "w1", "m3"} {
		a := r.manager(id, "0.2.1")
		if id == "w1" {
			a = r.worker(id, "0.2.1")
		}
		r.upd.OnConnected(a)
	}
	if s := r.state("m1"); s != "idle" {
		t.Errorf("state of m1 after it reconnected as 0.2.1 = %q", s)
	}
	if s := r.state("m2"); s != "updating" {
		t.Errorf("state of m2, which has not reconnected yet = %q, want updating", s)
	}
	r.upd.OnConnected(r.manager("m2", "0.2.1"))
	if s := r.state("m2"); s != "idle" {
		t.Errorf("state of m2 after it reconnected as 0.2.1 = %q", s)
	}
	if got := r.hub.sentTo(); len(got) != 1 {
		t.Errorf("updates sent = %v: nothing more is sent while or after the service rolls", got)
	}
}

func TestUpdaterASwarmUpdateGoesToAnotherConnectedManagerWhenTheLeaderIsNotConnectedAndNeverToAWorker(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.leader("m9") // not connected
	r.connect(r.worker("w1", "0.2.0"), r.manager("m3", "0.2.0"), r.manager("m2", "0.2.0"))
	if got := r.hub.sentTo(); len(got) != 1 || got[0] != "m2" {
		t.Fatalf("updates sent to %v, want one to the connected manager with the lowest ID, m2", got)
	}

	w := newUpdRig(t, "v0.2.1")
	w.connect(w.worker("w1", "0.2.0"))
	if got := w.hub.sentTo(); len(got) != 0 {
		t.Fatalf("a swarm with only a worker connected: updates sent to %v (a worker answers not_a_manager)", got)
	}
}

func TestUpdaterASwarmUpdateWaitsForAManagerAndIsSentWhenOneConnects(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.connect(r.worker("w1", "0.2.0"))
	if got := r.hub.sentTo(); len(got) != 0 {
		t.Fatalf("sent early to %v", got)
	}
	r.connect(r.worker("w1", "0.2.0"), r.manager("m1", "0.2.0"))
	if got := r.hub.sentTo(); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("updates sent to %v, want one to m1 once it connected", got)
	}
}

func TestUpdaterAFailedUpdateIsRecordedWithItsReasonAndNotRetriedAutomatically(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.connect(r.standalone("a1", "0.2.0", true))
	id := r.hub.last(t).Msg.ID

	r.upd.HandleResult("a1", wire.UpdateResult{T: wire.TypeUpdateResult, ID: id, OK: false, Error: wire.UpdateErrImageNotOwnRepository})
	if s := r.state("a1"); s != "failed:image_not_own_repository" {
		t.Fatalf("state = %q, want failed with the agent's reason", s)
	}
	rows := r.auditRows(t, audit.ActionAgentUpdateFailed)
	if len(rows) != 1 || rows[0]["target"] != "a1" || rows[0]["result"] != "failure" || rows[0]["reason"] != "image_not_own_repository" || rows[0]["actor"] != "system:agent-update" {
		t.Fatalf("agent.update_failed rows = %v", rows)
	}

	// The agent reconnects still on the old version: a broken release must not loop.
	r.connect(r.standalone("a1", "0.2.0", true))
	r.connect(r.standalone("a1", "0.2.0", true))
	if got := r.hub.sentTo(); len(got) != 1 {
		t.Fatalf("updates sent to %v: a failed automatic update is not tried again for the same image", got)
	}
	if s := r.state("a1"); s != "failed:image_not_own_repository" {
		t.Errorf("state after another reconnect = %q, the failure stays until it is resolved", s)
	}
}

func TestUpdaterAnInsecureSessionRefusalIsAFailedUpdateWithThatReasonAndIsAudited(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.connect(r.standalone("a1", "0.2.0", true))
	id := r.hub.last(t).Msg.ID

	r.upd.HandleResult("a1", wire.UpdateResult{T: wire.TypeUpdateResult, ID: id, OK: false, Error: wire.UpdateErrInsecureSession})
	if s := r.state("a1"); s != "failed:insecure_session" {
		t.Fatalf("state = %q, want failed with insecure_session", s)
	}
	rows := r.auditRows(t, audit.ActionAgentUpdateFailed)
	if len(rows) != 1 || rows[0]["reason"] != "insecure_session" {
		t.Fatalf("agent.update_failed rows = %v", rows)
	}
}

func TestUpdaterAResultWithAnotherIDOrFromAnotherAgentIsIgnored(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.connect(r.standalone("a1", "0.2.0", true))
	r.upd.HandleResult("a1", wire.UpdateResult{T: wire.TypeUpdateResult, ID: "someone-elses", OK: false, Error: "update_failed"})
	r.upd.HandleResult("a2", wire.UpdateResult{T: wire.TypeUpdateResult, ID: r.hub.last(t).Msg.ID, OK: false, Error: "update_failed"})
	if s := r.state("a1"); s != "updating" {
		t.Fatalf("state = %q, want updating: a result is matched by agent and id", s)
	}
	if rows := r.auditRows(t, audit.ActionAgentUpdateFailed); len(rows) != 0 {
		t.Fatalf("agent.update_failed rows = %v", rows)
	}
}

func TestUpdaterAnAgentThatNeverAnswersIsCannotSelfUpdate(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.connect(r.standalone("a1", "0.2.0", true))
	// The state changes before the audit row is written, and awaitFailed waits for the row.
	r.awaitFailed(t, "a1", "failed:no_answer", 1)
	rows := r.auditRows(t, audit.ActionAgentUpdateFailed)
	if len(rows) != 1 || rows[0]["reason"] != "no_answer" {
		t.Fatalf("agent.update_failed rows = %v, want one with reason no_answer", rows)
	}
}

func TestUpdaterAnAcceptedUpdateWhoseAgentNeverComesBackWithTheNewVersionFails(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.connect(r.standalone("a1", "0.2.0", true))
	r.upd.HandleResult("a1", wire.UpdateResult{T: wire.TypeUpdateResult, ID: r.hub.last(t).Msg.ID, OK: true})
	r.awaitFailed(t, "a1", "failed:no_new_version", 1)
}

func TestUpdaterARolledBackStandaloneUpdateIsAFailureAndAReconnectWithTheNewVersionLaterHealsIt(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.connect(r.standalone("a1", "0.2.0", true))
	r.upd.HandleResult("a1", wire.UpdateResult{T: wire.TypeUpdateResult, ID: r.hub.last(t).Msg.ID, OK: true})
	// The helper rolled back: the old agent comes back.
	r.upd.OnConnected(r.standalone("a1", "0.2.0", true))
	if s := r.state("a1"); s != "failed:rolled_back" {
		t.Fatalf("state = %q, want failed:rolled_back", s)
	}
	if got := r.hub.sentTo(); len(got) != 1 {
		t.Fatalf("a rolled-back update is not tried again: sent to %v", got)
	}
	// Later the agent turns out to be on the new version (an update by hand).
	r.upd.OnConnected(r.standalone("a1", "0.2.1", true))
	if s := r.state("a1"); s != "idle" {
		t.Fatalf("state = %q, want idle once the agent runs the new version", s)
	}
}

func TestUpdaterASendThatCannotBeWrittenLeavesNoStateAndIsTriedAtTheNextConnect(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.hub.sendErr = tunnel.ErrAgentGone
	r.connect(r.standalone("a1", "0.2.0", true))
	if s := r.state("a1"); s != "idle" {
		t.Fatalf("state = %q, want idle: nothing was sent", s)
	}
	if rows := r.auditRows(t, audit.ActionAgentUpdated); len(rows) != 0 {
		t.Fatalf("agent.updated rows = %v, nothing was updated", rows)
	}
	r.hub.sendErr = nil
	r.connect(r.standalone("a1", "0.2.0", true))
	if got := r.hub.sentTo(); len(got) != 1 {
		t.Fatalf("updates sent to %v, want one after the write works", got)
	}
}

func TestUpdaterAnAdminCanUpdateOneAgentByHandEvenWithAutoUpdateOffAndItIsAuditedUnderTheirName(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	if err := r.svc.SetAutoUpdate(r.ctx, audit.SystemCLI, r.env, false, "", ""); err != nil {
		t.Fatal(err)
	}
	// The agent must be enrolled for the update to find it in the environment.
	ids := []string{r.enrol(t, "node-1")}
	r.hub.set(r.env, agents.Agent{ID: ids[0], EnvID: r.env, Node: "node-1", Version: "0.2.0", Role: "standalone", SelfUpdate: true})

	admin := audit.Actor{Type: audit.ActorUser, ID: "u-1", Name: "ada"}
	if err := r.upd.UpdateAgent(r.ctx, admin, r.env, ids[0], "198.51.100.7", "ua"); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	if got := r.hub.sentTo(); len(got) != 1 || got[0] != ids[0] {
		t.Fatalf("updates sent to %v", got)
	}
	rows := r.auditRows(t, audit.ActionAgentUpdated)
	if len(rows) != 1 || rows[0]["actor"] != "u-1" || rows[0]["details.mode"] != "manual" {
		t.Fatalf("agent.updated rows = %v, want one by the admin, mode manual", rows)
	}
	if err := r.upd.UpdateAgent(r.ctx, admin, r.env, ids[0], "", ""); !errors.Is(err, agents.ErrUpdateInProgress) {
		t.Fatalf("a second update while one runs: err = %v, want ErrUpdateInProgress", err)
	}
}

func TestUpdaterAManualUpdateRefusesAnOfflineAgentAnUpToDateOneAHostThatRefusesAndAStranger(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	ids := []string{r.enrol(t, "node-1")}
	admin := audit.Actor{Type: audit.ActorUser, ID: "u-1", Name: "ada"}

	if err := r.upd.UpdateAgent(r.ctx, admin, r.env, ids[0], "", ""); !errors.Is(err, agents.ErrAgentOffline) {
		t.Errorf("an offline agent: err = %v, want ErrAgentOffline", err)
	}
	if err := r.upd.UpdateAgent(r.ctx, admin, r.env, "nobody", "", ""); !errors.Is(err, agents.ErrAgentNotFound) {
		t.Errorf("an unknown agent: err = %v, want ErrAgentNotFound", err)
	}
	r.hub.set(r.env, agents.Agent{ID: ids[0], EnvID: r.env, Node: "node-1", Version: "0.2.1", Role: "standalone", SelfUpdate: true})
	if err := r.upd.UpdateAgent(r.ctx, admin, r.env, ids[0], "", ""); !errors.Is(err, agents.ErrAgentUpToDate) {
		t.Errorf("an up-to-date agent: err = %v, want ErrAgentUpToDate", err)
	}
	r.hub.set(r.env, agents.Agent{ID: ids[0], EnvID: r.env, Node: "node-1", Version: "0.2.0", Role: "standalone", SelfUpdate: false})
	if err := r.upd.UpdateAgent(r.ctx, admin, r.env, ids[0], "", ""); !errors.Is(err, agents.ErrSelfUpdateOff) {
		t.Errorf("a host with self-update off: err = %v, want ErrSelfUpdateOff", err)
	}
	if got := r.hub.sentTo(); len(got) != 0 {
		t.Errorf("updates sent to %v, want none", got)
	}
}

func TestUpdaterUpdateAllUpdatesEachBehindStandaloneAgentAndSkipsTheRest(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	// Three standalone agents in one environment: two behind, one current, one refusing.
	r.hub.set(r.env,
		r.standalone("a1", "0.2.0", true), r.standalone("a2", "0.2.0", true),
		r.standalone("a3", "0.2.1", true), r.standalone("a4", "0.2.0", false))
	if err := r.svc.SetAutoUpdate(r.ctx, audit.SystemCLI, r.env, false, "", ""); err != nil {
		t.Fatal(err)
	}
	admin := audit.Actor{Type: audit.ActorUser, ID: "u-1", Name: "ada"}
	n, err := r.upd.UpdateAll(r.ctx, admin, r.env, "", "")
	if err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}
	got := r.hub.sentTo()
	sort.Strings(got)
	if n != 2 || strings.Join(got, ",") != "a1,a2" {
		t.Fatalf("started %d, sent to %v, want a1 and a2", n, got)
	}
}

func TestUpdaterUpdateAllOnASwarmSendsOneUpdateThroughAManager(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.leader("m1")
	r.hub.set(r.env, r.manager("m1", "0.2.0"), r.manager("m2", "0.2.0"), r.worker("w1", "0.2.0"))
	n, err := r.upd.UpdateAll(r.ctx, audit.Actor{Type: audit.ActorUser, ID: "u-1", Name: "ada"}, r.env, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.hub.sentTo(); n != 1 || len(got) != 1 || got[0] != "m1" {
		t.Fatalf("started %d, sent to %v, want one update through the leader", n, got)
	}
}

func TestUpdaterUpdateAllWithAgentsButNoneConnectedIsAgentOffline(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.enrol(t, "node-1")
	_, err := r.upd.UpdateAll(r.ctx, audit.Actor{Type: audit.ActorUser, ID: "u-1", Name: "ada"}, r.env, "", "")
	if !errors.Is(err, agents.ErrAgentOffline) {
		t.Fatalf("err = %v, want ErrAgentOffline", err)
	}
}

func TestUpdaterAManualUpdateOnADevServerAsksForTheEdgeImage(t *testing.T) {
	r := newUpdRig(t, "dev (abc1234)")
	r.hub.set(r.env, r.standalone("a1", "dev (old)", true))
	if _, err := r.upd.UpdateAll(r.ctx, audit.Actor{Type: audit.ActorUser, ID: "u-1", Name: "ada"}, r.env, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.hub.last(t).Msg.Image; got != ownRepo+":edge" {
		t.Fatalf("image = %q, want the edge image", got)
	}
}

func TestUpdaterAutoUpdateDefaultsToOnAndAChangeIsAuditedOnlyWhenItChanges(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	on, err := r.svc.AutoUpdate(r.ctx, r.env)
	if err != nil || !on {
		t.Fatalf("AutoUpdate = %v, %v; want on by default", on, err)
	}
	admin := audit.Actor{Type: audit.ActorUser, ID: "u-1", Name: "ada"}
	if err := r.svc.SetAutoUpdate(r.ctx, admin, r.env, true, "", ""); err != nil {
		t.Fatal(err)
	}
	if rows := r.auditRows(t, audit.ActionEnvironmentAutoUpdateChanged); len(rows) != 0 {
		t.Fatalf("switching on what is on audited %v", rows)
	}
	if err := r.svc.SetAutoUpdate(r.ctx, admin, r.env, false, "198.51.100.7", "ua"); err != nil {
		t.Fatal(err)
	}
	rows := r.auditRows(t, audit.ActionEnvironmentAutoUpdateChanged)
	if len(rows) != 1 || rows[0]["actor"] != "u-1" || rows[0]["target"] != r.env || rows[0]["details.from"] != "on" || rows[0]["details.to"] != "off" {
		t.Fatalf("environment.auto_update_changed rows = %v", rows)
	}
	if on, _ := r.svc.AutoUpdate(r.ctx, r.env); on {
		t.Fatal("AutoUpdate is still on")
	}
	if err := r.svc.SetAutoUpdate(r.ctx, admin, "no-such-env", false, "", ""); !errors.Is(err, agents.ErrEnvironmentNotFound) {
		t.Fatalf("an unknown environment: err = %v", err)
	}
	// Reading a missing environment is an error too, so the updater never treats
	// "unknown" as "on".
	if _, err := r.svc.AutoUpdate(r.ctx, "no-such-env"); !errors.Is(err, agents.ErrEnvironmentNotFound) {
		t.Fatalf("reading an unknown environment: err = %v", err)
	}
}

func TestUpdaterStateOfAnAgentTheUpdaterNeverHeardOfIsIdle(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	if s := r.state("never-seen"); s != "idle" {
		t.Fatalf("state = %q", s)
	}
}

func TestUpdaterManualUpdateIsNeededOnlyWhenBayplanCannotDoItForTheAdmin(t *testing.T) {
	cases := []struct {
		name                 string
		server, agent        string
		connected, selfAllow bool
		want                 bool
	}{
		{"behind, connected, host allows it", "v0.2.1", "0.2.0", true, true, false},
		{"behind, connected, host refuses", "v0.2.1", "0.2.0", true, false, true},
		{"behind, offline: the host's policy is not known", "v0.2.1", "0.2.0", false, false, false},
		{"outside the window", "v0.3.0", "0.1.4", false, false, true},
		{"too old to know the message", "v0.1.5", "0.0.9", true, true, true},
		{"up to date", "v0.2.1", "0.2.1", true, false, false},
		{"a dev agent", "v0.2.1", "dev (abc)", true, false, false},
		{"a dev server", "dev (abc)", "0.2.0", true, false, false},
	}
	for _, c := range cases {
		if got := agents.ManualUpdateNeeded(c.server, c.agent, c.connected, c.selfAllow); got != c.want {
			t.Errorf("%s: ManualUpdateNeeded = %v, want %v", c.name, got, c.want)
		}
	}
}
