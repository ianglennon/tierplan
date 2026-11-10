// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package acmetest is a fake ACME directory for tests (Phase 14, D-17). It is an in-process HTTPS
// server that lego's acme/api.Core drives end to end: account, order, http-01 challenge, finalize,
// certificate download and renewal information. Its time is an injected clock, so a test can issue a
// certificate and age it without waiting; its answers can be scripted (rate limits, holds, failures,
// renewal windows) and it records what it was asked.
//
// Only a custom server can issue on an injected clock and answer with a scripted Retry-After; Pebble
// keeps no rate limits and runs on real time.
//
// The package is imported by tests only. Nothing in it is linked into the tierplan binary.
package acmetest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-acme/lego/v5/acme"
	"github.com/go-acme/lego/v5/acme/api"
	"github.com/go-jose/go-jose/v4"
)

// Endpoint names a place the fake counts calls to and can be scripted to fail at.
const (
	EndpointDirectory   = "directory"
	EndpointNewAccount  = "newAccount"
	EndpointNewOrder    = "newOrder"
	EndpointOrder       = "order"
	EndpointAuthz       = "authz"
	EndpointChallenge   = "challenge"
	EndpointFinalize    = "finalize"
	EndpointCert        = "cert"
	EndpointRenewalInfo = "renewalInfo"
)

// problemNS is the prefix of an ACME problem type (RFC 8555 section 6.7).
const problemNS = "urn:ietf:params:acme:error:"

// Problem is an ACME problem document the fake answers with.
type Problem struct {
	// Status is the HTTP status; 0 means 400.
	Status int
	// Type is the problem type, either whole ("urn:ietf:params:acme:error:rateLimited") or the
	// short name after the prefix ("rateLimited").
	Type   string
	Detail string
	// RetryAfterSeconds, when positive, is sent as a Retry-After header in delta-seconds. The fake
	// never writes the HTTP-date form (RESEARCH Pitfall 8).
	RetryAfterSeconds int
}

// Options configure NewFake. The zero value is a working directory on the wall clock that marks
// every challenge valid without looking.
type Options struct {
	// Clock supplies the time of every certificate, order expiry and validation. Nil means the
	// wall clock. Certificate times are whole seconds, as an X.509 certificate holds.
	Clock interface{ Now() time.Time }
	// Validate maps a name being validated to the "host:port" the fake performs the http-01 GET
	// against. Nil marks challenges valid without a GET. A function returning "" means the name
	// cannot be reached.
	Validate func(host string) string
	// Profiles are the certificate profiles offered, with the lifetime each issues. Nil means
	// classic (90 days) and shortlived (160 hours).
	Profiles map[string]time.Duration
	// DefaultProfile is the profile an order without one gets. Empty means "classic".
	DefaultProfile string
	// TermsOfService, when set, is advertised and must be agreed to on registration.
	TermsOfService string
	// NoRenewalInfo hides the renewalInfo endpoint, as a CA without ARI does.
	NoRenewalInfo bool
}

// OrderSeen is what the fake was asked for in one accepted newOrder request.
type OrderSeen struct {
	Names    []string
	Profile  string
	Replaces string
}

// AccountSeen is what the fake was asked for in one newAccount request that created an account.
type AccountSeen struct {
	Contact              []string
	TermsOfServiceAgreed bool
}

// Fake is a running fake ACME directory.
type Fake struct {
	tb    testing.TB
	opts  Options
	clock interface{ Now() time.Time }
	srv   *httptest.Server
	http  *http.Client // performs the http-01 GETs

	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	mu       sync.Mutex
	seq      int
	nonces   map[string]struct{}
	accounts map[string]*account // by account URL (the kid)
	orders   map[string]*order
	authzs   map[string]*authz
	certs    map[string]*issued // by certificate id (the /cert/<id> path)
	byARI    map[string]*issued // by ARI certificate id

	// Scripting and observation (mu guards all of it).
	scripted   map[string][]Problem     // per endpoint, answered in turn
	badNonce   bool                     // the next signed request answers badNonce
	failVal    map[string]Problem       // per name, the validation fails with this
	holdVal    bool                     // challenges stay processing
	heldVal    []check                  // the challenges held
	holdFin    bool                     // finalized orders stay processing
	heldFin    []*order                 // the orders held
	windows    map[string]renewalWindow // per ARI certificate id
	replacedBy map[string]*order        // per ARI certificate id, the order that replaces it
	counts     map[string]int
	seen       []OrderSeen
	accountsIn []AccountSeen
	hits       map[string]int
}

