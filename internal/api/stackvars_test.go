// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// The variables of a stack (plan 07-10, STK-04, D-02): a secret variable is sealed at rest, shown
// as <hidden> in every answer, and readable only through an audited reveal by a user who holds
// secrets.reveal.

const secretPass = "s3cret-pass-1"

// secretVars is a variable table with one secret and one plain variable.
const secretVars = `[{"name":"DB_PASS","value":"` + secretPass + `","secret":true},{"name":"REGION","value":"eu-west","secret":false}]`

func (c *roleCaller) reveal(stackID, name string) (int, string, http.Header) {
	c.t.Helper()
	rec := c.do(http.MethodPost, "stacks/"+stackID+"/variables/"+name+"/reveal", "")
	return rec.Code, rec.Body.String(), rec.Header()
}

// deniedRows counts the permission.denied rows of the audit log.
func (f *envFix) deniedRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, audit.ActionPermissionDenied).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// stackWithSecret creates a stack as the env Admin and saves a draft that holds secretVars.
func stackWithSecret(t *testing.T, f *envFix, admin *roleCaller, name string) stackAnswer {
	t.Helper()
	st := newStack(t, admin, name)
	rec := admin.saveDraft(st.ID, saveBody(1, stacks.StarterCompose, secretVars))
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	return st
}

