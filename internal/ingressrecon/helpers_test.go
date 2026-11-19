// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrecon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// fixedNow is where every test clock and store stands.
var fixedNow = time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)

// rig is a real ingress store over SQLite, a vault, the notifier the stores tell and a clock a test moves. It
// holds one environment, env-1, with the domain example.com registered in it.
type rig struct {
	t        *testing.T
	st       *store.Store
	v        *vault.Vault
	model    *ingressstore.Store
	notifier *ingress.Notifier
	clock    *acmetest.Clock
	certs    *fakeCerts
}

const rigEnv = "env-1"

func newRig(t *testing.T) *rig {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	n := ingress.NewNotifier()
	r := &rig{
		t: t, st: st, v: v, notifier: n, clock: acmetest.NewClock(fixedNow),
		model: ingressstore.New(ingressstore.Deps{St: st, Vault: v, Now: func() time.Time { return fixedNow }, Notifier: n}),
	}
	r.certs = &fakeCerts{st: st, servable: map[string]bool{}, reasons: map[string]string{}}
	r.seedEnv(rigEnv, "One")
	if _, err := r.model.CreateDomain(context.Background(),
		ingressstore.CreateDomainInput{EnvID: rigEnv, Suffix: "example.com", Issuer: ingress.IssuerStaging, Actor: "admin"}, nil); err != nil {
		t.Fatalf("register example.com: %v", err)
	}
	return r
}

