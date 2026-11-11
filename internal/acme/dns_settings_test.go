// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dnscheck"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// forgetToken removes the token the environment saved, so a test starts from an install with none.
func (e *dnsEnv) forgetToken() {
	e.t.Helper()
	if err := e.store.DeleteDNSToken(context.Background(), dnsProviderID); err != nil {
		e.t.Fatalf("DeleteDNSToken: %v", err)
	}
}

// savedToken is the token the store holds for the fake provider, "" when none.
func (e *dnsEnv) savedToken() string {
	e.t.Helper()
	tok, _, err := e.store.DNSToken(context.Background(), dnsProviderID)
	if err != nil {
		e.t.Fatalf("DNSToken: %v", err)
	}
	return tok
}

func TestSaveValidationTestsANewTokenAndSealsIt(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeHTTP01)
	e.forgetToken()
	before := counts(e.mgrEnv)

	res, err := e.mgr.SaveValidation(ctx, ValidationInput{ChallengeType: ChallengeDNS01, Token: dnsToken})
	if err != nil {
		t.Fatalf("SaveValidation: %v", err)
	}

	if !res.Saved || res.Test == nil || !res.Test.Passed {
		t.Fatalf("result = %+v, want saved after a passed test", res)
	}
	if len(res.Test.Names) != 1 {
		t.Fatalf("tested %d names, want the public host alone", len(res.Test.Names))
	}
	n := res.Test.Names[0]
	if n.Name != testHost || n.Record != dns.ChallengeFQDN(testHost) || n.Zone != "example.com" {
		t.Errorf("name test = %+v, want %s at %s in example.com", n, testHost, dns.ChallengeFQDN(testHost))
	}
	for i, id := range []string{TestStepZone, TestStepCreated, TestStepRead, TestStepDeleted} {
		if s := n.Steps[i]; s.ID != id || s.State != TestDone {
			t.Errorf("step %d = %+v, want %s done", i, s, id)
		}
	}
	if n.Info.State != InfoSeen || n.Info.Seconds != 0 {
		t.Errorf("info = %+v, want seen after 0 s", n.Info)
	}
	if n.Failure != nil {
		t.Errorf("failure = %+v, want none", n.Failure)
	}

	if got := e.savedToken(); got != dnsToken {
		t.Errorf("saved token = %q, want the typed one", got)
	}
	st := e.state()
	if st.ChallengeType != ChallengeDNS01 || !st.ValidationUntested {
		t.Errorf("state = challenge %q, untested %v, want dns-01 and untested", st.ChallengeType, st.ValidationUntested)
	}
	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v after the test", recs)
	}
	if rows := e.ledger(); len(rows) != 0 {
		t.Errorf("the ledger still holds %+v after the test", rows)
	}
	e.noCARequests(before)
}

// wrappedProvider is a provider a test can listen to and answer differently from.
type wrappedProvider struct {
	dns.Provider
	// get, when set, answers every read.
	get func(dns.RecordRef) (dns.Record, error)
	// afterDelete, when set, is called after every delete.
	afterDelete func()
}

func (w wrappedProvider) GetTXT(ctx context.Context, ref dns.RecordRef) (dns.Record, error) {
	if w.get != nil {
		return w.get(ref)
	}
	return w.Provider.GetTXT(ctx, ref)
}

func (w wrappedProvider) DeleteTXT(ctx context.Context, ref dns.RecordRef) error {
	err := w.Provider.DeleteTXT(ctx, ref)
	if w.afterDelete != nil {
		w.afterDelete()
	}
	return err
}

// wrapProviders rebuilds the environment's manager so every provider it builds goes through wrap.
func (e *dnsEnv) wrapProviders(wrap func(dns.Provider) dns.Provider) {
	inner := e.countingFactory()
	e.mgr = NewManager(Deps{
		Store: e.store, Client: e.client, Challenges: e.tokens, Jobs: e.hub, Installer: e.inst, Clock: e.clock,
		PublicURL: e.urls.getPublic, AgentURL: e.urls.getAgent, BasePath: e.urls.getBase,
		Log:    slog.New(slog.NewTextHandler(e.logs, nil)),
		Jitter: func() float64 { return 0 },
		Rand:   func(int64) int64 { return 0 },
		DNS: DNSDeps{
			Providers:       map[string]dns.Factory{dnsProviderID: func(token string) dns.Provider { return wrap(inner(token)) }},
			NameserverAddrs: func(context.Context, string) ([]string, error) { return []string{e.nsAddr}, nil },
			ResolvConf:      func() ([]string, error) { return []string{e.dns.Addr()}, nil },
		},
	})
}

