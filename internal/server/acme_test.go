// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/authn"
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
	// host is the name the public URL carries; base is the public URL itself, with no path.
	host, base string

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
	r.base = r.env["TIERPLAN_PUBLIC_URL"]
	pub, err := url.Parse(r.base)
	if err != nil {
		t.Fatal(err)
	}
	r.host = pub.Hostname()
	r.opts = append([]Option{WithACMEClock(r.clock), WithTLSRefreshClock(r.tlsClock)}, opts...)
	t.Cleanup(r.stop)
	return r
}

// setEnv changes the environment variables of the next start; an empty value removes one.
func (r *acmeRig) setEnv(vars map[string]string) {
	r.t.Helper()
	for k, v := range vars {
		if v == "" {
			delete(r.env, k)
		} else {
			r.env[k] = v
		}
	}
	r.cfg = testConfig(r.t, r.env)
}

// counts is what the fake CA has been asked so far at the endpoints a quiet server must not reach.
func (r *acmeRig) counts() map[string]int {
	out := map[string]int{}
	for _, e := range []string{
		acmetest.EndpointDirectory, acmetest.EndpointNewAccount, acmetest.EndpointNewOrder, acmetest.EndpointRenewalInfo,
	} {
		out[e] = r.fake.Count(e)
	}
	return out
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
	// The scheduler runs its first round and arms its next timer: from here it is waiting on the
	// clock, and step moves it on.
	r.clock.WaitArmed(r.t)
	return srv
}

// step moves the ACME clock on by d, which wakes the scheduler, and returns once its round is over
// and it waits on the clock again. A run that round started may still be going: awaitRuns waits for it.
func (r *acmeRig) step(d time.Duration) {
	r.t.Helper()
	r.clock.Advance(d)
	r.clock.WaitArmed(r.t)
}

// awaitRuns waits for every certificate run the server has started to end.
func (r *acmeRig) awaitRuns() {
	r.t.Helper()
	for _, j := range r.srv.jobs.List(func(j jobs.Job) bool { return j.Kind == acme.JobKind }) {
		r.waitJob(j.ID)
	}
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
	if id := rs.JobID; id == "" {
		r.t.Fatal("no job was started")
	} else {
		r.waitJob(id)
	}
	r.requireNoFailedStep(rs.JobID)
}

// waitJob waits for a job of the server to end.
func (r *acmeRig) waitJob(id string) jobs.Job {
	r.t.Helper()
	job, err := r.srv.jobs.Wait(testsupport.Context(r.t), id)
	if err != nil {
		r.t.Fatalf("wait for job %s: %v", id, err)
	}
	return job
}

