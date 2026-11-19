// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// The certificate fixtures are fixed byte strings in PEM shape. The renderer never parses a
// certificate (the store validated it), so no crypto is needed and no date can make a test fail.
var (
	chainA = []byte("-----BEGIN CERTIFICATE-----\nQUFB\n-----END CERTIFICATE-----\n")
	keyA   = []byte("-----BEGIN PRIVATE KEY-----\nS0VZQQ==\n-----END PRIVATE KEY-----\n")
)

// swarmRoute is a route of a swarm stack's service, owned by stack.
func swarmRoute(host, prefix, stack, svc string, port int) ingress.SnapshotRoute {
	return ingress.SnapshotRoute{
		Route: ingress.Route{
			Hostname: host, PathPrefix: prefix, Service: svc, Port: port, Scheme: ingress.SchemeHTTP,
		},
		OwnerKind: ingress.OwnerStack,
		StackName: stack,
		Target:    ingress.TargetSwarm,
	}
}

func claimFor(host string) ingress.SnapshotClaim {
	return ingress.SnapshotClaim{
		Claim:     ingress.Claim{Hostname: host, OwnerKind: ingress.OwnerStack},
		StackName: "shop",
		Target:    ingress.TargetSwarm,
	}
}

// oneRouteInput is the tracer: stack shop, service web, port 8080, prefix /, one ready certificate.
func oneRouteInput() Input {
	return Input{
		Snapshot: ingress.Snapshot{
			Settings: ingress.Settings{Exists: true, Enabled: true, ConfigVersion: 3},
			Claims:   []ingress.SnapshotClaim{claimFor("app.example.com")},
			Routes:   []ingress.SnapshotRoute{swarmRoute("app.example.com", "/", "shop", "web", 8080)},
		},
		Certs:        []HostCert{{Hostname: "app.example.com", ChainPEM: chainA, KeyPEM: keyA}},
		CertsVersion: 7,
	}
}

// dig walks a decoded YAML document by key and fails the test when a step is missing.
func dig(t *testing.T, v any, keys ...string) any {
	t.Helper()
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("at %q: not a mapping: %v", k, v)
		}
		if v, ok = m[k]; !ok {
			t.Fatalf("missing key %q", k)
		}
	}
	return v
}

func dynamicOf(t *testing.T, b ingressbundle.Bundle) (raw []byte, doc map[string]any) {
	t.Helper()
	for _, f := range b.Files {
		if f.Path == ingressbundle.DynamicFile {
			raw = f.Data
		}
	}
	if raw == nil {
		t.Fatalf("bundle has no %s: %v", ingressbundle.DynamicFile, b)
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("dynamic file does not parse: %v\n%s", err, raw)
	}
	return raw, doc
}