// renewalWindow is a scripted renewalInfo answer.
type renewalWindow struct {
	start, end time.Time
	retryAfter time.Duration
}

type account struct {
	kid   string
	key   *jose.JSONWebKey
	thumb string
}

type order struct {
	id         string
	owner      string // thumbprint of the account key
	names      []string
	profile    string // the profile that sets the lifetime (the requested one, or the default)
	replaces   string
	expires    time.Time
	authzs     []*authz
	finalizing bool
	csr        *x509.CertificateRequest // the CSR of an order held in processing
	cert       *issued
}

type authz struct {
	id        string
	order     *order
	name      string
	token     string
	status    string // pending, valid, invalid, deactivated
	chall     string // the challenge's own status: pending, processing, valid, invalid
	problem   *Problem
	validated time.Time
}

type issued struct {
	id    string
	ari   string
	owner string
	leaf  *x509.Certificate
	chain []byte // PEM, leaf then issuer
	order *order
}

// NewFake starts a fake ACME directory over TLS and stops it when the test ends.
func NewFake(t testing.TB, o Options) *Fake {
	t.Helper()
	if o.Clock == nil {
		o.Clock = wallClock{}
	}
	if o.Profiles == nil {
		o.Profiles = map[string]time.Duration{"classic": 90 * 24 * time.Hour, "shortlived": 160 * time.Hour}
	}
	if o.DefaultProfile == "" {
		o.DefaultProfile = "classic"
	}
	f := &Fake{
		tb: t, opts: o, clock: o.Clock,
		http:     &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 30 * time.Second},
		nonces:   map[string]struct{}{},
		accounts: map[string]*account{},
		orders:   map[string]*order{},
		authzs:   map[string]*authz{},
		certs:    map[string]*issued{},
		byARI:    map[string]*issued{},

		scripted:   map[string][]Problem{},
		failVal:    map[string]Problem{},
		windows:    map[string]renewalWindow{},
		replacedBy: map[string]*order{},
		counts:     map[string]int{},
		hits:       map[string]int{},
	}
	f.newCA(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /directory", f.plain(EndpointDirectory, f.directory))
	mux.HandleFunc("HEAD /new-nonce", f.newNonce)
	mux.HandleFunc("GET /new-nonce", f.newNonce)
	mux.HandleFunc("POST /new-account", f.signed(EndpointNewAccount, true, f.newAccount))
	mux.HandleFunc("POST /acct/{id}", f.signed("", false, f.getAccount))
	mux.HandleFunc("POST /new-order", f.signed(EndpointNewOrder, false, f.newOrder))
	mux.HandleFunc("POST /order/{id}", f.signed(EndpointOrder, false, f.getOrder))
	mux.HandleFunc("POST /authz/{id}", f.signed(EndpointAuthz, false, f.getAuthz))
	mux.HandleFunc("POST /chall/{id}", f.signed(EndpointChallenge, false, f.postChallenge))
	mux.HandleFunc("POST /finalize/{id}", f.signed(EndpointFinalize, false, f.finalize))
	mux.HandleFunc("POST /cert/{id}", f.signed(EndpointCert, false, f.getCert))
	mux.HandleFunc("GET /renewal-info/{id}", f.plain(EndpointRenewalInfo, f.renewalInfo))

	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every response carries a fresh nonce (RFC 8555 section 6.5).
		w.Header().Set("Replay-Nonce", f.issueNonce())
		mux.ServeHTTP(w, r)
	}))
	// A client that does not trust the fake's root fails its handshake on purpose in some tests; the
	// server's log line for that is noise.
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.srv.StartTLS()
	t.Cleanup(func() {
		f.http.CloseIdleConnections()
		f.srv.Close()
	})
	return f
}

// wallClock is the clock a Fake without an injected one uses.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// newCA makes the issuing CA, valid either side of the clock by years so that a test that ages a
// certificate by months still verifies it.
func (f *Fake) newCA(t testing.TB) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("acmetest: CA key: %v", err)
	}
	now := f.clock.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "TierPlan fake ACME CA"},
		NotBefore:             now.AddDate(-10, 0, 0),
		NotAfter:              now.AddDate(30, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("acmetest: CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("acmetest: parse CA certificate: %v", err)
	}
	f.caKey, f.caCert = key, cert
}

// DirectoryURL is the URL of the directory, for the ACME client's CA directory setting.
func (f *Fake) DirectoryURL() string { return f.srv.URL + "/directory" }

// RootPool trusts the fake's HTTPS certificate: the pool an ACME client under test verifies the
// directory with. There is no way to skip verification; the fake is reached the way a real CA is.
func (f *Fake) RootPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	return pool
}

