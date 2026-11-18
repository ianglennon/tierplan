// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// rigProvider is the provider id of the fake DNS provider, which is what its ledger rows carry.
const rigProvider = "fake"

// rigToken is the one token the rig's fake provider accepts: the token of the domain the certificates sit under.
const rigToken = "domain-token-4c1e9a" //nolint:gosec // G101: a fake token of a test provider

// rigCredential is the credential reference the rig's solver stamps on its ledger rows (an ingress domain id).
const rigCredential = "dom-1"

// rigNS is the host name the fake zone's nameserver goes by; the checker maps it to the responder.
const rigNS = "ns1.tierplan-test.net"

// issueRig is everything an order of a per-hostname certificate runs against: a store with its vault and audit
// writer, a fake CA that validates DNS-01 by looking the TXT record up at a fake DNS provider, the provider
// (holding the zone example.com), a DNS responder that serves what the provider holds, the shared DNS engine
// over the own ACME store's ledger, and a clock that never sleeps. Later tests of this package reuse it.
type issueRig struct {
	t      *testing.T
	s      *Store
	st     *store.Store
	v      *vault.Vault
	clock  *acmetest.Clock
	fake   *acmetest.Fake
	prov   *dnstest.FakeProvider
	dns    *acmetest.DNSServer
	engine *acme.DNSEngine
	ledger *acme.Store
}

// newIssueRig builds the rig with one environment, "env-1".
func newIssueRig(t *testing.T) *issueRig {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	clock := acmetest.NewClock(fixedNow)
	log := slog.New(slog.DiscardHandler)
	r := &issueRig{t: t, st: st, v: v, clock: clock, prov: dnstest.NewFakeProvider(), dns: acmetest.StartDNSServer(t)}
	r.s = NewStore(StoreDeps{St: st, Vault: v, Now: clock.Now, Audit: audit.NewWriter(st, v, log, clock.Now)})
	r.prov.Sink = r.dns
	r.prov.AddZone("example.com", rigNS)
	r.fake = acmetest.NewFake(t, acmetest.Options{
		Clock: clock, ChallengeType: acme.ChallengeDNS01,
		LookupTXT: func(fqdn string) []string {
			var out []string
			for _, rec := range r.prov.Records() {
				if strings.EqualFold(rec.Name, fqdn) {
					out = append(out, rec.Value())
				}
			}
			return out
		},
	})
	r.ledger = acme.NewStore(st, v)
	r.engine = acme.NewDNSEngine(acme.DNSEngineDeps{
		DNS: acme.DNSDeps{
			Providers:       map[string]dns.Factory{rigProvider: r.prov.Factory(rigToken)},
			NameserverAddrs: func(context.Context, string) ([]string, error) { return []string{r.dns.Addr()}, nil },
			ResolvConf:      func() ([]string, error) { return []string{r.dns.Addr()}, nil },
		},
		Ledger: r.ledger, Clock: clock, Log: log, Jitter: func() float64 { return 0 },
	})
	seedEnv(t, st, "env-1", "one")
	return r
}

// directory is the fake CA's directory.
func (r *issueRig) directory() acme.Directory {
	return acme.Directory{URL: r.fake.DirectoryURL(), Roots: r.fake.RootPool(), Overridden: true}
}

// client is an acme.Client over the ingress account store, with its account registered (the terms agreed).
func (r *issueRig) client() *acme.Client {
	r.t.Helper()
	c := acme.NewClient(r.directory(), r.s.AccountStore(), r.clock, "tierplan-test/0")
	if _, err := c.Register(context.Background(), "", true); err != nil {
		r.t.Fatalf("Register: %v", err)
	}
	return c
}

// solver is the DNS-01 solver of certificate certID under the domain credential rigCredential.
func (r *issueRig) solver(certID string) *acme.DNSSolver {
	return &acme.DNSSolver{
		Engine: r.engine, Provider: rigProvider, CredentialRef: rigCredential,
		Tokens: func(context.Context) ([]byte, error) { return []byte(rigToken), nil },
		Owner: func(ctx context.Context) (string, error) {
			id, found, err := r.s.OpenOrderID(ctx, certID)
			if err != nil || !found {
				return "", err
			}
			return id, nil
		},
	}
}

// orderer is a new acme.Orderer over certificate certID's order store, which is what a restarted process builds.
func (r *issueRig) orderer(c *acme.Client, certID string) *acme.Orderer {
	return acme.NewOrderer(c, r.s.OrderStore(certID), r.solver(certID), r.clock)
}

