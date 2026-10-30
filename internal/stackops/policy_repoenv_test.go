// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/convert"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// repoEnvCompose reads its host access from variables with a safe default, so a policy that does not see the
// committed .env judges the defaults and not what a deploy runs.
const repoEnvCompose = `services:
  web:
    image: nginx:1.27
    privileged: ${P:-false}
    network_mode: ${NM:-bridge}
`

func repoEnvCopy(env string, originOfEnv stacks.FileOrigin, vars ...stacks.Var) stacks.WorkingCopy {
	return stacks.WorkingCopy{
		Files: []stacks.File{
			{Path: stacks.ComposeFile, Content: []byte(repoEnvCompose), Origin: stacks.OriginRepo},
			{Path: stacks.DotEnvFile, Content: []byte(env), Origin: originOfEnv},
		},
		Vars: vars,
	}
}

func policyFor(t *testing.T, rig *swarmRig, role string, copy stacks.WorkingCopy) stackops.PolicyResult {
	t.Helper()
	out, err := rig.deps.Policy(testsupport.Context(t), stackops.Caller{Principal: rig.principal(t, "p-"+role, role), Actor: role}, rig.envID, copy, convert.Plan{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// CR-C01: the policy judges the values the load interpolates with, the repository .env under the table.
func TestPolicyJudgesTheRepositoryEnvTheLoadInterpolatesWith(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	copy := repoEnvCopy("P=true\nNM=host\n", stacks.OriginRepo)

	res := policyFor(t, rig, "operator", copy)
	if !res.Refused {
		t.Fatalf("an Operator's policy = %+v, want privileged and host networking from the repository .env refused", res)
	}
	for _, kind := range []string{stackops.KindPrivileges, stackops.KindHostNetworking} {
		if !slices.Contains(res.Kinds, kind) {
			t.Errorf("kinds = %v, want %q", res.Kinds, kind)
		}
	}
	if len(res.Problems) == 0 || res.Problems[0].Code != stackops.CodeNeedsAdmin {
		t.Errorf("problems = %+v, want needs_admin", res.Problems)
	}
	if res := policyFor(t, rig, "admin", copy); res.Refused {
		t.Errorf("an env Admin was refused: %+v", res)
	}

	list, err := rig.deps.HostAccess(copy, convert.Plan{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(list.Kinds, stackops.KindPrivileges) || !slices.Contains(list.Kinds, stackops.KindHostNetworking) {
		t.Errorf("HostAccess kinds = %v, want privileges and host networking from the .env", list.Kinds)
	}
	if len(list.Entries) == 0 {
		t.Error("HostAccess lists nothing for a .env that turns on privileged")
	}
}

func TestPolicyTableValueBeatsTheRepositoryEnv(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	// A non-empty table value wins, as in the load.
	copy := repoEnvCopy("P=true\nNM=host\n", stacks.OriginRepo, stacks.Var{Name: "P", Value: "false"}, stacks.Var{Name: "NM", Value: "bridge"})
	if res := policyFor(t, rig, "operator", copy); res.Refused {
		t.Errorf("the table says false and bridge, policy = %+v", res)
	}
	// A blank table value is no value, so the .env shows through.
	blank := repoEnvCopy("P=true\n", stacks.OriginRepo, stacks.Var{Name: "P", Value: ""})
	if res := policyFor(t, rig, "operator", blank); !res.Refused {
		t.Errorf("a blank table value hides the .env from the policy: %+v", res)
	}
	// An .env that is not the repository's supplies nothing, as in the load.
	overlay := repoEnvCopy("P=true\n", stacks.OriginOverlay)
	if res := policyFor(t, rig, "operator", overlay); res.Refused {
		t.Errorf("an overlay .env was judged: %+v", res)
	}
}

func TestPolicyFailsClosedOnAnUnreadableRepositoryEnv(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	copy := repoEnvCopy(strings.Repeat("A=1\n", stacks.MaxFileBytes), stacks.OriginRepo)

	res := policyFor(t, rig, "operator", copy)
	if !res.Refused || len(res.Problems) != 1 || res.Problems[0].Code != stackops.CodePolicyUnreadable || res.Problems[0].File != stacks.DotEnvFile {
		t.Fatalf("policy = %+v, want one policy_unreadable problem for .env", res)
	}
	list, err := rig.deps.HostAccess(copy, convert.Plan{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 1 || list.Entries[0].File != stacks.DotEnvFile {
		t.Errorf("HostAccess entries = %+v, want the unreadable .env listed", list.Entries)
	}
}
