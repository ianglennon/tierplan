// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestRouteContainerActionsReachTheEngineUnchanged(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	cases := []struct{ method, target string }{
		{"POST", "/v1.47/containers/abc/stop?t=10"},
		{"POST", "/containers/abc/stop?t=-1&signal=SIGTERM"},
		{"POST", "/containers/abc/stop?t=86400"},
		{"POST", "/containers/abc/restart?t=0"},
		{"POST", "/containers/abc/kill?signal=SIGTERM"},
		{"POST", "/containers/abc/kill?signal=9"},
		{"POST", "/containers/abc/start"},
		{"POST", "/containers/abc/pause"},
		{"POST", "/containers/abc/unpause"},
		{"DELETE", "/containers/abc?force=1&v=1"},
		{"DELETE", "/containers/abc"},
	}
	for _, tc := range cases {
		resp := do(t, h, request(tc.method, tc.target))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s: status %d, want 200", tc.method, tc.target, resp.StatusCode)
		}
	}
	got := sock.requests()
	if len(got) != len(cases) {
		t.Fatalf("the engine saw %d requests, want %d: %v", len(got), len(cases), got)
	}
	for i, tc := range cases {
		if want := tc.method + " " + tc.target; got[i] != want {
			t.Errorf("request %d reached the engine as %q, want %q", i, got[i], want)
		}
	}
}

