// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// Pulling a git stack and deploying what was pulled (plan 09-19, GIT-04, D-17, D-21): a pull refreshes the draft and
// deploys nothing; the review and the deploy are the ones every stack has, and afterwards the stack reads in sync.

type pullAnswer struct {
	DraftVersion   int64  `json:"draftVersion"`
	Commit         string `json:"commit"`
	Ref            string `json:"ref"`
	DeployedCommit string `json:"deployedCommit"`
	Touched        int    `json:"touched"`
}

// gitWebAndDb has two services, so a change to one image can be told from the other staying put.
const gitWebAndDb = "services:\n  web:\n    image: nginx:%s\n    env_file: web.env\n  db:\n    image: postgres:16\n"

func gitCompose(webTag string) string { return strings.Replace(gitWebAndDb, "%s", webTag, 1) }

// deployedGitStack makes a git stack whose folder holds two services, deploys it as an Operator and waits for the job.
// The repository's first commit is then the deployed one.
func deployedGitStack(t *testing.T) (*gitStackFix, *roleCaller, string) {
	t.Helper()
	g := newGitStackFixWith(t, nil)
	g.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": gitCompose("1.27")}, "two services")
	rec := g.create(t, g.admin, g.body(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	made := answerOf[gitStackAnswer](t, rec)
	operator := g.f.caller(t, g.e, "operator-1", "operator")
	job := g.f.deployAndWait(t, operator, made.StackID, `{"draftVersion":`+itoaT(made.DraftVersion)+`}`)
	if job.State != "done" {
		t.Fatalf("first deploy = %+v, want done", job)
	}
	return g, operator, made.StackID
}

func (c *roleCaller) pullGit(stackID string, version int64) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.do(http.MethodPost, "stacks/"+stackID+"/git/pull", `{"draftVersion":`+itoaT(version)+`}`)
}

func TestPullReviewDeployInSync(t *testing.T) {
	t.Parallel()
	g, operator, stackID := deployedGitStack(t)
	ctx := context.Background()
	repo := g.stackRepo()

	deployed, ok, err := repo.DeployedGit(ctx, stackID)
	if err != nil || !ok || deployed.Commit == "" {
		t.Fatalf("DeployedGit after the first deploy = %+v, %v, %v, want the revision to record its commit", deployed, ok, err)
	}
	if got := g.readGit(t, g.admin, stackID); got.Status != "in_sync" || got.DeployedSha != deployed.Commit || got.HeadSha != deployed.Commit {
		t.Fatalf("git after the first deploy = %+v, want in sync at %s", got, deployed.Commit)
	}

	// The repository moves: web gets a newer image, db is untouched, and a commit elsewhere comes between.
	g.repo.Commit(t, map[string]string{"stacks/other/compose.yml": "services: {}\n# x\n"}, "elsewhere")
	head := g.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": gitCompose("1.28")}, "newer web")

	version := draftVersion(t, operator, stackID)
	revsBefore, _, err := repo.ListRevisions(ctx, stackID)
	if err != nil {
		t.Fatal(err)
	}
	rec := operator.pullGit(stackID, version)
	if rec.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body)
	}
	pulled := answerOf[pullAnswer](t, rec)
	if pulled.Commit != head || pulled.Ref != "main" || pulled.DeployedCommit != deployed.Commit || pulled.Touched != 1 || pulled.DraftVersion != version+1 {
		t.Fatalf("pull = %+v, want the head %s, 1 commit that touched the folder, version %d", pulled, head, version+1)
	}
	// A pull deploys nothing: no revision, no engine write.
	if revs, _, err := repo.ListRevisions(ctx, stackID); err != nil || len(revs) != len(revsBefore) {
		t.Errorf("revisions = %d, %v, want the %d there were", len(revs), err, len(revsBefore))
	}
	if st := g.readGit(t, g.admin, stackID); st.Status != "behind" || st.HeadSha != head || st.DeployedSha != deployed.Commit {
		t.Errorf("git after the pull = %+v, want behind at %s", st, head)
	}

	// The review is the normal one: it lists the one service that changed.
	rv, code, raw := operator.reviewStack(stackID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if rv.DraftVersion != pulled.DraftVersion || len(rv.Services) != 1 || rv.Services[0].Short != "web" || rv.Services[0].Change != "changed" {
		t.Fatalf("review services = %+v at draft %d, want only web changed", rv.Services, rv.DraftVersion)
	}

	// The deploy is the normal one.
	job := g.f.deployAndWait(t, operator, stackID, `{"draftVersion":`+itoaT(rv.DraftVersion)+`,"pins":`+pinsJSON(t, rv.Pins)+`}`)
	if job.State != "done" {
		t.Fatalf("deploy = %+v, want done", job)
	}

	// The revision made records the commit, the folder's tree and the source.
	good, ok, err := repo.LatestGoodRevision(ctx, stackID)
	if err != nil || !ok {
		t.Fatalf("LatestGoodRevision = %v, %v", ok, err)
	}
	if good.Git.Commit != head || good.Git.Tree == "" || good.Git.Tree == deployed.Tree || good.Git.Source.Folder != "stacks/web" || good.Git.Source.Ref != "main" {
		t.Errorf("revision git = %+v, want the pulled commit %s, a new tree and the source", good.Git, head)
	}
	// The deploy audit carries the commit.
	var carried bool
	for _, a := range g.f.stackAudits(t) {
		if a.Action == audit.ActionStackDeployed && a.Details["commit"] == head {
			carried = true
		}
	}
	if !carried {
		t.Errorf("no stack.deployed audit row carries the commit %s", head)
	}

	// The stack reads in sync with the new commit deployed.
	if got := g.readGit(t, g.admin, stackID); got.Status != "in_sync" || got.DeployedSha != head || got.HeadSha != head || got.Behind != 0 {
		t.Errorf("git after the deploy = %+v, want in sync with %s deployed", got, head)
	}
}

