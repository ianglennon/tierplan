// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/rbac/swarmspec"
)

const privilegedService = `{"Name":"web","TaskTemplate":{"ContainerSpec":{"Image":"nginx","Mounts":[
 {"Type":"bind","Source":"/var/run/docker.sock","Target":"/sock"},
 {"Type":"bind","Source":"/etc","Target":"/host-etc"}]}}}`

const cleanService = `{"Name":"web","TaskTemplate":{"ContainerSpec":{"Image":"nginx","Mounts":[{"Type":"bind","Source":"/srv/web","Target":"/data"}]}}}`

func decideService(t *testing.T, f *fx, p *rbac.Principal, env, src string, resolve swarmspec.NetworkResolver) rbac.ServiceDecision {
	t.Helper()
	d, err := f.c.CheckServiceSpec(context.Background(), p, env, []byte(src), resolve)
	if err != nil {
		t.Fatalf("CheckServiceSpec: %v", err)
	}
	return d
}

func TestCheckServiceSpecACleanSpecIsAllowedForEveryoneWhoAsks(t *testing.T) {
	f := composeFx(t)
	for _, who := range []string{"viewer", "operator", "envadmin", "root"} {
		d := decideService(t, f, user(who), "env-a", cleanService, nil)
		if !d.Allowed || len(d.Findings) != 0 || d.Reason != "" || d.Message != "" {
			t.Errorf("%s: %+v, want allowed with no findings", who, d)
		}
	}
}

func TestCheckServiceSpecAnOperatorIsRefusedAndTheMessageNamesEachFindingOnItsOwnLine(t *testing.T) {
	f := composeFx(t)
	d := decideService(t, f, user("operator"), "env-a", privilegedService, nil)
	if d.Allowed || d.Reason != rbac.ReasonPrivilegedService || len(d.Findings) != 2 {
		t.Fatalf("decision = %+v, want a privileged_service refusal with two findings", d)
	}
	lines := strings.Split(d.Message, "\n")
	if len(lines) != 2 ||
		lines[0] != "TaskTemplate.ContainerSpec.Mounts[0].Source: /var/run/docker.sock" ||
		lines[1] != "TaskTemplate.ContainerSpec.Mounts[1].Source: /etc" {
		t.Errorf("Message = %q", d.Message)
	}
}

func TestCheckServiceSpecAnEnvAdminAndAnInstanceAdminMayHaveItAndStillGetTheFindings(t *testing.T) {
	f := composeFx(t)
	for _, who := range []string{"envadmin", "root"} {
		d := decideService(t, f, user(who), "env-a", privilegedService, nil)
		if !d.Allowed || len(d.Findings) != 2 {
			t.Errorf("%s: %+v, want allowed with the findings returned", who, d)
		}
	}
	f.addUser(t, "otheradmin")
	f.assign(t, "user", "otheradmin", "admin", "env-b")
	if d := decideService(t, f, user("otheradmin"), "env-a", privilegedService, nil); d.Allowed {
		t.Errorf("an Admin of env-b was allowed a privileged spec on env-a: %+v", d)
	}
}

func TestCheckServiceSpecTheResolverReachesTheHostNetworkRule(t *testing.T) {
	f := composeFx(t)
	src := `{"Name":"web","TaskTemplate":{"ContainerSpec":{"Image":"nginx"},"Networks":[{"Target":"abc123"}]}}`
	resolve := func(string) (string, string, bool) { return "host", "host", true }
	if d := decideService(t, f, user("operator"), "env-a", src, resolve); d.Allowed || len(d.Findings) != 1 {
		t.Errorf("operator with a host network by id: %+v", d)
	}
	if d := decideService(t, f, user("operator"), "env-a", src, nil); !d.Allowed {
		t.Errorf("without a resolver an ID is not known to be the host network: %+v", d)
	}
}

func TestCheckServiceSpecUndecodableBytesAreRefusedForEveryoneIncludingInstanceAdmins(t *testing.T) {
	f := composeFx(t)
	for name, src := range map[string]string{
		"not json":      `{"Name":`,
		"unknown field": `{"Name":"web","Bogus":1}`,
		"wrong type":    `{"Name":"web","TaskTemplate":{"ContainerSpec":{"Image":5}}}`,
		"trailing data": cleanService + ` {}`,
		"empty":         ``,
	} {
		for _, who := range []string{"viewer", "envadmin", "root"} {
			d := decideService(t, f, user(who), "env-a", src, nil)
			if d.Allowed || d.Reason != rbac.ReasonServiceSpecUnreadable || d.Message == "" {
				t.Errorf("%s by %s: %+v, want service_spec_unreadable", name, who, d)
			}
		}
	}
}

func TestCheckServiceSpecAnAutomationIsJudgedAsItsOwner(t *testing.T) {
	f := composeFx(t)
	if d := decideService(t, f, rbac.OnBehalf("system:gitsync", "operator"), "env-a", privilegedService, nil); d.Allowed || d.Reason != rbac.ReasonPrivilegedService {
		t.Errorf("an automation for an Operator = %+v, want refused", d)
	}
	if d := decideService(t, f, rbac.OnBehalf("system:gitsync", "envadmin"), "env-a", privilegedService, nil); !d.Allowed {
		t.Errorf("an automation for an env Admin = %+v, want allowed", d)
	}
	for name, p := range map[string]*rbac.Principal{
		"no owner":      rbac.System("system:gitsync"),
		"missing owner": rbac.OnBehalf("system:gitsync", "nope"),
		"nil principal": nil,
	} {
		if d := decideService(t, f, p, "env-a", privilegedService, nil); d.Allowed || d.Reason != rbac.ReasonPrivilegedService {
			t.Errorf("%s: %+v, want refused", name, d)
		}
	}
}
