// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/config"
)

// orderEnv is everything an order test needs: a fake CA whose validation GETs reach a real HTTP-01
// token handler, an account registered with it, and a clock the test moves.
type orderEnv struct {
	t      *testing.T
	fake   *acmetest.Fake
	client *Client
	store  *Store
	clock  *acmetest.Clock
	tokens *Challenges
	hits   *atomic.Int32 // validation GETs that reached the token handler
}

func newOrderEnv(t *testing.T) *orderEnv {
	t.Helper()
	tokens := NewChallenges()
	var hits atomic.Int32
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		tokens.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(web.Close)
	addr := web.Listener.Addr().String()

	clock := acmetest.NewClock(testNow)
	fake := acmetest.NewFake(t, acmetest.Options{Clock: clock, Validate: func(string) string { return addr }})
	dir, err := NewDirectory(config.Config{ACMEDirectoryURL: fake.DirectoryURL(), ACMECAFile: fake.CAFile(t)})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	s, _, _ := newTestStore(t)
	client := NewClient(dir, s, clock, "tierplan-test/0")
	if _, err := client.Register(context.Background(), "", true); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return &orderEnv{t: t, fake: fake, client: client, store: s, clock: clock, tokens: tokens, hits: &hits}
}

// orderer is a new Orderer over the environment's state: what a restarted process would build.
func (e *orderEnv) orderer() *Orderer {
	return NewOrderer(e.client, e.store, e.tokens, e.clock)
}

// stepLog records the steps of a run as "step:event" and can react to one of them.
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

func (l *stepLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// cancelOn is a step log that cancels a run's context when the step starts.
func cancelOn(cancel context.CancelFunc, step string) *stepLog {
	return &stepLog{on: func(s, event string) {
		if s == step && event == "running" {
			cancel()
		}
	}}
}

// leafOf is the first certificate of a PEM chain.
func leafOf(t *testing.T, chainPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(chainPEM)
	if block == nil {
		t.Fatal("the chain holds no PEM block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse the leaf: %v", err)
	}
	return leaf
}

func TestOrderIssuesThroughHTTP01(t *testing.T) {
	ctx := context.Background()
	e := newOrderEnv(t)
	names := []string{"tierplan.example.com"}
	steps := &stepLog{}

	issued, err := e.orderer().Run(ctx, OrderRequest{Kind: KindIssue, Names: names, Profile: "classic"}, steps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !slices.Equal(issued.Leaf.DNSNames, names) {
		t.Errorf("leaf names = %v, want %v", issued.Leaf.DNSNames, names)
	}
	if !issued.Leaf.NotAfter.After(testNow) {
		t.Errorf("leaf expires %v, not after %v", issued.Leaf.NotAfter, testNow)
	}
	if pub, ok := issued.Leaf.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(issued.Key.Public()) {
		t.Error("the leaf is not for the returned key")
	}
	if issued.Kind != KindIssue || issued.OrderID == "" {
		t.Errorf("Issued = kind %q, order %q", issued.Kind, issued.OrderID)
	}

	// The certificate is in the acme row, and its key is the order's key.
	chain, key, found, err := e.store.Certificate(ctx)
	if err != nil || !found {
		t.Fatalf("Certificate = found %v, %v; want it stored", found, err)
	}
	if !key.Equal(issued.Key) || !leafOf(t, chain).Equal(issued.Leaf) {
		t.Error("the stored certificate or key is not the issued one")
	}
	if _, _, open, err := e.store.OpenOrder(ctx); err != nil || open {
		t.Errorf("OpenOrder = open %v, %v; want the order closed", open, err)
	}

	want := []string{
		"account:skipped",
		"order:running", "order:done",
		"challenge:running", "challenge:done",
		"validate:running", "validate:done",
		"finalize:running", "finalize:done",
		"stored:running", "stored:done",
	}
	if got := steps.all(); !slices.Equal(got, want) {
		t.Errorf("steps = %v\nwant    %v", got, want)
	}

	if n := e.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("newOrder requests = %d, want 1", n)
	}
	seen := e.fake.Orders()
	if len(seen) != 1 || seen[0].Profile != "classic" || !slices.Equal(seen[0].Names, names) {
		t.Errorf("the CA saw orders %+v, want one for %v with profile classic", seen, names)
	}
	if e.hits.Load() < 1 {
		t.Error("the CA never fetched the challenge over HTTP")
	}
	if n := e.tokens.Len(); n != 0 {
		t.Errorf("%d tokens are still answerable after the run, want 0", n)
	}
}

func TestOrderRefusesAMalformedRequestAndAMissingAccount(t *testing.T) {
	ctx := context.Background()
	e := newOrderEnv(t)

	for _, req := range []OrderRequest{
		{Kind: "", Names: []string{"a.example.com"}},
		{Kind: "bogus", Names: []string{"a.example.com"}},
		{Kind: KindIssue},
	} {
		if _, err := e.orderer().Run(ctx, req, nil); err == nil {
			t.Errorf("Run(%+v) succeeded, want a refusal", req)
		}
	}
	if n := e.fake.Count(acmetest.EndpointNewOrder); n != 0 {
		t.Errorf("newOrder requests = %d, want 0 for a refused request", n)
	}

	if err := e.client.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	steps := &stepLog{}
	_, err := e.orderer().Run(ctx, OrderRequest{Kind: KindIssue, Names: []string{"a.example.com"}}, steps)
	if err == nil || !strings.Contains(err.Error(), ErrNoAccount.Error()) {
		t.Errorf("Run without an account = %v, want ErrNoAccount", err)
	}
	if got := steps.all(); !slices.Equal(got, []string{"account:failed"}) {
		t.Errorf("steps = %v, want just the account failing", got)
	}
}
