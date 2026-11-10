// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-acme/lego/v5/acme/api"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// recInstaller records what the manager asks the server to serve.
type recInstaller struct {
	mu         sync.Mutex
	chains     [][]byte
	keys       [][]byte
	selfSigned int
	serveErr   error
}

func (r *recInstaller) ServeACME(chainPEM, keyPEM []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.serveErr != nil {
		return r.serveErr
	}
	r.chains = append(r.chains, slices.Clone(chainPEM))
	r.keys = append(r.keys, slices.Clone(keyPEM))
	return nil
}

func (r *recInstaller) ServeSelfSigned() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.selfSigned++
	return nil
}

func (r *recInstaller) served() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.chains)
}

func (r *recInstaller) selfSignedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.selfSigned
}

// urlSet is the URLs the manager reads on every use; a test changes them as an admin would.
type urlSet struct {
	mu                      sync.Mutex
	public, agent, basePath string
}

func (u *urlSet) setPublic(v string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.public = v
}

func (u *urlSet) setBasePath(v string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.basePath = v
}

func (u *urlSet) getPublic() string { u.mu.Lock(); defer u.mu.Unlock(); return u.public }
func (u *urlSet) getAgent() string  { u.mu.Lock(); defer u.mu.Unlock(); return u.agent }
func (u *urlSet) getBase() string   { u.mu.Lock(); defer u.mu.Unlock(); return u.basePath }

// mgrEnv is a Manager over an order environment: a fake CA with an account registered, a real job
// hub on the test's clock and an installer that records.
type mgrEnv struct {
	*orderEnv
	hub  *jobs.Hub
	inst *recInstaller
	urls *urlSet
	mgr  *Manager
}

const testHost = "tierplan.example.com"

func newMgrEnv(t *testing.T) *mgrEnv {
	t.Helper()
	oe := newOrderEnv(t)
	hub := jobs.New(jobs.Options{Now: oe.clock.Now})
	t.Cleanup(hub.Close)
	e := &mgrEnv{orderEnv: oe, hub: hub, inst: &recInstaller{}, urls: &urlSet{public: "https://" + testHost}}
	e.mgr = e.manager(e.inst, false)
	return e
}

// manager is a Manager over the environment's stored state: what a restarted process would build.
func (e *mgrEnv) manager(inst Installer, certFiles bool) *Manager {
	return NewManager(Deps{
		Store: e.store, Client: e.client, Challenges: e.tokens, Jobs: e.hub, Installer: inst, Clock: e.clock,
		PublicURL: e.urls.getPublic, AgentURL: e.urls.getAgent, BasePath: e.urls.getBase,
		CertFiles: certFiles, Log: slog.New(slog.DiscardHandler),
		Jitter: func() float64 { return 0 },
		Rand:   func(int64) int64 { return 0 },
	})
}

// wait returns the job of a started run once it has ended.
func (e *mgrEnv) wait(rs RunStart) jobs.Job {
	e.t.Helper()
	if rs.JobID == "" {
		e.t.Fatal("no job was started")
	}
	done := make(chan jobs.Job, 1)
	go func() {
		job, err := e.hub.Wait(context.Background(), rs.JobID)
		if err != nil {
			e.t.Errorf("Wait: %v", err)
		}
		done <- job
	}()
	return testsupport.Await(e.t, "the certificate run to end", done)
}

// issue starts an issue and waits for it to end.
func (e *mgrEnv) issue() jobs.Job {
	e.t.Helper()
	rs, err := e.mgr.Issue(context.Background(), "user:1")
	if err != nil {
		e.t.Fatalf("Issue: %v", err)
	}
	return e.wait(rs)
}

func (e *mgrEnv) state() State {
	e.t.Helper()
	st, err := e.store.State(context.Background())
	if err != nil {
		e.t.Fatalf("State: %v", err)
	}
	return st
}

func (e *mgrEnv) runs() []Run {
	e.t.Helper()
	runs, err := e.store.Runs(context.Background(), 50)
	if err != nil {
		e.t.Fatalf("Runs: %v", err)
	}
	return runs
}

func itemsOf(job jobs.Job) map[string]jobs.Item {
	m := map[string]jobs.Item{}
	for _, it := range job.Items {
		m[it.ID] = it
	}
	return m
}

