// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// stripInput is one host with the root (no strip) and /api (strip).
func stripInput() Input {
	api := swarmRoute("app.example.com", "/api", "shop", "api", 9000)
	api.Strip = true
	return Input{
		Snapshot: ingress.Snapshot{
			Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 9},
			Claims:   []ingress.SnapshotClaim{claimFor("app.example.com")},
			Routes: []ingress.SnapshotRoute{
				swarmRoute("app.example.com", "/", "shop", "web", 8080),
				api,
			},
		},
		Certs:        []HostCert{{Hostname: "app.example.com", ChainPEM: chainA, KeyPEM: keyA}},
		CertsVersion: 2,
	}
}

// digMap is dig for a step that must be a mapping.
func digMap(t *testing.T, v any, keys ...string) map[string]any {
	t.Helper()
	m, ok := dig(t, v, keys...).(map[string]any)
	if !ok {
		t.Fatalf("at %v: not a mapping", keys)
	}
	return m
}

// TestStripMiddleware is the tracer of LBL-07: a strip route gets its own stripPrefix middleware, named
// from its route hash, and its router lists it; a route without strip lists none (D-17).
func TestStripMiddleware(t *testing.T) {
	res := mustRender(t, stripInput())
	_, doc := dynamicOf(t, res.Bundle)

	apiRouter := digMap(t, doc, "http", "routers", RouterName("app.example.com", "/api"))
	wantName := MiddlewareName("app.example.com", "/api")
	if !regexp.MustCompile(`^m-strip-[0-9a-f]{12}$`).MatchString(wantName) {
		t.Fatalf("middleware name %q is not m-strip-<12 hex>", wantName)
	}
	got := toStrings(t, apiRouter["middlewares"])
	if len(got) != 1 || got[0] != wantName {
		t.Errorf("the /api router lists middlewares %v, want exactly [%s]", got, wantName)
	}

	prefixes := toStrings(t, dig(t, doc, "http", "middlewares", wantName, "stripPrefix", "prefixes"))
	if len(prefixes) != 1 || prefixes[0] != "/api" {
		t.Errorf("stripPrefix.prefixes = %v, want [/api]", prefixes)
	}

	rootRouter := digMap(t, doc, "http", "routers", RouterName("app.example.com", "/"))
	if _, has := rootRouter["middlewares"]; has {
		t.Errorf("the / router (no strip) has a middlewares key: %v", rootRouter["middlewares"])
	}
	mws := digMap(t, doc, "http", "middlewares")
	if len(mws) != 1 {
		t.Errorf("the bundle holds %d middlewares, want 1: %v", len(mws), mws)
	}
}

// TestStripNeedsARenderedRoute: a strip route that is skipped leaves no middleware behind.
func TestStripNeedsARenderedRoute(t *testing.T) {
	in := stripInput()
	in.Certs = nil // no certificate: every route is skipped
	res := mustRender(t, in)
	_, doc := dynamicOf(t, res.Bundle)
	if _, has := doc["http"]; has {
		t.Errorf("a bundle with no rendered route has an http section: %v", doc["http"])
	}
	if len(res.Skipped) != 2 {
		t.Errorf("skipped = %d, want 2", len(res.Skipped))
	}
}

// TestStripRootRefused: strip on the root prefix would remove every path. The parser refuses it; the
// renderer refuses it too (defence in depth, D-17) and returns no bundle.
func TestStripRootRefused(t *testing.T) {
	in := stripInput()
	in.Snapshot.Routes[0].Strip = true // the / route
	res, err := Render(in)
	if err == nil {
		t.Fatal("strip on / rendered")
	}
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error %T %v is not a *RefusedError", err, err)
	}
	if refused.Code != CodeStripRoot {
		t.Errorf("code = %q, want %q", refused.Code, CodeStripRoot)
	}
	if refused.Hostname != "app.example.com" || refused.PathPrefix != "/" {
		t.Errorf("names route %q %q, want app.example.com /", refused.Hostname, refused.PathPrefix)
	}
	if !strings.Contains(err.Error(), "app.example.com") {
		t.Errorf("error text %q does not name the hostname", err)
	}
	if res.Bundle.ID != "" || len(res.Bundle.Files) != 0 || len(res.Skipped) != 0 {
		t.Errorf("a refusal returned a non-zero Result: %+v", res)
	}
}
