// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Accept live into the draft (plan 08-24, STK-14, D-23, D-54): a stack deployed from a revision, changed outside
// TierPlan, becomes the stack's next draft on top of that revision.

const acceptCompose = `services:
  web:
    image: nginx:${TAG}
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
      MODE: prod
    secrets:
      - db_password
    configs:
      - site
    deploy:
      replicas: 2
secrets:
  db_password:
    file: ./secrets/db_password
configs:
  site:
    file: ./site.conf
`

// acceptRig is a stack stored with a good revision and deployed to the fake swarm from it.
type acceptRig struct {
	*swarmRig
	repo stackops.Repo
	st   stackops.Stack
	rev  stackops.Snapshot
	copy stacks.WorkingCopy
}

func newAcceptRig(t *testing.T) *acceptRig {
	t.Helper()
	rig := newSwarmRig(t, nil)
	v, err := vault.New(bytes.Repeat([]byte{9}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	repo := stackops.Repo{Store: rig.store, Vault: v, Now: time.Now}
	copy := stacks.WorkingCopy{
		Files: []stacks.File{
			{Path: stacks.ComposeFile, Content: []byte(acceptCompose)},
			{Path: "secrets/db_password", Content: []byte("hunter2"), Secret: true},
			{Path: "site.conf", Content: []byte("hello\n")},
		},
		Vars: []stacks.Var{{Name: "TAG", Value: "1.27-alpine"}, {Name: "DB_PASSWORD", Value: "s3cr3t", Secret: true}},
	}
	ctx := testsupport.Context(t)
	st, _, err := repo.CreateStack(ctx, rig.envID, "shop", stacks.TargetSwarm, "ada", copy)
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: st.Name, Target: st.Target, Copy: copy})
	if err != nil {
		t.Fatalf("Deployable: %v", err)
	}
	values := map[string]string{}
	for _, v := range copy.Vars {
		values[v.Name] = v.Value
	}
	res, job := rig.deploy(t, stackops.SwarmDeployInput{
		Stack: st, Project: loaded.Project, Files: stacks.NewFileSet(copy.Files), Vars: values, Copy: copy,
	})
	if !res.OK() {
		t.Fatalf("deploy = %+v %v", res, itemStates(job))
	}
	rev := goodRevision(t, repoFixture{repo: repo, envID: rig.envID}, st.ID, "ada")
	return &acceptRig{swarmRig: rig, repo: repo, st: st, rev: rev, copy: copy}
}

// scaleLive scales the deployed service outside TierPlan, as docker service scale does.
func (r *acceptRig) scaleLive(t *testing.T, replicas uint64) {
	t.Helper()
	svc, ok := r.ownedNow(t, r.st)["shop_web"]
	if !ok {
		t.Fatal("the swarm holds no service shop_web")
	}
	var spec swarm.ServiceSpec
	mustUnmarshal(t, svc.Spec, &spec)
	spec.Mode.Replicated.Replicas = &replicas
	err := r.deps.Aggregator.WriteOnManager(testsupport.Context(t), r.envID, func(c *dockerx.Client) error {
		_, err := c.ServiceUpdate(context.Background(), svc.ID, svc.Version, mustJSON(t, spec), "")
		return err
	})
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
}

func (r *acceptRig) accept(t *testing.T, version int64) (stackops.AcceptLiveResult, error) {
	t.Helper()
	return r.deps.AcceptLive(testsupport.Context(t), r.repo, stackops.AcceptLiveInput{
		EnvID: r.envID, Stack: r.st, DraftVersion: version, Caller: stackops.Caller{Actor: "bob"},
	})
}

