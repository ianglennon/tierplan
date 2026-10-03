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
	h := Handler(sock.path)
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
	if got := len(sock.requests()); got != 11 {
		t.Errorf("the engine saw %d requests, want 11: %v", got, sock.requests())
	}
}

func TestEventsStreamIsFlushedAsItArrives(t *testing.T) {
	sock := newFakeSocket(t)
	resp := do(t, Handler(sock.path), get("/events")) //nolint:bodyclose // closing would drain an endless stream; the helper closes the connection
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
	h := Handler(sock.path)
	for _, tc := range []struct {
		name, raw, wantMessage string
	}{
		{"create a container", request("POST", "/containers/create", "Content-Length: 2\r\n") + "{}", "agent_policy_denied: POST /containers/create"},
		{"inspect a container", get("/containers/abc/json"), "agent_policy_denied: GET /containers/abc/json"},
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
	resp := do(t, Handler(sock.path), get("/containers/json?x="+strings.Repeat("a", maxQuery+1)))
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
	resp := do(t, Handler(sock.path), "GET /info?a=\x01 HTTP/1.1\r\nHost: docker\r\n\r\n")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want a refusal", resp.StatusCode)
	}
	if len(sock.requests()) != 0 {
		t.Fatal("the engine saw a request with a control character")
	}
}

func TestAnUnreachableEngineAnswers502NotAPolicyDenial(t *testing.T) {
	h := Handler(filepath.Join(t.TempDir(), "missing.sock"))
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
		ServeConn(server, Handler(filepath.Join(t.TempDir(), "x.sock")))
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
	go ServeConn(server, Handler(sock.path))
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