// testToken runs Test token and fails the test on an error or when the CA saw any request.
func (e *dnsEnv) testToken(req TestRequest) TokenTest {
	e.t.Helper()
	before := counts(e.mgrEnv)
	tt, err := e.mgr.TestToken(context.Background(), req)
	if err != nil {
		e.t.Fatalf("TestToken: %v", err)
	}
	e.noCARequests(before)
	return tt
}

// tokenResult is the outcome of a Test token that runs in the background.
type tokenResult struct {
	test TokenTest
	err  error
}

// testTokenAsync starts Test token and returns the channel its result arrives on.
func (e *dnsEnv) testTokenAsync(req TestRequest) <-chan tokenResult {
	out := make(chan tokenResult, 1)
	go func() {
		tt, err := e.mgr.TestToken(context.Background(), req)
		out <- tokenResult{tt, err}
	}()
	return out
}

// rows is the states of a name's four rows, such as "done,done,failed,skipped".
func rows(n NameTest) string {
	states := make([]string, len(n.Steps))
	for i, s := range n.Steps {
		states[i] = s.State
	}
	return strings.Join(states, ",")
}

func wantRows(t *testing.T, n NameTest, want string) {
	t.Helper()
	if got := rows(n); got != want {
		t.Errorf("%s: rows = %s, want %s", n.Name, got, want)
	}
	for i, id := range []string{TestStepZone, TestStepCreated, TestStepRead, TestStepDeleted} {
		if n.Steps[i].ID != id {
			t.Errorf("%s: row %d is %q, want %q", n.Name, i, n.Steps[i].ID, id)
		}
	}
}

func TestDNSTokenSavedTokenPassesAndStoresNothing(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	untested := e.state().ValidationUntested

	tt := e.testToken(TestRequest{})

	if !tt.Passed || len(tt.Names) != 1 {
		t.Fatalf("result = %+v, want one name that passed", tt)
	}
	wantRows(t, tt.Names[0], "done,done,done,done")
	if got := e.savedToken(); got != dnsToken {
		t.Errorf("saved token = %q, want it unchanged", got)
	}
	if st := e.state(); st.ChallengeType != ChallengeDNS01 || st.ValidationUntested != untested {
		t.Errorf("state = challenge %q untested %v, want the settings as they were", st.ChallengeType, st.ValidationUntested)
	}
	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v", recs)
	}
	if rows := e.ledger(); len(rows) != 0 {
		t.Errorf("the ledger still holds %+v", rows)
	}
}

func TestDNSTokenTypedTokenIsUsedInsteadOfTheSaved(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	if err := e.store.SaveDNSToken(context.Background(), dnsProviderID, "saved-but-wrong-token", e.clock.Now()); err != nil {
		t.Fatal(err)
	}

	if tt := e.testToken(TestRequest{Token: dnsToken}); !tt.Passed {
		t.Errorf("the typed, accepted token failed: %+v", tt)
	}
	tt := e.testToken(TestRequest{})
	if tt.Passed {
		t.Errorf("the saved, unaccepted token passed: %+v", tt)
	}
	if got := tt.Names[0].Steps[0]; got.State != TestFailed || got.Class != dns.ClassTokenInvalid {
		t.Errorf("zone row = %+v, want failed as dns_token_invalid", got)
	}
	if got := e.savedToken(); got != "saved-but-wrong-token" {
		t.Errorf("saved token = %q, want it untouched by a test", got)
	}
}

