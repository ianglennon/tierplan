// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"bytes"
	"os"
	"path/filepath"
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
	if b.Schema != ingressbundle.Schema2 {
		t.Errorf("schema = %d, want %d", b.Schema, ingressbundle.Schema2)
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

// TestRuleAndPriority pins D-15 and D-16: the segment-exact rule and the explicit priority.
func TestRuleAndPriority(t *testing.T) {
	long := "/" + strings.Repeat("a", 254) // the longest prefix the parser accepts, 255 bytes
	rules := []struct{ host, prefix, want string }{
		{"h", "/", "Host(`h`)"},
		{"h", "/api", "Host(`h`) && (Path(`/api`) || PathPrefix(`/api/`))"},
		{"app.example.com", "/api/v2", "Host(`app.example.com`) && (Path(`/api/v2`) || PathPrefix(`/api/v2/`))"},
	}
	for _, r := range rules {
		if got := Rule(r.host, r.prefix); got != r.want {
			t.Errorf("Rule(%q, %q) = %q, want %q", r.host, r.prefix, got, r.want)
		}
	}
	priorities := []struct {
		prefix string
		want   int
	}{
		{"/", 1},
		{"/api", 1004},
		{"/api/v2", 2007},
		{"/apiary", 1007},
		{"/api/v2x", 2008},
		{long, 1255},
		{"/a/b/c", 3006},
	}
	for _, p := range priorities {
		if got := Priority(p.prefix); got != p.want {
			t.Errorf("Priority(%q) = %d, want %d", p.prefix, got, p.want)
		}
	}
	// A deeper prefix always beats a shallower one, even a longer shallow one.
	if Priority("/a/b") <= Priority(long) {
		t.Errorf("Priority(/a/b) = %d does not beat a 255-byte one-segment prefix (%d)", Priority("/a/b"), Priority(long))
	}
}

// TestEveryRouterIsOnWebsecureOnly pins PROH-21-03: no rendered router is ever on the web entry point.
func TestEveryRouterIsOnWebsecureOnly(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(goldenDir, "*", "tierplan.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no golden dynamic file was found")
	}
	routers := 0
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // G304: a golden file under testdata
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		httpSec, ok := doc["http"].(map[string]any)
		if !ok {
			continue // the empty bundle has no routers
		}
		rs, ok := httpSec["routers"].(map[string]any)
		if !ok {
			t.Errorf("%s: http without routers", f)
			continue
		}
		for name, r := range rs {
			routers++
			eps := toStrings(t, dig(t, r, "entryPoints"))
			if !slices.Equal(eps, []string{"websecure"}) {
				t.Errorf("%s: router %s entryPoints = %v, want [websecure] only", f, name, eps)
			}
		}
	}
	if routers == 0 {
		t.Fatal("no router was checked")
	}
}

// oneHostInput is the one-host golden's input with the versions the caller picks.
func oneHostInput(config, certs int64) Input {
	in := goldenScenarios()["one-host"]
	in.Snapshot.Settings.ConfigVersion = config
	in.CertsVersion = certs
	return in
}

func mustRender(t *testing.T, in Input) Result {
	t.Helper()
	res, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return res
}

func filePaths(b ingressbundle.Bundle) []string {
	out := make([]string, 0, len(b.Files))
	for _, f := range b.Files {
		out = append(out, f.Path)
	}
	return out
}

// TestDeterministicSameSnapshot pins D-07: the same snapshot renders the same bytes and the same ID,
// however its slices happen to be ordered.
func TestDeterministicSameSnapshot(t *testing.T) {
	a := mustRender(t, oneHostInput(3, 1))
	b := mustRender(t, oneHostInput(3, 1))
	if a.Bundle.ID != b.Bundle.ID {
		t.Errorf("IDs differ between two renders: %s and %s", a.Bundle.ID, b.Bundle.ID)
	}
	if !slices.Equal(filePaths(a.Bundle), filePaths(b.Bundle)) {
		t.Fatalf("file lists differ: %v and %v", filePaths(a.Bundle), filePaths(b.Bundle))
	}
	for i := range a.Bundle.Files {
		if !bytes.Equal(a.Bundle.Files[i].Data, b.Bundle.Files[i].Data) {
			t.Errorf("%s differs between two renders", a.Bundle.Files[i].Path)
		}
	}

	// The order the model hands its rows in does not matter either.
	rev := oneHostInput(3, 1)
	slices.Reverse(rev.Snapshot.Routes)
	slices.Reverse(rev.Snapshot.Claims)
	slices.Reverse(rev.Certs)
	c := mustRender(t, rev)
	if c.Bundle.ID != a.Bundle.ID {
		t.Errorf("reversing the input order changed the ID: %s and %s", c.Bundle.ID, a.Bundle.ID)
	}
	if !bytes.Equal(dynamicBytes(t, c), dynamicBytes(t, a)) {
		t.Error("reversing the input order changed the dynamic file")
	}
}

