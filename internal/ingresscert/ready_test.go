// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// certAt is the PEM chain and PKCS#8 key of a self-signed certificate for host that is valid from nb to na.
func certAt(t *testing.T, host string, nb, na time.Time) (chainPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: nb, NotAfter: na,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
}

// putCert stores a certificate for host in env-1 as a live row of the source and issuer (an upload has none),
// valid from nb to na, written at fixedNow.
func putCert(t *testing.T, s *Store, host, source, issuer string, nb, na time.Time) Cert {
	t.Helper()
	ctx := context.Background()
	chainPEM, keyPEM := certAt(t, host, nb, na)
	var id string
	if source == SourceUpload {
		id = uuid.NewString()
		ts := toNanos(fixedNow)
		if err := s.st.Q.InsertIngressCertUploaded(ctx, sqlc.InsertIngressCertUploadedParams{
			ID: id, Hostname: host, EnvID: "env-1", DomainID: "dom-1", CreatedBy: "tester", CreatedAt: ts, UpdatedAt: ts,
		}); err != nil {
			t.Fatalf("insert the upload of %s: %v", host, err)
		}
	} else {
		c, _, err := s.CreatePending(ctx, PendingInput{Hostname: host, EnvID: "env-1", DomainID: "dom-1", Issuer: issuer, CreatedBy: "tester"}, nil)
		if err != nil {
			t.Fatalf("CreatePending(%s): %v", host, err)
		}
		id = c.ID
	}
	if err := s.write(ctx, []string{"env-1"}, func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		return true, s.writeCertificate(ctx, q, id, chainPEM, keyPEM, "https://acme.test/acct/1", fixedNow)
	}); err != nil {
		t.Fatalf("write the certificate of %s: %v", host, err)
	}
	c, err := s.Cert(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// retire retires row c the way an issuer change does.
func retire(t *testing.T, st *store.Store, c Cert) {
	t.Helper()
	if _, err := st.Write.Exec(`UPDATE ingress_certs SET retired_at = ?, retired_reason = 'issuer_changed' WHERE id = ?`, toNanos(fixedNow), c.ID); err != nil {
		t.Fatal(err)
	}
}

// readyManager is a Manager that has only what refreshReady needs, on a clock the test moves.
func readyManager(s *Store) (*Manager, *acmetest.Clock) {
	clock := acmetest.NewClock(fixedNow)
	return &Manager{d: Deps{Store: s}, clock: clock, log: slog.New(slog.DiscardHandler)}, clock
}

// D-20, CRT-09: the readiness rule at its edges.
func TestReady(t *testing.T) {
	now := fixedNow
	day := 24 * time.Hour
	chain := []byte("chain")
	for _, tc := range []struct {
		name  string
		c     Cert
		opens bool
		want  bool
	}{
		{"production with 25h left", Cert{Source: SourceACME, Issuer: IssuerProduction, ChainPEM: chain, NotBefore: now.Add(-day), NotAfter: now.Add(25 * time.Hour)}, true, true},
		{"production with 23h left", Cert{Source: SourceACME, Issuer: IssuerProduction, ChainPEM: chain, NotBefore: now.Add(-day), NotAfter: now.Add(23 * time.Hour)}, true, false},
		{"production with exactly the margin left", Cert{Source: SourceACME, Issuer: IssuerProduction, ChainPEM: chain, NotBefore: now.Add(-day), NotAfter: now.Add(ReadyMargin)}, true, false},
		{"production not yet valid", Cert{Source: SourceACME, Issuer: IssuerProduction, ChainPEM: chain, NotBefore: now.Add(time.Hour), NotAfter: now.Add(90 * day)}, true, false},
		{"production valid from now", Cert{Source: SourceACME, Issuer: IssuerProduction, ChainPEM: chain, NotBefore: now, NotAfter: now.Add(90 * day)}, true, true},
		{"staging with 80 days left", Cert{Source: SourceACME, Issuer: IssuerStaging, ChainPEM: chain, NotBefore: now.Add(-day), NotAfter: now.Add(80 * day)}, true, false},
		{"upload with 30 days left", Cert{Source: SourceUpload, ChainPEM: chain, NotBefore: now.Add(-day), NotAfter: now.Add(30 * day)}, true, true},
		{"upload with 23h left", Cert{Source: SourceUpload, ChainPEM: chain, NotBefore: now.Add(-day), NotAfter: now.Add(23 * time.Hour)}, true, false},
		{"key does not open", Cert{Source: SourceACME, Issuer: IssuerProduction, ChainPEM: chain, NotBefore: now.Add(-day), NotAfter: now.Add(90 * day)}, false, false},
		{"no chain", Cert{Source: SourceACME, Issuer: IssuerProduction, NotBefore: now.Add(-day), NotAfter: now.Add(90 * day)}, true, false},
		{"no dates", Cert{Source: SourceACME, Issuer: IssuerProduction, ChainPEM: chain}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Ready(tc.c, now, tc.opens); got != tc.want {
				t.Errorf("Ready = %v, want %v", got, tc.want)
			}
		})
	}
}

