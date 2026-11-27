// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"bytes"
	"errors"
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

// TestEveryRouterIsOnWebsecureOnly pins PROH-21-03 as Phase 28 left it (D-12, Pitfall 8): the one
// router on the web entry point is TierPlan's own HTTP-01 carve-out, tierplan-acme. It is on web only,
// has no tls, sits at CarveOutPriority, and its rule is the scenario's ACME hosts, exactly, on the
// challenge path. Every other router is on websecure only and served over TLS. No router names any
// other entry point (the ping entry point included).
func TestEveryRouterIsOnWebsecureOnly(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(goldenDir, "*", "tierplan.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no golden dynamic file was found")
	}
	scenarios := goldenScenarios()
	routers, carveOuts := 0, 0
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
			fields, _ := r.(map[string]any)
			_, hasTLS := fields["tls"]
			if name != acmeRouterName {
				if !slices.Equal(eps, []string{"websecure"}) {
					t.Errorf("%s: router %s entryPoints = %v, want [websecure] only", f, name, eps)
				}
				if !hasTLS {
					t.Errorf("%s: router %s on websecure has no tls", f, name)
				}
				continue
			}
			carveOuts++
			if !slices.Equal(eps, []string{"web"}) {
				t.Errorf("%s: router %s entryPoints = %v, want [web] only", f, name, eps)
			}
			if hasTLS {
				t.Errorf("%s: router %s has a tls key; the carve-out is plain HTTP", f, name)
			}
			if p, _ := fields["priority"].(int); p != CarveOutPriority {
				t.Errorf("%s: router %s priority = %v, want %d", f, name, fields["priority"], CarveOutPriority)
			}
			scenario, ok := scenarios[filepath.Base(filepath.Dir(f))]
			if !ok || scenario.Self == nil {
				t.Errorf("%s: router %s in a scenario with no self input", f, name)
				continue
			}
			rule, _ := fields["rule"].(string)
			if hosts, ok := carveOutRuleHosts(rule); !ok {
				t.Errorf("%s: router %s rule %q is not hosts on the challenge path", f, name, rule)
			} else {
				for _, h := range hosts {
					if !slices.Contains(scenario.Self.ACMEHosts, h) {
						t.Errorf("%s: router %s routes %q, which is not an ACME host of the scenario", f, name, h)
					}
				}
			}
		}
	}
	if routers == 0 {
		t.Fatal("no router was checked")
	}
	if carveOuts == 0 {
		t.Fatal("no golden holds the carve-out, so its rule was never checked")
	}
}

// carveOutRuleHosts parses a carve-out rule: one Host matcher, or two or more joined by || in
// parentheses, then exactly the challenge PathPrefix. Anything else (another matcher, a negation,
// a second path) is not a carve-out rule.
func carveOutRuleHosts(rule string) ([]string, bool) {
	const tail = " && PathPrefix(`/.well-known/acme-challenge/`)"
	head, ok := strings.CutSuffix(rule, tail)
	if !ok {
		return nil, false
	}
	parts := []string{head}
	if inner, ok := strings.CutPrefix(head, "("); ok {
		inner, ok = strings.CutSuffix(inner, ")")
		if !ok {
			return nil, false
		}
		parts = strings.Split(inner, " || ")
		if len(parts) < 2 {
			return nil, false
		}
	}
	hosts := make([]string, 0, len(parts))
	for _, p := range parts {
		h, ok := strings.CutPrefix(p, "Host(`")
		if !ok {
			return nil, false
		}
		h, ok = strings.CutSuffix(h, "`)")
		if !ok || h == "" || strings.ContainsAny(h, "`()|&! ") {
			return nil, false
		}
		hosts = append(hosts, h)
	}
	return hosts, true
}

