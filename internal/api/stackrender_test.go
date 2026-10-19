// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// Rendering a stack's working copy (plan 07-13, STK-04, D-18): the stack as it will be deployed,
// with variables filled in and images pinned, secrets hidden, and the user's own file untouched.

type renderAnswer struct {
	Name string `json:"name"`
	YAML string `json:"yaml"`
}

func (c *roleCaller) render(stackID, body string) (renderAnswer, int, string, http.Header) {
	c.t.Helper()
	rec := c.do(http.MethodPost, "stacks/"+stackID+"/render", body)
	var a renderAnswer
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
			c.t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
	return a, rec.Code, rec.Body.String(), rec.Header()
}

// storedFile reads the bytes a snapshot of the stack holds for a path, straight from the store.
func (f *envFix) storedFiles(t *testing.T, stackID, path string) []string {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT f.content FROM stack_snapshot_files f JOIN stack_snapshots s ON s.id = f.snapshot_id
		WHERE s.stack_id = ? AND f.path = ? ORDER BY s.created_at`, stackID, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func lineOf(text, want string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, want) {
			return l
		}
	}
	return ""
}

func TestRenderShowsTheStackAsItWillDeployAndNeverCaches(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "shop")

	// Nothing deployed yet: the variable is filled in and the image will be looked up.
	body := checkBodyWithVars(stacks.StarterCompose, `[{"name":"WEB_PORT","value":"9090","secret":false}]`)
	a, code, raw, hdr := viewer.render(st.ID, body)
	if code != http.StatusOK {
		t.Fatalf("render by a viewer: %d %s, want 200 (stacks.read is enough)", code, raw)
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", hdr.Get("Cache-Control"))
	}
	if a.Name != "shop" {
		t.Errorf("name = %q, want shop", a.Name)
	}
	if !strings.Contains(lineOf(a.YAML, "9090"), `"9090:80"`) {
		t.Errorf("ports line = %q, want 9090:80", lineOf(a.YAML, "9090"))
	}
	if img := lineOf(a.YAML, "image:"); !strings.Contains(img, "# will be pinned when you deploy") {
		t.Errorf("image line = %q, want the plain reference and the comment", img)
	}

	// Deployed: the same text reads as the pin the deploy ran; a changed text does not.
	rec := admin.deploy(st.ID, `{"draftVersion":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("job = %+v, want done", job)
	}
	a, code, raw, _ = viewer.render(st.ID, checkBodyWithVars(stacks.StarterCompose, `[]`))
	if img := lineOf(a.YAML, "image:"); code != http.StatusOK || !strings.Contains(img, "nginx:1.27-alpine@sha256:") || strings.Contains(img, "#") {
		t.Errorf("after a deploy: %d image line %q, want the pinned reference and no comment (%s)", code, img, raw)
	}
	a, code, _, _ = viewer.render(st.ID, checkBodyWithVars(strings.Replace(stacks.StarterCompose, "1.27-alpine", "1.28-alpine", 1), `[]`))
	if img := lineOf(a.YAML, "image:"); code != http.StatusOK || strings.Contains(img, "@sha256:") || !strings.Contains(img, "# will be pinned when you deploy") {
		t.Errorf("a changed image text: %d image line %q, want it unpinned and marked", code, img)
	}
}

