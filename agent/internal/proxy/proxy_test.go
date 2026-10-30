// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package proxy

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"

	"github.com/ianglennon/coaming/proto/testsupport"
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
	// bodies are the request bodies seen, in the same order.
	bodies [][]byte
	// release lets the /events and /logs handlers finish.
	release chan struct{}
	// answers maps a request path to the JSON the daemon answers with, for the
	// network and volume lookups a service-spec check makes. Set before the first
	// request.
	answers map[string]string
	// status maps a request path in answers to the status it is answered with,
	// where it is not 200. Set before the first request.
	status map[string]int
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
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.RequestURI())
	f.headers = append(f.headers, r.Header.Clone())
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/start") &&
		strings.EqualFold(r.Header.Get("Upgrade"), "tcp") {
		// The engine answers an exec start with 101 and then speaks raw bytes both
		// ways. This one echoes them.
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, rw)
		return
	}
	if answer, ok := f.answers[r.URL.Path]; ok {
		w.Header().Set("Content-Type", "application/json")
		if code, ok := f.status[r.URL.Path]; ok {
			w.WriteHeader(code)
		}
		_, _ = w.Write([]byte(answer))
		return
	}
	if name, ok := strings.CutPrefix(r.URL.Path, "/volumes/"); ok && r.Method == http.MethodGet && name != "" {
		// An engine answers a volume it does not have with 404.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		// Encoded, not concatenated: the name comes from the request path.
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "get " + name + ": no such volume"})
		return
	}
	w.Header().Set("Api-Version", "1.52")
	w.Header().Set("Ostype", "linux")
	w.Header().Set("Swarm", "inactive")
	if strings.HasSuffix(r.URL.Path, "/events") || strings.HasSuffix(r.URL.Path, "/logs") {
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

// bodiesSeen is the request bodies the engine received, in order.
func (f *fakeSocket) bodiesSeen() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.bodies...)
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
	_ = client.SetDeadline(time.Now().Add(testsupport.Deadline))
	// The request is written on its own goroutine: a net.Pipe has no buffer, so a
	// handler that refuses a large body and answers before reading it all would
	// otherwise leave this write blocked.
	go func() { _, _ = io.WriteString(client, raw) }()
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
		{"create a container with no image", request("POST", "/containers/create", "Content-Length: 2\r\n") + "{}", "agent_policy_denied: POST /containers/create"},
		{"the archive of a container", get("/containers/abc/archive"), "agent_policy_denied: GET /containers/abc/archive"},
		{"the stats of a container with a key the one-shot read does not send", get("/containers/abc/stats?decode=1"), "agent_policy_denied: GET /containers/abc/stats"},
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
		{"a delete with the legacy link key", request("DELETE", "/containers/abc?link=1"), "agent_policy_denied: DELETE /containers/abc"},
		{"attach to a container", request("POST", "/containers/abc/attach"), "agent_policy_denied: POST /containers/abc/attach"},
		{"the file archive of a container", get("/containers/abc/archive"), "agent_policy_denied: GET /containers/abc/archive"},
		{"a forced delete of an image", request("DELETE", "/images/abc?force=1"), "agent_policy_denied: DELETE /images/abc"},
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
	testsupport.Await(t, "ServeConn to return after the stream closed", done)
}

func TestAKeepAliveStreamServesSeveralRequests(t *testing.T) {
	sock := newFakeSocket(t)
	client, server := net.Pipe()
	go ServeConn(server, Handler(sock.path, Policy{}, nil))
	defer func() { _ = client.Close() }()
	_ = client.SetDeadline(time.Now().Add(testsupport.Deadline))
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
		{name: "a POST on a non-allowlisted path without the flag", method: "POST", path: "/containers/abc/attach", hasBody: true, wantReason: "allowlist"},
		{name: "a container create without the flag", method: "POST", path: "/containers/create", hasBody: true, wantOK: true},
		{name: "a POST on an allowlisted path without the flag", method: "POST", path: "/info", wantReason: "allowlist"},
		{name: "a GET off the allowlist without the flag", method: "GET", path: "/swarm", wantReason: "allowlist"},
		{name: "a GET off the allowlist with the flag", method: "GET", path: "/swarm", policy: Policy{ReadOnly: true}, wantReason: "allowlist"},
		{name: "an upgrade on an allowed path", method: "GET", path: "/info", upgra: true, wantReason: "allowlist"},
		{name: "a body on a GET", method: "GET", path: "/info", hasBody: true, wantReason: "allowlist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason, _ := Allowed(tc.method, tc.path, "", tc.hasBody, tc.upgra, tc.policy)
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
			if ok, _, _ := Allowed("GET", tc.path, tc.query, false, false, Policy{ReadOnly: readOnly}); ok {
				t.Errorf("%s: %q?%q was allowed (read-only %v)", tc.name, tc.path, tc.query, readOnly)
			}
		}
	}
	// And the plain forms of the same paths are allowed, so the table tests the
	// trick and not the path.
	for _, p := range []string{"/info", "/v1.47/info", "/_ping", "/containers/json", "/v1.52/events"} {
		if ok, reason, _ := Allowed("GET", p, "all=1", false, false, Policy{}); !ok {
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
// /containers/{id}/json, which Coaming reads only for a container's restart
// policy. Phase 4 adds the lifecycle, the logs and the exec create under
// /containers/{id}/ (routes_test.go, body_test.go) and Phase 6 the stats; the archive,
// attach and export stay refused.
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
		{"the logs", "GET", "/containers/abc/logs", true},
		{"the stats", "GET", "/containers/abc/stats", true},
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
		{"an image tag is not opened", "POST", "/images/abc/tag", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, _, _ := Allowed(tc.method, tc.path, "", false, false, Policy{})
			if ok != tc.wantOK {
				t.Fatalf("Allowed(%s %q) = %v, want %v", tc.method, tc.path, ok, tc.wantOK)
			}
		})
	}
	// A read-only host still refuses the POST, whatever the allowlist grew to.
	if ok, reason, _ := Allowed("POST", "/containers/abc/json", "", false, false, Policy{ReadOnly: true}); ok || reason != "read_only_flag" {
		t.Errorf("a POST of an inspect on a read-only host = %v %q, want refused read_only_flag", ok, reason)
	}
}

