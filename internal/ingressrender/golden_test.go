// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// update rewrites the golden files from the current output: go test ./internal/ingressrender -update.
// Read the diff before committing it; a golden is the byte-for-byte contract of what a node is given.
var update = flag.Bool("update", false, "rewrite the golden files")

const goldenDir = "testdata/golden"

// goldenScenarios is one fixed Input per golden directory. Later plans add scenarios here.
func goldenScenarios() map[string]Input {
	return map[string]Input{
		// An environment with ingress on and nothing claimed: the dynamic file still holds strict SNI (D-23).
		"empty": {
			Snapshot: ingress.Snapshot{Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 1}},
		},
		// One hostname, the root and one path under it, one certificate: two routers, one shared
		// service, one certificate pair.
		"one-host": {
			Snapshot: ingress.Snapshot{
				Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 4},
				Claims:   []ingress.SnapshotClaim{claimFor("app.example.com")},
				Routes: []ingress.SnapshotRoute{
					swarmRoute("app.example.com", "/", "shop", "web", 8080),
					swarmRoute("app.example.com", "/shop", "shop", "web", 8080),
				},
			},
			Certs:        []HostCert{{Hostname: "app.example.com", ChainPEM: chainA, KeyPEM: keyA}},
			CertsVersion: 2,
		},
		// One host with nested prefixes: the priorities are 1, 1004 and 2007, the longest segment prefix wins (D-16).
		"overlap": {
			Snapshot: ingress.Snapshot{
				Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 5},
				Claims:   []ingress.SnapshotClaim{claimFor("app.example.com")},
				Routes: []ingress.SnapshotRoute{
					swarmRoute("app.example.com", "/api/v2", "shop", "api", 9000),
					swarmRoute("app.example.com", "/", "shop", "web", 8080),
					swarmRoute("app.example.com", "/api", "shop", "api", 9000),
				},
			},
			Certs:        []HostCert{{Hostname: "app.example.com", ChainPEM: chainA, KeyPEM: keyA}},
			CertsVersion: 2,
		},
		// A host whose certificate is not ready is left out whole (two routes, both skipped) and the ready
		// host beside it is rendered alone (D-10).
		"not-ready": {
			Snapshot: ingress.Snapshot{
				Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 6},
				Claims:   []ingress.SnapshotClaim{claimFor("pending.example.com"), claimFor("ready.example.com")},
				Routes: []ingress.SnapshotRoute{
					swarmRoute("pending.example.com", "/", "shop", "web", 8080),
					swarmRoute("pending.example.com", "/api", "shop", "api", 9000),
					swarmRoute("ready.example.com", "/", "shop", "web", 8080),
				},
			},
			Certs: []HostCert{
				{Hostname: "pending.example.com", Reason: SkipCertificateNotReady},
				{Hostname: "ready.example.com", ChainPEM: chainA, KeyPEM: keyA},
			},
			CertsVersion: 3,
		},
		// A staging certificate is reported by Serving as certificate_not_ready, so it never serves: with
		// this the only host, the dynamic file is the empty bundle's (PROH-21-01).
		"staging": {
			Snapshot: ingress.Snapshot{
				Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 7},
				Claims:   []ingress.SnapshotClaim{claimFor("staging.example.com")},
				Routes:   []ingress.SnapshotRoute{swarmRoute("staging.example.com", "/", "shop", "web", 8080)},
			},
			Certs:        []HostCert{{Hostname: "staging.example.com", Reason: SkipCertificateNotReady}},
			CertsVersion: 4,
		},
		// A standalone host's route is served through its alias (Phase 25, D-15).
		"standalone-served": {
			Snapshot: ingress.Snapshot{
				Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 8},
				Claims:   []ingress.SnapshotClaim{standaloneClaim("old.example.com")},
				Routes:   []ingress.SnapshotRoute{standaloneRoute("old.example.com", "/", "legacy", "app", 80)},
			},
			Certs:        []HostCert{{Hostname: "old.example.com", ChainPEM: chainA, KeyPEM: keyA}},
			CertsVersion: 5,
		},
		// The root and a path under it with strip: the /api router lists its own stripPrefix middleware
		// and the root router lists none (LBL-07, D-17).
		"strip": stripInput(),
		// TierPlan's own route (D-09, D-14): router tierplan-self minus the two connect paths, service
		// svc-tierplan-self to TierPlan on the self network through t-self, which verifies by name against
		// the trust file. No middleware (D-15) and no skip key.
		"self-route": selfRouteInput(true),
		// The same claim while TierPlan is not on the self network (D-03): no router, no service, no
		// transport, and the route is skipped as self_not_attached.
		"self-not-attached": selfRouteInput(false),
		// The self route with TierPlan's own HTTP-01 carve-out (D-12): router tierplan-acme on web only,
		// for the two ACME hosts on the challenge path, at CarveOutPriority with no tls, to
		// svc-tierplan-plain, TierPlan's plain listener. Everything else is the self-route golden's.
		"self-acme-carveout": selfACMEInput(),
		// One https route on port 8443: its service names the user transport and the bundle holds it (D-13).
		"https-backend": {
			Snapshot: ingress.Snapshot{
				Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 10},
				Claims:   []ingress.SnapshotClaim{claimFor("secure.example.com")},
				Routes:   []ingress.SnapshotRoute{httpsRoute("secure.example.com", "/", "shop", "web", 8443)},
			},
			Certs:        []HostCert{{Hostname: "secure.example.com", ChainPEM: chainA, KeyPEM: keyA}},
			CertsVersion: 2,
		},
	}
}

