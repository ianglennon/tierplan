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
func runControl(ctx context.Context, st net.Conn, socketPath string, every time.Duration, now func() time.Time, log *slog.Logger) error {
	defer func() { _ = st.Close() }()
	if err := write(st, []byte{wire.StreamKindControl}); err != nil {
		return fmt.Errorf("open the control stream: %w", err)
	}

	readErr := make(chan error, 1)
	go func() { readErr <- readControl(st, log) }()

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
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case <-t.C:
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
func readControl(st net.Conn, log *slog.Logger) error {
	sc := bufio.NewScanner(st)
	sc.Buffer(make([]byte, 0, 4096), wire.MaxControlLine)
	for sc.Scan() {
		var head struct {
			T string `json:"t"`
		}
		if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
			continue
		}
		// No server-to-agent message exists yet.
		log.Debug("ignored a control message of an unknown type", "type", head.T)
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
	m := wire.Denied{T: wire.TypeDenied, Method: method, Path: path, Reason: reason}
	select {
	case d.ch <- m:
	default:
		d.dropped.Add(1)
	}
}