// A registry credential is carried by the two routes that use one and by nothing
// else (T-05-09). The value below is only shaped like base64url JSON.
const testRegistryAuth = "eyJ1c2VybmFtZSI6InUiLCJwYXNzd29yZCI6InAifQ=="

// headersSeen is the headers of the requests the engine received, in order.
func (f *fakeSocket) headersSeen() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.headers...)
}

func TestRegistryAuthReachesTheEngineOnlyOnImagePull(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	pull := "/v1.56/images/create?fromImage=docker.io%2Flibrary%2Fbusybox&tag=1.36.1"
	for _, tc := range []struct {
		name, method, target string
		wantAuth             bool
	}{
		{"an image pull", "POST", pull, true},
		{"a container list", "GET", "/containers/json", false},
		{"an image list", "GET", "/images/json", false},
		{"a volume remove", "DELETE", "/volumes/v1", false},
		{"a container stop", "POST", "/containers/abc/stop", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(sock.requests())
			resp := do(t, h, request(tc.method, tc.target,
				"X-Registry-Auth: "+testRegistryAuth+"\r\n", "X-Registry-Config: e30=\r\n"))
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d, want 200", resp.StatusCode)
			}
			seen := sock.headersSeen()
			if len(seen) != before+1 {
				t.Fatalf("the engine saw %d requests, want %d", len(seen), before+1)
			}
			hdr := seen[before]
			got := hdr.Values("X-Registry-Auth")
			if tc.wantAuth {
				if len(got) != 1 || got[0] != testRegistryAuth {
					t.Errorf("the engine received X-Registry-Auth %q, want the one value untouched", got)
				}
			} else if len(got) != 0 {
				t.Errorf("the engine received X-Registry-Auth %q on %s %s", got, tc.method, tc.target)
			}
			if v := hdr.Values("X-Registry-Config"); len(v) != 0 {
				t.Errorf("the engine received X-Registry-Config %q on %s %s", v, tc.method, tc.target)
			}
		})
	}
}

func TestImagePullTakesOnlyTheKeysOfAPull(t *testing.T) {
	sock := newFakeSocket(t)
	var mu sync.Mutex
	var denied []string
	h := Handler(sock.path, Policy{}, func(method, path, reason string) {
		mu.Lock()
		denied = append(denied, method+" "+path+" "+reason)
		mu.Unlock()
	})
	okTargets := []string{
		"/images/create?fromImage=docker.io%2Flibrary%2Fbusybox&tag=1.36.1",
		"/v1.56/images/create?fromImage=ghcr.io%2Facme%2Fapp&tag=sha256%3A" + strings.Repeat("a", 64),
		"/images/create?fromImage=registry.example.org%3A5000%2Fteam%2Fapp&tag=v1&platform=linux%2Farm64%2Fv8",
	}
	for _, target := range okTargets {
		resp := do(t, h, request("POST", target))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST %s: status %d, want 200", target, resp.StatusCode)
		}
	}
	for _, tc := range []struct {
		name, raw string
	}{
		{"an import from a URL", request("POST", "/images/create?fromSrc=http%3A%2F%2Fx%2Fimg.tar&repo=x")},
		{"a repo key", request("POST", "/images/create?fromImage=a&tag=b&repo=x")},
		{"a message key", request("POST", "/images/create?fromImage=a&tag=b&message=x")},
		{"a changes key", request("POST", "/images/create?fromImage=a&tag=b&changes=CMD+x")},
		{"an import from stdin", request("POST", "/images/create?fromSrc=-", "Content-Length: 4\r\n") + "data"},
		{"a body on a pull", request("POST", "/images/create?fromImage=a&tag=b", "Content-Length: 2\r\n") + "{}"},
		{"a tag that is not a tag", request("POST", "/images/create?fromImage=a&tag=b%2Fc")},
		{"a platform that is not os/arch", request("POST", "/images/create?fromImage=a&tag=b&platform=x")},
		{"an image that starts with a dash", request("POST", "/images/create?fromImage=-a&tag=b")},
		{"a GET of create", get("/images/create?fromImage=a&tag=b")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(sock.requests())
			resp := do(t, h, tc.raw)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			if got := len(sock.requests()); got != before {
				t.Fatalf("a refused pull reached the engine")
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	for _, d := range denied {
		if strings.Contains(d, "?") || strings.Contains(d, "fromSrc") || strings.Contains(d, "fromImage") {
			t.Errorf("a refusal reported the query: %q", d)
		}
	}
}

func TestImagePullAndRemoveAreRefusedOnAReadOnlyHostBeforeTheTable(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{ReadOnly: true}, nil)
	for _, tc := range []struct{ method, target string }{
		{"POST", "/images/create?fromImage=a&tag=b"},
		{"POST", "/images/create?fromSrc=http%3A%2F%2Fx"},
		{"DELETE", "/images/x"},
	} {
		ok, reason, _ := Allowed(tc.method, strings.SplitN(tc.target, "?", 2)[0], "", false, false, Policy{ReadOnly: true})
		if ok || reason != "read_only_flag" {
			t.Errorf("%s %s on a read-only host = %v %q, want refused read_only_flag", tc.method, tc.target, ok, reason)
		}
		resp := do(t, h, request(tc.method, tc.target))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: status %d, want 403", tc.method, tc.target, resp.StatusCode)
		}
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("a write reached the engine of a read-only host: %v", got)
	}
}

