// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// acmeHost is the name the public URL carries in these tests. The fake CA is told to validate every
// name against the listener of the server under test, so the HTTP-01 check reaches the sniffing
// listener's plain path whatever the name is.
const acmeHost = "tierplan.test"

// acmeRig is a TierPlan server wired to a fake Let's Encrypt whose HTTP-01 check is answered by the
// server's own listener, on a clock the test moves. The server can be stopped and built again on
// the same data directory, as after an upgrade.
type acmeRig struct {
	t     *testing.T
	fake  *acmetest.Fake
	clock *acmetest.Clock
	// tlsClock is the clock of the certificate refresh loop, for a test that moves the self-signed
	// source's name check.
	tlsClock *stepClock
	env      map[string]string
	cfg      config.Config
	opts     []Option

	mu   sync.Mutex
	addr string

	srv    *Server
	cancel context.CancelFunc
	done   chan error
}

// newACMERig prepares a rig. extra environment variables are added to the data directory, the
// public URL and the fake directory the rig sets itself.
func newACMERig(t *testing.T, fakeOpts acmetest.Options, extra map[string]string, opts ...Option) *acmeRig {
	t.Helper()
	r := &acmeRig{t: t, clock: acmetest.NewClock(time.Now().UTC().Truncate(time.Second)), tlsClock: newStepClock(time.Now())}
	fakeOpts.Clock = r.clock
	fakeOpts.Validate = func(string) string { return r.address() }
	r.fake = acmetest.NewFake(t, fakeOpts)
	r.env = map[string]string{
		"TIERPLAN_DATA_DIR":           t.TempDir() + "/data",
		"TIERPLAN_PUBLIC_URL":         "https://" + acmeHost,
		"TIERPLAN_ACME_DIRECTORY_URL": r.fake.DirectoryURL(),
		"TIERPLAN_ACME_CA_FILE":       r.fake.CAFile(t),
	}
	for k, v := range extra {
		r.env[k] = v
	}
	r.cfg = testConfig(t, r.env)
	r.opts = append([]Option{WithACMEClock(r.clock), WithTLSRefreshClock(r.tlsClock)}, opts...)
	t.Cleanup(r.stop)
	return r
}

func (r *acmeRig) address() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addr
}

// start builds the server on the rig's data directory and serves it on ln.
func (r *acmeRig) start(ln net.Listener) *Server {
	r.t.Helper()
	srv, err := Build(context.Background(), r.cfg, slog.New(slog.DiscardHandler), r.opts...)
	if err != nil {
		_ = ln.Close()
		r.t.Fatalf("Build: %v", err)
	}
	r.mu.Lock()
	r.addr = ln.Addr().String()
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, ln) }()
	r.srv, r.cancel, r.done = srv, cancel, done
	return srv
}

// startNew serves a server on a fresh loopback port.
func (r *acmeRig) startNew() *Server {
	r.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		r.t.Fatal(err)
	}
	return r.start(ln)
}

// stop shuts the running server down and closes it, so its data directory can be built again. It
// is safe to call twice.
func (r *acmeRig) stop() {
	if r.srv == nil {
		return
	}
	r.cancel()
	<-r.done
	_ = r.srv.Close()
	r.srv, r.cancel, r.done = nil, nil, nil
}

// issue registers an account and orders the certificate, and waits for the run to end.
func (r *acmeRig) issue() {
	r.t.Helper()
	ctx := context.Background()
	if _, err := r.srv.acme.Register(ctx, "", true); err != nil {
		r.t.Fatalf("Register: %v", err)
	}
	rs, err := r.srv.acme.Issue(ctx, "test")
	if err != nil {
		r.t.Fatalf("Issue: %v", err)
	}
	r.waitJob(rs.JobID)
}

// waitJob waits for a job of the server to end.
func (r *acmeRig) waitJob(id string) {
	r.t.Helper()
	if id == "" {
		r.t.Fatal("no job was started")
	}
	ctx := testsupport.Context(r.t)
	job, err := r.srv.jobs.Wait(ctx, id)
	if err != nil {
		r.t.Fatalf("wait for job %s: %v", id, err)
	}
	for _, it := range job.Items {
		if it.State == "failed" {
			r.t.Fatalf("the certificate run failed at %q: %s", it.Label, it.Message)
		}
	}
}

// handshake connects to the server's address presenting acmeHost and returns the leaf it is
// answered with, verified against roots. nil roots verifies against the mirror of the install's own
// certificate.
func (r *acmeRig) handshake(roots *x509.CertPool) (*x509.Certificate, error) {
	r.t.Helper()
	conn, err := (&tls.Dialer{Config: &tls.Config{ServerName: acmeHost, RootCAs: roots, MinVersion: tls.VersionTLS12}}).
		DialContext(testsupport.Context(r.t), "tcp", r.address())
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return conn.(*tls.Conn).ConnectionState().PeerCertificates[0], nil
}

// mirrorPool is a pool holding the certificates of the healthcheck mirror.
func (r *acmeRig) mirrorPool() *x509.CertPool {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.cfg.DataDir, selfSignedDir, mirrorCertFile))
	if err != nil {
		r.t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		r.t.Fatal("the mirror holds no certificate")
	}
	return pool
}

// client is an HTTP client for the server that dials its address whatever the URL says, trusts the
// given roots, and verifies the name acmeHost.
func (r *acmeRig) client(roots *x509.CertPool) *http.Client {
	r.t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, r.address())
		},
		TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	r.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// signIn makes an admin in the store and signs in over HTTPS with c, returning the session cookie.
