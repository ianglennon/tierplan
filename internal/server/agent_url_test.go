// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The tunnel judges every agent connection against the address agents are told
// to dial: the agent URL when one is set, else the public URL (D-18). Plain
// transport is accepted only when that address is itself http, so an http agent
// URL on an https instance is the case that tells the two apart.
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
	// An http agent URL is where agents dial, so plain transport is acceptable
	// there and the request gets past the preflight to the WebSocket accept.
	if code := upgrade(build(map[string]string{
		"BAYPLAN_PUBLIC_URL": "https://bayplan.test",
		"BAYPLAN_AGENT_URL":  "http://10.0.0.5:7480",
	})); code == http.StatusForbidden || code == http.StatusServiceUnavailable {
		t.Fatalf("a plain connection with an http agent URL = %d, want it past the preflight", code)
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
