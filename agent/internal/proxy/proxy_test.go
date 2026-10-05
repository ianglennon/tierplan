// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSocket is a Docker daemon stand-in on a unix socket. It records every
// request it is asked to serve, so a test can prove a refused request never
// reached it.
type fakeSocket struct {
	path string

	mu   sync.Mutex
	seen []string
	// headers are the headers of the requests seen, in the same order.
	headers []http.Header
	// release lets the /events handler finish.
	release chan struct{}
}

func newFakeSocket(t *testing.T) *fakeSocket {
	t.Helper()
	dir, err := os.MkdirTemp("", "bpag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fakeSocket{path: filepath.Join(dir, "d.sock"), release: make(chan struct{})}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(func() {
		close(f.release)
		// The proxy keeps idle connections to the daemon; Close would wait on
		// them for five seconds.
		srv.CloseClientConnections()
		srv.Close()
	})
	return f
}

func (f *fakeSocket) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.RequestURI())
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	w.Header().Set("Api-Version", "1.52")
	w.Header().Set("Ostype", "linux")
	w.Header().Set("Swarm", "inactive")
	if strings.HasSuffix(r.URL.Path, "/events") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Type":"container"}` + "\n"))
		_ = http.NewResponseController(w).Flush()
		select {
		case <-f.release:
		case <-r.Context().Done():
		}
		return
	}
	_, _ = w.Write([]byte("OK"))
}

func (f *fakeSocket) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// do sends one raw request over a one-connection stream, exactly as the server's
// Docker client would, and returns the parsed response.
func do(t *testing.T, h http.Handler, raw string) *http.Response {
	t.Helper()
	client, server := net.Pipe()
	go ServeConn(server, h)
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(client, raw); err != nil {
		t.Fatalf("write the request: %v", err)
	}
	method, _, _ := strings.Cut(raw, " ")
	// The request is named so a HEAD answer is read as one with no body.
	resp, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read the response to %q: %v", raw, err)
	}
	return resp
}

func get(path string, headers ...string) string {
	return request("GET", path, headers...)
}

func request(method, path string, headers ...string) string {
	return method + " " + path + " HTTP/1.1\r\nHost: docker\r\n" + strings.Join(headers, "") + "\r\n"
}

func TestReadOnlyCallsReachTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/_ping"},
		{"HEAD", "/_ping"},
		{"GET", "/v1.47/info"},
		{"GET", "/version"},
		{"GET", "/containers/json?all=1"},
		{"GET", "/images/json"},
		{"GET", "/volumes"},
		{"GET", "/networks"},
		{"GET", "/nodes"},
		{"GET", "/services?status=true"},
		{"GET", "/tasks"},
		{"GET", "/containers/abc123/json"},
		{"GET", "/v1.47/containers/web-1/json"},
		{"HEAD", "/containers/abc123/json"},
	} {
		resp := do(t, h, request(tc.method, tc.path))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s: status %d, want 200", tc.method, tc.path, resp.StatusCode)
		}
		if got := resp.Header.Get("Api-Version"); got != "1.52" {
			t.Errorf("%s %s: the Api-Version header was not passed through: %q", tc.method, tc.path, got)
		}
		if got := resp.Header.Get("Swarm"); got != "inactive" {
			t.Errorf("%s %s: the Swarm header was not passed through: %q", tc.method, tc.path, got)
		}
	}
	if got := len(sock.requests()); got != 14 {
		t.Errorf("the engine saw %d requests, want 14: %v", got, sock.requests())
	}
}

func TestEventsStreamIsFlushedAsItArrives(t *testing.T) {
	sock := newFakeSocket(t)
	resp := do(t, Handler(sock.path, Policy{}, nil), get("/events")) //nolint:bodyclose // closing would drain an endless stream; the helper closes the connection
	// The body is never closed here: Close would drain a stream that has no
	// end. The helper closes the connection when the test finishes.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events: %d", resp.StatusCode)
	}
	// The handler is still open (it waits for release), so this line can only
	// have been delivered by flushing, not by the end of the response.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "container") {
		t.Fatalf("the first event did not arrive while the stream was open: %q, %v", line, err)
	}
}

