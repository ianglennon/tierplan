// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
)

// Docker names are unique per host, not per environment: recovery of a container update takes the container that holds the name and the
// replacement under its temporary name from the old container's node only (WR-07). A same-named container on another node is not part
// of the update, whatever order the nodes answered in.
func TestContainerSceneIsOfTheOldContainersNode(t *testing.T) {
	t.Parallel()
	const tempName = "app-tierplan-new-cccccccccccc"
	row := func(id, name, node string) envs.ContainerRow {
		return envs.ContainerRow{ContainerRow: dockerx.ContainerRow{ID: id, Name: "/" + name}, NodeID: node}
	}
	run := Run{Workload: "app", WorkloadID: "old-1"}

	for _, tc := range []struct {
		name  string
		rows  []envs.ContainerRow
		named string
		temp  string
	}{
		{
			name:  "the other node answered first",
			rows:  []envs.ContainerRow{row("other-named", "app", "n2"), row("other-temp", tempName, "n2"), row("old-1", "app-tierplan-old-cccccccccccc", "n1"), row("new-1", "app", "n1"), row("temp-1", tempName, "n1")},
			named: "new-1", temp: "temp-1",
		},
		{
			name:  "the other node answered last",
			rows:  []envs.ContainerRow{row("old-1", "app-tierplan-old-cccccccccccc", "n1"), row("new-1", "app", "n1"), row("other-named", "app", "n2")},
			named: "new-1",
		},
		{
			name: "nothing of the update on its node",
			rows: []envs.ContainerRow{row("old-1", "app", "n1"), row("other-named", "app", "n2"), row("other-temp", tempName, "n2")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sc, haveOld := containerSceneOf(envs.Aggregate{Containers: tc.rows}, run, tempName)
			if !haveOld || sc.node != "n1" {
				t.Fatalf("old = %v on %q, want it found on n1", haveOld, sc.node)
			}
			id := func(c *envs.ContainerRow) string {
				if c == nil {
					return ""
				}
				return c.ID
			}
			if id(sc.named) != tc.named || id(sc.temp) != tc.temp {
				t.Errorf("named = %q, temp = %q, want %q and %q", id(sc.named), id(sc.temp), tc.named, tc.temp)
			}
		})
	}
}
