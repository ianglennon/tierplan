// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/testsupport"
	"github.com/ianglennon/bayplan/proto/wire"
)

// The health broker (plan 02-12, ENV-06, D-36, D-37, D-54): what the global stream
// is made of. The agents are real sessions of the real hub (tunneltest), the
// registry is the production one, and the clock is the one the test moves.

// next waits for the next event for which match is true, skipping the others.
func next(t *testing.T, ch <-chan HealthEvent, what string, match func(HealthEvent) bool) HealthEvent {
	t.Helper()
	deadline := testsupport.Context(t).Done()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("the subscription ended while waiting for %s", what)
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func ofType(typ string) func(HealthEvent) bool {
	return func(ev HealthEvent) bool { return ev.Type == typ }
}

// drain returns every event that is ready now.
func drain(ch <-chan HealthEvent) []HealthEvent {
	var out []HealthEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

// startBroker seeds the broker from the environments that exist now and runs it.
func startBroker(t *testing.T, r *tunRig) {
	t.Helper()
	ctx := t.Context()
	r.brk.Recompute(ctx)
	go r.brk.Run(ctx)
}

// tamper is a join token for the same environment with a wrong secret.
func tamper(token string) string {
	i := len("bpjt_") + 36 + 1 // the first character of the secret
	c := byte('A')
	if token[i] == 'A' {
		c = 'B'
	}
	return token[:i] + string(c) + token[i+1:]
}

func TestASubscriberHearsEnvStatusAndAgentConnectedWhenAnAgentConnects(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	startBroker(t, r)

	snap, err := r.brk.SnapshotAll(t.Context())
	if err != nil || len(snap) != 1 || snap[0].ID != env.ID || snap[0].Health.State != StatePending {
		t.Fatalf("snapshot = %+v, %v; want the one pending environment", snap, err)
	}
	backlog, ch, cancel := r.brk.Subscribe(t.Context())
	defer cancel()
	if len(backlog) != 0 {
		t.Fatalf("backlog = %+v, want none before anything happened", backlog)
	}

	r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)

	conn := next(t, ch, "agent.connected", ofType(EventAgentConnected))
	d, ok := conn.Data.(AgentConnectedData)
	if conn.EnvID != env.ID || !ok || d.Node != "edge-1" || d.Type != "standalone" || d.AgentID == "" || conn.At.IsZero() {
		t.Fatalf("agent.connected = %+v (%+v), want environment %s, node edge-1, type standalone", conn, conn.Data, env.ID)
	}
	st := next(t, ch, "env.status online", func(ev HealthEvent) bool {
		e, ok := ev.Data.(Environment)
		return ev.Type == EventEnvStatus && ok && e.Health.State == StateOnline
	})
	if st.EnvID != env.ID || st.ID <= conn.ID {
		t.Fatalf("env.status = %+v after agent.connected %+v; want the same environment and a later id", st, conn)
	}
}

func TestAnEnrolmentRefusedForAKnownEnvironmentPublishesAgentRefusedWithItsSource(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	_, ch, cancel := r.brk.Subscribe(t.Context())
	defer cancel()

	c := r.srv.Dial(t, tunneltest.Agent{Token: tamper(env.Token), Facts: &wire.Facts{EngineID: "ENG:x", Hostname: "intruder", EngineReachable: true}})
	if c.Refuse == nil || c.Refuse.Code != wire.ReasonAuthRejected {
		t.Fatalf("the enrolment was not refused with auth_rejected: %+v", c)
	}

	// The event is published before the agent hears the refusal, so it is there now.
	evs := drain(ch)
	if len(evs) != 1 || evs[0].Type != EventAgentRefused {
		t.Fatalf("events = %+v, want exactly one agent.refused", evs)
	}
	d, ok := evs[0].Data.(AgentRefusedData)
	if evs[0].EnvID != env.ID || !ok || d.Code != wire.ReasonAuthRejected || d.Mode != wire.ModeEnrol || d.Node != "intruder" || d.IP != "127.0.0.1" || evs[0].At.IsZero() {
		t.Fatalf("agent.refused = %+v (%+v), want environment %s, auth_rejected, enrol, intruder, 127.0.0.1", evs[0], evs[0].Data, env.ID)
	}
}

func TestAnEnrolmentWhoseTokenNamesNoKnownEnvironmentPublishesNothing(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	_, ch, cancel := r.brk.Subscribe(t.Context())
	defer cancel()

	// A well-formed token whose environment ID is one nobody created.
	stranger := strings.Replace(env.Token, env.ID, uuid.NewString(), 1)
	if c := r.srv.Dial(t, tunneltest.Agent{Token: stranger}); c.Refuse == nil {
		t.Fatalf("a stranger's token was not refused: %+v", c)
	}
	// And one that is not a token at all.
	if c := r.srv.Dial(t, tunneltest.Agent{Token: "not-a-token"}); c.Refuse == nil {
		t.Fatalf("garbage was not refused: %+v", c)
	}
	if evs := drain(ch); len(evs) != 0 {
		t.Fatalf("events = %+v, want none for an environment that does not exist", evs)
	}

	// The known one still works, so silence above was the filter and not a dead hook.
	r.srv.Dial(t, tunneltest.Agent{Token: tamper(env.Token)})
	if evs := drain(ch); len(evs) != 1 || evs[0].EnvID != env.ID {
		t.Fatalf("events = %+v, want one agent.refused for the real environment", evs)
	}
}

func TestAHelloRefusedBecauseTheAgentWasRevokedIsRoutedByItsCertificate(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.srv.Events.WaitLen(t, 1)
	id := a.conn.Identity
	_ = a.conn.WS.CloseNow()
	r.srv.Events.WaitLen(t, 2)
	r.srv.Exec(t, `UPDATE agents SET status = 'revoked', revoked_at = ? WHERE id = ?`, r.clk.Now().UnixNano(), a.id)
	_, ch, cancel := r.brk.Subscribe(t.Context())
	defer cancel()

	c := r.srv.Dial(t, tunneltest.Agent{Identity: &id})
	if c.Refuse == nil || c.Refuse.Code != wire.ReasonRevoked {
		t.Fatalf("the revoked agent was not refused: %+v", c)
	}
	ev := next(t, ch, "agent.refused", ofType(EventAgentRefused))
	d, _ := ev.Data.(AgentRefusedData)
	if ev.EnvID != env.ID || d.Code != wire.ReasonRevoked || d.Mode != wire.ModeHello {
		t.Fatalf("agent.refused = %+v (%+v), want environment %s, revoked, hello", ev, ev.Data, env.ID)
	}
}

func TestRefusalsFromOneAddressForOneEnvironmentAreNotRepeatedWithinTheThrottle(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	_, ch, cancel := r.brk.Subscribe(t.Context())
	defer cancel()

	for range 5 {
		r.srv.Dial(t, tunneltest.Agent{Token: tamper(env.Token)})
	}
	if evs := drain(ch); len(evs) != 1 {
		t.Fatalf("5 refusals in a row made %d events, want 1: a guesser must not fill the stream", len(evs))
	}
	r.clk.Advance(refusalEvery)
	r.srv.Dial(t, tunneltest.Agent{Token: tamper(env.Token)})
	if evs := drain(ch); len(evs) != 1 {
		t.Fatalf("a refusal after the throttle made %d events, want 1", len(evs))
	}
}

func TestEventIDsStrictlyIncreaseAndAResumeReplaysOnlyWhatIsNewer(t *testing.T) {
	b := NewBroker(nil, nil)
	for range 5 {
		b.Publish(HealthEvent{Type: EventAgentRefused, EnvID: "e"})
	}
	backlog, _, cancel := b.Subscribe(t.Context())
	defer cancel()
	if len(backlog) != 5 {
		t.Fatalf("backlog = %d events, want 5", len(backlog))
	}
	for i, ev := range backlog {
		if ev.ID != uint64(i+1) {
			t.Fatalf("event %d has id %d, want %d: ids must start at 1 and strictly increase", i, ev.ID, i+1)
		}
	}

	ids := func(evs []HealthEvent) []uint64 {
		var out []uint64
		for _, ev := range evs {
			out = append(out, ev.ID)
		}
		return out
	}
	for _, c := range []struct {
		last string
		want []uint64
	}{
		{"3", []uint64{4, 5}}, // the third event was the last seen
		{"5", nil},            // nothing is newer
		{"1", []uint64{2, 3, 4, 5}},
		{"999", []uint64{1, 2, 3, 4, 5}}, // an id this buffer never held replays everything
		{"0", []uint64{1, 2, 3, 4, 5}},   // so does one older than the buffer
		{"not-a-number", []uint64{1, 2, 3, 4, 5}},
	} {
		got := ids(EventsAfter(backlog, c.last))
		if len(got) != len(c.want) {
			t.Errorf("EventsAfter(%q) = %v, want %v", c.last, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("EventsAfter(%q) = %v, want %v", c.last, got, c.want)
				break
			}
		}
	}
}

func TestTheRingKeepsTheNewest1024Events(t *testing.T) {
	b := NewBroker(nil, nil)
	for range 1100 {
		b.Publish(HealthEvent{Type: EventAgentRefused, EnvID: "e"})
	}
	backlog, _, cancel := b.Subscribe(t.Context())
	defer cancel()
	if len(backlog) != 1024 {
		t.Fatalf("backlog = %d events, want 1024", len(backlog))
	}
	if backlog[0].ID != 77 || backlog[1023].ID != 1100 {
		t.Fatalf("backlog runs from %d to %d, want 77 to 1100", backlog[0].ID, backlog[1023].ID)
	}
}

func TestASubscriberThatFallsBehindIsDroppedAndTheOthersAreNotSlowed(t *testing.T) {
	b := NewBroker(nil, nil)
	_, slow, cancelSlow := b.Subscribe(t.Context())
	defer cancelSlow()
	_, fast, cancelFast := b.Subscribe(t.Context())
	defer cancelFast()

	var got []uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range fast {
			got = append(got, ev.ID)
			if len(got) == 500 {
				return
			}
		}
	}()
	for range 500 {
		b.Publish(HealthEvent{Type: EventAgentRefused, EnvID: "e"})
		time.Sleep(20 * time.Microsecond) //nolint:forbidigo // paces the publisher so the fast reader is never behind; the pace is the input of the slow-subscriber test
	}
	select {
	case <-done:
	case <-testsupport.Context(t).Done():
		t.Fatal("the fast subscriber did not receive its events")
	}
	if len(got) != 500 {
		t.Fatalf("the fast subscriber got %d events, want 500", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("ids went %d then %d", got[i-1], got[i])
		}
	}
	n := 0
	for closed := false; !closed; {
		select {
		case _, ok := <-slow: // closed by the broker once its buffer was full
			if !ok {
				closed = true
				break
			}
			n++
		case <-testsupport.Context(t).Done():
			t.Fatalf("the slow subscriber's channel was never closed (read %d events)", n)
		}
	}
	if n == 0 || n >= 500 {
		t.Fatalf("the slow subscriber read %d events before its channel closed, want a buffer's worth", n)
	}
}

