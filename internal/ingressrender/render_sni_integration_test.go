// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Rendered-bundle cases of the pinned Traefik (21-09, D-21, D-22, D-30). The S1 tests prove what the
// proxy does with hand-written bundles; these prove the renderer's own output uses that behaviour
// correctly. Every test runs one real Traefik on an --internal network, probes it by container IP and
// issues certificates from a throwaway CA, never from an ACME server. The proxy reloads its file
// provider asynchronously, so each observation of a change is a bounded poll, never a sleep.

const (
	// rAppHost is the hostname most of these tests serve.
	rAppHost = "app.test.local"
)

// TestTraefikRenderedOneHostTracer is the tracer of RTE-01: a bundle Render produced for one swarm
// route, applied certificates first and the YAML last, is served by the pinned Traefik started with
// Static(): the known SNI gets the rendered leaf, a GET reaches the backend aliased shop_web, and the
// proxy says it does not check for a new version.
func TestTraefikRenderedOneHostTracer(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	env.StartWhoami(t, traefiktest.WhoamiOptions{Alias: "shop_web", Hostname: "shop-web"})

	res := mustRender(t, Input{
		Snapshot:     snapshotOf(swarmRoute(rAppHost, "/", "shop", "web", 80)),
		Certs:        servable(t, ca, rAppHost),
		CertsVersion: 1,
	})
	if len(res.Skipped) != 0 {
		t.Fatalf("the one route was skipped: %+v", res.Skipped)
	}
	applyBundle(t, env, res.Bundle)
	tr := env.StartTraefik(t, traefiktest.TraefikOptions{Static: Static()})

	var peerCN string
	testsupport.Poll(t, testsupport.Deadline, "a handshake for "+rAppHost+" to succeed", func() (bool, string) { // testsupport: external, Traefik reloads its file provider asynchronously
		st, err := tr.Handshake(rAppHost, ca.Pool())
		if err != nil {
			return false, err.Error()
		}
		if len(st.PeerCertificates) == 0 {
			return false, "no peer certificate"
		}
		peerCN = st.PeerCertificates[0].Subject.CommonName
		return true, ""
	})
	// The handshake verified against the test CA, so the leaf is one this test issued; its subject
	// says it is the one for this hostname and not Traefik's own.
	if peerCN != rAppHost {
		t.Errorf("the peer certificate's subject is %q, want %q", peerCN, rAppHost)
	}

	status, body, _ := tr.Get(t, ca.Pool(), rAppHost, "/shop", nil)
	if status != 200 || !strings.Contains(body, "Hostname: shop-web") {
		t.Errorf("GET https://%s/shop answered %d, want 200 from the backend shop-web; body:\n%s", rAppHost, status, body)
	}

	// The version check is decided at startup; the poll is for the log stream catching up.
	var logs string
	testsupport.Poll(t, testsupport.Deadline, "Traefik's startup log to hold the version-check line", func() (bool, string) { // testsupport: external, the container's log stream
		logs = tr.Logs(t)
		return strings.Contains(logs, "Version check is disabled."), "no version-check line yet"
	})
	if strings.Contains(logs, "Version check is enabled.") {
		t.Error("Traefik logged `Version check is enabled.`")
	}
}

// bodyHostname is the container hostname the echo backend reports in a response body, "" when the
// body has no such line.
func bodyHostname(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if h, ok := strings.CutPrefix(line, "Hostname: "); ok {
			return strings.TrimSpace(h)
		}
	}
	return ""
}

// renderedSite is one Traefik started over a rendered bundle, with the CA that signed its leaves. It
// reuses the polling and refusal helpers of the S1 site, which only need the three fields.
type renderedSite struct {
	*s1Site
}

// startRendered applies b to env, starts the pinned Traefik with the embedded static file and returns
// the site. Applying before the start is what a node supervisor does (D-23): the proxy never runs with
// an empty dynamic directory.
func startRendered(t *testing.T, env *traefiktest.Env, ca *traefiktest.CA, b ingressbundle.Bundle) renderedSite {
	t.Helper()
	applyBundle(t, env, b)
	tr := env.StartTraefik(t, traefiktest.TraefikOptions{Static: Static()})
	return renderedSite{&s1Site{env: env, ca: ca, tr: tr}}
}

