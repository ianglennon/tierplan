// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel_test

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// updateRecorder is a tunnel.UpdateObserver that keeps what it is told.
type updateRecorder struct {
	mu      sync.Mutex
	opened  []string
	results []wire.UpdateResult
	changed chan struct{}
}

func newUpdateRecorder() *updateRecorder { return &updateRecorder{changed: make(chan struct{}, 16)} }

func (r *updateRecorder) ControlOpened(s *tunnel.Session) {
	r.mu.Lock()
	r.opened = append(r.opened, s.AgentID)
	r.mu.Unlock()
	r.changed <- struct{}{}
}

func (r *updateRecorder) UpdateResult(_ *tunnel.Session, res wire.UpdateResult) {
	r.mu.Lock()
	r.results = append(r.results, res)
	r.mu.Unlock()
	r.changed <- struct{}{}
}

func (r *updateRecorder) wait(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		r.mu.Lock()
		done := ok()
		r.mu.Unlock()
		if done {
			return
		}
		select {
		case <-r.changed:
		case <-deadline:
			t.Fatal("timed out waiting for the update observer")
		}
	}
}

func withUpdates(t *testing.T, rec *updateRecorder) *tunneltest.Server {
	t.Helper()
	return tunneltest.Start(t, tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub {
		return h.WithTiming(10*time.Second, 1000*day).WithUpdateObserver(rec)
	}})
}

func TestSendUpdateWritesTheFrozenUpdateLineToTheAgentsControlStream(t *testing.T) {
	rec := newUpdateRecorder()
	srv := withUpdates(t, rec)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	lines := readControlLines(t, c.OpenControl(t))
	rec.wait(t, func() bool { return len(rec.opened) == 1 })

	err := srv.Hub.SendUpdate(c.Identity.AgentID, wire.Update{T: wire.TypeUpdate, ID: "u1", Image: "ghcr.io/ianglennon/bayplan-agent:0.2.1"})
	if err != nil {
		t.Fatalf("SendUpdate: %v", err)
	}
	got := string(lines.next())
	want := `{"t":"update","id":"u1","image":"ghcr.io/ianglennon/bayplan-agent:0.2.1"}`
	if got != want {
		t.Fatalf("the agent was sent %s, want %s", got, want)
	}
}

func TestSendUpdateFillsInTheMessageTypeSoACallerCannotSendAnotherKind(t *testing.T) {
	rec := newUpdateRecorder()
	srv := withUpdates(t, rec)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	lines := readControlLines(t, c.OpenControl(t))
	rec.wait(t, func() bool { return len(rec.opened) == 1 })

	if err := srv.Hub.SendUpdate(c.Identity.AgentID, wire.Update{T: "uninstall", Image: "x"}); err != nil {
		t.Fatalf("SendUpdate: %v", err)
	}
	var head struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(lines.next(), &head); err != nil || head.T != wire.TypeUpdate {
		t.Fatalf("type = %q, %v; SendUpdate must always send an update", head.T, err)
	}
}

func TestSendUpdateToAnAgentThatIsNotConnectedOrHasNoControlStreamIsErrAgentGone(t *testing.T) {
	rec := newUpdateRecorder()
	srv := withUpdates(t, rec)
	if err := srv.Hub.SendUpdate("no-such-agent", wire.Update{Image: "x"}); !errors.Is(err, tunnel.ErrAgentGone) {
		t.Fatalf("an unknown agent: err = %v, want ErrAgentGone", err)
	}
	c := srv.Enrol(t, srv.NewEnvironment(t, "NoControl"))
	srv.Events.WaitLen(t, 1)
	if err := srv.Hub.SendUpdate(c.Identity.AgentID, wire.Update{Image: "x"}); !errors.Is(err, tunnel.ErrAgentGone) {
		t.Fatalf("an agent with no control stream: err = %v, want ErrAgentGone", err)
	}
}

func TestAnUpdateResultOnTheControlStreamReachesTheObserverWithAnEchoedIDAndAClippedError(t *testing.T) {
	rec := newUpdateRecorder()
	srv := withUpdates(t, rec)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	control := c.OpenControl(t)
	rec.wait(t, func() bool { return len(rec.opened) == 1 })

	long := strings.Repeat("e", 500)
	for _, line := range []string{
		`{"t":"update_result","id":"u1","ok":true,"error":""}`,
		`{"t":"update_result","id":"u2","ok":false,"error":"` + long + `"}`,
		`{"t":"update_result","id":"` + strings.Repeat("i", 300) + `","ok":false,"error":"busy"}`,
		`{"t":"update_result","id":`, // not JSON: ignored
	} {
		if _, err := io.WriteString(control, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	rec.wait(t, func() bool { return len(rec.results) == 3 })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if r := rec.results[0]; r.ID != "u1" || !r.OK || r.Error != "" {
		t.Errorf("first result = %+v", r)
	}
	if r := rec.results[1]; r.ID != "u2" || r.OK || len(r.Error) != 64 {
		t.Errorf("an error from an agent is not trusted, so it is cut to 64 bytes: got %d bytes (%+v)", len(r.Error), r)
	}
	if r := rec.results[2]; len(r.ID) != wire.MaxMessageIDLen {
		t.Errorf("an id is cut to %d bytes, got %d", wire.MaxMessageIDLen, len(r.ID))
	}
}

func TestAnUpdateResultWithNoObserverIsIgnored(t *testing.T) {
	srv := quiet(t)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	control := c.OpenControl(t)
	if _, err := io.WriteString(control, `{"t":"update_result","id":"x","ok":true,"error":""}`+"\n"); err != nil {
		t.Fatal(err)
	}
	staysAfterAHeartbeat(t, srv.Hub.Session(c.Identity.AgentID), control, 904, "an update_result nobody asked for does not end the session")
}

func TestSendUninstallWritesTheFrozenUninstallLineAndAlwaysSendsThatType(t *testing.T) {
	rec := newUpdateRecorder()
	srv := withUpdates(t, rec)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	lines := readControlLines(t, c.OpenControl(t))
	rec.wait(t, func() bool { return len(rec.opened) == 1 })

	if err := srv.Hub.SendUninstall(c.Identity.AgentID, wire.Uninstall{T: "update", ID: "x1", RemoveData: true}); err != nil {
		t.Fatalf("SendUninstall: %v", err)
	}
	if got, want := string(lines.next()), `{"t":"uninstall","id":"x1","removeData":true}`; got != want {
		t.Fatalf("the agent was sent %s, want %s (the type is always uninstall, and service is absent unless set)", got, want)
	}
	if err := srv.Hub.SendUninstall(c.Identity.AgentID, wire.Uninstall{ID: "x2", RemoveData: true, Service: true}); err != nil {
		t.Fatalf("SendUninstall: %v", err)
	}
	if got, want := string(lines.next()), `{"t":"uninstall","id":"x2","removeData":true,"service":true}`; got != want {
		t.Fatalf("the agent was sent %s, want %s", got, want)
	}
}

func TestSendUninstallToAnAgentThatIsNotConnectedIsErrAgentGone(t *testing.T) {
	srv := withUpdates(t, newUpdateRecorder())
	if err := srv.Hub.SendUninstall("no-such-agent", wire.Uninstall{}); !errors.Is(err, tunnel.ErrAgentGone) {
		t.Fatalf("err = %v, want ErrAgentGone", err)
	}
}
