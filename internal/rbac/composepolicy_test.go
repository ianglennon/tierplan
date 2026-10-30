// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package rbac_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/rbac/compose"
)

const privilegedFile = `services:
  web:
    image: nginx
    privileged: true
`

const cleanFile = `services:
  web:
    image: nginx
    ports:
      - "8080:80"
    volumes:
      - ./data:/data
`

func composeFx(t *testing.T) *fx {
	t.Helper()
	f := newFx(t)
	f.addUser(t, "viewer")
	f.assign(t, "user", "viewer", "viewer", "env-a")
	f.addUser(t, "operator")
	f.assign(t, "user", "operator", "operator", "env-a")
	f.addUser(t, "envadmin")
	f.assign(t, "user", "envadmin", "admin", "env-a")
	f.instanceAdmin(t, "root")
	return f
}

func decide(t *testing.T, f *fx, p *rbac.Principal, env, src string) rbac.ComposeDecision {
	t.Helper()
	d, err := f.c.CheckComposeDeploy(context.Background(), p, env, []byte(src))
	if err != nil {
		t.Fatalf("CheckComposeDeploy: %v", err)
	}
	return d
}

func TestComposeAnOperatorDeployingAPrivilegedFileIsRefusedWithTheKeyAndLine(t *testing.T) {
	f := composeFx(t)
	d := decide(t, f, user("operator"), "env-a", privilegedFile)
	if d.Allowed || d.Reason != rbac.ReasonPrivilegedCompose || len(d.Findings) != 1 {
		t.Fatalf("decision = %+v, want a privileged_compose refusal with one finding", d)
	}
	if d.Message != "services.web.privileged (line 4): true" {
		t.Errorf("Message = %q", d.Message)
	}
}

func TestComposeTheMessageNamesEveryOffendingKeyOnItsOwnLine(t *testing.T) {
	f := composeFx(t)
	d := decide(t, f, user("operator"), "env-a", `services:
  web:
    image: nginx
    privileged: true
    pid: host
`)
	if d.Allowed || len(d.Findings) != 2 {
		t.Fatalf("decision = %+v", d)
	}
	lines := strings.Split(d.Message, "\n")
	if len(lines) != 2 || lines[0] != "services.web.privileged (line 4): true" || !strings.HasPrefix(lines[1], "services.web.pid (line 5)") {
		t.Errorf("Message = %q", d.Message)
	}
}

func TestComposeAnEnvAdminAndAnInstanceAdminMayDeployItAndStillGetTheFindings(t *testing.T) {
	f := composeFx(t)
	for _, who := range []string{"envadmin", "root"} {
		d := decide(t, f, user(who), "env-a", privilegedFile)
		if !d.Allowed || len(d.Findings) != 1 {
			t.Errorf("%s: %+v, want allowed with the finding returned", who, d)
		}
	}
	// An Admin of another environment is not an Admin of this one.
	f.addUser(t, "otheradmin")
	f.assign(t, "user", "otheradmin", "admin", "env-b")
	if d := decide(t, f, user("otheradmin"), "env-a", privilegedFile); d.Allowed {
		t.Errorf("an Admin of env-b was allowed a privileged file on env-a: %+v", d)
	}
}

func TestComposeACleanFileIsNotJudgedByTheHookWhoeverAsks(t *testing.T) {
	f := composeFx(t)
	for _, who := range []string{"viewer", "operator", "envadmin", "root"} {
		if d := decide(t, f, user(who), "env-a", cleanFile); !d.Allowed || len(d.Findings) != 0 || d.Reason != "" {
			t.Errorf("%s: %+v, want allowed with no findings", who, d)
		}
	}
}

func TestComposeAnAutomationIsJudgedAsItsOwner(t *testing.T) {
	f := composeFx(t)
	if d := decide(t, f, rbac.OnBehalf("system:gitsync", "operator"), "env-a", privilegedFile); d.Allowed || d.Reason != rbac.ReasonPrivilegedCompose {
		t.Errorf("an automation for an Operator = %+v, want refused", d)
	}
	if d := decide(t, f, rbac.OnBehalf("system:gitsync", "envadmin"), "env-a", privilegedFile); !d.Allowed {
		t.Errorf("an automation for an env Admin = %+v, want allowed", d)
	}
	// An automation with no owner, a forged one and a plain system principal
	// hold no admin rights to borrow.
	for name, p := range map[string]*rbac.Principal{
		"no owner":       rbac.System("system:gitsync"),
		"forged":         {Type: rbac.PrincipalSystem, ID: "system:unknown", OnBehalfOf: "root"},
		"missing owner":  rbac.OnBehalf("system:gitsync", "nope"),
		"nil principal":  nil,
		"anonymous user": {Type: rbac.PrincipalUser, ID: "nobody"},
	} {
		if d := decide(t, f, p, "env-a", privilegedFile); d.Allowed || d.Reason != rbac.ReasonPrivilegedCompose {
			t.Errorf("%s: %+v, want refused", name, d)
		}
	}
}

