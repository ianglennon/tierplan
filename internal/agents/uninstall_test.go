// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package agents_test

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/proto/testsupport"
	"github.com/ianglennon/bayplan/proto/wire"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "ada"}

func (h *fakeUpdateHub) uninstallsTo() []sentUninstall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sentUninstall(nil), h.uninstalls...)
}

// answerWith makes the fake hub answer every uninstall through the updater, as the
// tunnel does when the agent's update_result arrives, with ok or the error code.
func (r *updRig) answerWith(ok func(agentID string, msg wire.Uninstall) (bool, string)) {
	r.hub.onUninstall = func(agentID string, msg wire.Uninstall) {
		good, code := ok(agentID, msg)
		go r.upd.HandleResult(agentID, wire.UpdateResult{T: wire.TypeUpdateResult, ID: msg.ID, OK: good, Error: code})
	}
}

func okAlways(string, wire.Uninstall) (bool, string) { return true, "" }

func (r *updRig) uninstall(timeout time.Duration) []string {
	got := r.upd.UninstallAll(r.ctx, admin, r.env, "203.0.113.9", "test-agent", timeout)
	sort.Strings(got)
	return got
}

func TestUninstallAllSendsEveryOnlineStandaloneAgentAnUninstallThatRemovesItsDataAndAuditsEachConfirmation(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.hub.set(r.env, r.standalone("a1", "0.2.1", true), r.standalone("a2", "0.2.1", true))
	r.answerWith(okAlways)

	got := r.uninstall(testsupport.Deadline)
	if !reflect.DeepEqual(got, []string{"a1", "a2"}) {
		t.Fatalf("confirmed = %v, want a1 and a2", got)
	}
	sent := r.hub.uninstallsTo()
	if len(sent) != 2 {
		t.Fatalf("sent %d uninstalls, want 2", len(sent))
	}
	for _, s := range sent {
		if !s.Msg.RemoveData || s.Msg.Service || s.Msg.ID == "" || len(s.Msg.ID) > wire.MaxMessageIDLen {
			t.Errorf("message to %s = %+v: want removeData, no service flag and a short id", s.AgentID, s.Msg)
		}
	}
	if sent[0].Msg.ID == sent[1].Msg.ID {
		t.Error("two agents were sent the same request id")
	}
	rows := r.auditRows(t, audit.ActionAgentUninstalled)
	if len(rows) != 2 {
		t.Fatalf("%d agent.uninstalled rows, want one per confirming agent: %v", len(rows), rows)
	}
	for _, row := range rows {
		if row["actor"] != admin.ID || row["result"] != string(audit.ResultSuccess) {
			t.Errorf("row %v: want the requesting admin as actor and success", row)
		}
	}
}

func TestUninstallAllWaitsForTheAnswersAndNotLongerThanTheTimeout(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.hub.set(r.env, r.standalone("a1", "0.2.1", true), r.standalone("a2", "0.2.1", true))
	// a1 answers; a2 never does, so the call ends only when its timeout does.
	r.hub.onUninstall = func(agentID string, msg wire.Uninstall) {
		if agentID == "a1" {
			go r.upd.HandleResult("a1", wire.UpdateResult{T: wire.TypeUpdateResult, ID: msg.ID, OK: true})
		}
	}
	const timeout = 400 * time.Millisecond
	type outcome struct {
		confirmed []string
		took      time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		start := time.Now()
		got := r.uninstall(timeout)
		// wall-clock: lower bound; a timer cannot fire early, so load can only lengthen it
		done <- outcome{got, time.Since(start)}
	}()
	// The "not longer than the timeout" half: a call that ignored its timeout and
	// kept waiting for a2 fails here, at the shared deadline and with this message,
	// instead of hanging the package until the test binary's timeout.
	res := testsupport.Await(t, "UninstallAll to return after its 400 ms timeout although a2 never answers", done)
	if !reflect.DeepEqual(res.confirmed, []string{"a1"}) {
		t.Fatalf("confirmed = %v, want only a1", res.confirmed)
	}
	// A timer cannot fire early, so this holds on any runner.
	if res.took < timeout {
		t.Fatalf("took %v, want to wait for a2 until the %v timeout", res.took, timeout)
	}
	if rows := r.auditRows(t, audit.ActionAgentUninstalled); len(rows) != 1 || rows[0]["target"] != "a1" {
		t.Fatalf("audit rows = %v, want only a1's", rows)
	}
}

func TestUninstallAllDoesNotWaitForTheTimeoutWhenEveryoneHasAnswered(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.hub.set(r.env, r.standalone("a1", "0.2.1", true))
	r.answerWith(okAlways)
	// The timeout is longer than the shared deadline, so a call that waited for it
	// instead of returning once a1 answered would miss the deadline every time.
	done := make(chan []string, 1)
	go func() { done <- r.uninstall(10 * time.Minute) }()
	got := testsupport.Await(t, "UninstallAll to return once its only agent had answered, long before its 10 minute timeout", done)
	if !reflect.DeepEqual(got, []string{"a1"}) {
		t.Fatalf("confirmed = %v, want a1", got)
	}
}