// TestTraefikStrictSNI proves RTE-01 on a rendered bundle (D-04, D-10): a hostname whose certificate is
// ready is served, and a hostname whose certificate is not ready (its routes skipped), an unknown name,
// no SNI and an IP address as SNI each fail the handshake with the unrecognized-name alert and no
// certificate presented.
func TestTraefikStrictSNI(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	const ready, waiting, unknown = "ready.test.local", "wait.test.local", "unknown.test.local"

	certs := servable(t, ca, ready)
	certs = append(certs, HostCert{Hostname: waiting, Reason: SkipCertificateNotReady})
	res := mustRender(t, Input{
		Snapshot: snapshotOf(
			swarmRoute(ready, "/", "shop", "web", 80),
			swarmRoute(waiting, "/", "shop", "web", 80),
			swarmRoute(waiting, "/shop", "shop", "web", 80),
		),
		Certs:        certs,
		CertsVersion: 1,
	})
	var skipped []string
	for _, sk := range res.Skipped {
		if sk.Hostname != waiting || sk.Reason != SkipCertificateNotReady {
			t.Errorf("unexpected skipped route %+v", sk)
		}
		skipped = append(skipped, sk.PathPrefix)
	}
	slices.Sort(skipped)
	if !slices.Equal(skipped, []string{"/", "/shop"}) {
		t.Fatalf("skipped path prefixes of %s = %v, want both [/ /shop]", waiting, skipped)
	}

	s := startRendered(t, env, ca, res.Bundle)
	// The ready name succeeds only once the bundle is loaded, so the refusals below are the strict
	// configuration speaking and not a proxy that has not read its files yet.
	s.awaitHandshake(t, ready)
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"the hostname without a ready certificate " + waiting, errOf(s.tr.Handshake(waiting, ca.Pool()))},
		{"unknown SNI " + unknown, errOf(s.tr.Handshake(unknown, ca.Pool()))},
		{"no SNI", errOf(s.tr.Handshake("", ca.Pool()))},
		{"IP address as SNI", s.tr.HandshakeIPSNI(s1Loopback)},
	} {
		requireRefused(t, tc.what, tc.err)
	}
}

// TestTraefikEmptyBundleFailsClosed proves D-23 on a rendered bundle: the bundle Render produces for a
// snapshot with no claims is one dynamic file holding only the strict default options, and a Traefik
// that holds nothing else refuses every handshake the same way.
func TestTraefikEmptyBundleFailsClosed(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)

	res := mustRender(t, emptyInput())
	if len(res.Bundle.Files) != 1 || res.Bundle.Files[0].Path != ingressbundle.DynamicFile {
		t.Fatalf("the empty bundle's files = %v, want exactly %s", filePaths(res.Bundle), ingressbundle.DynamicFile)
	}
	s := startRendered(t, env, ca, res.Bundle)
	// Before the file is read Traefik presents its default certificate, so wait for the alert.
	s.awaitRefusal(t, "unknown.test.local")
	s.requireRefusesUnservedNames(t)
}

// emptyInput is an enabled environment with no claim and no route.
func emptyInput() Input {
	return Input{Snapshot: ingress.Snapshot{Settings: ingress.Settings{Exists: true, Enabled: true, ConfigVersion: 1}}}
}