func TestRegistryHeaderShapeIsChecked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		want   bool
	}{
		{"absent", nil, true},
		{"empty", []string{""}, true},
		{"base64url without padding", []string{"eyJhIjoiYiJ9"}, true},
		{"base64url with padding", []string{testRegistryAuth}, true},
		{"url-safe alphabet", []string{"a-b_c"}, true},
		{"two values", []string{"YQ==", "Yg=="}, false},
		{"two identical values", []string{"YQ==", "YQ=="}, false},
		{"exactly 16 KiB", []string{strings.Repeat("A", maxRegistryAuth)}, true},
		{"16 KiB and one byte", []string{strings.Repeat("A", maxRegistryAuth+1)}, false},
		{"a dot", []string{"a.b"}, false},
		{"a slash", []string{"a/b"}, false},
		{"a plus", []string{"a+b"}, false},
		{"a space", []string{"a b"}, false},
		{"a newline", []string{"ab\n"}, false},
		{"padding in the middle", []string{"a=b"}, false},
		{"three padding characters", []string{"abc==="}, false},
		{"padding only", []string{"=="}, false},
		{"a non-ASCII byte", []string{"é"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := registryHeaderOK(tc.values); got != tc.want {
				t.Errorf("registryHeaderOK(%q) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}

func TestAMalformedRegistryHeaderOnAPullIsRefusedAndNeverReachesTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	var mu sync.Mutex
	var reasons []string
	h := Handler(sock.path, Policy{}, func(_, path, reason string) {
		mu.Lock()
		reasons = append(reasons, path+" "+reason)
		mu.Unlock()
	})
	target := "/images/create?fromImage=a&tag=b"
	for _, tc := range []struct {
		name string
		hdrs []string
	}{
		{"two values", []string{"X-Registry-Auth: YQ==\r\n", "X-Registry-Auth: Yg==\r\n"}},
		{"over 16 KiB", []string{"X-Registry-Auth: " + strings.Repeat("A", maxRegistryAuth+1) + "\r\n"}},
		{"a dot", []string{"X-Registry-Auth: a.b\r\n"}},
		{"a slash", []string{"X-Registry-Auth: a/b\r\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, h, request("POST", target, tc.hdrs...))
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
		})
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("a refused pull reached the engine: %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range reasons {
		if r != "/images/create allowlist" {
			t.Errorf("onDenied got %q, want the path without its query and the allowlist reason", r)
		}
	}
	if len(reasons) != 4 {
		t.Errorf("onDenied ran %d times, want 4", len(reasons))
	}
}

func TestDistributionKeepsTheRegistryHeaderWhileInspectAndHistoryDropIt(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	for _, tc := range []struct {
		method, target string
		wantAuth       bool
	}{
		{"GET", "/distribution/ghcr.io/acme/app:1/json", true},
		{"GET", "/images/ghcr.io/acme/app:1/json", false},
		{"GET", "/images/ghcr.io/acme/app:1/history", false},
		{"DELETE", "/images/ghcr.io/acme/app:1", false},
	} {
		before := len(sock.requests())
		resp := do(t, h, request(tc.method, tc.target,
			"X-Registry-Auth: "+testRegistryAuth+"\r\n", "X-Registry-Config: e30=\r\n"))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: status %d, want 200", tc.method, tc.target, resp.StatusCode)
		}
		seen := sock.headersSeen()
		if len(seen) != before+1 {
			t.Fatalf("%s %s: the engine saw %d requests, want %d", tc.method, tc.target, len(seen), before+1)
		}
		got := seen[before].Values("X-Registry-Auth")
		if tc.wantAuth && (len(got) != 1 || got[0] != testRegistryAuth) {
			t.Errorf("%s %s: the engine received X-Registry-Auth %q, want the one value", tc.method, tc.target, got)
		}
		if !tc.wantAuth && len(got) != 0 {
			t.Errorf("%s %s: the engine received X-Registry-Auth %q", tc.method, tc.target, got)
		}
		if v := seen[before].Values("X-Registry-Config"); len(v) != 0 {
			t.Errorf("%s %s: the engine received X-Registry-Config %q", tc.method, tc.target, v)
		}
	}
}

// A service spec that gives tasks host access is refused on a host that did not
// opt in, whichever write route it arrives on, and never reaches the engine.
func TestHostPolicyRefusalIsA403WithTheTypedMessageAndTheHostPolicyReason(t *testing.T) {
	for _, tc := range []struct{ name, method, target, wantMsg string }{
		{"an update", "POST", "/services/web/update?version=3", "agent_policy_denied: host_policy: POST /services/web/update"},
		{"a create", "POST", "/services/create", "agent_policy_denied: host_policy: POST /services/create"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := newFakeSocket(t)
			var reasons []string
			h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
			spec := sampleSpec()
			spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/sock"}}
			resp := do(t, h, postJSON(tc.method, tc.target, enc(t, spec)))
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			var msg struct{ Message string }
			if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil || msg.Message != tc.wantMsg {
				t.Fatalf("message = %q (%v), want %q", msg.Message, err, tc.wantMsg)
			}
			if !slices.Equal(reasons, []string{"host_policy"}) {
				t.Errorf("onDenied reasons = %v, want [host_policy]", reasons)
			}
			if got := sock.requests(); len(got) != 0 {
				t.Fatalf("a refused spec reached the engine: %v", got)
			}
		})
	}
}

func TestHostPolicyOptInForwardsTheSameSpec(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{AllowPrivilegedServices: true}, nil)
	spec := sampleSpec()
	spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/", Target: "/host"}}
	spec.TaskTemplate.ContainerSpec.CapabilityAdd = []string{"SYS_ADMIN"}
	resp := do(t, h, postJSON("POST", "/services/web/update?version=3", enc(t, spec)))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := sock.requests(); !slices.Equal(got, []string{"POST /services/web/update?version=3"}) {
		t.Fatalf("the engine saw %v", got)
	}
}

// The create-then-mount sequence (S2-CR-01): the service that mounts a volume by name
// carries no host path for escalations() to judge, so the volume create is where the
// host access is refused. With the volume never made, the mount has nothing of the host.
func TestHostPolicyAVolumeThatMountsTheHostIsRefusedSoAServiceCannotMountItByName(t *testing.T) {
	bind := volume.CreateRequest{
		Name: "hostroot", Driver: "local",
		DriverOpts: map[string]string{"type": "none", "o": "bind", "device": "/"},
	}
	plugin := volume.CreateRequest{Name: "plug", Driver: "rexray/ebs"}
	for _, tc := range []struct {
		name string
		req  volume.CreateRequest
	}{
		{"a local volume that binds the host root", bind},
		{"a volume of a plugin driver", plugin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := newFakeSocket(t)
			var reasons []string
			h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
			resp := do(t, h, postJSON("POST", "/volumes/create", enc(t, tc.req)))
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			var msg struct{ Message string }
			if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil || msg.Message != "agent_policy_denied: host_policy: POST /volumes/create" {
				t.Fatalf("message = %q (%v)", msg.Message, err)
			}
			if !slices.Equal(reasons, []string{"host_policy"}) {
				t.Errorf("onDenied reasons = %v, want [host_policy]", reasons)
			}
			if got := sock.requests(); len(got) != 0 {
				t.Fatalf("a refused volume create reached the engine: %v", got)
			}
		})
	}

	t.Run("the service that mounts the name afterwards is judged by its own spec", func(t *testing.T) {
		sock := newFakeSocket(t)
		h := Handler(sock.path, Policy{}, nil)
		spec := sampleSpec()
		spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "hostroot", Target: "/host"}}
		resp := do(t, h, postJSON("POST", "/services/create", enc(t, spec)))
		_ = resp.Body.Close()
		// The guard is the refused volume create above: the volume was never made, so
		// the engine has nothing of the host to mount under this name.
		if got := sock.requests(); !slices.Contains(got, "POST /services/create") {
			t.Fatalf("the engine saw %v, want the service create", got)
		}
	})

	t.Run("the opt-in forwards the bind volume create", func(t *testing.T) {
		sock := newFakeSocket(t)
		h := Handler(sock.path, Policy{AllowPrivilegedServices: true}, nil)
		resp := do(t, h, postJSON("POST", "/volumes/create", enc(t, bind)))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200", resp.StatusCode)
		}
		if got := sock.requests(); !slices.Equal(got, []string{"POST /volumes/create"}) {
			t.Fatalf("the engine saw %v", got)
		}
	})
}