func TestDNSTokenTwoNamesAreListedInCertificateOrder(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	const agent = "agent.example.com"
	e.setAgent("https://" + agent)
	agentDeleted := make(chan struct{})
	var once sync.Once
	e.wrapProviders(func(p dns.Provider) dns.Provider {
		return wrappedProvider{Provider: p, afterDelete: func() { once.Do(func() { close(agentDeleted) }) }}
	})
	chal := dns.ChallengeFQDN(testHost)
	e.dns.Hold(chal) // the public host's record shows late, so the agent host finishes first

	out := e.testTokenAsync(TestRequest{})
	e.clock.WaitArmed(t) // the public host is waiting for its record
	testsupport.Await(t, "the agent host's test record to be deleted", agentDeleted)
	e.dns.Release(chal)
	e.clock.Advance(dnscheck.PropagationInterval)
	res := testsupport.Await(t, "Test token to finish", out)

	if res.err != nil {
		t.Fatalf("TestToken: %v", res.err)
	}
	if !res.test.Passed || len(res.test.Names) != 2 {
		t.Fatalf("result = %+v, want two names that passed", res.test)
	}
	if got := []string{res.test.Names[0].Name, res.test.Names[1].Name}; !slices.Equal(got, []string{testHost, agent}) {
		t.Errorf("names = %v, want the public host first", got)
	}
	for _, n := range res.test.Names {
		wantRows(t, n, "done,done,done,done")
	}
}

func TestDNSTokenAgentHostEqualToPublicHostIsOneName(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	e.setAgent("https://" + testHost + ":8443")

	tt := e.testToken(TestRequest{})

	if len(tt.Names) != 1 || tt.Names[0].Name != testHost {
		t.Errorf("names = %+v, want %s once", tt.Names, testHost)
	}
	if n := e.prov.CallCount("create"); n != 1 {
		t.Errorf("%d records were created, want 1", n)
	}
}

func TestDNSTokenDelegatedNameWritesAtTheTarget(t *testing.T) {
	const target = "_acme-challenge.tierplan.acme-target.net"
	e := newDNSEnv(t, ChallengeDNS01)
	e.prov.AddZone("acme-target.net", dnsNS)
	e.cname(dns.ChallengeFQDN(testHost), target)

	tt := e.testToken(TestRequest{})

	if !tt.Passed {
		t.Fatalf("result = %+v, want a pass", tt)
	}
	n := tt.Names[0]
	if n.DelegatedTo != target || n.Record != target || n.Zone != "acme-target.net" || n.OwnZone != "" {
		t.Errorf("name test = %+v, want the record, delegation and zone of %s and no OwnZone", n, target)
	}
	var created []string
	for _, c := range e.prov.Calls() {
		if c.Op == "create" {
			created = append(created, c.Name)
		}
	}
	if !slices.Equal(created, []string{target}) {
		t.Errorf("records were created at %v, want only %s", created, target)
	}
}

func TestDNSTokenOwnZoneNotOnTheProviderPasses(t *testing.T) {
	const target = "_acme-challenge.x.acme-target.net"
	e := newDNSEnv(t, ChallengeDNS01)
	e.urls.setPublic("https://tierplan.example.org") // the provider holds example.com, not example.org
	e.prov.AddZone("acme-target.net", dnsNS)
	e.cname(dns.ChallengeFQDN("tierplan.example.org"), target)

	tt := e.testToken(TestRequest{})

	if !tt.Passed {
		t.Fatalf("result = %+v, want a delegation into a provider zone to pass (D-15)", tt)
	}
	if n := tt.Names[0]; n.OwnZone != "example.org" || n.DelegatedTo != target || n.Zone != "acme-target.net" {
		t.Errorf("name test = %+v, want own zone example.org, delegated to %s in acme-target.net", n, target)
	}
}

func TestDNSTokenInfoSeenAtOnce(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)

	n := e.testToken(TestRequest{}).Names[0]

	if got := n.Info; got.State != InfoSeen || got.Seconds != 0 || !slices.Equal(got.Resolvers, []string{dnsNS}) {
		t.Errorf("info = %+v, want seen by %s after 0 s", got, dnsNS)
	}
}