func TestRevealIsAuditedAndNeedsThePermission(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	revealer := f.revealCaller(t, e, "revealer-1")
	st := stackWithSecret(t, f, admin, "reveal")
	revealsBefore := func() int {
		n := 0
		for _, a := range f.stackAudits(t) {
			if a.Action == audit.ActionStackValueRevealed {
				n++
			}
		}
		return n
	}

	// An Operator without the extra, and a Viewer: 403, audited as a denial, no value, no reveal row.
	deniedBefore := f.deniedRows(t)
	for name, c := range map[string]*roleCaller{"an operator": operator, "a viewer": viewer} {
		code, body, _ := c.reveal(st.ID, "DB_PASS")
		if code != http.StatusForbidden || strings.Contains(body, secretPass) {
			t.Errorf("%s: %d %s, want 403 with no value", name, code, body)
		}
	}
	if got := f.deniedRows(t); got != deniedBefore+2 {
		t.Errorf("permission.denied rows went from %d to %d, want two more", deniedBefore, got)
	}
	if n := revealsBefore(); n != 0 {
		t.Fatalf("stack.value_revealed rows = %d after two refusals, want none", n)
	}

	// The extra, and the env Admin who holds it: 200 with the value, uncached, one audit row each.
	for _, c := range []*roleCaller{revealer, admin} {
		code, body, header := c.reveal(st.ID, "DB_PASS")
		var got struct {
			Value string `json:"value"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || got.Value != secretPass {
			t.Fatalf("reveal: %d %s, want 200 with the value", code, body)
		}
		if cc := header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
	}
	if n := revealsBefore(); n != 2 {
		t.Fatalf("stack.value_revealed rows = %d, want one per reveal", n)
	}
	for _, a := range f.stackAudits(t) {
		if a.Action != audit.ActionStackValueRevealed {
			continue
		}
		if a.Result != "success" || a.TargetType != "stack" || a.TargetID != st.ID || a.EnvID != e.ID {
			t.Errorf("row = %+v, want a success on the stack in the environment", a)
		}
		if a.Details["stack"] != "reveal" || a.Details["kind"] != "variable" || a.Details["name"] != "DB_PASS" || len(a.Details) != 3 {
			t.Errorf("details = %v, want exactly stack, kind and name", a.Details)
		}
		for k, v := range a.Details {
			if strings.Contains(v, secretPass) || strings.Contains(k, "secret") {
				t.Errorf("detail %s=%s carries the secret", k, v)
			}
		}
	}
}

func TestRevealOnlyAnswersForASavedSecretVariable(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := stackWithSecret(t, f, admin, "reveal404")
	for name, tc := range map[string]struct{ stack, variable string }{
		"a plain variable":       {st.ID, "REGION"},
		"a variable not in it":   {st.ID, "NOPE"},
		"a stack that isn't":     {"no-such-stack", "DB_PASS"},
		"a name that can't be":   {st.ID, "A.B"},
		"a name with a dash":     {st.ID, "DB-PASS"},
		"a name that is too big": {st.ID, strings.Repeat("A", 200)},
	} {
		code, body, _ := admin.reveal(tc.stack, tc.variable)
		if code != http.StatusNotFound || strings.Contains(body, secretPass) {
			t.Errorf("%s: %d %s, want 404 with no value", name, code, body)
		}
	}
	for _, a := range f.stackAudits(t) {
		if a.Action == audit.ActionStackValueRevealed {
			t.Errorf("audit row %+v: a reveal that gave nothing must not be recorded as one", a)
		}
	}
}

func TestADraftNeverShowsASecretVariableValue(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := stackWithSecret(t, f, admin, "draftmask")
	rec := admin.do(http.MethodGet, "stacks/"+st.ID+"/draft", "")
	if strings.Contains(rec.Body.String(), secretPass) {
		t.Fatalf("the draft carries the secret: %s", rec.Body)
	}
	d := answerOf[draftAnswer](t, rec)
	for _, v := range d.Vars {
		if v.Name == "DB_PASS" && v.Value != redact.Placeholder {
			t.Errorf("DB_PASS = %q, want %q", v.Value, redact.Placeholder)
		}
		if v.Name == "REGION" && v.Value != "eu-west" {
			t.Errorf("REGION = %q, want it as saved", v.Value)
		}
	}
	if stacks.Hidden != redact.Placeholder {
		t.Errorf("stacks.Hidden = %q, redact.Placeholder = %q: the two sentinels must be one text", stacks.Hidden, redact.Placeholder)
	}
}

func TestCheckNeverReturnsASecretValue(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := stackWithSecret(t, f, admin, "checkmask")

	// The image is the secret, so the moving-tag warning would quote it; the table is sent with the
	// secret as the placeholder the browser holds, and also typed in clear.
	compose := "services:\n  web:\n    image: ${DB_PASS}\n"
	for name, vars := range map[string]string{
		"the placeholder the browser holds": `[{"name":"DB_PASS","value":"<hidden>","secret":true}]`,
		"the value typed in clear":          `[{"name":"DB_PASS","value":"` + secretPass + `","secret":true}]`,
	} {
		answer, code, raw := viewer.check(st.ID, checkBodyWithVars(compose, vars))
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, code, raw)
		}
		if strings.Contains(raw, secretPass) {
			t.Errorf("%s: the answer carries the secret: %s", name, raw)
		}
		var warned bool
		for _, p := range answer.Problems {
			warned = warned || (p.Code == "moving_tag" && strings.Contains(p.Message, "<hidden>"))
		}
		if !warned {
			t.Errorf("%s: problems = %+v, want the moving-tag warning with the secret shown as <hidden>", name, answer.Problems)
		}
		for _, v := range answer.Variables {
			if v.Name == "DB_PASS" && (v.Value != "<hidden>" || !v.Secret) {
				t.Errorf("%s: DB_PASS = %+v, want <hidden>", name, v)
			}
		}
	}
}

func TestSavingRefusesAVariableTheMaskerCouldNotHideAndNamesTheFault(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "varrules")
	for name, tc := range map[string]struct{ vars, problem, which string }{
		"a secret of three characters": {`[{"name":"PIN","value":"123","secret":true}]`, "secret_too_short", "PIN"},
		"a name with a dot":            {`[{"name":"A.B","value":"x","secret":false}]`, "variable_name_invalid", "A.B"},
		"a name starting with a digit": {`[{"name":"1A","value":"x","secret":false}]`, "variable_name_invalid", "1A"},
		"a name used twice":            {`[{"name":"A","value":"1","secret":false},{"name":"A","value":"2","secret":false}]`, "variable_name_taken", "A"},
	} {
		rec := operator.saveDraft(st.ID, saveBody(1, stacks.StarterCompose, tc.vars))
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != tc.problem {
			t.Errorf("%s: %d %.300s, want 422 %s", name, rec.Code, rec.Body, tc.problem)
			continue
		}
		var problem struct {
			Errors []struct{ Location, Message string } `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil || len(problem.Errors) == 0 ||
			problem.Errors[0].Location != "variable" || problem.Errors[0].Message != tc.which {
			t.Errorf("%s: errors = %+v, want the variable %q named", name, problem.Errors, tc.which)
		}
	}
	// A short plain value is fine, and so is a secret of four.
	for _, vars := range []string{`[{"name":"PIN","value":"1","secret":false}]`, `[{"name":"PIN","value":"1234","secret":true}]`} {
		if rec := operator.saveDraft(st.ID, saveBody(draftVersion(t, operator, st.ID), stacks.StarterCompose, vars)); rec.Code != http.StatusOK {
			t.Errorf("save of %s: %d %s", vars, rec.Code, rec.Body)
		}
	}
	// The check, which runs as a person types, does not refuse a half-typed secret.
	if _, code, raw := operator.check(st.ID, checkBodyWithVars(stacks.StarterCompose, `[{"name":"PIN","value":"12","secret":true}]`)); code != http.StatusOK {
		t.Errorf("check of a short secret: %d %s, want 200", code, raw)
	}
}

// Secret stack files (plan 07-13, STK-04, D-02, UI-SPEC Defaults 6): a file the compose file uses
// under secrets: is sealed on save, shown as <hidden>, kept when sent back untouched, replaced when
// sent new, and readable only through an audited reveal by a user who holds secrets.reveal.

const (
	tokenText = "tok-abcdef-12345"
	// composeWithTokFile uses tok.txt under the top-level secrets key, and nginx.conf under configs:, which is not secret.
	composeWithTokFile = "services:\n  web:\n    image: nginx:1.27-alpine\nsecrets:\n  tok:\n    file: tok.txt\nconfigs:\n  site:\n    file: nginx.conf\n"
)

type fileBody struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Secret  bool   `json:"secret"`
}

