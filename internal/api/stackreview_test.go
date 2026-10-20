// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stackops/stackopstest"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// Reviewing a stack (plan 07-14, STK-06, D-11): what a deploy would change against what is
// running, and a deploy that is bound to what was reviewed (STK-05, STK-09). Docker is a fake
// swarm manager; the router, policy, registry resolver, job hub and audit writer are the real ones.

type stackReviewAnswer struct {
	Problems []struct {
		Severity, Code, Message, File string
		Line                          int
	} `json:"problems"`
	Summary struct {
		Services, Networks, Volumes, Secrets, Configs struct{ Added, Changed, Removed int }
		ImagesPinned                                  int `json:"imagesPinned"`
		ImagesNew                                     int `json:"imagesNew"`
	} `json:"summary"`
	Images []struct {
		Service, Image, Digest, Current, State, Note string
	} `json:"images"`
	Services []struct {
		Name, Short, Change string
		Current, Next       map[string]any
		HiddenChanges       []string `json:"hiddenChanges"`
		Settings            int
	} `json:"services"`
	Objects []struct {
		Change, Kind, Name, Detail string
	} `json:"objects"`
	DraftVersion int64             `json:"draftVersion"`
	Pins         map[string]string `json:"pins"`
	NeedsAdmin   []string          `json:"needsAdmin"`
	YAML         struct {
		ChangedLines int `json:"changedLines"`
		Hunks        []struct {
			OldStart, OldLines, NewStart, NewLines int
			Lines                                  []struct {
				Kind     string
				Old, New int
				Text     string
			}
		}
		Lines []string
	} `json:"yaml"`
	Concurrent *struct {
		Revision       int64
		DeployedBy     string           `json:"deployedBy"`
		DeployedAt     string           `json:"deployedAt"`
		BaseToDeployed []concurrentFile `json:"baseToDeployed"`
		BaseToDraft    []concurrentFile `json:"baseToDraft"`
	} `json:"concurrent"`
}

// concurrentFile is one file of a concurrent diff, as the review answers it.
type concurrentFile struct {
	Path, Kind   string
	Hidden       bool
	ChangedLines int `json:"changedLines"`
	Hunks        []struct {
		Lines []struct {
			Kind string
			Text string
		}
	}
	Lines []string
}

// added is the text of the lines the file's hunks add.
func (f concurrentFile) added() []string {
	var out []string
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind == "added" {
				out = append(out, strings.TrimSpace(l.Text))
			}
		}
	}
	return out
}

func (c *roleCaller) reviewStack(stackID, body string) (stackReviewAnswer, int, string) {
	c.t.Helper()
	rec := c.do(http.MethodPost, "stacks/"+stackID+"/review", body)
	var a stackReviewAnswer
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
			c.t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
	return a, rec.Code, rec.Body.String()
}

// saveCompose saves compose (and the variables) over the draft as c and returns the new version.
func saveCompose(t *testing.T, c *roleCaller, stackID, compose, vars string) int64 {
	t.Helper()
	v := draftVersion(t, c, stackID)
	if rec := c.saveDraft(stackID, saveBody(v, compose, vars)); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	return draftVersion(t, c, stackID)
}

// deployAndWait deploys the draft with the given body and waits for the job.
func (f *envFix) deployAndWait(t *testing.T, c *roleCaller, stackID, body string) jobAnswer {
	t.Helper()
	rec := c.deploy(stackID, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	return f.awaitDeploy(t, started.JobID)
}

func pinsJSON(t *testing.T, pins map[string]string) string {
	t.Helper()
	b, err := json.Marshal(pins)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const reviewCompose = `services:
  web:
    image: nginx:1.27-alpine
    environment:
      MODE: prod
    ports:
      - "9090:80"
`

func TestReviewThenDeployUsesTheReviewedDigests(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	version := saveCompose(t, operator, st.ID, reviewCompose, `[]`)

	// First review: nothing runs, so everything is new, and the review writes nothing.
	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if got := rv.Summary.Services; got.Added != 1 || got.Changed != 0 || got.Removed != 0 {
		t.Errorf("services = %+v, want one added", got)
	}
	if got := rv.Summary.Networks; got.Added != 1 {
		t.Errorf("networks = %+v, want the default network added", got)
	}
	if rv.DraftVersion != version || len(rv.Problems) != 0 || len(rv.Services) != 1 || rv.Services[0].Change != "added" || rv.Services[0].Next == nil {
		t.Fatalf("review = %+v, want one added service of draft %d and no problems", rv, version)
	}
	first := dockertest.FakeDigestOf("docker.io/library/nginx:1.27-alpine")
	if len(rv.Images) != 1 || rv.Images[0].State != "new" || rv.Images[0].Digest != first || rv.Summary.ImagesNew != 1 {
		t.Fatalf("images = %+v summary %+v, want one new image with the looked-up digest", rv.Images, rv.Summary)
	}
	if pin := rv.Pins["shop_web"]; !strings.HasSuffix(pin, "@"+first) {
		t.Fatalf("pins = %v, want shop_web pinned to %s", rv.Pins, first)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("a review wrote %v", w)
	}

	// The tag moves after the review. A deploy that carries the review's pins ships the reviewed
	// digest and asks no registry.
	moved := "sha256:" + strings.Repeat("b", 64)
	f.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", moved)
	lookups := len(f.fake.DistributionRefs())
	job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`,"pins":`+pinsJSON(t, rv.Pins)+`}`)
	if job.State != "done" {
		t.Fatalf("job = %+v, want done", job)
	}
	if got := len(f.fake.DistributionRefs()); got != lookups {
		t.Errorf("the deploy made %d registry lookups, want none", got-lookups)
	}
	creates := f.fake.ServiceCreates()
	if len(creates) != 1 || creates[0].Spec.TaskTemplate.ContainerSpec.Image != rv.Pins["shop_web"] {
		t.Fatalf("service creates = %+v, want the reviewed image %s", creates, rv.Pins["shop_web"])
	}

	// A review right after the deploy shows nothing to deploy.
	rv, code, raw = operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review after the deploy: %d %s", code, raw)
	}
	if len(rv.Services) != 0 || len(rv.Objects) != 0 || len(rv.Problems) != 0 {
		t.Fatalf("review after the deploy = %+v, want no change", rv)
	}
	if len(rv.Images) != 1 || rv.Images[0].State != "pinned" || rv.Images[0].Digest != first || rv.Summary.ImagesPinned != 1 {
		t.Errorf("images = %+v, want the running digest kept although the tag moved", rv.Images)
	}
}