// A volume that already exists (made outside Coaming, or while the host had opted in)
// is mounted as it is, so the agent asks its own engine what backs a name a service
// mounts (S2-CR-01, plan 06-44).
func TestHostPolicyAServiceCannotMountAPreExistingVolumeThatMountsTheHost(t *testing.T) {
	const hostroot = `{"Name":"hostroot","Driver":"local","Options":{"type":"none","o":"bind","device":"/"},"Scope":"local"}`
	spec := sampleSpec()
	spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "hostroot", Target: "/host"}}
	for _, tc := range []struct{ name, target, wantMsg string }{
		{"a create", "/services/create", "agent_policy_denied: host_policy: POST /services/create"},
		{"an update", "/services/web/update?version=3", "agent_policy_denied: host_policy: POST /services/web/update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := newFakeSocket(t)
			sock.answers = map[string]string{"/volumes/hostroot": hostroot}
			var reasons []string
			h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
			resp := do(t, h, postJSON("POST", tc.target, enc(t, spec)))
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
			var msg struct{ Message string }
			if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil || msg.Message != tc.wantMsg {
				t.Fatalf("message = %q (%v), want %q", msg.Message, err, tc.wantMsg)
			}
			if !slices.Equal(reasons, []string{"host_policy"}) {
				t.Errorf("onDenied reasons = %v, want [host_policy]", reasons)
			}
			if got := sock.requests(); !slices.Equal(got, []string{"GET /volumes/hostroot"}) {
				t.Fatalf("the engine saw %v, want the one volume lookup", got)
			}
		})
	}

	t.Run("the opt-in forwards it without a lookup", func(t *testing.T) {
		sock := newFakeSocket(t)
		sock.answers = map[string]string{"/volumes/hostroot": hostroot}
		h := Handler(sock.path, Policy{AllowPrivilegedServices: true}, nil)
		resp := do(t, h, postJSON("POST", "/services/create", enc(t, spec)))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200", resp.StatusCode)
		}
		if got := sock.requests(); !slices.Equal(got, []string{"POST /services/create"}) {
			t.Fatalf("the engine saw %v", got)
		}
	})
}

