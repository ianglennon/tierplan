// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"cmp"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/convert"
	"github.com/ianglennon/tierplan/internal/stacks/rebuild"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// A stack made of several compose files (plan 09-02, D-06): every check that runs before a deploy reads all
// of them, and a stack of two files is judged as the one file its merge would be.

const (
	mfBase     = "services:\n  web:\n    image: nginx:1.26-alpine\n"
	mfOverride = "services:\n  web:\n    image: nginx:1.27-alpine\n"

	mfBaseVol = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	mfBindDoc = "services:\n  web:\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n"
	mfBindOne = "services:\n  web:\n    image: nginx:1.27-alpine\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n"
)

// mfCopy is a working copy of the compose files base.yaml and override.yaml, in that order.
func mfCopy(base, override string, vars ...stacks.Var) stacks.WorkingCopy {
	return stacks.WorkingCopy{
		Files:        []stacks.File{{Path: "base.yaml", Content: []byte(base)}, {Path: "override.yaml", Content: []byte(override)}},
		Vars:         vars,
		ComposeFiles: []string{"base.yaml", "override.yaml"},
	}
}

// mfMerged is a working copy of the one compose.yaml holding the merge of mfCopy(base, override).
func mfMerged(merged string, vars ...stacks.Var) stacks.WorkingCopy {
	return stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(merged)}}, Vars: vars}
}

// policyOf is the host-access decision for copy, as the caller with principal would get it at a review.
func (r *swarmRig) policyOf(t *testing.T, role string, copy stacks.WorkingCopy) stackops.PolicyResult {
	t.Helper()
	res := stacks.Check(testsupport.Context(t), stacks.CheckInput{Name: "web", Target: stacks.TargetSwarm, Copy: copy})
	if res.Loaded == nil {
		t.Fatalf("the copy does not load: %+v", res.Problems)
	}
	plan, err := convert.Convert(res.Loaded.Project, convert.Options{StackName: "web", StackID: "stack-web", Objects: stackops.PlannedObjects("web", nil)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.deps.Policy(testsupport.Context(t), stackops.Caller{Principal: r.principal(t, "p-"+role, role), Actor: role}, r.envID, copy, plan)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// shape is a problem without where it is: its code, severity, key and words.
type shape struct{ Severity, Code, Key, Message string }

func shapesOf(ps stacks.Problems) []shape {
	out := make([]shape, 0, len(ps))
	for _, p := range ps {
		out = append(out, shape{string(p.Severity), p.Code, p.Key, p.Message})
	}
	slices.SortFunc(out, func(a, b shape) int {
		return cmp.Or(cmp.Compare(a.Code, b.Code), cmp.Compare(a.Key, b.Key), cmp.Compare(a.Message, b.Message))
	})
	return out
}

func namesOf(refs []stacks.VarRef) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

func entryTexts(es []stackops.PolicyEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Text)
	}
	slices.Sort(out)
	return out
}

func TestMultiFileInvariant(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	tag := stacks.Var{Name: "TAG", Value: "1.27-alpine"}
	cases := []struct {
		name, base, override, merged string
		vars                         []stacks.Var
	}{
		{
			name: "the override replaces the image", base: mfBase, override: mfOverride, merged: mfOverride,
		},
		{
			name: "a bind mount only in the override",
			base: mfBaseVol, override: mfBindDoc, merged: mfBindOne,
		},
		{
			name:     "privileged in the base and an environment in the override",
			base:     "services:\n  web:\n    image: nginx:1.27-alpine\n    privileged: true\n",
			override: "services:\n  web:\n    environment:\n      A: \"1\"\n",
			merged:   "services:\n  web:\n    image: nginx:1.27-alpine\n    privileged: true\n    environment:\n      A: \"1\"\n",
		},
		{
			name:     "variables in both files",
			base:     "services:\n  web:\n    image: nginx:${TAG}\n",
			override: "services:\n  web:\n    environment:\n      PORT: ${PORT:-80}\n",
			merged:   "services:\n  web:\n    image: nginx:${TAG}\n    environment:\n      PORT: ${PORT:-80}\n",
			vars:     []stacks.Var{tag},
		},
		{
			name:     "a variable with no value in the override",
			base:     "services:\n  web:\n    image: nginx:1.27-alpine\n",
			override: "services:\n  web:\n    environment:\n      NEED: ${NEED}\n",
			merged:   "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      NEED: ${NEED}\n",
		},
		{
			name:     "a host network in the override",
			base:     "services:\n  web:\n    image: nginx:1.27-alpine\n",
			override: "services:\n  web:\n    network_mode: host\n",
			merged:   "services:\n  web:\n    image: nginx:1.27-alpine\n    network_mode: host\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			multi, one := mfCopy(tc.base, tc.override, tc.vars...), mfMerged(tc.merged, tc.vars...)
			ctx := testsupport.Context(t)
			got := stacks.Check(ctx, stacks.CheckInput{Name: "web", Target: stacks.TargetSwarm, Copy: multi})
			want := stacks.Check(ctx, stacks.CheckInput{Name: "web", Target: stacks.TargetSwarm, Copy: one})
			if g, w := shapesOf(got.Problems), shapesOf(want.Problems); !slices.Equal(g, w) {
				t.Errorf("Check problems differ:\n multi  %+v\n merged %+v", g, w)
			}
			if (got.Loaded == nil) != (want.Loaded == nil) {
				t.Fatalf("loaded: multi %v, merged %v", got.Loaded != nil, want.Loaded != nil)
			}
			if g, w := namesOf(stacks.Refs(multi)), namesOf(stacks.Refs(one)); !slices.Equal(g, w) {
				t.Errorf("Refs names = %v, want %v", g, w)
			}
			if got.Loaded == nil {
				return
			}
			for _, role := range []string{"operator", "admin"} {
				g, w := rig.policyOf(t, role, multi), rig.policyOf(t, role, one)
				if g.Refused != w.Refused || !slices.Equal(g.Kinds, w.Kinds) || !slices.Equal(entryTexts(g.Entries), entryTexts(w.Entries)) {
					t.Errorf("%s policy differs:\n multi  %+v\n merged %+v", role, g, w)
				}
			}
		})
	}
}

