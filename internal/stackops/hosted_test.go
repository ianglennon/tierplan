// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stackops/stackopstest"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// hostStack stands a compose file's stack on a fake standalone host as a deploy of it leaves it (plan
// 07-22): see stackopstest.Hosted. The stack's ID is the one standaloneStackOf gives.
func hostStack(t *testing.T, name, compose string, vars []stacks.Var, files ...stacks.File) *stackopstest.Hosted {
	t.Helper()
	return stackopstest.Host(t, name, "stack-"+name, compose, vars, files...)
}

// hostRig stands the fake engine up with the stack on it.
func hostRig(t *testing.T, h *stackopstest.Hosted) *swarmRig {
	t.Helper()
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		standalone(c)
		c.TasksFollowWrites = false
		h.Apply(c)
	})
}

// reviewOf reviews a compose file (the draft) against the host, with the variables and files given.
func (r *swarmRig) reviewOf(t *testing.T, name, compose string, vars []stacks.Var, revisions stackops.RevisionStore, files ...stacks.File) stackops.Review {
	t.Helper()
	in := r.standaloneReviewInput(t, name, compose, nil)
	in.Copy.Files = append(in.Copy.Files, files...)
	in.Copy.Vars = vars
	in.Revisions = revisions
	rv, err := r.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	return rv
}