func TestManagerIssueRunsAJobAndServesTheCertificate(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)

	rs, err := e.mgr.Issue(ctx, "user:1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rs.Reused || rs.JobID == "" {
		t.Fatalf("Issue = %+v, want a started job", rs)
	}
	job := e.wait(rs)

	if job.Kind != JobKind || job.Owner != "user:1" || job.Meta["kind"] != KindIssue || job.Meta["trigger"] != TriggerAdmin {
		t.Errorf("job = kind %q owner %q meta %v, want an acme job of user:1 for an admin issue", job.Kind, job.Owner, job.Meta)
	}
	items := itemsOf(job)
	for _, id := range []string{StepAccount, StepOrder, StepChallenge, StepValidate, StepFinalize, StepStored} {
		want := jobs.ItemDone
		if id == StepAccount {
			want = jobs.ItemSkipped
		}
		if items[id].State != want {
			t.Errorf("step %s is %q, want %q", id, items[id].State, want)
		}
	}

	if e.inst.served() != 1 {
		t.Fatalf("ServeACME was called %d times, want 1", e.inst.served())
	}
	leaf := leafOf(t, e.inst.chains[0])
	if !slices.Equal(leaf.DNSNames, []string{testHost}) {
		t.Errorf("the served leaf covers %v, want [%s]", leaf.DNSNames, testHost)
	}
	key, err := decodeKey(e.inst.keys[0])
	if err != nil {
		t.Fatalf("the served key does not decode: %v", err)
	}
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(key.Public()) {
		t.Error("the served key is not the key of the served leaf")
	}
	stored, _, found, err := e.store.Certificate(ctx)
	if err != nil || !found || string(stored) != string(e.inst.chains[0]) {
		t.Errorf("the stored chain differs from the served one (found %v, err %v)", found, err)
	}

	st := e.state()
	if st.Mode != ModeActive || st.ConsecutiveFailures != 0 || st.LastSuccessKind != KindIssue || !st.LastSuccessAt.Equal(testNow) {
		t.Errorf("state = %+v, want active with no failures after an issue at %v", st, testNow)
	}
	wantRenew := leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) * 2 / 3)
	if !st.RenewAt.Equal(wantRenew) || st.RenewBasis != string(BasisTwoThirds) {
		t.Errorf("renew_at = %v (%s), want %v (%s)", st.RenewAt, st.RenewBasis, wantRenew, BasisTwoThirds)
	}
	if !st.AriNextCheckAt.Equal(testNow) {
		t.Errorf("ari_next_check_at = %v, want %v so the scheduler asks next round", st.AriNextCheckAt, testNow)
	}

	runs := e.runs()
	if len(runs) != 1 || runs[0].ID != rs.JobID || runs[0].Outcome != OutcomeSucceeded || runs[0].Kind != KindIssue ||
		runs[0].Trigger != TriggerAdmin || runs[0].Actor != "user:1" {
		t.Errorf("runs = %+v, want one succeeded admin issue by user:1", runs)
	}

	status, err := e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Certificate == nil || !status.Certificate.NotAfter.Equal(leaf.NotAfter) || status.Certificate.Renewed {
		t.Errorf("Status.Certificate = %+v, want the issued certificate, not renewed", status.Certificate)
	}
	if status.LastRun == nil || status.LastRun.Outcome != OutcomeSucceeded || status.ActiveRun != nil {
		t.Errorf("Status runs = active %+v, last %+v, want only a succeeded last run", status.ActiveRun, status.LastRun)
	}
}

// counts snapshots how many requests the fake has seen at every endpoint.
func counts(e *mgrEnv) map[string]int {
	out := map[string]int{}
	for _, ep := range []string{
		acmetest.EndpointDirectory, acmetest.EndpointNewAccount, acmetest.EndpointNewOrder, acmetest.EndpointOrder,
		acmetest.EndpointAuthz, acmetest.EndpointChallenge, acmetest.EndpointFinalize, acmetest.EndpointCert,
		acmetest.EndpointRenewalInfo,
	} {
		out[ep] = e.fake.Count(ep)
	}
	return out
}

// noCARequests fails the test if the fake saw any request since before was taken.
func (e *mgrEnv) noCARequests(before map[string]int) {
	e.t.Helper()
	if after := counts(e); !maps.Equal(before, after) {
		e.t.Errorf("the CA saw requests it should not have: before %v, after %v", before, after)
	}
}