func TestReviewListsAChangedServiceAndARemovedOne(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	two := reviewCompose + "  worker:\n    image: busybox:1.36\n    command: sleep 1000\n"
	version := saveCompose(t, operator, st.ID, two, `[]`)
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}

	grown := strings.Replace(two, "    ports:", "    deploy:\n      replicas: 2\n    ports:", 1)
	grown = grown[:strings.Index(grown, "  worker:")]
	saveCompose(t, operator, st.ID, grown, `[]`)
	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if len(rv.Services) != 2 || rv.Services[0].Short != "web" || rv.Services[0].Change != "changed" || rv.Services[1].Short != "worker" || rv.Services[1].Change != "removed" {
		t.Fatalf("services = %+v, want web changed, then worker removed", rv.Services)
	}
	web := rv.Services[0]
	if web.Settings != 1 {
		t.Errorf("settings = %d, want 1 (the replica count)", web.Settings)
	}
	replicas := func(spec map[string]any) any {
		mode, _ := spec["Mode"].(map[string]any)
		rep, _ := mode["Replicated"].(map[string]any)
		return rep["Replicas"]
	}
	if replicas(web.Current) != float64(1) || replicas(web.Next) != float64(2) {
		t.Errorf("replicas current %v next %v, want 1 and 2", replicas(web.Current), replicas(web.Next))
	}
	if rv.Services[1].Current != nil || rv.Services[1].Next != nil {
		t.Errorf("a removed service is a summary only: %+v", rv.Services[1])
	}
	if got := rv.Summary.Services; got.Changed != 1 || got.Removed != 1 || got.Added != 0 {
		t.Errorf("summary = %+v", got)
	}
	if len(rv.Images) != 1 || rv.Images[0].State != "pinned" {
		t.Errorf("images = %+v, want web pinned (running)", rv.Images)
	}
}

func TestReviewHidesASecretThatChangedAndNamesItsPath(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      API_KEY: ${API_KEY}\n      MODE: prod\n"
	version := saveCompose(t, operator, st.ID, compose, `[{"name":"API_KEY","value":"oldsecret-value","secret":true}]`)
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}
	saveCompose(t, operator, st.ID, compose, `[{"name":"API_KEY","value":"newsecret-value","secret":true}]`)

	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if strings.Contains(raw, "secret-value") {
		t.Fatalf("a secret reached the answer: %s", raw)
	}
	if len(rv.Services) != 1 || rv.Services[0].Change != "changed" {
		t.Fatalf("services = %+v, want one changed service: the secret changed", rv.Services)
	}
	if got := rv.Services[0].HiddenChanges; len(got) != 1 || got[0] != "TaskTemplate.ContainerSpec.Env[API_KEY]" {
		t.Errorf("hiddenChanges = %v", got)
	}
	for _, side := range []map[string]any{rv.Services[0].Current, rv.Services[0].Next} {
		b, _ := json.Marshal(side)
		if !strings.Contains(string(b), `API_KEY=\u003chidden\u003e`) || !strings.Contains(string(b), "MODE=prod") {
			t.Errorf("a side = %s, want API_KEY hidden and MODE shown", b)
		}
	}
}

func TestReviewOfAStandaloneStackComparesItWithTheHostsContainers(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "solo")
	if st.Target != "standalone" {
		t.Fatalf("target = %q", st.Target)
	}
	rec := operator.do(http.MethodPost, "stacks/"+st.ID+"/review", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %s, want 200", rec.Code, rec.Body)
	}
	rv := answerOf[struct {
		Services []struct{ Short, Change string }      `json:"services"`
		Images   []struct{ Service, State string }     `json:"images"`
		Objects  []struct{ Change, Kind, Name string } `json:"objects"`
		Pins     map[string]string                     `json:"pins"`
	}](t, rec)
	if len(rv.Services) != 1 || rv.Services[0].Short != "web" || rv.Services[0].Change != "added" {
		t.Errorf("services = %+v, want web added", rv.Services)
	}
	if len(rv.Images) != 1 || rv.Images[0].State != "new" || !strings.Contains(rv.Pins["web"], "@sha256:") {
		t.Errorf("images = %+v pins = %v, want a new digest", rv.Images, rv.Pins)
	}
	if len(rv.Objects) != 1 || rv.Objects[0].Kind != "network" || rv.Objects[0].Name != "solo_default" {
		t.Errorf("objects = %+v, want the default network added", rv.Objects)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("a review wrote to the engine: %v", w)
	}
}

