// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// openPair is two consecutive opening braces, built here so no source file holds the pair literally.
var openPair = "{" + "{"

// httpsRoute is a swarm route whose backend is spoken to over https.
func httpsRoute(host, prefix, stack, svc string, port int) ingress.SnapshotRoute {
	r := swarmRoute(host, prefix, stack, svc, port)
	r.Scheme = ingress.SchemeHTTPS
	return r
}

// httpsInput is one https route to stack shop, service web, port 8443.
func httpsInput() Input {
	in := oneRouteInput()
	in.Snapshot.Routes[0] = httpsRoute("app.example.com", "/", "shop", "web", 8443)
	return in
}

// TestHTTPSBackendTransport pins D-13: an https route's service names the one user transport, the
// bundle holds that transport, and a bundle with no https route has no transports at all.
func TestHTTPSBackendTransport(t *testing.T) {
	t.Run("https route", func(t *testing.T) {
		_, doc := dynamicOf(t, mustRender(t, httpsInput()).Bundle)
		svcName := ServiceName(ingress.SchemeHTTPS, "https://shop_web:8443")
		lb := digMap(t, doc, "http", "services", svcName, "loadBalancer")
		if got := lb["serversTransport"]; got != UserHTTPSTransport {
			t.Errorf("serversTransport = %v, want %s", got, UserHTTPSTransport)
		}
		tr := digMap(t, doc, "http", "serversTransports", UserHTTPSTransport)
		if tr["insecureSkipVerify"] != true || tr["disableHTTP2"] != true {
			t.Errorf("transport = %v, want insecureSkipVerify and disableHTTP2 true", tr)
		}
		if got := len(digMap(t, doc, "http", "serversTransports")); got != 1 {
			t.Errorf("%d transports, want 1", got)
		}
	})

	t.Run("http only", func(t *testing.T) {
		raw, doc := dynamicOf(t, mustRender(t, oneRouteInput()).Bundle)
		if _, has := digMap(t, doc, "http")["serversTransports"]; has {
			t.Errorf("an http-only bundle has serversTransports:\n%s", raw)
		}
		if strings.Contains(string(raw), "serversTransport") {
			t.Errorf("an http-only bundle names a transport:\n%s", raw)
		}
	})

	t.Run("mixed", func(t *testing.T) {
		in := httpsInput()
		in.Snapshot.Routes = append(in.Snapshot.Routes, swarmRoute("app.example.com", "/plain", "shop", "web", 8080))
		_, doc := dynamicOf(t, mustRender(t, in).Bundle)
		httpSvc := ServiceName(ingress.SchemeHTTP, "http://shop_web:8080")
		if _, has := digMap(t, doc, "http", "services", httpSvc, "loadBalancer")["serversTransport"]; has {
			t.Error("the http service names a transport")
		}
		httpsSvc := ServiceName(ingress.SchemeHTTPS, "https://shop_web:8443")
		if got := digMap(t, doc, "http", "services", httpsSvc, "loadBalancer")["serversTransport"]; got != UserHTTPSTransport {
			t.Errorf("the https service names transport %v", got)
		}
	})

	t.Run("a skipped https route adds no transport", func(t *testing.T) {
		in := httpsInput()
		in.Certs = nil
		_, doc := dynamicOf(t, mustRender(t, in).Bundle)
		if _, has := doc["http"]; has {
			t.Errorf("no route rendered but the file has an http section: %v", doc["http"])
		}
	})
}