// pending records that host wants a production certificate and returns its row.
func (r *issueRig) pending(host string) Cert {
	r.t.Helper()
	c, created, err := r.s.CreatePending(context.Background(), PendingInput{
		Hostname: host, EnvID: "env-1", DomainID: rigCredential, Issuer: IssuerProduction, CreatedBy: "user:1",
	}, nil)
	if err != nil || !created {
		r.t.Fatalf("CreatePending(%s) = created %v, %v", host, created, err)
	}
	return c
}

// stepLog is a step reporter that cancels a run's context when a named step starts running.
type stepLog struct {
	mu     sync.Mutex
	events []string
	on     func(step, event string)
}

func (l *stepLog) record(step, event string) {
	l.mu.Lock()
	l.events = append(l.events, step+":"+event)
	on := l.on
	l.mu.Unlock()
	if on != nil {
		on(step, event)
	}
}

func (l *stepLog) Running(step, _ string) { l.record(step, "running") }
func (l *stepLog) Done(step string)       { l.record(step, "done") }
func (l *stepLog) Skipped(step, _ string) { l.record(step, "skipped") }
func (l *stepLog) Failed(step string, _ error) {
	l.record(step, "failed")
}

// cancelOn returns a step reporter that calls cancel as step starts.
func cancelOn(cancel context.CancelFunc, step string) *stepLog {
	return &stepLog{on: func(s, event string) {
		if s == step && event == "running" {
			cancel()
		}
	}}
}

