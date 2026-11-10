// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	legoacme "github.com/go-acme/lego/v5/acme"
	"github.com/go-acme/lego/v5/acme/api"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/proto/testsupport"
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
	return newOrderEnvWith(t, acmetest.Options{})
}

// newOrderEnvWith is newOrderEnv over a fake CA with the given behaviour (its clock and validation are
// the environment's own).
func newOrderEnvWith(t *testing.T, opts acmetest.Options) *orderEnv {
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
	opts.Clock, opts.Validate = clock, func(string) string { return addr }
	fake := acmetest.NewFake(t, opts)
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

// runResult is how an asynchronous Run ended.
type runResult struct {
	issued Issued
	err    error
}

// runAsync starts Run on its own goroutine and returns the channel its result arrives on.
func (e *orderEnv) runAsync(ctx context.Context, req OrderRequest, steps StepReporter) <-chan runResult {
	done := make(chan runResult, 1)
	go func() {
		issued, err := e.orderer().Run(ctx, req, steps)
		done <- runResult{issued, err}
	}()
	return done
}

// crashHeld runs req against a CA that keeps the challenge processing, and stops the run as soon as
// it starts waiting for validation: the order is left open at the CA and in the store, as a restart
// at that moment would leave it. The returned function lets the CA validate what it holds.
func (e *orderEnv) crashHeld(req OrderRequest) (release func()) {
	e.t.Helper()
	release = e.fake.HoldValidation()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := e.orderer().Run(ctx, req, cancelOn(cancel, StepValidate)); !errors.Is(err, context.Canceled) {
		e.t.Fatalf("the interrupted run ended with %v, want context.Canceled", err)
	}
	return release
}

// openOrder is the open order and its key; the test fails if there is none.
func (e *orderEnv) openOrder() (Order, *ecdsa.PrivateKey) {
	e.t.Helper()
	o, key, found, err := e.store.OpenOrder(context.Background())
	if err != nil || !found {
		e.t.Fatalf("OpenOrder = found %v, %v; want an open order", found, err)
	}
	return o, key
}

// noOpenOrder fails the test if an order is still open.
func (e *orderEnv) noOpenOrder() {
	e.t.Helper()
	if o, _, found, err := e.store.OpenOrder(context.Background()); err != nil || found {
		e.t.Fatalf("OpenOrder = %+v, found %v, %v; want no open order", o, found, err)
	}
}

// stages lists the stage of every order the store holds, oldest first.
func (e *orderEnv) stages() []string {
	e.t.Helper()
	rows, err := e.store.st.Read.QueryContext(context.Background(), `SELECT stage FROM acme_orders ORDER BY created_at, id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var stage string
		if err := rows.Scan(&stage); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, stage)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// requireStoredKey checks that the stored certificate was issued for key, the one the order kept
// before the interruption, and that the stored private key is that key.
func (e *orderEnv) requireStoredKey(key *ecdsa.PrivateKey) {
	e.t.Helper()
	chain, stored, found, err := e.store.Certificate(context.Background())
	if err != nil || !found {
		e.t.Fatalf("Certificate = found %v, %v; want it stored", found, err)
	}
	pub, ok := leafOf(e.t, chain).PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		e.t.Error("the stored certificate is not for the key the order kept before the interruption")
	}
	if !stored.Equal(key) {
		e.t.Error("the stored private key is not the key the order kept before the interruption")
	}
}

var orderReq = OrderRequest{Kind: KindIssue, Names: []string{"tierplan.example.com"}, Profile: "classic"}

func TestOrderResumesAfterACrashWithoutANewOrder(t *testing.T) {
	ctx := context.Background()
	e := newOrderEnv(t)
	release := e.crashHeld(orderReq)

	open, key := e.openOrder()
	if open.Stage != StageChallengeSubmitted || open.ResumeCount != 0 {
		t.Errorf("after the crash the order is %+v, want stage %s and no resumes", open, StageChallengeSubmitted)
	}
	if n := e.tokens.Len(); n != 0 {
		t.Errorf("%d tokens survive the stopped run, want 0", n)
	}

	// A new Orderer, as after a restart: it must find the order, answer the challenge again without
	// submitting it, and wait for the CA.
	steps := &stepLog{}
	done := e.runAsync(ctx, orderReq, steps)
	e.clock.WaitArmed(t) // the run has read the order, presented the token and is waiting on the clock
	release()
	e.clock.Advance(PollInterval)
	res := testsupport.Await(t, "the resumed run to finish", done)
	if res.err != nil {
		t.Fatalf("the resumed run: %v", res.err)
	}

	if n := e.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("newOrder requests = %d, want 1: a resume must not open a new order", n)
	}
	if n := e.fake.Count(acmetest.EndpointChallenge); n != 1 {
		t.Errorf("challenge submissions = %d, want 1: a processing challenge is never submitted again", n)
	}
	if n := e.fake.Count(acmetest.EndpointFinalize); n != 1 {
		t.Errorf("finalize requests = %d, want 1", n)
	}
	e.requireStoredKey(key)
	if !res.issued.Key.Equal(key) {
		t.Error("the resumed run issued for another key")
	}
	if got := steps.all(); !slices.Contains(got, "stored:done") {
		t.Errorf("steps = %v, want the run to end stored", got)
	}
	if n := e.tokens.Len(); n != 0 {
		t.Errorf("%d tokens are still answerable after the run, want 0", n)
	}
}

func TestOrderResumesFromReadyAndFinalizesOnce(t *testing.T) {
	ctx := context.Background()
	e := newOrderEnv(t)

	// Validation succeeds at once, so the order is ready when finalizing starts; the run is stopped
	// right there, before the request leaves.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if _, err := e.orderer().Run(cctx, orderReq, cancelOn(cancel, StepFinalize)); !errors.Is(err, context.Canceled) {
		t.Fatalf("the interrupted run ended with %v, want context.Canceled", err)
	}
	if n := e.fake.Count(acmetest.EndpointFinalize); n != 0 {
		t.Fatalf("finalize requests = %d, want 0 before the resume", n)
	}
	open, key := e.openOrder()
	if open.Stage != StageReady {
		t.Errorf("after the interruption the order is at stage %q, want %q", open.Stage, StageReady)
	}

	res := <-e.runAsync(ctx, orderReq, nil)
	if res.err != nil {
		t.Fatalf("the resumed run: %v", res.err)
	}
	if n := e.fake.Count(acmetest.EndpointFinalize); n != 1 {
		t.Errorf("finalize requests = %d, want 1", n)
	}
	if n := e.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("newOrder requests = %d, want 1", n)
	}
	if n := e.fake.Count(acmetest.EndpointChallenge); n != 1 {
		t.Errorf("challenge submissions = %d, want 1", n)
	}
	e.requireStoredKey(key)
}

func TestOrderResumeOfAProcessingOrderOnlyPolls(t *testing.T) {
	ctx := context.Background()
	e := newOrderEnv(t)
	release := e.fake.HoldFinalize()

	// The first run sends the CSR, sees the CA processing it, and is stopped while it polls.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	first := e.runAsync(cctx, orderReq, nil)
	e.clock.WaitArmed(t)
	cancel()
	if res := testsupport.Await(t, "the interrupted run to stop", first); !errors.Is(res.err, context.Canceled) {
		t.Fatalf("the interrupted run ended with %v, want context.Canceled", res.err)
	}
	if n := e.fake.Count(acmetest.EndpointFinalize); n != 1 {
		t.Fatalf("finalize requests = %d, want 1 before the resume", n)
	}
	open, key := e.openOrder()
	if open.Stage != StageFinalizing {
		t.Errorf("after the interruption the order is at stage %q, want %q", open.Stage, StageFinalizing)
	}

	second := e.runAsync(ctx, orderReq, nil)
	e.clock.WaitArmed(t) // the resumed run has seen the order processing and is polling
	release()
	e.clock.Advance(PollInterval)
	res := testsupport.Await(t, "the resumed run to finish", second)
	if res.err != nil {
		t.Fatalf("the resumed run: %v", res.err)
	}
	if n := e.fake.Count(acmetest.EndpointFinalize); n != 1 {
		t.Errorf("finalize requests = %d, want 1: a processing order is only polled", n)
	}
	if n := e.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("newOrder requests = %d, want 1", n)
	}
	if n := e.fake.Count(acmetest.EndpointChallenge); n != 1 {
		t.Errorf("challenge submissions = %d, want 1", n)
	}
	e.requireStoredKey(key)
}

func TestOrderFailedAuthorizationIsTerminal(t *testing.T) {
	ctx := context.Background()
	e := newOrderEnv(t)
	e.fake.FailValidation("tierplan.example.com", acmetest.Problem{Type: "connection", Detail: "timeout during connect"})

	steps := &stepLog{}
	_, err := e.orderer().Run(ctx, orderReq, steps)
	var ce *ChallengeError
	if !errors.As(err, &ce) || ce.Host != "tierplan.example.com" {
		t.Fatalf("Run = %v, want a ChallengeError for tierplan.example.com", err)
	}
	f := Classify(err, ClassifyInput{ChallengeType: ChallengeHTTP01, Profile: "classic"})
	if f.Class != ClassValidationUnreachable || f.Host != "tierplan.example.com" {
		t.Errorf("Classify = %+v, want validation_unreachable for tierplan.example.com", f)
	}

	e.noOpenOrder()
	if got := e.stages(); !slices.Equal(got, []string{StageFailed}) {
		t.Errorf("order stages = %v, want [%s]", got, StageFailed)
	}
	if got := steps.all(); got[len(got)-1] != "validate:failed" {
		t.Errorf("steps = %v, want the run to end with validate failing", got)
	}
	if n := e.fake.Count(acmetest.EndpointFinalize); n != 0 {
		t.Errorf("finalize requests = %d, want 0 for a failed authorization", n)
	}
	if n := e.tokens.Len(); n != 0 {
		t.Errorf("%d tokens are still answerable after the run, want 0", n)
	}
}

func TestOrderReplacesAStaleOpenOrder(t *testing.T) {
	tests := []struct {
		name    string
		second  OrderRequest
		advance time.Duration
	}{
		{"other names", OrderRequest{Kind: KindIssue, Names: []string{"other.example.com"}, Profile: "classic"}, 0},
		{"other kind", OrderRequest{Kind: KindRenew, Names: orderReq.Names, Profile: "classic"}, 0},
		{"expired", orderReq, 8 * 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newOrderEnv(t)
			release := e.crashHeld(orderReq)
			release() // the CA may validate what it holds; the new order must not wait on it
			e.clock.Advance(tc.advance)

			res := <-e.runAsync(ctx, tc.second, nil)
			if res.err != nil {
				t.Fatalf("Run: %v", res.err)
			}
			if n := e.fake.Count(acmetest.EndpointNewOrder); n != 2 {
				t.Errorf("newOrder requests = %d, want 2: the stale order is replaced", n)
			}
			if got := e.stages(); !slices.Equal(got, []string{StageFailed, StageStored}) {
				t.Errorf("order stages = %v, want the stale order closed as failed and the new one stored", got)
			}
			if !slices.Equal(res.issued.Leaf.DNSNames, tc.second.Names) || res.issued.Kind != tc.second.Kind {
				t.Errorf("issued %v as %q, want %v as %q", res.issued.Leaf.DNSNames, res.issued.Kind, tc.second.Names, tc.second.Kind)
			}
		})
	}
}

func TestOrderStopsAfterMaxResumes(t *testing.T) {
	ctx := context.Background()
	e := newOrderEnv(t)
	release := e.crashHeld(orderReq)
	defer release()

	for i := 1; i <= MaxResumes; i++ {
		cctx, cancel := context.WithCancel(ctx)
		_, err := e.orderer().Run(cctx, orderReq, cancelOn(cancel, StepValidate))
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("resume %d ended with %v, want context.Canceled", i, err)
		}
		if open, _ := e.openOrder(); open.ResumeCount != i {
			t.Fatalf("after resume %d the order records %d resumes", i, open.ResumeCount)
		}
	}

	_, err := e.orderer().Run(ctx, orderReq, nil)
	if !errors.Is(err, ErrTooManyResumes) {
		t.Fatalf("resume %d = %v, want ErrTooManyResumes", MaxResumes+1, err)
	}
	e.noOpenOrder()
	if got := e.stages(); !slices.Equal(got, []string{StageFailed}) {
		t.Errorf("order stages = %v, want [%s]", got, StageFailed)
	}
	if n := e.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("newOrder requests = %d, want 1: giving up on an order does not open another", n)
	}
	if n := e.fake.Count(acmetest.EndpointChallenge); n != 1 {
		t.Errorf("challenge submissions = %d, want 1", n)
	}
}

func TestOrderSendsProfileAndReplaces(t *testing.T) {
	ctx := context.Background()
	e := newOrderEnv(t)

	first, err := e.orderer().Run(ctx, orderReq, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	ariID, err := api.MakeARICertID(first.Leaf)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := e.orderer().Run(ctx, OrderRequest{Kind: KindRenew, Names: orderReq.Names, Profile: "shortlived", Replaces: ariID}, nil)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}

	seen := e.fake.Orders()
	if len(seen) != 2 {
		t.Fatalf("the CA saw %d orders, want 2", len(seen))
	}
	if seen[0].Profile != "classic" || seen[0].Replaces != "" {
		t.Errorf("first order = %+v, want profile classic and no replaces", seen[0])
	}
	if seen[1].Profile != "shortlived" || seen[1].Replaces != ariID {
		t.Errorf("renewal order = %+v, want profile shortlived and replaces %q", seen[1], ariID)
	}
	if life := renewed.Leaf.NotAfter.Sub(renewed.Leaf.NotBefore); life != 160*time.Hour {
		t.Errorf("the renewed certificate lives %v, want the shortlived profile's 160h", life)
	}
	if renewed.Kind != KindRenew {
		t.Errorf("Issued.Kind = %q, want %q", renewed.Kind, KindRenew)
	}

	// An order that names no profile sends none, and the CA picks its default.
	if _, err := e.orderer().Run(ctx, OrderRequest{Kind: KindIssue, Names: []string{"two.example.com"}}, nil); err != nil {
		t.Fatalf("default profile: %v", err)
	}
	if got := e.fake.Orders()[2].Profile; got != "" {
		t.Errorf("an order with no profile sent %q, want none", got)
	}
}

func TestOrderUnknownProfileIsClassified(t *testing.T) {
	ctx := context.Background()
	e := newOrderEnv(t)

	steps := &stepLog{}
	_, err := e.orderer().Run(ctx, OrderRequest{Kind: KindIssue, Names: orderReq.Names, Profile: "nope"}, steps)
	if err == nil {
		t.Fatal("Run with an unknown profile succeeded")
	}
	var pd *legoacme.ProblemDetails
	if !errors.As(err, &pd) {
		t.Errorf("Run = %v, want the CA's problem in the chain", err)
	}
	f := Classify(err, ClassifyInput{ChallengeType: ChallengeHTTP01, Profile: "nope"})
	if f.Class != ClassProfileInvalid || f.Profile != "nope" {
		t.Errorf("Classify = %+v, want profile_invalid for nope", f)
	}
	e.noOpenOrder()
	if got := e.stages(); len(got) != 0 {
		t.Errorf("order stages = %v, want no order stored for a refused request", got)
	}
	if got, want := steps.all(), []string{"account:skipped", "order:running", "order:failed"}; !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

// A renewal that names a certificate issued to another account (the CA forgot the account that was
// issued it, and a new one was registered) is refused as unauthorized. The order is placed once more
// without naming it, the run succeeds, and the stored certificate is recorded as the new account's.
// Only one more order is tried: a second refusal is the run's failure.
func TestOrderDropsAReplacesTheCARefusesAsAnotherAccounts(t *testing.T) {
	ctx := context.Background()
	e := newOrderEnv(t)
	first, err := e.orderer().Run(ctx, orderReq, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	ariID, err := api.MakeARICertID(first.Leaf)
	if err != nil {
		t.Fatal(err)
	}
	accountA, _, err := e.client.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.store.CertAccountURL(ctx); err != nil || got != accountA.URL {
		t.Fatalf("CertAccountURL = %q, %v; want the issuing account %q", got, err, accountA.URL)
	}
	if err := e.client.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	accountB, err := e.client.Register(ctx, "", true)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if accountB.URL == accountA.URL {
		t.Fatal("the new account has the old account's URL")
	}

	orders := e.fake.Count(acmetest.EndpointNewOrder)
	renew := OrderRequest{Kind: KindRenew, Names: orderReq.Names, Profile: orderReq.Profile, Replaces: ariID}
	if _, err := e.orderer().Run(ctx, renew, nil); err != nil {
		t.Fatalf("renew under the new account: %v", err)
	}
	if n := e.fake.Count(acmetest.EndpointNewOrder) - orders; n != 2 {
		t.Errorf("the renewal made %d newOrder requests, want the refused one and one without replaces", n)
	}
	if seen := e.fake.Orders(); seen[len(seen)-1].Replaces != "" {
		t.Errorf("the accepted renewal replaces %q, want nothing", seen[len(seen)-1].Replaces)
	}
	if got, err := e.store.CertAccountURL(ctx); err != nil || got != accountB.URL {
		t.Errorf("CertAccountURL = %q, %v; want the new account %q", got, err, accountB.URL)
	}

	// The retry is made once: when the CA refuses the order without replaces too, the run fails.
	unauthorized := acmetest.Problem{Status: http.StatusForbidden, Type: "unauthorized", Detail: "scripted refusal"}
	e.fake.FailNext(acmetest.EndpointNewOrder, unauthorized)
	e.fake.FailNext(acmetest.EndpointNewOrder, unauthorized)
	orders = e.fake.Count(acmetest.EndpointNewOrder)
	_, err = e.orderer().Run(ctx, renew, nil)
	var pd *legoacme.ProblemDetails
	if !errors.As(err, &pd) || pd.Type != legoacme.UnauthorizedErrorType {
		t.Errorf("Run = %v, want the CA's unauthorized refusal", err)
	}
	if n := e.fake.Count(acmetest.EndpointNewOrder) - orders; n != 2 {
		t.Errorf("a renewal refused twice made %d newOrder requests, want 2", n)
	}

	// Any other refusal of a renewal is not retried.
	e.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{Status: http.StatusBadRequest, Type: "malformed", Detail: "scripted refusal"})
	orders = e.fake.Count(acmetest.EndpointNewOrder)
	if _, err := e.orderer().Run(ctx, renew, nil); err == nil {
		t.Error("Run succeeded although the CA refused the order")
	}
	if n := e.fake.Count(acmetest.EndpointNewOrder) - orders; n != 1 {
		t.Errorf("a renewal refused as malformed made %d newOrder requests, want 1", n)
	}
}

// An open order placed under an account the CA has since forgotten is not resumed under the new one,
// which may not read it: it is closed and a new order is placed.
func TestOrderDoesNotResumeAnotherAccountsOrder(t *testing.T) {
	ctx := context.Background()
	e := newOrderEnv(t)
	e.crashHeld(orderReq)()
	left, _ := e.openOrder()
	accountA, _, err := e.client.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if left.AccountURL != accountA.URL {
		t.Errorf("the order's account = %q, want %q", left.AccountURL, accountA.URL)
	}

	if err := e.client.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.Register(ctx, "", true); err != nil {
		t.Fatalf("Register: %v", err)
	}
	orders := e.fake.Count(acmetest.EndpointNewOrder)
	if _, err := e.orderer().Run(ctx, orderReq, nil); err != nil {
		t.Fatalf("Run under the new account: %v", err)
	}
	if n := e.fake.Count(acmetest.EndpointNewOrder) - orders; n != 1 {
		t.Errorf("the run made %d newOrder requests, want a new order in place of the old account's", n)
	}
	if got := e.stages(); !slices.Contains(got, StageFailed) {
		t.Errorf("order stages = %v, want the old account's order closed as failed", got)
	}
}
