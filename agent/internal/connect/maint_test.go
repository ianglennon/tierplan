// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package connect

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/proto/wire"
)

// stubMaint is a selfmaint.Service that records what it was asked and answers as
// the test says. block, when set, holds every call until it is closed.
type stubMaint struct {
	mu        sync.Mutex
	updates   []wire.Update
	uninstall []wire.Uninstall
	block     chan struct{}
	answer    func(id string) wire.UpdateResult
}

func (s *stubMaint) reply(id string) wire.UpdateResult {
	if s.answer != nil {
		return s.answer(id)
	}
	return wire.UpdateResult{T: wire.TypeUpdateResult, ID: id, OK: true}
}

func (s *stubMaint) Update(_ context.Context, m wire.Update) wire.UpdateResult {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	s.updates = append(s.updates, m)
	s.mu.Unlock()
	return s.reply(m.ID)
}

func (s *stubMaint) Uninstall(_ context.Context, m wire.Uninstall) wire.UpdateResult {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	s.uninstall = append(s.uninstall, m)
	s.mu.Unlock()
	return s.reply(m.ID)
}

func (s *stubMaint) gotUpdates() []wire.Update {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]wire.Update(nil), s.updates...)
}

// maintRig runs runControl on one end of a pipe and exposes the server end: every
// line the agent writes arrives on lines, parsed to its raw bytes.
type maintRig struct {
	t      *testing.T
	server net.Conn
	lines  chan []byte
}

func newMaintRig(t *testing.T, svc *stubMaint) *maintRig {
	t.Helper()
	agentEnd, serverEnd := net.Pipe()
	r := &maintRig{t: t, server: serverEnd, lines: make(chan []byte, 1024)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	hooks := controlHooks{}
	if svc != nil {
		hooks.maint = svc
	}
	t.Cleanup(func() {
		cancel()
		_ = serverEnd.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("runControl did not stop")
		}
	})
	go func() {
		done <- runControl(ctx, agentEnd, filepath.Join(t.TempDir(), "none.sock"), 5*time.Millisecond, time.Now,
			slog.New(slog.DiscardHandler), newDenials(), hooks)
	}()
	go func() {
		br := bufio.NewReader(serverEnd)
		if _, err := br.ReadByte(); err != nil {
			return
		}
		sc := bufio.NewScanner(br)
		for sc.Scan() {
			r.lines <- append([]byte(nil), sc.Bytes()...)
		}
	}()
	return r
}

func (r *maintRig) send(line string) {
	r.t.Helper()
	_ = r.server.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := r.server.Write([]byte(line + "\n")); err != nil {
		r.t.Fatalf("write to the agent: %v", err)
	}
}

// result waits for the next update_result line.
func (r *maintRig) result() wire.UpdateResult {
	r.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case l := <-r.lines:
			var h struct {
				T string `json:"t"`
			}
			if json.Unmarshal(l, &h) == nil && h.T == wire.TypeUpdateResult {
				var res wire.UpdateResult
				if err := json.Unmarshal(l, &res); err != nil {
					r.t.Fatalf("bad update_result %s: %v", l, err)
				}
				return res
			}
		case <-deadline:
			r.t.Fatal("no update_result arrived")
			return wire.UpdateResult{}
		}
	}
}

// heartbeats drains the lines and counts the heartbeats among them.
func (r *maintRig) heartbeats() int {
	n := 0
	for {
		select {
		case l := <-r.lines:
			var h struct {
				T string `json:"t"`
			}
			if json.Unmarshal(l, &h) == nil && h.T == wire.TypeHeartbeat {
				n++
			}
		default:
			return n
		}
	}
}

func TestAnUpdateMessageReachesSelfMaintenanceAndItsResultGoesBackWithTheID(t *testing.T) {
	svc := &stubMaint{}
	r := newMaintRig(t, svc)
	r.send(`{"t":"update","id":"req-1","image":"ghcr.io/ianglennon/bayplan-agent:0.2.1","futureField":[1]}`)
	res := r.result()
	if !res.OK || res.ID != "req-1" || res.T != wire.TypeUpdateResult {
		t.Fatalf("result = %+v", res)
	}
	got := svc.gotUpdates()
	if len(got) != 1 || got[0].Image != "ghcr.io/ianglennon/bayplan-agent:0.2.1" || got[0].ID != "req-1" {
		t.Fatalf("self-maintenance got %+v", got)
	}
}

func TestAnUninstallMessageReachesSelfMaintenanceAndItsResultGoesBack(t *testing.T) {
	svc := &stubMaint{answer: func(id string) wire.UpdateResult {
		return wire.UpdateResult{T: wire.TypeUpdateResult, ID: id, Error: wire.UpdateErrSelfUpdateDisabled}
	}}
	r := newMaintRig(t, svc)
	r.send(`{"t":"uninstall","id":"u-1","removeData":true,"service":true}`)
	res := r.result()
	if res.OK || res.ID != "u-1" || res.Error != wire.UpdateErrSelfUpdateDisabled {
		t.Fatalf("result = %+v", res)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if len(svc.uninstall) != 1 || !svc.uninstall[0].RemoveData || !svc.uninstall[0].Service {
		t.Fatalf("self-maintenance got %+v", svc.uninstall)
	}
}

func TestASlowUpdateDoesNotStopHeartbeatsAndItsResultComesAfterwards(t *testing.T) {
	svc := &stubMaint{block: make(chan struct{})}
	r := newMaintRig(t, svc)
	r.send(`{"t":"update","id":"slow","image":"x/y:1"}`)
	time.Sleep(60 * time.Millisecond)
	if n := r.heartbeats(); n < 3 {
		t.Fatalf("only %d heartbeats while an update was running, want the stream to stay alive", n)
	}
	close(svc.block)
	if res := r.result(); res.ID != "slow" || !res.OK {
		t.Fatalf("result = %+v", res)
	}
}

func TestAnAgentWithoutSelfMaintenanceAnswersSelfUpdateDisabled(t *testing.T) {
	r := newMaintRig(t, nil)
	r.send(`{"t":"update","id":"a","image":"x/y:1"}`)
	if res := r.result(); res.OK || res.ID != "a" || res.Error != wire.UpdateErrSelfUpdateDisabled {
		t.Fatalf("update result = %+v", res)
	}
	r.send(`{"t":"uninstall","id":"b"}`)
	if res := r.result(); res.OK || res.ID != "b" || res.Error != wire.UpdateErrSelfUpdateDisabled {
		t.Fatalf("uninstall result = %+v", res)
	}
}

func TestAMalformedUpdateLineIsIgnoredAndTheNextOneStillWorks(t *testing.T) {
	svc := &stubMaint{}
	r := newMaintRig(t, svc)
	r.send(`{"t":"update","image":42}`) // image has the wrong type
	r.send(`{"t":"update","id":"ok","image":"x/y:1"}`)
	if res := r.result(); res.ID != "ok" {
		t.Fatalf("result = %+v", res)
	}
	if n := len(svc.gotUpdates()); n != 1 {
		t.Fatalf("%d updates ran, want only the well-formed one", n)
	}
}
