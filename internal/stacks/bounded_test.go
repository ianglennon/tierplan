// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"context"
	"runtime"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// allocatedBy returns the bytes the process allocated while fn ran. It reads the process-wide
// counter, so it is only meaningful when nothing else runs at the same time.
func allocatedBy(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// checkCopy runs under testsupport.Deadline, so a slow machine cannot fail these tests on the
// product's 5 s check deadline; they are about memory, not the clock.
func checkCopy(compose string) stacks.CheckInput {
	return stacks.CheckInput{Name: "probe", Target: stacks.TargetSwarm, Copy: copyOf(compose), Deadline: testsupport.Deadline}
}

// TestThePortRangeReproductionIsRefusedWithinAFixedMemoryBound reads the allocation counter around
// one check of the 40-line reproduction. Before plan 07-36 the same call allocated about 400 MB
// (40,000 ports at about 10 KB each; the verifier saw 376 MB), so a 16 MiB bound tells the two
// apart by a factor of about 25.
//
// It is not parallel, on purpose: it reads the process-wide allocation counter, and Go runs a
// top-level test that is not parallel while every parallel test of the package is paused, so
// nothing else allocates in between (with or without -shuffle).
func TestThePortRangeReproductionIsRefusedWithinAFixedMemoryBound(t *testing.T) {
	content := portRangeReproduction()
	stacks.Check(context.Background(), checkCopy(content)) // warm-up: one-time initialisation is not the check's cost
	var res stacks.CheckResult
	used := allocatedBy(func() { res = stacks.Check(context.Background(), checkCopy(content)) })
	t.Logf("one check of the reproduction allocated %.2f MiB", float64(used)/(1<<20))
	if p := onlyProblem(t, res); p.Code != stacks.CodePortsTooMany {
		t.Fatalf("problem = %+v, want ports_too_many", p)
	}
	const ceiling = 16 << 20 // MemStats.TotalAlloc, in bytes
	if used >= ceiling {
		t.Errorf("the check allocated %d bytes, want under 16 MiB", used)
	}
}

// TestTheLargestFileTheBudgetAcceptsStaysWithinItsMeasuredCost checks the cost plan 07-36 states
// for an accepted file: exactly MaxPorts ports in one range. Measured while planning: about 206 MB
// allocated (with and without the race detector), about 43 MB of peak heap, 0.27 s, and 1.06 s
// under the race detector. Peak heap is sampled, not deterministic, so it is stated here and not
// asserted. A compose-go upgrade that trips the ceiling means re-measuring and either raising the
// ceiling with the new figure or lowering MaxPorts, never silently widening it.
//
// It is not parallel, for the reason given on the test above.
func TestTheLargestFileTheBudgetAcceptsStaysWithinItsMeasuredCost(t *testing.T) {
	content := portBudgetFile(stacks.MaxPorts)
	var res stacks.CheckResult
	used := allocatedBy(func() { res = stacks.Check(context.Background(), checkCopy(content)) })
	t.Logf("one check of exactly MaxPorts (%d) ports allocated %.2f MiB", stacks.MaxPorts, float64(used)/(1<<20))
	if res.Problems.Errors() != 0 || res.Loaded == nil {
		t.Fatalf("problems = %+v loaded = %v, want a clean load", res.Problems, res.Loaded != nil)
	}
	const ceiling = 256 << 20 // MemStats.TotalAlloc, in bytes
	if used >= ceiling {
		t.Errorf("the check allocated %d bytes, want under 256 MiB", used)
	}
}