// TestCarveOutRefusals pins T-28-46 and D-12: an ACME host that could reach the rule with a backtick or
// a brace fails the render whether or not TierPlan is attached; a blank ACME host fails an attached
// render; and the carve-out is rendered only while TierPlan is attached and has an ACME host. Hosts are
// lower-cased and deduplicated, so each is matched once.
func TestCarveOutRefusals(t *testing.T) {
	carveOutOnly := func(attached bool, hosts ...string) Input {
		return Input{
			Snapshot: ingress.Snapshot{Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 1}},
			Self:     &SelfInput{BackendHost: "tierplan_tierplan", BackendPort: 7480, ACMEHosts: hosts, Attached: attached},
		}
	}
	for _, tc := range []struct {
		name  string
		in    Input
		code  string
		field string
	}{
		{"a backtick", carveOutOnly(true, "tierplan.example.org", "a`b"), CodeUnsafeValue, "self acmeHost"},
		{"a brace", carveOutOnly(true, "tier{plan.example.org"), CodeUnsafeValue, "self acmeHost"},
		{"a backtick while not attached", carveOutOnly(false, "a`b"), CodeUnsafeValue, "self acmeHost"},
		{"a blank host", carveOutOnly(true, "tierplan.example.org", ""), CodeSelfInput, "self acmeHost"},
		{"a backtick beside the self route", func() Input {
			in := selfACMEInput()
			in.Self.ACMEHosts = append(in.Self.ACMEHosts, "x`) || Host(`evil.example")
			return in
		}(), CodeUnsafeValue, "self acmeHost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Render(tc.in)
			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Render = %+v, %v; want a refusal", res.Skipped, err)
			}
			if refused.Code != tc.code || refused.Field != tc.field {
				t.Errorf("refused %s on %q, want %s on %q", refused.Code, refused.Field, tc.code, tc.field)
			}
		})
	}

	noCarveOut := func(t *testing.T, in Input) {
		t.Helper()
		raw, _ := dynamicOf(t, mustRender(t, in).Bundle)
		for _, s := range []string{acmeRouterName, plainServiceName, "acme-challenge", "web\n"} {
			if strings.Contains(string(raw), s) {
				t.Errorf("the dynamic file names %s:\n%s", s, raw)
			}
		}
	}
	t.Run("not attached: no carve-out", func(t *testing.T) {
		noCarveOut(t, carveOutOnly(false, "tierplan.example.org"))
		in := selfACMEInput()
		in.Self.Attached = false
		noCarveOut(t, in)
	})
	t.Run("no ACME host: no carve-out", func(t *testing.T) {
		noCarveOut(t, carveOutOnly(true))
		noCarveOut(t, selfRouteInput(true))
	})
	t.Run("no self input: no carve-out", func(t *testing.T) {
		in := selfACMEInput()
		in.Self = nil
		noCarveOut(t, in)
	})
	t.Run("hosts are lower-cased and deduplicated", func(t *testing.T) {
		in := carveOutOnly(true, "Tierplan.Example.org", "tierplan.example.org", "agent.example.org")
		_, doc := dynamicOf(t, mustRender(t, in).Bundle)
		rule, _ := dig(t, doc, "http", "routers", acmeRouterName, "rule").(string)
		want := "(Host(`agent.example.org`) || Host(`tierplan.example.org`)) && PathPrefix(`/.well-known/acme-challenge/`)"
		if rule != want {
			t.Errorf("rule = %q, want %q", rule, want)
		}
	})
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