// count runs a single-integer query on the read pool.
func (r *issueRig) count(query string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.st.Read.QueryRow(query, args...).Scan(&n); err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// leaf is the first certificate of a PEM chain.
func leaf(t *testing.T, chainPEM []byte) *x509.Certificate {
	t.Helper()
	l, err := leafOf(chainPEM)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestTracerOrdererIssuesIntoIngressCerts is the tracer for CRT-01: acme.Orderer over the order store of one
// certificate, acme.Client over the ingress account store and the exported DNS solver with a domain's token
// and credential issue a certificate for app.example.com from the fake CA over DNS-01. The row then holds the
// chain, its key opens under the certificate's context, the order is closed, the issuance is counted and
// audited, and the TXT record and its ledger row are gone.
func TestTracerOrdererIssuesIntoIngressCerts(t *testing.T) {
	ctx := context.Background()
	r := newIssueRig(t)
	c := r.pending("app.example.com")
	versionBefore, configBefore := versions(t, r.st, "env-1")

	client := r.client()
	issued, err := r.orderer(client, c.ID).Run(ctx, acme.OrderRequest{Kind: acme.KindIssue, Names: []string{"app.example.com"}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := r.s.Cert(ctx, c.ID)
	if err != nil {
		t.Fatalf("Cert: %v", err)
	}
	if !slices.Equal(leaf(t, got.ChainPEM).DNSNames, []string{"app.example.com"}) {
		t.Errorf("the stored chain is for %v, want app.example.com", leaf(t, got.ChainPEM).DNSNames)
	}
	if got.Phase != PhaseIdle || got.Serial == "" || got.AccountURL == "" || got.NotAfter.IsZero() {
		t.Errorf("row = phase %q serial %q account %q notAfter %v, want an idle issued row", got.Phase, got.Serial, got.AccountURL, got.NotAfter)
	}
	key, err := r.s.OpenKey(got)
	if err != nil {
		t.Fatalf("OpenKey: %v", err)
	}
	if pub, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(issued.Key.Public()) {
		t.Error("the stored key is not the key the order was made with")
	}
	if _, err := r.v.Open(vault.ContextIngressCertKey+c.ID, got.KeySealed); err != nil {
		t.Errorf("the key does not open under ingress-cert-key/v1/<cert id>: %v", err)
	}

	if _, found, err := r.s.OpenOrderID(ctx, c.ID); err != nil || found {
		t.Errorf("OpenOrderID found = %v, %v, want the order closed", found, err)
	}
	if n := r.count(`SELECT COUNT(*) FROM ingress_cert_orders WHERE cert_id = ? AND status = 'closed' AND stage = 'stored'`, c.ID); n != 1 {
		t.Errorf("%d closed stored orders, want 1", n)
	}

	var issuer, registered, host, kind string
	if err := r.st.Read.QueryRow(`SELECT issuer, registered_domain, hostname, kind FROM ingress_cert_issuances`).Scan(&issuer, &registered, &host, &kind); err != nil {
		t.Fatalf("read the issuance: %v", err)
	}
	if issuer != IssuerProduction || registered != "example.com" || host != "app.example.com" || kind != "new" {
		t.Errorf("issuance = %s %s %s %s, want production example.com app.example.com new", issuer, registered, host, kind)
	}
	if n := r.count(`SELECT COUNT(*) FROM ingress_cert_issuances`); n != 1 {
		t.Errorf("%d issuance rows, want 1", n)
	}

	versionAfter, configAfter := versions(t, r.st, "env-1")
	if versionAfter <= versionBefore {
		t.Errorf("certs_version = %d after the issue, was %d: want it raised", versionAfter, versionBefore)
	}
	if configAfter != configBefore {
		t.Errorf("config_version moved from %d to %d: a certificate is not a route", configBefore, configAfter)
	}

	var actorID, targetType, targetID, envID string
	err = r.st.Read.QueryRow(`SELECT actor_id, target_type, target_id, COALESCE(env_id, '') FROM audit_log WHERE action = ?`,
		audit.ActionIngressCertIssued).Scan(&actorID, &targetType, &targetID, &envID)
	if err != nil {
		t.Fatalf("read the audit row: %v", err)
	}
	if actorID != audit.SystemIngress.ID || targetType != "ingress_certificate" || targetID != c.ID || envID != "env-1" {
		t.Errorf("audit = %s %s %s %s, want system:ingress ingress_certificate %s env-1", actorID, targetType, targetID, envID, c.ID)
	}

	if recs := r.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v", recs)
	}
	if n, err := r.ledger.CountDNSRecordsByCredential(ctx, rigCredential); err != nil || n != 0 {
		t.Errorf("ledger rows of the credential = %d, %v, want 0", n, err)
	}
}

// insertOrder stores an open order of certificate certID for host and returns it with its key.
func (r *issueRig) insertOrder(certID, host string) (acme.Order, *ecdsa.PrivateKey) {
	r.t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		r.t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		r.t.Fatal(err)
	}
	now := r.clock.Now()
	o := acme.Order{
		ID: id.String(), DirectoryURL: r.fake.DirectoryURL(), URL: "https://ca.test/order/" + id.String(),
		Kind: acme.KindIssue, Names: []string{host}, AccountURL: "https://ca.test/acct/1",
		Stage: acme.StageCreated, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	if err := r.s.OrderStore(certID).InsertOrder(context.Background(), o, key); err != nil {
		r.t.Fatalf("InsertOrder: %v", err)
	}
	return o, key
}

// An order store sees only its own certificate's orders, the order key opens only under the order's own
// context, and deleting the certificate deletes its orders and their sealed keys (D-35).
func TestOrderStoreIsScopedToItsCertificate(t *testing.T) {
	ctx := context.Background()
	r := newIssueRig(t)
	a, b := r.pending("a.example.com"), r.pending("b.example.com")
	orderA, _ := r.insertOrder(a.ID, "a.example.com")
	orderB, _ := r.insertOrder(b.ID, "b.example.com")

	for _, tc := range []struct {
		cert Cert
		want acme.Order
	}{{a, orderA}, {b, orderB}} {
		got, key, found, err := r.s.OrderStore(tc.cert.ID).OpenOrder(ctx)
		if err != nil || !found || key == nil {
			t.Fatalf("OpenOrder(%s) = found %v, %v", tc.cert.Hostname, found, err)
		}
		if got.ID != tc.want.ID || !slices.Equal(got.Names, tc.want.Names) || got.Status != "open" || got.AccountURL != tc.want.AccountURL {
			t.Errorf("OpenOrder(%s) = %+v, want order %s", tc.cert.Hostname, got, tc.want.ID)
		}
		if id, found, err := r.s.OpenOrderID(ctx, tc.cert.ID); err != nil || !found || id != tc.want.ID {
			t.Errorf("OpenOrderID(%s) = %q, %v, %v, want %s", tc.cert.Hostname, id, found, err, tc.want.ID)
		}
	}
	if _, _, found, err := r.s.OrderStore("no-such-cert").OpenOrder(ctx); err != nil || found {
		t.Errorf("OpenOrder of an unknown certificate = found %v, %v, want none", found, err)
	}

	// The key is sealed under the order's id and opens nowhere else.
	var sealed string
	if err := r.st.Read.QueryRow(`SELECT cert_key_sealed FROM ingress_cert_orders WHERE id = ?`, orderA.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := r.v.Open(vault.ContextIngressOrderKey+orderA.ID, sealed); err != nil {
		t.Errorf("the order key does not open under its own context: %v", err)
	}
	for _, wrong := range []string{vault.ContextIngressOrderKey + orderB.ID, vault.ContextIngressCertKey + a.ID, vault.ContextACMEOrderKey + orderA.ID} {
		if _, err := r.v.Open(wrong, sealed); err == nil {
			t.Errorf("the order key opened under %q", wrong)
		}
	}

	// Deleting the certificate takes its orders, and so their sealed keys, with it; the other's stay.
	if _, err := r.st.Write.Exec(`DELETE FROM ingress_certs WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if n := r.count(`SELECT COUNT(*) FROM ingress_cert_orders WHERE cert_id = ?`, a.ID); n != 0 {
		t.Errorf("%d orders of the deleted certificate remain, want 0", n)
	}
	if n := r.count(`SELECT COUNT(*) FROM ingress_cert_orders WHERE cert_id = ?`, b.ID); n != 1 {
		t.Errorf("%d orders of the other certificate, want 1", n)
	}
}

// A run cut short after the order was created resumes on the next Run with the same order and key: the CA is
// asked for one order only.
func TestOrderStoreResumesACutRun(t *testing.T) {
	ctx := context.Background()
	r := newIssueRig(t)
	c := r.pending("app.example.com")
	client := r.client()
	req := acme.OrderRequest{Kind: acme.KindIssue, Names: []string{"app.example.com"}}

	release := r.fake.HoldValidation()
	cutCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if _, err := r.orderer(client, c.ID).Run(cutCtx, req, cancelOn(cancel, acme.StepValidate)); !errors.Is(err, context.Canceled) {
		t.Fatalf("the cut run ended with %v, want context.Canceled", err)
	}
	first, firstKey, found, err := r.s.OrderStore(c.ID).OpenOrder(ctx)
	if err != nil || !found {
		t.Fatalf("OpenOrder after the cut = found %v, %v, want the order left open", found, err)
	}
	if n := r.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Fatalf("newOrder was called %d times, want 1", n)
	}

	// A new Orderer, as after a restart: it finds the order, answers the challenge again without submitting it
	// and waits for the CA. The CA validates what it holds once the second run has put the record back.
	type result struct {
		issued acme.Issued
		err    error
	}
	done := make(chan result, 1)
	go func() {
		issued, err := r.orderer(client, c.ID).Run(ctx, req, nil)
		done <- result{issued, err}
	}()
	r.clock.WaitArmed(t)
	release()
	r.clock.Advance(acme.PollInterval)
	res := testsupport.Await(t, "the resumed run to finish", done)
	if res.err != nil {
		t.Fatalf("the second Run: %v", res.err)
	}
	issued := res.issued
	if issued.OrderID != first.ID {
		t.Errorf("the second run used order %s, want the first %s", issued.OrderID, first.ID)
	}
	if !firstKey.PublicKey.Equal(&issued.Key.PublicKey) {
		t.Error("the resumed run used another key than the one sealed with the order")
	}
	if n := r.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("newOrder was called %d times after the resume, want still 1", n)
	}
	row, err := r.s.Cert(ctx, c.ID)
	if err != nil || row.Phase != PhaseIdle || row.ChainPEM == nil {
		t.Fatalf("Cert = %+v, %v, want an issued row", row, err)
	}
	if _, found, err := r.s.OpenOrderID(ctx, c.ID); err != nil || found {
		t.Errorf("OpenOrderID found = %v, %v, want the order closed", found, err)
	}
}

// StoreIssued writes nothing when the certificate row it was ordered for is gone: deleted, retired, or
// replaced by an upload. An upload is never overwritten by an ACME certificate (CRT-08, T-20-11).
func TestStoreIssuedRefusesAReplacedCertificate(t *testing.T) {
	const host = "app.example.com"
	for _, tc := range []struct {
		name  string
		apply func(t *testing.T, r *issueRig, c Cert) (uploadID string)
	}{
		{"deleted", func(t *testing.T, r *issueRig, c Cert) string {
			if _, err := r.st.Write.Exec(`DELETE FROM ingress_certs WHERE id = ?`, c.ID); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{"retired", func(t *testing.T, r *issueRig, c Cert) string {
			if _, err := r.st.Write.Exec(`UPDATE ingress_certs SET retired_at = ?, retired_reason = 'replaced' WHERE id = ?`,
				r.clock.Now().UnixNano(), c.ID); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{"replaced by an upload", func(t *testing.T, r *issueRig, c Cert) string {
			if _, err := r.st.Write.Exec(`DELETE FROM ingress_certs WHERE id = ?`, c.ID); err != nil {
				t.Fatal(err)
			}
			_, chain, _ := selfSigned(t, host)
			id, err := uuid.NewV7()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.st.Write.Exec(
				`INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, issuer, phase, chain_pem, created_at, updated_at)
				 VALUES (?, ?, ?, ?, 'upload', '', 'idle', ?, 1, 1)`,
				id.String(), host, c.EnvID, c.DomainID, string(chain)); err != nil {
				t.Fatal(err)
			}
			return id.String()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newIssueRig(t)
			c := r.pending(host)
			order, _ := r.insertOrder(c.ID, host)
			uploadID := tc.apply(t, r, c)
			certsBefore, _ := versions(t, r.st, "env-1")
			var uploadBefore string
			if uploadID != "" {
				if err := r.st.Read.QueryRow(`SELECT chain_pem FROM ingress_certs WHERE id = ?`, uploadID).Scan(&uploadBefore); err != nil {
					t.Fatal(err)
				}
			}

			key, chain, _ := selfSigned(t, host)
			err := r.s.OrderStore(c.ID).StoreIssued(ctx, order.ID, chain, key, r.clock.Now())
			if !errors.Is(err, ErrCertGone) {
				t.Fatalf("StoreIssued = %v, want ErrCertGone", err)
			}

			if n := r.count(`SELECT COUNT(*) FROM ingress_certs WHERE hostname = ? AND source = 'acme' AND chain_pem != ''`, host); n != 0 {
				t.Errorf("%d ACME rows hold a chain, want none", n)
			}
			if n := r.count(`SELECT COUNT(*) FROM ingress_cert_issuances`); n != 0 {
				t.Errorf("%d issuance rows, want none", n)
			}
			if n := r.count(`SELECT COUNT(*) FROM audit_log WHERE action IN (?, ?)`, audit.ActionIngressCertIssued, audit.ActionIngressCertRenewed); n != 0 {
				t.Errorf("%d issue audit rows, want none", n)
			}
			if after, _ := versions(t, r.st, "env-1"); after != certsBefore {
				t.Errorf("certs_version moved from %d to %d, want it left alone", certsBefore, after)
			}
			if uploadID != "" {
				var source, chainAfter string
				if err := r.st.Read.QueryRow(`SELECT source, chain_pem FROM ingress_certs WHERE id = ?`, uploadID).Scan(&source, &chainAfter); err != nil {
					t.Fatal(err)
				}
				if source != SourceUpload || chainAfter != uploadBefore {
					t.Errorf("the upload row is now source %q with a changed chain %v", source, chainAfter != uploadBefore)
				}
			}
		})
	}
}

// PruneClosedOrders removes the closed orders last touched before the cutoff and never an open one.
func TestPruneKeepsOpenOrders(t *testing.T) {
	ctx := context.Background()
	r := newIssueRig(t)
	c := r.pending("app.example.com")
	store := r.s.OrderStore(c.ID)

	oldClosed, _ := r.insertOrder(c.ID, "app.example.com")
	if err := store.CloseOrder(ctx, oldClosed.ID, acme.StageFailed, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(ClosedOrderRetention + time.Hour)
	recentClosed, _ := r.insertOrder(c.ID, "app.example.com")
	if err := store.CloseOrder(ctx, recentClosed.ID, acme.StageFailed, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	open, _ := r.insertOrder(c.ID, "app.example.com")

	n, err := r.s.PruneClosedOrders(ctx, r.clock.Now().Add(-ClosedOrderRetention))
	if err != nil || n != 1 {
		t.Fatalf("PruneClosedOrders = %d, %v, want 1", n, err)
	}
	for id, want := range map[string]int{oldClosed.ID: 0, recentClosed.ID: 1, open.ID: 1} {
		if got := r.count(`SELECT COUNT(*) FROM ingress_cert_orders WHERE id = ?`, id); got != want {
			t.Errorf("order %s: %d rows, want %d", id, got, want)
		}
	}
	if id, found, err := r.s.OpenOrderID(ctx, c.ID); err != nil || !found || id != open.ID {
		t.Errorf("OpenOrderID = %q, %v, %v, want the open order %s", id, found, err, open.ID)
	}
}
