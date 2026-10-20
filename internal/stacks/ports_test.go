// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// The port budget (plan 07-36, STK-01, STK-04): compose-go expands every short-form range of
// ports: into one entry per port while it loads, so a file of a few hundred bytes can cost
// gigabytes. The ranges are counted before compose-go sees them.

// portRangeReproduction is the verifier's reproduction: 40 distinct 1,000-port ranges under one
// service's ports (compose-go would expand them to 40,000 ports). They are distinct because
// compose-go folds identical entries, which would make the file cheap. The ports: key is on line
// 4, so entry i is on line 5+i and the 21st entry (i = 20), where the count passes 20,000, is on
// line 25.
func portRangeReproduction() string {
	var b strings.Builder
	b.WriteString("services:\n  web:\n    image: nginx:1.27-alpine\n    ports:\n")
	for i := range 40 {
		fmt.Fprintf(&b, "      - %d-%d:20000-20999\n", 10000+1000*i, 10999+1000*i)
	}
	return b.String()
}

// portBudgetFile is a file with one range of n ports, then any extra entries.
func portBudgetFile(n int, extra ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "services:\n  web:\n    image: nginx:1.27-alpine\n    ports:\n      - \"1-%d:1-%d\"\n", n, n)
	for _, e := range extra {
		fmt.Fprintf(&b, "      - %q\n", e)
	}
	return b.String()
}

func TestThePortRangeReproductionIsRefusedAtItsLine(t *testing.T) {
	t.Parallel()
	content := portRangeReproduction()
	if got := len(content); got > 1400 {
		t.Fatalf("the reproduction is %d bytes, want a file of about 1,325 bytes", got)
	}
	res := checkOf(t, content)
	if res.Loaded != nil {
		t.Fatal("the reproduction came back loaded: compose-go must never expand it")
	}
	p := onlyProblem(t, res)
	if p.Code != stacks.CodePortsTooMany || p.Severity != stacks.SeverityError || p.Line != 25 || p.Key != "services.web.ports" {
		t.Errorf("problem = %+v, want an error ports_too_many at line 25, key services.web.ports", p)
	}

	_, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "probe", Target: stacks.TargetSwarm, Copy: copyOf(content)})
	var pe *stacks.PortsError
	if !errors.As(err, &pe) || !errors.Is(err, stacks.ErrPortsTooMany) {
		t.Fatalf("Load error = %v, want a *PortsError wrapping ErrPortsTooMany", err)
	}
	if pe.Line != 25 {
		t.Errorf("PortsError line = %d, want 25", pe.Line)
	}
}