// requireNoFailedStep fails the test when a step of the finished job failed.
func (r *acmeRig) requireNoFailedStep(id string) {
	r.t.Helper()
	job, ok := r.srv.jobs.Get(id)
	if !ok {
		r.t.Fatalf("job %s is gone", id)
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
	// The certificate is judged at the ACME clock's time: a test that ages the clock by months
	// would otherwise present a certificate that is not valid yet by the wall clock.
	conn, err := (&tls.Dialer{Config: &tls.Config{ServerName: r.host, RootCAs: roots, MinVersion: tls.VersionTLS12, Time: r.clock.Now}}).
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
	resp, err := c.Post(r.base+"/api/auth/login", "application/json",
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

// csrf is the CSRF token the signed-in session sends on a change.
func (r *acmeRig) csrf(c *http.Client, cookie *http.Cookie) string {
	r.t.Helper()
	req, err := http.NewRequestWithContext(testsupport.Context(r.t), http.MethodGet, r.base+"/api/me", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got.CSRFToken == "" {
		r.t.Fatalf("no CSRF token from /api/me: %v", err)
	}
	return got.CSRFToken
}

// meStatus is the status of an authenticated GET with the cookie.
func (r *acmeRig) meStatus(c *http.Client, cookie *http.Cookie) int {
	r.t.Helper()
	req, err := http.NewRequestWithContext(testsupport.Context(r.t), http.MethodGet, r.base+"/api/me", nil)
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

// issuedBy reports whether leaf was issued by the fake CA, judged at the ACME clock's time.
func (r *acmeRig) issuedBy(leaf *x509.Certificate) bool {
	_, err := leaf.Verify(x509.VerifyOptions{DNSName: r.host, Roots: r.fake.IssuerPool(), CurrentTime: r.clock.Now()})
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

// After a Let's Encrypt certificate was served, Stop puts the self-signed certificate back with the
// pin it always had, so every agent installed with that pin still connects (D-16, T-14-28).
func TestACMEStopReturnsToTheSelfSignedPin(t *testing.T) {
	r := newACMERig(t, acmetest.Options{}, nil)
	srv := r.startNew()
	pinBefore := srv.https.Pin()
	if pinBefore == "" {
		t.Fatal("the self-signed certificate publishes no pin")
	}
	r.issue()
	if srv.https.Kind() != tlsKindACME || srv.https.Pin() != "" {
		t.Fatalf("after the issue kind = %d and pin = %q, want Let's Encrypt and no pin", srv.https.Kind(), srv.https.Pin())
	}

	if err := srv.acme.Stop(context.Background(), "test"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	leaf, err := r.handshake(r.mirrorPool())
	if err != nil {
		t.Fatalf("the handshake after Stop does not verify against the mirror: %v", err)
	}
	if r.issuedBy(leaf) {
		t.Error("the handshake after Stop still presents the Let's Encrypt certificate")
	}
	if got := authn.SPKIPin(leaf); got != pinBefore {
		t.Errorf("pin after Stop = %q, want the pin from before the issue, %q", got, pinBefore)
	}
	if got := srv.https.Pins(); len(got) != 1 || got[0] != pinBefore {
		t.Errorf("Pins() after Stop = %v, want [%s]", got, pinBefore)
	}
	if srv.https.HSTSAllowed() {
		t.Error("HSTS is allowed over the self-signed certificate")
	}
	mirrorMatchesServed(t, srv, r.cfg.DataDir)
	r.healthy()
}

// When the Let's Encrypt certificate reaches its end while renewals keep failing, the self-signed
// certificate is served by itself with the pin it always had, the mirror follows and the healthcheck
// stays green; the renewal that works later puts Let's Encrypt back (D-15).
func TestACMEExpiryFallsBackToSelfSigned(t *testing.T) {
	r := newACMERig(t, acmetest.Options{}, nil)
	srv := r.startNew()
	pinBefore := srv.https.Pin()
	r.issue()
	first := leafOf(t, srv.https.Config())

	// One renewal fails; the clock jumps past the end of the certificate.
	r.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{Status: 400, Type: "malformed", Detail: "scripted refusal"})
	r.step(first.NotAfter.Sub(r.clock.Now()) + time.Hour)
	r.awaitRuns()

	leaf, err := r.handshake(r.mirrorPool())
	if err != nil {
		t.Fatalf("the handshake after the expiry does not verify against the mirror: %v", err)
	}
	if got := authn.SPKIPin(leaf); got != pinBefore || r.issuedBy(leaf) {
		t.Errorf("after the expiry the handshake presents pin %q (issued by the fake CA: %v), want the self-signed pin %q", got, r.issuedBy(leaf), pinBefore)
	}
	if got := srv.https.Pins(); len(got) != 1 || got[0] != pinBefore {
		t.Errorf("Pins() after the expiry = %v, want [%s]", got, pinBefore)
	}
	st, err := srv.acme.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Fallback {
		t.Error("Status.Fallback = false after the certificate expired, want true")
	}
	mirrorMatchesServed(t, srv, r.cfg.DataDir)
	r.healthy()

	// The next renewal works and Let's Encrypt is served again. The healthcheck is not asked here: it
	// judges the certificate by the wall clock, and this one was issued for a clock months ahead.
	r.step(2 * time.Minute)
	r.awaitRuns()
	renewed, err := r.handshake(r.fake.IssuerPool())
	if err != nil {
		t.Fatalf("the handshake after the renewal: %v", err)
	}
	if renewed.SerialNumber.Cmp(first.SerialNumber) == 0 || !r.issuedBy(renewed) {
		t.Errorf("after the renewal the handshake presents serial %v, want a new certificate from the fake CA", renewed.SerialNumber)
	}
	if srv.https.Kind() != tlsKindACME || srv.https.Pins() != nil {
		t.Errorf("after the renewal kind = %d and pins = %v, want Let's Encrypt and none", srv.https.Kind(), srv.https.Pins())
	}
	mirrorMatchesServed(t, srv, r.cfg.DataDir)
}

// While a Let's Encrypt certificate is served, an agent URL change (the public URL is fixed by the environment in these tests) makes the self-signed source sign
// again for its row, but the mirror the healthcheck trusts keeps the certificate being served.
func TestSelfSignedRefreshDoesNotOverwriteTheACMEMirror(t *testing.T) {
	r := newACMERig(t, acmetest.Options{}, nil)
	srv := r.startNew()
	r.issue()
	ctx := context.Background()
	rows := sealedCertStore{st: srv.Store(), v: srv.Vault()}
	before, _, found, err := rows.Load(ctx, selfSignedID)
	if err != nil || !found {
		t.Fatalf("the self-signed row: %v (found %v)", err, found)
	}

	// The public URL is fixed by the environment here, so the agent URL is what changes the names.
	srv.Instance().ApplyAgentURL("https://agent.other.test")
	r.tlsClock.Tick(t, selfSignedNameCheck)
	r.tlsClock.WaitArmed(t) // the loop arms again once Refresh has run

	after, _, _, err := rows.Load(ctx, selfSignedID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("the self-signed row was not signed again for the new name, so the test did not reach the refresh")
	}
	block, _ := pem.Decode(after)
	signed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := signed.VerifyHostname("agent.other.test"); err != nil {
		t.Errorf("the self-signed row does not cover the new name: %v", err)
	}
	mirrorMatchesServed(t, srv, r.cfg.DataDir)
	if !r.issuedBy(r.mirrorLeaf()) {
		t.Error("the mirror no longer holds the Let's Encrypt certificate")
	}
	r.healthy()
}

// suppliedPair writes a certificate and key for the rig's name and returns the paths and the leaf.
func suppliedPair(t *testing.T, host string) (crt, key string, leaf *x509.Certificate) {
	t.Helper()
	dir := t.TempDir()
	crt, key = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	return crt, key, writePair(t, crt, key, host)
}

// With TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE set the files are served and Let's Encrypt is
// refused: nothing is registered or ordered, and nothing reaches the CA (D-12).
func TestACMECertFilesDisableIssue(t *testing.T) {
	crt, key, leaf := suppliedPair(t, acmeHost)
	r := newACMERig(t, acmetest.Options{}, map[string]string{"TIERPLAN_TLS_CERT_FILE": crt, "TIERPLAN_TLS_KEY_FILE": key})
	srv := r.startNew()

	if srv.https.Kind() != tlsKindSupplied {
		t.Fatalf("kind = %d, want the supplied files", srv.https.Kind())
	}
	if got := leafOf(t, srv.https.Config()); !bytes.Equal(got.Raw, leaf.Raw) {
		t.Error("the server does not serve the supplied certificate")
	}
	ctx := context.Background()
	if _, err := srv.acme.Register(ctx, "", true); !errors.Is(err, acme.ErrCertFiles) {
		t.Errorf("Register = %v, want ErrCertFiles", err)
	}
	if _, err := srv.acme.Issue(ctx, "test"); !errors.Is(err, acme.ErrCertFiles) {
		t.Errorf("Issue = %v, want ErrCertFiles", err)
	}
	st, err := srv.acme.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.CertFiles || st.PausedReason != "" {
		t.Errorf("Status CertFiles = %v, PausedReason = %q; want true and empty (nothing stored to pause)", st.CertFiles, st.PausedReason)
	}
	for e, n := range r.counts() {
		if n != 0 {
			t.Errorf("the CA was asked %d times at %s, want none", n, e)
		}
	}
}

// With a Let's Encrypt certificate stored, setting the files later serves the files and keeps the
// Let's Encrypt rows; removing them and restarting serves Let's Encrypt again, and while the files are
// served the scheduler asks the CA nothing (D-12, D-13).
func TestACMECertFilesPauseAndKeepLetsEncrypt(t *testing.T) {
	r := newACMERig(t, acmetest.Options{}, nil)
	srv := r.startNew()
	r.issue()
	ctx := context.Background()
	letsEncrypt := leafOf(t, srv.https.Config())

	snapshot := func(srv *Server) (chain []byte, st acme.State, acct acme.Account) {
		t.Helper()
		store := acme.NewStore(srv.Store(), srv.Vault())
		chain, _, found, err := store.Certificate(ctx)
		if err != nil || !found {
			t.Fatalf("the stored certificate: %v (found %v)", err, found)
		}
		if st, err = store.State(ctx); err != nil {
			t.Fatal(err)
		}
		var ok bool
		if acct, _, ok, err = store.Account(ctx, r.fake.DirectoryURL()); err != nil || !ok {
			t.Fatalf("the stored account: %v (found %v)", err, ok)
		}
		return chain, st, acct
	}
	chain, state, account := snapshot(srv)
	r.stop()

	// The files are set: they are served, and the Let's Encrypt rows are as they were.
	crt, key, leaf := suppliedPair(t, acmeHost)
	r.setEnv(map[string]string{"TIERPLAN_TLS_CERT_FILE": crt, "TIERPLAN_TLS_KEY_FILE": key})
	srv = r.startNew()
	if srv.https.Kind() != tlsKindSupplied || !bytes.Equal(leafOf(t, srv.https.Config()).Raw, leaf.Raw) {
		t.Fatal("the server does not serve the supplied files")
	}
	st, err := srv.acme.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.PausedReason != acme.PausedCertFiles || !st.CertFiles || !st.HasState {
		t.Errorf("Status PausedReason = %q, CertFiles = %v, HasState = %v; want cert_files, true, true", st.PausedReason, st.CertFiles, st.HasState)
	}
	gotChain, gotState, gotAccount := snapshot(srv)
	if !bytes.Equal(gotChain, chain) || !reflect.DeepEqual(gotState, state) || !reflect.DeepEqual(gotAccount, account) {
		t.Error("the Let's Encrypt certificate, state or account changed while the files were served")
	}
	// Past the renewal point, the scheduler still asks the CA nothing.
	before := r.counts()
	r.step(61 * 24 * time.Hour)
	if after := r.counts(); !reflect.DeepEqual(before, after) {
		t.Errorf("the CA was asked while certificate files were served: before %v, after %v", before, after)
	}
	r.stop()

	// The files are removed: Let's Encrypt is served again, as it was. Its renewal is due by now, so
	// the first round of the new server tries one; it is refused, which leaves the certificate alone.
	r.setEnv(map[string]string{"TIERPLAN_TLS_CERT_FILE": "", "TIERPLAN_TLS_KEY_FILE": ""})
	r.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{Status: 400, Type: "malformed", Detail: "scripted refusal"})
	srv = r.startNew()
	if srv.https.Kind() != tlsKindACME || !bytes.Equal(leafOf(t, srv.https.Config()).Raw, letsEncrypt.Raw) {
		t.Errorf("after the files were removed the kind is %d, want the stored Let's Encrypt certificate served again", srv.https.Kind())
	}
	r.awaitRuns()
}

// A start-up with the test-only directory set logs one loud warning that names the variable (D-19); a
// start-up without it logs none.
func TestACMEDirectoryOverrideWarnsAtStartUp(t *testing.T) {
	build := func(env map[string]string) string {
		t.Helper()
		var logs bytes.Buffer
		srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Close(); err != nil {
			t.Fatal(err)
		}
		return logs.String()
	}
	warnings := func(logs string) (n int) {
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, "TIERPLAN_ACME_DIRECTORY_URL") {
				n++
			}
		}
		return n
	}
	fake := acmetest.NewFake(t, acmetest.Options{})
	got := build(map[string]string{
		"TIERPLAN_DATA_DIR": t.TempDir() + "/data", "TIERPLAN_ACME_DIRECTORY_URL": fake.DirectoryURL(), "TIERPLAN_ACME_CA_FILE": fake.CAFile(t),
	})
	if n := warnings(got); n != 1 || !strings.Contains(got, fake.DirectoryURL()) {
		t.Errorf("the log has %d warnings naming the variable (want one, with the URL):\n%s", n, got)
	}
	if got := build(map[string]string{"TIERPLAN_DATA_DIR": t.TempDir() + "/data"}); warnings(got) != 0 {
		t.Errorf("a start-up with the real directory warns about the test one:\n%s", got)
	}
}