func TestReviewNeedsTheDeployPermission(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "shop")
	rec := viewer.do(http.MethodPost, "stacks/"+st.ID+"/review", `{}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a viewer's review: %d %s, want 403", rec.Code, rec.Body)
	}
}

func TestDeployRefusesAMovedDraft(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "stale")
	reviewed := saveCompose(t, operator, st.ID, reviewCompose, `[]`)
	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK || rv.DraftVersion != reviewed {
		t.Fatalf("review: %d %s", code, raw)
	}
	// Somebody saves again after the review.
	saveCompose(t, operator, st.ID, reviewCompose+"# edited\n", `[]`)

	rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(reviewed)+`,"pins":`+pinsJSON(t, rv.Pins)+`}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "draft_changed" {
		t.Fatalf("deploy of the reviewed version: %d %s, want 409 draft_changed", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
	var revisions int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM stack_snapshots WHERE kind = 'revision'`).Scan(&revisions); err != nil || revisions != 0 {
		t.Errorf("revisions = %d (%v), a refused deploy must leave none", revisions, err)
	}
}

func TestDeployRefusesAPinTheServerWouldNotHaveChosen(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	version := saveCompose(t, operator, st.ID, reviewCompose, `[]`)
	good := "docker.io/library/nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	other := "docker.io/library/busybox:1@sha256:" + strings.Repeat("a", 64)

	for name, tc := range map[string]map[string]string{
		"another repository":                     {"shop_web": other},
		"a bad digest":                           {"shop_web": "docker.io/library/nginx:1.27-alpine@sha256:abc"},
		"a missing service":                      {},
		"an unknown service":                     {"shop_web": good, "shop_ghost": good},
		"an image ID that is not on the manager": {"shop_web": "sha256:" + strings.Repeat("c", 64)},
	} {
		rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(version)+`,"pins":`+pinsJSON(t, tc)+`}`)
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "pins_invalid" {
			t.Errorf("%s: %d %s, want 422 pins_invalid", name, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a refused deploy changes nothing", w)
	}

	// A kept pin that differs from the running image is refused too; the one that matches is not.
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}
	version = saveCompose(t, operator, st.ID, reviewCompose+"# edited\n", `[]`)
	rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(version)+`,"pins":`+pinsJSON(t, map[string]string{"shop_web": good})+`}`)
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "pins_invalid" {
		t.Errorf("a kept pin that is not the running image: %d %s, want 422 pins_invalid", rec.Code, rec.Body)
	}
	rv, _, _ := operator.reviewStack(st.ID, `{}`)
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`,"pins":`+pinsJSON(t, rv.Pins)+`}`); job.State != "done" {
		t.Errorf("the review's own pins: %+v, want a deploy", job)
	}
}

// seedStack stores a stack for an environment without the checks the create route makes, as a
// stack that was created before the environment changed or before an object appeared.
func (f *envFix) seedStack(t *testing.T, envID, name string, target stacks.Target) string {
	t.Helper()
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	st, _, err := repo.CreateStack(testsupport.Context(t), envID, name, target, "seed", stacks.WorkingCopy{
		Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(stacks.StarterCompose)}},
	})
	if err != nil {
		t.Fatalf("seed stack %s: %v", name, err)
	}
	return st.ID
}

type problemAnswer struct {
	Type   string                               `json:"type"`
	Detail string                               `json:"detail"`
	Errors []struct{ Message, Location string } `json:"errors"`
}

func problemOf(t *testing.T, rec *httptest.ResponseRecorder) problemAnswer {
	t.Helper()
	var p problemAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("not a problem body: %v: %s", err, rec.Body)
	}
	return p
}

func (a stackReviewAnswer) problem(code string) (struct {
	Severity, Code, Message, File string
	Line                          int
}, bool) {
	for _, p := range a.Problems {
		if p.Code == code {
			return p, true
		}
	}
	return struct {
		Severity, Code, Message, File string
		Line                          int
	}{}, false
}

func TestOperatorCannotDeployHostEscalation(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "esc")
	privileged := "services:\n  web:\n    image: nginx:1.27-alpine\n    privileged: true\n"
	version := saveCompose(t, operator, st.ID, privileged, `[]`)

	// The review says what needs Admin, with the line, and names the kinds for the Deploy tooltip.
	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	p, ok := rv.problem("needs_admin")
	if !ok || p.Severity != "error" || p.Message != "This needs Admin: privileged: true." || p.File != "compose.yaml" || p.Line != 4 {
		t.Fatalf("problems = %+v, want a blocking \"This needs Admin: privileged: true.\" at compose.yaml:4", rv.Problems)
	}
	if len(rv.NeedsAdmin) != 1 || rv.NeedsAdmin[0] != "extra privileges" {
		t.Errorf("needsAdmin = %v", rv.NeedsAdmin)
	}

	// The deploy refuses it before anything is stored or sent.
	rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(version)+`}`)
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "privileged_compose" {
		t.Fatalf("an Operator's deploy: %d %s, want 403 privileged_compose", rec.Code, rec.Body)
	}
	if pr := problemOf(t, rec); !strings.Contains(pr.Detail, "privileged: true at compose.yaml:4") || len(pr.Errors) != 1 {
		t.Errorf("problem = %+v, want the setting and its place named", pr)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
	var revisions int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM stack_snapshots WHERE kind = 'revision'`).Scan(&revisions); err != nil || revisions != 0 {
		t.Errorf("revisions = %d (%v), a refused deploy must leave none", revisions, err)
	}

	// An env Admin sees no problem and deploys it.
	rv, _, _ = admin.reviewStack(st.ID, `{}`)
	if _, ok := rv.problem("needs_admin"); ok || len(rv.NeedsAdmin) != 0 {
		t.Errorf("an Admin's review = %+v", rv.Problems)
	}
	if job := f.deployAndWait(t, admin, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Errorf("an Admin's deploy = %+v, want done", job)
	}
}