func TestOrdinaryPortRangesStillLoad(t *testing.T) {
	t.Parallel()
	for name, ports := range map[string]string{
		"a media range":      `["10000-20000:10000-20000/udp"]`,
		"a turn relay range": `["49152-65535:49152-65535/udp", "3478:3478/udp", "3478:3478", "5349:5349"]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := checkOf(t, "services:\n  web:\n    image: nginx:1.27-alpine\n    ports: "+ports+"\n")
			if res.Problems.Errors() != 0 || res.Loaded == nil {
				t.Errorf("problems = %+v loaded = %v, want a clean load", res.Problems, res.Loaded != nil)
			}
		})
	}
}

func TestThePortBudgetAcceptsExactlyMaxPorts(t *testing.T) {
	t.Parallel()
	res := checkOf(t, portBudgetFile(stacks.MaxPorts))
	if res.Problems.Errors() != 0 || res.Loaded == nil {
		t.Fatalf("problems = %+v loaded = %v, want exactly MaxPorts ports to check clean", res.Problems, res.Loaded != nil)
	}
	res = checkOf(t, portBudgetFile(stacks.MaxPorts, "30000:30000"))
	if res.Loaded != nil {
		t.Fatal("one port over the budget came back loaded")
	}
	p := onlyProblem(t, res)
	if p.Code != stacks.CodePortsTooMany || p.Line != 6 {
		t.Errorf("problem = %+v, want ports_too_many at line 6 (the entry that takes the count over)", p)
	}
}

// portsCheck checks a service with the given YAML under it and returns the problem list.
func portsCheck(t *testing.T, body string, vars ...stacks.Var) stacks.CheckResult {
	t.Helper()
	return stacks.Check(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetSwarm,
		Copy: copyOf("services:\n  web:\n    image: nginx:1.27-alpine\n"+body, vars...),
	})
}

func TestPortRangesOutsideThePortSpaceAreRefusedAtTheirKey(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body string
		key  string
		line int
	}{
		"expose over 65535":       {"    expose: [\"1-9000000000000\"]\n", "services.web.expose", 4},
		"a host port over 65535":  {"    ports: [\"70000:80\"]\n", "services.web.ports", 4},
		"an inverted range":       {"    ports: [\"9000-8000:9000-8000\"]\n", "services.web.ports", 4},
		"a long-form published":   {"    ports:\n      - target: 80\n        published: \"8000-90000\"\n", "services.web.ports", 5},
		"a long-form target":      {"    ports:\n      - target: 70000\n        published: 80\n", "services.web.ports", 5},
		"a number too long to be": {"    expose: [\"123456789012345678901234567890\"]\n", "services.web.expose", 4},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := portsCheck(t, tc.body)
			if res.Loaded != nil {
				t.Fatal("an impossible range came back loaded")
			}
			p := onlyProblem(t, res)
			if p.Code != stacks.CodePortRangeInvalid || p.Severity != stacks.SeverityError || p.Key != tc.key || p.Line != tc.line {
				t.Errorf("problem = %+v, want an error port_range_invalid at %s line %d", p, tc.key, tc.line)
			}
			for _, quoted := range []string{"9000000000000", "70000", "90000", "9000-8000", "123456789012345678901234567890"} {
				if strings.Contains(p.Message, quoted) || strings.Contains(p.Key, quoted) {
					t.Errorf("the problem quotes the range text %q: %+v", quoted, p)
				}
			}
		})
	}
}

func TestInterpolatedPortRangesCountAgainstTheBudget(t *testing.T) {
	t.Parallel()
	const body = "    ports: [\"${RANGE}\"]\n"
	res := portsCheck(t, body, stacks.Var{Name: "RANGE", Value: "1-30000:1-30000", Secret: true})
	if res.Loaded != nil {
		t.Fatal("a range from a variable came back loaded")
	}
	p := onlyProblem(t, res)
	if p.Code != stacks.CodePortsTooMany || p.Line != 4 {
		t.Errorf("problem = %+v, want ports_too_many at line 4", p)
	}
	if strings.Contains(p.Message, "30000") {
		t.Errorf("the problem quotes the value of a secret variable: %q", p.Message)
	}
	res = portsCheck(t, body, stacks.Var{Name: "RANGE", Value: "8080:80"})
	if res.Problems.Errors() != 0 || res.Loaded == nil {
		t.Errorf("problems = %+v, want a short range from a variable to load", res.Problems)
	}
	// A default written in the file counts too.
	res = portsCheck(t, "    ports: [\"${RANGE:-1-30000:1-30000}\"]\n")
	if got := onlyProblem(t, res); got.Code != stacks.CodePortsTooMany {
		t.Errorf("problem = %+v, want ports_too_many for a default", got)
	}
}

func TestPortsThroughAliasesAndMergeKeysAreCounted(t *testing.T) {
	t.Parallel()
	for name, compose := range map[string]string{
		"an anchored list used twice": "x-ports: &p [\"1-15000:1-15000\"]\nservices:\n  a:\n    image: nginx:1.27-alpine\n    ports: *p\n  b:\n    image: nginx:1.27-alpine\n    ports: *p\n",
		"a merge key base":            "x-base: &base\n  image: nginx:1.27-alpine\n  ports: [\"1-15000:1-15000\"]\nservices:\n  a:\n    <<: *base\n  b:\n    <<: *base\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := checkOf(t, compose)
			if res.Loaded != nil {
				t.Fatal("the file came back loaded")
			}
			if p := onlyProblem(t, res); p.Code != stacks.CodePortsTooMany {
				t.Errorf("problem = %+v, want ports_too_many", p)
			}
		})
	}
}

func TestExposeRangesCountAgainstTheBudget(t *testing.T) {
	t.Parallel()
	res := portsCheck(t, "    expose: [\"1-30000\"]\n")
	if p := onlyProblem(t, res); p.Code != stacks.CodePortsTooMany || p.Key != "services.web.expose" {
		t.Errorf("problem = %+v, want ports_too_many at services.web.expose", p)
	}
	// ports and expose share one budget.
	res = portsCheck(t, "    ports: [\"1-15000:1-15000\"]\n    expose: [\"20000-30000\"]\n")
	if p := onlyProblem(t, res); p.Code != stacks.CodePortsTooMany || p.Key != "services.web.expose" {
		t.Errorf("problem = %+v, want the expose range to take the shared total over", p)
	}
}

func TestANonYAMLFileWithHugeRangesIsRefusedBeforeTheLoad(t *testing.T) {
	t.Parallel()
	// A tab for indentation: yaml.v3 cannot decode it, so the walker cannot see the ranges.
	const broken = "services:\n\tweb:\n\t\tports: [\"1-10000:1-10000\", \"1-10000:1-10000\", \"1-10000:1-10000\"]\n"
	_, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "probe", Target: stacks.TargetSwarm, Copy: copyOf(broken)})
	var pe *stacks.PortsError
	if !errors.As(err, &pe) || !errors.Is(err, stacks.ErrPortsTooMany) {
		t.Fatalf("Load error = %v, want a *PortsError wrapping ErrPortsTooMany", err)
	}
	res := checkOf(t, broken)
	if res.Loaded != nil || onlyProblem(t, res).Code != stacks.CodePortsTooMany {
		t.Errorf("problems = %+v, want the one ports_too_many problem", res.Problems)
	}

	// The same broken file without ranges gives the usual yaml_invalid problem.
	res = checkOf(t, "services:\n\tweb:\n\t\timage: nginx\n")
	if res.Loaded != nil || onlyProblem(t, res).Code != stacks.CodeYAMLInvalid {
		t.Errorf("problems = %+v, want the one yaml_invalid problem", res.Problems)
	}
}