// D-20: the one word for where a certificate stands, and the words for the staging state.
func TestStateOf(t *testing.T) {
	now := fixedNow
	day := 24 * time.Hour
	chain := []byte("chain")
	valid := func(c Cert) Cert {
		c.ChainPEM, c.NotBefore, c.NotAfter = chain, now.Add(-day), now.Add(60*day)
		return c
	}
	acmeProd := Cert{Source: SourceACME, Issuer: IssuerProduction, Phase: PhaseIdle}
	for _, tc := range []struct {
		name string
		c    Cert
		want string
	}{
		{"staging with a chain", valid(Cert{Source: SourceACME, Issuer: IssuerStaging, Phase: PhaseIdle}), StateDNSProvenOnStaging},
		{"production with a chain", valid(acmeProd), StateValid},
		{"queued, no chain", Cert{Source: SourceACME, Issuer: IssuerProduction, Phase: PhaseQueued}, StateQueued},
		{"queued, no chain, failures", Cert{Source: SourceACME, Issuer: IssuerProduction, Phase: PhaseQueued, ConsecutiveFailures: 2}, StateFailed},
		{"a chain with failures", valid(Cert{Source: SourceACME, Issuer: IssuerProduction, Phase: PhaseQueued, ConsecutiveFailures: 1}), StateRenewalFailed},
		{"ordering, no chain", Cert{Source: SourceACME, Issuer: IssuerProduction, Phase: PhaseOrdering}, StateOrdering},
		{"a chain being renewed", valid(Cert{Source: SourceACME, Issuer: IssuerProduction, Phase: PhaseOrdering}), StateOrdering},
		{"an upload", valid(Cert{Source: SourceUpload, Phase: PhaseIdle}), StateUploaded},
		{"stopped", Cert{Source: SourceACME, Issuer: IssuerProduction, Phase: PhaseStopped, StoppedReason: "domain_removed"}, StateStopped},
		{"retired", valid(Cert{Source: SourceACME, Issuer: IssuerProduction, Phase: PhaseIdle, RetiredAt: now.Add(-time.Hour)}), StateRetired},
		{"past not_after", Cert{Source: SourceACME, Issuer: IssuerProduction, Phase: PhaseIdle, ChainPEM: chain, NotBefore: now.Add(-90 * day), NotAfter: now.Add(-time.Second)}, StateExpired},
		{"an upload past not_after", Cert{Source: SourceUpload, Phase: PhaseIdle, ChainPEM: chain, NotBefore: now.Add(-90 * day), NotAfter: now.Add(-time.Second)}, StateExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StateOf(tc.c, now); got != tc.want {
				t.Errorf("StateOf = %q, want %q", got, tc.want)
			}
		})
	}
	if got := StateText(StateDNSProvenOnStaging); got != "DNS proven on staging" {
		t.Errorf("StateText(%s) = %q", StateDNSProvenOnStaging, got)
	}
	for _, s := range []string{StateQueued, StateOrdering, StateValid, StateUploaded, StateRenewalFailed, StateFailed, StateExpired, StateStopped, StateRetired} {
		if StateText(s) == "" || StateText(s) == s {
			t.Errorf("StateText(%s) = %q, want words for a person", s, StateText(s))
		}
	}
}