// trustSelf is the fixture trust bundle of TierPlan's own certificate: one PEM CERTIFICATE block. The
// renderer checks only that a certificate block is there.
var trustSelf = []byte("-----BEGIN CERTIFICATE-----\nU0VMRg==\n-----END CERTIFICATE-----\n")

// systemRoute is the one route row of TierPlan's system claim, as the store writes it: prefix /,
// service tierplan, port 7480, https, no stack.
func systemRoute(host string) ingress.SnapshotRoute {
	return ingress.SnapshotRoute{
		Route: ingress.Route{
			Hostname: host, Name: "tierplan", PathPrefix: "/", Service: "tierplan", Port: 7480, Scheme: ingress.SchemeHTTPS,
		},
		OwnerKind: ingress.OwnerSystem,
	}
}

func systemClaim(host string) ingress.SnapshotClaim {
	return ingress.SnapshotClaim{Claim: ingress.Claim{Hostname: host, OwnerKind: ingress.OwnerSystem}}
}

// selfInput is TierPlan on a swarm as the service tierplan_tierplan, listening on 7480, verified as
// tierplan.example.org.
func selfInput(attached bool) *SelfInput {
	return &SelfInput{
		Host:        "tp.apps.example.org",
		BackendHost: "tierplan_tierplan",
		BackendPort: 7480,
		ServerName:  "tierplan.example.org",
		TrustPEM:    trustSelf,
		Attached:    attached,
	}
}

// selfRouteInput is the system claim tp.apps.example.org with a ready certificate and the self input.
func selfRouteInput(attached bool) Input {
	return Input{
		Snapshot: ingress.Snapshot{
			Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 12},
			Claims:   []ingress.SnapshotClaim{systemClaim("tp.apps.example.org")},
			Routes:   []ingress.SnapshotRoute{systemRoute("tp.apps.example.org")},
		},
		Certs:        []HostCert{{Hostname: "tp.apps.example.org", ChainPEM: chainA, KeyPEM: keyA}},
		CertsVersion: 3,
		OwnVersion:   1,
		Self:         selfInput(attached),
	}
}

// selfACMEInput is the self-route scenario with the hosts of the public and agent URLs as ACME hosts.
func selfACMEInput() Input {
	in := selfRouteInput(true)
	in.Self.ACMEHosts = []string{"tierplan.internal", "tierplan.example.org"}
	return in
}

// goldenBundle is the JSON a bundle.json holds: everything of a Result but file data.
type goldenBundle struct {
	ID       string                 `json:"id"`
	Schema   int                    `json:"schema"`
	Version  ingressbundle.Version  `json:"version"`
	Manifest ingressbundle.Manifest `json:"manifest"`
	Skipped  []goldenSkipped        `json:"skipped"`
}

type goldenSkipped struct {
	Hostname   string `json:"hostname"`
	PathPrefix string `json:"pathPrefix"`
	Stack      string `json:"stack"`
	Service    string `json:"service"`
	Reason     string `json:"reason"`
}

func goldenJSON(t *testing.T, res Result) []byte {
	t.Helper()
	skipped := make([]goldenSkipped, 0, len(res.Skipped))
	for _, s := range res.Skipped {
		skipped = append(skipped, goldenSkipped{s.Hostname, s.PathPrefix, s.Stack, s.Service, string(s.Reason)})
	}
	out, err := json.MarshalIndent(goldenBundle{
		ID: res.Bundle.ID, Schema: res.Bundle.Schema, Version: res.Bundle.Version,
		Manifest: res.Bundle.Manifest, Skipped: skipped,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func dynamicBytes(t *testing.T, res Result) []byte {
	t.Helper()
	for _, f := range res.Bundle.Files {
		if f.Path == ingressbundle.DynamicFile {
			return f.Data
		}
	}
	t.Fatalf("bundle has no %s", ingressbundle.DynamicFile)
	return nil
}

// TestGolden pins the dynamic file and the manifest of every scenario byte for byte (D-07).
func TestGolden(t *testing.T) {
	scenarios := goldenScenarios()

	// A golden directory with no scenario is a stale file that nothing checks.
	dirs, err := os.ReadDir(goldenDir)
	if err != nil && !*update {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if _, ok := scenarios[d.Name()]; !ok {
			t.Errorf("%s/%s has no scenario in goldenScenarios", goldenDir, d.Name())
		}
	}

	for _, name := range slices.Sorted(maps.Keys(scenarios)) {
		t.Run(name, func(t *testing.T) {
			res, err := Render(scenarios[name])
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			files := map[string][]byte{
				"tierplan.yml": dynamicBytes(t, res),
				"bundle.json":  goldenJSON(t, res),
			}
			dir := filepath.Join(goldenDir, name)
			if *update {
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range slices.Sorted(maps.Keys(files)) {
				path := filepath.Join(dir, file)
				if *update {
					if err := os.WriteFile(path, files[file], 0o600); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata
				if err != nil {
					t.Fatalf("read golden: %v (run with -update to create it)", err)
				}
				if !bytes.Equal(files[file], want) {
					t.Errorf("%s differs from the golden file.\n--- got ---\n%s\n--- want ---\n%s", path, files[file], want)
				}
			}
		})
	}
}