// Docker mounts an existing volume as it is, so unlike the network lookup a volume
// lookup that cannot answer still stops the write. It is an engine fault and not host
// access, so it is answered like any request the engine does not answer, and a person
// is not told to lower the host's security setting for it (review WR-02).
func TestHostPolicyAVolumeLookupThatFailsIsAnEngineFaultNotHostAccess(t *testing.T) {
	sock := newFakeSocket(t)
	sock.answers = map[string]string{"/volumes/data": `{"message":"boom"}`}
	sock.status = map[string]int{"/volumes/data": http.StatusInternalServerError}
	var reasons []string
	h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
	spec := sampleSpec()
	spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "data", Target: "/data"}}
	resp := do(t, h, postJSON("POST", "/services/create", enc(t, spec)))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || len(reasons) != 0 {
		t.Fatalf("status %d reasons %v, want 502 and no denial reported", resp.StatusCode, reasons)
	}
	if !strings.Contains(string(body), "agent_engine_unreachable: the Docker engine did not answer a volume lookup") {
		t.Fatalf("body %s does not name the engine fault", body)
	}
	if got := sock.requests(); !slices.Equal(got, []string{"GET /volumes/data"}) {
		t.Fatalf("the engine saw %v, want the one volume lookup", got)
	}

	t.Run("a real host-access finding is still named as such", func(t *testing.T) {
		sock := newFakeSocket(t)
		sock.answers = map[string]string{"/volumes/data": `{"message":"boom"}`}
		sock.status = map[string]int{"/volumes/data": http.StatusInternalServerError}
		var reasons []string
		h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
		spec := sampleSpec()
		spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{
			{Type: mount.TypeVolume, Source: "data", Target: "/data"},
			{Type: mount.TypeBind, Source: "/", Target: "/host"},
		}
		resp := do(t, h, postJSON("POST", "/services/create", enc(t, spec)))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !slices.Equal(reasons, []string{"host_policy"}) {
			t.Fatalf("status %d reasons %v, want 403 host_policy", resp.StatusCode, reasons)
		}
	})
}

// A volume the manager's engine does not have is created by Docker from the mount's
// own options, so those options are what is judged.
func TestHostPolicyANamedVolumeTheEngineDoesNotHaveIsJudgedByTheMountsOwnOptions(t *testing.T) {
	mountWith := func(opts map[string]string) swarm.ServiceSpec {
		spec := sampleSpec()
		spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{
			Type: mount.TypeVolume, Source: "fresh", Target: "/data",
			VolumeOptions: &mount.VolumeOptions{DriverConfig: &mount.Driver{Name: "local", Options: opts}},
		}}
		return spec
	}

	t.Run("an nfs share is forwarded", func(t *testing.T) {
		sock := newFakeSocket(t)
		h := Handler(sock.path, Policy{}, nil)
		spec := mountWith(map[string]string{"type": "nfs", "o": "addr=10.0.0.5,rw", "device": ":/export"})
		resp := do(t, h, postJSON("POST", "/services/create", enc(t, spec)))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200", resp.StatusCode)
		}
		if got := sock.requests(); !slices.Equal(got, []string{"GET /volumes/fresh", "POST /services/create"}) {
			t.Fatalf("the engine saw %v", got)
		}
	})

	t.Run("a bind of /etc is refused", func(t *testing.T) {
		sock := newFakeSocket(t)
		var reasons []string
		h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
		spec := mountWith(map[string]string{"type": "none", "o": "bind", "device": "/etc"})
		resp := do(t, h, postJSON("POST", "/services/create", enc(t, spec)))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !slices.Equal(reasons, []string{"host_policy"}) {
			t.Fatalf("status %d reasons %v, want 403 host_policy", resp.StatusCode, reasons)
		}
		for _, r := range sock.requests() {
			if strings.HasPrefix(r, "POST ") {
				t.Fatalf("a refused spec reached the engine: %v", sock.requests())
			}
		}
	})
}

