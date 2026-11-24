// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// TestS3KilledHolderMoves is spike S3 leg 1a, the tracer: a global keepalived service on the host network, multicast VRRP
// with auth_hmac and a key file holding `hex:` text, gives exactly one node the address; SIGKILL of that node's container
// (no goodbye advert) makes another node hold it. The seconds are logged, measured from the kill to the first poll that
// sees another holder, so they are a little above the real gap.
func TestS3KilledHolderMoves(t *testing.T) {
	r := newS3Swarm(t)
	r.start(s3Opts{})
	dockertest.WaitRunningTasks(t, r.sw.Managers[0], r.svcID, len(r.sw.Nodes()), s3Settle)

	holder, secs := r.waitOneHolder(s3FirstHolder, nil)
	t.Logf("engine %s: %s holds %s after %.1f s", r.sw.Engine, holder.Hostname, r.vip.Addr(), secs)

	ctr := r.taskContainer(holder)
	if ctr == "" {
		t.Fatalf("%s holds the address but has no running keepalived container", holder.Hostname)
	}
	r.kill(holder, ctr)
	next, secs := r.waitOneHolder(s3Moved, holder)
	if next == holder {
		t.Fatalf("the address did not leave %s", holder.Hostname)
	}
	t.Logf("S3 killed-holder: engine=%s seconds=%.1f from=%s to=%s", r.sw.Engine, secs, holder.Hostname, next.Hostname)
}
