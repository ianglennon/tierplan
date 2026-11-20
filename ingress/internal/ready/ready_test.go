// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ready

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// pingServer is a stand-in for Traefik's ping entrypoint. Its status is switched by the test.
type pingServer struct {
	srv    *httptest.Server
	status atomic.Int32
}

func newPingServer(t *testing.T) *pingServer {
	t.Helper()
	p := &pingServer{}
	p.status.Store(http.StatusOK)
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(p.status.Load()))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// readyServer serves the real handler over a real listener against the given ping target.
func readyServer(t *testing.T, st *State, ping Pinger) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(st, ping))
	t.Cleanup(srv.Close)
	return srv
}

// do sends one request and returns the status, the body and the response headers.
func do(t *testing.T, method, url string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	// DisableKeepAlives keeps every request on its own connection, and the transport is the client's own, so
	// nothing is shared between cases.
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body), resp.Header
}

// assertBare fails unless the response carries nothing but what net/http adds to an empty response.
func assertBare(t *testing.T, body string, h http.Header) {
	t.Helper()
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
	for k := range h {
		switch k {
		case "Date", "Content-Length":
		default:
			t.Errorf("unexpected response header %q: %v", k, h[k])
		}
	}
}

func TestReadyIs503UntilABundleIsApplied(t *testing.T) {
	ping := newPingServer(t)
	st := &State{}
	srv := readyServer(t, st, NewPinger(http.DefaultClient, ping.srv.URL))

	code, body, h := do(t, http.MethodGet, srv.URL+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("no bundle applied: status = %d, want 503", code)
	}
	assertBare(t, body, h)

	st.SetApplied(true)
	code, body, h = do(t, http.MethodGet, srv.URL+"/readyz")
	if code != http.StatusOK {
		t.Fatalf("bundle applied and ping 200: status = %d, want 200", code)
	}
	assertBare(t, body, h)
	if ct := h.Get("Content-Type"); ct != "" {
		t.Errorf("Content-Type = %q, want none", ct)
	}

	st.SetApplied(false)
	if code, _, _ := do(t, http.MethodGet, srv.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("applied cleared: status = %d, want 503", code)
	}
}

func TestReadyIs503WhenTraefikDoesNotAnswer200(t *testing.T) {
	ping := newPingServer(t)
	st := &State{}
	st.SetApplied(true)
	srv := readyServer(t, st, NewPinger(http.DefaultClient, ping.srv.URL))

	if code, _, _ := do(t, http.MethodGet, srv.URL+"/readyz"); code != http.StatusOK {
		t.Fatalf("ping 200: status = %d, want 200", code)
	}

	ping.status.Store(http.StatusServiceUnavailable) // Traefik terminating
	code, body, h := do(t, http.MethodGet, srv.URL+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("ping 503: status = %d, want 503", code)
	}
	assertBare(t, body, h)

	ping.status.Store(http.StatusNoContent) // anything but 200 is not "answering"
	if code, _, _ := do(t, http.MethodGet, srv.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("ping 204: status = %d, want 503", code)
	}

	ping.status.Store(http.StatusOK)
	if code, _, _ := do(t, http.MethodGet, srv.URL+"/readyz"); code != http.StatusOK {
		t.Fatalf("ping 200 again: status = %d, want 200", code)
	}

	ping.srv.Close() // connection refused
	if code, _, _ := do(t, http.MethodGet, srv.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("ping refused: status = %d, want 503", code)
	}
}

func TestReadyIs503WhenThePingOutlastsItsTimeout(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(blocked.Close)
	t.Cleanup(func() { close(release) })

	st := &State{}
	st.SetApplied(true)
	// A hung Traefik must not hang the endpoint: the pinger's own bound turns it into a 503.
	srv := readyServer(t, st, NewPinger(blocked.Client(), blocked.URL))

	code, body, h := do(t, http.MethodGet, srv.URL+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("ping hung: status = %d, want 503", code)
	}
	assertBare(t, body, h)
	select {
	case <-entered:
	default:
		t.Fatal("the pinger never reached the ping server")
	}
}

func TestReadyHeadMirrorsGet(t *testing.T) {
	ping := newPingServer(t)
	st := &State{}
	srv := readyServer(t, st, NewPinger(http.DefaultClient, ping.srv.URL))

	code, body, h := do(t, http.MethodHead, srv.URL+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("HEAD, no bundle: status = %d, want 503", code)
	}
	assertBare(t, body, h)

	st.SetApplied(true)
	code, body, h = do(t, http.MethodHead, srv.URL+"/readyz")
	if code != http.StatusOK {
		t.Fatalf("HEAD, bundle applied: status = %d, want 200", code)
	}
	assertBare(t, body, h)
}

func TestReadyAnswersOnlyTheExactPathAndMethods(t *testing.T) {
	ping := newPingServer(t)
	st := &State{}
	st.SetApplied(true)
	srv := readyServer(t, st, NewPinger(http.DefaultClient, ping.srv.URL))

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/readyz", http.StatusMethodNotAllowed},
		{http.MethodPut, "/readyz", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/readyz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/readyz/", http.StatusNotFound},
		{http.MethodGet, "//readyz", http.StatusNotFound},
		{http.MethodGet, "/metrics", http.StatusNotFound},
		{http.MethodGet, "/readyz?x=1", http.StatusOK},
		{http.MethodPost, "/other", http.StatusNotFound},
	} {
		code, body, h := do(t, tc.method, srv.URL+tc.path)
		if code != tc.want {
			t.Errorf("%s %s: status = %d, want %d", tc.method, tc.path, code, tc.want)
		}
		assertBare(t, body, h)
		if h.Get("Location") != "" {
			t.Errorf("%s %s: redirect to %q, want none", tc.method, tc.path, h.Get("Location"))
		}
	}
}

func TestReadyDefaultPingURLComesFromTheProtoConstants(t *testing.T) {
	if got, want := DefaultPingURL(), "http://127.0.0.1:8082/ping"; got != want {
		t.Fatalf("DefaultPingURL = %q, want %q", got, want)
	}
}