// holdRun starts an issue (or a renewal) whose validation the CA holds, and returns once the run
// is waiting on the clock for the CA, so an order is open at the CA and in the store.
func (e *mgrEnv) holdRun(start func() (RunStart, error)) (rs RunStart, release func()) {
	e.t.Helper()
	release = e.fake.HoldValidation()
	rs, err := start()
	if err != nil {
		release()
		e.t.Fatalf("start a held run: %v", err)
	}
	e.clock.WaitArmed(e.t)
	return rs, release
}

func TestManagerAllowsOneRunAtATime(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	rs, release := e.holdRun(func() (RunStart, error) { return e.mgr.Issue(ctx, "user:1") })

	before := counts(e)
	if _, err := e.mgr.Issue(ctx, "user:2"); !errors.Is(err, ErrRunActive) {
		t.Errorf("a second Issue = %v, want ErrRunActive", err)
	}
	if _, err := e.mgr.RenewNow(ctx, "user:2"); !errors.Is(err, ErrRunActive) {
		t.Errorf("RenewNow during a run = %v, want ErrRunActive", err)
	}
	if err := e.mgr.Stop(ctx, "user:2"); !errors.Is(err, ErrRunActive) {
		t.Errorf("Stop during a run = %v, want ErrRunActive", err)
	}
	if err := e.mgr.SetProfile(ctx, "classic"); !errors.Is(err, ErrRunActive) {
		t.Errorf("SetProfile during a run = %v, want ErrRunActive", err)
	}
	e.noCARequests(before)
	if n := e.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("the CA saw %d orders, want exactly the first run's", n)
	}

	status, err := e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if a := status.ActiveRun; a == nil || a.JobID != rs.JobID || a.Kind != KindIssue || a.Trigger != TriggerAdmin || a.Outcome != OutcomeRunning {
		t.Errorf("Status.ActiveRun = %+v, want the running admin issue %s", a, rs.JobID)
	}

	release()
	e.clock.Advance(PollInterval)
	job := e.wait(rs)
	if job.State != jobs.JobPartial {
		t.Errorf("the job ended %q, want partial (the account step is skipped, the rest done)", job.State)
	}
	// The slot is free again once the run has ended.
	if _, err := e.mgr.Issue(ctx, "user:2"); err != nil {
		t.Errorf("Issue after the run = %v, want it accepted (reused)", err)
	}
}

func TestManagerIssueRefusesWithEveryRefusal(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	e.urls.setPublic("https://192.0.2.1")
	e.urls.setBasePath("/tp")
	before := counts(e)

	_, err := e.mgr.Issue(ctx, "user:1")
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Issue = %v, want a *RefusedError", err)
	}
	var codes []string
	for _, r := range refused.Refusals {
		codes = append(codes, r.Code)
	}
	if !slices.Contains(codes, RefuseNamesIP) || !slices.Contains(codes, RefuseBasePath) {
		t.Errorf("refusals = %v, want names_ip and base_path together", codes)
	}
	e.noCARequests(before)
	if n := e.fake.Count(acmetest.EndpointNewOrder); n != 0 {
		t.Errorf("the CA saw %d orders, want none", n)
	}
	if got := e.hub.List(func(jobs.Job) bool { return true }); len(got) != 0 {
		t.Errorf("a refused Issue started %d jobs, want none", len(got))
	}
	if runs := e.runs(); len(runs) != 0 {
		t.Errorf("a refused Issue recorded %d runs, want none", len(runs))
	}
}

func TestManagerIssueWithUnchangedNamesReuses(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	e.issue()
	leaf := leafOf(t, e.inst.chains[0])
	before := counts(e)

	rs, err := e.mgr.Issue(ctx, "user:1")
	if err != nil {
		t.Fatalf("Issue again: %v", err)
	}
	if !rs.Reused || rs.JobID != "" || !rs.ValidUntil.Equal(leaf.NotAfter) {
		t.Errorf("Issue again = %+v, want Reused until %v and no job", rs, leaf.NotAfter)
	}
	e.noCARequests(before)
	if n := e.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("the CA saw %d orders, want 1", n)
	}
	if e.inst.served() != 1 {
		t.Errorf("the certificate was installed %d times, want once", e.inst.served())
	}

	// Once the certificate is due, the same names are ordered again.
	e.clock.Advance(61 * 24 * time.Hour)
	rs, err = e.mgr.Issue(ctx, "user:1")
	if err != nil || rs.Reused {
		t.Fatalf("Issue when due = %+v, %v, want a new run", rs, err)
	}
	e.wait(rs)
	if n := e.fake.Count(acmetest.EndpointNewOrder); n != 2 {
		t.Errorf("the CA saw %d orders, want 2", n)
	}
}

