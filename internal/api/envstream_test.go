// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// The global environments stream (plan 02-12, ENV-06, D-36, D-37, D-54, T-02-46,
// T-02-47): one stream per tab, a snapshot first, then the events of the health
// broker, filtered by what the caller may read. The agents are real sessions of
// the real hub and the router is the production one.

const envStreamPath = "/api/environments/stream"

// streamEvent is the data of every frame but the snapshot.
type streamEvent struct {
	EnvID       string        `json:"envId"`
	At          string        `json:"at"`
	Environment *agentEnvBody `json:"environment"`
	AgentID     string        `json:"agentId"`
	Node        string        `json:"node"`
	Type        string        `json:"type"`
	Reason      string        `json:"reason"`
	Code        string        `json:"code"`
	Mode        string        `json:"mode"`
	IP          string        `json:"ip"`
	FromAgentID string        `json:"fromAgentId"`
	FromNode    string        `json:"fromNode"`
	ToAgentID   string        `json:"toAgentId"`
	ToNode      string        `json:"toNode"`
	Kind        string        `json:"kind"`
}

type snapshotBody struct {
	Items []agentEnvBody `json:"items"`
}

func (f *agentFix) openEnvStream(t *testing.T, cookie, lastEventID string) *sseConn {
	t.Helper()
	return openSSE(t, f.authFixture, envStreamPath, cookie, lastEventID)
}

// nextFrame returns the next data frame or fails.
func nextFrame(t *testing.T, c *sseConn) frame {
	t.Helper()
	fr, ok := c.next(5 * time.Second)
	if !ok {
		t.Fatal("no frame arrived")
	}
	return fr
}

