// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package httpx

import (
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"testing"
)

// The origin set (D-07, SELF-01): a mutation is accepted from the public URL's origin and,
// while TierPlan's own route is on, from the route host's origin, which the server derives
// from the stored claim. Nothing a request carries can add a member.

const (
	directOrigin = "https://tierplan.example.org:7480"
	routeOrigin  = "https://tp.apps.example.org"
)

// extraSource is a settable ExtraOrigins, as the server's holder will be: read on every request.
type extraSource struct {
	mu   sync.Mutex
	list []string
}

func (s *extraSource) set(list ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = slices.Clone(list)
}

func (s *extraSource) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.list)
}

// postFrom posts to the public route with Origin set to origin plus any further mods.
func postFrom(h http.Handler, origin string, mods ...func(*http.Request)) int {
	all := append([]func(*http.Request){header("Origin", origin)}, mods...)
	return do(h, http.MethodPost, "/api/public", all...).Code
}

func TestExtraOriginsWidenOnlyWhatTheSourceSays(t *testing.T) {
	src := &extraSource{}
	f := newFixture(t, func(o *Options) {
		o.PublicOrigin = func() string { return directOrigin }
		o.ExtraOrigins = src.get
	})
	h := f.r.Handler()

	if got := postFrom(h, directOrigin); got != http.StatusOK {
		t.Fatalf("the public origin = %d, want 200", got)
	}
	if got := postFrom(h, routeOrigin); got != http.StatusForbidden {
		t.Fatalf("the route origin with no route = %d, want 403", got)
	}

	src.set(routeOrigin)
	if got := postFrom(h, routeOrigin); got != http.StatusOK {
		t.Fatalf("the route origin while the source names it = %d, want 200", got)
	}
	// The direct origin keeps working while the route is on (SELF-01, HLD 4.8).
	if got := postFrom(h, directOrigin); got != http.StatusOK {
		t.Fatalf("the public origin while the route is on = %d, want 200", got)
	}

	// Read on every request: turning the route off takes the member away at once.
	src.set()
	if got := postFrom(h, routeOrigin); got != http.StatusForbidden {
		t.Fatalf("the route origin after the route is off = %d, want 403", got)
	}
}

func TestOriginSetEmptyCases(t *testing.T) {
	t.Run("nil source: the public origin only", func(t *testing.T) {
		f := newFixture(t, func(o *Options) { o.PublicOrigin = func() string { return directOrigin } })
		h := f.r.Handler()
		if got := postFrom(h, directOrigin); got != http.StatusOK {
			t.Errorf("public origin = %d, want 200", got)
		}
		if got := postFrom(h, routeOrigin); got != http.StatusForbidden {
			t.Errorf("route origin = %d, want 403", got)
		}
	})
	t.Run("empty and unparsable entries add nothing", func(t *testing.T) {
		f := newFixture(t, func(o *Options) {
			o.PublicOrigin = func() string { return directOrigin }
			o.ExtraOrigins = func() []string { return []string{"", "not a url", "ftp://tp.apps.example.org", "tp.apps.example.org"} }
		})
		h := f.r.Handler()
		if got := postFrom(h, directOrigin); got != http.StatusOK {
			t.Errorf("public origin = %d, want 200", got)
		}
		for _, o := range []string{routeOrigin, "http://tp.apps.example.org", "ftp://tp.apps.example.org", "null"} {
			if got := postFrom(h, o); got != http.StatusForbidden {
				t.Errorf("%s = %d, want 403", o, got)
			}
		}
	})
	t.Run("no public URL and one extra origin", func(t *testing.T) {
		f := newFixture(t, func(o *Options) {
			o.PublicOrigin = func() string { return "" }
			o.ExtraOrigins = func() []string { return []string{routeOrigin} }
		})
		h := f.r.Handler()
		own := func(r *http.Request) { r.Host = "box.lan:8080" }
		if got := postFrom(h, routeOrigin, own); got != http.StatusOK {
			t.Errorf("the extra origin = %d, want 200", got)
		}
		for _, o := range []string{directOrigin, "https://evil.example", "http://tp.apps.example.org"} {
			if got := postFrom(h, o, own); got != http.StatusForbidden {
				t.Errorf("%s = %d, want 403", o, got)
			}
		}
		// Before setup the request's own origin is the expected one (D-04); the extra origin
		// does not take its place.
		if got := postFrom(h, "http://box.lan:8080", own); got != http.StatusOK {
			t.Errorf("the request's own origin before setup = %d, want 200", got)
		}
	})
}