func TestOperatorCannotDeployAHostPathHiddenBehindAVariable(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "hidden")
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    volumes:\n      - \"${DATA}:/data\"\n"

	version := saveCompose(t, operator, st.ID, compose, `[{"name":"DATA","value":"/var/run/docker.sock","secret":false}]`)
	rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(version)+`}`)
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "privileged_compose" || !strings.Contains(rec.Body.String(), "a bind mount of /var/run/docker.sock at compose.yaml:5") {
		t.Fatalf("the socket through a variable: %d %s, want 403 privileged_compose naming the bind", rec.Code, rec.Body)
	}
	version = saveCompose(t, operator, st.ID, compose, `[{"name":"DATA","value":"/srv/data","secret":false}]`)
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Errorf("a data root through a variable: %+v, want an Operator's deploy to go through", job)
	}
}

func TestNameClashIsRefusedAtCreateAndReview(t *testing.T) {
	t.Parallel()
	foreign := dockertest.FakeService{ID: "s1", Name: "taken_app", Mode: "replicated", Image: "x", Labels: map[string]string{"com.docker.stack.namespace": "taken"}}
	f, e := stackEnv(t, nil, foreign)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")

	rec := admin.createStack(`{"name":"taken"}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "stack_name_clash" || problemOf(t, rec).Detail != "A stack named taken already exists on this environment, not managed by Bayplan." {
		t.Fatalf("create of a name a foreign stack holds: %d %s, want 409 stack_name_clash", rec.Code, rec.Body)
	}
	list := answerOf[struct {
		Items []stackAnswer `json:"items"`
	}](t, admin.do(http.MethodGet, "stacks", ""))
	if len(list.Items) != 0 {
		t.Errorf("stacks = %+v, a refused create stores nothing", list.Items)
	}
	if rec := admin.createStack(`{"name":"free"}`); rec.Code != http.StatusCreated {
		t.Errorf("create of a free name: %d %s", rec.Code, rec.Body)
	}

	// A stack that Bayplan already holds under that name is a stack_name_taken, not a clash, and the
	// objects it made itself never count as one.
	own := newStack(t, admin, "mine")
	version := saveCompose(t, operator, own.ID, reviewCompose, `[]`)
	if job := f.deployAndWait(t, operator, own.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}
	if rec := admin.createStack(`{"name":"mine"}`); rec.Code != http.StatusConflict || problemType(t, rec) != "stack_name_taken" {
		t.Errorf("create of a name Bayplan holds: %d %s, want 409 stack_name_taken", rec.Code, rec.Body)
	}
	rv, _, _ := operator.reviewStack(own.ID, `{}`)
	if _, ok := rv.problem("stack_name_clash"); ok {
		t.Errorf("a stack's own objects were read as a clash: %+v", rv.Problems)
	}

	// A stack record that exists next to a foreign stack of its name: review shows it, deploy refuses.
	taken := f.seedStack(t, e.ID, "taken", stacks.TargetSwarm)
	rv, code, raw := operator.reviewStack(taken, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	p, ok := rv.problem("stack_name_clash")
	if !ok || p.Severity != "error" || p.Message != "A stack named taken already exists on this environment, not managed by Bayplan." {
		t.Fatalf("problems = %+v, want a blocking stack_name_clash", rv.Problems)
	}
	before := len(f.fake.Writes())
	rec = operator.deploy(taken, `{"draftVersion":`+itoaT(draftVersion(t, operator, taken))+`}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "stack_name_clash" {
		t.Errorf("deploy of a stack whose name a foreign stack holds: %d %s, want 409 stack_name_clash", rec.Code, rec.Body)
	}
	if got := len(f.fake.Writes()); got != before {
		t.Errorf("a refused deploy wrote %v", f.fake.Writes()[before:])
	}
}

func TestTargetChangedIsRefused(t *testing.T) {
	t.Parallel()
	// A stack made for a standalone host, on an environment that is a swarm now.
	f, e := stackEnv(t, nil)
	operator := f.caller(t, e, "operator-1", "operator")
	solo := f.seedStack(t, e.ID, "solo", stacks.TargetStandalone)
	rec := operator.do(http.MethodPost, "stacks/"+solo+"/review", `{}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "target_changed" {
		t.Errorf("review of a standalone stack on a swarm: %d %s, want 409 target_changed", rec.Code, rec.Body)
	}
	rec = operator.deploy(solo, `{"draftVersion":1}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "target_changed" {
		t.Errorf("deploy of a standalone stack on a swarm: %d %s, want 409 target_changed", rec.Code, rec.Body)
	}

	// A stack made for a swarm, on an environment that is a standalone host now.
	f2, e2 := stackEnv(t, func(c *dockertest.FakeConfig) {
		c.SwarmState, c.SwarmControl, c.NodeRows = "", false, nil
	})
	operator2 := f2.caller(t, e2, "operator-1", "operator")
	swarmStack := f2.seedStack(t, e2.ID, "was-swarm", stacks.TargetSwarm)
	rec = operator2.do(http.MethodPost, "stacks/"+swarmStack+"/review", `{}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "target_changed" {
		t.Errorf("review of a swarm stack on a standalone host: %d %s, want 409 target_changed", rec.Code, rec.Body)
	}
	rec = operator2.deploy(swarmStack, `{"draftVersion":1}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "target_changed" {
		t.Errorf("deploy of a swarm stack on a standalone host: %d %s, want 409 target_changed", rec.Code, rec.Body)
	}
	if w := f2.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
}

// The Rendered YAML tab (plan 07-16, D-11): a deploy stores the masked rendering on its revision,
// and the next review diffs the draft's rendering against it.

func TestReviewDiffsTheRenderingAgainstTheLastDeploy(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	version := saveCompose(t, operator, st.ID, reviewCompose, `[]`)

	// A first deploy is compared with an empty document: every line is added.
	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if len(rv.YAML.Hunks) != 1 || rv.YAML.Hunks[0].OldLines != 0 || rv.YAML.ChangedLines != rv.YAML.Hunks[0].NewLines {
		t.Fatalf("first review yaml = %+v, want one hunk that adds the whole rendering", rv.YAML)
	}
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`,"pins":`+pinsJSON(t, rv.Pins)+`}`); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}

	// Right after the deploy the rendering is what was stored: nothing changed.
	rv, code, raw = operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review after the deploy: %d %s", code, raw)
	}
	if rv.YAML.ChangedLines != 0 || len(rv.YAML.Hunks) != 0 {
		t.Fatalf("yaml after the deploy = %+v, want no change", rv.YAML)
	}

	// One replica line more is two changed lines (the deploy: key and the replicas: value) in one hunk.
	grown := strings.Replace(reviewCompose, "    ports:", "    deploy:\n      replicas: 2\n    ports:", 1)
	saveCompose(t, operator, st.ID, grown, `[]`)
	rv, code, raw = operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review of the change: %d %s", code, raw)
	}
	if len(rv.YAML.Hunks) != 1 || rv.YAML.ChangedLines != 2 {
		t.Fatalf("yaml = %+v, want one hunk of 2 changed lines", rv.YAML)
	}
	var added []string
	for _, l := range rv.YAML.Hunks[0].Lines {
		if l.Kind == "added" {
			added = append(added, strings.TrimSpace(l.Text))
		}
	}
	if len(added) != 2 || added[0] != "deploy:" || added[1] != "replicas: 2" {
		t.Errorf("added lines = %q", added)
	}
	if len(rv.YAML.Lines) == 0 {
		t.Error("the answer carries no lines of the new text, so a page cannot show the unchanged lines")
	}
}