// CAFile writes the fake's HTTPS certificate as PEM to a file in the test's temporary directory and
// returns its path, for TIERPLAN_ACME_CA_FILE.
func (f *Fake) CAFile(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-acme-root.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("acmetest: write CA file: %v", err)
	}
	return path
}

// IssuerFile writes the certificate of the CA that signs the certificates the fake issues as PEM to a
// file in the test's temporary directory and returns its path. A process under test that must trust
// what the fake issues, such as an agent given SSL_CERT_FILE, reads it as its public roots.
func (f *Fake) IssuerFile(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-acme-issuer.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: f.caCert.Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("acmetest: write issuer file: %v", err)
	}
	return path
}

// IssuerPool trusts the CA that signs the certificates the fake issues.
func (f *Fake) IssuerPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(f.caCert)
	return pool
}

// issueNonce makes a nonce and remembers it until it is used.
func (f *Fake) issueNonce() string {
	n := randomToken(12)
	f.mu.Lock()
	f.nonces[n] = struct{}{}
	f.mu.Unlock()
	return n
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source failing is not a case a test recovers from
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// nextID is a new identifier for an account, order, authorization or certificate. The caller holds mu.
func (f *Fake) nextID() string {
	f.seq++
	return strconv.Itoa(f.seq)
}

// request is a verified ACME request as a handler sees it.
type request struct {
	w       http.ResponseWriter
	r       *http.Request
	acct    *account         // the account a kid request names
	jwk     *jose.JSONWebKey // the key a newAccount request embeds
	payload []byte
}

type handler func(q *request)

// plain wraps an unsigned GET: it counts the call, answers a scripted failure, and holds mu for
// the handler.
func (f *Fake) plain(endpoint string, h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if p, ok := f.reach(endpoint); ok {
			f.problem(w, p)
			return
		}
		h(w, r)
	}
}

// reach counts a call to endpoint and returns the failure scripted for it, if any. The caller holds
// mu. An empty endpoint is not counted or scripted.
func (f *Fake) reach(endpoint string) (Problem, bool) {
	if endpoint == "" {
		return Problem{}, false
	}
	f.counts[endpoint]++
	queue := f.scripted[endpoint]
	if len(queue) == 0 {
		return Problem{}, false
	}
	f.scripted[endpoint] = queue[1:]
	return queue[0], true
}

// signed wraps a POST: it reads the JWS, checks the nonce, the URL and the signature, and hands the
// verified request to h with mu held. A handler that must not hold mu across I/O releases it itself.
// newAccount is the one request signed with an embedded key; every other is signed by an account.
func (f *Fake) signed(endpoint string, embeddedKey bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			f.problem(w, Problem{Type: "malformed", Detail: "cannot read the request body"})
			return
		}
		req, err := parseSigned(body)
		if err != nil {
			f.problem(w, Problem{Type: "malformed", Detail: err.Error()})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.badNonce {
			f.badNonce = false
			f.problem(w, Problem{Type: "badNonce", Detail: "JWS has an invalid anti-replay nonce (scripted)"})
			return
		}
		if _, ok := f.nonces[req.nonce]; !ok {
			f.problem(w, Problem{Type: "badNonce", Detail: "JWS has an invalid anti-replay nonce"})
			return
		}
		delete(f.nonces, req.nonce)
		if want := f.srv.URL + r.URL.Path; req.url != want {
			f.problem(w, Problem{Status: http.StatusUnauthorized, Type: "unauthorized", Detail: fmt.Sprintf("JWS url %q does not match the request URL %q", req.url, want)})
			return
		}
		// A call that got this far reached the endpoint: it is counted, and a failure scripted for it
		// is answered, before the account is looked up.
		if p, ok := f.reach(endpoint); ok {
			f.problem(w, p)
			return
		}
		q := &request{w: w, r: r}
		var key *jose.JSONWebKey
		switch {
		case embeddedKey && req.jwk != nil:
			q.jwk, key = req.jwk, req.jwk
		case embeddedKey:
			f.problem(w, Problem{Type: "malformed", Detail: "newAccount must be signed with an embedded jwk"})
			return
		case req.kid == "":
			f.problem(w, Problem{Type: "malformed", Detail: "request must name its account with kid"})
			return
		default:
			acct, ok := f.accounts[req.kid]
			if !ok {
				f.problem(w, Problem{Type: "accountDoesNotExist", Detail: "no account with that URL"})
				return
			}
			q.acct, key = acct, acct.key
		}
		payload, err := req.verify(key)
		if err != nil {
			f.problem(w, Problem{Status: http.StatusUnauthorized, Type: "unauthorized", Detail: err.Error()})
			return
		}
		q.payload = payload
		h(q)
	}
}