// seedEnv inserts a standalone environment.
func (r *rig) seedEnv(id, name string) {
	r.t.Helper()
	if _, err := r.st.Write.Exec(
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type) VALUES (?, 'agent', ?, '', 1, 1, 1, 'standalone')`,
		id, name); err != nil {
		r.t.Fatalf("seed environment %s: %v", id, err)
	}
}

// seedSwarmStack inserts a swarm stack in env-1.
func (r *rig) seedSwarmStack(id, name string) {
	r.t.Helper()
	if _, err := r.st.Write.Exec(
		`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES (?, ?, ?, 'swarm', 'tester', 1)`,
		id, rigEnv, name); err != nil {
		r.t.Fatalf("seed swarm stack %s: %v", id, err)
	}
}

// storeRoutes stores a stack's declarations through the policy, which notifies the environment.
func (r *rig) storeRoutes(stackID string, decls ...ingress.Declaration) {
	r.t.Helper()
	if _, err := r.model.ReplaceStackRoutes(context.Background(),
		ingressstore.ReplaceInput{EnvID: rigEnv, StackID: stackID, Actor: "alice", Decls: decls}); err != nil {
		r.t.Fatalf("store the routes of %s: %v", stackID, err)
	}
}

// decl is a route to the service web on port 8080.
func decl(host, path string) ingress.Declaration {
	return ingress.Declaration{Service: "web", Host: host, Label: "tierplan.ingress.host", PathPrefix: path, Port: 8080}
}

// deps returns the reconciler's dependencies over the rig, with the fake certificate source and onRender, a test's
// seam, which may be nil.
func (r *rig) deps(onRender func(envID, id string, changed bool)) Deps {
	return Deps{
		Model: r.model, Certs: r.certs, Notifier: r.notifier, Clock: r.clock,
		Log: slog.New(slog.DiscardHandler), OnRender: onRender,
	}
}

// fakeCerts is a Certs whose servable hostnames a test sets. The version it returns is the environment's
// certs_version read from the same database, as the real Serving does.
type fakeCerts struct {
	st *store.Store

	mu       sync.Mutex
	servable map[string]bool
	reasons  map[string]string
}

// setServable makes host servable.
func (c *fakeCerts) setServable(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.servable[host] = true
}

// setReason makes host unservable for the reason.
func (c *fakeCerts) setReason(host, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reasons[host] = reason
}

func (c *fakeCerts) Serving(_ context.Context, envID string, hostnames []string, _ time.Time) (int64, []ingresscert.ServingCert, error) {
	var version int64
	if err := c.st.Read.QueryRow(`SELECT COALESCE((SELECT certs_version FROM ingress_env WHERE env_id = ?), 0)`, envID).Scan(&version); err != nil {
		return 0, nil, err
	}
	hosts := slices.Clone(hostnames)
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ingresscert.ServingCert, 0, len(hosts))
	for _, h := range hosts {
		if c.servable[h] {
			out = append(out, ingresscert.ServingCert{
				Hostname: h, ChainPEM: []byte("-----BEGIN CERTIFICATE-----\nchain of " + h + "\n-----END CERTIFICATE-----\n"),
				KeyPEM: []byte("-----BEGIN PRIVATE KEY-----\nkey of " + h + "\n-----END PRIVATE KEY-----\n"), Source: ingresscert.SourceACME,
			})
			continue
		}
		reason := c.reasons[h]
		if reason == "" {
			reason = ingresscert.ReasonNoCertificate
		}
		out = append(out, ingresscert.ServingCert{Hostname: h, Reason: reason})
	}
	return version, out, nil
}

// insertCert stores a certificate row of the source and issuer for host, valid from nb to na, with a
// self-signed chain and a PKCS#8 key sealed under the row's id, as the certificate store does.
func insertCert(t *testing.T, st *store.Store, v *vault.Vault, id, host, envID, domainID, source, issuer string, nb, na time.Time) {
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
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	sealed, err := v.Seal(vault.ContextIngressCertKey+id, keyPEM)
	if err != nil {
		t.Fatalf("seal the key of %s: %v", id, err)
	}
	if _, err := st.Write.Exec(
		`INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, issuer, chain_pem, key_sealed, not_before, not_after, issued_at, phase, ready, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'idle', 1, ?, ?)`,
		id, host, envID, domainID, source, issuer, string(chain), sealed, nb.UnixNano(), na.UnixNano(), nb.UnixNano(), nb.UnixNano(), nb.UnixNano()); err != nil {
		t.Fatalf("insert the certificate of %s: %v", host, err)
	}
	if _, err := st.Write.Exec(`UPDATE ingress_env SET certs_version = certs_version + 1 WHERE env_id = ?`, envID); err != nil {
		t.Fatalf("raise the certificate version of %s: %v", envID, err)
	}
}

// domainID is the id of env-1's registered domain example.com.
func (r *rig) domainID() string {
	r.t.Helper()
	var id string
	if err := r.st.Read.QueryRow(`SELECT id FROM ingress_domains WHERE env_id = ? AND suffix = 'example.com'`, rigEnv).Scan(&id); err != nil {
		r.t.Fatalf("read the domain id: %v", err)
	}
	return id
}

// routerRules returns the rule of every router in the bundle's dynamic file, sorted.
func routerRules(t *testing.T, b ingressbundle.Bundle) []string {
	t.Helper()
	for _, f := range b.Files {
		if f.Path != ingressbundle.DynamicFile {
			continue
		}
		var doc struct {
			HTTP struct {
				Routers map[string]struct {
					Rule string `yaml:"rule"`
				} `yaml:"routers"`
			} `yaml:"http"`
		}
		if err := yaml.Unmarshal(f.Data, &doc); err != nil {
			t.Fatalf("the dynamic file does not parse: %v", err)
		}
		rules := make([]string, 0, len(doc.HTTP.Routers))
		for _, r := range doc.HTTP.Routers {
			rules = append(rules, r.Rule)
		}
		slices.Sort(rules)
		return rules
	}
	t.Fatalf("the bundle has no %s", ingressbundle.DynamicFile)
	return nil
}

// hasRouterFor reports whether a router's rule names host.
func hasRouterFor(rules []string, host string) bool {
	return slices.ContainsFunc(rules, func(r string) bool { return strings.Contains(r, host) })
}

// started is a running reconciler: its Run goroutine, the OnRender events and a subscription to Changes.
type started struct {
	r       *Reconciler
	renders chan renderEvent
	changes *ingress.Subscription
}

// renderEvent is one OnRender call.
type renderEvent struct {
	envID, id string
	changed   bool
}

// start builds a reconciler over the rig and runs it until the test ends.
func (rg *rig) start() *started {
	t := rg.t
	t.Helper()
	renders := make(chan renderEvent, 64)
	r := New(rg.deps(func(envID, id string, changed bool) { renders <- renderEvent{envID, id, changed} }))
	s := &started{r: r, renders: renders, changes: r.Changes().Subscribe()}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		testsupport.Await(t, "Run to return", done)
		s.changes.Close()
	})
	return s
}

// awaitRender waits for the next render and returns it.
func (s *started) awaitRender(t *testing.T) renderEvent {
	t.Helper()
	return testsupport.Await(t, "the reconciler to render", s.renders)
}

// awaitChange waits for the Changes wake and returns the environments it names.
func (s *started) awaitChange(t *testing.T) []string {
	t.Helper()
	testsupport.Await(t, "the reconciler to announce a new bundle", s.changes.C())
	return s.changes.Pending()
}