func TestAnUnchangedRollupPublishesNothingAcrossTenTicks(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	r.srv.NewEnvironment(t, "Waiting")
	r.brk.Recompute(t.Context()) // the first pass only learns what is there
	_, ch, cancel := r.brk.Subscribe(t.Context())
	defer cancel()

	for range 10 {
		r.clk.Advance(5 * time.Second)
		r.brk.Recompute(t.Context())
	}
	if evs := drain(ch); len(evs) != 0 {
		t.Fatalf("events = %+v, want none: nothing changed", evs)
	}
}

func TestTheGraceEndingPublishesOneEnvStatusAndAnAgentGoingQuietAnotherAtThirtySeconds(t *testing.T) {
	r := newTunRig(t, nil) // the server "started" now: 60 s of grace
	env := r.srv.NewEnvironment(t, "Remote")
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.srv.Events.WaitLen(t, 1)
	r.whenOnline(t, env.ID)
	r.brk.Recompute(t.Context())
	_ = a.conn.WS.CloseNow()
	r.srv.Events.WaitLen(t, 2)
	_, ch, cancel := r.brk.Subscribe(t.Context())
	defer cancel()

	var states []State
	for range 18 { // 90 s in 5 s ticks
		r.clk.Advance(5 * time.Second)
		r.brk.Recompute(t.Context())
		for _, ev := range drain(ch) {
			if e, ok := ev.Data.(Environment); ok && ev.Type == EventEnvStatus {
				states = append(states, e.Health.State)
			}
		}
	}
	if len(states) != 2 || states[0] != StateReconnecting || states[1] != StateOffline {
		t.Fatalf("env.status states = %v, want exactly reconnecting (at 30 s) then offline (at 60 s)", states)
	}
}