func TestAcceptLiveRebuildsTheDraft(t *testing.T) {
	t.Parallel()
	rig := newAcceptRig(t)
	rig.scaleLive(t, 3)
	ctx := testsupport.Context(t)
	before, err := rig.repo.Draft(ctx, rig.st.ID)
	if err != nil {
		t.Fatal(err)
	}

	res, err := rig.accept(t, before.Version)
	if err != nil {
		t.Fatalf("AcceptLive: %v", err)
	}
	saved := res.Draft
	if res.Revision != rig.rev.Seq || res.Services != 1 {
		t.Errorf("result = revision %d, %d services; want revision %d and 1 service", res.Revision, res.Services, rig.rev.Seq)
	}

	draft, err := rig.repo.Draft(ctx, rig.st.ID)
	if err != nil {
		t.Fatal(err)
	}
	compose := string(draft.Copy.Compose())
	if !strings.Contains(compose, "replicas: 3") {
		t.Errorf("the draft lacks what runs (3 replicas):\n%s", compose)
	}
	if strings.Contains(compose, "networks") {
		t.Errorf("the plain default network the revision never declared is in the draft:\n%s", compose)
	}
	if !strings.Contains(compose, "image: nginx:${TAG}\n") {
		t.Errorf("the image lost the text the revision wrote it with:\n%s", compose)
	}
	if draft.Version != before.Version+1 || draft.Version != saved.Version || draft.CreatedBy != "bob" {
		t.Errorf("draft = version %d by %q, saved = version %d; want one more than %d, by bob", draft.Version, draft.CreatedBy, saved.Version, before.Version)
	}
	if draft.BaseSeq != rig.rev.Seq || saved.BaseSeq != rig.rev.Seq {
		t.Errorf("base_seq = %d (saved %d), want the deployed revision's %d", draft.BaseSeq, saved.BaseSeq, rig.rev.Seq)
	}
	// The files and the variables are the revision's own, compose.yaml apart.
	for _, f := range rig.copy.Files[1:] {
		i := slices.IndexFunc(draft.Copy.Files, func(g stacks.File) bool { return g.Path == f.Path })
		if i < 0 || !bytes.Equal(draft.Copy.Files[i].Content, f.Content) || draft.Copy.Files[i].Secret != f.Secret {
			t.Errorf("file %s = %+v, want the revision's", f.Path, draft.Copy.Files)
		}
	}
	if len(draft.Copy.Vars) != len(rig.copy.Vars) {
		t.Fatalf("vars = %+v, want the revision's", draft.Copy.Vars)
	}
	for _, v := range rig.copy.Vars {
		if !slices.Contains(draft.Copy.Vars, v) {
			t.Errorf("vars = %+v, lack %+v", draft.Copy.Vars, v)
		}
	}
	// The rebuilt file passes the check every stack passes, with the revision's variables.
	if _, err := stacks.Deployable(ctx, stacks.CheckInput{Name: rig.st.Name, Target: rig.st.Target, Copy: draft.Copy}); err != nil {
		t.Errorf("the draft cannot be deployed: %v", err)
	}
}

func TestAcceptLiveKeepsSecretsAsVariables(t *testing.T) {
	t.Parallel()
	rig := newAcceptRig(t)
	rig.scaleLive(t, 3)

	if _, err := rig.accept(t, 1); err != nil {
		t.Fatalf("AcceptLive: %v", err)
	}

	draft, err := rig.repo.Draft(testsupport.Context(t), rig.st.ID)
	if err != nil {
		t.Fatal(err)
	}
	compose := string(draft.Copy.Compose())
	for _, secret := range []string{"s3cr3t", "hunter2"} {
		if strings.Contains(compose, secret) {
			t.Errorf("compose.yaml holds the secret %q:\n%s", secret, compose)
		}
	}
	for _, want := range []string{"DB_PASSWORD: ${DB_PASSWORD}", "file: ./secrets/db_password", "file: ./site.conf"} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose.yaml lacks %q:\n%s", want, compose)
		}
	}
	if strings.Contains(compose, "external") {
		t.Errorf("a secret or config the stack made became external:\n%s", compose)
	}
	// The variable stays sealed in the draft.
	i := slices.IndexFunc(draft.Copy.Vars, func(v stacks.Var) bool { return v.Name == "DB_PASSWORD" })
	if i < 0 || !draft.Copy.Vars[i].Secret || draft.Copy.Vars[i].Value != "s3cr3t" {
		t.Errorf("vars = %+v, want DB_PASSWORD still a secret with its value", draft.Copy.Vars)
	}
}