// TestSelfInputRefusals pins T-28-36 and Pitfall 9: every self value that can reach the dynamic file is
// held to the route rule, and an attached input that cannot be rendered fails the whole render, so the
// node keeps its last good bundle.
func TestSelfInputRefusals(t *testing.T) {
	keyOnly := []byte("-----BEGIN PRIVATE KEY-----\nS0VZQQ==\n-----END PRIVATE KEY-----\n")
	cases := []struct {
		name  string
		edit  func(s *SelfInput)
		code  string
		field string
	}{
		{"host with a brace", func(s *SelfInput) { s.Host = "tp{.apps.example.org" }, CodeUnsafeValue, "self host"},
		{"backend host with a backtick", func(s *SelfInput) { s.BackendHost = "tierplan`x" }, CodeUnsafeValue, "self backendHost"},
		{"backend host with a slash", func(s *SelfInput) { s.BackendHost = "evil.example/x" }, CodeUnsafeValue, "self backendHost"},
		{"server name with a brace", func(s *SelfInput) { s.ServerName = "tierplan}.example.org" }, CodeUnsafeValue, "self serverName"},
		{"acme host with a backtick", func(s *SelfInput) { s.ACMEHosts = []string{"a`b"} }, CodeUnsafeValue, "self acmeHost"},
		{"port 0", func(s *SelfInput) { s.BackendPort = 0 }, CodeSelfInput, "self backendPort"},
		{"port above 65535", func(s *SelfInput) { s.BackendPort = 65536 }, CodeSelfInput, "self backendPort"},
		{"empty trust", func(s *SelfInput) { s.TrustPEM = nil }, CodeSelfInput, "self trustPEM"},
		{"trust with only a private key", func(s *SelfInput) { s.TrustPEM = keyOnly }, CodeSelfInput, "self trustPEM"},
		{"no server name", func(s *SelfInput) { s.ServerName = "" }, CodeSelfInput, "self serverName"},
		{"no backend host", func(s *SelfInput) { s.BackendHost = "" }, CodeSelfInput, "self backendHost"},
		{"a base path", func(s *SelfInput) { s.BasePath = "/tp" }, CodeSelfInput, "self basePath"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := selfRouteInput(true)
			tc.edit(in.Self)
			res, err := Render(in)
			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Render = %+v, %v; want a refusal", res.Skipped, err)
			}
			if refused.Code != tc.code || refused.Field != tc.field {
				t.Errorf("refused %s on %q, want %s on %q", refused.Code, refused.Field, tc.code, tc.field)
			}
		})
	}

	// Strings and the base path are refused even while TierPlan is not attached; an incomplete
	// not-attached input (no port, no trust yet) renders, because nothing of it is written.
	t.Run("not attached: a brace still fails", func(t *testing.T) {
		in := selfRouteInput(false)
		in.Self.Host = "tp{.apps.example.org"
		if _, err := Render(in); err == nil {
			t.Error("a not-attached self input with a brace rendered")
		}
	})
	t.Run("not attached: a base path still fails", func(t *testing.T) {
		in := selfRouteInput(false)
		in.Self.BasePath = "/tp"
		if _, err := Render(in); err == nil {
			t.Error("a not-attached self input with a base path rendered")
		}
	})
	t.Run("not attached: incomplete input renders", func(t *testing.T) {
		in := selfRouteInput(false)
		in.Self.BackendPort = 0
		in.Self.TrustPEM = nil
		if _, err := Render(in); err != nil {
			t.Errorf("Render: %v", err)
		}
	})
	t.Run("a trust bundle with a key before the certificate renders", func(t *testing.T) {
		in := selfRouteInput(true)
		in.Self.TrustPEM = append(slices.Clone(keyOnly), trustSelf...)
		if _, err := Render(in); err != nil {
			t.Errorf("Render: %v", err)
		}
	})
}

// TestSystemClaimWithoutSelfInputSkips is D-03: a system claim's route in a render with no self input,
// a self input that is not attached, or a self input for another host is skipped as self_not_attached,
// never no_backend, and nothing of TierPlan's route reaches the file.
func TestSystemClaimWithoutSelfInputSkips(t *testing.T) {
	other := selfInput(true)
	other.Host = "elsewhere.apps.example.org"
	for name, self := range map[string]*SelfInput{
		"nil":          nil,
		"not attached": selfInput(false),
		"another host": other,
	} {
		t.Run(name, func(t *testing.T) {
			in := selfRouteInput(true)
			in.Self = self
			res := mustRender(t, in)
			want := []Skipped{{Hostname: "tp.apps.example.org", PathPrefix: "/", Service: "tierplan", Reason: SkipSelfNotAttached}}
			if !slices.Equal(res.Skipped, want) {
				t.Errorf("Skipped = %+v, want %+v", res.Skipped, want)
			}
			raw, _ := dynamicOf(t, res.Bundle)
			for _, s := range []string{selfRouterName, selfServiceName, SelfTransport, "rootCAs"} {
				if strings.Contains(string(raw), s) {
					t.Errorf("the dynamic file names %s:\n%s", s, raw)
				}
			}
			if len(res.Bundle.Manifest.Routes) != 0 {
				t.Errorf("manifest routes = %+v, want none", res.Bundle.Manifest.Routes)
			}
		})
	}
}