// problem writes an ACME problem document.
func (f *Fake) problem(w http.ResponseWriter, p Problem) {
	if p.Status == 0 {
		p.Status = http.StatusBadRequest
	}
	if !strings.Contains(p.Type, ":") {
		p.Type = problemNS + p.Type
	}
	if p.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(p.RetryAfterSeconds))
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": p.Type, "detail": p.Detail, "status": p.Status})
}

// reply writes v as JSON with the status.
func (f *Fake) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) newNonce(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (f *Fake) directory(w http.ResponseWriter, _ *http.Request) {
	profiles := map[string]string{}
	for name, life := range f.opts.Profiles {
		profiles[name] = "certificates valid for " + life.String()
	}
	d := acme.Directory{
		NewNonceURL:   f.srv.URL + "/new-nonce",
		NewAccountURL: f.srv.URL + "/new-account",
		NewOrderURL:   f.srv.URL + "/new-order",
		RevokeCertURL: f.srv.URL + "/revoke-cert",
		KeyChangeURL:  f.srv.URL + "/key-change",
		Meta:          acme.Meta{TermsOfService: f.opts.TermsOfService, Profiles: profiles},
	}
	if !f.opts.NoRenewalInfo {
		d.RenewalInfo = f.srv.URL + "/renewal-info"
	}
	f.reply(w, http.StatusOK, d)
}

func (f *Fake) accountView(a *account) acme.Account {
	return acme.Account{Status: acme.StatusValid, Orders: a.kid + "/orders"}
}

func (f *Fake) newAccount(q *request) {
	var req acme.Account
	if len(q.payload) > 0 {
		if err := json.Unmarshal(q.payload, &req); err != nil {
			f.problem(q.w, Problem{Type: "malformed", Detail: "account payload: " + err.Error()})
			return
		}
	}
	thumb, err := thumbprint(q.jwk)
	if err != nil {
		f.problem(q.w, Problem{Type: "badPublicKey", Detail: err.Error()})
		return
	}
	for _, a := range f.accounts {
		if a.thumb == thumb {
			q.w.Header().Set("Location", a.kid)
			f.reply(q.w, http.StatusOK, f.accountView(a))
			return
		}
	}
	if req.OnlyReturnExisting {
		f.problem(q.w, Problem{Type: "accountDoesNotExist", Detail: "no account for that key"})
		return
	}
	if f.opts.TermsOfService != "" && !req.TermsOfServiceAgreed {
		f.problem(q.w, Problem{Status: http.StatusForbidden, Type: "userActionRequired", Detail: "the terms of service must be agreed to"})
		return
	}
	a := &account{kid: f.srv.URL + "/acct/" + f.nextID(), key: q.jwk, thumb: thumb}
	f.accountsIn = append(f.accountsIn, AccountSeen{Contact: slices.Clone(req.Contact), TermsOfServiceAgreed: req.TermsOfServiceAgreed})
	f.accounts[a.kid] = a
	q.w.Header().Set("Location", a.kid)
	f.reply(q.w, http.StatusCreated, f.accountView(a))
}

func (f *Fake) getAccount(q *request) {
	q.w.Header().Set("Location", q.acct.kid)
	f.reply(q.w, http.StatusOK, f.accountView(q.acct))
}

func (f *Fake) newOrder(q *request) {
	var req acme.Order
	if err := json.Unmarshal(q.payload, &req); err != nil {
		f.problem(q.w, Problem{Type: "malformed", Detail: "order payload: " + err.Error()})
		return
	}
	if len(req.Identifiers) == 0 {
		f.problem(q.w, Problem{Type: "malformed", Detail: "an order needs at least one identifier"})
		return
	}
	var names []string
	for _, id := range req.Identifiers {
		switch {
		case id.Type != "dns":
			f.problem(q.w, Problem{Type: "unsupportedIdentifier", Detail: fmt.Sprintf("identifier type %q is not supported", id.Type)})
			return
		case id.Value == "" || strings.HasPrefix(id.Value, "*."):
			f.problem(q.w, Problem{Type: "rejectedIdentifier", Detail: fmt.Sprintf("name %q cannot be validated by http-01", id.Value)})
			return
		}
		names = append(names, strings.ToLower(id.Value))
	}
	profile := req.Profile
	if profile == "" {
		profile = f.opts.DefaultProfile
	}
	if _, ok := f.opts.Profiles[profile]; !ok {
		f.problem(q.w, Problem{Type: "invalidProfile", Detail: fmt.Sprintf("profile %q is not offered", profile)})
		return
	}
	if req.Replaces != "" {
		prev, ok := f.byARI[req.Replaces]
		switch {
		case !ok:
			f.problem(q.w, Problem{Status: http.StatusNotFound, Type: "malformed", Detail: "the certificate to replace is not known"})
			return
		case prev.owner != q.acct.thumb:
			f.problem(q.w, Problem{Status: http.StatusForbidden, Type: "unauthorized", Detail: "the certificate to replace was issued to another account"})
			return
		}
		if other := f.replacedBy[req.Replaces]; other != nil {
			if status, _ := f.orderStatus(other); status != acme.StatusInvalid {
				f.problem(q.w, Problem{Status: http.StatusConflict, Type: "alreadyReplaced", Detail: "another order already replaces that certificate"})
				return
			}
		}
	}
	o := &order{
		id: f.nextID(), owner: q.acct.thumb, names: names, profile: profile, replaces: req.Replaces,
		expires: f.clock.Now().Add(7 * 24 * time.Hour),
	}
	for _, name := range names {
		a := &authz{
			id: f.nextID(), order: o, name: name, token: randomToken(24),
			status: acme.StatusPending, chall: acme.StatusPending,
		}
		o.authzs = append(o.authzs, a)
		f.authzs[a.id] = a
	}
	f.orders[o.id] = o
	if req.Replaces != "" {
		f.replacedBy[req.Replaces] = o
	}
	f.seen = append(f.seen, OrderSeen{Names: slices.Clone(names), Profile: req.Profile, Replaces: req.Replaces})
	q.w.Header().Set("Location", f.srv.URL+"/order/"+o.id)
	f.reply(q.w, http.StatusCreated, f.orderView(o))
}

// orderStatus derives the order's status from its authorizations and its finalization.
func (f *Fake) orderStatus(o *order) (string, *acme.ProblemDetails) {
	if o.cert != nil {
		return acme.StatusValid, nil
	}
	allValid := true
	for _, a := range o.authzs {
		switch a.status {
		case acme.StatusInvalid, acme.StatusDeactivated:
			pd := &acme.ProblemDetails{Type: problemNS + "unauthorized", Detail: "authorization for " + a.name + " is " + a.status, HTTPStatus: http.StatusForbidden}
			if a.problem != nil {
				pd = f.problemDetails(*a.problem)
			}
			return acme.StatusInvalid, pd
		case acme.StatusValid:
		default:
			allValid = false
		}
	}
	switch {
	case o.finalizing:
		return acme.StatusProcessing, nil
	case allValid:
		return acme.StatusReady, nil
	}
	return acme.StatusPending, nil
}

func (f *Fake) problemDetails(p Problem) *acme.ProblemDetails {
	if p.Status == 0 {
		p.Status = http.StatusBadRequest
	}
	if !strings.Contains(p.Type, ":") {
		p.Type = problemNS + p.Type
	}
	return &acme.ProblemDetails{Type: p.Type, Detail: p.Detail, HTTPStatus: p.Status}
}

func (f *Fake) orderView(o *order) acme.Order {
	status, pd := f.orderStatus(o)
	v := acme.Order{
		Status:   status,
		Expires:  o.expires.UTC().Format(time.RFC3339),
		Profile:  o.profile,
		Error:    pd,
		Finalize: f.srv.URL + "/finalize/" + o.id,
		Replaces: o.replaces,
	}
	for _, n := range o.names {
		v.Identifiers = append(v.Identifiers, acme.Identifier{Type: "dns", Value: n})
	}
	for _, a := range o.authzs {
		v.Authorizations = append(v.Authorizations, f.srv.URL+"/authz/"+a.id)
	}
	if o.cert != nil {
		v.Certificate = f.srv.URL + "/cert/" + o.cert.id
	}
	return v
}

func (f *Fake) challengeView(a *authz) acme.Challenge {
	c := acme.Challenge{
		Type: "http-01", URL: f.srv.URL + "/chall/" + a.id, Status: a.chall, Token: a.token,
		Validated: a.validated,
	}
	if a.problem != nil && a.chall == acme.StatusInvalid {
		c.Error = f.problemDetails(*a.problem)
	}
	return c
}

func (f *Fake) authzView(a *authz) acme.Authorization {
	return acme.Authorization{
		Status:     a.status,
		Expires:    a.order.expires,
		Identifier: acme.Identifier{Type: "dns", Value: a.name},
		Challenges: []acme.Challenge{f.challengeView(a)},
	}
}

// ownedOrder finds the order named by the request path and checks it belongs to the account.
func (f *Fake) ownedOrder(q *request) (*order, bool) {
	o, ok := f.orders[q.r.PathValue("id")]
	if !ok {
		f.problem(q.w, Problem{Status: http.StatusNotFound, Type: "malformed", Detail: "no such order"})
		return nil, false
	}
	if o.owner != q.acct.thumb {
		f.problem(q.w, Problem{Status: http.StatusForbidden, Type: "unauthorized", Detail: "the order belongs to another account"})
		return nil, false
	}
	return o, true
}

// ownedAuthz finds the authorization named by the request path and checks it belongs to the account.
func (f *Fake) ownedAuthz(q *request) (*authz, bool) {
	a, ok := f.authzs[q.r.PathValue("id")]
	if !ok {
		f.problem(q.w, Problem{Status: http.StatusNotFound, Type: "malformed", Detail: "no such authorization"})
		return nil, false
	}
	if a.order.owner != q.acct.thumb {
		f.problem(q.w, Problem{Status: http.StatusForbidden, Type: "unauthorized", Detail: "the authorization belongs to another account"})
		return nil, false
	}
	return a, true
}

func (f *Fake) getOrder(q *request) {
	o, ok := f.ownedOrder(q)
	if !ok {
		return
	}
	f.reply(q.w, http.StatusOK, f.orderView(o))
}

func (f *Fake) getAuthz(q *request) {
	a, ok := f.ownedAuthz(q)
	if !ok {
		return
	}
	if len(q.payload) > 0 {
		var req acme.Authorization
		if err := json.Unmarshal(q.payload, &req); err == nil && req.Status == acme.StatusDeactivated {
			a.status = acme.StatusDeactivated
		}
	}
	f.reply(q.w, http.StatusOK, f.authzView(a))
}

// check is what a validation needs, taken while mu is held so the GET can run without it.
type check struct {
	a        *authz
	host     string
	token    string
	thumb    string
	scripted *Problem // set by prepare: the failure scripted for the name
}

func (f *Fake) snapshot(a *authz, thumb string) check {
	return check{a: a, host: a.name, token: a.token, thumb: thumb}
}

// prepare settles what a validation will do, at the moment it runs: a failure scripted for the name
// wins, and a GET about to be made is counted. The caller holds mu.
func (f *Fake) prepare(c *check) {
	if p, ok := f.failVal[c.host]; ok {
		c.scripted = &p
		return
	}
	if f.opts.Validate != nil {
		f.hits[c.token]++
	}
}

// attempt performs the http-01 validation (RFC 8555 section 8.3): a GET of the challenge path over
// plain HTTP at the address the test maps the name to, whose body must be the key authorization. It
// returns nil when the challenge holds. mu is not held.
func (f *Fake) attempt(c check) *Problem {
	if c.scripted != nil {
		return c.scripted
	}
	if f.opts.Validate == nil {
		return nil
	}
	addr := f.opts.Validate(c.host)
	if addr == "" {
		return &Problem{Type: "connection", Detail: "no address to validate " + c.host + " at"}
	}
	url := "http://" + addr + "/.well-known/acme-challenge/" + c.token
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return &Problem{Type: "connection", Detail: "building the request for " + url + ": " + err.Error()}
	}
	resp, err := f.http.Do(req) // the client has a fixed timeout
	if err != nil {
		return &Problem{Type: "connection", Detail: "fetching " + url + ": " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return &Problem{Type: "connection", Detail: "reading " + url + ": " + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return &Problem{Status: http.StatusForbidden, Type: "unauthorized", Detail: fmt.Sprintf("fetching %s: status %d", url, resp.StatusCode)}
	}
	if want := c.token + "." + c.thumb; strings.TrimSpace(string(body)) != want {
		return &Problem{Type: "incorrectResponse", Detail: fmt.Sprintf("the response from %s was not the key authorization", url)}
	}
	return nil
}

// apply records the outcome of a validation. The caller holds mu.
func (f *Fake) apply(a *authz, p *Problem) {
	if p != nil {
		a.chall, a.status, a.problem = acme.StatusInvalid, acme.StatusInvalid, p
		return
	}
	a.chall, a.status, a.validated = acme.StatusValid, acme.StatusValid, f.clock.Now()
}

func (f *Fake) postChallenge(q *request) {
	a, ok := f.ownedAuthz(q)
	if !ok {
		return
	}
	if a.status != acme.StatusPending || a.chall != acme.StatusPending {
		f.problem(q.w, Problem{Type: "malformed", Detail: "the challenge is not pending"})
		return
	}
	a.chall = acme.StatusProcessing
	chk := f.snapshot(a, q.acct.thumb)
	q.w.Header().Add("Link", "<"+f.srv.URL+"/authz/"+a.id+`>;rel="up"`)
	if f.holdVal {
		f.heldVal = append(f.heldVal, chk)
		f.reply(q.w, http.StatusOK, f.challengeView(a))
		return
	}
	f.prepare(&chk)
	// The GET is to the test's own server, never back to the fake, but it is I/O: mu is not held.
	f.mu.Unlock()
	prob := f.attempt(chk)
	f.mu.Lock()
	f.apply(a, prob)
	f.reply(q.w, http.StatusOK, f.challengeView(a))
}

func (f *Fake) finalize(q *request) {
	o, ok := f.ownedOrder(q)
	if !ok {
		return
	}
	if status, _ := f.orderStatus(o); status != acme.StatusReady {
		f.problem(q.w, Problem{Status: http.StatusForbidden, Type: "orderNotReady", Detail: "the order is " + status + ", not ready"})
		return
	}
	var req acme.CSRMessage
	if err := json.Unmarshal(q.payload, &req); err != nil {
		f.problem(q.w, Problem{Type: "malformed", Detail: "finalize payload: " + err.Error()})
		return
	}
	der, err := base64.RawURLEncoding.DecodeString(req.Csr)
	if err != nil {
		f.problem(q.w, Problem{Type: "badCSR", Detail: "the CSR is not base64url: " + err.Error()})
		return
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err == nil {
		err = csr.CheckSignature()
	}
	if err != nil {
		f.problem(q.w, Problem{Type: "badCSR", Detail: "the CSR does not parse: " + err.Error()})
		return
	}
	got := slices.Clone(csr.DNSNames)
	for i := range got {
		got[i] = strings.ToLower(got[i])
	}
	want := slices.Clone(o.names)
	slices.Sort(got)
	slices.Sort(want)
	if len(csr.IPAddresses) > 0 || len(csr.EmailAddresses) > 0 || !slices.Equal(got, want) {
		f.problem(q.w, Problem{Type: "badCSR", Detail: fmt.Sprintf("the CSR names %v, the order names %v", got, want)})
		return
	}
	if f.holdFin {
		o.finalizing, o.csr = true, csr
		f.heldFin = append(f.heldFin, o)
		f.reply(q.w, http.StatusOK, f.orderView(o))
		return
	}
	if err := f.issue(o, csr); err != nil {
		f.problem(q.w, Problem{Status: http.StatusInternalServerError, Type: "serverInternal", Detail: err.Error()})
		return
	}
	f.reply(q.w, http.StatusOK, f.orderView(o))
}

// issue signs the order's certificate with times from the clock and the profile's lifetime, and
// closes the order. The caller holds mu.
func (f *Fake) issue(o *order, csr *x509.CertificateRequest) error {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return err
	}
	serial.Add(serial, big.NewInt(1<<40))
	now := f.clock.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		NotBefore:             now,
		NotAfter:              now.Add(f.opts.Profiles[o.profile]),
		DNSNames:              o.names,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.caCert, csr.PublicKey, f.caKey)
	if err != nil {
		return fmt.Errorf("sign the certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("parse the certificate: %w", err)
	}
	ari, err := api.MakeARICertID(leaf)
	if err != nil {
		return err
	}
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.caCert.Raw})...)
	c := &issued{id: f.nextID(), ari: ari, owner: o.owner, leaf: leaf, chain: chain, order: o}
	f.certs[c.id] = c
	f.byARI[ari] = c
	o.cert, o.finalizing = c, false
	return nil
}

