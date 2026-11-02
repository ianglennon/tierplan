// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package lint_test

import (
	"context"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/lint"
)

// checkOf runs the whole check of a compose file on a target, the way the editor does.
func checkOf(t *testing.T, target stacks.Target, compose string) stacks.CheckResult {
	t.Helper()
	return stacks.Check(context.Background(), stacks.CheckInput{
		Name: "probe", Target: target,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}},
	})
}

// byCode are the problems with one code.
func byCode(res stacks.CheckResult, code string) stacks.Problems {
	var out stacks.Problems
	for _, p := range res.Problems {
		if p.Code == code {
			out = append(out, p)
		}
	}
	return out
}

func TestTargetAndSeverityValuesAreStacksValues(t *testing.T) {
	t.Parallel()
	// stacks converts the values by name, so they must be the same strings.
	if string(lint.TargetSwarm) != string(stacks.TargetSwarm) || string(lint.TargetStandalone) != string(stacks.TargetStandalone) {
		t.Errorf("lint targets %q %q, stacks targets %q %q", lint.TargetSwarm, lint.TargetStandalone, stacks.TargetSwarm, stacks.TargetStandalone)
	}
	if string(lint.SeverityError) != string(stacks.SeverityError) || string(lint.SeverityWarning) != string(stacks.SeverityWarning) {
		t.Errorf("lint severities %q %q, stacks severities %q %q", lint.SeverityError, lint.SeverityWarning, stacks.SeverityError, stacks.SeverityWarning)
	}
}

func TestSwarmWarnsAtTheLineOfABuildThatHasAnImage(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: registry.example/web:1.2
    build: .
`
	res := checkOf(t, stacks.TargetSwarm, compose)
	if len(res.Problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one", res.Problems)
	}
	p := res.Problems[0]
	want := stacks.Problem{
		Severity: stacks.SeverityWarning, Code: stacks.CodeIgnoredOnTarget, File: stacks.ComposeFile, Key: "services.web.build",
		Message: "`build` is ignored on Swarm. Build and push the image, then set `image`.",
		Line:    4, Col: 5, EndLine: 4, EndCol: 10,
	}
	if p != want {
		t.Errorf("problem = %+v\nwant      %+v", p, want)
	}
	if res.Problems.Errors() != 0 || res.Loaded == nil {
		t.Errorf("a warning must not block: errors %d, loaded %v", res.Problems.Errors(), res.Loaded)
	}
}

func TestStandaloneSaysBuildIsIgnoredBecauseTierPlanDoesNotBuild(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: registry.example/web:1.2
    build: .
`
	res := checkOf(t, stacks.TargetStandalone, compose)
	if len(res.Problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one", res.Problems)
	}
	p := res.Problems[0]
	if p.Severity != stacks.SeverityWarning || p.Code != stacks.CodeIgnoredOnTarget || p.Line != 4 {
		t.Errorf("problem = %+v, want a warning ignored_on_target at line 4", p)
	}
	if want := "`build` is ignored. TierPlan doesn't build images; it deploys the image named in `image`."; p.Message != want {
		t.Errorf("message = %q, want %q", p.Message, want)
	}
}

func TestBuildWithoutAnImageIsABlockingErrorOnBothTargets(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    build: .
`
	for _, target := range []stacks.Target{stacks.TargetSwarm, stacks.TargetStandalone} {
		res := checkOf(t, target, compose)
		if len(res.Problems) != 1 {
			t.Fatalf("%s: problems = %+v, want exactly one", target, res.Problems)
		}
		p := res.Problems[0]
		if p.Severity != stacks.SeverityError || p.Code != stacks.CodeBuildWithoutImage || p.Line != 3 {
			t.Errorf("%s: problem = %+v, want an error build_without_image at line 3", target, p)
		}
		if want := "web has build but no image. Build and push the image, then set image."; p.Message != want {
			t.Errorf("%s: message = %q, want %q", target, p.Message, want)
		}
	}
}

func TestSwarmIgnoresContainerNameWithAWarningAndStandaloneDoesNot(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: registry.example/web:1.2
    container_name: front
`
	swarm := checkOf(t, stacks.TargetSwarm, compose)
	got := byCode(swarm, stacks.CodeIgnoredOnTarget)
	if len(got) != 1 || got[0].Severity != stacks.SeverityWarning || got[0].Line != 4 {
		t.Fatalf("swarm problems = %+v, want one warning at line 4", swarm.Problems)
	}
	if want := "`container_name` is ignored on Swarm. A swarm names its containers. Remove the key."; got[0].Message != want {
		t.Errorf("message = %q, want %q", got[0].Message, want)
	}
	if standalone := checkOf(t, stacks.TargetStandalone, compose); len(standalone.Problems) != 0 {
		t.Errorf("standalone problems = %+v, want none", standalone.Problems)
	}
}