// nextOf returns the next frame of the given event name, skipping the others.
func nextOf(t *testing.T, c *sseConn, event string) (frame, streamEvent) {
	t.Helper()
	for {
		fr := nextFrame(t, c)
		if fr.event != event {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal([]byte(fr.data), &ev); err != nil {
			t.Fatalf("frame %s data %q: %v", event, fr.data, err)
		}
		return fr, ev
	}
}

func pings(c *sseConn) int { return strings.Count(c.bodyString(), ": ping") }

// waitPings waits for n more pings than there are now.
func waitPings(t *testing.T, c *sseConn, n int) {
	t.Helper()
	want := pings(c) + n
	waitFor(t, "pings", func() bool { return pings(c) >= want })
}

// grantRead gives user environments.read on one environment only.
func (f *authFixture) grantRead(t *testing.T, user, envID string) {
	t.Helper()
	role := "role-read-" + user
	for _, q := range []string{
		`INSERT OR IGNORE INTO roles (id, name, builtin) VALUES ('` + role + `', '` + role + `', 0)`,
		`INSERT OR IGNORE INTO role_permissions (role_id, permission) VALUES ('` + role + `', 'environments.read')`,
	} {
		if _, err := f.st.Write.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.st.Write.Exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
		VALUES (?, 'user', ?, ?, ?, 0)`, "ra-"+user+"-"+envID, "id-"+user, role, envID); err != nil {
		t.Fatal(err)
	}
}

func refused(envID, ip string) envs.HealthEvent {
	return envs.HealthEvent{Type: envs.EventAgentRefused, EnvID: envID, Data: envs.AgentRefusedData{Code: "auth_rejected", Mode: "enrol", Node: "n", IP: ip}}
}

func TestEnvStreamSendsTheSnapshotFirstThenEnvStatusAndAgentConnected(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Remote")
	c := f.openEnvStream(t, f.cookie, "")
	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d", c.status0())
	}
	h := c.header
	if h.Get("Content-Type") != "text/event-stream" || h.Get("Cache-Control") != "no-cache" || h.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers = %v, want an unbuffered text/event-stream", h)
	}

	fr := nextFrame(t, c)
	var snap snapshotBody
	if fr.event != "snapshot" || fr.id != "" || json.Unmarshal([]byte(fr.data), &snap) != nil || len(snap.Items) != 1 || snap.Items[0].ID != env.ID || snap.Items[0].Status.State != "pending" {
		t.Fatalf("first frame = %+v, want the snapshot of the one pending environment and no id", fr)
	}

	f.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1", Version: "28.4.0"}, nil)
	conn, cev := nextOf(t, c, "agent.connected")
	if cev.EnvID != env.ID || cev.Node != "edge-1" || cev.Type != "standalone" || cev.AgentID == "" || cev.At == "" {
		t.Fatalf("agent.connected = %+v, want environment %s, node edge-1, type standalone", cev, env.ID)
	}
	var online frame
	var oev streamEvent
	for {
		online, oev = nextOf(t, c, "env.status")
		if oev.Environment != nil && oev.Environment.Status.State == "online" {
			break
		}
	}
	a, _ := strconv.Atoi(conn.id)
	b, _ := strconv.Atoi(online.id)
	if oev.EnvID != env.ID || oev.Environment.ID != env.ID || oev.Environment.Name != "Remote" || a == 0 || b <= a {
		t.Fatalf("env.status = %+v (id %s) after agent.connected id %s: want the same environment and a larger id", oev, online.id, conn.id)
	}
}

func TestEnvStreamReplaysOnlyEventsAfterLastEventIDAndAnUnknownIDReplaysTheBuffer(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Remote")
	var ids []string
	for range 5 {
		ids = append(ids, strconv.FormatUint(f.brk.Publish(refused(env.ID, "203.0.113.7")).ID, 10))
	}
	replayed := func(lastID string) []string {
		c := f.openEnvStream(t, f.cookie, lastID)
		if fr := nextFrame(t, c); fr.event != "snapshot" {
			t.Fatalf("first frame = %+v, want the snapshot even on a resume", fr)
		}
		var got []string
		for {
			fr, ok := c.next(150 * time.Millisecond)
			if !ok {
				return got
			}
			if fr.event == "agent.refused" {
				got = append(got, fr.id)
			}
		}
	}
	if got := replayed(ids[2]); strings.Join(got, ",") != strings.Join(ids[3:], ",") {
		t.Errorf("resuming after %s replayed %v, want %v", ids[2], got, ids[3:])
	}
	if got := replayed(ids[4]); len(got) != 0 {
		t.Errorf("resuming after the newest event replayed %v, want nothing", got)
	}
	if got := replayed("987654321"); strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Errorf("an unknown id replayed %v, want the whole buffer %v", got, ids)
	}
	if got := replayed(""); len(got) != 0 {
		t.Errorf("a fresh connection replayed %v, want only the snapshot and what happens next", got)
	}
}

func TestEnvStreamShowsACallerOnlyWhatTheyMayReadAndStopsAtThePingAfterAGrantIsWithdrawn(t *testing.T) {
	f := newAgentFix(t)
	f.envStreamHeartbeat = 20 * time.Millisecond
	f.h = f.build(t)
	a := f.srv.NewEnvironment(t, "A")
	b := f.srv.NewEnvironment(t, "B")
	f.addUser(t, "viewer", false)
	f.grantRead(t, "viewer", a.ID)
	cookie, _ := f.signIn(t, "viewer", false)

	c := f.openEnvStream(t, cookie, "")
	fr := nextFrame(t, c)
	if fr.event != "snapshot" || !strings.Contains(fr.data, a.ID) || strings.Contains(fr.data, b.ID) || strings.Contains(fr.data, `"B"`) {
		t.Fatalf("snapshot = %s, want environment A and no sign of B", fr.data)
	}

	f.brk.Publish(refused(b.ID, "198.51.100.1"))
	f.brk.Publish(refused(a.ID, "198.51.100.2"))
	_, ev := nextOf(t, c, "agent.refused")
	if ev.EnvID != a.ID || ev.IP != "198.51.100.2" {
		t.Fatalf("the first refusal the viewer saw was %+v, want A's: B's must never arrive", ev)
	}

	if _, err := f.st.Write.Exec(`DELETE FROM role_assignments WHERE principal_id = 'id-viewer'`); err != nil {
		t.Fatal(err)
	}
	waitPings(t, c, 2) // at least one ping began after the grant went
	f.brk.Publish(refused(a.ID, "198.51.100.3"))
	waitPings(t, c, 3)
	if fr, ok := c.next(50 * time.Millisecond); ok {
		t.Fatalf("a frame %+v reached the viewer after their grant was withdrawn and a ping had passed", fr)
	}
}

func TestEnvStreamSendsANoGrantUserAnEmptySnapshotAndNothingElse(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Secret")
	f.addUser(t, "nobody", false)
	cookie, _ := f.signIn(t, "nobody", false)
	c := f.openEnvStream(t, cookie, "")
	fr := nextFrame(t, c)
	if fr.event != "snapshot" || strings.Contains(fr.data, env.ID) || !strings.Contains(fr.data, `"items":[]`) {
		t.Fatalf("snapshot = %+v, want an empty list", fr)
	}
	f.brk.Publish(refused(env.ID, "198.51.100.9"))
	if fr, ok := c.next(100 * time.Millisecond); ok {
		t.Fatalf("a user with no grant received %+v", fr)
	}
}

func TestEnvStreamPingsAtTheConfiguredIntervalAndEndsWithinAPingOfSignOut(t *testing.T) {
	f := newAgentFix(t)
	f.envStreamHeartbeat = 20 * time.Millisecond
	f.h = f.build(t)
	c := f.openEnvStream(t, f.cookie, "")
	waitPings(t, c, 3)
	if _, err := f.st.Write.Exec(`DELETE FROM sessions`); err != nil {
		t.Fatal(err)
	}
	c.waitEnded(t, time.Second, "the session was deleted")
}

func TestEnvStreamEndsAtItsMaximumLifetime(t *testing.T) {
	f := newAgentFix(t)
	f.envStreamMaxLifetime = 100 * time.Millisecond
	f.h = f.build(t)
	c := f.openEnvStream(t, f.cookie, "")
	c.waitEnded(t, time.Second, "its maximum lifetime passed")
}

func TestEnvStreamAllowsFourPerUserAndRefusesTheFifthWith429(t *testing.T) {
	f := newAgentFix(t)
	var conns []*sseConn
	for range 4 {
		c := f.openEnvStream(t, f.cookie, "")
		if c.status0() != http.StatusOK {
			t.Fatalf("stream %d status = %d", len(conns)+1, c.status0())
		}
		conns = append(conns, c)
	}
	fifth := f.openEnvStream(t, f.cookie, "")
	// openSSE returns once the status is written; the problem body follows, so
	// wait for the refused handler to return before reading it.
	select {
	case <-fifth.done:
	case <-time.After(5 * time.Second):
		t.Fatal("a refused stream stayed open")
	}
	if fifth.status0() != http.StatusTooManyRequests || !strings.Contains(fifth.bodyString(), `"type":"rate_limited"`) {
		t.Fatalf("the 5th stream = %d %s, want 429 rate_limited", fifth.status0(), fifth.bodyString())
	}
	conns[0].close()
	if c := f.openEnvStream(t, f.cookie, ""); c.status0() != http.StatusOK {
		t.Errorf("a stream after one closed = %d, want 200", c.status0())
	}
}

func TestEnvStreamWithoutASessionIs401(t *testing.T) {
	f := newAgentFix(t)
	if c := f.openEnvStream(t, "", ""); c.status0() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", c.status0())
	}
}

func TestEnvStreamDeliversARefusedEnrolmentToAnAdminWatching(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Remote")
	c := f.openEnvStream(t, f.cookie, "")
	nextFrame(t, c) // the snapshot

	// The same environment's token with a wrong secret: its first character changed.
	i := len("bpjt_") + 36 + 1
	swap := "A"
	if env.Token[i:i+1] == "A" {
		swap = "B"
	}
	conn := f.srv.Dial(t, tunneltest.Agent{Token: env.Token[:i] + swap + env.Token[i+1:], Facts: &wire.Facts{EngineID: "ENG:x", Hostname: "intruder", EngineReachable: true}})
	if conn.Refuse == nil {
		t.Fatalf("the enrolment was not refused: %+v", conn)
	}
	fr, ev := nextOf(t, c, "agent.refused")
	if fr.id == "" || ev.EnvID != env.ID || ev.Code != "auth_rejected" || ev.Mode != "enrol" || ev.Node != "intruder" || ev.IP != "127.0.0.1" || ev.At == "" {
		t.Fatalf("agent.refused = %+v (id %q), want environment %s, auth_rejected, enrol, intruder, 127.0.0.1 and a time", ev, fr.id, env.ID)
	}
}

func TestEnvStreamDeliversAManagerFailoverToAnAdminWatching(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	c := f.openEnvStream(t, f.cookie, "")
	nextFrame(t, c) // the snapshot

	f.brk.Publish(envs.HealthEvent{Type: envs.EventFailover, EnvID: env.ID, Data: envs.FailoverData{
		FromAgentID: "a1", FromNode: "mgr-2", ToAgentID: "a2", ToNode: "mgr-1",
	}})
	fr, ev := nextOf(t, c, "failover")
	if fr.id == "" || ev.EnvID != env.ID || ev.FromAgentID != "a1" || ev.FromNode != "mgr-2" || ev.ToAgentID != "a2" || ev.ToNode != "mgr-1" || ev.At == "" {
		t.Fatalf("failover = %+v (id %q), want mgr-2 to mgr-1 for %s with a time", ev, fr.id, env.ID)
	}
}

func TestEnvStreamDeliversAProxyWarningToWhoeverHoldsSettingsReadAndToNobodyElse(t *testing.T) {
	f := newAgentFix(t)
	f.envStreamHeartbeat = 20 * time.Millisecond
	f.h = f.build(t)
	env := f.srv.NewEnvironment(t, "Remote")
	f.addUser(t, "viewer", false)
	f.grantRead(t, "viewer", env.ID)
	viewer, _ := f.signIn(t, "viewer", false)
	f.addUser(t, "nobody", false)
	nobody, _ := f.signIn(t, "nobody", false)

	admin := f.openEnvStream(t, f.cookie, "")
	reader := f.openEnvStream(t, viewer, "")
	none := f.openEnvStream(t, nobody, "")
	for _, c := range []*sseConn{admin, reader, none} {
		nextFrame(t, c) // the snapshot
	}

	f.brk.PublishProxyWarning("url_mismatch")
	// An event the viewer may see, after the warning, shows where the warning would have been.
	f.brk.Publish(refused(env.ID, "198.51.100.2"))

	fr, ev := nextOf(t, admin, "proxy.warning")
	if fr.id == "" || ev.EnvID != "" || ev.Kind != "url_mismatch" || ev.At == "" {
		t.Fatalf("proxy.warning = %+v (id %q), want kind url_mismatch, no environment and a time", ev, fr.id)
	}
	for name, c := range map[string]*sseConn{"an environments.read viewer": reader, "a user with no grant": none} {
		for {
			got, ok := c.next(150 * time.Millisecond)
			if !ok {
				break
			}
			if got.event == "proxy.warning" {
				t.Fatalf("%s received a proxy.warning: %+v", name, got)
			}
		}
	}
}