func TestEverythingElseIsRefusedWithATypedAnswerAndNeverReachesTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	for _, tc := range []struct {
		name, raw, wantMessage string
	}{
		{"create a container", request("POST", "/containers/create", "Content-Length: 2\r\n") + "{}", "agent_policy_denied: POST /containers/create"},
		{"the logs of a container", get("/containers/abc/logs"), "agent_policy_denied: GET /containers/abc/logs"},
		{"inspect a container with a POST", request("POST", "/containers/abc/json"), "agent_policy_denied: POST /containers/abc/json"},
		{"swarm inspect returns the join tokens", get("/swarm"), "agent_policy_denied: GET /swarm"},
		{"plugins", get("/plugins"), "agent_policy_denied: GET /plugins"},
		{"a double slash", get("//info"), "agent_policy_denied: GET //info"},
		{"a dot-dot segment", get("/containers/../info"), "agent_policy_denied: GET /containers/../info"},
		{"an encoded slash", get("/containers%2Fjson"), "agent_policy_denied: GET /containers%2Fjson"},
		{"an upgrade", get("/info", "Upgrade: websocket\r\n", "Connection: Upgrade\r\n"), "agent_policy_denied: GET /info"},
		{"an upgrade token in Connection alone", get("/info", "Connection: keep-alive, upgrade\r\n"), "agent_policy_denied: GET /info"},
		{"a body on a GET", request("GET", "/info", "Content-Length: 2\r\n") + "{}", "agent_policy_denied: GET /info"},
		{"a chunked GET", request("GET", "/info", "Transfer-Encoding: chunked\r\n") + "0\r\n\r\n", "agent_policy_denied: GET /info"},
		{"an absolute-form target", "GET http://docker/info HTTP/1.1\r\nHost: docker\r\n\r\n", "agent_policy_denied: GET http://docker/info"},
		{"a delete", request("DELETE", "/containers/abc"), "agent_policy_denied: DELETE /containers/abc"},
		{"a longer path under an allowed one", get("/containers/json/extra"), "agent_policy_denied: GET /containers/json/extra"},
		{"a trailing slash", get("/info/"), "agent_policy_denied: GET /info/"},
		{"an upper-case path", get("/INFO"), "agent_policy_denied: GET /INFO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, h, tc.raw)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type %q, want application/json", ct)
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

func TestAnOversizedQueryIsRefused(t *testing.T) {
	sock := newFakeSocket(t)
	resp := do(t, Handler(sock.path, Policy{}, nil), get("/containers/json?x="+strings.Repeat("a", maxQuery+1)))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
	if len(sock.requests()) != 0 {
		t.Fatal("the engine saw the oversized request")
	}
}

func TestAControlCharacterInTheTargetIsRefused(t *testing.T) {
	sock := newFakeSocket(t)
	resp := do(t, Handler(sock.path, Policy{}, nil), "GET /info?a=\x01 HTTP/1.1\r\nHost: docker\r\n\r\n")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want a refusal", resp.StatusCode)
	}
	if len(sock.requests()) != 0 {
		t.Fatal("the engine saw a request with a control character")
	}
}

func TestAnUnreachableEngineAnswers502NotAPolicyDenial(t *testing.T) {
	h := Handler(filepath.Join(t.TempDir(), "missing.sock"), Policy{}, nil)
	resp := do(t, h, get("/_ping"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), DeniedPrefix) {
		t.Fatalf("an engine failure was reported as a policy denial: %s", b)
	}
}

func TestServeConnReturnsWhenTheStreamCloses(t *testing.T) {
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		ServeConn(server, Handler(filepath.Join(t.TempDir(), "x.sock"), Policy{}, nil))
		close(done)
	}()
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeConn did not return after the stream closed")
	}
}

func TestAKeepAliveStreamServesSeveralRequests(t *testing.T) {
	sock := newFakeSocket(t)
	client, server := net.Pipe()
	go ServeConn(server, Handler(sock.path, Policy{}, nil))
	defer func() { _ = client.Close() }()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(client)
	for i := 0; i < 3; i++ {
		if _, err := io.WriteString(client, get("/_ping")); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
}

func TestReadOnlyRefusesEveryChangeWhateverTheAllowlistSays(t *testing.T) {
	for _, tc := range []struct {
		name           string
		method, path   string
		policy         Policy
		wantOK         bool
		wantReason     string
		hasBody, upgra bool
	}{
		{name: "a read with the flag set", method: "GET", path: "/info", policy: Policy{ReadOnly: true}, wantOK: true},
		{name: "a head with the flag set", method: "HEAD", path: "/_ping", policy: Policy{ReadOnly: true}, wantOK: true},
		{name: "a POST on an allowlisted path with the flag set", method: "POST", path: "/info", policy: Policy{ReadOnly: true}, wantReason: "read_only_flag"},
		{name: "a POST on any other path with the flag set", method: "POST", path: "/containers/create", policy: Policy{ReadOnly: true}, hasBody: true, wantReason: "read_only_flag"},
		{name: "a DELETE with the flag set", method: "DELETE", path: "/containers/abc", policy: Policy{ReadOnly: true}, wantReason: "read_only_flag"},
		{name: "a POST on a non-allowlisted path without the flag", method: "POST", path: "/containers/create", hasBody: true, wantReason: "allowlist"},
		{name: "a POST on an allowlisted path without the flag", method: "POST", path: "/info", wantReason: "allowlist"},
		{name: "a GET off the allowlist without the flag", method: "GET", path: "/swarm", wantReason: "allowlist"},
		{name: "a GET off the allowlist with the flag", method: "GET", path: "/swarm", policy: Policy{ReadOnly: true}, wantReason: "allowlist"},
		{name: "an upgrade on an allowed path", method: "GET", path: "/info", upgra: true, wantReason: "allowlist"},
		{name: "a body on a GET", method: "GET", path: "/info", hasBody: true, wantReason: "allowlist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := Allowed(tc.method, tc.path, "", tc.hasBody, tc.upgra, tc.policy)
			if ok != tc.wantOK || reason != tc.wantReason {
				t.Fatalf("Allowed = %v %q, want %v %q", ok, reason, tc.wantOK, tc.wantReason)
			}
		})
	}
}