func TestDNSTokenInfoSeenAfterTwoPolls(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	chal := dns.ChallengeFQDN(testHost)
	e.dns.Hold(chal)

	out := e.testTokenAsync(TestRequest{})
	e.clock.WaitArmed(t)
	e.clock.Advance(dnscheck.PropagationInterval) // the second look: still missing
	e.clock.WaitArmed(t)
	e.dns.Release(chal)
	e.clock.Advance(dnscheck.PropagationInterval)
	res := testsupport.Await(t, "Test token to finish", out)

	if res.err != nil || !res.test.Passed {
		t.Fatalf("TestToken = %+v, %v, want a pass", res.test, res.err)
	}
	if got := res.test.Names[0].Info; got.State != InfoSeen || got.Seconds != 4 {
		t.Errorf("info = %+v, want seen after 4 s", got)
	}
}

func TestDNSTokenInfoNotSeenDoesNotChangeTheVerdict(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	e.dns.Hold(dns.ChallengeFQDN(testHost)) // the record never shows

	out := e.testTokenAsync(TestRequest{})
	e.clock.WaitArmed(t)
	e.clock.Advance(2 * dnscheck.TestObserveLimit) // well past the cap
	res := testsupport.Await(t, "Test token to finish", out)

	if res.err != nil {
		t.Fatalf("TestToken: %v", res.err)
	}
	if !res.test.Passed {
		t.Errorf("the test failed for a record that did not show: %+v", res.test)
	}
	n := res.test.Names[0]
	wantRows(t, n, "done,done,done,done")
	if got := n.Info; got.State != InfoNotSeen || got.Seconds != 20 || !slices.Equal(got.Resolvers, []string{dnsNS}) {
		t.Errorf("info = %+v, want not seen by %s within 20 s (capped)", got, dnsNS)
	}
}

func TestDNSTokenDraftResolversReplaceTheNameservers(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	resolver := e.dns.Addr()
	draft := []string{resolver}

	n := e.testToken(TestRequest{Resolvers: &draft}).Names[0]

	if got := n.Info; got.State != InfoSeen || !slices.Equal(got.Resolvers, []string{resolver}) {
		t.Errorf("info = %+v, want seen by the draft resolver %s", got, resolver)
	}
	var recursive bool
	for _, q := range e.dns.Queries() {
		if q.Type == 16 && q.Name == strings.TrimSuffix(n.Record, ".") && q.Recursive {
			recursive = true
		}
	}
	if !recursive {
		t.Error("no recursive TXT question for the test record reached the draft resolver")
	}
}

func TestDNSTokenReadBackOfADifferentValueFails(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	e.wrapProviders(func(p dns.Provider) dns.Provider {
		return wrappedProvider{Provider: p, get: func(dns.RecordRef) (dns.Record, error) {
			return dns.Record{Content: "something-else"}, nil
		}}
	})

	tt := e.testToken(TestRequest{})

	if tt.Passed {
		t.Fatalf("result = %+v, want a failure", tt)
	}
	n := tt.Names[0]
	wantRows(t, n, "done,done,failed,done")
	if n.Steps[2].Class == "" || n.Failure == nil || n.Failure.Class != n.Steps[2].Class {
		t.Errorf("read row = %+v, failure = %+v, want a classified failure on the read row", n.Steps[2], n.Failure)
	}
	if n.Info.State != InfoNotChecked {
		t.Errorf("info = %+v, want not checked", n.Info)
	}
	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v: a failed read must still delete", recs)
	}
	if rows := e.ledger(); len(rows) != 0 {
		t.Errorf("the ledger still holds %+v", rows)
	}
}

