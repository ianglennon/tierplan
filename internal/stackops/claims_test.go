// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// A deploy stores the claims of its stack at the end of the check item, before the engine is touched, and puts them
// back as the last good revision declared them whenever it does not end well (plan 24-15, D-15, D-16, D-45). These
// tests run the real ingress store over the real database; only the engine is a fake.

// claimOps is stackops.Ingress over the real store, with a seam to let another writer in between two calls.
type claimOps struct {
	st *ingressstore.Store
	// after, when set, runs once, right after the first successful ReplaceStackRoutes of stackID.
	mu      sync.Mutex
	stackID string
	after   func()
}

func (o *claimOps) AttachFor(context.Context, string, stacks.Target) (*stacks.IngressAttach, error) {
	return nil, nil
}

func (o *claimOps) World(ctx context.Context, envID string, canView func(string) bool) (ingress.World, error) {
	return o.st.World(ctx, envID, canView)
}

func (o *claimOps) ReplaceStackRoutes(ctx context.Context, in ingressstore.ReplaceInput) (ingress.Plan, error) {
	plan, err := o.st.ReplaceStackRoutes(ctx, in)
	if err != nil {
		return plan, err
	}
	o.mu.Lock()
	var run func()
	if o.after != nil && in.StackID == o.stackID {
		run, o.after = o.after, nil
	}
	o.mu.Unlock()
	if run != nil {
		run()
	}
	return plan, nil
}

func (o *claimOps) ReleaseStack(ctx context.Context, envID, stackID, actor string) error {
	return o.st.ReleaseStack(ctx, envID, stackID, actor, nil)
}

func (o *claimOps) Notify(envID string) { o.st.Notify(envID) }

// syncBuf is a log sink two goroutines may write.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// claimFix is a deploy rig with an ingress store that has one domain, example.com, registered for the environment.
type claimFix struct {
	rig      *swarmRig
	repo     stackops.Repo
	ing      *ingressstore.Store
	ops      *claimOps
	notifier *ingress.Notifier
	logs     *syncBuf
}