func TestRenderNeverChangesTheStoredCompose(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "untouched")

	// A file with a comment, odd spacing and a variable: the bytes the user wrote.
	written := "# my stack\nservices:\n  web:   \n    image:   nginx:1.27-alpine   # keep me\n    ports: [ \"${WEB_PORT:-8080}:80\" ]\n\n\n"
	rec := admin.saveDraft(st.ID, saveBody(1, written, `[{"name":"WEB_PORT","value":"9090","secret":false}]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	draftBytes := func() string {
		for _, fl := range readDraft(t, admin, st.ID).Files {
			if fl.Path == "compose.yaml" {
				return fl.Content
			}
		}
		return ""
	}
	if got := draftBytes(); got != written {
		t.Fatalf("the saved draft = %q, want the bytes written %q", got, written)
	}

	// Render and check it, then deploy it: the bytes are the same after each.
	if _, code, raw, _ := admin.render(st.ID, checkBodyWithVars(written, `[{"name":"WEB_PORT","value":"9090","secret":false}]`)); code != http.StatusOK {
		t.Fatalf("render: %d %s", code, raw)
	}
	if got := draftBytes(); got != written {
		t.Errorf("after a render the draft = %q, want %q", got, written)
	}
	if _, code, raw := admin.check(st.ID, checkBodyWithVars(written, `[]`)); code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	if got := draftBytes(); got != written {
		t.Errorf("after a check the draft = %q, want %q", got, written)
	}
	rec = admin.deploy(st.ID, `{"draftVersion":2}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("job = %+v, want done", job)
	}
	if got := draftBytes(); got != written {
		t.Errorf("after a deploy the draft = %q, want %q", got, written)
	}
	// Every snapshot Bayplan stored for the file, the draft and the revision, holds the same bytes.
	stored := f.storedFiles(t, st.ID, "compose.yaml")
	if len(stored) < 2 {
		t.Fatalf("stored compose files = %d, want the draft and the revision", len(stored))
	}
	for i, got := range stored {
		if i > 0 && got != written {
			t.Errorf("stored compose %d = %q, want the bytes the user saved %q", i, got, written)
		}
	}
}

func TestRenderHidesSecretsAndRefusesAStackWithErrors(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := stackWithSecret(t, f, admin, "rendermask")

	compose := "services:\n  db:\n    image: postgres:16\n    environment:\n      PW: ${DB_PASS}\n"
	for name, vars := range map[string]string{
		"the placeholder the browser holds": `[{"name":"DB_PASS","value":"<hidden>","secret":true}]`,
		"the value typed in clear":          `[{"name":"DB_PASS","value":"` + secretPass + `","secret":true}]`,
	} {
		a, code, raw, _ := admin.render(st.ID, checkBodyWithVars(compose, vars))
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, code, raw)
		}
		if strings.Contains(raw, secretPass) {
			t.Errorf("%s: the rendered stack carries the secret: %s", name, raw)
		}
		if !strings.Contains(lineOf(a.YAML, "PW:"), "<hidden>") {
			t.Errorf("%s: PW line = %q, want <hidden>", name, lineOf(a.YAML, "PW:"))
		}
	}

	// An error in the stack is 422 stack_invalid, with the problem and its line, and no YAML.
	rec := admin.do(http.MethodPost, "stacks/"+st.ID+"/render", checkBodyWithVars("services:\n  web:\n    image: nginx\n    imag: x\n", `[]`))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "stack_invalid" {
		t.Fatalf("an invalid stack: %d %s, want 422 stack_invalid", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "compose.yaml:4") {
		t.Errorf("the 422 does not name the line of the error: %s", rec.Body)
	}
	// A stack that is not there is 404.
	if _, code, _, _ := admin.render("no-such-stack", checkBodyWithVars(compose, `[]`)); code != http.StatusNotFound {
		t.Errorf("a stack that is not there: %d, want 404", code)
	}
}

func TestRenderSharesTheChecksLimitOfTwoPerUser(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	f, e := checkEnvTuned(t, func(f *envFix) {
		f.stackOnCheck = func(context.Context) {
			if calls.Add(1) <= 2 {
				entered <- struct{}{}
				<-release
			}
		}
	})
	admin := f.caller(t, e, "envadmin-1", "admin")
	holder := f.caller(t, e, "holder-1", "operator")
	st := newStack(t, admin, "limited")

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			holder.render(st.ID, checkBody(stacksStarter))
		}()
	}
	testsupport.Await(t, "the first render to be in flight", entered)
	testsupport.Await(t, "the second render to be in flight", entered)
	// A check, from the same user, is the third thing running: refused, because they share a limit.
	if _, code, raw := holder.check(st.ID, checkBody(stacksStarter)); code != http.StatusTooManyRequests || !strings.Contains(raw, "check_busy") {
		t.Errorf("a check beside two renders: %d %s, want 429 check_busy", code, raw)
	}
	if _, code, raw, _ := holder.render(st.ID, checkBody(stacksStarter)); code != http.StatusTooManyRequests || !strings.Contains(raw, "check_busy") {
		t.Errorf("a third render: %d %s, want 429 check_busy", code, raw)
	}
	close(release)
	wg.Wait()
}