func TestReviewNeverSendsASecretInTheRenderedYAML(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      API_KEY: ${API_KEY}\n      MODE: prod\n"
	version := saveCompose(t, operator, st.ID, compose, `[{"name":"API_KEY","value":"oldsecret-value","secret":true}]`)
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}
	saveCompose(t, operator, st.ID, strings.Replace(compose, "MODE: prod", "MODE: test", 1), `[{"name":"API_KEY","value":"newsecret-value","secret":true}]`)
	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if strings.Contains(raw, "secret-value") {
		t.Fatalf("a secret reached the answer: %s", raw)
	}
	if rv.YAML.ChangedLines != 2 {
		t.Errorf("yaml = %+v, want the MODE line changed and the secret line unchanged (hidden against hidden)", rv.YAML)
	}
	// The rendering stored with the revision is masked too.
	var stored string
	if err := f.st.Write.QueryRowContext(t.Context(), `SELECT rendered FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision'`, st.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "secret-value") || !strings.Contains(stored, "<hidden>") {
		t.Errorf("stored rendering = %q, want it masked", stored)
	}
}

// A deploy never overwrites a newer one unseen (plan 07-16, STK-05, D-09). Someone else deploys a
// revision while the operator's draft is open; the review says so, and the server refuses the
// deploy until it is told the person saw what changed.

const concurrentCompose = "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      API_KEY: ${API_KEY}\n      MODE: %s\n"

func concurrentFixture(mode string) string { return strings.Replace(concurrentCompose, "%s", mode, 1) }