func TestHostPolicyAHostNetworkNamedByIDIsFoundOnTheAgentsOwnEngine(t *testing.T) {
	const id = "7a1c0f3e9b2d4c5f8e6a1b0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e"
	sock := newFakeSocket(t)
	sock.answers = map[string]string{"/networks/" + id: `{"Name":"host","Driver":"host","Scope":"local"}`}
	var reasons []string
	h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
	spec := sampleSpec()
	spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: id}}
	resp := do(t, h, postJSON("POST", "/services/create", enc(t, spec)))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !slices.Equal(reasons, []string{"host_policy"}) {
		t.Fatalf("status %d reasons %v, want 403 host_policy", resp.StatusCode, reasons)
	}
	if got := sock.requests(); !slices.Equal(got, []string{"GET /networks/" + id}) {
		t.Fatalf("the engine saw %v, want the one network lookup", got)
	}
}

// A lookup that cannot answer is not an escalation: the engine refuses an unknown
// network itself when it gets the spec.
func TestHostPolicyALookupThatFailsDoesNotRefuseTheSpec(t *testing.T) {
	sock := newFakeSocket(t) // answers "OK", which is not a network
	h := Handler(sock.path, Policy{}, nil)
	spec := sampleSpec()
	spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "app_net"}}
	resp := do(t, h, postJSON("POST", "/services/create", enc(t, spec)))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := sock.requests(); !slices.Equal(got, []string{"GET /networks/app_net", "POST /services/create"}) {
		t.Fatalf("the engine saw %v", got)
	}
}

