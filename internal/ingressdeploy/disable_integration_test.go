// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestDisableWipesOnSwarm is SC 4: switching off a swarm that holds no claim removes the proxy service (once its tasks are
// gone), the cache volume on both nodes, the network and the join token secrets, and leaves the status with ingress off
// and no object. The job's timing on the real engine is logged: the task shutdown and the release of the volume are what
// the 24-27 design assumed.
func TestDisableWipesOnSwarm(t *testing.T) {
	e := startSwarmEnv(t)
	requireJobDone(t, e.enable())
	e.awaitReady()
	for _, n := range e.sw.Nodes() {
		if present, _ := e.hasCacheVolume(n); !present {
			t.Fatalf("%s has no cache volume before the switch-off", n.Hostname)
		}
	}

	start := time.Now()
	job := e.disable()
	t.Logf("the switch-off job took %s on engine %s", time.Since(start), e.sw.Engine)
	requireJobDone(t, job)
	for _, it := range job.Items {
		if it.ID == ingressdeploy.ItemVolumes {
			if it.Result["wiped"] != "2" || it.Result["pending"] != "0" {
				t.Errorf("the volumes item says %v, want both nodes wiped and none pending", it.Result)
			}
		}
	}

	if svc, net, secrets := e.objects(); svc != 0 || net != 0 || secrets != 0 {
		t.Errorf("the swarm still holds %d proxy service(s), %d network(s) and %d join token secret(s)", svc, net, secrets)
	}
	for _, n := range e.sw.Nodes() {
		if present, _ := e.hasCacheVolume(n); present {
			t.Errorf("%s still holds the cache volume after the switch-off", n.Hostname)
		}
	}
	st, err := e.deploy.Status(testsupport.Context(t), e.envID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Enabled || st.Objects.Network || st.Objects.Service || st.Objects.Secret || st.DisablingIncomplete || st.PendingWipes != 0 || st.Nodes.Enrolled != 0 || !st.TokenDisabled {
		t.Errorf("status after the switch-off = %+v, want ingress off, no object, no pending wipe, no node and the token disabled", st)
	}
	if n := e.auditCount(audit.ActionIngressDisabled); n != 1 {
		t.Errorf("%d ingress.disabled audit rows, want one", n)
	}
}

// TestPendingWipeOnSwarm is D-27 on a real swarm: with the worker's agent gone, the switch-off cannot reach its cache volume,
// so the job names the worker and the volume becomes a pending wipe that is still on the node; when the worker's agent
// connects again the volume is removed and the entry is cleared and audited.
func TestPendingWipeOnSwarm(t *testing.T) {
	e := startSwarmEnv(t)
	requireJobDone(t, e.enable())
	e.awaitReady()

	e.dropAgent(e.wrk)
	job := e.disable()
	requireJobDone(t, job)
	workerName := ""
	for _, it := range job.Items {
		if it.ID == ingressdeploy.ItemVolumes {
			workerName = it.Result["unreachable"]
			if it.Result["wiped"] != "1" || it.Result["pending"] != "1" || workerName == "" {
				t.Fatalf("the volumes item says %v, want the manager wiped and the worker pending", it.Result)
			}
		}
	}
	if !strings.Contains(workerName, "w1") {
		t.Errorf("the job names %q as unreachable, want the worker", workerName)
	}
	if present, _ := e.hasCacheVolume(e.mgr); present {
		t.Errorf("the manager still holds the cache volume")
	}
	if present, _ := e.hasCacheVolume(e.wrk); !present {
		t.Fatalf("the worker's cache volume is gone although its agent was not connected: nothing could have removed it")
	}
	st, err := e.deploy.Status(testsupport.Context(t), e.envID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.PendingWipes != 1 {
		t.Fatalf("status = %+v, want one pending wipe", st)
	}
	if n := e.auditCount(audit.ActionIngressWipeQueued); n != 1 {
		t.Errorf("%d ingress.wipe_queued audit rows, want one", n)
	}

	// The worker's agent comes back: the broker's agent.connected event wakes the wipe loop.
	e.connectAgent(e.wrk)
	dockertest.Wait(t, wipeBound, "the worker's cache volume to be removed and its pending wipe cleared", func() (bool, string) {
		present, _ := e.hasCacheVolume(e.wrk)
		st, err := e.deploy.Status(testsupport.Context(t), e.envID)
		if err != nil {
			return false, err.Error()
		}
		return !present && st.PendingWipes == 0, fmt.Sprintf("volume present %v, pending wipes %d", present, st.PendingWipes)
	})
	if n := e.auditCount(audit.ActionIngressWipeDone); n != 1 {
		t.Errorf("%d ingress.wipe_done audit rows, want one", n)
	}
}

// auditCount is the number of audit rows of an action.
func (e *swarmEnv) auditCount(action string) int {
	e.t.Helper()
	var n int
	if err := e.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n); err != nil {
		e.t.Fatalf("count audit rows: %v", err)
	}
	return n
}