func revisionCount(t *testing.T, f *envFix, stackID string) int {
	t.Helper()
	var n int
	if err := f.st.Write.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision'`, stackID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConcurrentDraftNeedsAcknowledgement(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	secret := func(v string) string { return `[{"name":"API_KEY","value":"` + v + `","secret":true}]` }

	// The operator deploys revision 1. Their own deploy is not a concurrent one.
	version := saveCompose(t, operator, st.ID, concurrentFixture("prod"), secret("first-secret-value"))
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}
	if rv, code, raw := operator.reviewStack(st.ID, `{}`); code != http.StatusOK || rv.Concurrent != nil {
		t.Fatalf("review after one's own deploy: %d, concurrent %+v, %s; want none", code, rv.Concurrent, raw)
	}

	// Bob deploys revision 2 behind the draft's back (through the store, as a deploy that did not
	// start from this draft would), with another mode and another secret.
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	saveCompose(t, operator, st.ID, concurrentFixture("staging"), secret("second-secret-value"))
	ctx := testsupport.Context(t)
	rev, err := repo.NewRevision(ctx, st.ID, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	// Then the operator's own edit, still from revision 1: three replicas.
	grown := strings.Replace(concurrentFixture("prod"), "    environment:", "    deploy:\n      replicas: 3\n    environment:", 1)
	version = saveCompose(t, operator, st.ID, grown, secret("third-secret-value"))

	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	c := rv.Concurrent
	if c == nil {
		t.Fatalf("the review does not say a deployment was made after the draft started: %s", raw)
	}
	if c.Revision != 2 || c.DeployedBy != "bob" || c.DeployedAt == "" {
		t.Errorf("concurrent = %+v, want revision 2 by bob", c)
	}
	if len(c.BaseToDeployed) == 0 || c.BaseToDeployed[0].Path != "compose.yaml" || strings.Join(c.BaseToDeployed[0].added(), "|") != "MODE: staging" {
		t.Errorf("base to deployed = %+v, want compose.yaml with MODE: staging added", c.BaseToDeployed)
	}
	if len(c.BaseToDraft) == 0 || c.BaseToDraft[0].Path != "compose.yaml" || strings.Join(c.BaseToDraft[0].added(), "|") != "deploy:|replicas: 3" {
		t.Errorf("base to draft = %+v, want compose.yaml with the replicas added", c.BaseToDraft)
	}
	for _, v := range []string{"first-secret-value", "second-secret-value", "third-secret-value"} {
		if strings.Contains(raw, v) {
			t.Fatalf("the secret %q reached the review: %s", v, raw)
		}
	}

	// The server refuses the deploy on its own, whatever the browser did.
	rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(version)+`}`)
	if rec.Code != http.StatusConflict || problemOf(t, rec).Type != "concurrent_unacknowledged" {
		t.Fatalf("deploy without the acknowledgement: %d %s, want 409 concurrent_unacknowledged", rec.Code, rec.Body)
	}
	if n := revisionCount(t, f, st.ID); n != 2 {
		t.Fatalf("a refused deploy left %d revisions, want 2", n)
	}

	// With it, the deploy runs and takes revision 3.
	rec = operator.deploy(st.ID, `{"draftVersion":`+itoaT(version)+`,"acknowledgeConcurrent":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy with the acknowledgement: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		JobID    string `json:"jobId"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Revision != 3 {
		t.Errorf("revision = %d, want 3", started.Revision)
	}
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}

	// The draft now starts from revision 3, so it is no longer concurrent.
	if rv, code, _ = operator.reviewStack(st.ID, `{}`); code != http.StatusOK || rv.Concurrent != nil {
		t.Errorf("review after the acknowledged deploy: %d, concurrent %+v; want none", code, rv.Concurrent)
	}
}

func TestASecondDeployOfTheSameDraftNeedsNoAcknowledgement(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	version := saveCompose(t, operator, st.ID, reviewCompose, `[]`)
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}
	grown := strings.Replace(reviewCompose, "    ports:", "    deploy:\n      replicas: 2\n    ports:", 1)
	version = saveCompose(t, operator, st.ID, grown, `[]`)
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Fatalf("the second deploy of one's own draft was refused or failed: %+v", job)
	}
}

func TestADraftOfAStackNeverDeployedIsNeverConcurrent(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	version := saveCompose(t, operator, st.ID, reviewCompose, `[]`)
	if rv, code, _ := operator.reviewStack(st.ID, `{}`); code != http.StatusOK || rv.Concurrent != nil {
		t.Errorf("review of a first deploy: %d, concurrent %+v; want none", code, rv.Concurrent)
	}
	if rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(version)+`}`); rec.Code != http.StatusAccepted {
		t.Errorf("deploy of a first deploy: %d %s", rec.Code, rec.Body)
	}
}

// A standalone stack deployed through the API (the Compose SDK is a stand-in that creates nothing), then
// stood on the fake host as a deploy leaves it: one container for each service, made from the prepared service.
const standaloneReviewCompose = "services:\n  web:\n    image: nginx:1.27-alpine\n    ports:\n      - \"8080:80\"\n    restart: unless-stopped\n"

// deployedStandalone creates the stack solo, deploys compose to the standalone host and then puts the stack's
// containers on the fake, as the real Compose SDK would have left them. It returns what the host holds.
func deployedStandalone(t *testing.T, f *envFix, e envBody, compose string) (*roleCaller, stackAnswer, *stackopstest.Hosted) {
	t.Helper()
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "solo")
	version := saveCompose(t, operator, st.ID, compose, `[]`)
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}
	// The pins the deploy recorded are the images the containers run.
	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	h := stackopstest.HostPinned(t, "solo", st.ID, compose, nil, rv.Pins)
	return operator, st, h
}

// standOnHost makes the fake report the stack's containers, images, networks and volumes.
func standOnHost(f *envFix, h *stackopstest.Hosted) {
	cfg := swarmManagerEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.NodeRows = "", false, nil
	h.Apply(&cfg)
	f.fake.Reconfigure(cfg)
}

