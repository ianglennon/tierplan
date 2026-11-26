// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// vipStallEngine is an engine whose connected agents named in stall accept a call and never answer it: the call ends only
// when its context does. entered says that such a call began.
type vipStallEngine struct {
	ingressdeploy.Engine
	stall   map[string]bool
	entered chan string
}

func (e *vipStallEngine) OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	if !e.stall[nodeID] {
		return e.Engine.OnNode(ctx, envID, nodeID, fn)
	}
	select {
	case e.entered <- nodeID:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// stalled builds a service over the rig's store and nodes whose agent-b never answers.
func (v *vipRig) stalled() (*ingressdeploy.Service, *vipStallEngine) {
	eng := &vipStallEngine{Engine: v.deps.Engine, stall: map[string]bool{"agent-b": true}, entered: make(chan string, 8)}
	d := v.deps
	d.Engine = eng
	return ingressdeploy.New(d), eng
}

// signalWhenDone runs fn and closes the returned channel when it has returned.
func signalWhenDone(fn func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	return done
}

// TestVIPReaderBoundsOneNodesRead pins WR-01: a node whose agent is connected but whose engine never answers costs a pass
// no more than the per-node bound. The other node is read and the stalled one is not heard, so it is silent, and the pass
// ends instead of holding the environment for as long as the server runs.
func TestVIPReaderBoundsOneNodesRead(t *testing.T) {
	v := newVIPRig(t)
	v.a.AppendLogs("ka-a", logLine(t, vipT0, vipStateEvent(vipNodeA, 1, ingressbundle.VIPStateMaster, "")))
	svc, _ := v.stalled()
	svc.SetVIPReadTimeouts(100*time.Millisecond, time.Hour) // wall-clock: the bound a stalled engine is cut at; a correct pass ends no later, and a wrong one never ends

	done := signalWhenDone(func() { svc.ReadVIPOnce(testsupport.Context(t), v.envID) })
	testsupport.Await(t, "a pass over a node that never answers to end", done)

	if h := svc.VIPHolders(v.envID); len(h) != 1 || h[0].Hostname != "node-a" {
		t.Fatalf("holders = %v, want [node-a]: the node that answered is read whatever the other does", h)
	}
	for _, n := range svc.VIPNodeStates(v.envID) {
		if n.Hostname == "node-b" && !n.LastHeard.IsZero() {
			t.Errorf("node-b was heard at %s, but its engine never answered", n.LastHeard)
		}
	}
}

// TestVIPReaderPassGivesWayToACallerWithADeadline pins the other half of WR-01: a drain's handoff and a switch-off's wait
// read through readVIPOnce, so a pass that is stuck must not make them wait past their own context.
func TestVIPReaderPassGivesWayToACallerWithADeadline(t *testing.T) {
	v := newVIPRig(t)
	svc, eng := v.stalled()
	svc.SetVIPReadTimeouts(time.Hour, time.Hour)

	held, release := context.WithCancel(testsupport.Context(t))
	first := signalWhenDone(func() { svc.ReadVIPOnce(held, v.envID) })
	testsupport.Await(t, "the first pass to reach the stalled node", eng.entered)

	short, cancel := context.WithTimeout(testsupport.Context(t), 50*time.Millisecond) // wall-clock: the caller's own deadline; the pass it waits behind never ends by itself
	defer cancel()
	second := signalWhenDone(func() { svc.ReadVIPOnce(short, v.envID) })
	testsupport.Await(t, "a caller with a deadline to be let go while a pass is stuck", second)

	release()
	testsupport.Await(t, "the stuck pass to end once its own context did", first)
}

// TestVIPReaderRoundBoundsAnEnvironmentsPass pins that the loop bounds one environment's pass: with the per-node bound out
// of reach, the round still ends, so the other environments are read again.
func TestVIPReaderRoundBoundsAnEnvironmentsPass(t *testing.T) {
	v := newVIPRig(t)
	svc, _ := v.stalled()
	svc.SetVIPReadTimeouts(time.Hour, 100*time.Millisecond) // wall-clock: the bound of one environment's pass; a correct round ends no later, and a wrong one never ends
	rounds := make(chan struct{}, 2)
	svc.SetVIPHook(func() { rounds <- struct{}{} })

	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := signalWhenDone(func() { svc.RunVIPReader(ctx) })
	testsupport.Await(t, "a round over an environment with a stalled node to end", rounds)
	cancel()
	testsupport.Await(t, "the reader to stop", done)
}
