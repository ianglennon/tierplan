// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

const (
	// s3CheckMoved bounds the wait for another node to hold the address once the holder's check fails: the stand-in task's
	// shutdown, two failed runs of the check (fall 2, interval 1), FAULT and the election (about 2.5 s measured in research).
	s3CheckMoved = 10 * time.Second
	// s3CheckWatch is how long a holder whose check cannot run is watched for losing the address.
	s3CheckWatch = 15 * time.Second
)

// setReady labels the node ready or takes the label away, which places or removes the node's readiness stand-in task, so the
// node's check passes or fails. Taking it away is how a test fails one node's proxy.
func (r *s3Rig) setReady(n *dockertest.Node, ready bool) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	arg := "--label-rm"
	label := s3ReadyLabel
	if ready {
		arg, label = "--label-add", s3ReadyLabel+"=yes"
	}
	if out, err := r.sw.Managers[0].Exec(ctx, "docker", "node", "update", arg, label, n.Hostname); err != nil {
		r.t.Fatalf("%s %s on %s: %v\n%s", arg, label, n.Hostname, err, out)
	}
}

// TestS3FailingCheckNeedsSetuidSetgid is A-01 and Pitfall 1, the tracer of leg 1b: the holder's proxy goes away (its readiness
// stand-in is removed), and the address must move to another node. With NET_ADMIN, NET_RAW, SETUID and SETGID the check runs, the
// holder goes FAULT and another node holds the address. With NET_ADMIN and NET_RAW only, the script runner's setgroups fails
// (`Couldn't setgroups`), the check never runs, and the holder keeps the address; if a newer engine or kernel lets setgroups
// pass, that is logged as `caps=2 moved` and the result decides the set.
func TestS3FailingCheckNeedsSetuidSetgid(t *testing.T) {
	base := newS3Swarm(t)
	cases := []struct {
		name string
		caps []string
	}{
		{"caps-4", []string{"NET_ADMIN", "NET_RAW", "SETUID", "SETGID"}},
		{"caps-2", []string{"NET_ADMIN", "NET_RAW"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base.with(t)
			r.start(s3Opts{caps: c.caps})
			dockertest.WaitRunningTasks(t, r.sw.Managers[0], r.svcID, len(r.sw.Nodes()), s3Settle)
			holder, secs := r.waitOneHolder(s3FirstHolder, nil)
			t.Logf("engine %s: %s holds %s after %.1f s", r.sw.Engine, holder.Hostname, r.vip.Addr(), secs)
			ctr := r.taskContainer(holder)
			if ctr == "" {
				t.Fatalf("%s holds the address but has no running keepalived container", holder.Hostname)
			}

			// The label comes back at cleanup even when the test fails half way, so the next case starts with every proxy ready.
			t.Cleanup(func() { r.setReady(holder, true) })
			start := time.Now() // wall-clock: a measurement printed for people; the bound is the wait below
			r.setReady(holder, false)

			if len(c.caps) == 4 {
				next, _ := r.waitOneHolder(s3CheckMoved, holder)
				t.Logf("S3 failing-check: engine=%s caps=4 seconds=%.1f from=%s to=%s", r.sw.Engine, time.Since(start).Seconds(), holder.Hostname, next.Hostname)
			} else {
				moved := s3Until(s3CheckWatch, func() bool { return !r.has(holder, "") })
				switch {
				case moved:
					t.Logf("S3 failing-check: engine=%s caps=2 moved", r.sw.Engine)
				default:
					if out := r.logs(holder, ctr); !strings.Contains(out, "Couldn't setgroups") {
						t.Fatalf("%s kept the address with NET_ADMIN and NET_RAW only, but its log has no `Couldn't setgroups`:\n%s", holder.Hostname, out)
					}
					t.Logf("S3 failing-check: engine=%s caps=2 held", r.sw.Engine)
				}
			}

			r.setReady(holder, true)
			dockertest.WaitRunningTasks(t, r.sw.Managers[0], r.readyID, len(r.sw.Nodes()), s3Settle)
			r.remove()
		})
	}
}