// Every one of these has been used to get past a path filter somewhere. None may
// be allowed, whatever the policy.
func TestAllowedRejectsTheTrickyPaths(t *testing.T) {
	for _, tc := range []struct{ name, path, query string }{
		{"a double slash", "//info", ""},
		{"a trailing slash", "/info/", ""},
		{"a dot segment", "/./info", ""},
		{"an encoded dot-dot", "/%2e%2e/info", ""},
		{"a double slash after the version", "/v1.47//info", ""},
		{"an encoded NUL", "/info%00", ""},
		{"a raw NUL", "/info\x00", ""},
		{"a control character in the path", "/info\x01", ""},
		{"a control character in the query", "/info", "a=\x01"},
		{"a newline in the query", "/info", "a=b\nHost: evil"},
		{"a DEL character", "/info\x7f", ""},
		{"a five KiB query", "/containers/json", strings.Repeat("a", 5<<10)},
		{"an encoded slash", "/containers%2Fjson", ""},
		{"a backslash", "/containers\\json", ""},
		{"an upper-case path", "/INFO", ""},
		{"an absolute-form target", "http://docker/info", ""},
		{"a path that is only a version", "/v1.47", ""},
		{"a version with a letter", "/v1.4x/info", ""},
		{"an empty path", "", ""},
		{"no leading slash", "info", ""},
		{"a unicode look-alike", "/info\u2215x", ""},
	} {
		for _, readOnly := range []bool{false, true} {
			if ok, _ := Allowed("GET", tc.path, tc.query, false, false, Policy{ReadOnly: readOnly}); ok {
				t.Errorf("%s: %q?%q was allowed (read-only %v)", tc.name, tc.path, tc.query, readOnly)
			}
		}
	}
	// And the plain forms of the same paths are allowed, so the table tests the
	// trick and not the path.
	for _, p := range []string{"/info", "/v1.47/info", "/_ping", "/containers/json", "/v1.52/events"} {
		if ok, reason := Allowed("GET", p, "all=1", false, false, Policy{}); !ok {
			t.Errorf("%s was refused (%s)", p, reason)
		}
	}
}