func (r *acmeRig) signIn(c *http.Client) *http.Cookie {
	r.t.Helper()
	const password = "correct horse battery"
	hash, err := auth.HashPassword(password)
	if err != nil {
		r.t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := r.srv.Store().Write.BeginTx(ctx, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixNano()
	if err := r.srv.Store().Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: "u1", Username: "admin", PasswordHash: hash, CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
	}); err != nil {
		r.t.Fatal(err)
	}
	if err := rbac.GrantInstanceAdminTx(ctx, tx, r.srv.Store().Q, "u1"); err != nil {
		r.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatal(err)
	}
	resp, err := c.Post("https://"+acmeHost+"/api/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
	if err != nil {
		r.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(resp.Cookies()) != 1 {
		r.t.Fatalf("login: %d", resp.StatusCode)
	}
	return resp.Cookies()[0]
}

// meStatus is the status of an authenticated GET with the cookie.
func (r *acmeRig) meStatus(c *http.Client, cookie *http.Cookie) int {
	r.t.Helper()
	req, err := http.NewRequestWithContext(testsupport.Context(r.t), http.MethodGet, "https://"+acmeHost+"/api/me", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// healthy fails the test unless the healthcheck, which trusts only the mirror, passes.
func (r *acmeRig) healthy() {
	r.t.Helper()
	if err := Healthcheck(testsupport.Context(r.t), r.address(), HealthcheckCertFile(r.cfg)); err != nil {
		r.t.Errorf("the healthcheck fails: %v", err)
	}
}

// issuedBy reports whether leaf was issued by the fake CA.
func (r *acmeRig) issuedBy(leaf *x509.Certificate) bool {
	_, err := leaf.Verify(x509.VerifyOptions{DNSName: acmeHost, Roots: r.fake.IssuerPool()})
	return err == nil
}

// mirrorLeaf is the first certificate in the healthcheck mirror.
func (r *acmeRig) mirrorLeaf() *x509.Certificate {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.cfg.DataDir, selfSignedDir, mirrorCertFile))
	if err != nil {
		r.t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		r.t.Fatal("the mirror holds no certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		r.t.Fatal(err)
	}
	return leaf
}

// A Let's Encrypt certificate is issued through the real listener and swapped in with no restart
// (ACME-04, CHAL-06, D-01, D-02): the fake CA fetches the token over plain HTTP from the sniffing
// listener, the next handshake presents the new leaf, a session cookie from before the swap still
// works, the healthcheck mirror matches, and no pin or HSTS is published. A restart serves the stored
// certificate at once.
func TestACMEIssueSwap(t *testing.T) {
	r := newACMERig(t, acmetest.Options{}, nil)
	srv := r.startNew()
	pinBefore := srv.https.Pin()
	if pinBefore == "" || len(srv.https.Pins()) != 1 {
		t.Fatalf("the self-signed certificate publishes pins %v, want one", srv.https.Pins())
	}
	before := leafOf(t, srv.https.Config())

	client := r.client(r.mirrorPool())
	cookie := r.signIn(client)

	r.issue()

	// The next handshake presents the Let's Encrypt leaf.
	leaf, err := r.handshake(r.fake.IssuerPool())
	if err != nil {
		t.Fatalf("the handshake after the swap does not verify against the fake CA: %v", err)
	}
	if leaf.SerialNumber.Cmp(before.SerialNumber) == 0 || !r.issuedBy(leaf) {
		t.Fatalf("the handshake presents %q (serial %v), want a certificate the fake CA issued", leaf.Subject, leaf.SerialNumber)
	}
	if !bytes.Equal(leafOf(t, srv.https.Config()).Raw, leaf.Raw) {
		t.Error("the runtime and the handshake disagree about the served certificate")
	}

	// The challenge was answered over plain HTTP by the listener on its existing port.
	if got := r.fake.Count(acmetest.EndpointChallenge); got != 1 {
		t.Errorf("the fake CA was asked to validate %d times, want 1", got)
	}
	if got := r.fake.Orders(); len(got) != 1 || len(got[0].Names) != 1 || got[0].Names[0] != acmeHost {
		t.Errorf("the order = %+v, want one for %s", got, acmeHost)
	}
	if n := srv.acme.Challenges().Len(); n != 0 {
		t.Errorf("%d challenge tokens are still presented after the run", n)
	}

	// Nobody is signed out: the old cookie still works, over the new certificate.
	newClient := r.client(r.fake.IssuerPool())
	if got := r.meStatus(newClient, cookie); got != http.StatusOK {
		t.Errorf("the session from before the swap answers %d, want 200", got)
	}

	// The healthcheck trusts the mirror only, and it matches what is served.
	mirrorMatchesServed(t, srv, r.cfg.DataDir)
	r.healthy()

	// Let's Encrypt publishes no pin and sends no HSTS in this phase.
	if got := srv.https.Pins(); got != nil {
		t.Errorf("Pins() = %v with a Let's Encrypt certificate, want none", got)
	}
	if srv.https.HSTSAllowed() {
		t.Error("HSTS is allowed with a Let's Encrypt certificate, want it off")
	}
	if got := srv.https.served().Source; got != api.SourceLetsEncrypt {
		t.Errorf("served source = %q, want %q", got, api.SourceLetsEncrypt)
	}

	// A restart on the same data directory serves the stored certificate before it listens.
	stored := leaf.Raw
	r.stop()
	again := r.startNew()
	if got := leafOf(t, again.https.Config()); !bytes.Equal(got.Raw, stored) {
		t.Error("after a restart the server does not serve the stored Let's Encrypt certificate")
	}
	if again.https.Kind() != tlsKindACME {
		t.Errorf("after a restart the kind is %d, want tlsKindACME", again.https.Kind())
	}
	mirrorMatchesServed(t, again, r.cfg.DataDir)
	r.healthy()
	if _, err := r.handshake(r.fake.IssuerPool()); err != nil {
		t.Errorf("the handshake after a restart: %v", err)
	}
}