func (f *Fake) getCert(q *request) {
	c, ok := f.certs[q.r.PathValue("id")]
	if !ok {
		f.problem(q.w, Problem{Status: http.StatusNotFound, Type: "malformed", Detail: "no such certificate"})
		return
	}
	if c.owner != q.acct.thumb {
		f.problem(q.w, Problem{Status: http.StatusForbidden, Type: "unauthorized", Detail: "the certificate belongs to another account"})
		return
	}
	q.w.Header().Set("Content-Type", "application/pem-certificate-chain")
	q.w.WriteHeader(http.StatusOK)
	_, _ = q.w.Write(c.chain)
}

// renewalInfo answers RFC 9773 renewal information. By default the suggested window is two thirds
// of the lifetime, a day either side, and the client is told to ask again in six hours; a window set
// with SetRenewalWindow replaces both. Retry-After is always delta-seconds.
func (f *Fake) renewalInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, known := f.byARI[id]
	script, scripted := f.windows[id]
	if f.opts.NoRenewalInfo || (!known && !scripted) {
		f.problem(w, Problem{Status: http.StatusNotFound, Type: "malformed", Detail: "no renewal information for that certificate"})
		return
	}
	start, end, retry := script.start, script.end, script.retryAfter
	if !scripted {
		life := c.leaf.NotAfter.Sub(c.leaf.NotBefore)
		mid := c.leaf.NotBefore.Add(life * 2 / 3)
		start, end, retry = mid.Add(-24*time.Hour), mid.Add(24*time.Hour), 6*time.Hour
	}
	if secs := int(retry / time.Second); secs > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	f.reply(w, http.StatusOK, map[string]any{"suggestedWindow": map[string]any{"start": start, "end": end}})
}