func TestSwarmRefusesAKeyItCannotDeploy(t *testing.T) {
	t.Parallel()
	const compose = `services:
  db:
    image: registry.example/db:1
  web:
    image: registry.example/web:1.2
    volumes_from:
      - db
`
	res := checkOf(t, stacks.TargetSwarm, compose)
	got := byCode(res, stacks.CodeBlockingKey)
	if len(got) != 1 || got[0].Severity != stacks.SeverityError || got[0].Line != 6 {
		t.Fatalf("problems = %+v, want one blocking_key error at line 6", res.Problems)
	}
	if want := "`volumes_from` can't be deployed to Swarm. Define the volume under `volumes` and mount it in each service."; got[0].Message != want {
		t.Errorf("message = %q, want %q", got[0].Message, want)
	}
	if res.Problems.Errors() != 1 {
		t.Errorf("errors = %d, want 1: an error blocks", res.Problems.Errors())
	}
}

func TestAKeyWrittenThroughAnAnchorIsFlaggedWhereItIsWritten(t *testing.T) {
	t.Parallel()
	const compose = `x-base: &base
  image: registry.example/web:1.2
  container_name: front
services:
  web:
    <<: *base
`
	res := checkOf(t, stacks.TargetSwarm, compose)
	got := byCode(res, stacks.CodeIgnoredOnTarget)
	if len(got) != 1 || got[0].Line != 3 {
		t.Fatalf("problems = %+v, want one warning at line 3, where the key is written", res.Problems)
	}
}

func TestAMovingImageIsAWarningAtItsLineOnBothTargets(t *testing.T) {
	t.Parallel()
	for name, image := range map[string]string{"no tag": "nginx", "latest": "nginx:latest", "a registry with a port": "registry:5000/app"} {
		for _, target := range []stacks.Target{stacks.TargetSwarm, stacks.TargetStandalone} {
			t.Run(name+" on "+string(target), func(t *testing.T) {
				t.Parallel()
				res := checkOf(t, target, "services:\n  web:\n    image: "+image+"\n")
				if len(res.Problems) != 1 {
					t.Fatalf("problems = %+v, want exactly one", res.Problems)
				}
				p := res.Problems[0]
				want := "“" + image + "” can move: it has no tag or uses :latest. TierPlan keeps the running digest on redeploys, but picks one when the service is new or the reference changes."
				if p.Severity != stacks.SeverityWarning || p.Code != stacks.CodeMovingTag || p.Line != 3 || p.Message != want {
					t.Errorf("problem = %+v, want a moving_tag warning at line 3 saying %q", p, want)
				}
				if res.Problems.Errors() != 0 || res.Loaded == nil {
					t.Error("a moving tag must not block")
				}
			})
		}
	}
}

func TestAPinnedImageHasNoMovingTagWarning(t *testing.T) {
	t.Parallel()
	for _, image := range []string{"nginx:1.27", "nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		res := checkOf(t, stacks.TargetSwarm, "services:\n  web:\n    image: "+image+"\n")
		if len(res.Problems) != 0 {
			t.Errorf("%s: problems = %+v, want none", image, res.Problems)
		}
	}
}

func TestAnImageBuiltFromVariablesIsJudgedOnItsValue(t *testing.T) {
	t.Parallel()
	res := stacks.Check(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{
			Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte("services:\n  web:\n    image: ${IMAGE}\n")}},
			Vars:  []stacks.Var{{Name: "IMAGE", Value: "nginx"}},
		},
	})
	if got := byCode(res, stacks.CodeMovingTag); len(got) != 1 || got[0].Line != 3 {
		t.Errorf("problems = %+v, want a moving_tag warning at line 3", res.Problems)
	}
}