func TestEveryRefusalReportsOnDeniedOnceAndAnAllowedRequestReportsNothing(t *testing.T) {
	sock := newFakeSocket(t)
	type denied struct{ method, path, reason string }
	var mu sync.Mutex
	var got []denied
	h := Handler(sock.path, Policy{}, func(method, path, reason string) {
		mu.Lock()
		got = append(got, denied{method, path, reason})
		mu.Unlock()
	})
	for _, raw := range []string{
		request("POST", "/containers/create", "Content-Length: 2\r\n") + "{}",
		get("/swarm"),
		get("//info"),
		get("/containers/json?x=" + strings.Repeat("a", maxQuery+1)),
	} {
		resp := do(t, h, raw)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status %d for %q, want 403", resp.StatusCode, raw[:20])
		}
	}
	resp := do(t, h, get("/info"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an allowed request answered %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []denied{
		{"POST", "/containers/create", "allowlist"},
		{"GET", "/swarm", "allowlist"},
		{"GET", "//info", "allowlist"},
		{"GET", "/containers/json", "allowlist"},
	}
	if len(got) != len(want) {
		t.Fatalf("onDenied ran %d times (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("denial %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestAReadOnlyAgentReportsTheReadOnlyReasonAndNeverReachesTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	var mu sync.Mutex
	var reasons []string
	h := Handler(sock.path, Policy{ReadOnly: true}, func(_, _, reason string) {
		mu.Lock()
		reasons = append(reasons, reason)
		mu.Unlock()
	})
	resp := do(t, h, request("POST", "/info"))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "agent_policy_denied: POST /info") {
		t.Fatalf("status %d body %s, want the typed refusal", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reasons) != 1 || reasons[0] != "read_only_flag" {
		t.Fatalf("reasons = %v, want [read_only_flag]", reasons)
	}
	if len(sock.requests()) != 0 {
		t.Fatalf("the engine saw %v", sock.requests())
	}
}

func TestADeniedPathIsCappedWhereItIsReported(t *testing.T) {
	sock := newFakeSocket(t)
	var mu sync.Mutex
	var paths []string
	h := Handler(sock.path, Policy{}, func(_, path, _ string) {
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
	})
	resp := do(t, h, get("/"+strings.Repeat("a", 3000)))
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || len(paths[0]) != maxShownPath {
		t.Fatalf("reported paths %d long, want one of %d", len(paths[0]), maxShownPath)
	}
}

func TestHopByHopAndForwardingHeadersDoNotReachTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	resp := do(t, h, get("/_ping",
		"Keep-Alive: timeout=5\r\n", "Proxy-Authorization: Basic eDp5\r\n", "Te: trailers\r\n",
		"Connection: X-Custom\r\n", "X-Custom: 1\r\n", "X-Forwarded-For: 203.0.113.9\r\n", "Accept: application/json\r\n"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	sock.mu.Lock()
	defer sock.mu.Unlock()
	if len(sock.headers) != 1 {
		t.Fatalf("the engine saw %d requests", len(sock.headers))
	}
	for _, name := range []string{"Keep-Alive", "Proxy-Authorization", "Te", "X-Custom", "X-Forwarded-For"} {
		if v := sock.headers[0].Get(name); v != "" {
			t.Errorf("the engine received %s: %s", name, v)
		}
	}
	if sock.headers[0].Get("Accept") != "application/json" {
		t.Errorf("an end-to-end header was dropped: %v", sock.headers[0])
	}
}

// The one thing opened up for plan 20 (D-27, T-02-70): GET and HEAD of
// /containers/{id}/json, which Bayplan reads only for a container's restart
// policy. Nothing else under /containers/{id}/ is allowed.
func TestAllowedOpensOnlyTheInspectOfOneContainer(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		wantOK             bool
	}{
		{"an inspect by ID", "GET", "/containers/abc123/json", true},
		{"an inspect by name with the version prefix", "GET", "/v1.47/containers/web-1/json", true},
		{"a HEAD of an inspect", "HEAD", "/containers/abc123/json", true},
		{"a name with dots and underscores", "GET", "/containers/my_app.1/json", true},
		{"a POST of an inspect", "POST", "/containers/abc/json", false},
		{"a DELETE of an inspect", "DELETE", "/containers/abc/json", false},
		{"the logs", "GET", "/containers/abc/logs", false},
		{"a stats stream", "GET", "/containers/abc/stats", false},
		{"a file archive", "GET", "/containers/abc/archive", false},
		{"an export", "GET", "/containers/abc/export", false},
		{"a dot-dot ID", "GET", "/containers/../json", false},
		{"an ID that starts with a dot", "GET", "/containers/.hidden/json", false},
		{"an ID that starts with a dash", "GET", "/containers/-x/json", false},
		{"an empty ID", "GET", "/containers//json", false},
		{"an encoded slash in the ID", "GET", "/containers/a%2Fb/json", false},
		{"an ID with a space", "GET", "/containers/a b/json", false},
		{"an ID of 129 characters", "GET", "/containers/a" + strings.Repeat("b", 128) + "/json", false},
		{"a second segment after json", "GET", "/containers/abc/json/extra", false},
		{"the list itself still allowed", "GET", "/containers/json", true},
		{"the images inspect is not opened", "GET", "/images/abc/json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, _ := Allowed(tc.method, tc.path, "", false, false, Policy{})
			if ok != tc.wantOK {
				t.Fatalf("Allowed(%s %q) = %v, want %v", tc.method, tc.path, ok, tc.wantOK)
			}
		})
	}
	// A read-only host still refuses the POST, whatever the allowlist grew to.
	if ok, reason := Allowed("POST", "/containers/abc/json", "", false, false, Policy{ReadOnly: true}); ok || reason != "read_only_flag" {
		t.Errorf("a POST of an inspect on a read-only host = %v %q, want refused read_only_flag", ok, reason)
	}
}