func TestStandaloneReviewShowsFieldChanges(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	operator, st, h := deployedStandalone(t, f, e, standaloneReviewCompose)
	standOnHost(f, h)

	// Right after the deploy there is nothing to do.
	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if len(rv.Services) != 0 || len(rv.Objects) != 0 {
		t.Fatalf("a review right after a deploy: services = %+v objects = %+v, want none", rv.Services, rv.Objects)
	}

	// The port moves: web is changed at its published port alone, and the image reads pinned (running).
	saveCompose(t, operator, st.ID, strings.Replace(standaloneReviewCompose, "8080:80", "9090:80", 1), `[]`)
	rv, code, raw = operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if len(rv.Services) != 1 || rv.Services[0].Short != "web" || rv.Services[0].Change != "changed" || rv.Services[0].Settings != 1 {
		t.Fatalf("services = %+v, want web changed at one setting", rv.Services)
	}
	cur, _ := json.Marshal(rv.Services[0].Current)
	next, _ := json.Marshal(rv.Services[0].Next)
	if !strings.Contains(string(cur), `"published":"8080"`) || !strings.Contains(string(next), `"published":"9090"`) {
		t.Errorf("current = %s\nnext = %s", cur, next)
	}
	if len(rv.Images) != 1 || rv.Images[0].State != "pinned" {
		t.Errorf("images = %+v, want web pinned (running)", rv.Images)
	}
	if w := f.fake.Writes(); len(w) != 1 {
		t.Errorf("engine writes = %v, want only the deploy's pull of the image", w)
	}
}

func TestStandaloneReviewShowsAHandChangedContainer(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	operator, st, h := deployedStandalone(t, f, e, standaloneReviewCompose)
	// docker update --restart always: the compose labels and the config hash stay.
	h.Docs["web"].HostConfig.RestartPolicy = container.RestartPolicy{Name: "always"}
	standOnHost(f, h)

	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if len(rv.Services) != 1 || rv.Services[0].Short != "web" || rv.Services[0].Change != "changed" || rv.Services[0].Settings != 1 {
		t.Fatalf("services = %+v, want web changed at its restart policy though the file is as deployed", rv.Services)
	}
	cur, _ := json.Marshal(rv.Services[0].Current)
	next, _ := json.Marshal(rv.Services[0].Next)
	if !strings.Contains(string(cur), `"restart":"always"`) || !strings.Contains(string(next), `"restart":"unless-stopped"`) {
		t.Errorf("current = %s\nnext = %s", cur, next)
	}
	if rv.Summary.Services.Changed != 1 {
		t.Errorf("summary = %+v", rv.Summary.Services)
	}
}

// Update images (plan 07-28, D-16, STK-09): the explicit way a pinned digest moves, through the same
// review and deploy as any other change.

const updateCompose = `services:
  web:
    image: nginx:1.27-alpine
  api:
    image: busybox:1.36
    command: sleep 1000
`