// TestIDFollowsContentNotVersion pins D-06 through Render: a version moves the Version and never the
// ID; a certificate or a route changes the ID.
func TestIDFollowsContentNotVersion(t *testing.T) {
	base := mustRender(t, oneHostInput(3, 1))

	moved := mustRender(t, oneHostInput(9, 5))
	if moved.Bundle.ID != base.Bundle.ID {
		t.Errorf("a version change moved the ID: %s and %s", moved.Bundle.ID, base.Bundle.ID)
	}
	if moved.Bundle.Version == base.Bundle.Version {
		t.Errorf("versions did not follow the input: %+v", moved.Bundle.Version)
	}
	if want := (ingressbundle.Version{Config: 9, Certs: 5}); moved.Bundle.Version != want {
		t.Errorf("version = %+v, want %+v", moved.Bundle.Version, want)
	}
	own := oneHostInput(3, 1)
	own.OwnVersion = 4
	if got := mustRender(t, own); got.Bundle.ID != base.Bundle.ID || got.Bundle.Version.Own != 4 {
		t.Errorf("OwnVersion 4: id %s (want %s), version %+v", got.Bundle.ID, base.Bundle.ID, got.Bundle.Version)
	}

	newCert := oneHostInput(3, 1)
	newCert.Certs = []HostCert{{
		Hostname: "app.example.com",
		ChainPEM: []byte("-----BEGIN CERTIFICATE-----\nQkJC\n-----END CERTIFICATE-----\n"),
		KeyPEM:   keyA,
	}}
	rotated := mustRender(t, newCert)
	if rotated.Bundle.ID == base.Bundle.ID {
		t.Error("a different certificate kept the ID")
	}
	if slices.Equal(filePaths(rotated.Bundle), filePaths(base.Bundle)) {
		t.Errorf("a different certificate kept the file names: %v", filePaths(rotated.Bundle))
	}

	newPort := oneHostInput(3, 1)
	newPort.Snapshot.Routes[1].Port = 9090
	if got := mustRender(t, newPort); got.Bundle.ID == base.Bundle.ID {
		t.Error("a different port kept the ID")
	}
}

// TestSharedCertificatePairIsWrittenOnce pins D-02: two hostnames on one identical pair give one
// certificate file, one key file and one tls.certificates entry.
func TestSharedCertificatePairIsWrittenOnce(t *testing.T) {
	in := Input{
		Snapshot: ingress.Snapshot{
			Settings: ingress.Settings{Exists: true, Enabled: true, ConfigVersion: 1},
			Claims:   []ingress.SnapshotClaim{claimFor("app.example.com"), claimFor("www.example.com")},
			Routes: []ingress.SnapshotRoute{
				swarmRoute("app.example.com", "/", "shop", "web", 8080),
				swarmRoute("www.example.com", "/", "shop", "web", 8080),
			},
		},
		Certs: []HostCert{
			{Hostname: "app.example.com", ChainPEM: chainA, KeyPEM: keyA},
			{Hostname: "www.example.com", ChainPEM: chainA, KeyPEM: keyA},
		},
	}
	res := mustRender(t, in)

	var crts, keys int
	for _, p := range filePaths(res.Bundle) {
		switch {
		case strings.HasSuffix(p, ".crt"):
			crts++
		case strings.HasSuffix(p, ".key"):
			keys++
		}
	}
	if crts != 1 || keys != 1 {
		t.Errorf("certificate files = %d, key files = %d, want 1 and 1 (%v)", crts, keys, filePaths(res.Bundle))
	}
	_, doc := dynamicOf(t, res.Bundle)
	if certs, ok := dig(t, doc, "tls", "certificates").([]any); !ok || len(certs) != 1 {
		t.Errorf("tls.certificates = %v, want one entry", certs)
	}
	if routers, ok := dig(t, doc, "http", "routers").(map[string]any); !ok || len(routers) != 2 {
		t.Errorf("routers = %v, want two", routers)
	}
	if services, ok := dig(t, doc, "http", "services").(map[string]any); !ok || len(services) != 1 {
		t.Errorf("services = %v, want one shared service", services)
	}
}
