// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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

	"github.com/ianglennon/bayplan/proto/wire"
)

const (
	// pingTimeout bounds GET /_ping on the Docker socket.
	pingTimeout = 3 * time.Second
	// controlWriteTimeout bounds one write to the control stream, so a server
	// that stopped reading cannot stall the heartbeat loop forever.
	controlWriteTimeout = 10 * time.Second
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

	readErr := make(chan error, 1)
	go func() { readErr <- readControl(st, log, hooks) }()

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
		if err := waitOrReport(ctx, st, t.C, readErr, den, log); err != nil {
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
}

// renewLine is the renew message, ready to write.
var renewLine = []byte(`{"t":"` + wire.TypeRenew + `"}` + "\n")

// waitOrReport waits for the next heartbeat tick, and writes a denied message to
// the control stream for every refusal that arrives meanwhile. It returns nil when
// the tick comes or ctx ends, and an error when the stream breaks.
func waitOrReport(ctx context.Context, st net.Conn, tick <-chan time.Time, readErr <-chan error, den *denials, log *slog.Logger) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
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
func readControl(st net.Conn, log *slog.Logger, hooks controlHooks) error {
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
		default:
			log.Debug("ignored a control message of an unknown type", "type", head.T)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read the control stream: %w", err)
	}
	return errors.New("the server closed the control stream")
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
