// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/ecdsa"
	"slices"
	"sync"
	"testing"

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
		CertFiles: certFiles,
		Jitter:    func() float64 { return 0 },
		Rand:      func(int64) int64 { return 0 },
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