func newClaimFix(t *testing.T, standaloneHost bool, tune func(*dockertest.FakeConfig), services ...dockertest.FakeService) *claimFix {
	t.Helper()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		if standaloneHost {
			standalone(c)
			c.TasksFollowWrites = false
		}
		if tune != nil {
			tune(c)
		}
	}, services...)
	v, err := vault.New(bytes.Repeat([]byte{5}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	f := &claimFix{rig: rig, notifier: ingress.NewNotifier(), logs: &syncBuf{}}
	f.repo = stackops.Repo{Store: rig.store, Vault: v, Now: time.Now}
	f.ing = ingressstore.New(ingressstore.Deps{St: rig.store, Vault: v, Notifier: f.notifier})
	f.ops = &claimOps{st: f.ing}
	rig.deps.Ingress = f.ops
	rig.deps.Log = slog.New(slog.NewTextHandler(f.logs, nil))
	if _, err := f.ing.CreateDomain(testsupport.Context(t), ingressstore.CreateDomainInput{
		EnvID: rig.envID, Suffix: "example.com", Issuer: ingress.IssuerStaging, Actor: "user:test",
	}, nil); err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	return f
}

// routed is a one-service swarm compose file that asks for host.
func routed(host string) string {
	return "services:\n  web:\n    image: nginx:1.27-alpine\n    deploy:\n      labels:\n        tierplan.ingress.host: " + host +
		"\n        tierplan.ingress.port: \"80\"\n"
}

// routedStandalone is the same for a standalone host.
func routedStandalone(host string) string {
	return "services:\n  web:\n    image: nginx:1.27-alpine\n    labels:\n      tierplan.ingress.host: " + host +
		"\n      tierplan.ingress.port: \"80\"\n"
}

// declsOf are the routes a compose file declares.
func declsOf(t *testing.T, compose string, target stacks.Target, files ...stacks.File) []ingress.Declaration {
	t.Helper()
	all := append([]stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, files...)
	// ingress: unattached, only the declarations are read
	res := stacks.Check(testsupport.Context(t), stacks.CheckInput{Name: "decls", Target: target, Copy: stacks.WorkingCopy{Files: all}})
	if res.Loaded == nil {
		t.Fatalf("check: %v", res.Problems)
	}
	return stacks.IngressDeclarations(res.Loaded.Project, target)
}

// newStack stores a stack of the environment with compose as its draft.
func (f *claimFix) newStack(t *testing.T, name string, target stacks.Target, compose string, files ...stacks.File) stackops.Stack {
	t.Helper()
	all := append([]stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, files...)
	st, _, err := f.repo.CreateStack(testsupport.Context(t), f.rig.envID, name, target, "user:test", stacks.WorkingCopy{Files: all})
	if err != nil {
		t.Fatalf("CreateStack %s: %v", name, err)
	}
	return st
}

// makeGood records the stack's draft as a revision that ended good, and the claims its deploy would have stored.
func (f *claimFix) makeGood(t *testing.T, st stackops.Stack) {
	t.Helper()
	ctx := testsupport.Context(t)
	snap, err := f.repo.NewRevision(ctx, st.ID, "user:test", stackops.OriginDeploy)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if err := f.repo.SetOutcome(ctx, snap.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	f.claim(t, st, declsOf(t, string(snap.Copy.Files[0].Content), st.Target))
}

// claim stores decls as the claims of the stack, as an earlier deploy would have.
func (f *claimFix) claim(t *testing.T, st stackops.Stack, decls []ingress.Declaration) {
	t.Helper()
	if _, err := f.ing.ReplaceStackRoutes(testsupport.Context(t), ingressstore.ReplaceInput{
		EnvID: f.rig.envID, StackID: st.ID, Actor: "user:test", Decls: decls,
	}); err != nil {
		t.Fatalf("claim for %s: %v", st.Name, err)
	}
}

// write is the ClaimWrite of a deploy of compose for st, with the last good revision read as the API reads it.
func (f *claimFix) write(t *testing.T, st stackops.Stack, compose string, files ...stacks.File) *stackops.ClaimWrite {
	t.Helper()
	restore, hasGood, err := stackops.LastGoodDeclarations(testsupport.Context(t), f.repo, st)
	if err != nil {
		t.Fatalf("LastGoodDeclarations: %v", err)
	}
	return &stackops.ClaimWrite{
		Decls: declsOf(t, compose, st.Target, files...), Restore: restore, HasGood: hasGood, Actor: "user:test",
	}
}

// swarmInput is the input of a deploy of compose as st that writes claims.
func (f *claimFix) swarmInput(t *testing.T, st stackops.Stack, compose string, files ...stacks.File) stackops.SwarmDeployInput {
	t.Helper()
	_, in := stackInput(t, st.Name, compose, files)
	in.Stack = st
	in.Ingress = f.write(t, st, compose, files...)
	return in
}

// held are the hostnames the stack holds, sorted.
func (f *claimFix) held(t *testing.T, st stackops.Stack) []string {
	t.Helper()
	all, err := f.ing.InstallClaims(testsupport.Context(t))
	if err != nil {
		t.Fatalf("InstallClaims: %v", err)
	}
	out := []string{}
	for _, c := range all {
		if c.Claim.StackID == st.ID {
			out = append(out, c.Claim.Hostname)
		}
	}
	slices.Sort(out)
	return out
}

func wantHeld(t *testing.T, f *claimFix, st stackops.Stack, want ...string) {
	t.Helper()
	if got := f.held(t, st); !slices.Equal(got, want) {
		t.Errorf("stack %s holds %v, want %v", st.Name, got, want)
	}
}

func TestRefusedClaimFailsDeployAndChangesNothing(t *testing.T) {
	t.Parallel()
	f := newClaimFix(t, false, nil)
	holder := f.newStack(t, "holder", stacks.TargetSwarm, routed("taken.example.com"))
	f.claim(t, holder, declsOf(t, routed("taken.example.com"), stacks.TargetSwarm))
	st := f.newStack(t, "web", stacks.TargetSwarm, routed("taken.example.com"))
	in := f.swarmInput(t, st, routed("taken.example.com"))

	res, job := f.rig.deploy(t, in)

	if res.OK() || res.Code != stackops.CodeIngressRefused || res.FailedService != "" || res.Wrote {
		t.Fatalf("result = %+v, want ingress_refused with no service named and nothing written", res)
	}
	got := itemStates(job)
	if got[stackops.ItemCheck] != "failed:ingress_refused" || got[stackops.ItemLookup] != "skipped:not_run" || got[stackops.ItemServices] != "skipped:not_run" {
		t.Errorf("items = %v, want the check failed and every later item not run", got)
	}
	if w := f.rig.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a refused claim must leave the engine alone", w)
	}
	if c := itemByID(t, job, stackops.ItemCheck); !strings.Contains(c.Message, stackops.CodeMessage(stackops.CodeIngressRefused)) || len(c.Message) <= len(stackops.CodeMessage(stackops.CodeIngressRefused))+1 {
		t.Errorf("check message = %q, want the sentence of the code and the reason after it", c.Message)
	}
	wantHeld(t, f, st)
	wantHeld(t, f, holder, "taken.example.com")
}

func TestCertificateNotReadyNeverFailsDeploy(t *testing.T) {
	t.Parallel()
	// example.com is registered with no certificate issued and no DNS token: nothing about certificates is read.
	f := newClaimFix(t, false, nil)
	st := f.newStack(t, "web", stacks.TargetSwarm, routed("app.example.com"))

	res, job := f.rig.deploy(t, f.swarmInput(t, st, routed("app.example.com")))

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result = %+v, job %s %v, want a deploy that succeeds with no certificate", res, job.State, itemStates(job))
	}
	wantHeld(t, f, st, "app.example.com")
}