func TestManagerFailureRecordsTheLedgerAndKeepsServing(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	e.issue()
	served := e.inst.served()
	issuedLeaf := leafOf(t, e.inst.chains[0])

	e.fake.FailValidation(testHost, acmetest.Problem{
		Type: "connection", Detail: "RAW-CA-DETAIL could not connect to 203.0.113.9",
	})
	rs, err := e.mgr.RenewNow(ctx, "user:1")
	if err != nil {
		t.Fatalf("RenewNow: %v", err)
	}
	job := e.wait(rs)

	items := itemsOf(job)
	if items[StepValidate].State != jobs.ItemFailed || items[StepValidate].Code != ClassValidationUnreachable {
		t.Errorf("validate = %q %q, want failed validation_unreachable", items[StepValidate].State, items[StepValidate].Code)
	}
	for _, id := range []string{StepFinalize, StepStored} {
		if items[id].State != jobs.ItemSkipped {
			t.Errorf("step %s is %q, want skipped after the failure", id, items[id].State)
		}
	}

	st := e.state()
	if st.ConsecutiveFailures != 1 || !st.NextAttemptAt.Equal(testNow.Add(time.Minute)) || !st.LastAttemptAt.Equal(testNow) {
		t.Errorf("ledger = failures %d, last %v, next %v; want 1, %v, %v", st.ConsecutiveFailures, st.LastAttemptAt,
			st.NextAttemptAt, testNow, testNow.Add(time.Minute))
	}
	if st.LastErrorClass != ClassValidationUnreachable || st.Mode != ModeActive {
		t.Errorf("state = class %q mode %q, want validation_unreachable and still active", st.LastErrorClass, st.Mode)
	}
	if e.inst.served() != served {
		t.Errorf("a failed renewal installed a certificate (%d calls, want %d)", e.inst.served(), served)
	}

	runs := e.runs()
	if len(runs) != 2 || runs[0].Outcome != OutcomeFailed || runs[0].ErrorClass != ClassValidationUnreachable {
		t.Errorf("runs = %+v, want the newest failed with validation_unreachable", runs)
	}

	status, err := e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	le := status.LastError
	if le == nil || le.Class != ClassValidationUnreachable || le.Host != testHost || !le.At.Equal(testNow) {
		t.Fatalf("Status.LastError = %+v, want validation_unreachable for %s at %v", le, testHost, testNow)
	}
	if !strings.Contains(le.Detail, "RAW-CA-DETAIL") || len(le.Detail) > MaxDetailBytes {
		t.Errorf("Status.LastError.Detail = %q, want the raw problem, capped at %d bytes", le.Detail, MaxDetailBytes)
	}
	if status.Certificate == nil || !status.Certificate.NotAfter.Equal(issuedLeaf.NotAfter) {
		t.Errorf("Status.Certificate = %+v, want the certificate that is still served", status.Certificate)
	}
	if status.Failures != 1 || !status.NextAttemptAt.Equal(testNow.Add(time.Minute)) {
		t.Errorf("Status ledger = %d failures, next %v", status.Failures, status.NextAttemptAt)
	}
}

func TestManagerRetryAfterBlocksManualAttempts(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	e.issue()

	e.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{
		Status: 429, Type: "rateLimited", Detail: "too many orders", RetryAfterSeconds: 3600,
	})
	rs, err := e.mgr.RenewNow(ctx, "user:1")
	if err != nil {
		t.Fatalf("RenewNow: %v", err)
	}
	e.wait(rs)
	if got := e.state().LastErrorClass; got != ClassRateLimited {
		t.Fatalf("last error class = %q, want rate_limited", got)
	}

	before := counts(e)
	until := testNow.Add(time.Hour)
	for name, call := range map[string]func() error{
		"Issue":    func() error { _, err := e.mgr.Issue(ctx, "user:1"); return err },
		"RenewNow": func() error { _, err := e.mgr.RenewNow(ctx, "user:1"); return err },
	} {
		var blocked *BlockedError
		if err := call(); !errors.As(err, &blocked) || blocked.Reason != BlockToldToWait || !blocked.Until.Equal(until) {
			t.Errorf("%s = %v, want a BlockedError told_to_wait until %v", name, err, until)
		}
	}
	e.noCARequests(before)

	status, err := e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if b := status.Block; b == nil || b.Reason != BlockToldToWait || !b.Until.Equal(until) {
		t.Errorf("Status.Block = %+v, want told_to_wait until %v", b, until)
	}

	// At the instant the CA's wait ends, a manual attempt is allowed again.
	e.clock.Advance(time.Hour)
	rs, err = e.mgr.RenewNow(ctx, "user:1")
	if err != nil {
		t.Fatalf("RenewNow when the wait is over: %v", err)
	}
	e.wait(rs)
}

