// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAddingAnAgentEnvironmentNeedsAnAddressAgentsCanUse is the "never advertise"
// half of G-02-R4-3 (review WR-02), end to end through a real server and real
// HTTP. Coaming hands out only an address its agents will dial. A plain address
// for another host is refused (409 insecure_agent_url, nothing created) unless the
// server's development switch COAMING_AGENT_ALLOW_INSECURE is on, and then the
// install carries the agent's own switch (plan 02-42). A plain address on this
// machine needs no switch, and an https address never carries one.
func TestAddingAnAgentEnvironmentNeedsAnAddressAgentsCanUse(t *testing.T) {
	type install struct {
		AgentURL string `json:"agentUrl"`
		Swarm    struct {
			Command string `json:"command"`
		} `json:"swarm"`
		Standalone struct {
			Command string `json:"command"`
		} `json:"standalone"`
	}
	const insecure = "COAMING_AGENT_ALLOW_INSECURE"

	cases := []struct {
		name string
		// env is extra server environment; %ADDR% in a value is the listener's address.
		env map[string]string
		// wantCode is the status of POST /api/environments {"kind":"agent"}.
		wantCode int
		// wantAgentURL is the install's agentUrl for a 201, %ADDR% as above.
		wantAgentURL string
		// wantSwitch says whether the install commands carry the agent's switch.
		wantSwitch bool
	}{
		{
			name:     "plain to another host, switch off",
			env:      map[string]string{"COAMING_AGENT_URL": "http://10.0.0.5:7480"},
			wantCode: http.StatusConflict,
		},
		{
			name:         "plain to another host, switch on",
			env:          map[string]string{"COAMING_AGENT_URL": "http://10.0.0.5:7480", insecure: "true"},
			wantCode:     http.StatusCreated,
			wantAgentURL: "ws://10.0.0.5:7480/agent/connect",
			wantSwitch:   true,
		},
		{
			name:         "plain on this machine",
			env:          map[string]string{},
			wantCode:     http.StatusCreated,
			wantAgentURL: "ws://%ADDR%/agent/connect",
		},
		{
			name:         "https",
			env:          map[string]string{"COAMING_AGENT_URL": "https://coaming.test"},
			wantCode:     http.StatusCreated,
			wantAgentURL: "wss://coaming.test/agent/connect",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The listener, so the public URL can name its own address before Build.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Skipf("cannot listen on 127.0.0.1: %v", err)
			}
			ts := httptest.NewUnstartedServer(nil)
			_ = ts.Listener.Close()
			ts.Listener = ln
			addr := ln.Addr().String()

			env := map[string]string{
				"COAMING_DATA_DIR":   t.TempDir() + "/data",
				"COAMING_PUBLIC_URL": "http://" + addr,
			}
			for k, v := range tc.env {
				env[k] = strings.ReplaceAll(v, "%ADDR%", addr)
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

			cookie, csrf := signInAdmin(t, srv, ts)
			call := func(method, path, body string) (int, []byte) {
				t.Helper()
				req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				if csrf != "" {
					req.Header.Set("X-CSRF-Token", csrf)
				}
				resp, err := ts.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				b, _ := io.ReadAll(resp.Body)
				return resp.StatusCode, b
			}
			listed := func() int {
				t.Helper()
				code, body := call(http.MethodGet, "/api/environments", "")
				if code != http.StatusOK {
					t.Fatalf("GET /api/environments = %d %s", code, body)
				}
				var got struct {
					Items []json.RawMessage `json:"items"`
				}
				if err := json.Unmarshal(body, &got); err != nil {
					// A bare array is accepted too.
					var arr []json.RawMessage
					if err2 := json.Unmarshal(body, &arr); err2 != nil {
						t.Fatalf("decode the environment list: %v\n%s", err, body)
					}
					return len(arr)
				}
				return len(got.Items)
			}

			before := listed()
			code, body := call(http.MethodPost, "/api/environments", `{"kind":"agent"}`)
			if code != tc.wantCode {
				t.Fatalf("POST /api/environments kind agent = %d %s, want %d", code, body, tc.wantCode)
			}

			if tc.wantCode == http.StatusConflict {
				var p struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(body, &p); err != nil {
					t.Fatal(err)
				}
				if p.Type != "insecure_agent_url" {
					t.Errorf("problem type = %q, want insecure_agent_url\n%s", p.Type, body)
				}
				if got := listed(); got != before {
					t.Errorf("a refused add left %d environments, want %d", got, before)
				}
				return
			}

			var created struct {
				Install install `json:"install"`
			}
			if err := json.Unmarshal(body, &created); err != nil {
				t.Fatal(err)
			}
			in := created.Install
			if want := strings.ReplaceAll(tc.wantAgentURL, "%ADDR%", addr); in.AgentURL != want {
				t.Errorf("install.agentUrl = %q, want %q", in.AgentURL, want)
			}
			for name, text := range map[string]string{"swarm": in.Swarm.Command, "standalone": in.Standalone.Command} {
				if got := strings.Contains(text, insecure+"=true"); got != tc.wantSwitch {
					t.Errorf("the %s command carries %s = %v, want %v:\n%s", name, insecure, got, tc.wantSwitch, text)
				}
			}
			if tc.wantSwitch && !strings.Contains(in.Standalone.Command, "-e '"+insecure+"=true'") {
				t.Errorf("the standalone command lacks -e '%s=true':\n%s", insecure, in.Standalone.Command)
			}
			if got := listed(); got != before+1 {
				t.Errorf("the add left %d environments, want %d", got, before+1)
			}
		})
	}
}