// claimSteps is every way a deploy of the stack's second revision can end badly. The stack's good revision declares
// a.example.com and the second revision b.example.com.
var claimSteps = []struct {
	name string
	// step is the item that is expected to fail.
	step    string
	tune    func(*dockertest.FakeConfig)
	compose string
	files   []stacks.File
}{
	{
		name: "lookup", step: stackops.ItemLookup,
		tune: func(c *dockertest.FakeConfig) {
			c.PullStatus = dockertest.FakePullStatus{Code: 404, Message: "manifest unknown"}
		},
		compose: strings.Replace(routed("b.example.com"), "nginx:1.27-alpine", "nginx:1.27", 1),
	},
	{
		name: "networks", step: stackops.ItemNetworks,
		tune: func(c *dockertest.FakeConfig) {
			c.SwarmClusterID = "cluster-1"
			c.SwarmUnallocated = map[string]int{"10.70.0.0/24": -1}
		},
		compose: "services:\n  web:\n    image: nginx:1.27-alpine\n    networks: [back]\n    deploy:\n      labels:\n        tierplan.ingress.host: b.example.com\n        tierplan.ingress.port: \"80\"\nnetworks:\n  back:\n    ipam:\n      config:\n        - subnet: 10.70.0.0/24\n",
	},
	{
		name: "objects", step: stackops.ItemObjects,
		tune:    func(c *dockertest.FakeConfig) { c.WriteError = "engine said no" },
		compose: "services:\n  web:\n    image: nginx:1.27-alpine\n    secrets:\n      - tok\n    deploy:\n      labels:\n        tierplan.ingress.host: b.example.com\n        tierplan.ingress.port: \"80\"\nsecrets:\n  tok:\n    file: tok.txt\n",
		files:   tokFile("s3cret"),
	},
	{
		name: "services", step: stackops.ItemServices,
		tune:    func(c *dockertest.FakeConfig) { c.ServiceCreateError = "the swarm rejected the service" },
		compose: routed("b.example.com"),
	},
}

