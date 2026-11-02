// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/netspace"
)

// What the janitor makes of a network the orphan check judged (plan 11-08, JAN-04, D-01, D-11, P10
// D-08 to D-13). Every case judges at t0.

// netRow is a network of class c on a node, idle since idle before t0.
func netRow(c netspace.Class, idle time.Duration) NetworkRow {
	return NetworkRow{
		ID: "n1", Name: "shop_default", NodeID: "node-1", Node: "wrk-1", Scope: "local", Class: c,
		FormerOwner: "shop", FormerOwnerKind: "stack", IdleSince: t0.Add(-idle),
	}
}

func TestClassifyNetworkOrphaned(t *testing.T) {
	t.Parallel()
	item, listed := ClassifyNetwork(netRow(netspace.ClassOrphaned, 25*time.Hour), true, DefaultPolicy(), t0)
	if !listed || item.Class != ClassRemovable || item.Reason.Code != ReasonNetworkOrphaned || item.Reason.Stack != "shop" {
		t.Fatalf("item = %+v listed = %v, want removable network_orphaned naming the stack shop", item, listed)
	}
	if item.Type != KindNetwork || item.Ref != "n1" || item.Name != "shop_default" || item.NodeID != "node-1" || item.Node != "wrk-1" || item.Size != -1 {
		t.Errorf("item = %+v, want the network's identity and no known size", item)
	}
	if item.Age != 25*time.Hour {
		t.Errorf("age = %v, want 25h", item.Age)
	}
	t.Run("a compose project is named as a project", func(t *testing.T) {
		t.Parallel()
		row := netRow(netspace.ClassOrphaned, 48*time.Hour)
		row.FormerOwnerKind = "project"
		item, _ := ClassifyNetwork(row, true, DefaultPolicy(), t0)
		if item.Reason.Project != "shop" || item.Reason.Stack != "" {
			t.Errorf("reason = %+v, want the project shop and no stack", item.Reason)
		}
	})
	t.Run("opting in to unused networks changes nothing for an orphaned one", func(t *testing.T) {
		t.Parallel()
		p := DefaultPolicy()
		p.RemoveUnusedNetworks = true
		item, _ := ClassifyNetwork(netRow(netspace.ClassOrphaned, 25*time.Hour), true, p, t0)
		if item.Class != ClassRemovable || item.Reason.Code != ReasonNetworkOrphaned {
			t.Errorf("item = %+v, want removable network_orphaned", item)
		}
	})
}

func TestClassifyNetworkUnusedNeedsOptIn(t *testing.T) {
	t.Parallel()
	row := netRow(netspace.ClassUnused, 72*time.Hour)
	row.FormerOwner, row.FormerOwnerKind = "", ""

	off, listed := ClassifyNetwork(row, true, DefaultPolicy(), t0)
	if !listed || off.Class != ClassReported || off.Reason.Code != ReasonOptInOffNetworks {
		t.Fatalf("opt-in off: %+v listed = %v, want reported opt_in_off_networks", off, listed)
	}
	p := DefaultPolicy()
	p.RemoveUnusedNetworks = true
	on, listed := ClassifyNetwork(row, true, p, t0)
	if !listed || on.Class != ClassRemovable || on.Reason.Code != ReasonNetworkUnused || on.Reason.Stack != "" {
		t.Fatalf("opt-in on: %+v listed = %v, want removable network_unused", on, listed)
	}
	// The opt-in decides only the class: it is the same network.
	off.Class, off.Reason.Code = on.Class, on.Reason.Code
	if !reflect.DeepEqual(off, on) {
		t.Errorf("opt-in off %+v and on %+v differ in more than the class and the reason", off, on)
	}
}

