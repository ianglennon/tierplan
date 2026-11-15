// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// ingressCheck runs Check on compose for target, with policy as the environment's world (nil for none).
func ingressCheck(t *testing.T, compose string, target stacks.Target, policy *stacks.IngressPolicy) stacks.CheckResult {
	t.Helper()
	return stacks.Check(context.Background(), stacks.CheckInput{
		Name: "probe", Target: target, Copy: copyOf(compose), IngressPolicy: policy,
	})
}

// ingressProblems are the problems of res whose code is an ingress code.
func ingressProblems(res stacks.CheckResult) stacks.Problems {
	var out stacks.Problems
	for _, p := range res.Problems {
		if strings.HasPrefix(p.Code, "ingress_") {
			out = append(out, p)
		}
	}
	return out
}

// oneIngress fails unless res holds exactly one ingress problem, and returns it.
func oneIngress(t *testing.T, res stacks.CheckResult) stacks.Problem {
	t.Helper()
	ps := ingressProblems(res)
	if len(ps) != 1 {
		t.Fatalf("ingress problems = %+v (all: %+v), want exactly one", ps, res.Problems)
	}
	return ps[0]
}

func TestCheckIngressMapFormIsPositionedOnTheLabel(t *testing.T) {
	t.Parallel()
	const swarm = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "abc"
`
	p := oneIngress(t, ingressCheck(t, swarm, stacks.TargetSwarm, nil))
	if p.Code != ingress.CodePortInvalid || p.Severity != stacks.SeverityError || p.File != stacks.ComposeFile {
		t.Errorf("problem = %+v, want an error ingress_port_invalid in compose.yaml", p)
	}
	if p.Line != 7 || p.Col != 9 {
		t.Errorf("position = %d:%d, want 7:9 (the tierplan.ingress.port key)", p.Line, p.Col)
	}

	const standalone = `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: app.example.com
      tierplan.ingress.port: "abc"
`
	p = oneIngress(t, ingressCheck(t, standalone, stacks.TargetStandalone, nil))
	if p.Code != ingress.CodePortInvalid || p.Line != 6 || p.Col != 7 {
		t.Errorf("standalone problem = %+v, want ingress_port_invalid at 6:7", p)
	}
}

func TestCheckIngressListFormIsPositionedOnTheBlock(t *testing.T) {
	t.Parallel()
	const swarm = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        - tierplan.ingress.host=app.example.com
        - tierplan.ingress.port=80
        - tierplan.ingress.path=/a{b}
`
	p := oneIngress(t, ingressCheck(t, swarm, stacks.TargetSwarm, nil))
	if p.Code != ingress.CodePathInvalid || p.Severity != stacks.SeverityError {
		t.Errorf("problem = %+v, want an error ingress_path_invalid", p)
	}
	if p.Line != 5 || p.Col != 7 || p.Key != "services.web.deploy.labels" {
		t.Errorf("position = %d:%d key %q, want the labels block at 5:7", p.Line, p.Col, p.Key)
	}

	const standalone = `services:
  web:
    image: nginx:1.27
    labels:
      - tierplan.ingress.host=app.example.com
      - tierplan.ingress.port=80
      - tierplan.ingress.path=/a{b}
`
	p = oneIngress(t, ingressCheck(t, standalone, stacks.TargetStandalone, nil))
	if p.Code != ingress.CodePathInvalid || p.Line != 4 || p.Col != 5 || p.Key != "services.web.labels" {
		t.Errorf("standalone problem = %+v, want ingress_path_invalid on the labels block at 4:5", p)
	}
}

func TestCheckIngressParserCodes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		labels string
		code   string
		line   int
	}{
		"an unknown key":     {"tierplan.ingress.hostt: x.example.com\n        tierplan.ingress.host: a.example.com\n        tierplan.ingress.port: \"80\"", ingress.CodeLabelUnknown, 6},
		"a bad hostname":     {"tierplan.ingress.host: \"not a host\"\n        tierplan.ingress.port: \"80\"", ingress.CodeHostInvalid, 6},
		"a missing port":     {"tierplan.ingress.host: a.example.com", ingress.CodePortMissing, 0},
		"a bad scheme":       {"tierplan.ingress.host: a.example.com\n        tierplan.ingress.port: \"80\"\n        tierplan.ingress.scheme: ftp", ingress.CodeSchemeInvalid, 8},
		"strip on the root":  {"tierplan.ingress.host: a.example.com\n        tierplan.ingress.port: \"80\"\n        tierplan.ingress.strip: \"true\"", ingress.CodeStripRoot, 8},
		"a bad route name":   {"tierplan.ingress.Api.host: a.example.com", ingress.CodeRouteNameInvalid, 6},
		"a bad strip value":  {"tierplan.ingress.host: a.example.com\n        tierplan.ingress.port: \"80\"\n        tierplan.ingress.strip: maybe", ingress.CodeStripInvalid, 8},
		"a bad path":         {"tierplan.ingress.host: a.example.com\n        tierplan.ingress.port: \"80\"\n        tierplan.ingress.path: api", ingress.CodePathInvalid, 8},
		"a missing hostname": {"tierplan.ingress.port: \"80\"", ingress.CodeHostMissing, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			compose := "services:\n  web:\n    image: nginx:1.27\n    deploy:\n      labels:\n        " + tc.labels + "\n"
			p := oneIngress(t, ingressCheck(t, compose, stacks.TargetSwarm, nil))
			if p.Code != tc.code || p.Severity != stacks.SeverityError {
				t.Fatalf("problem = %+v, want an error %s", p, tc.code)
			}
			if p.Line < 5 || p.Col < 1 {
				t.Errorf("position = %d:%d, want a real position inside the labels block", p.Line, p.Col)
			}
			if tc.line != 0 && p.Line != tc.line {
				t.Errorf("line = %d, want %d", p.Line, tc.line)
			}
		})
	}
}