func TestClaimsFollowLastGood(t *testing.T) {
	t.Parallel()
	for _, tc := range claimSteps {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newClaimFix(t, false, tc.tune)
			st := f.newStack(t, "web", stacks.TargetSwarm, routed("a.example.com"))
			f.makeGood(t, st)
			wantHeld(t, f, st, "a.example.com")
			in := f.swarmInput(t, st, tc.compose, tc.files...)

			res, job := f.rig.deploy(t, in)

			if res.OK() {
				t.Fatalf("result = %+v, want the %s step to fail", res, tc.step)
			}
			if got := itemStates(job)[tc.step]; !strings.HasPrefix(got, "failed:") {
				t.Fatalf("items = %v, want %s failed", itemStates(job), tc.step)
			}
			wantHeld(t, f, st, "a.example.com")
		})
	}

	t.Run("cleanup", func(t *testing.T) {
		t.Parallel()
		f := newClaimFix(t, false, nil)
		two := routed("a.example.com") + "  old:\n    image: busybox:1.36\n"
		st := f.newStack(t, "web", stacks.TargetSwarm, two)
		// The good revision ran both services; the claim is what its deploy stored.
		plain := f.swarmInput(t, st, two)
		plain.Ingress = nil
		if res, _ := f.rig.deploy(t, plain); !res.OK() {
			t.Fatalf("first deploy = %+v", res)
		}
		f.makeGood(t, st)
		f.rig.fake.SetServiceRemoveError("web_old", "the manager cannot remove it")
		in := f.swarmInput(t, st, routed("b.example.com"))

		res, job := f.rig.deploy(t, in)

		if res.OK() || !strings.HasPrefix(itemStates(job)[stackops.ItemCleanup], "failed:") {
			t.Fatalf("result = %+v, items = %v, want the cleanup step to fail", res, itemStates(job))
		}
		wantHeld(t, f, st, "a.example.com")
	})

	t.Run("converge_timeout", func(t *testing.T) {
		t.Parallel()
		f := newClaimFix(t, false, nil)
		clk := newStepClock()
		f.rig.deps.Clock, f.rig.deps.ConvergeTimeout = clk, time.Minute
		f.rig.fake.SetTaskState("web_web", "rejected", "No such image: nginx:1.27-alpine")
		st := f.newStack(t, "web", stacks.TargetSwarm, routed("a.example.com"))
		f.makeGood(t, st)
		d := f.rig.startDeploy(t, f.swarmInput(t, st, routed("b.example.com")))
		for range 30 {
			clk.Tick(t, stackops.PollInterval)
		}

		res, _ := d.finish(t)

		if res.Code != stackops.CodeConvergeTimeout {
			t.Fatalf("result = %+v, want a converge timeout", res)
		}
		wantHeld(t, f, st, "a.example.com")
	})

	t.Run("cancel", func(t *testing.T) {
		t.Parallel()
		f := newClaimFix(t, false, nil)
		f.rig.deps.Clock, f.rig.deps.ConvergeTimeout = newStepClock(), time.Minute
		f.rig.fake.SetTaskState("web_web", "preparing", "")
		st := f.newStack(t, "web", stacks.TargetSwarm, routed("a.example.com"))
		f.makeGood(t, st)
		d := f.rig.startDeploy(t, f.swarmInput(t, st, routed("b.example.com")))
		d.until(t, "the service to wait for its tasks", func(items map[string]jobs.Item) bool {
			return items["service:web_web"].Unit == "tasks"
		})
		wantHeld(t, f, st, "b.example.com") // the deploy holds the new claim while it runs
		if err := f.rig.hub.Cancel(d.job.ID); err != nil {
			t.Fatal(err)
		}

		res, _ := d.finish(t)

		if res.Code != stackops.CodeCancelled {
			t.Fatalf("result = %+v, want cancelled", res)
		}
		wantHeld(t, f, st, "a.example.com")
	})
}

func TestClaimsReleasedOnFailedFirstDeploy(t *testing.T) {
	t.Parallel()
	f := newClaimFix(t, false, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "the swarm rejected the service" })
	st := f.newStack(t, "web", stacks.TargetSwarm, routed("b.example.com"))
	in := f.swarmInput(t, st, routed("b.example.com"))
	if in.Ingress.HasGood {
		t.Fatal("the stack has no good revision yet")
	}

	res, _ := f.rig.deploy(t, in)

	if res.OK() || res.Code != stackops.CodeServiceRejected {
		t.Fatalf("result = %+v, want the service rejected", res)
	}
	wantHeld(t, f, st)
}

func TestFailedRevisionsHostnameIsFreeAgain(t *testing.T) {
	t.Parallel()
	f := newClaimFix(t, false, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "the swarm rejected the service" })
	st := f.newStack(t, "web", stacks.TargetSwarm, routed("a.example.com"))
	f.makeGood(t, st)
	other := f.newStack(t, "other", stacks.TargetSwarm, routed("b.example.com"))

	if res, _ := f.rig.deploy(t, f.swarmInput(t, st, routed("b.example.com"))); res.OK() {
		t.Fatalf("result = %+v, want the deploy to fail", res)
	}

	// Right after the failure the second stack may claim what only the failed revision asked for.
	f.claim(t, other, declsOf(t, routed("b.example.com"), stacks.TargetSwarm))
	wantHeld(t, f, other, "b.example.com")
	wantHeld(t, f, st, "a.example.com")
}