func TestServiceCreateKeepsTheRegistryCredentialAndRemoveIsForwarded(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	resp := do(t, h, postJSON("POST", "/v1.52/services/create", enc(t, sampleSpec()), "X-Registry-Auth: eyJ1c2VybmFtZSI6InUifQ==\r\n"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: status %d, want 200", resp.StatusCode)
	}
	resp = do(t, h, request("DELETE", "/services/web"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove: status %d, want 200", resp.StatusCode)
	}
	if got := sock.requests(); !slices.Equal(got, []string{"POST /v1.52/services/create", "DELETE /services/web"}) {
		t.Fatalf("the engine saw %v", got)
	}
	sock.mu.Lock()
	defer sock.mu.Unlock()
	if got := sock.headers[0].Get("X-Registry-Auth"); got != "eyJ1c2VybmFtZSI6InUifQ==" {
		t.Errorf("X-Registry-Auth = %q, want it kept on a create", got)
	}
}

func TestServiceRemoveWithAnyQueryKeyIsRefused(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	resp := do(t, h, request("DELETE", "/services/web?force=1"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("the engine saw %v", got)
	}
}

func TestAReadOnlyHostRefusesServiceCreateAndRemoveBeforeAnyValidator(t *testing.T) {
	sock := newFakeSocket(t)
	var reasons []string
	h := Handler(sock.path, Policy{ReadOnly: true, AllowPrivilegedServices: true}, func(_, _, reason string) { reasons = append(reasons, reason) })
	for _, raw := range []string{
		postJSON("POST", "/services/create", enc(t, sampleSpec())),
		postJSON("POST", "/services/create", `{not json`),
		request("DELETE", "/services/web"),
		postJSON("POST", "/services/web/update?version=1", enc(t, sampleSpec())),
	} {
		resp := do(t, h, raw)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status %d, want 403 for %.40q", resp.StatusCode, raw)
		}
	}
	if !slices.Equal(reasons, []string{"read_only_flag", "read_only_flag", "read_only_flag", "read_only_flag"}) {
		t.Errorf("reasons = %v", reasons)
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("the engine saw %v", got)
	}
}

// writesSeen is the requests the engine got that change something, in order; the lookups a
// policy check makes before a write are reads and are left out.
func (f *fakeSocket) writesSeen() []string {
	var out []string
	for _, r := range f.requests() {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

func TestAContainerCreateReachesTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	body := enc(t, sdkContainerCreate())
	resp := do(t, h, postJSON("POST", "/v1.52/containers/create?name=shop-web-1&platform=linux%2Famd64", body,
		"X-Registry-Auth: eyJ1c2VybmFtZSI6InUifQ==\r\n"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := sock.writesSeen(); !slices.Equal(got, []string{"POST /v1.52/containers/create?name=shop-web-1&platform=linux%2Famd64"}) {
		t.Fatalf("the engine saw %v", got)
	}
	bodies := sock.bodiesSeen()
	if len(bodies) == 0 || string(bodies[len(bodies)-1]) != body {
		t.Fatalf("the engine received %s, want the rebuilt body %s", bodies, body)
	}
	sock.mu.Lock()
	defer sock.mu.Unlock()
	last := sock.headers[len(sock.headers)-1]
	if ct := last.Get("Content-Type"); ct != "application/json" {
		t.Errorf("the engine saw the content type %q", ct)
	}
	if got := last.Get("X-Registry-Auth"); got != "" {
		t.Errorf("a container create carried a registry credential to the engine: %q", got)
	}
}

func TestAReadOnlyHostRefusesContainerCreate(t *testing.T) {
	sock := newFakeSocket(t)
	var reasons []string
	h := Handler(sock.path, Policy{ReadOnly: true, AllowPrivilegedServices: true}, func(_, _, reason string) { reasons = append(reasons, reason) })
	for _, raw := range []string{
		postJSON("POST", "/containers/create?name=web", enc(t, sdkContainerCreate())),
		postJSON("POST", "/containers/create", `{not json`),
	} {
		resp := do(t, h, raw)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status %d, want 403 for %.40q", resp.StatusCode, raw)
		}
	}
	if !slices.Equal(reasons, []string{"read_only_flag", "read_only_flag"}) {
		t.Errorf("reasons = %v", reasons)
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("the engine saw %v", got)
	}
}

// archivePath is the request the Compose SDK's CopyToContainer sends for a stack file secret,
// without a copyUIDGID key (the secret names no owner).
const archivePath = "/v1.52/containers/web-1/archive?noOverwriteDirNonDir=true&path=%2F"

// putTar is a PUT with a tar body of the right length.
func putTar(target string, raw []byte, headers ...string) string {
	headers = append([]string{"Content-Type: application/x-tar\r\n", "Content-Length: " + strconv.Itoa(len(raw)) + "\r\n"}, headers...)
	return request(http.MethodPut, target, headers...) + string(raw)
}

// containerAnswer is what an engine answers to the inspect of a container: the labels and the
// running flag are the two things the agent reads.
func containerAnswer(running bool, labels map[string]string) string {
	b, _ := json.Marshal(map[string]any{
		"Id": "0123456789ab", "State": map[string]any{"Running": running},
		"Config": map[string]any{"Image": "nginx:1.27", "Labels": labels},
	})
	return string(b)
}

var composeLabels = map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "web"}

func TestASecretFileIsInjectedIntoACreatedComposeContainer(t *testing.T) {
	sock := newFakeSocket(t)
	sock.answers = map[string]string{"/containers/web-1/json": containerAnswer(false, composeLabels)}
	var reasons []string
	h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
	raw := sdkArchive(t, "/run/secrets/tok", "s3cret-value", 0o444)
	resp := do(t, h, putTar(archivePath, raw))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(reasons) != 0 {
		t.Fatalf("status %d, reasons %v, want 200 and no refusal", resp.StatusCode, reasons)
	}
	if got := sock.requests(); !slices.Equal(got, []string{"GET /containers/web-1/json", "PUT " + archivePath}) {
		t.Fatalf("the engine saw %v, want the container lookup and then the write", got)
	}
	bodies := sock.bodiesSeen()
	if len(bodies) != 2 || !bytes.Equal(bodies[1], raw) {
		t.Fatalf("the engine did not receive the tar byte for byte")
	}
	sock.mu.Lock()
	defer sock.mu.Unlock()
	if ct := sock.headers[1].Get("Content-Type"); ct != "application/x-tar" {
		t.Errorf("the engine saw the content type %q, want application/x-tar", ct)
	}
}

func TestArchiveIntoARunningContainerIsRefused(t *testing.T) {
	sock := newFakeSocket(t)
	sock.answers = map[string]string{"/containers/web-1/json": containerAnswer(true, composeLabels)}
	var reasons []string
	h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
	resp := do(t, h, putTar(archivePath, sdkArchive(t, "/run/secrets/tok", "x", 0o444)))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "agent_policy_denied: PUT /v1.52/containers/web-1/archive") {
		t.Fatalf("status %d body %s, want a typed 403", resp.StatusCode, body)
	}
	if !slices.Equal(reasons, []string{"allowlist"}) {
		t.Errorf("reasons = %v", reasons)
	}
	if got := sock.writesSeen(); len(got) != 0 {
		t.Fatalf("a write reached the engine: %v", got)
	}
}

func TestArchiveIntoAContainerComposeDidNotCreateIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		answer string
		status int
	}{
		"no compose label":         {answer: containerAnswer(false, map[string]string{"app": "x"})},
		"no label at all":          {answer: containerAnswer(false, nil)},
		"a container that is gone": {answer: `{"message":"No such container: web-1"}`, status: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			sock := newFakeSocket(t)
			sock.answers = map[string]string{"/containers/web-1/json": tc.answer}
			if tc.status != 0 {
				sock.status = map[string]int{"/containers/web-1/json": tc.status}
			}
			var reasons []string
			h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
			resp := do(t, h, putTar(archivePath, sdkArchive(t, "/run/secrets/tok", "x", 0o444)))
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || !slices.Equal(reasons, []string{"allowlist"}) {
				t.Fatalf("status %d reasons %v, want 403 allowlist", resp.StatusCode, reasons)
			}
			if got := sock.writesSeen(); len(got) != 0 {
				t.Fatalf("a write reached the engine: %v", got)
			}
		})
	}
}

