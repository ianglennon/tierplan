// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/netspace"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// The orphan check (plan 10-10, NET-04, D-08 to D-10, D-13). The agents are real sessions of the
// real hub answering from fake daemons, as in addressing_test.go; the clock is the rig's.

// standaloneOrphans is a standalone host holding the networks nets makes (given the rig's now), and
// the reader over it.
func standaloneOrphans(t *testing.T, nets func(now time.Time) []dockertest.FakeNetwork, containers ...dockertest.FakeContainer) (*tunRig, string, *AddressSpaces) {
	t.Helper()
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Edge")
	r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1", Networks: nets(r.clk.Now()), Containers: containers}, nil)
	r.whenOnline(t, env.ID)
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now})
	return r, env.ID, NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now})
}

func TestOrphansUnusedAfterGrace(t *testing.T) {
	var created time.Time
	r, envID, as := standaloneOrphans(t, func(now time.Time) []dockertest.FakeNetwork {
		created = now.Add(-25 * time.Hour)
		return []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Created: created},
			{ID: "idle", Name: "idle", Driver: "bridge", Scope: "local", Subnets: []string{"172.30.0.0/24"}, Created: created},
		}
	})
	rep, err := as.Orphans(testsupport.Context(t), envID)
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %+v, want only the idle network: bridge is the engine's own", rep.Rows)
	}
	row := rep.Rows[0]
	if row.Name != "idle" || row.Class != netspace.ClassUnused || !row.IdleSince.Equal(created) || row.IdleFromSeen || row.FormerOwner != "" {
		t.Errorf("row = %+v, want idle, unused, idle since its creation and not from a time seen", row)
	}
	if !rep.Complete || len(rep.WithheldNodes) != 0 || rep.ManagerMissing {
		t.Errorf("report = %+v, want a complete check with nothing withheld", rep)
	}
	seen, err := r.st.RQ.ListNetworkSeen(testsupport.Context(t), envID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("network_seen = %+v, want a clock for each network of the host", seen)
	}
	for _, s := range seen {
		if s.LastInUseAt != 0 || s.FirstSeenAt != r.clk.Now().UnixNano() {
			t.Errorf("clock = %+v, want first seen now and never in use", s)
		}
	}
}
