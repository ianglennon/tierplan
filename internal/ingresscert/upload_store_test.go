// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// testCA is a certificate authority that signs the leaves of the upload tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

// newTestCA makes a root valid around fixedNow.
func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Root"},
		NotBefore: fixedNow.Add(-365 * 24 * time.Hour), NotAfter: fixedNow.Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create the root: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse the root: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// leafOpts shapes a leaf: its names and validity (the zero values mean a valid window around fixedNow).
type leafOpts struct {
	sans                []string
	notBefore, notAfter time.Time
}

// issue signs a leaf for pub with the CA and returns it as a PEM certificate.
func (ca *testCA) issue(t *testing.T, pub crypto.PublicKey, o leafOpts) []byte {
	t.Helper()
	if o.notBefore.IsZero() {
		o.notBefore = fixedNow.Add(-time.Hour)
	}
	if o.notAfter.IsZero() {
		o.notAfter = fixedNow.Add(90 * 24 * time.Hour)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: o.sans[0]}, DNSNames: o.sans,
		NotBefore: o.notBefore, NotAfter: o.notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatalf("sign a leaf: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// ecLeaf makes a P-256 key and a leaf for sans, with the key as PKCS#8 PEM.
func (ca *testCA) ecLeaf(t *testing.T, sans ...string) (chainPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	keyPEM, err = encodeECKey(key)
	if err != nil {
		t.Fatalf("encode the key: %v", err)
	}
	return ca.issue(t, &key.PublicKey, leafOpts{sans: sans}), keyPEM
}

// uploadWorld is a world in which env-1 holds the registered domain example.com and nothing is claimed.
func uploadWorld(envID, domainID string) ingress.World {
	return ingress.World{
		Domains:  []ingress.Domain{{ID: domainID, EnvID: envID, Suffix: "example.com", Issuer: ingress.IssuerStaging}},
		Reserved: map[string]bool{},
		Claims:   map[string]ingress.ClaimInfo{},
	}
}

// TestTracerAnUploadBecomesTheLiveCertificate is the tracer for CRT-08: a valid chain and key for a hostname
// become its live upload row, the key sealed under the new row's id, certs_version raised and the hook run in
// the same write.
func TestTracerAnUploadBecomesTheLiveCertificate(t *testing.T) {
	s, st := newTestStore(t)
	ctx := context.Background()
	seedEnv(t, st, "env-1", "one")
	ca := newTestCA(t)
	chainPEM, keyPEM := ca.ecLeaf(t, "app.example.com")

	var hookReplaced string
	var hookCert Cert
	got, warnings, err := s.Upload(ctx, UploadInput{
		EnvID: "env-1", Hostname: "App.Example.com.", Actor: "user:1",
		ChainPEM: chainPEM, KeyPEM: keyPEM, World: uploadWorld("env-1", "dom-1"),
	}, ca.pool, func(_ context.Context, _ *sql.Tx, c Cert, replaced string, _ []string) error {
		hookCert, hookReplaced = c, replaced
		return nil
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("a chain that verifies against the roots has no warning; got %v", warnings)
	}
	if got.Source != SourceUpload || got.Issuer != "" || got.Phase != PhaseIdle || !got.Ready || got.RetiredAt != (time.Time{}) {
		t.Fatalf("the upload is a live idle ready upload row with no issuer; got %+v", got)
	}
	if got.Hostname != "app.example.com" || got.EnvID != "env-1" || got.DomainID != "dom-1" || got.CreatedBy != "user:1" {
		t.Fatalf("the row names the normalized hostname, the environment, the domain and the actor; got %+v", got)
	}
	live, found, err := s.LiveCert(ctx, "app.example.com")
	if err != nil || !found || live.ID != got.ID {
		t.Fatalf("LiveCert = %v, found %v, id %q; want the uploaded row %q", err, found, live.ID, got.ID)
	}
	signer, err := s.OpenKey(live)
	if err != nil {
		t.Fatalf("OpenKey: %v", err)
	}
	wantDER, err := x509.MarshalPKCS8PrivateKey(signer)
	if err != nil {
		t.Fatalf("encode the opened key: %v", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil || !bytes.Equal(block.Bytes, wantDER) {
		t.Fatalf("the opened key is not the uploaded key")
	}
	if bytes.Contains([]byte(live.KeySealed), block.Bytes) {
		t.Fatalf("the stored key is not sealed")
	}
	if certs, config := versions(t, st, "env-1"); certs != 1 || config != 0 {
		t.Fatalf("an upload raises certs_version once and never config_version; got %d and %d", certs, config)
	}
	if hookReplaced != ReplacedNone || hookCert.ID != got.ID {
		t.Fatalf("the hook saw replaced %q for %q; want none for %q", hookReplaced, hookCert.ID, got.ID)
	}
}

// clockStore is a Store on a clock the test moves, with an audit writer, over the raw database.
type clockStore struct {
	*Store
	st  *store.Store
	now time.Time
}

// newClockStore returns a clockStore starting at fixedNow.
func newClockStore(t *testing.T) *clockStore {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	c := &clockStore{st: st, now: fixedNow}
	clock := func() time.Time { return c.now }
	c.Store = NewStore(StoreDeps{St: st, Vault: v, Now: clock, Audit: audit.NewWriter(st, v, slog.New(slog.DiscardHandler), clock)})
	return c
}

// count runs a COUNT query.
func (c *clockStore) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := c.st.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// seedOrder inserts an open order under certificate certID.
func (c *clockStore) seedOrder(t *testing.T, certID string) {
	t.Helper()
	if _, err := c.st.Write.Exec(`INSERT INTO ingress_cert_orders
		(id, cert_id, directory_url, order_url, kind, names, cert_key_sealed, stage, created_at, updated_at)
		VALUES (?, ?, 'https://acme.test/dir', 'https://acme.test/o/1', 'issue', 'app.example.com', 'sealed', 'created', 1, 1)`,
		"order-"+certID, certID); err != nil {
		t.Fatalf("seed an order: %v", err)
	}
}

// upload uploads a fresh certificate for host into envID, judged in a world where envID holds example.com.
func (c *clockStore) upload(t *testing.T, ca *testCA, envID, host string, world ingress.World) (Cert, string, error) {
	t.Helper()
	chainPEM, keyPEM := ca.ecLeaf(t, host)
	var replaced string
	got, _, err := c.Upload(context.Background(), UploadInput{
		EnvID: envID, Hostname: host, Actor: "user:1", ChainPEM: chainPEM, KeyPEM: keyPEM, World: world,
	}, ca.pool, func(_ context.Context, _ *sql.Tx, _ Cert, r string, _ []string) error {
		replaced = r
		return nil
	})
	return got, replaced, err
}

// refusalCode returns the code of a RequestRefusedError, "" for any other error.
func refusalCode(err error) string {
	var refused *RequestRefusedError
	if errors.As(err, &refused) {
		return refused.Problem.Code
	}
	return ""
}

// TestUploadReplacesWhateverIsLive pins D-22: an upload replaces this environment's live row whatever its
// source, the ACME row and its order go with it, and the hook is told what was replaced.
func TestUploadReplacesWhateverIsLive(t *testing.T) {
	c := newClockStore(t)
	ctx := context.Background()
	seedEnv(t, c.st, "env-1", "one")
	ca := newTestCA(t)
	world := uploadWorld("env-1", "dom-1")

	acmeRow, _, err := c.CreatePending(ctx, PendingInput{
		Hostname: "app.example.com", EnvID: "env-1", DomainID: "dom-1", Issuer: IssuerProduction, CreatedBy: "user:1",
	}, nil)
	if err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	c.seedOrder(t, acmeRow.ID)

	first, replaced, err := c.upload(t, ca, "env-1", "app.example.com", world)
	if err != nil || replaced != ReplacedACME {
		t.Fatalf("upload over an ACME row = %v, replaced %q; want replaced acme", err, replaced)
	}
	if _, err := c.Cert(ctx, acmeRow.ID); !errors.Is(err, ErrCertNotFound) {
		t.Fatalf("the replaced ACME row is still there: %v", err)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM ingress_cert_orders WHERE cert_id = ?`, acmeRow.ID); n != 0 {
		t.Fatalf("%d orders of the replaced ACME row remain", n)
	}

	second, replaced, err := c.upload(t, ca, "env-1", "app.example.com", world)
	if err != nil || replaced != ReplacedUpload || second.ID == first.ID {
		t.Fatalf("upload over an upload = %v, replaced %q, id %q (was %q); want replaced upload with a new row", err, replaced, second.ID, first.ID)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM ingress_certs WHERE hostname = 'app.example.com'`); n != 1 {
		t.Fatalf("%d rows for the hostname, want one live row", n)
	}
	if _, err := c.OpenKey(second); err != nil {
		t.Fatalf("the new upload's key does not open under its own id: %v", err)
	}
}

// TestUploadRefusesAnotherEnvironmentsName pins PROH-20-08 and D-23: a hostname claimed by another
// environment's stack, or under no domain of the environment, is refused before anything is written.
func TestUploadRefusesAnotherEnvironmentsName(t *testing.T) {
	c := newClockStore(t)
	seedEnv(t, c.st, "env-1", "one")
	seedEnv(t, c.st, "env-2", "two")
	ca := newTestCA(t)

	taken := uploadWorld("env-1", "dom-1")
	taken.Claims["app.example.com"] = ingress.ClaimInfo{
		Claim:     ingress.Claim{EnvID: "env-2", Hostname: "app.example.com", DomainID: "dom-2", OwnerKind: ingress.OwnerStack, StackID: "stack-2"},
		EnvName:   "two",
		StackName: "shop",
	}
	for _, tc := range []struct {
		name, host string
		world      ingress.World
		want       string
	}{
		{"claimed by a stack of another environment", "app.example.com", taken, ingress.CodeHostTaken},
		{"under no domain of the environment", "app.example.org", uploadWorld("env-1", "dom-1"), ingress.CodeDomainNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := c.upload(t, ca, "env-1", tc.host, tc.world)
			if got := refusalCode(err); got != tc.want {
				t.Fatalf("upload = %v, want a refusal with code %s", err, tc.want)
			}
			if n := c.count(t, `SELECT COUNT(*) FROM ingress_certs`); n != 0 {
				t.Fatalf("a refused upload wrote %d rows", n)
			}
			if v, _ := versions(t, c.st, "env-1"); v != 0 {
				t.Fatalf("a refused upload raised certs_version to %d", v)
			}
		})
	}
}

// TestUploadNeverTakesAnotherEnvironmentsHeldCertificate pins D-08, D-21, D-31 and T-20-46: another
// environment's request-ahead holds its row for the grace; past it, or for an unrequested row, the upload
// replaces it through the second gate with an audit row and both versions raised.
func TestUploadNeverTakesAnotherEnvironmentsHeldCertificate(t *testing.T) {
	ctx := context.Background()
	ca := newTestCA(t)
	setup := func(t *testing.T, requested bool) (*clockStore, Cert) {
		c := newClockStore(t)
		seedEnv(t, c.st, "env-a", "a")
		seedEnv(t, c.st, "env-b", "b")
		in := PendingInput{Hostname: "app.example.com", EnvID: "env-b", DomainID: "dom-b", Issuer: IssuerProduction, CreatedBy: "user:2"}
		if requested {
			in.RequestedAt = c.now
		}
		row, _, err := c.CreatePending(ctx, in, nil)
		if err != nil {
			t.Fatalf("CreatePending: %v", err)
		}
		c.seedOrder(t, row.ID)
		return c, row
	}
	world := uploadWorld("env-a", "dom-a")

	t.Run("held by a request ahead", func(t *testing.T) {
		c, b := setup(t, true)
		aBefore, _ := versions(t, c.st, "env-a")
		bBefore, _ := versions(t, c.st, "env-b")
		_, _, err := c.upload(t, ca, "env-a", "app.example.com", world)
		if refusalCode(err) != ingress.CodeHostTaken {
			t.Fatalf("upload = %v, want ingress_host_taken", err)
		}
		if bytes.Contains([]byte(err.Error()), []byte("env-b")) || bytes.Contains([]byte(err.Error()), []byte(b.ID)) {
			t.Fatalf("the refusal names the other environment or its row: %q", err.Error())
		}
		kept, err := c.Cert(ctx, b.ID)
		if err != nil || kept.EnvID != "env-b" || kept.DomainID != "dom-b" || !kept.RequestedAt.Equal(b.RequestedAt) {
			t.Fatalf("the held row changed: %v, %+v", err, kept)
		}
		if n := c.count(t, `SELECT COUNT(*) FROM ingress_cert_orders WHERE cert_id = ?`, b.ID); n != 1 {
			t.Fatalf("the held row's order count = %d, want 1", n)
		}
		aAfter, _ := versions(t, c.st, "env-a")
		bAfter, _ := versions(t, c.st, "env-b")
		if aAfter != aBefore || bAfter != bBefore {
			t.Fatalf("a refusal moved versions: a %d -> %d, b %d -> %d", aBefore, aAfter, bBefore, bAfter)
		}
		if n := c.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = ?`, audit.ActionIngressCertMoved); n != 0 {
			t.Fatalf("a refusal wrote %d moved rows", n)
		}
	})

	assertMoved := func(t *testing.T, c *clockStore, b Cert, aBefore, bBefore int64) {
		t.Helper()
		got, replaced, err := c.upload(t, ca, "env-a", "app.example.com", world)
		if err != nil || replaced != ReplacedACME {
			t.Fatalf("upload = %v, replaced %q; want it to replace the ACME row", err, replaced)
		}
		if _, err := c.Cert(ctx, b.ID); !errors.Is(err, ErrCertNotFound) {
			t.Fatalf("the other environment's row survived: %v", err)
		}
		if n := c.count(t, `SELECT COUNT(*) FROM ingress_cert_orders WHERE cert_id = ?`, b.ID); n != 0 {
			t.Fatalf("%d orders of the replaced row remain", n)
		}
		if got.EnvID != "env-a" || got.Source != SourceUpload {
			t.Fatalf("the new row = %+v, want an upload of env-a", got)
		}
		aAfter, _ := versions(t, c.st, "env-a")
		bAfter, _ := versions(t, c.st, "env-b")
		if aAfter != aBefore+1 || bAfter != bBefore+1 {
			t.Fatalf("versions: a %d -> %d, b %d -> %d; want each raised once", aBefore, aAfter, bBefore, bAfter)
		}
		if n := c.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = ?`, audit.ActionIngressCertMoved); n != 1 {
			t.Fatalf("%d moved rows, want 1", n)
		}
		var actorID, targetID, envID, details string
		if err := c.st.Read.QueryRow(`SELECT actor_id, target_id, COALESCE(env_id, ''), details FROM audit_log WHERE action = ?`,
			audit.ActionIngressCertMoved).Scan(&actorID, &targetID, &envID, &details); err != nil {
			t.Fatalf("read the moved row: %v", err)
		}
		var d map[string]string
		if err := json.Unmarshal([]byte(details), &d); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		if actorID != audit.SystemIngress.ID || targetID != got.ID || envID != "env-a" ||
			d["hostname"] != "app.example.com" || d["from_env"] != "env-b" || d["to_env"] != "env-a" ||
			d["from_domain"] != "dom-b" || d["to_domain"] != "dom-a" || d["outcome"] != "replaced" || d["cause"] != "upload" {
			t.Fatalf("moved row = %s %s %s %v", actorID, targetID, envID, d)
		}
	}

	t.Run("past the grace", func(t *testing.T) {
		c, b := setup(t, true)
		c.now = c.now.Add(Grace + time.Minute)
		aBefore, _ := versions(t, c.st, "env-a")
		bBefore, _ := versions(t, c.st, "env-b")
		assertMoved(t, c, b, aBefore, bBefore)
	})

	t.Run("released by its environment", func(t *testing.T) {
		c, b := setup(t, false)
		// CreatePending stamps the request time when it is not given, so the request is cleared by hand: this
		// row was claimed, not requested ahead, and its claim went away a day ago.
		if _, err := c.st.Write.Exec(`UPDATE ingress_certs SET requested_at = 0, requested_by = '', unreferenced_since = ? WHERE id = ?`,
			c.now.Add(-24*time.Hour).UnixNano(), b.ID); err != nil {
			t.Fatalf("release the row: %v", err)
		}
		aBefore, _ := versions(t, c.st, "env-a")
		bBefore, _ := versions(t, c.st, "env-b")
		assertMoved(t, c, b, aBefore, bBefore)
	})
}

// TestSwitchToACMEQueuesAnOrderAndKeepsTheUploadUntilReplaced pins D-22: the live upload is retired, still
// readable, and a queued ACME row with the domain's issuer takes the hostname.
func TestSwitchToACMEQueuesAnOrderAndKeepsTheUploadUntilReplaced(t *testing.T) {
	c := newClockStore(t)
	ctx := context.Background()
	seedEnv(t, c.st, "env-1", "one")
	seedEnv(t, c.st, "env-2", "two")
	ca := newTestCA(t)
	up, _, err := c.upload(t, ca, "env-1", "app.example.com", uploadWorld("env-1", "dom-1"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	acmeRow, _, err := c.CreatePending(ctx, PendingInput{
		Hostname: "other.example.com", EnvID: "env-1", DomainID: "dom-1", Issuer: IssuerStaging, CreatedBy: "user:1",
	}, nil)
	if err != nil {
		t.Fatalf("CreatePending: %v", err)
	}

	for _, tc := range []struct{ name, env, id string }{
		{"an ACME row", "env-1", acmeRow.ID},
		{"a certificate of another environment", "env-2", up.ID},
		{"no such certificate", "env-1", "missing"},
	} {
		before, _ := versions(t, c.st, "env-1")
		if _, err := c.SwitchToACME(ctx, tc.env, tc.id, "user:1", IssuerProduction, nil); !errors.Is(err, ErrCertNotFound) {
			t.Fatalf("SwitchToACME on %s = %v, want ErrCertNotFound", tc.name, err)
		}
		if after, _ := versions(t, c.st, "env-1"); after != before {
			t.Fatalf("a refused switch moved certs_version (%d -> %d)", before, after)
		}
	}
	if live, found, err := c.LiveCert(ctx, "app.example.com"); err != nil || !found || live.ID != up.ID {
		t.Fatalf("a refused switch changed the live row: %v %v %+v", err, found, live)
	}

	c.now = c.now.Add(time.Hour)
	before, _ := versions(t, c.st, "env-1")
	var hooked Cert
	fresh, err := c.SwitchToACME(ctx, "env-1", up.ID, "user:1", IssuerProduction, func(_ context.Context, _ *sql.Tx, n Cert) error {
		hooked = n
		return nil
	})
	if err != nil {
		t.Fatalf("SwitchToACME: %v", err)
	}
	if fresh.Source != SourceACME || fresh.Issuer != IssuerProduction || fresh.Phase != PhaseQueued ||
		fresh.Hostname != "app.example.com" || fresh.DomainID != "dom-1" || fresh.RetiredAt != (time.Time{}) || hooked.ID != fresh.ID {
		t.Fatalf("the new row = %+v (hook saw %q); want a live queued production ACME row for the hostname", fresh, hooked.ID)
	}
	if !fresh.RequestedAt.IsZero() {
		t.Fatalf("a switch back is not a request ahead, but requested_at is %v", fresh.RequestedAt)
	}
	retired, err := c.Cert(ctx, up.ID)
	if err != nil {
		t.Fatalf("the retired upload is gone: %v", err)
	}
	if retired.RetiredAt.IsZero() || retired.RetiredReason != RetiredSwitchedToACME || !retired.Ready || len(retired.ChainPEM) == 0 {
		t.Fatalf("the upload must be retired as switched_to_acme and still servable; got %+v", retired)
	}
	if _, err := c.OpenKey(retired); err != nil {
		t.Fatalf("the retired upload's key no longer opens: %v", err)
	}
	if live, found, err := c.LiveCert(ctx, "app.example.com"); err != nil || !found || live.ID != fresh.ID {
		t.Fatalf("LiveCert = %v %v %q, want the new ACME row %q", err, found, live.ID, fresh.ID)
	}
	if after, _ := versions(t, c.st, "env-1"); after != before+1 {
		t.Fatalf("certs_version %d -> %d, want one rise", before, after)
	}
}
