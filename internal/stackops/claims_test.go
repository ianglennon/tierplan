// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"bytes"
	"context"
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