// An engine that cannot say what a container is gives no permission: the answer is the 502 of an
// engine fault (the 06-50 rule), nothing is reported as a policy denial and nothing is forwarded.
func TestArchiveLookupThatFailsIsAnEngineFaultNotAPermission(t *testing.T) {
	for name, tc := range map[string]struct {
		answer string
		status int
	}{
		"a server error":      {answer: `{"message":"boom"}`, status: http.StatusInternalServerError},
		"an unreadable reply": {answer: `not json`},
	} {
		t.Run(name, func(t *testing.T) {
			sock := newFakeSocket(t)
			sock.answers = map[string]string{"/containers/web-1/json": tc.answer}
			if tc.status != 0 {
				sock.status = map[string]int{"/containers/web-1/json": tc.status}
			}
			var reasons []string
			h := Handler(sock.path, Policy{}, func(_, _, reason string) { reasons = append(reasons, reason) })
			resp := do(t, h, putTar(archivePath, sdkArchive(t, "/run/secrets/tok", "x", 0o444)))
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadGateway || len(reasons) != 0 {
				t.Fatalf("status %d reasons %v, want 502 and no denial reported", resp.StatusCode, reasons)
			}
			if !strings.Contains(string(body), "agent_engine_unreachable: the Docker engine did not answer a container lookup") {
				t.Fatalf("body %s does not name the engine fault", body)
			}
			if got := sock.writesSeen(); len(got) != 0 {
				t.Fatalf("a write reached the engine: %v", got)
			}
		})
	}
}

// A bad tar is refused before the engine is asked anything.
func TestAnInvalidArchiveNeverReachesTheEngineNorItsLookup(t *testing.T) {
	sock := newFakeSocket(t)
	sock.answers = map[string]string{"/containers/web-1/json": containerAnswer(false, composeLabels)}
	h := Handler(sock.path, Policy{}, nil)
	for name, raw := range map[string][]byte{
		"a symlink":      tarOf(t, tarEntry{hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: "/run/secrets/tok", Linkname: "/etc/shadow", Mode: 0o777, ModTime: archiveModTime}}),
		"a dot-dot name": sdkArchive(t, "../etc/passwd", "x", 0o444),
		"a setuid file":  sdkArchive(t, "/bin/x", "x", 0o4755),
		"garbage":        []byte("not a tar"),
	} {
		resp := do(t, h, putTar(archivePath, raw))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, resp.StatusCode)
		}
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("the engine saw %v", got)
	}
}

func TestAReadOnlyHostRefusesArchiveBeforeAnyBodyIsRead(t *testing.T) {
	sock := newFakeSocket(t)
	sock.answers = map[string]string{"/containers/web-1/json": containerAnswer(false, composeLabels)}
	var reasons []string
	h := Handler(sock.path, Policy{ReadOnly: true, AllowPrivilegedServices: true}, func(_, _, reason string) { reasons = append(reasons, reason) })
	for _, raw := range [][]byte{sdkArchive(t, "/run/secrets/tok", "x", 0o444), []byte("not a tar")} {
		resp := do(t, h, putTar(archivePath, raw))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status %d, want 403", resp.StatusCode)
		}
	}
	if !slices.Equal(reasons, []string{"read_only_flag", "read_only_flag"}) {
		t.Errorf("reasons = %v", reasons)
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("the engine saw %v", got)
	}
}

func TestAContainerRenameReachesTheEngineWithNoBody(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	resp := do(t, h, request("POST", "/v1.52/containers/0123456789ab/rename?name=shop-web-1"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := sock.writesSeen(); !slices.Equal(got, []string{"POST /v1.52/containers/0123456789ab/rename?name=shop-web-1"}) {
		t.Fatalf("the engine saw %v", got)
	}
	for _, raw := range []string{
		request("POST", "/containers/0123456789ab/rename"),
		request("POST", "/containers/0123456789ab/rename?name=a%2Fb"),
		postJSON("POST", "/containers/0123456789ab/rename?name=web", `{"name":"x"}`),
	} {
		resp := do(t, h, raw)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status %d, want 403 for %.60q", resp.StatusCode, raw)
		}
	}
	if got := sock.writesSeen(); len(got) != 1 {
		t.Fatalf("a refused rename reached the engine: %v", got)
	}
}

func TestAnImageInspectWithTheSDKsKeysReachesTheEngine(t *testing.T) {
	sock := newFakeSocket(t)
	h := Handler(sock.path, Policy{}, nil)
	target := "/v1.52/images/nginx:1.27/json?manifests=1&platform=%7B%22architecture%22%3A%22amd64%22%2C%22os%22%3A%22linux%22%7D"
	resp := do(t, h, get(target))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := sock.requests(); !slices.Equal(got, []string{"GET " + target}) {
		t.Fatalf("the engine saw %v", got)
	}
}

func TestAReadOnlyHostRefusesContainerRename(t *testing.T) {
	sock := newFakeSocket(t)
	var reasons []string
	h := Handler(sock.path, Policy{ReadOnly: true}, func(_, _, reason string) { reasons = append(reasons, reason) })
	resp := do(t, h, request("POST", "/containers/web-1/rename?name=web-2"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !slices.Equal(reasons, []string{"read_only_flag"}) {
		t.Fatalf("status %d reasons %v, want 403 read_only_flag", resp.StatusCode, reasons)
	}
	if got := sock.requests(); len(got) != 0 {
		t.Fatalf("the engine saw %v", got)
	}
}
