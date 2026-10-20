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