func TestComposeAnUnreadableOrOversizedFileIsRefusedForEveryoneIncludingInstanceAdmins(t *testing.T) {
	f := composeFx(t)
	huge := "services:\n  web:\n    image: " + strings.Repeat("a", compose.MaxSize)
	for name, src := range map[string]string{
		"does not parse": "services: [unclosed\n  web: {",
		"too large":      huge,
	} {
		for _, who := range []string{"viewer", "envadmin", "root"} {
			d := decide(t, f, user(who), "env-a", src)
			if d.Allowed || d.Reason != rbac.ReasonComposeUnreadable {
				t.Errorf("%s by %s: %+v, want compose_unreadable", name, who, d)
			}
		}
	}
	if d, err := f.c.CheckComposeDeploy(context.Background(), user("root"), "env-a", bytes.Repeat([]byte("x"), compose.MaxSize+1)); err != nil || d.Reason != rbac.ReasonComposeUnreadable {
		t.Errorf("oversized bytes = %+v, %v", d, err)
	}
}

func TestComposeAnInterpolatedPrivilegedValueCannotBeVerifiedSoItNeedsEnvAdmin(t *testing.T) {
	f := composeFx(t)
	src := "services:\n  web:\n    image: nginx\n    privileged: ${PRIV}\n"
	if d := decide(t, f, user("operator"), "env-a", src); d.Allowed {
		t.Errorf("an unresolvable value was treated as safe for an Operator: %+v", d)
	}
	if d := decide(t, f, user("envadmin"), "env-a", src); !d.Allowed {
		t.Errorf("an env Admin was refused: %+v", d)
	}
}

func decideResolved(t *testing.T, f *fx, p *rbac.Principal, env, src string, vars map[string]string) rbac.ComposeDecision {
	t.Helper()
	d, err := f.c.CheckComposeDeployResolved(context.Background(), p, env, []byte(src), func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	})
	if err != nil {
		t.Fatalf("CheckComposeDeployResolved: %v", err)
	}
	return d
}

const dataBindFile = `services:
  web:
    image: nginx
    volumes:
      - "${DATA}:/data"
`

func TestComposeResolvedAnOperatorIsJudgedByTheValueAVariableGives(t *testing.T) {
	f := composeFx(t)
	d := decideResolved(t, f, user("operator"), "env-a", dataBindFile, map[string]string{"DATA": "/var/run/docker.sock"})
	if d.Allowed || d.Reason != rbac.ReasonPrivilegedCompose || len(d.Findings) != 1 {
		t.Fatalf("decision = %+v, want a privileged_compose refusal for the socket", d)
	}
	if d.Message != "services.web.volumes (line 5): /var/run/docker.sock" {
		t.Errorf("Message = %q, want the resolved source at the line it was written", d.Message)
	}
	if d := decideResolved(t, f, user("operator"), "env-a", dataBindFile, map[string]string{"DATA": "/srv/data"}); !d.Allowed || len(d.Findings) != 0 {
		t.Errorf("a data root through a variable: %+v, want allowed with no findings", d)
	}
}

func TestComposeResolvedAnAdminMayDeployItAndAnUnsetNameStillNeedsAdmin(t *testing.T) {
	f := composeFx(t)
	vars := map[string]string{"DATA": "/var/run/docker.sock"}
	for _, who := range []string{"envadmin", "root"} {
		if d := decideResolved(t, f, user(who), "env-a", dataBindFile, vars); !d.Allowed || len(d.Findings) != 1 {
			t.Errorf("%s: %+v, want allowed with the finding returned", who, d)
		}
	}
	if d := decideResolved(t, f, user("operator"), "env-a", dataBindFile, nil); d.Allowed {
		t.Errorf("an unset name was treated as safe for an Operator: %+v", d)
	}
}

func TestComposeResolvedAnAutomationIsJudgedAsItsOwner(t *testing.T) {
	f := composeFx(t)
	vars := map[string]string{"DATA": "/etc"}
	if d := decideResolved(t, f, rbac.OnBehalf("system:gitsync", "operator"), "env-a", dataBindFile, vars); d.Allowed {
		t.Errorf("an automation for an Operator = %+v, want refused", d)
	}
	if d := decideResolved(t, f, rbac.OnBehalf("system:gitsync", "envadmin"), "env-a", dataBindFile, vars); !d.Allowed {
		t.Errorf("an automation for an env Admin = %+v, want allowed", d)
	}
}

func TestComposeResolvedRefusesAnUnreadableFileForEveryone(t *testing.T) {
	f := composeFx(t)
	for _, who := range []string{"envadmin", "root"} {
		if d := decideResolved(t, f, user(who), "env-a", "services: [unclosed\n  web: {", nil); d.Allowed || d.Reason != rbac.ReasonComposeUnreadable {
			t.Errorf("%s: %+v, want compose_unreadable", who, d)
		}
	}
}
