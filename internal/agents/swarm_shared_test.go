// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"context"
	"flag"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/server/servertest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// sharedSwarmCases are the swarm tests that leave the swarm itself as they found it:
// they make their own services, secrets, configs, networks and volumes, and install
// and remove agents, but never stop, remove, demote or promote a node, leave the swarm
// or need a shape other than three managers and a worker. They take turns on one swarm
// (TestSwarmShared). Each still gets its own Bayplan and agent environment, so no
// audit row, agent row or detector state passes from one to the next. A test that
// changes the swarm itself stays a TestSwarm function with a swarm of its own.
var sharedSwarmCases = []struct {
	name string
	run  func(*testing.T, *swarmFix)
}{
	{"ConstraintsMatchPlacement", sharedConstraintsMatchPlacement},
	{"Enrol", sharedEnrol},
	{"ImagePullFanOut", sharedImagePullFanOut},
	{"NetworkConnect", sharedNetworkConnect},
	{"Networks", sharedNetworks},
	{"PrivatePullEveryNode", sharedPrivatePullEveryNode},
	{"RealEngineFacts", sharedRealEngineFacts},
	{"RealEvents", sharedRealEvents},
	{"RestartLoop", sharedRestartLoop},
	{"RevokeBlocksReenrol", sharedRevokeBlocksReenrol},
	{"RotateToken", sharedRotateToken},
	{"RoutesNodeLocalCalls", sharedRoutesNodeLocalCalls},
	{"SecretRotate", sharedSecretRotate},
	{"ServiceAutoRollback", sharedServiceAutoRollback},
	{"ServiceLifecycle", sharedServiceLifecycle},
	{"ServiceLogsMerged", sharedServiceLogsMerged},
	{"StackAdoptRoundTrip", sharedStackAdoptRoundTrip},
	{"StackDeployNetworkFull", sharedStackDeployNetworkFull},
	{"StackDeployNetworkFullOnEveryNode", sharedStackDeployNetworkFullOnEveryNode},
	{"StackDeployParity", sharedStackDeployParity},
	{"StackDeployTasksFailed", sharedStackDeployTasksFailed},
	{"StackDrift", sharedStackDrift},
	{"StackHistoryRollback", sharedStackHistoryRollback},
	{"StackLifecycle", sharedStackLifecycle},
	{"StackPin", sharedStackPin},
	{"StackRedeployIdempotent", sharedStackRedeployIdempotent},
	{"StackReviewEmptyAfterDeploy", sharedStackReviewEmptyAfterDeploy},
	{"StackUpdateImages", sharedStackUpdateImages},
	{"StatsFromWorker", sharedStatsFromWorker},
	{"UninstallOnRemove", sharedUninstallOnRemove},
	{"Volumes", sharedVolumes},
	{"WorkerShellAndLogs", sharedWorkerShellAndLogs},
}

// TestSwarmShared runs sharedSwarmCases as subtests on one swarm of three managers and
// a worker, in an order shuffled like -shuffle shuffles tests. After each case the swarm
// is reset to the state it started in and checked; a swarm that does not pass the
// check is removed and a new one started, so a case never runs on what another left.
// BAYPLAN_SWARM_SHARED_SEED replays an order: the seed is logged at the start.
func TestSwarmShared(t *testing.T) {
	var sw *dockertest.Swarm
	var base dockertest.SwarmBaseline
	fresh := func() {
		t.Helper()
		sw = dockertest.StartSwarm(t, "", 3, 1)
		dockertest.LoadAgentImage(t, sw.Nodes()...)
		bctx, cancel := context.WithTimeout(context.Background(), testsupport.Deadline)
		b, err := sw.Baseline(bctx)
		cancel()
		if err != nil {
			t.Fatalf("record the swarm's baseline: %v", err)
		}
		base = b
	}
	fresh()

	// resetOrReplace brings the swarm back to its baseline before the next case. The last
	// case's Bayplan has stopped (its cleanup ran when the subtest ended), so its agents
	// are disconnected; the reset removes them with everything else. A swarm that fails
	// the reset is removed and a new one started, so no case runs on what another left.
	resetOrReplace := func() {
		t.Helper()
		rctx, cancel := context.WithTimeout(context.Background(), testsupport.Deadline*10)
		err := sw.Reset(rctx, base)
		cancel()
		if err == nil {
			return
		}
		t.Logf("the swarm could not be reset, so the next case gets a new one: %v", err)
		tctx, cancel := context.WithTimeout(context.Background(), testsupport.Deadline*4)
		if terr := sw.Terminate(tctx); terr != nil {
			t.Logf("remove the old swarm: %v", terr)
		}
		cancel()
		fresh()
	}

	order := sharedOrder(t, len(sharedSwarmCases))
	dirty := false // a case has run on the swarm since its last reset
	for _, idx := range order {
		c := sharedSwarmCases[idx]
		if dirty {
			resetOrReplace()
			dirty = false
		}
		t.Run(c.name, func(t *testing.T) {
			dirty = true // a case -run filters out never gets here
			dumpOnFailure(t, sw.Nodes()...)
			srv := servertest.Start(t, servertest.Options{Host: sw.Gateway})
			c.run(t, &swarmFix{sw: sw, srv: srv, env: srv.CreateAgentEnvironment("")})
		})
	}
}

// sharedOrder is the order to run n cases in. It follows go test's -shuffle: off keeps
// the listed order, a number is the seed, and on picks one. BAYPLAN_SWARM_SHARED_SEED
// overrides all three, to replay an order from a log.
func sharedOrder(t *testing.T, n int) []int {
	t.Helper()
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	setting := "off"
	if f := flag.Lookup("test.shuffle"); f != nil {
		setting = f.Value.String()
	}
	if env := os.Getenv("BAYPLAN_SWARM_SHARED_SEED"); env != "" {
		setting = env
	}
	if setting == "off" {
		t.Log("shared swarm cases in listed order (-shuffle off)")
		return order
	}
	seed, err := strconv.ParseInt(setting, 10, 64)
	if err != nil {
		seed = time.Now().UnixNano()
	}
	rand.New(rand.NewPCG(uint64(seed), 0)).Shuffle(n, func(i, j int) { order[i], order[j] = order[j], order[i] }) //nolint:gosec // a test order that must replay from its seed, not a secret
	t.Logf("shared swarm cases shuffled with seed %d (replay with BAYPLAN_SWARM_SHARED_SEED=%d)", seed, seed)
	return order
}