// TestRefusesUnsafeValues pins D-08: a backtick or a brace in any of the five route fields fails the
// render with the route and the field named, whether or not the route would have been rendered.
func TestRefusesUnsafeValues(t *testing.T) {
	fields := []struct {
		field string
		set   func(r *ingress.SnapshotRoute, v string)
	}{
		{"hostname", func(r *ingress.SnapshotRoute, v string) { r.Hostname = v }},
		{"pathPrefix", func(r *ingress.SnapshotRoute, v string) { r.PathPrefix = v }},
		{"service", func(r *ingress.SnapshotRoute, v string) { r.Service = v }},
		{"name", func(r *ingress.SnapshotRoute, v string) { r.Name = v }},
		{"stackName", func(r *ingress.SnapshotRoute, v string) { r.StackName = v }},
	}
	chars := map[string]string{"backtick": "`", "open brace": "{", "close brace": "}"}

	for _, f := range fields {
		for _, name := range slices.Sorted(maps.Keys(chars)) {
			char := chars[name]
			for _, rendered := range []bool{true, false} {
				label := f.field + " with " + name
				if !rendered {
					label += " on a skipped route"
				}
				t.Run(label, func(t *testing.T) {
					in := oneRouteInput()
					if !rendered {
						in.Certs = nil // the route would be skipped as no_certificate
					}
					f.set(&in.Snapshot.Routes[0], "a"+char+"b")
					res, err := Render(in)
					var refused *RefusedError
					if !errors.As(err, &refused) {
						t.Fatalf("error %T %v is not a *RefusedError", err, err)
					}
					if refused.Code != CodeUnsafeValue {
						t.Errorf("code = %q, want %q", refused.Code, CodeUnsafeValue)
					}
					if refused.Field != f.field {
						t.Errorf("field = %q, want %q", refused.Field, f.field)
					}
					r := in.Snapshot.Routes[0]
					if refused.Hostname != r.Hostname || refused.PathPrefix != r.PathPrefix {
						t.Errorf("names route %q %q, want %q %q", refused.Hostname, refused.PathPrefix, r.Hostname, r.PathPrefix)
					}
					if !strings.Contains(err.Error(), f.field) {
						t.Errorf("error text %q does not name the field", err)
					}
					if res.Bundle.ID != "" || len(res.Bundle.Files) != 0 || len(res.Skipped) != 0 {
						t.Errorf("a refusal returned a non-zero Result: %+v", res)
					}
				})
			}
		}
	}

	t.Run("the first route in order is the one named, fields in fixed order", func(t *testing.T) {
		in := oneRouteInput()
		bad := swarmRoute("app.example.com", "/z", "shop", "web", 8080)
		bad.Hostname, bad.Service = "app.example.com", "w`eb"
		bad.PathPrefix = "/z{"
		later := swarmRoute("zzz.example.com", "/", "sh`op", "web", 8080)
		in.Snapshot.Routes = []ingress.SnapshotRoute{later, bad, in.Snapshot.Routes[0]}
		_, err := Render(in)
		var refused *RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("error %T %v is not a *RefusedError", err, err)
		}
		if refused.Hostname != "app.example.com" || refused.PathPrefix != "/z{" || refused.Field != "pathPrefix" {
			t.Errorf("refused %+v, want the app.example.com /z{ route on pathPrefix", refused)
		}
	})

	t.Run("safe punctuation is not refused", func(t *testing.T) {
		in := oneRouteInput()
		in.Snapshot.Routes[0].Name = "my-route_1.v2"
		if _, err := Render(in); err != nil {
			t.Errorf("a plain name was refused: %v", err)
		}
	})
}

