// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// richestInput has overlapping prefixes, a strip route, an https route, a shared backend, two hosts with
// certificates, and routes skipped as no_certificate, certificate_not_ready and self_not_attached.
func richestInput() Input {
	strip := swarmRoute("app.example.com", "/api", "shop", "api", 9000)
	strip.Strip = true
	// A system-owned route with no self input in the render is skipped as self_not_attached.
	system := swarmRoute("sys.example.com", "/", "", "ui", 8080)
	system.OwnerKind = ingress.OwnerSystem
	system.StackName = ""
	system.Target = ""
	return Input{
		Snapshot: ingress.Snapshot{
			Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 11},
			Claims: []ingress.SnapshotClaim{
				claimFor("app.example.com"), claimFor("other.example.com"), claimFor("pending.example.com"),
				claimFor("bare.example.com"), standaloneClaim("old.example.com"),
				{Claim: ingress.Claim{Hostname: "sys.example.com", OwnerKind: ingress.OwnerSystem}},
			},
			Routes: []ingress.SnapshotRoute{
				swarmRoute("app.example.com", "/", "shop", "web", 8080),
				strip,
				swarmRoute("app.example.com", "/api/v2", "shop", "api", 9000),
				httpsRoute("app.example.com", "/secure", "shop", "vault", 8443),
				swarmRoute("other.example.com", "/", "shop", "web", 8080),
				swarmRoute("other.example.com", "/ab", "shop", "web", 8080),
				swarmRoute("pending.example.com", "/", "shop", "web", 8080),
				swarmRoute("bare.example.com", "/", "shop", "web", 8080),
				standaloneRoute("old.example.com", "/", "legacy", "app", 80),
				system,
			},
		},
		Certs: []HostCert{
			{Hostname: "app.example.com", ChainPEM: chainA, KeyPEM: keyA},
			{Hostname: "other.example.com", ChainPEM: chainA, KeyPEM: keyA},
			{Hostname: "pending.example.com", Reason: SkipCertificateNotReady},
			{Hostname: "old.example.com", ChainPEM: chainA, KeyPEM: keyA},
			{Hostname: "sys.example.com", ChainPEM: chainA, KeyPEM: keyA},
		},
		CertsVersion: 5,
	}
}

// TestDeterministicShuffled pins D-07 for the input order: 50 seeded shuffles of the claims, routes and
// certificates give the same files, the same ID and the same skipped routes. The seed is in every
// failure message, so a failure reproduces.
func TestDeterministicShuffled(t *testing.T) {
	base := richestInput()
	want := mustRender(t, base)
	if len(want.Skipped) < 3 || len(want.Bundle.Manifest.Routes) < 4 {
		t.Fatalf("the fixture is too poor to prove anything: %d skipped, %d rendered", len(want.Skipped), len(want.Bundle.Manifest.Routes))
	}

	for seed := uint64(1); seed <= 50; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec // G404: a seeded generator on purpose, so a failure reproduces
		in := base
		in.Snapshot.Claims = slices.Clone(base.Snapshot.Claims)
		in.Snapshot.Routes = slices.Clone(base.Snapshot.Routes)
		in.Certs = slices.Clone(base.Certs)
		rng.Shuffle(len(in.Snapshot.Claims), func(i, j int) {
			in.Snapshot.Claims[i], in.Snapshot.Claims[j] = in.Snapshot.Claims[j], in.Snapshot.Claims[i]
		})
		rng.Shuffle(len(in.Snapshot.Routes), func(i, j int) {
			in.Snapshot.Routes[i], in.Snapshot.Routes[j] = in.Snapshot.Routes[j], in.Snapshot.Routes[i]
		})
		rng.Shuffle(len(in.Certs), func(i, j int) { in.Certs[i], in.Certs[j] = in.Certs[j], in.Certs[i] })

		got, err := Render(in)
		if err != nil {
			t.Fatalf("seed %d: Render: %v", seed, err)
		}
		if got.Bundle.ID != want.Bundle.ID {
			t.Fatalf("seed %d: ID %s, want %s", seed, got.Bundle.ID, want.Bundle.ID)
		}
		if !reflect.DeepEqual(got.Bundle.Files, want.Bundle.Files) {
			t.Fatalf("seed %d: the files differ from the unshuffled render", seed)
		}
		if !reflect.DeepEqual(got.Skipped, want.Skipped) {
			t.Fatalf("seed %d: skipped %v, want %v", seed, got.Skipped, want.Skipped)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: the result differs from the unshuffled render", seed)
		}
	}
}