// reviewGit is the git part of a review, as the review answers it.
type reviewGit struct {
	Commit         string `json:"commit"`
	Ref            string `json:"ref"`
	RefKind        string `json:"refKind"`
	Folder         string `json:"folder"`
	RepoName       string `json:"repoName"`
	DeployedCommit string `json:"deployedCommit"`
	Touched        int    `json:"touched"`
	SourceChanged  bool   `json:"sourceChanged"`
}

func (c *roleCaller) reviewGit(stackID string) (*reviewGit, stackReviewAnswer) {
	c.t.Helper()
	rec := c.do(http.MethodPost, "stacks/"+stackID+"/review", `{}`)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("review: %d %s", rec.Code, rec.Body)
	}
	var wrap struct {
		Git *reviewGit `json:"git"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wrap); err != nil {
		c.t.Fatal(err)
	}
	return wrap.Git, answerOf[stackReviewAnswer](c.t, rec)
}

func TestReviewGitFields(t *testing.T) {
	t.Parallel()
	g, operator, stackID := deployedGitStack(t)
	repo := g.stackRepo()
	deployed, _, err := repo.DeployedGit(context.Background(), stackID)
	if err != nil {
		t.Fatal(err)
	}

	// Right after the first deploy the draft is the deployed commit: nothing is behind it.
	got, _ := operator.reviewGit(stackID)
	want := reviewGit{
		Commit: deployed.Commit, Ref: "main", RefKind: "branch", Folder: "stacks/web", RepoName: "acme",
		DeployedCommit: deployed.Commit, Touched: 0, SourceChanged: false,
	}
	if got == nil || *got != want {
		t.Fatalf("review git = %+v, want %+v", got, want)
	}

	// Two commits change the folder, one between them does not.
	g.repo.Commit(t, map[string]string{"stacks/web/web.env": "A=c\n"}, "env")
	g.repo.Commit(t, map[string]string{"stacks/other/compose.yml": "services: {}\n# y\n"}, "elsewhere")
	head := g.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": gitCompose("1.28")}, "newer web")
	if rec := operator.pullGit(stackID, draftVersion(t, operator, stackID)); rec.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body)
	}
	got, _ = operator.reviewGit(stackID)
	want.Commit, want.Touched = head, 2
	if got == nil || *got != want {
		t.Fatalf("review git after a pull = %+v, want %+v", got, want)
	}

	// The follow settings changed since the deploy: the review says the source is not the deployed one.
	sg, err := repo.StackGit(context.Background(), stackID)
	if err != nil {
		t.Fatal(err)
	}
	moved := stackops.GitSource{RepoID: sg.RepoID, RepoName: "acme", RefKind: "branch", Ref: "release", Folder: "stacks/web", ComposeFiles: sg.ComposeFiles}
	if err := repo.SetAppliedSource(context.Background(), stackID, moved); err != nil {
		t.Fatal(err)
	}
	g.repo.Branch(t, "release", head)
	if rec := operator.pullGit(stackID, draftVersion(t, operator, stackID)); rec.Code != http.StatusOK {
		t.Fatalf("pull of the new ref: %d %s", rec.Code, rec.Body)
	}
	got, _ = operator.reviewGit(stackID)
	if got == nil || !got.SourceChanged || got.Ref != "release" || got.Touched != 0 || got.DeployedCommit != deployed.Commit {
		t.Errorf("review git after the source changed = %+v, want sourceChanged with ref release and no count", got)
	}
}

func TestReviewOfAnInlineStackHasNoGit(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "plain")
	rec := admin.do(http.MethodPost, "stacks/"+st.ID+"/review", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"git"`) {
		t.Errorf("the review of an inline stack carries git: %s", rec.Body)
	}
}