func TestRestoreRefusedFallsBackToRelease(t *testing.T) {
	t.Parallel()
	f := newClaimFix(t, false, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "the swarm rejected the service" })
	st := f.newStack(t, "web", stacks.TargetSwarm, routed("a.example.com"))
	f.makeGood(t, st)
	other := f.newStack(t, "other", stacks.TargetSwarm, routed("a.example.com"))
	// The deploy's own write frees a.example.com; another stack takes it before the deploy ends.
	f.ops.mu.Lock()
	f.ops.stackID = st.ID
	f.ops.after = func() { f.claim(t, other, declsOf(t, routed("a.example.com"), stacks.TargetSwarm)) }
	f.ops.mu.Unlock()

	res, _ := f.rig.deploy(t, f.swarmInput(t, st, routed("b.example.com")))

	if res.OK() {
		t.Fatalf("result = %+v, want the deploy to fail", res)
	}
	wantHeld(t, f, st)
	wantHeld(t, f, other, "a.example.com")
	if !strings.Contains(f.logs.String(), "released") {
		t.Errorf("log = %q, want a warning that the claims were released", f.logs.String())
	}
}

func TestLastGoodDeclarations(t *testing.T) {
	t.Parallel()
	f := newClaimFix(t, false, nil)
	st := f.newStack(t, "web", stacks.TargetSwarm, routed("a.example.com"))

	decls, ok, err := stackops.LastGoodDeclarations(testsupport.Context(t), f.repo, st)
	if err != nil || ok || decls != nil {
		t.Fatalf("with no good revision: %v, %v, %v, want nil, false, nil", decls, ok, err)
	}

	f.makeGood(t, st)
	decls, ok, err = stackops.LastGoodDeclarations(testsupport.Context(t), f.repo, st)
	if err != nil || !ok || len(decls) != 1 || decls[0].Host != "a.example.com" || decls[0].Service != "web" {
		t.Fatalf("with a good revision: %v, %v, %v, want the one route of a.example.com", decls, ok, err)
	}
}

func TestStandaloneClaimsFollowLastGood(t *testing.T) {
	t.Parallel()

	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		f := newClaimFix(t, true, nil)
		stand := &upStandIn{}
		f.rig.deps.Up = stand.up
		holder := f.newStack(t, "holder", stacks.TargetStandalone, routedStandalone("taken.example.com"))
		f.claim(t, holder, declsOf(t, routedStandalone("taken.example.com"), stacks.TargetStandalone))
		st := f.newStack(t, "web", stacks.TargetStandalone, routedStandalone("taken.example.com"))
		in := standaloneInput("web", routedStandalone("taken.example.com"))
		in.Stack = st
		in.Ingress = f.write(t, st, routedStandalone("taken.example.com"))

		res, job := f.rig.deployStandalone(t, in)

		if res.OK() || res.Code != stackops.CodeIngressRefused || res.Wrote {
			t.Fatalf("result = %+v, want ingress_refused with nothing written", res)
		}
		if got := itemStates(job); got[stackops.ItemCheck] != "failed:ingress_refused" || got[stackops.ItemServices] != "skipped:not_run" {
			t.Errorf("items = %v, want the check failed and the rest not run", got)
		}
		if stand.calls != 0 || len(f.rig.fake.Writes()) != 0 {
			t.Errorf("SDK calls %d, writes %v, want nothing sent to the host", stand.calls, f.rig.fake.Writes())
		}
		wantHeld(t, f, st)
	})

	t.Run("failure after the write restores the last good claims", func(t *testing.T) {
		t.Parallel()
		f := newClaimFix(t, true, nil)
		stand := &upStandIn{err: errors.New("compose up failed")}
		f.rig.deps.Up = stand.up
		st := f.newStack(t, "web", stacks.TargetStandalone, routedStandalone("a.example.com"))
		f.makeGood(t, st)
		in := standaloneInput("web", routedStandalone("b.example.com"))
		in.Stack = st
		in.Ingress = f.write(t, st, routedStandalone("b.example.com"))

		res, _ := f.rig.deployStandalone(t, in)

		if res.OK() || stand.calls != 1 {
			t.Fatalf("result = %+v after %d SDK calls, want the SDK to run and fail", res, stand.calls)
		}
		wantHeld(t, f, st, "a.example.com")
	})

	t.Run("a first deploy that fails releases every claim", func(t *testing.T) {
		t.Parallel()
		f := newClaimFix(t, true, nil)
		stand := &upStandIn{err: errors.New("compose up failed")}
		f.rig.deps.Up = stand.up
		st := f.newStack(t, "web", stacks.TargetStandalone, routedStandalone("b.example.com"))
		in := standaloneInput("web", routedStandalone("b.example.com"))
		in.Stack = st
		in.Ingress = f.write(t, st, routedStandalone("b.example.com"))

		if res, _ := f.rig.deployStandalone(t, in); res.OK() {
			t.Fatalf("result = %+v, want the deploy to fail", res)
		}
		wantHeld(t, f, st)
	})

	t.Run("nil writes and reverts nothing", func(t *testing.T) {
		t.Parallel()
		f := newClaimFix(t, true, nil)
		stand := &upStandIn{err: errors.New("compose up failed")}
		f.rig.deps.Up = stand.up
		st := f.newStack(t, "web", stacks.TargetStandalone, routedStandalone("a.example.com"))
		f.makeGood(t, st)
		in := standaloneInput("web", routedStandalone("b.example.com"))
		in.Stack = st

		if res, _ := f.rig.deployStandalone(t, in); res.OK() {
			t.Fatalf("result = %+v, want the deploy to fail", res)
		}
		wantHeld(t, f, st, "a.example.com")

		stand.err = nil
		if res, _ := f.rig.deployStandalone(t, in); !res.OK() {
			t.Fatalf("result = %+v, want the deploy to succeed", res)
		}
		wantHeld(t, f, st, "a.example.com") // the claim is only ever written by a deploy that asks for it
	})
}

