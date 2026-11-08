// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package connect

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ianglennon/tierplan/agent/internal/selfmaint"
	"github.com/ianglennon/tierplan/proto/wire"
)

const (
	// pingTimeout bounds GET /_ping on the Docker socket.
	pingTimeout = 3 * time.Second
	// controlWriteTimeout bounds one write to the control stream, so a server
	// that stopped reading cannot stall the heartbeat loop forever.
	controlWriteTimeout = 10 * time.Second
	// maxPendingResults is how many update_result lines wait for the writer.
	maxPendingResults = 8
	// maxRunningRequests is how many update or uninstall requests run at once.
	// Self-maintenance runs one and answers the rest busy, so this only bounds the
	// goroutines a flood of requests can start.
	maxRunningRequests = 4
)

// runControl serves the control stream the agent opened: it writes the kind
// byte, then a heartbeat at once and one every interval, and reads the server's
// lines. It returns when ctx ends (nil) or the stream breaks. A line of a type
// the agent does not know, or that is not JSON, is ignored so that later plans
// can add server messages without breaking agents already in the field.
func runControl(ctx context.Context, st net.Conn, socketPath string, every time.Duration, now func() time.Time, log *slog.Logger, den *denials, hooks controlHooks) error {
	defer func() { _ = st.Close() }()
	if err := write(st, []byte{wire.StreamKindControl}); err != nil {
		return fmt.Errorf("open the control stream: %w", err)
	}

	// out carries the answers of self-maintenance, which runs off the reading
	// goroutine, to this loop, the stream's single writer. done tells a late
	// answer that the session is over.
	out := make(chan []byte, maxPendingResults)
	done := make(chan struct{})
	defer close(done)
	readErr := make(chan error, 1)
	go func() { readErr <- readControl(ctx, st, log, hooks, out, done) }()

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		hb := wire.Heartbeat{T: wire.TypeHeartbeat, EngineReachable: ping(ctx, socketPath), ClockMs: now().UnixMilli()}
		b, err := json.Marshal(hb)
		if err != nil {
			return err
		}
		if err := write(st, append(b, '\n')); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("send a heartbeat: %w", err)
		}
		if hooks.renew != nil && hooks.renew.poll() {
			if err := write(st, renewLine); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("ask for a new certificate: %w", err)
			}
		}
		if err := waitOrReport(ctx, st, t.C, readErr, den, out, log); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// controlHooks are what the control stream hands the server's messages to. A nil
// part means the agent ignores that kind of message.
type controlHooks struct {
	// renew schedules the agent's certificate renewal and takes the server's answer.
	renew *renewer
	// urls takes the addresses and pins the server pushed (D-19).
	urls func(wire.URLUpdate)
	// maint answers the server's update and uninstall messages (D-24, D-26). Nil
	// means the agent has no self-maintenance, and answers self_update_disabled. On a
	// session that was not verified it is an unverifiedMaint, whatever the agent has.
	maint selfmaint.Service
}

// unverifiedMaint answers every update and uninstall with insecure_session and does
// nothing else: it stands in for self-maintenance on a session whose server was not
// verified, because an update or an uninstall names an image the agent will run with
// the Docker socket, or removes the agent, and a spoofed server must not be able to
// ask for either (D-24, D-26; the same rule that stops such a server pushing addresses
// and pins). It makes no Docker call.
type unverifiedMaint struct {
	log    *slog.Logger
	warned atomic.Bool
}

func (m *unverifiedMaint) refuse(what string) wire.UpdateResult {
	if m.warned.CompareAndSwap(false, true) {
		m.log.Warn("the server asked the agent to " + what + " over a connection that was not verified; the agent refused. Remote update and uninstall need a TLS-verified session")
	}
	return wire.UpdateResult{Error: wire.UpdateErrInsecureSession}
}

func (m *unverifiedMaint) Update(context.Context, wire.Update) wire.UpdateResult {
	return m.refuse("update itself")
}

func (m *unverifiedMaint) Uninstall(context.Context, wire.Uninstall) wire.UpdateResult {
	return m.refuse("uninstall itself")
}

// renewLine is the renew message, ready to write.
var renewLine = []byte(`{"t":"` + wire.TypeRenew + `"}` + "\n")

// waitOrReport waits for the next heartbeat tick, and writes a denied message to
// the control stream for every refusal that arrives meanwhile. It returns nil when
// the tick comes or ctx ends, and an error when the stream breaks.
func waitOrReport(ctx context.Context, st net.Conn, tick <-chan time.Time, readErr <-chan error, den *denials, out <-chan []byte, log *slog.Logger) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case b := <-out:
			if err := write(st, b); err != nil {
				return fmt.Errorf("answer an update request: %w", err)
			}
		case m := <-den.ch:
			b, err := json.Marshal(m)
			if err != nil {
				return err
			}
			if err := write(st, append(b, '\n')); err != nil {
				return fmt.Errorf("report a refused request: %w", err)
			}
			if n := den.dropped.Swap(0); n > 0 {
				log.Warn("refused requests were not reported to the server: too many at once", "dropped", n)
			}
		case <-tick:
			return nil
		}
	}
}