func TestAServiceWithBuildAndAMovingImageGetsBothWarnings(t *testing.T) {
	t.Parallel()
	res := checkOf(t, stacks.TargetSwarm, "services:\n  web:\n    image: nginx\n    build: .\n")
	if len(byCode(res, stacks.CodeMovingTag)) != 1 || len(byCode(res, stacks.CodeIgnoredOnTarget)) != 1 || res.Problems.Errors() != 0 {
		t.Errorf("problems = %+v, want the moving tag and the build warning", res.Problems)
	}
}

// indexed lints a compose file the schema would refuse, by indexing the bytes directly.
func indexed(t *testing.T, target lint.Target, compose string, networks ...string) []lint.Finding {
	t.Helper()
	ix, err := stacks.NewPositionIndex([]byte(compose))
	if err != nil {
		t.Fatal(err)
	}
	p := &types.Project{Services: types.Services{}, Networks: types.Networks{}}
	for _, n := range networks {
		p.Networks[n] = types.NetworkConfig{Name: n}
	}
	return lint.Lint(lint.Input{Target: target, Project: p, Index: ix})
}

func TestANetworkTheAgentRefusesIsAnErrorNamingTheKeyOnBothTargets(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		compose string
		key     string
		line    int
	}{
		"ipam options":  {"networks:\n  n:\n    ipam:\n      options:\n        a: b\n", "networks.n.ipam.options", 4},
		"aux addresses": {"networks:\n  n:\n    ipam:\n      config:\n        - subnet: 10.1.0.0/24\n          aux_addresses:\n            h: 10.1.0.5\n", "networks.n.ipam.config.0.aux_addresses", 6},
		"config_from":   {"networks:\n  n:\n    config_from:\n      network: base\n", "networks.n.config_from", 3},
		"ingress":       {"networks:\n  n:\n    ingress: true\n", "networks.n.ingress", 3},
	} {
		for _, target := range []lint.Target{lint.TargetSwarm, lint.TargetStandalone} {
			t.Run(name+" on "+string(target), func(t *testing.T) {
				t.Parallel()
				got := indexed(t, target, tc.compose, "n")
				if len(got) != 1 {
					t.Fatalf("findings = %+v, want exactly one", got)
				}
				f := got[0]
				if f.Severity != lint.SeverityError || f.Code != lint.CodeNetworkOptionUnsupported || f.Key != tc.key || f.Pos.Line != tc.line {
					t.Errorf("finding = %+v, want an error network_option_unsupported on %s at line %d", f, tc.key, tc.line)
				}
				if !strings.Contains(f.Message, "`"+tc.key+"`") {
					t.Errorf("message %q must name the key", f.Message)
				}
			})
		}
	}
}

func TestANetworkTheAgentAcceptsHasNoFinding(t *testing.T) {
	t.Parallel()
	for name, compose := range map[string]string{
		"empty options":  "networks:\n  n:\n    ipam:\n      options: {}\n",
		"a plain pool":   "networks:\n  n:\n    ipam:\n      config:\n        - subnet: 10.1.0.0/24\n          gateway: 10.1.0.1\n          ip_range: 10.1.0.0/25\n",
		"ingress false":  "networks:\n  n:\n    ingress: false\n",
		"empty aux":      "networks:\n  n:\n    ipam:\n      config:\n        - subnet: 10.1.0.0/24\n          aux_addresses: {}\n",
		"no ipam at all": "networks:\n  n:\n    driver: overlay\n",
	} {
		if got := indexed(t, lint.TargetSwarm, compose, "n"); len(got) != 0 {
			t.Errorf("%s: findings = %+v, want none", name, got)
		}
	}
}

func TestAnExternalNetworkIsNotJudgedBecauseTierPlanNeverCreatesIt(t *testing.T) {
	t.Parallel()
	ix, err := stacks.NewPositionIndex([]byte("networks:\n  n:\n    ipam:\n      options:\n        a: b\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := &types.Project{Networks: types.Networks{"n": {Name: "n", External: true}}}
	if got := lint.Lint(lint.Input{Target: lint.TargetSwarm, Project: p, Index: ix}); len(got) != 0 {
		t.Errorf("findings = %+v, want none", got)
	}
}
