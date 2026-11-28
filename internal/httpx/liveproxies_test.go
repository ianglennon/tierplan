// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
)

// seen is what a handler behind the client-address middleware learned about one request.
type seen struct {
	ip        string
	trusted   bool
	onSelfNet bool
}

// derive sends one request from peer carrying xff through h and reports what the context says.
func derive(h func(http.Handler) http.Handler, peer, xff string) seen {
	var got seen
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = seen{
			ip:        ClientIP(r.Context()),
			trusted:   PeerTrusted(r.Context()),
			onSelfNet: PeerOnSelfNetwork(r.Context()),
		}
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = peer
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	h(inner).ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func TestLiveTrustedSetIsReadPerRequest(t *testing.T) {
	t.Parallel()
	static := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	live := func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("10.9.0.0/24")} }
	mw := ClientIPMiddlewareLive(static, live)

	cases := []struct {
		name string
		peer string
		want seen
	}{
		{"a peer on the live set", "10.9.0.5:4000", seen{ip: "203.0.113.7", trusted: true, onSelfNet: true}},
		{"a peer on the static list", "192.0.2.9:4000", seen{ip: "203.0.113.7", trusted: true, onSelfNet: false}},
		{"a peer in neither", "198.51.100.4:4000", seen{ip: "198.51.100.4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := derive(mw, tc.peer, "203.0.113.7")
			if got.ip != tc.want.ip || got.trusted != tc.want.trusted || got.onSelfNet != tc.want.onSelfNet {
				t.Errorf("peer %s: got %+v, want %+v", tc.peer, got, tc.want)
			}
		})
	}
}

func TestLiveTrustedSetRefresh(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		current []netip.Prefix
	)
	live := func() []netip.Prefix {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	set := func(p ...netip.Prefix) {
		mu.Lock()
		current = p
		mu.Unlock()
	}
	var builds atomic.Int32
	mw := clientIPMiddlewareLive(nil, live, func() { builds.Add(1) })

	if got := derive(mw, "10.9.0.5:4000", "203.0.113.7"); got.ip != "10.9.0.5" || got.trusted || got.onSelfNet {
		t.Fatalf("before the live set has a prefix: got %+v, want the peer and no trust", got)
	}
	if n := builds.Load(); n != 0 {
		t.Fatalf("builds with nothing live = %d, want 0", n)
	}

	set(netip.MustParsePrefix("10.9.0.0/24"))
	for i := range 3 {
		got := derive(mw, "10.9.0.5:4000", "203.0.113.7")
		if got.ip != "203.0.113.7" || !got.trusted || !got.onSelfNet {
			t.Fatalf("request %d after the refresh: got %+v, want the forwarded address, trusted, on the self network", i, got)
		}
	}
	if n := builds.Load(); n != 1 {
		t.Errorf("builds for one distinct slice = %d, want 1", n)
	}

	// An equal slice in a new backing array is not a change.
	set(netip.MustParsePrefix("10.9.0.0/24"))
	derive(mw, "10.9.0.5:4000", "203.0.113.7")
	if n := builds.Load(); n != 1 {
		t.Errorf("builds after an equal slice = %d, want 1", n)
	}

	// The set going away takes the trust with it.
	set()
	if got := derive(mw, "10.9.0.5:4000", "203.0.113.7"); got.ip != "10.9.0.5" || got.trusted {
		t.Errorf("after the live set emptied: got %+v, want the peer and no trust", got)
	}
	if n := builds.Load(); n != 2 {
		t.Errorf("builds after a second distinct slice = %d, want 2", n)
	}
}

func TestRouterWithoutLiveTrustedProxiesIsUnchanged(t *testing.T) {
	t.Parallel()
	static := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/32")}

	var viaRouter seen
	rt := New(Options{TrustedProxies: static})
	// A route must exist for chi to run the middleware chain at all.
	rt.Exempt(http.MethodGet, "/probe", func(_ http.ResponseWriter, r *http.Request) {
		viaRouter = seen{ip: ClientIP(r.Context()), trusted: PeerTrusted(r.Context()), onSelfNet: PeerOnSelfNetwork(r.Context())}
	})
	direct := ClientIPMiddleware(static)

	cases := []struct{ peer, xff string }{
		{"192.0.2.9:1", "203.0.113.7"},
		{"192.0.2.9:1", "203.0.113.7, 192.0.2.50"},
		{"192.0.2.9:1", "198.51.100.1, 203.0.113.7"},
		{"192.0.2.9:1", ""},
		{"[2001:db8::5]:1", "203.0.113.7"},
		{"198.51.100.4:1", "203.0.113.7"},
		{"10.9.0.5:1", "203.0.113.7"},
		{"not-an-address", "203.0.113.7"},
	}
	for _, tc := range cases {
		want := derive(direct, tc.peer, tc.xff)
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		req.RemoteAddr = tc.peer
		if tc.xff != "" {
			req.Header.Set("X-Forwarded-For", tc.xff)
		}
		viaRouter = seen{}
		rt.Handler().ServeHTTP(httptest.NewRecorder(), req)
		if viaRouter != want {
			t.Errorf("peer %s xff %q: router %+v, ClientIPMiddleware %+v", tc.peer, tc.xff, viaRouter, want)
		}
		if viaRouter.onSelfNet {
			t.Errorf("peer %s: a static-only router marked the peer as on the self network", tc.peer)
		}
	}
}

func TestRouterInstallsTheLiveTrustedProxies(t *testing.T) {
	t.Parallel()
	var got seen
	rt := New(Options{LiveTrustedProxies: func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("10.9.0.0/24")} }})
	rt.Exempt(http.MethodGet, "/probe", func(_ http.ResponseWriter, r *http.Request) {
		got = seen{ip: ClientIP(r.Context()), trusted: PeerTrusted(r.Context()), onSelfNet: PeerOnSelfNetwork(r.Context())}
	})
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.RemoteAddr = "10.9.0.5:1"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	rt.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if want := (seen{ip: "203.0.113.7", trusted: true, onSelfNet: true}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLiveTrustedSetRefreshUnderLoad(t *testing.T) {
	t.Parallel()
	const (
		workers  = 8
		requests = 200
		flips    = 100
	)
	subnet := []netip.Prefix{netip.MustParsePrefix("10.9.0.0/24")}
	var cur atomic.Pointer[[]netip.Prefix]
	empty := []netip.Prefix{}
	cur.Store(&empty)
	live := func() []netip.Prefix { return *cur.Load() }
	mw := ClientIPMiddlewareLive(nil, live)

	var wg sync.WaitGroup
	wrong := make(chan string, workers*requests)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range requests {
				got := derive(mw, "10.9.0.5:4000", "203.0.113.7")
				// Either derivation is right; a mix of the two is not.
				switch {
				case got.ip == "203.0.113.7" && got.trusted && got.onSelfNet:
				case got.ip == "10.9.0.5" && !got.trusted && !got.onSelfNet:
				default:
					wrong <- got.ip
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range flips {
			if i%2 == 0 {
				cur.Store(&subnet)
			} else {
				cur.Store(&empty)
			}
		}
	}()
	wg.Wait()
	close(wrong)
	for ip := range wrong {
		t.Errorf("an answer that is neither derivation: %q", ip)
	}
}