// TestRefusesBackendURLCharacters pins the phase 21 review's WR-02: the stack and service names go into
// the backend URL, so any character that would give that URL another host, userinfo, path, query or
// fragment is refused, rendered route or not. Names of the characters compose allows are not refused.
func TestRefusesBackendURLCharacters(t *testing.T) {
	fields := []struct {
		field string
		set   func(r *ingress.SnapshotRoute, v string)
	}{
		{"service", func(r *ingress.SnapshotRoute, v string) { r.Service = v }},
		{"stackName", func(r *ingress.SnapshotRoute, v string) { r.StackName = v }},
	}
	chars := map[string]string{
		"at sign": "@", "slash": "/", "backslash": `\`, "hash": "#", "question mark": "?", "colon": ":",
		"percent": "%", "space": " ", "tab": "\t", "newline": "\n", "non-ASCII": "é",
	}
	for _, f := range fields {
		for _, name := range slices.Sorted(maps.Keys(chars)) {
			for _, rendered := range []bool{true, false} {
				label := f.field + " with " + name
				if !rendered {
					label += " on a skipped route"
				}
				t.Run(label, func(t *testing.T) {
					in := oneRouteInput()
					if !rendered {
						in.Certs = nil
					}
					f.set(&in.Snapshot.Routes[0], "x"+chars[name]+"evil.example")
					res, err := Render(in)
					var refused *RefusedError
					if !errors.As(err, &refused) {
						t.Fatalf("error %T %v is not a *RefusedError", err, err)
					}
					if refused.Code != CodeUnsafeValue || refused.Field != f.field {
						t.Errorf("refused code %q field %q, want %q %q", refused.Code, refused.Field, CodeUnsafeValue, f.field)
					}
					if res.Bundle.ID != "" || len(res.Bundle.Files) != 0 {
						t.Errorf("a refusal returned a bundle: %+v", res)
					}
				})
			}
		}
	}

	t.Run("compose-legal names are not refused", func(t *testing.T) {
		in := oneRouteInput()
		in.Snapshot.Routes[0].StackName = "my-stack_2.v1"
		in.Snapshot.Routes[0].Service = "Web.API-2_x"
		res := mustRender(t, in)
		if len(res.Skipped) != 0 {
			t.Errorf("a compose-legal route was skipped: %+v", res.Skipped)
		}
	})
}

// TestPortOutOfRangeHasNoBackend pins the second half of WR-02: a port no TCP service can listen on
// names no backend, so the route is skipped as no_backend rather than written into a URL.
func TestPortOutOfRangeHasNoBackend(t *testing.T) {
	for _, port := range []int{0, -1, 65536, 1 << 20} {
		in := oneRouteInput()
		in.Snapshot.Routes[0].Port = port
		res := mustRender(t, in)
		if len(res.Skipped) != 1 || res.Skipped[0].Reason != SkipNoBackend {
			t.Errorf("port %d: Skipped = %+v, want the route skipped as %s", port, res.Skipped, SkipNoBackend)
		}
	}
	in := oneRouteInput()
	in.Snapshot.Routes[0].Port = 65535
	if res := mustRender(t, in); len(res.Skipped) != 0 {
		t.Errorf("port 65535: Skipped = %+v, want it rendered", res.Skipped)
	}
}

// TestNoTemplateDelimiters pins the second line of D-08: a rendered file holding two consecutive
// opening braces is refused, and the empty mapping and single braces are not.
func TestNoTemplateDelimiters(t *testing.T) {
	bad := []ingressbundle.File{
		{Path: "dynamic/tierplan.yml", Data: []byte("tls: {}\n")},
		{Path: "dynamic/other.yml", Data: []byte("rule: Host(`a`) && " + openPair + " .Env " + "}}\n")},
	}
	err := checkNoTemplateDelimiters(bad)
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error %T %v is not a *RefusedError", err, err)
	}
	if refused.Code != CodeTemplateDelimiter || refused.Field != "dynamic/other.yml" {
		t.Errorf("refused %+v, want code %s on dynamic/other.yml", refused, CodeTemplateDelimiter)
	}
	if strings.Contains(err.Error(), openPair) {
		t.Errorf("the error text repeats the delimiter: %q", err)
	}

	ok := []ingressbundle.File{
		{Path: "dynamic/tierplan.yml", Data: []byte("tls: {}\nx: {a: 1}\ny: [{a: 1}, {b: 2}]\nz: { }\n")},
		{Path: "certs/a.key", Data: []byte("-----BEGIN PRIVATE KEY-----\nS0VZQQ==\n-----END PRIVATE KEY-----\n")},
	}
	if err := checkNoTemplateDelimiters(ok); err != nil {
		t.Errorf("a file with an empty mapping and single braces was refused: %v", err)
	}

	// No golden scenario trips it.
	for _, name := range slices.Sorted(maps.Keys(goldenScenarios())) {
		res := mustRender(t, goldenScenarios()[name])
		if err := checkNoTemplateDelimiters(res.Bundle.Files); err != nil {
			t.Errorf("scenario %s: %v", name, err)
		}
	}
}

// TestNamesDoNotCollide pins D-18: a Traefik name is never shared by two different objects.
func TestNamesDoNotCollide(t *testing.T) {
	r1 := swarmRoute("a.example.com", "/", "shop", "web", 8080)
	r2 := swarmRoute("b.example.com", "/", "shop", "web", 8080)

	t.Run("duplicate router names", func(t *testing.T) {
		err := checkUnique([]namedObject{
			{kind: "router", name: "r-aaa", identity: "1", route: r1},
			{kind: "router", name: "r-aaa", identity: "2", route: r2},
		})
		var refused *RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("error %T %v is not a *RefusedError", err, err)
		}
		if refused.Code != CodeNameCollision || refused.Hostname != "b.example.com" || refused.PathPrefix != "/" {
			t.Errorf("refused %+v, want %s naming the second route b.example.com /", refused, CodeNameCollision)
		}
	})
	t.Run("the same router twice", func(t *testing.T) {
		err := checkUnique([]namedObject{
			{kind: "router", name: "r-aaa", identity: "1", route: r1},
			{kind: "router", name: "r-aaa", identity: "1", route: r1},
		})
		if err == nil {
			t.Error("two routers with one name and one identity were accepted")
		}
	})
	t.Run("a service shared by identity is one service", func(t *testing.T) {
		err := checkUnique([]namedObject{
			{kind: "service", name: "s-aaa", identity: "http\x00http://x:1", shared: true, route: r1},
			{kind: "service", name: "s-aaa", identity: "http\x00http://x:1", shared: true, route: r2},
		})
		if err != nil {
			t.Errorf("two routes to one backend were refused: %v", err)
		}
	})
	t.Run("a service name for two backends", func(t *testing.T) {
		err := checkUnique([]namedObject{
			{kind: "service", name: "s-aaa", identity: "http\x00http://x:1", shared: true, route: r1},
			{kind: "service", name: "s-aaa", identity: "http\x00http://y:1", shared: true, route: r2},
		})
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Code != CodeNameCollision {
			t.Errorf("error %v, want a %s refusal", err, CodeNameCollision)
		}
	})
	t.Run("the same name in two kinds is fine", func(t *testing.T) {
		err := checkUnique([]namedObject{
			{kind: "router", name: "x", identity: "1", route: r1},
			{kind: "middleware", name: "x", identity: "1", route: r1},
		})
		if err != nil {
			t.Errorf("one name used by a router and a middleware was refused: %v", err)
		}
	})
	t.Run("a snapshot that repeats a route is refused by Render", func(t *testing.T) {
		in := oneRouteInput()
		in.Snapshot.Routes = append(in.Snapshot.Routes, in.Snapshot.Routes[0])
		_, err := Render(in)
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Code != CodeNameCollision {
			t.Errorf("error %v, want a %s refusal", err, CodeNameCollision)
		}
	})

	// Across every golden scenario the names are unique and the manifest holds the full route hash.
	full := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, name := range slices.Sorted(maps.Keys(goldenScenarios())) {
		t.Run("scenario "+name, func(t *testing.T) {
			res := mustRender(t, goldenScenarios()[name])
			_, doc := dynamicOf(t, res.Bundle)
			routers := map[string]bool{}
			for _, e := range res.Bundle.Manifest.Routes {
				if !full.MatchString(e.Hash) {
					t.Errorf("route %s hash %q is not 64 hex digits", e.Router, e.Hash)
				}
				if routers[e.Router] {
					t.Errorf("router %s is listed twice", e.Router)
				}
				routers[e.Router] = true
				// TierPlan's own route has a fixed name (D-09); every stack route's name is its hash.
				if e.Router != "r-"+e.Hash[:nameHashLen] && (e.Router != selfRouterName || e.Stack != "") {
					t.Errorf("router %s is not derived from its hash %s", e.Router, e.Hash)
				}
			}
			if _, has := doc["http"]; !has {
				return
			}
			// TierPlan's HTTP-01 carve-out (D-12) is a router but not a route: it is never in the manifest.
			fileRouters := digMap(t, doc, "http", "routers")
			got := len(fileRouters)
			if _, carve := fileRouters[acmeRouterName]; carve {
				got--
			}
			if got != len(routers) {
				t.Errorf("%d route routers in the file but %d manifest routes", got, len(routers))
			}
		})
	}
}