func TestManagerFailureGuardAfterThreeFailedRuns(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	e.fake.FailValidation(testHost, acmetest.Problem{Type: "connection", Detail: "refused"})

	for i := 0; i < FailureGuardRuns; i++ {
		rs, err := e.mgr.Issue(ctx, "user:1")
		if err != nil {
			t.Fatalf("Issue #%d: %v", i+1, err)
		}
		e.wait(rs)
		if i < FailureGuardRuns-1 {
			e.clock.Advance(10 * time.Minute)
		}
	}
	if got := e.state().ConsecutiveFailures; got != FailureGuardRuns {
		t.Fatalf("consecutive failures = %d, want %d", got, FailureGuardRuns)
	}

	before := counts(e)
	_, err := e.mgr.Issue(ctx, "user:1")
	var blocked *BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != BlockFailureGuard || !blocked.Until.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("the fourth Issue = %v, want failure_guard until %v", err, testNow.Add(time.Hour))
	}
	e.noCARequests(before)

	e.clock.Advance(40 * time.Minute) // now + 1 h exactly
	rs, err := e.mgr.Issue(ctx, "user:1")
	if err != nil {
		t.Fatalf("Issue at the instant the guard reopens: %v", err)
	}
	e.wait(rs)
	if n := e.fake.Count(acmetest.EndpointNewOrder); n != FailureGuardRuns+1 {
		t.Errorf("the CA saw %d orders, want %d", n, FailureGuardRuns+1)
	}
}

func TestManagerRenewNowUsesTheCertificatesOwnNamesAndReplaces(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	e.urls.setPublic("https://a.example.com")
	e.issue()
	leaf := leafOf(t, e.inst.chains[0])

	// The URL changes. Nothing orders the new name by itself.
	e.urls.setPublic("https://b.example.com")
	before := counts(e)
	status, err := e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.NamesMismatch || !slices.Equal(status.Names, []string{"b.example.com"}) ||
		status.Certificate == nil || !slices.Equal(status.Certificate.Names, []string{"a.example.com"}) {
		t.Errorf("Status = mismatch %v, names %v, certificate %+v; want a mismatch between a and b",
			status.NamesMismatch, status.Names, status.Certificate)
	}
	e.noCARequests(before)

	rs, err := e.mgr.RenewNow(ctx, "user:1")
	if err != nil {
		t.Fatalf("RenewNow: %v", err)
	}
	job := e.wait(rs)
	if job.Meta["kind"] != KindRenew {
		t.Errorf("job kind = %q, want renew", job.Meta["kind"])
	}
	wantID, err := api.MakeARICertID(leaf)
	if err != nil {
		t.Fatalf("MakeARICertID: %v", err)
	}
	orders := e.fake.Orders()
	if len(orders) != 2 || !slices.Equal(orders[1].Names, []string{"a.example.com"}) || orders[1].Replaces != wantID {
		t.Fatalf("orders = %+v, want a renewal of a.example.com that replaces %s", orders, wantID)
	}
	for _, o := range orders {
		if slices.Contains(o.Names, "b.example.com") {
			t.Errorf("order %+v asked for the changed name without an Issue", o)
		}
	}
	if !e.state().NextAttemptAt.IsZero() || e.state().LastSuccessKind != KindRenew {
		t.Errorf("state after the renewal = %+v, want a renew success", e.state())
	}
	if status, _ = e.mgr.Status(ctx); status.Certificate == nil || !status.Certificate.Renewed || !status.NamesMismatch {
		t.Errorf("Status after the renewal = %+v, want a renewed certificate and a standing mismatch", status.Certificate)
	}

	// Only an admin's Issue orders the new name.
	e.issue()
	orders = e.fake.Orders()
	if len(orders) != 3 || !slices.Equal(orders[2].Names, []string{"b.example.com"}) || orders[2].Replaces != "" {
		t.Errorf("orders = %+v, want the Issue to order b.example.com", orders)
	}
}

