// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Phase 21, D-19 (tracer): Build constructs the ingress reconciler over the server's stores, notifier and ACME clock and
// runs it in the background. An environment seeded with a claimed swarm route and no certificate has a current bundle
// as soon as the server is up: schema 1, the one dynamic file, and the route left out with no_certificate.
func TestServerStartsTheIngressReconciler(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctx := testsupport.Context(t)
	clock := acmetest.NewClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	opts := []Option{WithACMEClock(clock)}
	cfg := func() map[string]string { return map[string]string{"TIERPLAN_DATA_DIR": dir} }

	// The first process only seeds the data, so the second one meets it at start. Issuing is seeded as paused (as after a
	// restore, D-32): the certificate manager's first round would otherwise give the claimed hostname a row while the
	// reconciler renders, and the route would be left out as certificate_not_ready or no_certificate by a race.
	first, err := Build(ctx, testConfig(t, cfg()), slog.New(slog.DiscardHandler), opts...)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE ingress_cert_state SET paused_reason = 'restore' WHERE id = 1`,
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-r', 'agent', 'Recon env', '', 1, 1, 1)`,
		`INSERT INTO ingress_env (env_id, enabled, config_version, updated_at, updated_by) VALUES ('env-r', 1, 1, 1, 'tester')`,
		`INSERT INTO ingress_domains (id, env_id, suffix, issuer, dns_provider, created_by, created_at, updated_at)
		 VALUES ('dom-r', 'env-r', 'apps.example.com', 'staging', 'cloudflare', 'tester', 1, 1)`,
		`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('stack-r', 'env-r', 'shop', 'swarm', 'tester', 1)`,
		`INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at)
		 VALUES ('web.apps.example.com', 'env-r', 'dom-r', 'stack', 'stack-r', 'tester', 1)`,
		`INSERT INTO ingress_routes (id, env_id, hostname, name, path_prefix, service, port, scheme, strip, created_at, updated_at)
		 VALUES ('route-r', 'env-r', 'web.apps.example.com', 'web', '/', 'web', 8080, 'http', 0, 1, 1)`,
	} {
		if _, err := first.Store().Write.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	srv, err := Build(ctx, testConfig(t, cfg()), slog.New(slog.DiscardHandler), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if srv.ingressRecon == nil {
		t.Fatal("the server built no ingress reconciler")
	}

	// Subscribe before looking, so a start render that lands between the two cannot be missed.
	sub := srv.ingressRecon.Changes().Subscribe()
	defer sub.Close()
	for {
		if _, ok := srv.ingressRecon.Current("env-r"); ok {
			break
		}
		testsupport.Await(t, "the start render of env-r", sub.C())
	}

	b, _ := srv.ingressRecon.Current("env-r")
	if b.Schema != 1 {
		t.Errorf("bundle schema = %d, want 1", b.Schema)
	}
	if len(b.Files) != 1 || b.Files[0].Path != "dynamic/tierplan.yml" {
		t.Errorf("bundle files = %v, want the one dynamic/tierplan.yml", b.Files)
	}
	skipped := srv.ingressRecon.Skipped("env-r")
	if len(skipped) != 1 || skipped[0].Hostname != "web.apps.example.com" || skipped[0].Reason != ingressrender.SkipNoCertificate {
		t.Errorf("skipped = %+v, want web.apps.example.com with no_certificate", skipped)
	}
	if f := srv.ingressRecon.Failures(); len(f) != 0 {
		t.Errorf("failures = %+v, want none", f)
	}
}

// Phase 21, D-19: Close stops the reconciler with the other background goroutines. Close waits for them, so it returns
// only once the reconciler's Run has returned on the ended background context.
func TestServerCloseStopsTheReconciler(t *testing.T) {
	ctx := testsupport.Context(t)
	dir := filepath.Join(t.TempDir(), "data")
	clock := acmetest.NewClock(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	srv, err := Build(ctx, testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler), WithACMEClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if srv.ingressRecon == nil {
		_ = srv.Close()
		t.Fatal("the server built no ingress reconciler")
	}
	done := make(chan error, 1)
	go func() { done <- srv.Close() }()
	if err := testsupport.Await(t, "Close to return", done); err != nil {
		t.Errorf("Close: %v", err)
	}
}