// The scripting and observation surface. Every method is safe to call while the fake serves.

// FailNext makes the next call that reaches endpoint (one of the Endpoint constants) answer with p,
// once. Calls queue: a second FailNext answers the call after that.
func (f *Fake) FailNext(endpoint string, p Problem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripted[endpoint] = append(f.scripted[endpoint], p)
}

// BadNonceOnce makes the next signed request, whatever its endpoint, answer badNonce. A client that
// retries (lego does) then succeeds. The refused request is not counted.
func (f *Fake) BadNonceOnce() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.badNonce = true
}

// FailValidation makes the validation of host fail with p: the challenge and its authorization go
// invalid and the challenge carries the problem. No GET is made for a scripted failure.
func (f *Fake) FailValidation(host string, p Problem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failVal[strings.ToLower(host)] = p
}

// HoldValidation leaves every challenge submitted from now on in processing, with no GET made, until
// the returned function is called. The release validates the held challenges. Nothing waits on a
// timer: the test decides when validation happens.
func (f *Fake) HoldValidation() (release func()) {
	f.mu.Lock()
	f.holdVal = true
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.holdVal = false
		held := f.heldVal
		f.heldVal = nil
		for i := range held {
			f.prepare(&held[i])
		}
		f.mu.Unlock()
		for _, c := range held {
			prob := f.attempt(c)
			f.mu.Lock()
			f.apply(c.a, prob)
			f.mu.Unlock()
		}
	}
}