func write(st net.Conn, b []byte) error {
	_ = st.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
	_, err := st.Write(b)
	return err
}

// readControl reads the server's lines until the stream ends. It always returns
// an error, because a control stream the server closed means the session is over.
func readControl(ctx context.Context, st net.Conn, log *slog.Logger, hooks controlHooks, out chan<- []byte, done <-chan struct{}) error {
	slots := make(chan struct{}, maxRunningRequests)
	sc := bufio.NewScanner(st)
	sc.Buffer(make([]byte, 0, 4096), wire.MaxControlLine)
	for sc.Scan() {
		var head struct {
			T string `json:"t"`
		}
		if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
			continue
		}
		switch head.T {
		case wire.TypeRenewed, wire.TypeRenewRefused:
			if hooks.renew != nil {
				hooks.renew.handle(head.T, sc.Bytes())
			}
		case wire.TypeURLs:
			var u wire.URLUpdate
			if hooks.urls != nil && json.Unmarshal(sc.Bytes(), &u) == nil {
				hooks.urls(u)
			}
		case wire.TypeUpdate:
			var m wire.Update
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				log.Debug("ignored an update message that could not be read")
				continue
			}
			runMaint(ctx, slots, out, done, m.ID, func(ctx context.Context) wire.UpdateResult {
				if hooks.maint == nil {
					return wire.UpdateResult{Error: wire.UpdateErrSelfUpdateDisabled}
				}
				return hooks.maint.Update(ctx, m)
			})
		case wire.TypeUninstall:
			var m wire.Uninstall
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				log.Debug("ignored an uninstall message that could not be read")
				continue
			}
			runMaint(ctx, slots, out, done, m.ID, func(ctx context.Context) wire.UpdateResult {
				if hooks.maint == nil {
					return wire.UpdateResult{Error: wire.UpdateErrSelfUpdateDisabled}
				}
				return hooks.maint.Uninstall(ctx, m)
			})
		default:
			log.Debug("ignored a control message of an unknown type", "type", head.T)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read the control stream: %w", err)
	}
	return errors.New("the server closed the control stream")
}

// runMaint runs one update or uninstall off the reading goroutine, because a pull
// or a helper start takes minutes and the control stream must keep answering, and
// queues its update_result for the writer. The result always carries the request's
// id, capped, and the message type. A session that ended meanwhile drops the result;
// the server then sees the agent reconnect, or not. At most maxRunningRequests run
// at once, and a request past that is answered busy without running.
func runMaint(ctx context.Context, slots chan struct{}, out chan<- []byte, done <-chan struct{}, id string, run func(context.Context) wire.UpdateResult) {
	send := func(res wire.UpdateResult) {
		res.T, res.ID = wire.TypeUpdateResult, wire.CapID(id)
		b, err := json.Marshal(res)
		if err != nil {
			return
		}
		select {
		case out <- append(b, '\n'):
		case <-done:
		}
	}
	select {
	case slots <- struct{}{}:
	default:
		go send(wire.UpdateResult{Error: wire.UpdateErrBusy})
		return
	}
	go func() {
		defer func() { <-slots }()
		send(run(ctx))
	}()
}

// ping asks the Docker engine, through its socket, whether it is alive. It is
// the engineReachable of a heartbeat.
func ping(ctx context.Context, socketPath string) bool {
	hc := &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		}},
		Timeout: pingTimeout,
	}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// denials carries refusals from the proxy, which runs on a stream per request,
// to the control stream, which has a single writer.
type denials struct {
	ch      chan wire.Denied
	dropped atomic.Int64
}

// maxDeniedMethod and maxDeniedPath cap what the server sent before it is sent
// back: both come from the server's own request.
const (
	maxDeniedMethod = 16
	maxDeniedPath   = 256
)

func newDenials() *denials { return &denials{ch: make(chan wire.Denied, 64)} }

// report queues a refusal. It never blocks the request that was refused: when 64
// are waiting the message is dropped and counted.
func (d *denials) report(method, path, reason string) {
	m := wire.Denied{T: wire.TypeDenied, Method: capString(method, maxDeniedMethod), Path: capString(path, maxDeniedPath), Reason: reason}
	select {
	case d.ch <- m:
	default:
		d.dropped.Add(1)
	}
}

// capString cuts s to at most n bytes on a character boundary.
func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
