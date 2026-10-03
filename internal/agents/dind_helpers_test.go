// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration || swarm

package agents_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// execCtx is the context a command run in a dind node gets.
func execCtx(t testing.TB, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// sh runs a shell line in a node and fails the test when it exits non-zero. The
// line is run as the install commands are meant to be pasted: through sh -c.
func sh(t testing.TB, n *dockertest.Node, line string) string {
	t.Helper()
	out, err := n.Sh(execCtx(t, 3*time.Minute), line)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// dumpOnFailure prints the agent's own log and what Docker says about it, from
// each node, when the test fails: the first thing anyone debugging a run needs.
func dumpOnFailure(t *testing.T, nodes ...*dockertest.Node) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, n := range nodes {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			for _, line := range []string{
				"docker ps -a --format '{{.Names}} {{.Image}} {{.Status}}'",
				"docker logs --tail 40 bayplan-agent 2>&1",
				"docker service ls 2>&1; docker service ps --no-trunc bayplan-agent 2>&1",
			} {
				out, _ := n.Sh(ctx, line)
				t.Logf("%s: %s\n%s", n.Hostname, line, strings.TrimSpace(out))
			}
			cancel()
		}
	})
}