func TestAcceptLiveDraftConflict(t *testing.T) {
	t.Parallel()
	rig := newAcceptRig(t)
	rig.scaleLive(t, 3)
	ctx := testsupport.Context(t)
	before, err := rig.repo.Draft(ctx, rig.st.ID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = rig.accept(t, before.Version+5)

	var conflict *stackops.DraftConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("AcceptLive = %v, want a draft conflict", err)
	}
	after, err := rig.repo.Draft(ctx, rig.st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || !bytes.Equal(after.Copy.Compose(), before.Copy.Compose()) {
		t.Errorf("the draft changed on a conflict: version %d -> %d", before.Version, after.Version)
	}
}

func TestAcceptLiveRefusesAStackWithNoDeployedRevision(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	v, err := vault.New(bytes.Repeat([]byte{9}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	repo := stackops.Repo{Store: rig.store, Vault: v, Now: time.Now}
	st, _, err := repo.CreateStack(testsupport.Context(t), rig.envID, "fresh", stacks.TargetSwarm, "ada",
		stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte("services: {}\n")}}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = rig.deps.AcceptLive(testsupport.Context(t), repo, stackops.AcceptLiveInput{EnvID: rig.envID, Stack: st, Caller: stackops.Caller{Actor: "bob"}})

	if !errors.Is(err, stackops.ErrNoDeployedRevision) {
		t.Errorf("AcceptLive = %v, want ErrNoDeployedRevision", err)
	}
}

// writes are the requests that create, update or remove something on an engine.
var writes = regexp.MustCompile(`/(create|update|remove|prune|rename|disconnect|connect)(\?|$)|^/v[0-9.]+/[a-z]+/[^/]+/update`)

func TestAcceptLiveChangesNothingOnTheEngine(t *testing.T) {
	t.Parallel()
	rig := newAcceptRig(t)
	rig.scaleLive(t, 3)
	requests, updates, creates := len(rig.fake.Requests()), len(rig.fake.ServiceUpdates()), len(rig.fake.ServiceCreates())

	if _, err := rig.accept(t, 1); err != nil {
		t.Fatalf("AcceptLive: %v", err)
	}

	if got := len(rig.fake.ServiceUpdates()); got != updates {
		t.Errorf("service updates went from %d to %d", updates, got)
	}
	if got := len(rig.fake.ServiceCreates()); got != creates {
		t.Errorf("service creates went from %d to %d", creates, got)
	}
	read := rig.fake.Requests()[requests:]
	if len(read) == 0 {
		t.Fatal("the rebuild read nothing from the engine")
	}
	for _, req := range read {
		if writes.MatchString(req) || secretOrConfigRead.MatchString(req) {
			t.Errorf("the rebuild sent %s: it must only list the engine, and never read a secret or a config", req)
		}
	}
	live, ok := rig.ownedNow(t, rig.st)["shop_web"]
	if !ok {
		t.Fatal("the service is gone")
	}
	var spec swarm.ServiceSpec
	mustUnmarshal(t, live.Spec, &spec)
	if spec.Mode.Replicated == nil || spec.Mode.Replicated.Replicas == nil || *spec.Mode.Replicated.Replicas != 3 {
		t.Errorf("the service no longer runs 3 replicas: %+v", spec.Mode)
	}
}

// CR-C02: a stack that deploys from git has read-only repository files (D-11), so a definition rebuilt from what runs
// has no file to go in. Accept live is refused before anything is read or written.
func TestAcceptLiveRefusesAStackThatDeploysFromGit(t *testing.T) {
	t.Parallel()
	rig := newAcceptRig(t)
	rig.scaleLive(t, 5)
	st := rig.st
	st.Source = stacks.Source{Kind: stacks.SourceGit}
	before, err := rig.repo.Draft(testsupport.Context(t), st.ID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = rig.deps.AcceptLive(testsupport.Context(t), rig.repo, stackops.AcceptLiveInput{EnvID: rig.envID, Stack: st, Caller: stackops.Caller{Actor: "bob"}})

	if !errors.Is(err, stackops.ErrGitStack) {
		t.Fatalf("AcceptLive = %v, want ErrGitStack", err)
	}
	after, err := rig.repo.Draft(testsupport.Context(t), st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || after.IsDraft != before.IsDraft {
		t.Errorf("draft went from %+v to %+v: it was written", before.Version, after.Version)
	}
}

// A draft that points an inline stack at a repository holds repository files too, and accept live would replace it
// with a draft that has none.
func TestAcceptLiveRefusesAStackWithAPendingConversionToGit(t *testing.T) {
	t.Parallel()
	rig := newAcceptRig(t)
	ctx := testsupport.Context(t)
	repoFixture{repo: rig.repo, envID: rig.envID}.addGitRepo(t, gitRepoID, "acme")
	draft, err := rig.repo.Draft(ctx, rig.st.ID)
	if err != nil {
		t.Fatal(err)
	}
	rev := gitRevision()
	rev.Source.ComposeFiles = []string{"compose.yaml"}
	wc := stacks.WorkingCopy{
		Files:        []stacks.File{repoFile("compose.yaml", "services: {web: {image: nginx}}\n")},
		ComposeFiles: []string{"compose.yaml"},
	}
	saved, err := rig.repo.CreatePendingGitSource(ctx, rig.st.ID, draft.Version, "ada", wc, rev)
	if err != nil {
		t.Fatalf("CreatePendingGitSource: %v", err)
	}

	_, err = rig.accept(t, saved.Version)

	if !errors.Is(err, stackops.ErrGitStack) {
		t.Fatalf("AcceptLive = %v, want ErrGitStack", err)
	}
	after, err := rig.repo.Draft(ctx, rig.st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != saved.Version || after.Git.Commit != gitCommit {
		t.Errorf("draft = version %d at %q, want the pending conversion left as it was", after.Version, after.Git.Commit)
	}
}