func TestUninstallAllDoesNotConfirmARefusalAnErrorASendFailureOrAnAnswerForSomeoneElse(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.hub.set(r.env,
		r.standalone("refuses", "0.2.1", true), r.standalone("gone", "0.2.1", true),
		r.standalone("liar", "0.2.1", true), r.standalone("good", "0.2.1", true))
	r.hub.goneAgents = map[string]bool{"gone": true}
	r.hub.onUninstall = func(agentID string, msg wire.Uninstall) {
		switch agentID {
		case "refuses":
			go r.upd.HandleResult(agentID, wire.UpdateResult{ID: msg.ID, OK: false, Error: wire.UpdateErrSelfUpdateDisabled})
		case "liar":
			// An answer with another request id, and one that claims to be from someone
			// else's id, must not count.
			go r.upd.HandleResult(agentID, wire.UpdateResult{ID: "not-mine", OK: true})
			go r.upd.HandleResult("good", wire.UpdateResult{ID: msg.ID, OK: true})
		case "good":
			go r.upd.HandleResult(agentID, wire.UpdateResult{ID: msg.ID, OK: true})
		}
	}
	// wall-clock: liar never gives a valid answer, so the call ends only with its timeout; it is the window the other three answer within
	got := r.uninstall(500 * time.Millisecond)
	if !reflect.DeepEqual(got, []string{"good"}) {
		t.Fatalf("confirmed = %v, want only good", got)
	}
}

func TestUninstallAllSkipsAHostThatTurnedSelfUpdateOffAndAsksNothingOfAnEnvironmentWithNoAgentsOnline(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.hub.set(r.env, r.standalone("off", "0.2.1", false), r.standalone("on", "0.2.1", true))
	r.answerWith(okAlways)
	if got := r.uninstall(testsupport.Deadline); !reflect.DeepEqual(got, []string{"on"}) {
		t.Fatalf("confirmed = %v, want only on", got)
	}
	for _, s := range r.hub.uninstallsTo() {
		if s.AgentID == "off" {
			t.Fatal("an agent whose host refuses self-update was sent an uninstall")
		}
	}

	r = newUpdRig(t, "v0.2.1")
	if got := r.uninstall(testsupport.Deadline); len(got) != 0 || len(r.hub.uninstallsTo()) != 0 {
		t.Fatalf("an environment with nobody online: confirmed %v, sent %v", got, r.hub.uninstallsTo())
	}
}

func TestUninstallAllOnASwarmTellsEveryOtherAgentFirstAndThenOnlyTheLeaderToRemoveTheService(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.hub.set(r.env, r.manager("m1", "0.2.1"), r.manager("m2", "0.2.1"), r.manager("m3", "0.2.1"), r.worker("w1", "0.2.1"), r.worker("w2", "0.2.1"))
	r.leader("m2")

	var mu sync.Mutex
	answered := map[string]bool{}
	var leaderSawUnanswered []string
	// The others answer only once the test has seen all four of them asked and no
	// service removal sent, so "the leader waits for them" is asserted as an ordering:
	// the leader is not asked while they are unanswered.
	release := make(chan struct{})
	askedOthers := make(chan struct{}, 4)
	r.hub.onUninstall = func(agentID string, msg wire.Uninstall) {
		mu.Lock()
		if msg.Service {
			for _, other := range []string{"m1", "m3", "w1", "w2"} {
				if !answered[other] {
					leaderSawUnanswered = append(leaderSawUnanswered, other)
				}
			}
		}
		mu.Unlock()
		if !msg.Service {
			askedOthers <- struct{}{}
		}
		go func() {
			if !msg.Service {
				<-release
			}
			mu.Lock()
			answered[agentID] = true
			mu.Unlock()
			r.upd.HandleResult(agentID, wire.UpdateResult{ID: msg.ID, OK: true})
		}()
	}

	result := make(chan []string, 1)
	go func() { result <- r.uninstall(testsupport.Deadline) }()
	for range 4 {
		testsupport.Await(t, "the other four agents to be asked to uninstall", askedOthers)
	}
	for _, s := range r.hub.uninstallsTo() {
		if s.Msg.Service {
			t.Fatalf("the leader %s was asked to remove the service while the others had not answered", s.AgentID)
		}
	}
	close(release)
	got := testsupport.Await(t, "the uninstall to finish", result)
	if !reflect.DeepEqual(got, []string{"m1", "m2", "m3", "w1", "w2"}) {
		t.Fatalf("confirmed = %v, want all five", got)
	}
	var withService []string
	for _, s := range r.hub.uninstallsTo() {
		if s.Msg.Service {
			withService = append(withService, s.AgentID)
		}
		if !s.Msg.RemoveData {
			t.Errorf("%s was not told to remove its data", s.AgentID)
		}
	}
	if !reflect.DeepEqual(withService, []string{"m2"}) {
		t.Fatalf("service removal asked of %v, want only the leader m2", withService)
	}
	if len(leaderSawUnanswered) != 0 {
		t.Fatalf("the leader was told to remove the service before %v had answered: removing it ends their agents before their helpers exist", leaderSawUnanswered)
	}
	last := r.hub.uninstallsTo()
	if !last[len(last)-1].Msg.Service {
		t.Fatalf("the service removal was not the last message: %v", last)
	}
}

