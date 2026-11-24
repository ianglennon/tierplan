// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// s3Cleared bounds the wait for a replacement task's start to clear what a kill left, and for it to enter BACKUP: swarm's
// restart delay (5 s), the pull-free start and the check's two passes, with a wide margin.
const s3Cleared = 90 * time.Second

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

// s3Replaced waits until the node runs a keepalived container other than old: swarm's replacement of a killed task.
func s3Replaced(t *testing.T, r *s3Rig, n *dockertest.Node, old string) string {
	t.Helper()
	var repl string
	dockertest.Wait(t, s3Settle, "a replacement keepalived task on "+n.Hostname, func() (bool, string) {
		repl = r.taskContainer(n)
		return repl != "" && repl != old, "container " + repl
	})
	return repl
}

// TestS3NoPreempt is D-04: the node whose task swarm replaced after the kill comes back as a BACKUP and does not take the
// address back from the node that took it over, which stays healthy. The address is gone from the old node first (the
// replacement's start clears what the kill left, see TestS3StaleAddressCleared); then the old node is watched for 20 s.
func TestS3NoPreempt(t *testing.T) {
	r := newS3Swarm(t)
	r.start(s3Opts{})
	dockertest.WaitRunningTasks(t, r.sw.Managers[0], r.svcID, len(r.sw.Nodes()), s3Settle)

	old, _ := r.waitOneHolder(s3FirstHolder, nil)
	ctr := r.taskContainer(old)
	r.kill(old, ctr)
	next, secs := r.waitOneHolder(s3Moved, old)
	t.Logf("engine %s: %s took the address from the killed %s after %.1f s", r.sw.Engine, next.Hostname, old.Hostname, secs)

	repl := s3Replaced(t, r, old, ctr)
	dockertest.Wait(t, s3Cleared, old.Hostname+" to lose the address its killed task left", func() (bool, string) {
		return !r.has(old, ""), "still listed"
	})
	dockertest.Wait(t, s3Cleared, "the replacement on "+old.Hostname+" to enter BACKUP", func() (bool, string) {
		out := r.logs(old, repl)
		return strings.Contains(out, "Entering BACKUP STATE"), out
	})

	violation := ""
	s3Until(20*time.Second, func() bool {
		switch {
		case r.has(old, ""):
			violation = old.Hostname + " took the address back"
		case !r.has(next, ""):
			violation = next.Hostname + " lost the address"
		}
		return violation != ""
	})
	if violation != "" {
		t.Fatalf("S3 nopreempt: engine=%s violated: %s", r.sw.Engine, violation)
	}
	if got := r.holders(); len(got) != 1 || got[0] != next {
		t.Fatalf("holders at the end: %s, want only %s", s3Names(got), next.Hostname)
	}
	if out := r.logs(old, repl); strings.Contains(out, "Entering MASTER STATE") {
		t.Fatalf("the replacement on %s became MASTER:\n%s", old.Hostname, out)
	}
	t.Logf("S3 nopreempt: engine=%s held", r.sw.Engine)
}

// TestS3StaleAddressCleared is D-10 and A-02: a killed holder leaves the address on the node's interface (nothing took it
// off), and keepalived removes it itself when the replacement starts, so no helper code deletes addresses. The first read
// is taken at once after the kill, before swarm's replacement can start.
func TestS3StaleAddressCleared(t *testing.T) {
	r := newS3Swarm(t)
	r.start(s3Opts{})
	dockertest.WaitRunningTasks(t, r.sw.Managers[0], r.svcID, len(r.sw.Nodes()), s3Settle)

	old, _ := r.waitOneHolder(s3FirstHolder, nil)
	ctr := r.taskContainer(old)
	r.kill(old, ctr)
	state := "absent"
	if r.has(old, "") {
		state = "present"
	}
	t.Logf("S3 stale-after-kill: engine=%s %s", r.sw.Engine, state)

	s3Replaced(t, r, old, ctr)
	dockertest.Wait(t, s3Cleared, old.Hostname+" to lose the address and exactly one other node to hold it", func() (bool, string) {
		got := r.holders()
		return len(got) == 1 && got[0] != old, "holders " + s3Names(got)
	})
	t.Logf("S3 stale-cleared: engine=%s by=keepalived", r.sw.Engine)
}

