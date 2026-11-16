// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"errors"
	"maps"
	"testing"
)

// A fold deletes the Local environment's row and every ingress table cascades from it, so a Local
// environment holding ingress state is refused, inside the fold's write transaction (SWM-02,
// D-12, D-17).

// ingressRows is what a refused fold must leave alone: the count of rows in each ingress table.
func (f *foldRig) ingressRows(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"ingress_env", "ingress_domains", "ingress_reserved", "ingress_hostnames", "ingress_routes"} {
		var n int
		if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[tbl] = n
	}
	return out
}

func (f *foldRig) mustExec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.st.Write.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestTracerFoldIsRefusedWhileTheLocalEnvironmentHoldsAnIngressDomain(t *testing.T) {
	f := newFoldRig(t, nil)
	f.mustExec(t, `INSERT INTO ingress_domains (id, env_id, suffix, created_by, created_at, updated_at) VALUES ('dom-1', ?, 'example.org', 'u1', 1, 1)`, f.localEnv.ID)

	err := f.svc.FoldLocal(context.Background(), admin, f.env.ID, "10.0.0.9", "test")
	if !errors.Is(err, ErrFoldIngressActive) {
		t.Fatalf("FoldLocal = %v, want ErrFoldIngressActive", err)
	}
	var ia *IngressActiveError
	if !errors.As(err, &ia) || ia.Domains != 1 {
		t.Fatalf("FoldLocal error %v is not an IngressActiveError with 1 domain", err)
	}
	f.assertNothingFolded(t)
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ingress_domains WHERE id = 'dom-1' AND env_id = ?`, f.localEnv.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("the domain row count = %d, %v after a refused fold, want 1", n, err)
	}
}

func TestFoldIngressRefusalPerKindOfState(t *testing.T) {
	const domain = `INSERT INTO ingress_domains (id, env_id, suffix, created_by, created_at, updated_at) VALUES ('dom-1', ?, 'example.org', 'u1', 1, 1)`
	const reserved = `INSERT INTO ingress_reserved (domain_id, hostname, created_by, created_at) VALUES ('dom-1', 'vault.example.org', 'u1', 1)`
	const hostname = `INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, claimed_by, claimed_at) VALUES ('app.example.org', ?, 'dom-1', 'system', 'u1', 1)`
	const route = `INSERT INTO ingress_routes (id, env_id, hostname, name, path_prefix, service, port, created_at, updated_at) VALUES ('rt-1', ?, 'app.example.org', '', '/', 'web', 80, 1, 1)`
	const ingressOn = `INSERT INTO ingress_env (env_id, enabled, config_version, updated_at, updated_by) VALUES (?, 1, 1, 1, 'u1')`
	const counterOnly = `INSERT INTO ingress_env (env_id, enabled, config_version, updated_at, updated_by) VALUES (?, 0, 3, 1, 'u1')`

	// seed statements take the Local environment's ID when they have a placeholder.
	cases := []struct {
		name string
		seed []string
		want *IngressActiveError // nil: the fold goes through
	}{
		{"a domain only", []string{domain}, &IngressActiveError{Domains: 1}},
		{"a domain and a reserved name", []string{domain, reserved}, &IngressActiveError{Domains: 1, Reserved: 1}},
		{"a hostname claim", []string{domain, hostname}, &IngressActiveError{Domains: 1, Hostnames: 1}},
		{"a hostname and a route", []string{domain, hostname, route}, &IngressActiveError{Domains: 1, Hostnames: 1, Routes: 1}},
		{"ingress turned on and nothing else", []string{ingressOn}, &IngressActiveError{Enabled: true}},
		{"a version counter with ingress off and nothing else", []string{counterOnly}, nil},
		{"nothing at all", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFoldRig(t, nil)
			for _, q := range c.seed {
				if q == reserved {
					f.mustExec(t, q)
				} else {
					f.mustExec(t, q, f.localEnv.ID)
				}
			}
			before := f.ingressRows(t)
			err := f.svc.FoldLocal(context.Background(), admin, f.env.ID, "10.0.0.9", "test")
			if c.want == nil {
				if err != nil {
					t.Fatalf("FoldLocal = %v, want the fold to go through", err)
				}
				if n := len(f.audits(t, "environment.local_folded")); n != 1 {
					t.Errorf("%d audit rows, want 1", n)
				}
				if after := f.ingressRows(t); after["ingress_env"] != 0 {
					t.Errorf("ingress_env rows after the fold = %d, want the Local environment's gone with it", after["ingress_env"])
				}
				return
			}
			if !errors.Is(err, ErrFoldIngressActive) {
				t.Fatalf("FoldLocal = %v, want ErrFoldIngressActive", err)
			}
			var ia *IngressActiveError
			if !errors.As(err, &ia) || *ia != *c.want {
				t.Fatalf("FoldLocal error = %+v, want %+v", ia, c.want)
			}
			f.assertNothingFolded(t)
			if after := f.ingressRows(t); !maps.Equal(after, before) {
				t.Errorf("ingress rows after a refused fold = %v, want %v", after, before)
			}
		})
	}
}

func TestFoldIngressStateAddedAfterTheOfferIsStillRefused(t *testing.T) {
	f := newFoldRig(t, nil)
	if !f.offered(t, f.env.ID) {
		t.Fatal("the fold is not offered before the domain is added")
	}
	f.mustExec(t, `INSERT INTO ingress_domains (id, env_id, suffix, created_by, created_at, updated_at) VALUES ('dom-late', ?, 'example.org', 'u1', 1, 1)`, f.localEnv.ID)

	err := f.svc.FoldLocal(context.Background(), admin, f.env.ID, "10.0.0.9", "test")
	if !errors.Is(err, ErrFoldIngressActive) {
		t.Fatalf("FoldLocal = %v, want ErrFoldIngressActive for a domain added after the offer", err)
	}
	f.assertNothingFolded(t)
}