// TestTheSettingsSayWhenAgentsCannotUseTheirAddress is the operator's side of
// G-02-R4-3 (review WR-02), end to end through a real server. GET /api/settings
// says agentEndpointRefused exactly when the address agents are told to dial is
// plain ws:// for a host that is not this machine and the development switch is
// off: the same rule as the hub and the install.
func TestTheSettingsSayWhenAgentsCannotUseTheirAddress(t *testing.T) {
	cases := []struct {
		name string
		// env is extra server environment; the public URL is the listener's own address.
		env          map[string]string
		wantEndpoint string // %ADDR% is the listener's address
		wantRefused  bool
	}{
		{
			name:         "plain to another host, switch off",
			env:          map[string]string{"COAMING_AGENT_URL": "http://10.0.0.5:7480"},
			wantEndpoint: "ws://10.0.0.5:7480/agent/connect",
			wantRefused:  true,
		},
		{
			name:         "plain to another host, switch on",
			env:          map[string]string{"COAMING_AGENT_URL": "http://10.0.0.5:7480", "COAMING_AGENT_ALLOW_INSECURE": "true"},
			wantEndpoint: "ws://10.0.0.5:7480/agent/connect",
		},
		{
			name:         "no agent URL, public URL on this machine",
			env:          map[string]string{},
			wantEndpoint: "ws://%ADDR%/agent/connect",
		},
		{
			name:         "https agent URL",
			env:          map[string]string{"COAMING_AGENT_URL": "https://coaming.test"},
			wantEndpoint: "wss://coaming.test/agent/connect",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Skipf("cannot listen on 127.0.0.1: %v", err)
			}
			ts := httptest.NewUnstartedServer(nil)
			_ = ts.Listener.Close()
			ts.Listener = ln
			addr := ln.Addr().String()

			env := map[string]string{
				"COAMING_DATA_DIR":   t.TempDir() + "/data",
				"COAMING_PUBLIC_URL": "http://" + addr,
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

			cookie, _ := signInAdmin(t, srv, ts)
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/settings", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(cookie)
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /api/settings = %d %s", resp.StatusCode, body)
			}
			var got struct {
				AgentEndpoint        string `json:"agentEndpoint"`
				AgentEndpointRefused *bool  `json:"agentEndpointRefused"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if want := strings.ReplaceAll(tc.wantEndpoint, "%ADDR%", addr); got.AgentEndpoint != want {
				t.Errorf("agentEndpoint = %q, want %q", got.AgentEndpoint, want)
			}
			if got.AgentEndpointRefused == nil {
				t.Fatalf("GET /api/settings has no agentEndpointRefused field: %s", body)
			}
			if *got.AgentEndpointRefused != tc.wantRefused {
				t.Errorf("agentEndpointRefused = %v, want %v", *got.AgentEndpointRefused, tc.wantRefused)
			}
		})
	}
}
