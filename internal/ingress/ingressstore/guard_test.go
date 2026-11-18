// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/store"
)

// seedLedgerRow puts a DNS ledger row of a hostname certificate, made with the token of domain domainID, in the
// ledger.
func seedLedgerRow(t *testing.T, st *store.Store, id, domainID string) {
	t.Helper()
	if _, err := st.Write.Exec(
		`INSERT INTO acme_dns_records (id, provider, zone_id, record_id, name, value, owner_kind, owner_id, created_at, credential_ref)
		 VALUES (?, 'cloudflare', 'zone-1', 'rec-1', '_acme-challenge.app.example.com', 'v', 'order', 'order-1', 1, ?)`,
		id, domainID); err != nil {
		t.Fatalf("seed ledger row %s: %v", id, err)
	}
}

// seedLiveCert puts a certificate row of a hostname under domainID; retired rows carry a retired_at.
func seedLiveCert(t *testing.T, st *store.Store, id, host, envID, domainID string, retiredAt int64) {
	t.Helper()
	if _, err := st.Write.Exec(
		`INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, created_at, updated_at, retired_at) VALUES (?, ?, ?, ?, 'acme', 1, 1, ?)`,
		id, host, envID, domainID, retiredAt); err != nil {
		t.Fatalf("seed certificate %s: %v", id, err)
	}
}

// D-27: a token is never replaced while a record made with it is still in the ledger, so a record is never
// orphaned from the token that can delete it. With no row the replacement works as before.
func TestDomainTokenReplaceWaitsForItsRecords(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	d, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-1", Suffix: "example.com", Issuer: "staging", Token: []byte("old-token"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	seedLedgerRow(t, st, "row-1", d.ID)

	_, err = s.SetDomainToken(ctx, "env-1", d.ID, []byte("new-token"), "admin", nil)
	var pending *CleanupPendingError
	if !errors.As(err, &pending) || pending.Records != 1 {
		t.Fatalf("SetDomainToken = %v, want *CleanupPendingError with 1 record", err)
	}
	if got, err := s.DomainToken(ctx, "env-1", d.ID); err != nil || string(got) != "old-token" {
		t.Fatalf("DomainToken = %q, %v, want the old token kept", got, err)
	}

	if _, err := st.Write.Exec(`DELETE FROM acme_dns_records WHERE id = 'row-1'`); err != nil {
		t.Fatalf("delete the row: %v", err)
	}
	if _, err := s.SetDomainToken(ctx, "env-1", d.ID, []byte("new-token"), "admin", nil); err != nil {
		t.Fatalf("SetDomainToken with no ledger row: %v", err)
	}
	if got, err := s.DomainToken(ctx, "env-1", d.ID); err != nil || string(got) != "new-token" {
		t.Fatalf("DomainToken = %q, %v, want the new token", got, err)
	}
}

// D-27: only the rows made with this domain's token count; another domain's rows block neither its delete nor its
// token replacement.
func TestAnotherDomainsRecordsDoNotBlock(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	a, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-1", Suffix: "a.example", Issuer: "staging", Token: []byte("tok-a"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("CreateDomain a: %v", err)
	}
	b, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-1", Suffix: "b.example", Issuer: "staging", Token: []byte("tok-b"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("CreateDomain b: %v", err)
	}
	seedLedgerRow(t, st, "row-b", b.ID)

	if _, err := s.SetDomainToken(ctx, "env-1", a.ID, []byte("tok-a2"), "admin", nil); err != nil {
		t.Fatalf("SetDomainToken(a) = %v, want success: the row is b's", err)
	}
	if err := s.DeleteDomain(ctx, "env-1", a.ID, nil); err != nil {
		t.Fatalf("DeleteDomain(a) = %v, want success: the row is b's", err)
	}
	var pending *CleanupPendingError
	if err := s.DeleteDomain(ctx, "env-1", b.ID, nil); !errors.As(err, &pending) {
		t.Fatalf("DeleteDomain(b) = %v, want *CleanupPendingError", err)
	}
}

// D-28: the domain list counts the live certificates under each domain; a retired row is not counted.
func TestDomainsCountTheirLiveCertificates(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	a, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-1", Suffix: "a.example", Issuer: "staging", Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("CreateDomain a: %v", err)
	}
	b, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-1", Suffix: "b.example", Issuer: "staging", Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("CreateDomain b: %v", err)
	}
	seedLiveCert(t, st, "c1", "one.a.example", "env-1", a.ID, 0)
	seedLiveCert(t, st, "c2", "two.a.example", "env-1", a.ID, 0)
	seedLiveCert(t, st, "c3", "three.a.example", "env-1", a.ID, 5)
	seedLiveCert(t, st, "c4", "one.b.example", "env-1", b.ID, 0)

	views, err := s.Domains(ctx, "env-1")
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	got := map[string]int{}
	for _, v := range views {
		got[v.Suffix] = v.Certificates
	}
	if got["a.example"] != 2 || got["b.example"] != 1 {
		t.Errorf("certificates = %v, want a.example 2 (the retired row not counted) and b.example 1", got)
	}
}
