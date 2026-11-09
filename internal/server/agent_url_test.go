// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
)

// The tunnel judges every agent connection against the address agents are told
// to dial: the agent URL when one is set, else the public URL (D-18). No plain
// connection is admitted whatever the agent URL says: a plain request is refused
// with an https public URL, with a plain agent URL on this machine and with an
// https agent URL (TestOnlyTLSAgentConnectionsAreAdmitted covers the TLS side).
func TestTunnelFollowsTheAgentURLNotThePublicURL(t *testing.T) {
	upgrade := func(srv *Server) int {
		req := httptest.NewRequest(http.MethodGet, "/agent/connect", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		req.RemoteAddr = "127.0.0.1:40000"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	build := func(env map[string]string) *Server {
		t.Helper()
		env["TIERPLAN_DATA_DIR"] = t.TempDir() + "/data"
		srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Close() })
		return srv
	}

	// https public URL, no agent URL: a plain connection is refused as insecure.
	if code := upgrade(build(map[string]string{"TIERPLAN_PUBLIC_URL": "https://tierplan.test"})); code != http.StatusForbidden {
		t.Fatalf("a plain connection with an https public URL = %d, want 403", code)
	}
	// An https agent URL on another host does not make plain transport
	// acceptable either: no plain connection is admitted, whatever the agent URL.
	if code := upgrade(build(map[string]string{
		"TIERPLAN_PUBLIC_URL": "https://tierplan.test",
		"TIERPLAN_AGENT_URL":  "https://10.0.0.5:7480",
	})); code != http.StatusForbidden {
		t.Fatalf("a plain connection with an https agent URL on another host = %d, want 403", code)
	}
	// An https agent URL is where agents dial, so a plain connection is refused
	// although the public URL is another https address on this machine.
	if code := upgrade(build(map[string]string{
		"TIERPLAN_PUBLIC_URL": "https://127.0.0.1:7480",
		"TIERPLAN_AGENT_URL":  "https://tierplan.test",
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
		env["TIERPLAN_DATA_DIR"] = t.TempDir() + "/data"
		srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Close() })
		return srv
	}
	if got := build(map[string]string{"TIERPLAN_PUBLIC_URL": "https://tierplan.test/tp"}).tunnel.URLs(); len(got) != 1 || got[0] != "wss://tierplan.test/tp/agent/connect" {
		t.Errorf("public URL only: %v", got)
	}
	if got := build(map[string]string{"TIERPLAN_PUBLIC_URL": "https://tierplan.test", "TIERPLAN_AGENT_URL": "https://10.0.0.5:7480"}).tunnel.URLs(); len(got) != 1 || got[0] != "wss://10.0.0.5:7480/agent/connect" {
		t.Errorf("agent URL set: %v", got)
	}
	if got := build(map[string]string{}).tunnel.URLs(); len(got) != 0 {
		t.Errorf("no URL set: %v, want no address to hand out", got)
	}
}

// TierPlan admits an agent only on a TLS connection, whatever address agents are
// told to dial (D-04, D-09). Over the real TLS listener the upgrade is admitted
// for every agent address, and the refusal of a plain request is asserted through
// the handler, so no test here depends on the listener serving plain connections.
func TestOnlyTLSAgentConnectionsAreAdmitted(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{name: "https public URL", env: map[string]string{"TIERPLAN_PUBLIC_URL": "https://tierplan.test"}},
		{name: "https agent address", env: map[string]string{"TIERPLAN_PUBLIC_URL": "https://tierplan.test", "TIERPLAN_AGENT_URL": "https://10.0.0.5:7480"}},
		{name: "https agent address on this machine", env: map[string]string{"TIERPLAN_PUBLIC_URL": "https://tierplan.test", "TIERPLAN_AGENT_URL": "https://127.0.0.1:7480"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				"TIERPLAN_DATA_DIR": t.TempDir() + "/data",
				"TIERPLAN_LISTEN":   "127.0.0.1:0",
			}
			for k, v := range tc.env {
				env[k] = v
			}
			cfg := testConfig(t, env)
			lines := listeningLines{addrs: make(chan string, 1)}
			srv, err := Build(context.Background(), cfg, slog.New(slog.NewJSONHandler(lines, nil)))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(testsupport.Context(t))
			done := make(chan error, 1)
			go func() { done <- srv.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				<-done
			})
			base := testsupport.Await(t, "the listening log line", lines.addrs)

			// wss over the server's TLS listener, trusting the mirror: admitted.
			mirror, err := os.ReadFile(HealthcheckCertFile(cfg)) //nolint:gosec // the mirror under the test's own data directory
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(mirror) {
				t.Fatal("the mirror holds no certificate")
			}
			client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}}}
			t.Cleanup(client.CloseIdleConnections)
			dctx, dcancel := context.WithTimeout(ctx, testsupport.Deadline)
			defer dcancel()
			c, resp, err := websocket.Dial(dctx, "wss"+strings.TrimPrefix(base, "https")+"/agent/connect", &websocket.DialOptions{HTTPClient: client, Subprotocols: []string{wire.Subprotocol}}) //nolint:bodyclose // a WebSocket upgrade response has no body to close
			if err != nil {
				code := 0
				if resp != nil {
					code = resp.StatusCode
				}
				t.Fatalf("wss dial = %v (status %d), want the upgrade admitted", err, code)
			}
			defer func() { _ = c.CloseNow() }()
			if got := c.Subprotocol(); got != wire.Subprotocol {
				t.Fatalf("subprotocol = %q, want %q", got, wire.Subprotocol)
			}

			// The same request without TLS, through the handler: refused.
			req := httptest.NewRequest(http.MethodGet, "/agent/connect", nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			req.Header.Set("Sec-WebSocket-Protocol", wire.Subprotocol)
			req.RemoteAddr = "127.0.0.1:40000"
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden || !strings.HasPrefix(rec.Body.String(), wire.RefuseInsecureTransport) {
				t.Fatalf("plain upgrade = %d %q, want 403 starting %s", rec.Code, rec.Body.String(), wire.RefuseInsecureTransport)
			}
		})
	}
}