func TestClassifyNetworkGrace(t *testing.T) {
	t.Parallel()
	for _, c := range []netspace.Class{netspace.ClassOrphaned, netspace.ClassUnused, netspace.ClassGrace} {
		t.Run(string(c), func(t *testing.T) {
			t.Parallel()
			p := DefaultPolicy()
			p.RemoveUnusedNetworks = true
			// Idle for 20 h of a 24 h grace period: 4 h to go. The orphan check calls it grace itself, and a
			// network it called orphaned or unused is held by the same clock.
			item, listed := ClassifyNetwork(netRow(c, 20*time.Hour), true, p, t0)
			if !listed || item.Class != ClassGrace || item.Reason.Code != ReasonYoungerThanGrace || item.Reason.Remaining != 4*time.Hour || item.Reason.Age != 20*time.Hour {
				t.Fatalf("item = %+v listed = %v, want grace younger_than_grace, 4h remaining after 20h", item, listed)
			}
		})
	}
	t.Run("a policy grace longer than the check's holds a network the check listed", func(t *testing.T) {
		t.Parallel()
		p := DefaultPolicy()
		p.Grace = 72 * time.Hour
		item, _ := ClassifyNetwork(netRow(netspace.ClassOrphaned, 30*time.Hour), true, p, t0)
		if item.Class != ClassGrace || item.Reason.Remaining != 42*time.Hour {
			t.Errorf("item = %+v, want grace with 42h remaining", item)
		}
	})
	t.Run("a policy grace shorter than the check's does not hurry it", func(t *testing.T) {
		t.Parallel()
		p := DefaultPolicy()
		p.Grace = time.Hour
		item, _ := ClassifyNetwork(netRow(netspace.ClassGrace, 10*time.Hour), true, p, t0)
		if item.Class != ClassGrace || item.Reason.Remaining != 14*time.Hour {
			t.Errorf("item = %+v, want grace with 14h remaining: the check's 24h still holds", item)
		}
	})
	t.Run("at exactly the grace period it is removable", func(t *testing.T) {
		t.Parallel()
		item, _ := ClassifyNetwork(netRow(netspace.ClassOrphaned, 24*time.Hour), true, DefaultPolicy(), t0)
		if item.Class != ClassRemovable {
			t.Errorf("item = %+v, want removable at exactly 24h", item)
		}
	})
	t.Run("grace holds while the picture is incomplete", func(t *testing.T) {
		t.Parallel()
		item, _ := ClassifyNetwork(netRow(netspace.ClassGrace, 2*time.Hour), false, DefaultPolicy(), t0)
		if item.Class != ClassGrace {
			t.Errorf("item = %+v, want grace: the clock decides before completeness does", item)
		}
	})
}

func TestClassifyNetworkKept(t *testing.T) {
	t.Parallel()
	byLabel := netRow(netspace.ClassKept, 72*time.Hour)
	byLabel.KeptByLabel = true
	byRow := netRow(netspace.ClassKept, 72*time.Hour)
	byRow.KeptByRow = true
	both := netRow(netspace.ClassKept, 72*time.Hour)
	both.KeptByLabel, both.KeptByRow = true, true
	for name, tc := range map[string]struct {
		row  NetworkRow
		want ReasonCode
	}{
		"label": {byLabel, ReasonKeptLabel},
		"row":   {byRow, ReasonKeptRow},
		"both":  {both, ReasonKeptLabel},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := DefaultPolicy()
			p.RemoveUnusedNetworks = true
			item, listed := ClassifyNetwork(tc.row, true, p, t0)
			if !listed || item.Class != ClassProtected || item.Reason.Code != tc.want {
				t.Fatalf("item = %+v listed = %v, want protected %s even with every opt-in on", item, listed, tc.want)
			}
		})
	}
}

func TestClassifyNetworkNotListed(t *testing.T) {
	t.Parallel()
	for _, c := range []netspace.Class{netspace.ClassExcluded, netspace.ClassInUse, netspace.ClassOwned, netspace.ClassSkipped, ""} {
		t.Run(string(c), func(t *testing.T) {
			t.Parallel()
			p := DefaultPolicy()
			p.RemoveUnusedNetworks = true
			if item, listed := ClassifyNetwork(netRow(c, 72*time.Hour), true, p, t0); listed {
				t.Fatalf("a %q network was listed: %+v", c, item)
			}
		})
	}
}

func TestClassifyNetworkCantTell(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	p.RemoveUnusedNetworks = true
	// A node that does not report: the check cannot tell.
	item, listed := ClassifyNetwork(netRow(netspace.ClassCantTell, 72*time.Hour), false, p, t0)
	if !listed || item.Class != ClassCantTell || item.Reason.Code != ReasonNetworksWithheld {
		t.Fatalf("item = %+v listed = %v, want cant_tell networks_withheld", item, listed)
	}
	// Every node reported, so what is unknown is a stack that did not load, and the network is
	// never removable for it: not with the opt-in on either.
	item, listed = ClassifyNetwork(netRow(netspace.ClassCantTell, 72*time.Hour), true, p, t0)
	if !listed || item.Class != ClassCantTell || item.Reason.Code != ReasonStackUnknown {
		t.Fatalf("item = %+v listed = %v, want cant_tell stack_unknown", item, listed)
	}
}

