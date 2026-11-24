// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// factsRig is a status rig whose clock stands at vipT0 plus two hours, so the moves of the last hour are exact.
func factsRig(t *testing.T) (*statusRig, time.Time) {
	t.Helper()
	now := vipT0.Add(2 * time.Hour)
	return newStatusRigAt(t, now), now
}

func (s *statusRig) mustFacts() envs.VIPFacts {
	s.t.Helper()
	f, ok, err := s.svc.VIPFacts(testsupport.Context(s.t), s.envID)
	if err != nil || !ok {
		s.t.Fatalf("VIPFacts = %+v, %v, %v; want facts", f, ok, err)
	}
	return f
}

// TestVIPFactsFromState pins what the bell is given (A-07): the row, the reader's nodes, the holder-less instant, the moves
// of the last hour through VIPMovesSince and the manager table.
func TestVIPFactsFromState(t *testing.T) {
	ctx := testsupport.Context(t)

	t.Run("no row is not an environment with a virtual IP", func(t *testing.T) {
		s, _ := factsRig(t)
		if _, err := s.istore.DeleteVIP(ctx, s.envID); err != nil {
			t.Fatalf("DeleteVIP: %v", err)
		}
		f, ok, err := s.svc.VIPFacts(ctx, s.envID)
		if err != nil || ok {
			t.Fatalf("VIPFacts = %+v, %v, %v; want false and no error", f, ok, err)
		}
	})

	t.Run("a node holds the address", func(t *testing.T) {
		s, _ := factsRig(t)
		s.svc.SeedVIPState(s.envID, []ingressdeploy.SeedNode{
			{NodeID: vipNodeA, Hostname: "node-a", State: ingressbundle.VIPStateMaster},
			{NodeID: vipNodeB, Hostname: "node-b", State: ingressbundle.VIPStateBackup},
		}, time.Time{})
		f := s.mustFacts()
		if !f.Enabled || f.Removing || f.Address != "192.168.10.250/24" || f.RouterID != 51 || f.AuthWindow != ingressbundle.VIPAuthWindow {
			t.Fatalf("the configuration = %+v", f)
		}
		if len(f.Holders) != 1 || f.Holders[0].NodeID != vipNodeA || f.Holders[0].Host != "node-a" || !f.NoHolderSince.IsZero() {
			t.Fatalf("holders = %+v since %v, want node-a alone and no holder-less instant", f.Holders, f.NoHolderSince)
		}
		if f.ForeignRouter || len(f.ClockNodes) != 0 || len(f.Moves) != 0 || len(f.PrivilegedLost) != 0 || len(f.Faulting) != 0 {
			t.Fatalf("facts = %+v, want nothing else", f)
		}
	})

	t.Run("no holder, a node starting, a foreign router and a clock", func(t *testing.T) {
		s, now := factsRig(t)
		lost := now.Add(-time.Minute)
		started := now.Add(-5 * time.Second)
		s.svc.SeedVIPState(s.envID, []ingressdeploy.SeedNode{
			{NodeID: vipNodeA, Hostname: "node-a", State: ingressbundle.VIPStateFault, TaskStarted: started},
			{NodeID: vipNodeB, Hostname: "node-b", State: ingressbundle.VIPStateBackup, Foreign: true, ForeignSender: "192.168.10.77", ClockSkew: true},
		}, lost)
		f := s.mustFacts()
		if len(f.Holders) != 0 || !f.NoHolderSince.Equal(lost) {
			t.Fatalf("holders = %+v since %v, want none since %v", f.Holders, f.NoHolderSince, lost)
		}
		if len(f.Faulting) != 1 || f.Faulting[0].Host != "node-a" || !f.Faulting[0].TaskStarted.Equal(started) {
			t.Fatalf("faulting = %+v, want node-a with its task start", f.Faulting)
		}
		if !f.ForeignRouter || f.ForeignSender != "192.168.10.77" {
			t.Fatalf("foreign = %v %q, want the router and its sender", f.ForeignRouter, f.ForeignSender)
		}
		if len(f.ClockNodes) != 1 || f.ClockNodes[0].Host != "node-b" || f.ClockNodes[0].NodeID != vipNodeB {
			t.Fatalf("clock nodes = %+v, want node-b", f.ClockNodes)
		}
	})

	t.Run("a sender that is not an address is dropped and a node with no name is named by its ID", func(t *testing.T) {
		s, _ := factsRig(t)
		s.svc.SeedVIPState(s.envID, []ingressdeploy.SeedNode{
			{NodeID: vipNodeA, State: ingressbundle.VIPStateMaster, Foreign: true, ForeignSender: "<b>not an address</b>", ClockSkew: true},
		}, time.Time{})
		f := s.mustFacts()
		if !f.ForeignRouter || f.ForeignSender != "" {
			t.Fatalf("foreign = %v %q, want the router without a sender", f.ForeignRouter, f.ForeignSender)
		}
		if len(f.ClockNodes) != 1 || f.ClockNodes[0].Host != vipNodeA {
			t.Fatalf("clock nodes = %+v, want the node ID where there is no host name", f.ClockNodes)
		}
	})

	t.Run("the moves are those after one hour ago and the managers those that refuse", func(t *testing.T) {
		s, now := factsRig(t)
		for _, m := range []ingressstore.VIPMove{
			{EnvID: s.envID, At: now.Add(-2 * time.Hour), FromNodeID: vipNodeA, FromNodeName: "node-a", ToNodeID: vipNodeB, ToNodeName: "node-b", Reason: "holder_failed"},
			{EnvID: s.envID, At: now.Add(-time.Hour), FromNodeID: vipNodeB, FromNodeName: "node-b", ToNodeID: vipNodeA, ToNodeName: "node-a", Reason: "holder_failed"},
			{EnvID: s.envID, At: now.Add(-30 * time.Minute), FromNodeID: vipNodeA, FromNodeName: "node-a", ToNodeID: vipNodeB, ToNodeName: "node-b", Reason: "drain_handoff"},
			{EnvID: s.envID, At: now.Add(-10 * time.Minute), FromNodeID: vipNodeB, FromNodeName: "node-b", Reason: "disabled"},
		} {
			if err := s.istore.InsertVIPMove(ctx, m); err != nil {
				t.Fatalf("InsertVIPMove: %v", err)
			}
		}
		s.managers.rows = []envs.ManagerPolicyRow{
			{NodeID: vipNodeA, Hostname: "node-a", State: envs.ManagerAllows},
			{NodeID: vipNodeB, Hostname: "node-b", State: envs.ManagerRefuses},
			{NodeID: "c", Hostname: "node-c", State: envs.ManagerUnknown},
		}
		f := s.mustFacts()
		if len(f.Moves) != 2 || f.Moves[0].Reason != "drain_handoff" || f.Moves[0].From != "node-a" || f.Moves[0].To != "node-b" ||
			!f.Moves[0].At.Equal(now.Add(-30*time.Minute)) || f.Moves[1].Reason != "disabled" || f.Moves[1].To != "" {
			t.Fatalf("moves = %+v, want the two of the last hour (the hour-old one is not after it)", f.Moves)
		}
		if len(f.PrivilegedLost) != 1 || f.PrivilegedLost[0] != "node-b" {
			t.Fatalf("privileged lost = %v, want node-b alone (an unknown manager does not count)", f.PrivilegedLost)
		}
	})

	t.Run("a manager table that cannot be read leaves that item out", func(t *testing.T) {
		s, _ := factsRig(t)
		s.managers.err = errors.New("quorum lost")
		f := s.mustFacts()
		if len(f.PrivilegedLost) != 0 {
			t.Fatalf("privileged lost = %v, want none", f.PrivilegedLost)
		}
	})

	t.Run("an environment being switched off is removing", func(t *testing.T) {
		s, _ := factsRig(t)
		if err := s.istore.SetVIPEnabled(ctx, s.envID, false); err != nil {
			t.Fatalf("SetVIPEnabled: %v", err)
		}
		f := s.mustFacts()
		if f.Enabled || !f.Removing {
			t.Fatalf("enabled %v removing %v for enabled 0, want removing", f.Enabled, f.Removing)
		}
	})

	t.Run("a switch-off that is running is removing too", func(t *testing.T) {
		s, _ := factsRig(t)
		s.svc.MarkVIPDisabling(s.envID, true)
		if f := s.mustFacts(); !f.Enabled || !f.Removing {
			t.Fatalf("enabled %v removing %v while the disable runs, want enabled and removing", f.Enabled, f.Removing)
		}
		s.svc.MarkVIPDisabling(s.envID, false)
		if f := s.mustFacts(); f.Removing {
			t.Fatalf("removing after the disable ended")
		}
	})

	t.Run("the facts hold no key", func(t *testing.T) {
		s, _ := factsRig(t)
		s.svc.SeedVIPState(s.envID, []ingressdeploy.SeedNode{{NodeID: vipNodeA, Hostname: "node-a", State: ingressbundle.VIPStateMaster}}, time.Time{})
		// The rig's row is sealed as "sealed": it must not appear in anything the bell is given (PROH-26-03).
		if dump := fmt.Sprintf("%+v", s.mustFacts()); strings.Contains(dump, "sealed") || strings.Contains(strings.ToLower(dump), "key") {
			t.Fatalf("the facts carry the key or its name: %s", dump)
		}
	})
}