func TestRouteSmuggledVariantsAreRefusedAndNeverReachTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	for _, tc := range []struct {
		name, raw, wantMessage string
	}{
		{"link on a remove", request("DELETE", "/containers/abc?link=1"), "agent_policy_denied: DELETE /containers/abc"},
		{"link beside force", request("DELETE", "/containers/abc?force=1&link=1"), "agent_policy_denied: DELETE /containers/abc"},
		{"a value that is not a bool on a remove", request("DELETE", "/containers/abc?force=yes"), "agent_policy_denied: DELETE /containers/abc"},
		{"an unknown key on stop", request("POST", "/containers/abc/stop?t=10&foo=1"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a timeout that is not a number", request("POST", "/containers/abc/stop?t=abc"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a timeout above a day", request("POST", "/containers/abc/stop?t=86401"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a timeout below -1", request("POST", "/containers/abc/stop?t=-2"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a shell substitution in the signal", request("POST", "/containers/abc/stop?signal=$(x)"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a second value that is not allowed", request("POST", "/containers/abc/stop?t=10&t=abc"), "agent_policy_denied: POST /containers/abc/stop"},
		{"an escaped unknown key", request("POST", "/containers/abc/stop?%66oo=1"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a query the parser rejects", request("POST", "/containers/abc/stop?t=1;foo=2"), "agent_policy_denied: POST /containers/abc/stop"},
		{"a body on stop", request("POST", "/containers/abc/stop", "Content-Length: 2\r\n") + "{}", "agent_policy_denied: POST /containers/abc/stop"},
		{"a form body on start", request("POST", "/containers/abc/start", "Content-Type: application/x-www-form-urlencoded\r\n", "Content-Length: 5\r\n") + "t=1&x", "agent_policy_denied: POST /containers/abc/start"},
		{"a query key on start", request("POST", "/containers/abc/start?detachKeys=x"), "agent_policy_denied: POST /containers/abc/start"},
		{"a signal on a pause", request("POST", "/containers/abc/pause?signal=KILL"), "agent_policy_denied: POST /containers/abc/pause"},
		{"an upgrade on stop", request("POST", "/containers/abc/stop", "Upgrade: tcp\r\n", "Connection: Upgrade\r\n"), "agent_policy_denied: POST /containers/abc/stop"},
		{"an ID that starts with a dot", request("POST", "/containers/.x/stop"), "agent_policy_denied: POST /containers/.x/stop"},
		{"attach", request("POST", "/containers/abc/attach"), "agent_policy_denied: POST /containers/abc/attach"},
		{"archive", get("/containers/abc/archive"), "agent_policy_denied: GET /containers/abc/archive"},
		{"a put to a container", request("PUT", "/containers/abc/archive"), "agent_policy_denied: PUT /containers/abc/archive"},
		{"a GET of stop", get("/containers/abc/stop"), "agent_policy_denied: GET /containers/abc/stop"},
		{"a delete of a container action", request("DELETE", "/containers/abc/stop"), "agent_policy_denied: DELETE /containers/abc/stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, h, tc.raw)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			var body struct{ Message string }
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("the refusal is not JSON: %v", err)
			}
			if body.Message != tc.wantMessage {
				t.Errorf("message %q, want %q", body.Message, tc.wantMessage)
			}
		})
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("a refused request reached the engine: %v", got)
	}
}

func TestRouteReadOnlyRefusesEveryWriteAndStillAllowsTheReads(t *testing.T) {
	sock := newFakeSocket(t)
	var mu sync.Mutex
	var reasons []string
	h := Handler(sock.path, Policy{ReadOnly: true}, func(_, _, reason string) {
		mu.Lock()
		reasons = append(reasons, reason)
		mu.Unlock()
	})
	writes := []struct{ method, target string }{
		{"POST", "/containers/abc/stop?t=10"},
		{"POST", "/containers/abc/kill?signal=SIGTERM"},
		{"POST", "/containers/abc/start"},
		{"POST", "/containers/abc/restart"},
		{"POST", "/containers/abc/pause"},
		{"POST", "/containers/abc/unpause"},
		{"DELETE", "/containers/abc?force=1&v=1"},
		{"DELETE", "/containers/abc?link=1"},
	}
	for _, tc := range writes {
		resp := do(t, h, request(tc.method, tc.target))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: status %d on a read-only host, want 403", tc.method, tc.target, resp.StatusCode)
		}
	}
	if len(sock.requests()) != 0 {
		t.Fatalf("a write reached the engine of a read-only host: %v", sock.requests())
	}
	mu.Lock()
	defer mu.Unlock()
	for i, r := range reasons {
		if r != "read_only_flag" {
			t.Errorf("refusal %d has the reason %q, want read_only_flag", i, r)
		}
	}
	if len(reasons) != len(writes) {
		t.Errorf("onDenied ran %d times, want %d", len(reasons), len(writes))
	}
	for _, target := range []string{"/_ping", "/containers/json?all=1", "/containers/abc/json"} {
		resp := do(t, h, get(target))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s on a read-only host: status %d, want 200", target, resp.StatusCode)
		}
	}
}

func TestRouteTheReadSurfaceOfEarlierPhasesStillTakesAnyQuery(t *testing.T) {
	q := "filters=" + strings.Repeat("a", 1000) + "&all=1&whatever=2"
	for _, p := range []string{
		"/_ping", "/version", "/info", "/containers/json", "/images/json", "/volumes", "/networks",
		"/events", "/nodes", "/services", "/tasks", "/containers/abc/json",
		"/v1.47/events", "/v1.52/containers/web-1/json",
	} {
		for _, method := range []string{"GET", "HEAD"} {
			if ok, reason, body := Allowed(method, p, q, false, false, Policy{}); !ok || body != BodyNone {
				t.Errorf("%s %s was refused (%s)", method, p, reason)
			}
		}
	}
	if ok, _, _ := Allowed("GET", "/events", strings.Repeat("a", maxQuery+1), false, false, Policy{}); ok {
		t.Error("a query over the cap was allowed on a read route")
	}
}

func TestRouteMatchTreatsHeadAsGetAndNothingElseAsAnything(t *testing.T) {
	if _, ok := Match("HEAD", "/_ping"); !ok {
		t.Error("HEAD /_ping did not match the GET route")
	}
	if _, ok := Match("HEAD", "/containers/abc/stop"); ok {
		t.Error("HEAD matched a POST route")
	}
	rt, ok := Match("POST", "/v1.47/containers/abc/kill")
	if !ok || rt.name != "container kill" || rt.body != BodyNone || rt.upgrade {
		t.Errorf("POST kill matched %+v, %v", rt, ok)
	}
	if _, ok := Match("GET", "/containers/abc/kill"); ok {
		t.Error("GET matched a POST route")
	}
}
