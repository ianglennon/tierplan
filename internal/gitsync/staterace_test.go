// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitsync_test

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/gitsync"
	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// A check that began before a deploy ended does not put the old deployed revision back: the state is read after the
// repository has answered, under the stack's state lock, so the deploy's write is what the check builds on (review
// WR-B03).
func TestCheckKeepsADeployThatEndedWhileItFetched(t *testing.T) {
	t.Parallel()
	gate := newGateClient()
	f := newCheckFixture(t, func(c gitx.Client) gitx.Client { gate.Client = c; return gate }, nil)
	first := f.deploy(t)
	head := f.changeWeb(t, "1")
	ctx := context.Background()

	type result struct {
		out gitsync.Outcome
		err error
	}
	gate.on.Store(true)
	done := make(chan result, 1)
	go func() {
		out, err := f.svc.Check(ctx, "env-1", f.stackID, gitsync.TriggerManual)
		done <- result{out, err}
	}()
	testsupport.Await(t, "the check to reach the fetch", gate.entered)
	gate.on.Store(false)

	// A person pulls the head and deploys it while the check waits on the repository.
	if _, err := f.pull(t, f.draft(t).Version); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	rev := f.deploy(t)
	if rev.Seq <= first.Seq {
		t.Fatalf("second revision = #%d, first #%d", rev.Seq, first.Seq)
	}
	f.svc.Deployed(ctx, f.stackID, rev)
	gate.release <- struct{}{}

	r := testsupport.Await(t, "the check to end", done)
	if r.err != nil || r.out != gitsync.OutcomeInSync {
		t.Fatalf("check = %s, %v, want in_sync: the head is what was deployed meanwhile", r.out, r.err)
	}
	s := f.state(t)
	if s.Status != stackops.GitStatusInSync || s.DeployedCommit != head || s.HeadCommit != head || s.Behind != 0 {
		t.Errorf("state = %+v, want in sync with %s deployed", s, head)
	}
}