func TestAnEnvironmentAddedAfterTheFirstPassIsPublishedOnceAndARemovedOneIsForgotten(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	r.brk.Recompute(t.Context())
	_, ch, cancel := r.brk.Subscribe(t.Context())
	defer cancel()

	env := r.srv.NewEnvironment(t, "Late")
	r.brk.Recompute(t.Context())
	r.brk.Recompute(t.Context())
	evs := drain(ch)
	if len(evs) != 1 || evs[0].Type != EventEnvStatus || evs[0].EnvID != env.ID {
		t.Fatalf("events = %+v, want one env.status for the new environment", evs)
	}

	r.srv.Exec(t, `DELETE FROM environments WHERE id = ?`, env.ID)
	r.brk.Recompute(t.Context())
	if evs := drain(ch); len(evs) != 0 {
		t.Fatalf("events = %+v, want none when an environment goes", evs)
	}
	r.brk.mu.Lock()
	n := len(r.brk.last)
	r.brk.mu.Unlock()
	if n != 0 {
		t.Fatalf("the broker still remembers %d environments after the last was removed", n)
	}
}

func TestConcurrentSubscribersPublishersAndRecomputesPassTheRaceDetector(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	startBroker(t, r)

	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			for range 20 {
				_, ch, cancel := r.brk.Subscribe(context.Background())
				var last uint64
				for _, ev := range drain(ch) {
					if ev.ID <= last {
						t.Errorf("ids went %d then %d", last, ev.ID)
					}
					last = ev.ID
				}
				cancel()
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 100 {
				r.brk.Publish(HealthEvent{Type: EventAgentRefused, EnvID: env.ID})
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			r.brk.Notify(env.ID)
			r.brk.Recompute(context.Background())
		}
	})
	r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil) // while the rest is going on
	wg.Wait()
	r.whenOnline(t, env.ID)
}
