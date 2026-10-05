// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/bayplan/proto/wire"
)

// The tunnel judges every agent connection against the address agents are told
// to dial: the agent URL when one is set, else the public URL (D-18). The agent
// URL, not the public URL, decides, so the same request is admitted or refused
// depending only on it: an https public URL with a plain agent URL on this
// machine admits plain transport, an http public URL on this machine with an
// https agent URL refuses it (a plain agent URL for another host is the subject of
// TestAPlainAgentURLForAnotherHostIsRefusedUnlessTheSwitchIsOn).
func TestTunnelFollowsTheAgentURLNotThePublicURL(t *testing.T) {
	upgrade := func(srv *Server) int {
		req := httptest.NewRequest(http.MethodGet, "/agent/connect", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		req.RemoteAddr = "203.0.113.7:40000"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	build := func(env map[string]string) *Server {
		t.Helper()
		env["BAYPLAN_DATA_DIR"] = t.TempDir() + "/data"
		srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Close() })
		return srv
	}

	// https public URL, no agent URL: a plain connection is refused as insecure.
	if code := upgrade(build(map[string]string{"BAYPLAN_PUBLIC_URL": "https://bayplan.test"})); code != http.StatusForbidden {
		t.Fatalf("a plain connection with an https public URL = %d, want 403", code)
	}
	// A plain agent URL on this machine is where agents dial, so plain transport
	// is acceptable there and the request gets past the preflight to the
	// WebSocket accept, although the public URL is https.
	if code := upgrade(build(map[string]string{
		"BAYPLAN_PUBLIC_URL": "https://bayplan.test",
		"BAYPLAN_AGENT_URL":  "http://127.0.0.1:7480",
	})); code == http.StatusForbidden || code == http.StatusServiceUnavailable {
		t.Fatalf("a plain connection with an http agent URL on this machine = %d, want it past the preflight", code)
	}
	// An https agent URL is where agents dial, so a plain connection is refused
	// although the public URL is plain http on this machine.
	if code := upgrade(build(map[string]string{
		"BAYPLAN_PUBLIC_URL": "http://127.0.0.1:7480",
		"BAYPLAN_AGENT_URL":  "https://bayplan.test",
	})); code != http.StatusForbidden {
		t.Fatalf("a plain connection with an https agent URL = %d, want 403", code)
	}
}

// A welcome, and a push when an address changes, carry the endpoint agents are told
// to dial: the agent URL when one is set, else the public URL, with the base path
// (D-18, D-19).
func TestTheHubTellsAgentsTheEndpointFromTheAgentURLElseThePublicURL(t *testing.T) {
	build := func(env map[string]string) *Server {
		t.Helper()
		env["BAYPLAN_DATA_DIR"] = t.TempDir() + "/data"
		srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Close() })
		return srv
	}
	if got := build(map[string]string{"BAYPLAN_PUBLIC_URL": "https://bayplan.test/bp"}).tunnel.URLs(); len(got) != 1 || got[0] != "wss://bayplan.test/bp/agent/connect" {
		t.Errorf("public URL only: %v", got)
	}
	if got := build(map[string]string{"BAYPLAN_PUBLIC_URL": "https://bayplan.test", "BAYPLAN_AGENT_URL": "https://10.0.0.5:7480"}).tunnel.URLs(); len(got) != 1 || got[0] != "wss://10.0.0.5:7480/agent/connect" {
		t.Errorf("agent URL set: %v", got)
	}
	if got := build(map[string]string{}).tunnel.URLs(); len(got) != 0 {
		t.Errorf("no URL set: %v, want no address to hand out", got)
	}
}

// Bayplan accepts plain ws:// for an agent address that is not this machine only
// while its own development switch is on, the rule the agent applies to itself
// since 02-41. Loopback is exempt on both ends, and an http public URL stays
// legal for the UI: the refusal is keyed on the address agents are told to dial
// (the agent URL, else the public URL), not on the scheme of the public URL
// (G-02-R4-3, review WR-02).
func TestAPlainAgentURLForAnotherHostIsRefusedUnlessTheSwitchIsOn(t *testing.T) {
	cases := []struct {
		name string
		// env is added to the data dir and a public URL on the listener's address.
		env      map[string]string
		admitted bool
	}{
		{name: "plain to another host, switch off", env: map[string]string{"BAYPLAN_AGENT_URL": "http://10.0.0.5:7480"}},
		{name: "plain to another host, switch on", env: map[string]string{"BAYPLAN_AGENT_URL": "http://10.0.0.5:7480", "BAYPLAN_AGENT_ALLOW_INSECURE": "true"}, admitted: true},
		{name: "plain on this machine by address", env: map[string]string{}, admitted: true},
		{name: "plain on this machine by name", env: map[string]string{"BAYPLAN_AGENT_URL": "http://localhost:7480"}, admitted: true},
		{name: "https agent address, switch off", env: map[string]string{"BAYPLAN_AGENT_URL": "https://bayplan.test"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Skipf("cannot listen on loopback: %v", err)
			}
			ts := httptest.NewUnstartedServer(nil)
			_ = ts.Listener.Close()
			ts.Listener = ln
			addr := ln.Addr().String()
			env := map[string]string{
				"BAYPLAN_DATA_DIR":   t.TempDir() + "/data",
				"BAYPLAN_PUBLIC_URL": "http://" + addr,
			}
			for k, v := range tc.env {
				env[k] = v
			}
			srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			ts.Config.Handler = srv.Handler()
			ts.Start()
			t.Cleanup(func() {
				ts.Close()
				_ = srv.Close()
			})

			if tc.admitted {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				c, resp, err := websocket.Dial(ctx, "ws://"+addr+"/agent/connect", &websocket.DialOptions{Subprotocols: []string{wire.Subprotocol}})
				if err != nil {
					code := 0
					if resp != nil {
						code = resp.StatusCode
					}
					t.Fatalf("dial = %v (status %d), want the upgrade admitted", err, code)
				}
				defer func() { _ = c.CloseNow() }()
				if got := c.Subprotocol(); got != wire.Subprotocol {
					t.Fatalf("subprotocol = %q, want %q", got, wire.Subprotocol)
				}
				return
			}

			req, err := http.NewRequest(http.MethodGet, ts.URL+"/agent/connect", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			req.Header.Set("Sec-WebSocket-Protocol", wire.Subprotocol)
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusForbidden || !strings.HasPrefix(string(body), wire.RefuseInsecureTransport) {
				t.Fatalf("plain upgrade = %d %q, want 403 starting %s", resp.StatusCode, body, wire.RefuseInsecureTransport)
			}
		})
	}
}
