// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration || swarm

package agents_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
)

// execCtx is the context a command run in a dind node gets.
func execCtx(t testing.TB, d time.Duration) context.Context {
	t.Helper()
	// wall-clock: this is a deadline on a docker command run in a dind node, not a wait; every caller passes a minute or more and the helper only ends a command that never returns
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// networkNamed reports whether a node lists a network of exactly this name. The names are compared here:
// Engine 24 ignores ^ and $ in the name filter of `docker network ls` for a swarm-scoped network, so an
// anchored filter lists no overlay network there (a local one is matched as asked).
func networkNamed(ctx context.Context, n *dockertest.Node, name string) (bool, error) {
	out, err := n.Sh(ctx, "docker network ls --filter name="+name+" --format '{{.Name}}'")
	if err != nil {
		return false, err
	}
	return slices.Contains(strings.Fields(out), name), nil
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
				// Every agent container, a standalone one or a swarm task's, by its label.
				"for c in $(docker ps -aq --filter label=tierplan.agent=true); do docker logs --tail 40 $c 2>&1; done",
				"if [ \"$(docker info -f '{{.Swarm.ControlAvailable}}')\" = true ]; then docker service ls; docker service ps --no-trunc $(docker service ls -q) 2>&1 | head -30; fi",
			} {
				out, _ := n.Sh(ctx, line)
				t.Logf("%s: %s\n%s", n.Hostname, line, strings.TrimSpace(out))
			}
			cancel()
		}
	})
}

// fieldOf returns the value of a /proc/<pid>/status line, with the tabs of the
// kernel's layout turned into single spaces.
func fieldOf(status, key string) string {
	for _, line := range strings.Split(status, "\n") {
		if rest, ok := strings.CutPrefix(line, key); ok {
			return strings.Join(strings.Fields(rest), " ")
		}
	}
	return ""
}

// summarise is an environment as one line of JSON, for a failure message.
func summarise(e servertest.Environment) string {
	b, _ := json.Marshal(e)
	return string(b)
}
