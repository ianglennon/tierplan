// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/updates"
)

// A service's failed update needs services.read and a container's containers.read (plan 12-44, T-12-73): the two are separate
// grants, so each is tried alone.
func TestBellUpdateItemsFilteredByWhatTheCallerMayRead(t *testing.T) {
	t.Parallel()
	failed := []updates.FailedUpdate{
		{Failure: updates.Failure{Kind: "service", Workload: "web", Digest: "sha256:b"}, WorkloadID: "svc-web"},
		{Failure: updates.Failure{Kind: "container", Workload: "db", Digest: "sha256:b"}, WorkloadID: "c-db"},
	}
	for _, tc := range []struct {
		name         string
		svc, ctr     bool
		wantWorkload []string
	}{
		{"both", true, true, []string{"web", "db"}},
		{"services only", true, false, []string{"web"}},
		{"containers only", false, true, []string{"db"}},
		{"neither", false, false, nil},
	} {
		got := readableUpdateRollbacks(failed, tc.svc, tc.ctr)
		if len(got) != len(tc.wantWorkload) {
			t.Errorf("%s: %+v, want %v", tc.name, got, tc.wantWorkload)
			continue
		}
		for i, w := range tc.wantWorkload {
			if got[i].Workload != w {
				t.Errorf("%s: item %d = %q, want %q", tc.name, i, got[i].Workload, w)
			}
		}
	}
}