func TestOriginSetAdjacency(t *testing.T) {
	const host = "https://tp.apps.example.org"
	f := newFixture(t, func(o *Options) {
		o.PublicOrigin = func() string { return host }
		o.ExtraOrigins = func() []string { return []string{host, "HTTPS://TP.Apps.Example.Org:443"} }
	})
	if set := f.r.originSet(host); !slices.Equal(set, []string{host}) {
		t.Fatalf("set = %q, want the one member %q", set, host)
	}
	h := f.r.Handler()
	cases := []struct {
		origin string
		want   int
	}{
		{host, http.StatusOK},
		{"https://TP.APPS.example.org", http.StatusOK},
		{"https://tp.apps.example.org:443", http.StatusOK},
		{"https://tp.apps.example.org:8443", http.StatusForbidden},
		{"http://tp.apps.example.org", http.StatusForbidden},
		{"http://tp.apps.example.org:443", http.StatusForbidden},
		{"https://apps.example.org", http.StatusForbidden},
		{"https://x.tp.apps.example.org", http.StatusForbidden},
	}
	for _, tc := range cases {
		if got := postFrom(h, tc.origin); got != tc.want {
			t.Errorf("%s = %d, want %d", tc.origin, got, tc.want)
		}
	}
}

func TestOriginSetOrderIrrelevant(t *testing.T) {
	const other = "https://second.example.org"
	lists := [][]string{
		{routeOrigin, other},
		{other, routeOrigin},
		{other, routeOrigin, other, routeOrigin},
		{"HTTPS://tp.apps.example.org:443", other, routeOrigin},
	}
	origins := []string{directOrigin, routeOrigin, other, "https://evil.example", "http://tp.apps.example.org"}
	var first []int
	for i, list := range lists {
		f := newFixture(t, func(o *Options) {
			o.PublicOrigin = func() string { return directOrigin }
			o.ExtraOrigins = func() []string { return list }
		})
		set := f.r.originSet(OriginOf(directOrigin))
		if len(set) != 3 {
			t.Errorf("list %q: set = %q, want three members", list, set)
		}
		h := f.r.Handler()
		answers := make([]int, 0, len(origins))
		for _, o := range origins {
			answers = append(answers, postFrom(h, o))
		}
		if i == 0 {
			first = answers
			if want := []int{200, 200, 200, 403, 403}; !slices.Equal(answers, want) {
				t.Fatalf("answers = %v, want %v", answers, want)
			}
			continue
		}
		if !slices.Equal(answers, first) {
			t.Errorf("list %q: answers = %v, want the same as for %q: %v", list, answers, lists[0], first)
		}
	}
}

func TestHeadersCannotWidenTheOriginSet(t *testing.T) {
	src := &extraSource{}
	f := newFixture(t, func(o *Options) {
		o.PublicOrigin = func() string { return directOrigin }
		o.ExtraOrigins = src.get
		// A peer whose forwarded headers are believed: even it cannot add a member.
		o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	})
	h := f.r.Handler()
	viaProxy := func(r *http.Request) { r.RemoteAddr = "10.1.2.3:4000" }
	naming := map[string][]func(*http.Request){
		"X-Forwarded-Host": {header("X-Forwarded-Host", "tp.apps.example.org"), header("X-Forwarded-Proto", "https")},
		"Host":             {func(r *http.Request) { r.Host = "tp.apps.example.org" }},
		"Forwarded":        {header("Forwarded", "host=tp.apps.example.org;proto=https")},
		"Referer":          {header("Referer", routeOrigin+"/stacks")},
		"all of them": {
			header("X-Forwarded-Host", "tp.apps.example.org"), header("X-Forwarded-Proto", "https"),
			func(r *http.Request) { r.Host = "tp.apps.example.org" },
			header("Forwarded", "host=tp.apps.example.org;proto=https"), header("Referer", routeOrigin+"/"),
		},
	}
	for name, mods := range naming {
		for _, peer := range []func(*http.Request){func(*http.Request) {}, viaProxy} {
			if got := postFrom(h, routeOrigin, append(mods, peer)...); got != http.StatusForbidden {
				t.Errorf("%s naming the route host with no route = %d, want 403", name, got)
			}
		}
	}

	// While the route is on, a host it does not claim stays refused, whatever the headers say.
	src.set(routeOrigin)
	const unclaimed = "https://other.apps.example.org"
	for name, mods := range map[string][]func(*http.Request){
		"bare":             nil,
		"X-Forwarded-Host": {header("X-Forwarded-Host", "other.apps.example.org"), viaProxy},
		"Host":             {func(r *http.Request) { r.Host = "other.apps.example.org" }},
		"Referer":          {header("Referer", unclaimed+"/")},
	} {
		if got := postFrom(h, unclaimed, mods...); got != http.StatusForbidden {
			t.Errorf("an unclaimed host (%s) while the route is on = %d, want 403", name, got)
		}
	}
	if got := postFrom(h, routeOrigin, viaProxy); got != http.StatusOK {
		t.Errorf("the claimed route origin through the proxy = %d, want 200", got)
	}
}