func filesJSON(files ...fileBody) string {
	b, _ := json.Marshal(files)
	return string(b)
}

// secretFileSave is a save body whose files are given, and whose variables are none.
func secretFileSave(version int64, files ...fileBody) string {
	return `{"version":` + itoaT(version) + `,"files":` + filesJSON(files...) + `,"vars":[]}`
}

func (c *roleCaller) revealFile(stackID, path string) (int, string, http.Header) {
	c.t.Helper()
	b, _ := json.Marshal(struct {
		Path string `json:"path"`
	}{path})
	rec := c.do(http.MethodPost, "stacks/"+stackID+"/files/reveal", string(b))
	return rec.Code, rec.Body.String(), rec.Header()
}

// stackWithSecretFile creates a stack and saves a draft in which tok.txt is sent as a plain file.
func stackWithSecretFile(t *testing.T, admin *roleCaller, name string) stackAnswer {
	t.Helper()
	st := newStack(t, admin, name)
	rec := admin.saveDraft(st.ID, secretFileSave(1,
		fileBody{Path: "compose.yaml", Content: composeWithTokFile},
		fileBody{Path: "tok.txt", Content: tokenText},
		fileBody{Path: "nginx.conf", Content: "events {}\n"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	return st
}

type secretDraft struct {
	Version int64 `json:"version"`
	Files   []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Secret  bool   `json:"secret"`
	} `json:"files"`
}

func secretDraftOf(t *testing.T, c *roleCaller, stackID string) (secretDraft, string) {
	t.Helper()
	rec := c.do(http.MethodGet, "stacks/"+stackID+"/draft", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET draft: %d %s", rec.Code, rec.Body)
	}
	return answerOf[secretDraft](t, rec), rec.Body.String()
}

func fileOf(d secretDraft, path string) (content string, secret, found bool) {
	for _, f := range d.Files {
		if f.Path == path {
			return f.Content, f.Secret, true
		}
	}
	return "", false, false
}

func TestSecretFileIsSealedOnSaveAndAlwaysShownHidden(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := stackWithSecretFile(t, admin, "sealfile")

	// The save answer names the files it sealed, so the browser can let go of what it typed.
	rec := admin.saveDraft(st.ID, secretFileSave(2,
		fileBody{Path: "compose.yaml", Content: composeWithTokFile + "# touched\n"},
		fileBody{Path: "tok.txt", Content: tokenText},
		fileBody{Path: "nginx.conf", Content: "events {}\n"}))
	var saved struct {
		Changed     bool     `json:"changed"`
		SecretFiles []string `json:"secretFiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil || rec.Code != http.StatusOK || !saved.Changed {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if len(saved.SecretFiles) != 1 || saved.SecretFiles[0] != "tok.txt" {
		t.Errorf("secretFiles = %v, want [tok.txt]: the config file is not secret", saved.SecretFiles)
	}

	// Every read, by anyone, shows it hidden and flagged; the config file is plain.
	for name, c := range map[string]*roleCaller{"an admin": admin, "a viewer": viewer} {
		d, raw := secretDraftOf(t, c, st.ID)
		if strings.Contains(raw, tokenText) {
			t.Errorf("%s: the draft carries the secret file's content: %s", name, raw)
		}
		if content, secret, found := fileOf(d, "tok.txt"); !found || content != "<hidden>" || !secret {
			t.Errorf("%s: tok.txt = %q secret %v, want <hidden> and secret", name, content, secret)
		}
		if content, secret, _ := fileOf(d, "nginx.conf"); content != "events {}\n" || secret {
			t.Errorf("%s: nginx.conf = %q secret %v, want it plain", name, content, secret)
		}
	}

	// At rest it is sealed.
	var plain, sealed string
	row := f.st.Read.QueryRow(`SELECT f.content, f.content_sealed FROM stack_snapshot_files f JOIN stack_snapshots s ON s.id = f.snapshot_id
		WHERE s.stack_id = ? AND f.path = 'tok.txt'`, st.ID)
	if err := row.Scan(&plain, &sealed); err != nil || plain != "" || sealed == "" || strings.Contains(sealed, tokenText) {
		t.Errorf("stored tok.txt = %q / %q (%v), want it sealed", plain, sealed, err)
	}
}

func TestSecretFileSentBackHiddenIsKeptAndSentNewReplacesIt(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := stackWithSecretFile(t, admin, "keepfile")
	hidden := fileBody{Path: "tok.txt", Content: "<hidden>", Secret: true}

	// Untouched: the placeholder keeps the saved content, and with nothing else changed writes nothing.
	rec := admin.saveDraft(st.ID, secretFileSave(2,
		fileBody{Path: "compose.yaml", Content: composeWithTokFile}, hidden, fileBody{Path: "nginx.conf", Content: "events {}\n"}))
	var saved struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil || rec.Code != http.StatusOK || saved.Changed {
		t.Fatalf("placeholder save: %d %s, want 200 and changed false", rec.Code, rec.Body)
	}
	if code, body, _ := admin.revealFile(st.ID, "tok.txt"); code != http.StatusOK || !strings.Contains(body, tokenText) {
		t.Fatalf("reveal after a keep: %d %s, want the saved content", code, body)
	}

	// Replaced: the new content is sealed in its place, and is hidden from then on.
	rec = admin.saveDraft(st.ID, secretFileSave(2,
		fileBody{Path: "compose.yaml", Content: composeWithTokFile},
		fileBody{Path: "tok.txt", Content: "tok-replaced-98765", Secret: true},
		fileBody{Path: "nginx.conf", Content: "events {}\n"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body)
	}
	code, body, _ := admin.revealFile(st.ID, "tok.txt")
	if code != http.StatusOK || !strings.Contains(body, "tok-replaced-98765") || strings.Contains(body, tokenText) {
		t.Errorf("reveal after a replace: %d %s, want only the new content", code, body)
	}
	if _, raw := secretDraftOf(t, admin, st.ID); strings.Contains(raw, "tok-replaced-98765") {
		t.Errorf("the draft carries the replacement's content: %s", raw)
	}

	// A placeholder for a secret file that was never stored is refused.
	rec = admin.saveDraft(st.ID, secretFileSave(3,
		fileBody{Path: "compose.yaml", Content: composeWithTokFile}, fileBody{Path: "other.key", Content: "<hidden>", Secret: true}))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "stack_placeholder_without_value" {
		t.Errorf("a placeholder with nothing to keep: %d %s, want 422", rec.Code, rec.Body)
	}
}

func TestSecretFileStaysSecretWhenTheReferenceGoes(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := stackWithSecretFile(t, admin, "stayfile")

	// The compose file stops using it. The browser sends the file back as it knows it: hidden and
	// secret. It stays so.
	rec := admin.saveDraft(st.ID, secretFileSave(2,
		fileBody{Path: "compose.yaml", Content: "services:\n  web:\n    image: nginx:1.27-alpine\n"},
		fileBody{Path: "tok.txt", Content: "<hidden>", Secret: true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	d, _ := secretDraftOf(t, admin, st.ID)
	if content, secret, _ := fileOf(d, "tok.txt"); content != "<hidden>" || !secret {
		t.Errorf("tok.txt = %q secret %v, want it still hidden and secret", content, secret)
	}
	// Even sent plain, as another client might, it does not become readable.
	rec = admin.saveDraft(st.ID, secretFileSave(3,
		fileBody{Path: "compose.yaml", Content: "services:\n  web:\n    image: nginx:1.27-alpine\n"},
		fileBody{Path: "tok.txt", Content: "tok-sent-plain-1"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("plain save: %d %s", rec.Code, rec.Body)
	}
	if _, raw := secretDraftOf(t, admin, st.ID); strings.Contains(raw, "tok-sent-plain-1") {
		t.Errorf("a secret file sent plain is readable in the draft: %s", raw)
	}
}

func TestRevealAFileIsAuditedAndNeedsThePermission(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	revealer := f.revealCaller(t, e, "revealer-1")
	st := stackWithSecretFile(t, admin, "revealfile")
	reveals := func() (n int) {
		for _, a := range f.stackAudits(t) {
			if a.Action == audit.ActionStackValueRevealed {
				n++
			}
		}
		return n
	}

	deniedBefore := f.deniedRows(t)
	for name, c := range map[string]*roleCaller{"an operator": operator, "a viewer": viewer} {
		code, body, _ := c.revealFile(st.ID, "tok.txt")
		if code != http.StatusForbidden || strings.Contains(body, tokenText) {
			t.Errorf("%s: %d %s, want 403 with no content", name, code, body)
		}
	}
	if got := f.deniedRows(t); got != deniedBefore+2 {
		t.Errorf("permission.denied rows went from %d to %d, want two more", deniedBefore, got)
	}
	if n := reveals(); n != 0 {
		t.Fatalf("stack.value_revealed rows = %d after two refusals, want none", n)
	}

	for _, c := range []*roleCaller{revealer, admin} {
		code, body, header := c.revealFile(st.ID, "tok.txt")
		var got struct {
			Content string `json:"content"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || got.Content != tokenText {
			t.Fatalf("reveal: %d %s, want 200 with the content", code, body)
		}
		if cc := header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
	}
	if n := reveals(); n != 2 {
		t.Fatalf("stack.value_revealed rows = %d, want one per reveal", n)
	}
	for _, a := range f.stackAudits(t) {
		if a.Action != audit.ActionStackValueRevealed {
			continue
		}
		if a.Result != "success" || a.TargetType != "stack" || a.TargetID != st.ID || a.EnvID != e.ID {
			t.Errorf("row = %+v, want a success on the stack in the environment", a)
		}
		if a.Details["stack"] != "revealfile" || a.Details["kind"] != "file" || a.Details["name"] != "tok.txt" || len(a.Details) != 3 {
			t.Errorf("details = %v, want exactly stack, kind file and the path as name", a.Details)
		}
		for _, v := range a.Details {
			if strings.Contains(v, tokenText) {
				t.Errorf("a detail carries the content: %v", a.Details)
			}
		}
	}
}

func TestRevealAFileOnlyAnswersForASavedSecretFile(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := stackWithSecretFile(t, admin, "revealfile404")
	for name, tc := range map[string]struct{ stack, path string }{
		"a plain file":         {st.ID, "nginx.conf"},
		"the compose file":     {st.ID, "compose.yaml"},
		"a file not in it":     {st.ID, "nope.txt"},
		"a path that can't be": {st.ID, "../tok.txt"},
		"an empty path":        {st.ID, ""},
		"a stack that isn't":   {"no-such-stack", "tok.txt"},
	} {
		code, body, _ := admin.revealFile(tc.stack, tc.path)
		if code != http.StatusNotFound || strings.Contains(body, tokenText) {
			t.Errorf("%s: %d %s, want 404 with no content", name, code, body)
		}
	}
	for _, a := range f.stackAudits(t) {
		if a.Action == audit.ActionStackValueRevealed {
			t.Errorf("audit row %+v: a reveal that gave nothing must not be recorded as one", a)
		}
	}
}

func TestCheckNamesSecretFilesAndNeverReturnsTheirContent(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := stackWithSecretFile(t, admin, "checkfile")

	// The file is sent typed in clear before it is saved as secret, and as the placeholder after.
	for name, tok := range map[string]draftFile{
		"typed in clear":     {Path: "tok.txt", Content: tokenText},
		"as the placeholder": {Path: "tok.txt", Content: "<hidden>", Secret: true},
	} {
		body := checkBody(composeWithTokFile, tok, draftFile{Path: "nginx.conf", Content: "events {}\n"})
		var answer struct {
			SecretFiles []string `json:"secretFiles"`
		}
		rec := viewer.do(http.MethodPost, "stacks/"+st.ID+"/check", body)
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), tokenText) {
			t.Errorf("%s: the check carries the secret file's content: %s", name, rec.Body)
		}
		if len(answer.SecretFiles) != 1 || answer.SecretFiles[0] != "tok.txt" {
			t.Errorf("%s: secretFiles = %v, want [tok.txt]", name, answer.SecretFiles)
		}
	}
}

func TestRenderNeverCarriesASecretFilesContent(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "renderfile")
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    env_file: app.env\nsecrets:\n  tok:\n    file: app.env\n"
	body := checkBody(compose, draftFile{Path: "app.env", Content: "TOKEN=" + tokenText + "\n"})
	a, code, raw, _ := admin.render(st.ID, body)
	if code != http.StatusOK {
		t.Fatalf("render: %d %s", code, raw)
	}
	if strings.Contains(raw, tokenText) {
		t.Errorf("the rendered stack carries the secret file's content: %s", raw)
	}
	if !strings.Contains(a.YAML, "<hidden>") {
		t.Errorf("no <hidden> in the rendered stack:\n%s", a.YAML)
	}
}