// TestS3StaleAfterInterfaceChange is the case Pitfall 3 left open: a holder killed on eth0, then the configuration moved to
// eth1 with the same address. The restart on the new interface cannot remove what the old one left, unless keepalived
// looks at every interface. It records whether the address remains on eth0; it passes either way (26-17 reads the line).
// The killed node's replacement is held back by a restart delay, so that only the updated task, on eth1, ever runs there.
// The delay is 20 s, not longer: whether the update lands before swarm has made the replacement (a new task at once) or
// after (the held-back task takes the new configuration and starts when its delay ends) is a race, and a delay of minutes
// left the second case without a running task on that node for the whole wait.
func TestS3StaleAfterInterfaceChange(t *testing.T) {
	r := newS3Swarm(t)
	r.secondNetwork()
	r.start(s3Opts{restartDelay: 20 * time.Second})
	dockertest.WaitRunningTasks(t, r.sw.Managers[0], r.svcID, len(r.sw.Nodes()), s3Settle)

	old, _ := r.waitOneHolder(s3FirstHolder, nil)
	if !r.has(old, "eth0") {
		t.Fatalf("%s is the holder but does not list the address on eth0", old.Hostname)
	}
	r.kill(old, r.taskContainer(old))
	if !r.has(old, "eth0") {
		t.Logf("the kill left no address on %s: the case below is not about a stale address", old.Hostname)
	}

	r.updateConfig(s3Opts{iface: "eth1"})
	dockertest.WaitRunningTasks(t, r.sw.Managers[0], r.svcID, len(r.sw.Nodes()), s3Settle)
	dockertest.Wait(t, s3FirstHolder, "exactly one node to hold the address on eth1", func() (bool, string) {
		got := r.holdersOn("eth1")
		return len(got) == 1, "holders on eth1 " + s3Names(got)
	})

	outcome := "remains"
	if !r.has(old, "eth0") {
		outcome = "removed"
	}
	t.Logf("S3 stale-interface-change: engine=%s %s", r.sw.Engine, outcome)
}

// TestS3SecretUnderTmpfsRun is research assumption A1: whether a swarm secret at its default target, /run/secrets/<name>,
// mounts and is read by keepalived when /run is a tmpfs and the root is read-only, and the same with the secret's file name
// an absolute path outside /run. Each variant records whether every task runs, one node holds the address and the log has
// an auth_hmac key error. The test fails only if neither variant works.
func TestS3SecretUnderTmpfsRun(t *testing.T) {
	base := newS3Swarm(t)
	variants := []struct{ name, secretFile, keyPath string }{
		{"default-target", "s3-key", "/run/secrets/s3-key"},
		{"outside-run", "/etc/tierplan-keepalived/key", "/etc/tierplan-keepalived/key"},
	}
	works := map[string]bool{}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			r := base.with(t)
			r.start(s3Opts{secretFile: v.secretFile, keyPath: v.keyPath})
			want := len(r.sw.Nodes())
			var tasks int
			var holders []*dockertest.Node
			ok := s3Until(2*time.Minute, func() bool {
				tasks = r.runningTasks()
				if tasks != want {
					return false
				}
				holders = r.holders()
				return len(holders) == 1
			})
			keyErr := s3KeyError.FindString(r.serviceLogs())
			t.Logf("variant %s: ok=%v running tasks=%d/%d holders=%s key error=%q", v.name, ok, tasks, want, s3Names(holders), keyErr)
			works[v.name] = ok && keyErr == ""
			r.remove()
		})
	}
	word := func(name string) string {
		if works[name] {
			return "ok"
		}
		return "fails"
	}
	t.Logf("S3 secret-path: engine=%s default-target=%s outside-run=%s", base.sw.Engine, word("default-target"), word("outside-run"))
	if !works["default-target"] && !works["outside-run"] {
		t.Fatalf("a secret works at neither path on engine %s", base.sw.Engine)
	}
}