// HoldFinalize leaves every order finalized from now on in processing, without a certificate, until
// the returned function is called. The release issues the held certificates.
func (f *Fake) HoldFinalize() (release func()) {
	f.mu.Lock()
	f.holdFin = true
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.holdFin = false
		held := f.heldFin
		f.heldFin = nil
		for _, o := range held {
			if err := f.issue(o, o.csr); err != nil {
				f.tb.Errorf("acmetest: issue a held order: %v", err)
			}
		}
	}
}

// SetRenewalWindow sets the renewalInfo answer for leaf: the suggested window and the delta-seconds
// Retry-After (none when retryAfter is under a second).
func (f *Fake) SetRenewalWindow(leaf *x509.Certificate, start, end time.Time, retryAfter time.Duration) {
	f.tb.Helper()
	id, err := api.MakeARICertID(leaf)
	if err != nil {
		f.tb.Fatalf("acmetest: renewal id of the certificate: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.windows[id] = renewalWindow{start: start, end: end, retryAfter: retryAfter}
}

// ForgetAccounts makes every account unknown: a request naming one answers accountDoesNotExist, and
// registering the same key again creates a new account at a new URL. Certificates already issued
// stay issued.
func (f *Fake) ForgetAccounts() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.accounts)
}

// Count is the number of calls that reached endpoint, including the ones a scripted failure
// refused. A signed request refused earlier (badNonce, a bad signature) is not counted.
func (f *Fake) Count(endpoint string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[endpoint]
}

// Orders lists the newOrder requests the fake accepted, in order. Profile is the one the request
// named (empty when it named none).
func (f *Fake) Orders() []OrderSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.seen)
	for i := range out {
		out[i].Names = slices.Clone(out[i].Names)
	}
	return out
}

// AccountsSeen lists the newAccount requests that created an account, in order, with the contact and
// terms agreement each carried.
func (f *Fake) AccountsSeen() []AccountSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.accountsIn)
}

// ValidationHits is the number of http-01 GETs the fake made for token.
func (f *Fake) ValidationHits(token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[token]
}
