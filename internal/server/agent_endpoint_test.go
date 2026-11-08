// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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
// HTTP. TierPlan hands out only an address its agents will dial: an https address
// is handed out as wss://. An http address cannot reach this point, because
// TIERPLAN_PUBLIC_URL and TIERPLAN_AGENT_URL refuse it at start.
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
	cases := []struct {
		name string
		// env is extra server environment; %ADDR% in a value is the listener's address.
		env map[string]string
		// wantCode is the status of POST /api/environments {"kind":"agent"}.
		wantCode int
		// wantAgentURL is the install's agentUrl for a 201, %ADDR% as above.
		wantAgentURL string
	}{
		{
			name:         "https on this machine",
			env:          map[string]string{"TIERPLAN_PUBLIC_URL": "https://%ADDR%"},
			wantCode:     http.StatusCreated,
			wantAgentURL: "wss://%ADDR%/agent/connect",
		},
		{
			name:         "https",
			env:          map[string]string{"TIERPLAN_AGENT_URL": "https://tierplan.test"},
			wantCode:     http.StatusCreated,
			wantAgentURL: "wss://tierplan.test/agent/connect",
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
				"TIERPLAN_DATA_DIR":   t.TempDir() + "/data",
				"TIERPLAN_PUBLIC_URL": "https://" + addr,
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
			if got := listed(); got != before+1 {
				t.Errorf("the add left %d environments, want %d", got, before+1)
			}
		})
	}
}
