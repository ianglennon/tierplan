// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package lint_test

import (
	"context"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/lint"
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

func TestStandaloneSaysBuildIsIgnoredBecauseBayplanDoesNotBuild(t *testing.T) {
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
	if want := "`build` is ignored. Bayplan doesn't build images; it deploys the image named in `image`."; p.Message != want {
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