func TestMultiFilePolicyBindInOverride(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	copy := mfCopy(mfBaseVol, mfBindDoc)

	res := rig.policyOf(t, "operator", copy)
	if !res.Refused || !slices.Equal(res.Kinds, []string{stackops.KindBindMounts}) {
		t.Fatalf("an Operator's policy = %+v, want the bind mount in override.yaml refused", res)
	}
	if len(res.Entries) != 1 || res.Entries[0].File != "override.yaml" || res.Entries[0].Line != 4 {
		t.Errorf("entries = %+v, want one at override.yaml line 4", res.Entries)
	}
	if len(res.Problems) != 1 || res.Problems[0].File != "override.yaml" || res.Problems[0].Line != 4 || res.Problems[0].Code != stackops.CodeNeedsAdmin {
		t.Errorf("problems = %+v, want needs_admin at override.yaml line 4", res.Problems)
	}
	if res := rig.policyOf(t, "admin", copy); res.Refused {
		t.Errorf("an env Admin was refused: %+v", res)
	}
	// The same bind in the base is found too.
	if res := rig.policyOf(t, "operator", mfCopy(mfBindOne, "services:\n  web:\n    environment:\n      A: \"1\"\n")); !res.Refused || res.Entries[0].File != "base.yaml" {
		t.Errorf("a bind in the base: %+v, want it refused at base.yaml", res)
	}
	// And so is a setting the host-access list shows to anybody.
	list, err := rig.deps.HostAccess(copy, convert.Plan{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 1 || list.Entries[0].File != "override.yaml" {
		t.Errorf("HostAccess entries = %+v, want the bind at override.yaml", list.Entries)
	}
}

func TestMultiFileReviewSeesAnImageChangedOnlyInTheOverride(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	review := func(copy stacks.WorkingCopy) stackops.Review {
		t.Helper()
		rv, err := rig.deps.Review(testsupport.Context(t), stackops.ReviewInput{
			EnvID: rig.envID, Stack: stackops.Stack{ID: "stack-web", Name: "web", Target: stacks.TargetSwarm},
			Copy: copy, DraftVersion: 3,
			Caller: stackops.Caller{Principal: rig.principal(t, "root", ""), Actor: "root"},
		})
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		return rv
	}
	multi := review(mfCopy(mfBase, mfOverride))
	one := review(mfMerged(mfOverride))
	if multi.Problems.Errors() != 0 {
		t.Fatalf("problems = %+v", multi.Problems)
	}
	// The override's image is the one pinned.
	want := "nginx:1.27-alpine@" + dockertest.FakeDigestOf("docker.io/library/nginx:1.27-alpine")
	if multi.Pins["web_web"] != want {
		t.Errorf("pins = %v, want web_web at %s", multi.Pins, want)
	}
	if len(multi.Services) != len(one.Services) || multi.Summary != one.Summary || len(multi.Pins) != len(one.Pins) {
		t.Fatalf("review of two files = %+v\nreview of the merged file = %+v", multi, one)
	}
	for i := range one.Services {
		if multi.Services[i].Short != one.Services[i].Short || multi.Services[i].Change != one.Services[i].Change {
			t.Errorf("service %d = %+v, want %+v", i, multi.Services[i], one.Services[i])
		}
	}
	for name, pin := range one.Pins {
		if multi.Pins[name] != pin {
			t.Errorf("pin %s = %s, want %s", name, multi.Pins[name], pin)
		}
	}
	// A problem the review adds after the check is put in the compose file that holds its key, not in a
	// compose.yaml the stack does not have.
	bad := review(mfCopy(mfBase, "services:\n  web:\n    image: nginx:1.27-alpine\n    networks: [outer]\nnetworks:\n  outer:\n    external: true\n    name: no-such-network\n"))
	var found bool
	for _, p := range bad.Problems {
		if p.File == stacks.ComposeFile {
			t.Errorf("problem %+v is in compose.yaml, which the stack does not have", p)
		}
		if p.Code == stackops.CodeExternalMissingProblem && p.File == "override.yaml" && p.Line > 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("problems = %+v, want the missing external network at a line of override.yaml", bad.Problems)
	}
}

func TestImageTextOfFilesLetsALaterFileWin(t *testing.T) {
	t.Parallel()
	got := rebuild.ImageTextOfFiles(
		[]byte("services:\n  web:\n    image: nginx:1.26\n  db:\n    image: postgres:16\n  cache:\n    image: redis:7\n"),
		[]byte("services:\n  web:\n    image: nginx:${TAG}\n  cache:\n    image: \"redis 7\"\n"),
	)
	want := map[string]string{"web": "nginx:${TAG}", "db": "postgres:16"}
	if len(got) != len(want) || got["web"] != want["web"] || got["db"] != want["db"] {
		t.Errorf("ImageTextOfFiles = %v, want %v: the second file wins, and a later image that is not one plain word leaves the service out", got, want)
	}
	if one := rebuild.ImageTextOf([]byte("services:\n  web:\n    image: nginx:1.26\n")); one["web"] != "nginx:1.26" {
		t.Errorf("ImageTextOf = %v", one)
	}
}