func TestUpdateModeChangesOnlyTheChosenServices(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "upd")
	f.deployGood(t, operator, st.ID, updateCompose)
	version := draftVersion(t, operator, st.ID)

	const webRef, apiRef = "docker.io/library/nginx:1.27-alpine", "docker.io/library/busybox:1.36"
	webOld, apiOld := dockertest.FakeDigestOf(webRef), dockertest.FakeDigestOf(apiRef)
	webNew, apiNew := "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64)
	f.fake.SetDistributionDigest(webRef, webNew)
	f.fake.SetDistributionDigest(apiRef, apiNew)

	// Chosen: web. Its tag is looked up again; api keeps what it runs and the registry is not asked about it.
	lookups := len(f.fake.DistributionRefs())
	rv, code, raw := operator.reviewStack(st.ID, `{"mode":"update","services":["web"]}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	rows := map[string]struct{ State, Digest, Current string }{}
	for _, r := range rv.Images {
		rows[r.Service] = struct{ State, Digest, Current string }{r.State, r.Digest, r.Current}
	}
	if got := rows["web"]; got.State != "new" || got.Digest != webNew || got.Current != webOld {
		t.Errorf("web image = %+v, want new, from %s to %s", got, webOld, webNew)
	}
	if got := rows["api"]; got.State != "pinned" || got.Digest != apiOld || got.Current != apiOld {
		t.Errorf("api image = %+v, want pinned at %s", got, apiOld)
	}
	for _, ref := range f.fake.DistributionRefs()[lookups:] {
		if strings.Contains(ref, "busybox") {
			t.Errorf("the review asked the registry about %s, which was not chosen", ref)
		}
	}
	if len(rv.Services) != 1 || rv.Services[0].Short != "web" || rv.Services[0].Change != "changed" {
		t.Fatalf("services = %+v, want only web changed", rv.Services)
	}

	// A name the stack does not have, or no name at all, is 422 services_invalid and nothing is looked up or started.
	lookups = len(f.fake.DistributionRefs())
	for name, body := range map[string]string{
		"an unknown service": `"mode":"update","services":["web","ghost"]`,
		"no service":         `"mode":"update","services":[]`,
	} {
		if rec := operator.do(http.MethodPost, "stacks/"+st.ID+"/review", `{`+body+`}`); rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "services_invalid" {
			t.Errorf("review with %s: %d %s, want 422 services_invalid", name, rec.Code, rec.Body)
		}
		if rec := operator.deploy(st.ID, `{"draftVersion":`+itoaT(version)+`,`+body+`,"pins":`+pinsJSON(t, rv.Pins)+`}`); rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "services_invalid" {
			t.Errorf("deploy with %s: %d %s, want 422 services_invalid", name, rec.Code, rec.Body)
		}
	}
	if n := len(f.fake.DistributionRefs()); n != lookups {
		t.Errorf("a refused request made %d registry checks", n-lookups)
	}
	if got := len(f.revisions(t, st.ID)); got != 1 {
		t.Errorf("revisions = %d, a refused update must leave none", got)
	}

	// The deploy that follows ships web at the new digest, api as it was, and is audited as an update.
	updates := len(f.fake.ServiceUpdates())
	lookups = len(f.fake.DistributionRefs())
	job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(version)+`,"mode":"update","services":["web"],"pins":`+pinsJSON(t, rv.Pins)+`}`)
	if job.State != "done" {
		t.Fatalf("job = %+v, want done", job)
	}
	if n := len(f.fake.DistributionRefs()); n != lookups {
		t.Errorf("the deploy made %d registry checks, it carries the reviewed digests", n-lookups)
	}
	// A swarm deploy writes every service of the stack (as docker stack deploy does); what the update
	// must not do is move the digest of one that was not chosen.
	sent := map[string]string{}
	for _, u := range f.fake.ServiceUpdates()[updates:] {
		sent[u.Spec.Name] = u.Spec.TaskTemplate.ContainerSpec.Image
	}
	if sent["upd_web"] != "nginx:1.27-alpine@"+webNew || sent["upd_api"] != "busybox:1.36@"+apiOld || len(sent) != 2 {
		t.Fatalf("service writes = %v, want upd_web at %s and upd_api still at %s", sent, webNew, apiOld)
	}
	var row *auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == "stack.deployed" {
			row = &r
		}
	}
	if row == nil || row.Details["mode"] != "update" || row.Result != "success" {
		t.Errorf("stack.deployed = %+v, want a successful update", row)
	}

	// Afterwards web runs the new digest, api still the old, and the stored pins say so.
	rv, code, raw = operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review after: %d %s", code, raw)
	}
	after := map[string]string{}
	for _, r := range rv.Images {
		after[r.Service] = r.Current
	}
	if after["web"] != webNew || after["api"] != apiOld || len(rv.Services) != 0 {
		t.Errorf("after the update web runs %s and api %s, services %+v; want %s and %s and no change", after["web"], after["api"], rv.Services, webNew, apiOld)
	}
}

// A review whose conversion step refuses a secret variable's value answers 200 without the value anywhere in
// it (review WR-04, T-07-160).
func TestReviewAnswerNeverQuotesASecretVariable(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "leaky")
	saveCompose(t, operator, st.ID, "services:\n  web:\n    image: nginx:1.27-alpine\n    deploy:\n      endpoint_mode: ${PW}\n", leakyVars)

	rv, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if strings.Contains(raw, leakyValue) {
		t.Fatalf("a secret reached the answer: %s", raw)
	}
	if _, ok := rv.problem("convert_failed"); !ok {
		t.Errorf("problems = %+v, want a conversion problem (the premise of the test)", rv.Problems)
	}
}

// A review of a draft whose cast position or converter refuses an escaped secret variable answers 200 without
// any part of it (review CR-01, T-07-168).
func TestReviewAnswerNeverQuotesAnEscapedSecret(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	cases := map[string]struct {
		compose string
		// fixed is the key of the field the loader or the converter refuses, reported with the fixed sentence
		// (07-40, 07-42).
		fixed string
	}{
		"a cast position":              {head + "    deploy:\n      replicas: ${PW}\n", "services.web.deploy.replicas"},
		"a value the converter quotes": {head + "    deploy:\n      endpoint_mode: ${PW}\n", "services.web.deploy.endpoint_mode"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, e := stackEnv(t, nil)
			admin := f.caller(t, e, "envadmin-1", "admin")
			operator := f.caller(t, e, "operator-1", "operator")
			st := newStack(t, admin, "escaped")
			saveCompose(t, operator, st.ID, tc.compose, escapedLeakyVars(t))

			_, code, raw := operator.reviewStack(st.ID, `{}`)
			if code != http.StatusOK {
				t.Fatalf("review: %d %s", code, raw)
			}
			for _, part := range escapedLeakyParts {
				if strings.Contains(raw, part) {
					t.Errorf("review answer = %s, want no part (%q) of the secret", raw, part)
				}
			}
			if !strings.Contains(raw, stacks.SecretValueMessage(tc.fixed)) {
				t.Errorf("review answer = %s, want the fixed sentence at %s", raw, tc.fixed)
			}
		})
	}
}

// A review of a draft with a size field fed by a secret variable answers 200 with the fixed sentence and no
// part of the value (07-40, D-02, T-07-172).
func TestReviewAnswerNeverQuotesPartOfASecret(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "partial")
	saveCompose(t, operator, st.ID, "services:\n  web:\n    image: nginx:1.27-alpine\n    mem_limit: ${PW}\n", partSecretVars(t, "Password2024!"))

	_, code, raw := operator.reviewStack(st.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("review: %d %s", code, raw)
	}
	if strings.Contains(raw, "Password2024") {
		t.Errorf("review answer = %s, want no part of the secret", raw)
	}
	if !strings.Contains(raw, stacks.SecretValueMessage("services.web.mem_limit")) {
		t.Errorf("review answer = %s, want the fixed sentence at services.web.mem_limit", raw)
	}
}