// TestRenderOneRouteTracer is the tracer of RTE-01: one claimed swarm route with a ready certificate
// becomes a verified three-file bundle.
func TestRenderOneRouteTracer(t *testing.T) {
	res, err := Render(oneRouteInput())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	b := res.Bundle
	if err := b.Verify(); err != nil {
		t.Fatalf("the bundle does not verify: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("Skipped = %v, want none", res.Skipped)
	}
	if b.Schema != ingressbundle.Schema1 {
		t.Errorf("schema = %d, want %d", b.Schema, ingressbundle.Schema1)
	}
	if want := (ingressbundle.Version{Config: 3, Certs: 7, Own: 0}); b.Version != want {
		t.Errorf("version = %+v, want %+v", b.Version, want)
	}

	crt := "certs/" + ingressbundle.Checksum(chainA) + ".crt"
	key := "certs/" + ingressbundle.Checksum(keyA) + ".key"
	var paths []string
	for _, f := range b.Files {
		paths = append(paths, f.Path)
	}
	// Build sorts the files by path, so the pair's order follows the hashes.
	want := []string{crt, key, ingressbundle.DynamicFile}
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Fatalf("files = %v, want %v", paths, want)
	}

	_, doc := dynamicOf(t, b)

	// D-04, D-23: strict SNI with a TLS floor; never a default store.
	def := dig(t, doc, "tls", "options", "default")
	if got := dig(t, def, "sniStrict"); got != true {
		t.Errorf("sniStrict = %v, want true", got)
	}
	if got := dig(t, def, "minVersion"); got != "VersionTLS12" {
		t.Errorf("minVersion = %v, want VersionTLS12", got)
	}
	if tlsSec, ok := dig(t, doc, "tls").(map[string]any); !ok {
		t.Error("tls is not a mapping")
	} else if _, stores := tlsSec["stores"]; stores {
		t.Error("tls.stores is present: a default store is never rendered (D-04)")
	}

	// D-24: the YAML names the pair by absolute path, the manifest by relative path.
	certs, ok := dig(t, doc, "tls", "certificates").([]any)
	if !ok || len(certs) != 1 {
		t.Fatalf("tls.certificates = %v, want one entry", certs)
	}
	if got, want := dig(t, certs[0], "certFile"), ingressbundle.CertRoot+"/"+strings.TrimPrefix(crt, "certs/"); got != want {
		t.Errorf("certFile = %v, want %v", got, want)
	}
	if got, want := dig(t, certs[0], "keyFile"), ingressbundle.CertRoot+"/"+strings.TrimPrefix(key, "certs/"); got != want {
		t.Errorf("keyFile = %v, want %v", got, want)
	}
	for _, e := range b.Manifest.Files {
		if strings.HasPrefix(e.Path, "/") {
			t.Errorf("manifest path %q is absolute", e.Path)
		}
	}

	// One router: Host rule, priority 1, websecure only, tls {}.
	routers, ok := dig(t, doc, "http", "routers").(map[string]any)
	if !ok || len(routers) != 1 {
		t.Fatalf("routers = %v, want one", routers)
	}
	rname := RouterName("app.example.com", "/")
	r := dig(t, routers, rname)
	if got := dig(t, r, "rule"); got != "Host(`app.example.com`)" {
		t.Errorf("rule = %v", got)
	}
	if got := dig(t, r, "priority"); got != 1 {
		t.Errorf("priority = %v, want 1", got)
	}
	if got := dig(t, r, "entryPoints"); !slices.Equal(toStrings(t, got), []string{"websecure"}) {
		t.Errorf("entryPoints = %v, want [websecure]", got)
	}
	if tls, ok := dig(t, r, "tls").(map[string]any); !ok || len(tls) != 0 {
		t.Errorf("tls = %v, want an empty mapping", dig(t, r, "tls"))
	}
	svcName := ServiceName(ingress.SchemeHTTP, "http://shop_web:8080")
	if got := dig(t, r, "service"); got != svcName {
		t.Errorf("service = %v, want %s", got, svcName)
	}

	// One service with one server.
	servers, ok := dig(t, doc, "http", "services", svcName, "loadBalancer", "servers").([]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("servers = %v, want one", servers)
	}
	if got := dig(t, servers[0], "url"); got != "http://shop_web:8080" {
		t.Errorf("url = %v", got)
	}

	// The manifest keeps the readable identity and the full hash.
	if len(b.Manifest.Routes) != 1 {
		t.Fatalf("manifest routes = %v, want one", b.Manifest.Routes)
	}
	wantRoute := ingressbundle.RouteEntry{
		Router: rname, Hostname: "app.example.com", PathPrefix: "/", Stack: "shop", Service: "web",
		Hash: routeHash("app.example.com", "/"),
	}
	if b.Manifest.Routes[0] != wantRoute {
		t.Errorf("route entry = %+v, want %+v", b.Manifest.Routes[0], wantRoute)
	}
	if len(wantRoute.Hash) != 64 {
		t.Errorf("route hash is %d hex digits, want 64", len(wantRoute.Hash))
	}
}

func toStrings(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %v", v)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("list element %v is not a string", e)
		}
		out = append(out, s)
	}
	return out
}