func TestDNSTokenDeleteFailureFailsTheTestAndKeepsTheRow(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	e.prov.FailDelete(-1, &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassTokenScope, Status: 403})

	tt := e.testToken(TestRequest{})

	if tt.Passed {
		t.Fatalf("result = %+v, want a failure: a test record that cannot be deleted fails the test", tt)
	}
	n := tt.Names[0]
	wantRows(t, n, "done,done,done,failed")
	if n.Steps[3].Class != dns.ClassTokenScope || n.Failure == nil || n.Failure.Class != dns.ClassTokenScope {
		t.Errorf("deleted row = %+v, failure = %+v, want dns_token_scope", n.Steps[3], n.Failure)
	}
	if n.Info.State != InfoSeen {
		t.Errorf("info = %+v, want what it saw (seen) even though the test failed", n.Info)
	}
	left := e.ledger()
	if len(left) != 1 || left[0].OwnerKind != OwnerTest || left[0].LastErrorClass != dns.ClassTokenScope || left[0].Attempts != 1 {
		t.Fatalf("ledger = %+v, want the one test record, stamped with the failed delete", left)
	}
	if !strings.HasPrefix(left[0].Value, "tierplan-test-") || !strings.HasPrefix(left[0].Comment, "tierplan:") || !strings.Contains(left[0].Comment, ":test-") {
		t.Errorf("test record value %q and comment %q are not marked as a TierPlan test", left[0].Value, left[0].Comment)
	}
}

func TestDNSTokenInvalidTokenStopsAtTheZone(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)

	tt := e.testToken(TestRequest{Token: "a-token-the-provider-refuses"})

	if tt.Passed {
		t.Fatalf("result = %+v, want a failure", tt)
	}
	n := tt.Names[0]
	wantRows(t, n, "failed,skipped,skipped,skipped")
	if n.Steps[0].Class != dns.ClassTokenInvalid || n.Info.State != InfoNotChecked {
		t.Errorf("zone row = %+v, info = %+v, want dns_token_invalid and not checked", n.Steps[0], n.Info)
	}
	if c := e.prov.CallCount("create"); c != 0 {
		t.Errorf("%d records were created with a token the provider refuses", c)
	}
}

func TestDNSTokenMalformedTokenMakesNoRequest(t *testing.T) {
	for name, token := range map[string]string{
		"a space inside":    "has a space",
		"a control byte":    "tab\there",
		"over 256 bytes":    strings.Repeat("a", 257),
		"a non-ASCII byte":  "caf\u00e9-token",
		"a newline at once": "line\nbreak",
	} {
		t.Run(name, func(t *testing.T) {
			e := newDNSEnv(t, ChallengeDNS01)

			tt := e.testToken(TestRequest{Token: token})

			if tt.Passed || len(tt.Names) != 1 {
				t.Fatalf("result = %+v, want one failed name", tt)
			}
			n := tt.Names[0]
			wantRows(t, n, "failed,skipped,skipped,skipped")
			if n.Steps[0].Class != dns.ClassTokenInvalid || n.Info.State != InfoNotChecked {
				t.Errorf("zone row = %+v, info = %+v, want dns_token_invalid and not checked", n.Steps[0], n.Info)
			}
			if calls := e.prov.Calls(); len(calls) != 0 || e.built.Load() != 0 {
				t.Errorf("the provider saw %+v and %d were built, want no request at all", calls, e.built.Load())
			}
		})
	}
}

func TestDNSTokenNoTokenTypedOrSavedIsRefusedBeforeAnyCall(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	e.forgetToken()

	for _, typed := range []string{"", "   "} {
		if _, err := e.mgr.TestToken(context.Background(), TestRequest{Token: typed}); !errors.Is(err, ErrDNSTokenRequired) {
			t.Errorf("TestToken(%q) = %v, want ErrDNSTokenRequired", typed, err)
		}
	}
	if calls := e.prov.Calls(); len(calls) != 0 {
		t.Errorf("the provider saw %+v", calls)
	}
}

func TestDNSTokenWithNoCertificateNamesGivesIssuesRefusals(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	e.urls.setPublic("")
	_, issueErr := e.mgr.Issue(context.Background(), "user:1")
	var issued *RefusedError
	if !errors.As(issueErr, &issued) {
		t.Fatalf("Issue = %v, want a RefusedError to compare with", issueErr)
	}

	_, err := e.mgr.TestToken(context.Background(), TestRequest{})

	var refused *RefusedError
	if !errors.As(err, &refused) || !reflect.DeepEqual(refused.Refusals, issued.Refusals) {
		t.Errorf("TestToken = %v, want Issue's refusals %+v", err, issued.Refusals)
	}
	if calls := e.prov.Calls(); len(calls) != 0 {
		t.Errorf("the provider saw %+v", calls)
	}
}

