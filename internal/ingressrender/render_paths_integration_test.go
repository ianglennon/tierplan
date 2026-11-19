// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
)

// Rendered-bundle path cases of the pinned Traefik (21-11, RTE-03, LBL-07, D-13, D-16, D-17). 21-S1
// proved how Traefik matches paths for hand-written routers; these prove the routers Render writes use
// that behaviour: a path route matches its own path segment only, the longest segment prefix wins, a
// strip route removes exactly its prefix, and a path no route claims is Traefik's 404.
//
// The backends are separate whoami containers aliased <stack>_<service> as the renderer names them. The
// serving backend is told by its `Hostname:` line and the forwarded target by its `GET <target>` line.
// whoami answers /api, /health, /data, /echo and /bench itself (D-30), so a probe at one of those
// without a strip route would not show the echo; the /api probes below always go through a strip route
// or reach a longer path.

const (
	// pathsHost, overlapHost, stripHost and unmatchedHost are one hostname per test.
	pathsHost     = "paths.test.local"
	overlapHost   = "overlap.test.local"
	stripHost     = "strip.test.local"
	unmatchedHost = "unmatched.test.local"
	secureHost    = "secure.test.local"
)

// echoOf is what whoami's echo says about the request it served: the container hostname, the request
// target of the request line ("GET <target> HTTP/1.1") and the headers by lower-cased name.
type echoOf struct {
	backend string
	target  string
	headers map[string][]string
}

func parseEcho(body string) echoOf {
	e := echoOf{backend: bodyHostname(body), headers: echoHeaders(body)}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if rest, ok := strings.CutPrefix(line, "GET "); ok {
			e.target, _, _ = strings.Cut(rest, " HTTP/")
		}
	}
	return e
}

// pathCase is one request of a table and where Traefik sends it.
type pathCase struct {
	request string
	backend string
	// forwarded is the request target the backend must see; empty asserts only the backend (the
	// forwarded target is logged), for the cases Traefik cleans before forwarding.
	forwarded string
}

// requirePathCases sends every case over the site and checks the serving backend, and the forwarded
// target where the case states one.
func requirePathCases(t *testing.T, s renderedSite, host string, cases []pathCase) {
	t.Helper()
	for _, tc := range cases {
		status, body, _, err := s.tr.TryGet(s.ca.Pool(), host, tc.request, nil)
		if err != nil || status != http.StatusOK {
			t.Errorf("GET %s answered %d (error %v), want 200 from %s", tc.request, status, err, tc.backend)
			continue
		}
		got := parseEcho(body)
		t.Logf("%-16s -> backend %-5s as %s", tc.request, got.backend, got.target)
		if got.backend != tc.backend {
			t.Errorf("GET %s went to backend %q, want %q (forwarded as %q)", tc.request, got.backend, tc.backend, got.target)
		}
		if tc.forwarded != "" && got.target != tc.forwarded {
			t.Errorf("GET %s reached the backend as %q, want %q", tc.request, got.target, tc.forwarded)
		}
	}
}

// TestTraefikPaths is the tracer of RTE-03 on a rendered bundle: with routes / (backend root), /shop
// (backend shop) and /shop/v2 (backend v2) on one hostname, a path route matches only its own path
// segment and the longest segment prefix wins. /shopping is not under /shop, /shop/v2x is not under
// /shop/v2, and the hygiene variants (dot segments, a doubled slash, an encoded letter, an upper-case
// spelling) cannot reach a neighbour's backend (T-21-33).
func TestTraefikPaths(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	env.StartWhoami(t, traefiktest.WhoamiOptions{Alias: "site_root", Hostname: "root"})
	env.StartWhoami(t, traefiktest.WhoamiOptions{Alias: "site_shop", Hostname: "shop"})
	env.StartWhoami(t, traefiktest.WhoamiOptions{Alias: "site_v2", Hostname: "v2"})

	res := mustRender(t, Input{
		Snapshot: snapshotOf(
			swarmRoute(pathsHost, "/", "site", "root", 80),
			swarmRoute(pathsHost, "/shop", "site", "shop", 80),
			swarmRoute(pathsHost, "/shop/v2", "site", "v2", 80),
		),
		Certs:        servable(t, ca, pathsHost),
		CertsVersion: 1,
	})
	if len(res.Skipped) != 0 {
		t.Fatalf("a route was skipped: %+v", res.Skipped)
	}
	s := startRendered(t, env, ca, res.Bundle)
	s.awaitHandshake(t, pathsHost)
	// The last route to load answers 200 only once the whole bundle is read, and 200 also waits for the
	// backends to listen.
	s.awaitGet(t, pathsHost, "/shop/v2/z")
	s.awaitGet(t, pathsHost, "/shop")
	s.awaitGet(t, pathsHost, "/")

	requirePathCases(t, s, pathsHost, []pathCase{
		{"/", "root", "/"},
		{"/x", "root", "/x"},
		{"/shopping", "root", "/shopping"},
		{"/shop", "shop", "/shop"},
		{"/shop/", "shop", "/shop/"},
		{"/shop/x", "shop", "/shop/x"},
		{"/shop/v2", "v2", "/shop/v2"},
		{"/shop/v2/z", "v2", "/shop/v2/z"},
		{"/shop/v2x", "shop", "/shop/v2x"},
		{"/shop/../shopping", "root", ""},
		{"//shop/x", "shop", ""},
		{"/%73hop/x", "shop", ""},
		{"/SHOP", "root", ""},
	})
}