func TestGitChangePinsOnlyChangedImages(t *testing.T) {
	t.Parallel()
	g, operator, stackID := deployedGitStack(t)
	ctx := context.Background()
	repo := g.stackRepo()
	f := g.f

	dbDigest := dockertest.FakeDigestOf("docker.io/library/postgres:16")
	// The tag the database runs moves on the registry; nothing in the commit asks for it.
	f.fake.SetDistributionDigest("docker.io/library/postgres:16", "sha256:"+strings.Repeat("c", 64))

	// The commit changes the text of one image reference only.
	g.repo.Commit(t, map[string]string{"stacks/web/compose.yaml": gitCompose("1.28")}, "newer web")
	if rec := operator.pullGit(stackID, draftVersion(t, operator, stackID)); rec.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body)
	}
	lookups := len(f.fake.DistributionRefs())
	_, rv := operator.reviewGit(stackID)
	for _, ref := range f.fake.DistributionRefs()[lookups:] {
		if strings.Contains(ref, "postgres") {
			t.Errorf("the review looked up %s, an image the commit did not change", ref)
		}
	}
	byService := map[string]struct{ State, Digest string }{}
	for _, im := range rv.Images {
		byService[im.Service] = struct{ State, Digest string }{im.State, im.Digest}
	}
	web, db := byService["web"], byService["db"]
	if web.State != "new" || web.Digest != dockertest.FakeDigestOf("docker.io/library/nginx:1.28") {
		t.Errorf("web image = %+v, want a fresh lookup of nginx:1.28", web)
	}
	if db.State != "pinned" || db.Digest != dbDigest {
		t.Errorf("db image = %+v, want it pinned to the running %s although its tag moved", db, dbDigest)
	}

	// The deploy records exactly those pins.
	job := f.deployAndWait(t, operator, stackID, `{"draftVersion":`+itoaT(rv.DraftVersion)+`,"pins":`+pinsJSON(t, rv.Pins)+`}`)
	if job.State != "done" {
		t.Fatalf("deploy = %+v, want done", job)
	}
	good, ok, err := repo.LatestGoodRevision(ctx, stackID)
	if err != nil || !ok {
		t.Fatalf("LatestGoodRevision = %v, %v", ok, err)
	}
	pins, err := repo.RevisionPins(ctx, good.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(pins["web_db"], "@"+dbDigest) || !strings.HasSuffix(pins["web_web"], "@"+dockertest.FakeDigestOf("docker.io/library/nginx:1.28")) {
		t.Errorf("revision pins = %v, want db kept and web looked up", pins)
	}
}