func TestManagerStopServesSelfSignedAndKeepsTheAccount(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	e.issue()

	// An order left open at the CA: a renewal an admin cancelled while it waited for validation.
	rs, release := e.holdRun(func() (RunStart, error) { return e.mgr.RenewNow(ctx, "user:1") })
	if err := e.hub.Cancel(rs.JobID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	e.wait(rs)
	release()
	if _, _, found, _ := e.store.OpenOrder(ctx); !found {
		t.Fatal("no order is open after the cancelled run")
	}

	if err := e.mgr.Stop(ctx, "user:1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if e.inst.selfSignedCount() != 1 {
		t.Errorf("ServeSelfSigned was called %d times, want once", e.inst.selfSignedCount())
	}
	if st := e.state(); st.Mode != ModeOff {
		t.Errorf("mode = %q, want off", st.Mode)
	}
	if _, _, found, _ := e.store.OpenOrder(ctx); found {
		t.Error("an order is still open after Stop")
	}
	if _, ok, err := e.client.Account(ctx); err != nil || !ok {
		t.Errorf("the account after Stop = %v, %v, want it kept", ok, err)
	}
	if _, _, found, err := e.store.Certificate(ctx); err != nil || !found {
		t.Errorf("the certificate after Stop = %v, %v, want it kept", found, err)
	}
	if err := e.mgr.Stop(ctx, "user:1"); !errors.Is(err, ErrNotActive) {
		t.Errorf("Stop again = %v, want ErrNotActive", err)
	}
	if _, err := e.mgr.RenewNow(ctx, "user:1"); !errors.Is(err, ErrNotActive) {
		t.Errorf("RenewNow while stopped = %v, want ErrNotActive", err)
	}

	// Issuing the same names serves the stored certificate again; no order.
	before := counts(e)
	served := e.inst.served()
	rs, err := e.mgr.Issue(ctx, "user:1")
	if err != nil || !rs.Reused {
		t.Fatalf("Issue after Stop = %+v, %v, want the stored certificate reused", rs, err)
	}
	e.noCARequests(before)
	if e.inst.served() != served+1 || e.state().Mode != ModeActive {
		t.Errorf("after the re-issue: installed %d times (want %d), mode %q (want active)", e.inst.served(), served+1, e.state().Mode)
	}
}

func TestManagerCancelledRunLeavesTheLedgerAlone(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	rs, release := e.holdRun(func() (RunStart, error) { return e.mgr.Issue(ctx, "user:1") })
	if err := e.hub.Cancel(rs.JobID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	job := e.wait(rs)
	release()

	if it := itemsOf(job)[StepValidate]; it.State != jobs.ItemCancelled {
		t.Errorf("the validate step ended %q, want cancelled", it.State)
	}
	if st := e.state(); st.ConsecutiveFailures != 0 || !st.NextAttemptAt.IsZero() || st.LastErrorClass != "" {
		t.Errorf("a cancelled run charged the ledger: %+v", st)
	}
	if runs := e.runs(); len(runs) != 1 || runs[0].Outcome != OutcomeCancelled {
		t.Errorf("runs = %+v, want one cancelled run", runs)
	}
	// The slot is free, and the open order is picked up by the next run.
	rs, err := e.mgr.Issue(ctx, "user:1")
	if err != nil {
		t.Fatalf("Issue after a cancel: %v", err)
	}
	e.wait(rs)
	if n := e.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("the CA saw %d orders, want the cancelled run's order resumed (1)", n)
	}
}

func TestManagerPausedMakesNoRequests(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	e.issue()
	st := e.state()
	st.PausedReason = PausedRestore
	if err := e.store.SaveState(ctx, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	before := counts(e)

	if _, err := e.mgr.Issue(ctx, "user:1"); !errors.Is(err, ErrPaused) {
		t.Errorf("Issue = %v, want ErrPaused", err)
	}
	if _, err := e.mgr.RenewNow(ctx, "user:1"); !errors.Is(err, ErrPaused) {
		t.Errorf("RenewNow = %v, want ErrPaused", err)
	}
	if err := e.mgr.SetProfile(ctx, "classic"); !errors.Is(err, ErrPaused) {
		t.Errorf("SetProfile = %v, want ErrPaused", err)
	}
	if _, err := e.mgr.Directory(ctx); !errors.Is(err, ErrPaused) {
		t.Errorf("Directory = %v, want ErrPaused", err)
	}
	if _, err := e.mgr.Register(ctx, "", true); !errors.Is(err, ErrPaused) {
		t.Errorf("Register = %v, want ErrPaused", err)
	}
	e.noCARequests(before)

	status, err := e.mgr.Status(ctx)
	if err != nil || status.PausedReason != PausedRestore {
		t.Errorf("Status.PausedReason = %q, %v, want restore", status.PausedReason, err)
	}

	if err := e.mgr.Resume(ctx, "user:1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := e.state().PausedReason; got != "" {
		t.Errorf("paused_reason after Resume = %q, want empty", got)
	}
	if err := e.mgr.Resume(ctx, "user:1"); !errors.Is(err, ErrNotPaused) {
		t.Errorf("Resume again = %v, want ErrNotPaused", err)
	}
	if _, err := e.mgr.Directory(ctx); err != nil {
		t.Errorf("Directory after Resume: %v", err)
	}
}

func TestManagerProfile(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)

	if err := e.mgr.SetProfile(ctx, "shortlived"); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	if got := e.state().Profile; got != "shortlived" {
		t.Errorf("saved profile = %q, want shortlived", got)
	}
	if err := e.mgr.SetProfile(ctx, "nope"); !errors.Is(err, ErrProfileUnknown) {
		t.Errorf("SetProfile(nope) = %v, want ErrProfileUnknown", err)
	}
	if got := e.state().Profile; got != "shortlived" {
		t.Errorf("an unknown profile replaced the saved one: %q", got)
	}

	e.issue()
	if orders := e.fake.Orders(); len(orders) != 1 || orders[0].Profile != "shortlived" {
		t.Errorf("orders = %+v, want one that asks for shortlived", orders)
	}
	// The short-lived profile gives a certificate under ten days: renewal is at half its life.
	if st := e.state(); st.RenewBasis != string(BasisHalf) {
		t.Errorf("renew basis = %q, want half for a short-lived certificate", st.RenewBasis)
	}

	if err := e.mgr.SetProfile(ctx, ""); err != nil || e.state().Profile != "" {
		t.Errorf("SetProfile(\"\") = %v, profile %q, want the CA default", err, e.state().Profile)
	}
}

func TestManagerAccountGoneForgetsTheAccount(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	e.fake.ForgetAccounts()

	rs, err := e.mgr.Issue(ctx, "user:1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	job := e.wait(rs)
	if it := itemsOf(job)[StepOrder]; it.State != jobs.ItemFailed || it.Code != ClassAccountGone {
		t.Errorf("order step = %q %q, want failed account_gone", it.State, it.Code)
	}

	status, err := e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Account != nil {
		t.Errorf("Status.Account = %+v, want it forgotten so the form appears again", status.Account)
	}
	if status.LastError == nil || status.LastError.Class != ClassAccountGone {
		t.Errorf("Status.LastError = %+v, want account_gone", status.LastError)
	}
	if _, err := e.mgr.Issue(ctx, "user:1"); !errors.Is(err, ErrNoAccount) {
		t.Errorf("Issue without an account = %v, want ErrNoAccount", err)
	}
}

func TestManagerInstallFailureKeepsTheStoredCertificate(t *testing.T) {
	ctx := context.Background()
	e := newMgrEnv(t)
	e.inst.serveErr = errors.New("the listener refused the certificate")

	job := e.issue()
	if it := itemsOf(job)[StepStored]; it.State != jobs.ItemFailed || it.Code != ClassInternal {
		t.Errorf("stored step = %q %q, want failed internal", it.State, it.Code)
	}
	st := e.state()
	if st.Mode != ModeActive || st.ConsecutiveFailures != 0 || st.LastErrorClass != ClassInternal {
		t.Errorf("state = %+v, want active, no failures, last error internal", st)
	}
	if _, _, found, err := e.store.Certificate(ctx); err != nil || !found {
		t.Errorf("the certificate = %v, %v, want it stored for the next start", found, err)
	}
	if runs := e.runs(); len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded {
		t.Errorf("runs = %+v, want the run recorded as succeeded: the CA part worked", runs)
	}
}