func TestCheckIngressRouteDuplicateIsAnError(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: a.example.com
        tierplan.ingress.port: "80"
        tierplan.ingress.other.host: a.example.com
        tierplan.ingress.other.port: "81"
`
	p := oneIngress(t, ingressCheck(t, compose, stacks.TargetSwarm, nil))
	if p.Code != ingress.CodeRouteDuplicate || p.Severity != stacks.SeverityError {
		t.Errorf("problem = %+v, want an error ingress_route_duplicate", p)
	}
}

func TestCheckIngressWrongPlaceSwarm(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: app.example.com
      tierplan.ingress.port: "80"
`
	res := ingressCheck(t, compose, stacks.TargetSwarm, nil)
	p := oneIngress(t, res)
	if p.Code != ingress.CodeLabelsWrongPlace || p.Severity != stacks.SeverityError {
		t.Fatalf("problem = %+v, want an error ingress_labels_wrong_place", p)
	}
	if p.Key != "services.web.labels" || p.Line != 4 || p.Col != 5 {
		t.Errorf("position = %d:%d key %q, want services.web.labels at 4:5", p.Line, p.Col, p.Key)
	}
	if res.Problems.Errors() == 0 {
		t.Error("a wrong-place ingress label must block")
	}
}

func TestCheckIngressWrongPlaceStandaloneKeepsTheIgnoredOnTargetWarning(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
`
	res := ingressCheck(t, compose, stacks.TargetStandalone, nil)
	p := oneIngress(t, res)
	if p.Code != ingress.CodeLabelsWrongPlace || p.Severity != stacks.SeverityError {
		t.Fatalf("problem = %+v, want an error ingress_labels_wrong_place", p)
	}
	if p.Key != "services.web.deploy.labels" || p.Line != 5 || p.Col != 7 {
		t.Errorf("position = %d:%d key %q, want services.web.deploy.labels at 5:7", p.Line, p.Col, p.Key)
	}
	var lint bool
	for _, q := range res.Problems {
		lint = lint || q.Code == stacks.CodeIgnoredOnTarget
	}
	if !lint {
		t.Errorf("the existing ignored_on_target warning is gone: %+v", res.Problems)
	}
}

func TestCheckIngressLabelsInBothPlacesWarn(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: old.example.com
      tierplan.ingress.port: "80"
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
`
	res := ingressCheck(t, compose, stacks.TargetSwarm, nil)
	p := oneIngress(t, res)
	if p.Code != ingress.CodeLabelsIgnored || p.Severity != stacks.SeverityWarning {
		t.Fatalf("problem = %+v, want a warning ingress_labels_ignored", p)
	}
	if p.Key != "services.web.labels" || p.Line != 4 {
		t.Errorf("position = %d key %q, want the ignored labels block at line 4", p.Line, p.Key)
	}
	if res.Problems.Errors() > 0 {
		t.Errorf("a warning must not block: %+v", res.Problems)
	}
}

func TestCheckIngressValidRoutesAreSilent(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
        tierplan.ingress.api.host: api.example.com
        tierplan.ingress.api.port: "9000"
        tierplan.ingress.api.path: /v1
        tierplan.ingress.api.strip: "true"
`
	if ps := ingressProblems(ingressCheck(t, compose, stacks.TargetSwarm, nil)); len(ps) != 0 {
		t.Errorf("valid routes reported %+v", ps)
	}
}

func TestCheckIngressNoLabelsChangesNothing(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    labels:\n      tier: front\n"
	world := &stacks.IngressPolicy{StackID: "s1"}
	with := ingressCheck(t, compose, stacks.TargetStandalone, world).Problems
	without := ingressCheck(t, compose, stacks.TargetStandalone, nil).Problems
	if len(with) != len(without) {
		t.Errorf("a stack with no ingress labels changed: with %+v, without %+v", with, without)
	}
	if ps := ingressProblems(ingressCheck(t, compose, stacks.TargetStandalone, world)); len(ps) != 0 {
		t.Errorf("ingress problems for a stack with none: %+v", ps)
	}
}