// D-41: a certificate that crosses the margin by time alone has its stored flag cleared by the manager and
// certs_version raised once; nothing else about the row changes, and a second look changes nothing.
func TestReadyFlipRaisesCertsVersion(t *testing.T) {
	ctx := context.Background()
	s, st := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	c := putCert(t, s, "app.example.com", SourceACME, IssuerProduction, fixedNow.Add(-time.Hour), fixedNow.Add(10*24*time.Hour))
	if !c.Ready {
		t.Fatal("a production certificate with 10 days left is written ready")
	}
	m, clock := readyManager(s)

	// Nothing changes while the margin holds.
	before, _ := versions(t, st, "env-1")
	m.refreshReady(ctx)
	if after, _ := versions(t, st, "env-1"); after != before {
		t.Fatalf("certs_version %d -> %d with nothing changed", before, after)
	}

	// 23 hours left.
	clock.Advance(c.NotAfter.Add(-23 * time.Hour).Sub(fixedNow))
	configBefore := func() int64 { _, cfg := versions(t, st, "env-1"); return cfg }()
	m.refreshReady(ctx)
	got, err := s.Cert(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ready {
		t.Errorf("ready is still set with 23h left")
	}
	if after, cfg := versions(t, st, "env-1"); after != before+1 || cfg != configBefore {
		t.Errorf("certs_version %d -> %d, config %d -> %d, want one raise and config unmoved", before, after, configBefore, cfg)
	}
	want := c
	want.Ready, want.UpdatedAt = false, got.UpdatedAt
	if !bytes.Equal(want.ChainPEM, got.ChainPEM) || want.KeySealed != got.KeySealed || want.Phase != got.Phase ||
		want.Serial != got.Serial || !want.NotAfter.Equal(got.NotAfter) || !want.RenewAt.Equal(got.RenewAt) {
		t.Errorf("the flag was the only change expected; row = %+v", got)
	}
	m.refreshReady(ctx)
	if again, _ := versions(t, st, "env-1"); again != before+1 {
		t.Errorf("a second look raised certs_version to %d, want it to stay at %d", again, before+1)
	}

	// A flag that is clear while the certificate is ready by time is set again (a restored database, say), and
	// that raises the version too.
	if _, err := st.Write.Exec(`UPDATE ingress_certs SET ready = 0 WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	clock.Advance(-48 * time.Hour)
	m.refreshReady(ctx)
	if got, _ := s.Cert(ctx, c.ID); !got.Ready {
		t.Errorf("ready is not set again with %v left", got.NotAfter.Sub(clock.Now()))
	}
	if again, _ := versions(t, st, "env-1"); again != before+2 {
		t.Errorf("certs_version = %d, want %d after the flag was set again", again, before+2)
	}

	// A key that does not open keeps a certificate from becoming ready.
	if _, err := st.Write.Exec(`UPDATE ingress_certs SET ready = 0, key_sealed = 'not a vault envelope' WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	m.refreshReady(ctx)
	if got, _ := s.Cert(ctx, c.ID); got.Ready {
		t.Errorf("a certificate whose key does not open is ready")
	}
	if again, _ := versions(t, st, "env-1"); again != before+2 {
		t.Errorf("certs_version = %d, want %d: nothing changed", again, before+2)
	}
}

// Pattern 7, the boundary Phase 21 reads: the live row when it is ready, else a retired production or uploaded
// row that still is, with its key opened; a hostname with nothing ready is left out.
func TestServablePrefersTheLiveRowThenARetiredOne(t *testing.T) {
	ctx := context.Background()
	s, st := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	day := 24 * time.Hour
	nb := fixedNow.Add(-day)

	live := putCert(t, s, "live.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(60*day))
	upload := putCert(t, s, "upload.example.com", SourceUpload, "", nb, fixedNow.Add(30*day))

	// A hostname whose live row is not ready yet and whose retired row still is.
	old := putCert(t, s, "swap.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(20*day))
	retire(t, st, old)
	if _, _, err := s.CreatePending(ctx, PendingInput{Hostname: "swap.example.com", EnvID: "env-1", DomainID: "dom-1", Issuer: IssuerProduction, CreatedBy: "tester"}, nil); err != nil {
		t.Fatal(err)
	}

	// A hostname whose live row is about to expire and whose retired row is good.
	retiredGood := putCert(t, s, "expiring.example.com", SourceUpload, "", nb, fixedNow.Add(40*day))
	retire(t, st, retiredGood)
	expiring := putCert(t, s, "expiring.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(12*time.Hour))
	if expiring.Ready {
		t.Fatal("a certificate with 12h left is written not ready")
	}

	// Both the live and a retired row are ready: the live one wins.
	both := putCert(t, s, "both.example.com", SourceUpload, "", nb, fixedNow.Add(30*day))
	retire(t, st, both)
	liveBoth := putCert(t, s, "both.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(60*day))

	// A live row whose key does not open is not served.
	broken := putCert(t, s, "broken.example.com", SourceACME, IssuerProduction, nb, fixedNow.Add(60*day))
	if _, err := st.Write.Exec(`UPDATE ingress_certs SET key_sealed = 'not a vault envelope' WHERE id = ?`, broken.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.Servable(ctx, "env-1")
	if err != nil {
		t.Fatalf("Servable: %v", err)
	}
	byHost := map[string]ServableCert{}
	for _, sc := range got {
		byHost[sc.Hostname] = sc
		if sc.Key == nil {
			t.Errorf("%s has no opened key", sc.Hostname)
		}
	}
	check := func(host, wantSource string, wantChain []byte) {
		t.Helper()
		sc, ok := byHost[host]
		if !ok {
			t.Errorf("%s is not servable", host)
			return
		}
		if sc.Source != wantSource || !bytes.Equal(sc.ChainPEM, wantChain) {
			t.Errorf("%s: source %q, want %q, and the chain of the expected row", host, sc.Source, wantSource)
		}
	}
	check("live.example.com", SourceACME, live.ChainPEM)
	check("upload.example.com", SourceUpload, upload.ChainPEM)
	check("swap.example.com", SourceACME, old.ChainPEM)
	check("expiring.example.com", SourceUpload, retiredGood.ChainPEM)
	check("both.example.com", SourceACME, liveBoth.ChainPEM)
	if _, ok := byHost["broken.example.com"]; ok {
		t.Errorf("a certificate whose key does not open is servable")
	}
	if len(got) != 5 {
		t.Errorf("Servable returned %d hostnames, want 5 (live, upload, swap, expiring through its retired upload, both): %v", len(got), got)
	}
	// Ordered by hostname.
	for i := 1; i < len(got); i++ {
		if got[i-1].Hostname >= got[i].Hostname {
			t.Errorf("Servable is not ordered by hostname: %s before %s", got[i-1].Hostname, got[i].Hostname)
		}
	}
	// Another environment sees none of it.
	seedEnv(t, st, "env-2", "two")
	if other, err := s.Servable(ctx, "env-2"); err != nil || len(other) != 0 {
		t.Errorf("Servable(env-2) = %v, %v, want nothing", other, err)
	}
}

// CRT-09, PROH-20-02: a staging certificate is never ready and never servable, however long it has left, even
// when its stored flag is forced on, and the manager clears such a flag.
func TestStagingIsNeverServable(t *testing.T) {
	ctx := context.Background()
	s, st := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	day := 24 * time.Hour
	stg := putCert(t, s, "stg.example.com", SourceACME, IssuerStaging, fixedNow.Add(-day), fixedNow.Add(80*day))
	if stg.Ready {
		t.Fatal("a staging certificate is written ready")
	}
	if StateOf(stg, fixedNow) != StateDNSProvenOnStaging || StateText(StateOf(stg, fixedNow)) != "DNS proven on staging" {
		t.Errorf("state = %q, want DNS proven on staging", StateOf(stg, fixedNow))
	}
	retiredStg := putCert(t, s, "old-stg.example.com", SourceACME, IssuerStaging, fixedNow.Add(-day), fixedNow.Add(80*day))
	retire(t, st, retiredStg)

	if _, err := st.Write.Exec(`UPDATE ingress_certs SET ready = 1`); err != nil {
		t.Fatal(err)
	}
	got, err := s.Servable(ctx, "env-1")
	if err != nil || len(got) != 0 {
		t.Fatalf("Servable = %v, %v, want nothing: staging is never served", got, err)
	}

	m, _ := readyManager(s)
	before, _ := versions(t, st, "env-1")
	m.refreshReady(ctx)
	for _, c := range []Cert{stg, retiredStg} {
		if row, _ := s.Cert(ctx, c.ID); row.Ready {
			t.Errorf("%s is still stored ready after the manager looked", c.Hostname)
		}
	}
	if after, _ := versions(t, st, "env-1"); after != before+1 {
		t.Errorf("certs_version %d -> %d, want one raise for the flags cleared together", before, after)
	}
}
