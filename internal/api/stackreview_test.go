// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stacks"
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
	f, e := actionsEnv(t, nil)
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
	f, e := actionsEnv(t, nil)
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
	f, e := actionsEnv(t, nil)
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

func TestReviewOfAStandaloneStackIsRefusedUntilTheComposeEngineExists(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, func(c *dockertest.FakeConfig) {
		c.SwarmState, c.SwarmControl, c.NodeRows = "", false, nil
	})
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "solo")
	if st.Target != "standalone" {
		t.Fatalf("target = %q", st.Target)
	}
	rec := operator.do(http.MethodPost, "stacks/"+st.ID+"/review", `{}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "target_engine_missing" {
		t.Errorf("review: %d %s, want 409 target_engine_missing", rec.Code, rec.Body)
	}
}

func TestReviewNeedsTheDeployPermission(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil)
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
	f, e := actionsEnv(t, nil)
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
	f, e := actionsEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shop")
	version := saveCompose(t, operator, st.ID, reviewCompose, `[]`)
	good := "docker.io/library/nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	other := "docker.io/library/busybox:1@sha256:" + strings.Repeat("a", 64)

	for name, tc := range map[string]map[string]string{
		"another repository": {"shop_web": other},
		"a bad digest":       {"shop_web": "docker.io/library/nginx:1.27-alpine@sha256:abc"},
		"a missing service":  {},
		"an unknown service": {"shop_web": good, "shop_ghost": good},
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

var _ = stacks.StarterCompose