func TestDNSTokenPausedAndCertificateFilesRefuseTheTest(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	if m := e.build(true); true {
		if _, err := m.TestToken(context.Background(), TestRequest{}); !errors.Is(err, ErrCertFiles) {
			t.Errorf("TestToken with certificate files = %v, want ErrCertFiles", err)
		}
	}
	st := e.state()
	st.PausedReason = PausedRestore
	if err := e.store.SaveState(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	if _, err := e.mgr.TestToken(context.Background(), TestRequest{}); !errors.Is(err, ErrPaused) {
		t.Errorf("TestToken while paused = %v, want ErrPaused", err)
	}
	if calls := e.prov.Calls(); len(calls) != 0 {
		t.Errorf("the provider saw %+v", calls)
	}
}

func TestDNSTokenTestHoldsTheDNSSlot(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	e.dns.Hold(dns.ChallengeFQDN(testHost))
	before := counts(e.mgrEnv)

	out := e.testTokenAsync(TestRequest{})
	e.clock.WaitArmed(t) // the test is waiting for its record, holding the slot

	if _, err := e.mgr.Issue(ctx, "user:1"); !errors.Is(err, ErrRunActive) {
		t.Errorf("Issue during a test = %v, want ErrRunActive", err)
	}
	if _, err := e.mgr.RenewNow(ctx, "user:1"); !errors.Is(err, ErrRunActive) {
		t.Errorf("RenewNow during a test = %v, want ErrRunActive", err)
	}
	if err := e.mgr.Stop(ctx, "user:1"); !errors.Is(err, ErrRunActive) {
		t.Errorf("Stop during a test = %v, want ErrRunActive", err)
	}
	if _, err := e.mgr.SaveValidation(ctx, ValidationInput{ChallengeType: ChallengeDNS01}); !errors.Is(err, ErrRunActive) {
		t.Errorf("SaveValidation during a test = %v, want ErrRunActive", err)
	}
	if _, err := e.mgr.TestToken(ctx, TestRequest{}); !errors.Is(err, ErrRunActive) {
		t.Errorf("a second test = %v, want ErrRunActive", err)
	}
	if !e.mgr.runActive() {
		t.Error("runActive is false during a test, so the scheduler would start a run")
	}
	status, err := e.mgr.Status(ctx)
	if err != nil || status.ActiveRun != nil {
		t.Errorf("Status.ActiveRun = %+v, %v, want nil: a test is not a run", status.ActiveRun, err)
	}
	e.noCARequests(before)

	e.dns.Release(dns.ChallengeFQDN(testHost))
	e.clock.Advance(dnscheck.PropagationInterval)
	res := testsupport.Await(t, "Test token to finish", out)
	if res.err != nil || !res.test.Passed {
		t.Fatalf("TestToken = %+v, %v, want a pass", res.test, res.err)
	}
	if e.mgr.runActive() {
		t.Error("the slot is still held after the test ended")
	}
	if _, err := e.mgr.Issue(ctx, "user:1"); err != nil {
		t.Errorf("Issue after the test: %v", err)
	}
}

func TestDNSTokenResultErrorsAndLogsCarryNoToken(t *testing.T) {
	const typedBad = "typed-secret-token-4411-refused"
	e := newDNSEnv(t, ChallengeDNS01)
	e.prov.FailDelete(-1, &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassUnavailable, Status: 502})

	var seen []string
	seen = append(seen, fmt.Sprintf("%+v", e.testToken(TestRequest{})))
	seen = append(seen, fmt.Sprintf("%+v", e.testToken(TestRequest{Token: typedBad})))
	_, err := e.mgr.TestToken(context.Background(), TestRequest{Token: strings.Repeat("x", 300)})
	seen = append(seen, fmt.Sprint(err), e.logs.String())

	for _, text := range seen {
		if strings.Contains(text, dnsToken) || strings.Contains(text, typedBad) {
			t.Errorf("a token appears in %q", text)
		}
	}
}