func TestUninstallAllOnASwarmUsesAnotherManagerWhenTheLeaderIsNotConnectedOrRefusesSelfUpdate(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	m1 := r.manager("m1", "0.2.1")
	m2 := r.manager("m2", "0.2.1")
	m2.SelfUpdate = false
	r.hub.set(r.env, m1, m2, r.worker("w1", "0.2.1"))
	r.leader("m2") // the leader refuses self-update
	r.answerWith(okAlways)
	r.uninstall(testsupport.Deadline)
	var withService []string
	for _, s := range r.hub.uninstallsTo() {
		if s.Msg.Service {
			withService = append(withService, s.AgentID)
		}
		if s.AgentID == "m2" {
			t.Fatal("an agent whose host refuses self-update was sent an uninstall")
		}
	}
	if !reflect.DeepEqual(withService, []string{"m1"}) {
		t.Fatalf("service removal asked of %v, want m1", withService)
	}
}

func TestUninstallAllOnASwarmWithNoManagerAbleToRemoveTheServiceSendsNothing(t *testing.T) {
	for name, agentsOnline := range map[string]func(r *updRig){
		"only workers": func(r *updRig) { r.hub.set(r.env, r.worker("w1", "0.2.1"), r.worker("w2", "0.2.1")) },
		"managers that refuse": func(r *updRig) {
			m := r.manager("m1", "0.2.1")
			m.SelfUpdate = false
			r.hub.set(r.env, m, r.worker("w1", "0.2.1"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newUpdRig(t, "v0.2.1")
			agentsOnline(r)
			r.answerWith(okAlways)
			if got := r.uninstall(testsupport.Deadline); len(got) != 0 {
				t.Fatalf("confirmed %v", got)
			}
			if s := r.hub.uninstallsTo(); len(s) != 0 {
				t.Fatalf("sent %v: helpers on workers would wait for a service nobody removes", s)
			}
		})
	}
}

func TestUninstallAllCanBeCancelled(t *testing.T) {
	r := newUpdRig(t, "v0.2.1")
	r.hub.set(r.env, r.standalone("a1", "0.2.1", true))
	asked := make(chan struct{}, 1)
	r.hub.onUninstall = func(string, wire.Uninstall) { asked <- struct{}{} }
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	// The call is waiting on the answer a1 never gives once the hub has been asked to
	// send; cancel it then.
	go func() {
		select {
		case <-asked:
			cancel()
		case <-ctx.Done():
		}
	}()
	// The timeout is longer than the shared deadline, so a call that ignored the
	// cancellation and waited for it would miss the deadline every time.
	done := make(chan []string, 1)
	go func() { done <- r.upd.UninstallAll(ctx, admin, r.env, "", "", 10*time.Minute) }()
	got := testsupport.Await(t, "UninstallAll to return after its context was cancelled", done)
	if len(got) != 0 {
		t.Fatalf("confirmed %v after the call was cancelled", got)
	}
}

func TestUninstallAllTreatsAnInsecureSessionRefusalAsNotUninstalledSoTheManualCleanupStaysTheWay(t *testing.T) {
	for name, set := range map[string]func(r *updRig){
		"standalone": func(r *updRig) {
			r.hub.set(r.env, r.standalone("a1", "0.2.1", true), r.standalone("a2", "0.2.1", true))
		},
		"swarm": func(r *updRig) {
			r.hub.set(r.env, r.manager("m1", "0.2.1"), r.worker("w1", "0.2.1"))
			r.leader("m1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newUpdRig(t, "v0.2.1")
			set(r)
			r.answerWith(func(string, wire.Uninstall) (bool, string) { return false, wire.UpdateErrInsecureSession })
			if got := r.uninstall(testsupport.Deadline); len(got) != 0 {
				t.Fatalf("confirmed %v: an agent that refused on an insecure session did not uninstall", got)
			}
			if rows := r.auditRows(t, audit.ActionAgentUninstalled); len(rows) != 0 {
				t.Fatalf("audited %v as uninstalled", rows)
			}
		})
	}
}