// While a node is not reporting, or no manager answered, nothing is removable whatever the class
// (JAN-04: the janitor MUST NOT judge any network while a node is not reporting).
func TestNetworksWithheldWhenIncomplete(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	p.RemoveUnusedNetworks = true
	for _, c := range []netspace.Class{netspace.ClassOrphaned, netspace.ClassUnused, netspace.ClassCantTell} {
		t.Run(string(c), func(t *testing.T) {
			t.Parallel()
			item, listed := ClassifyNetwork(netRow(c, 72*time.Hour), false, p, t0)
			if !listed || item.Class != ClassCantTell || item.Reason.Code != ReasonNetworksWithheld {
				t.Fatalf("a %s network of an incomplete report: %+v listed = %v, want cant_tell networks_withheld", c, item, listed)
			}
		})
	}

	t.Run("the plan says so, names the nodes and lists none as removable", func(t *testing.T) {
		t.Parallel()
		rep := NetworkReport{
			Complete: false, WithheldNodes: []string{"wrk-2"},
			Rows: []NetworkRow{netRow(netspace.ClassOrphaned, 72*time.Hour), netRow(netspace.ClassUnused, 72*time.Hour)},
		}
		rep.Rows[1].ID, rep.Rows[1].Name = "n2", "loose"
		s := swarmOf()
		s.NotReporting = []string{"wrk-1"}
		plan := Build(Input{Snapshot: s, Networks: &rep}, p, t0)
		if !plan.NetworksWithheld || len(plan.Items) != 0 || len(plan.NotRemoved) != 2 {
			t.Fatalf("plan = %+v, want networks withheld and both networks left", plan)
		}
		if !slices.Equal(plan.WithheldNodes, []string{"wrk-1", "wrk-2"}) {
			t.Errorf("withheld nodes = %v, want both nodes named once", plan.WithheldNodes)
		}
		for _, it := range plan.NotRemoved {
			if it.Class != ClassCantTell || it.Reason.Code != ReasonNetworksWithheld {
				t.Errorf("%+v, want cant_tell networks_withheld", it)
			}
		}
		if _, networks, _, _, _ := plan.Summary(); networks != 0 {
			t.Errorf("summary counts %d networks, want none removable", networks)
		}
	})

	t.Run("a complete report withholds nothing", func(t *testing.T) {
		t.Parallel()
		rep := NetworkReport{Complete: true, Rows: []NetworkRow{netRow(netspace.ClassOrphaned, 72*time.Hour)}}
		plan := Build(Input{Snapshot: swarmOf(), Networks: &rep}, p, t0)
		if plan.NetworksWithheld || len(plan.Items) != 1 || plan.Items[0].Reason.Code != ReasonNetworkOrphaned {
			t.Fatalf("plan = %+v, want the orphaned network removable and nothing withheld", plan)
		}
	})

	t.Run("a plan with no network report lists none and withholds none", func(t *testing.T) {
		t.Parallel()
		plan := Build(Input{Snapshot: swarmOf()}, p, t0)
		if plan.NetworksWithheld || len(plan.Items) != 0 || len(plan.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want an empty plan that withholds nothing", plan)
		}
	})
}

func TestBuildOrdersNetworksAfterContainers(t *testing.T) {
	t.Parallel()
	s := swarmOf(task("c1", "1", 0), task("c2", "1", 1), task("c3", "1", 2), task("c4", "1", 3))
	rep := NetworkReport{Complete: true, Rows: []NetworkRow{netRow(netspace.ClassOrphaned, 72*time.Hour)}}
	p := DefaultPolicy()
	ends := map[string]ContainerEnd{}
	for _, r := range Candidates(s, p) {
		ends[r.ID] = ContainerEnd{FinishedAt: t0.Add(-48 * time.Hour), SizeRw: -1}
	}
	plan := Build(Input{Snapshot: s, Ends: ends, Networks: &rep}, p, t0)
	var kinds []Kind
	for _, it := range plan.Items {
		kinds = append(kinds, it.Type)
	}
	if !slices.Equal(kinds, []Kind{KindContainer, KindContainer, KindNetwork}) {
		t.Fatalf("kinds = %v, want the two containers beyond keep-N and then the network", kinds)
	}
}