// TestTraefikRedirect proves RTE-02 on rendered bundles: the web entrypoint answers every plain-HTTP
// request with a permanent redirect to https, the path and the query kept verbatim and a port in the
// Host dropped, for a routed hostname, an unrouted one, and with the empty bundle applied.
func TestTraefikRedirect(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)

	res := mustRender(t, Input{
		Snapshot:     snapshotOf(swarmRoute(rAppHost, "/", "shop", "web", 80)),
		Certs:        servable(t, ca, rAppHost),
		CertsVersion: 1,
	})
	s := startRendered(t, env, ca, res.Bundle)
	// Not needed for the redirect itself, which is an entrypoint's, but it shows the bundle is loaded
	// so the redirect is observed over the rendered configuration.
	s.awaitHandshake(t, rAppHost)

	requireRedirects := func(t *testing.T, when string) {
		t.Helper()
		for _, tc := range []struct{ name, host, target, want string }{
			{"path and query kept verbatim", rAppHost, "/p/q?x=1&a=2", "https://app.test.local/p/q?x=1&a=2"},
			{"a port in the Host is dropped", rAppHost + ":18080", "/p", "https://app.test.local/p"},
			{"a hostname with no route", "other.test.local", "/p", "https://other.test.local/p"},
		} {
			status, location := s.tr.HTTPGet(t, tc.host, tc.target)
			if status != http.StatusMovedPermanently || location != tc.want {
				t.Errorf("%s, %s: GET http://%s%s answered %d Location %q, want 301 %q", when, tc.name, tc.host, tc.target, status, location, tc.want)
			}
		}
	}
	requireRedirects(t, "with the route")

	// The empty bundle replaces the route; the redirect never depended on it.
	applyBundle(t, env, mustRender(t, emptyInput()).Bundle)
	s.awaitRefusal(t, rAppHost)
	requireRedirects(t, "with the empty bundle")
}

// TestTraefikRemovedRouteStopsBeingServed proves PROH-21-05 on the data plane: a route removed by a
// re-render stops being served once the proxy reloads (its path falls to the hostname's / route), and
// a hostname removed entirely fails the handshake. Both are observed by a bounded poll (D-21).
func TestTraefikRemovedRouteStopsBeingServed(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	const gone = "gone.test.local"
	env.StartWhoami(t, traefiktest.WhoamiOptions{Alias: "shop_root", Hostname: "root"})
	env.StartWhoami(t, traefiktest.WhoamiOptions{Alias: "shop_shop", Hostname: "shop"})

	root := swarmRoute(rAppHost, "/", "shop", "root", 80)
	shop := swarmRoute(rAppHost, "/shop", "shop", "shop", 80)
	goneRoute := swarmRoute(gone, "/", "shop", "root", 80)
	certs := servable(t, ca, rAppHost, gone)

	before := mustRender(t, Input{Snapshot: snapshotOf(root, shop, goneRoute), Certs: certs, CertsVersion: 1})
	s := startRendered(t, env, ca, before.Bundle)
	s.awaitHandshake(t, gone)
	s.awaitHandshake(t, rAppHost)
	if status, body, _ := s.tr.Get(t, ca.Pool(), rAppHost, "/shop", nil); status != http.StatusOK || bodyHostname(body) != "shop" {
		t.Fatalf("before the removal GET /shop answered %d from %q, want 200 from the backend shop; body:\n%s", status, bodyHostname(body), body)
	}

	after := mustRender(t, Input{Snapshot: snapshotOf(root), Certs: certs[:1], CertsVersion: 2})
	if after.Bundle.ID == before.Bundle.ID {
		t.Fatalf("removing a route and a hostname left the bundle ID %s unchanged", after.Bundle.ID)
	}
	applyBundle(t, env, after.Bundle)

	testsupport.Poll(t, testsupport.Deadline, "/shop to fall to the root route after the re-render", func() (bool, string) { // testsupport: external, Traefik reloads its file provider asynchronously
		status, body, _, err := s.tr.TryGet(ca.Pool(), rAppHost, "/shop", nil)
		if err != nil {
			return false, err.Error()
		}
		return status == http.StatusOK && bodyHostname(body) == "root", "answered " + http.StatusText(status) + " from " + bodyHostname(body)
	})
	testsupport.Poll(t, testsupport.Deadline, gone+" to fail the handshake after the re-render", func() (bool, string) { // testsupport: external, the same reload
		_, err := s.tr.Handshake(gone, ca.Pool())
		if err == nil {
			return false, "the handshake still succeeds"
		}
		return strings.Contains(err.Error(), unrecognizedName), err.Error()
	})
	_, err := s.tr.Handshake(gone, ca.Pool())
	requireRefused(t, "the removed hostname "+gone, err)
}
