// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/stacks"
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
	f, e := actionsEnv(t, nil)
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
	f, e := actionsEnv(t, nil)
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
	f, e := actionsEnv(t, nil)
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
	f, e := actionsEnv(t, nil)
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
	f, e := actionsEnv(t, nil)
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