func TestSecondClaimOfAHostnameIsRefused(t *testing.T) {
	t.Parallel()
	f := newClaimFix(t, true, nil)
	stand := &upStandIn{}
	f.rig.deps.Up = stand.up
	compose := routedStandalone("c.example.com")
	first := f.newStack(t, "first", stacks.TargetStandalone, compose)
	second := f.newStack(t, "second", stacks.TargetStandalone, compose)
	// Both were reviewed while c.example.com was free: the policy the review read said yes to each.
	inFirst, inSecond := standaloneInput("first", compose), standaloneInput("second", compose)
	inFirst.Stack, inSecond.Stack = first, second
	inFirst.Ingress, inSecond.Ingress = f.write(t, first, compose), f.write(t, second, compose)

	if res, _ := f.rig.deployStandalone(t, inFirst); !res.OK() {
		t.Fatalf("first deploy = %+v, want it to claim the name", res)
	}
	calls := stand.calls
	res, job := f.rig.deployStandalone(t, inSecond)

	if res.OK() || res.Code != stackops.CodeIngressRefused {
		t.Fatalf("second deploy = %+v, want ingress_refused", res)
	}
	if got := itemStates(job)[stackops.ItemCheck]; got != "failed:ingress_refused" {
		t.Errorf("check item = %q, want failed:ingress_refused", got)
	}
	if stand.calls != calls {
		t.Errorf("SDK calls = %d, want the loser to write nothing to the host", stand.calls-calls)
	}
	wantHeld(t, f, first, "c.example.com")
	wantHeld(t, f, second)
}

func TestUnchangedClaimsNotifyNobody(t *testing.T) {
	t.Parallel()
	f := newClaimFix(t, true, nil)
	stand := &upStandIn{}
	f.rig.deps.Up = stand.up
	compose := routedStandalone("a.example.com")
	st := f.newStack(t, "web", stacks.TargetStandalone, compose)
	in := standaloneInput("web", compose)
	in.Stack = st
	in.Ingress = f.write(t, st, compose)

	if res, _ := f.rig.deployStandalone(t, in); !res.OK() {
		t.Fatalf("first deploy = %+v", res)
	}
	versionOf := func() int64 {
		all, err := f.ing.Versions(testsupport.Context(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range all {
			if v.EnvID == f.rig.envID {
				return v.ConfigVersion
			}
		}
		t.Fatal("the environment has no ingress version")
		return 0
	}
	before := versionOf()
	sub := f.notifier.Subscribe()
	defer sub.Close()

	if res, _ := f.rig.deployStandalone(t, in); !res.OK() {
		t.Fatalf("second deploy = %+v", res)
	}

	if after := versionOf(); after != before {
		t.Errorf("config_version = %d after a deploy that changed nothing, want %d", after, before)
	}
	if pending := sub.Pending(); len(pending) != 0 {
		t.Errorf("notified %v, want nobody told", pending)
	}
}
