// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The clock an image's grace period runs from (WR-07, D-07): the first time a snapshot finds it without a
// name. An image superseded by a newer build of its tag was built long ago, so measured from its build time it
// would be removed at the next run; measured from when it lost its name it has the whole grace period.
func TestDanglingStoreStamp(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	st := storetest.New(t)
	if _, err := st.Write.ExecContext(ctx,
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'agent', 'e', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	d := DanglingStore{St: st}
	grace := DefaultPolicy().Grace
	old := bareImage("old", 90*24*time.Hour) // built 90 days ago, dangling since it was superseded
	tagged := bareImage("tagged", 90*24*time.Hour)
	tagged.RepoTags = []string{"app:1"}
	snap := func(nodes []Node, images ...Image) *Snapshot {
		return &Snapshot{Nodes: nodes, Images: images}
	}
	n1 := []Node{{ID: "n1-id", Name: "n1"}}
	stamp := func(s *Snapshot, now time.Time) {
		t.Helper()
		if err := d.Stamp(ctx, "env-1", s, now); err != nil {
			t.Fatalf("Stamp: %v", err)
		}
	}
	judge := func(s *Snapshot, now time.Time) Item {
		t.Helper()
		item, _ := ClassifyImage(s.Images[0], false, false, true, DefaultPolicy(), now)
		return item
	}

	// First seen at t0: the image is held for the whole grace period although it was built long ago.
	s := snap(n1, old, tagged)
	stamp(s, t0)
	if !s.Images[0].DanglingSince.Equal(t0) || !s.Images[1].DanglingSince.IsZero() {
		t.Fatalf("dangling since = %v and %v, want t0 for the dangling image and none for the tagged one", s.Images[0].DanglingSince, s.Images[1].DanglingSince)
	}
	if item := judge(s, t0); item.Class != ClassGrace || item.Reason.Remaining != grace {
		t.Fatalf("item = %+v, want grace for the whole period on first sight", item)
	}

	// A later snapshot keeps the first time, and once the grace period has run the image is removable.
	s = snap(n1, old)
	stamp(s, t0.Add(grace))
	if !s.Images[0].DanglingSince.Equal(t0) {
		t.Fatalf("dangling since = %v, want the first sighting kept", s.Images[0].DanglingSince)
	}
	if item := judge(s, t0.Add(grace)); item.Class != ClassRemovable {
		t.Fatalf("item = %+v, want removable once dangling for the grace period", item)
	}

	// While its node does not report, the clock is kept.
	stamp(snap(nil), t0.Add(2*grace))
	s = snap(n1, old)
	stamp(s, t0.Add(2*grace))
	if !s.Images[0].DanglingSince.Equal(t0) {
		t.Fatalf("dangling since = %v, want the clock kept while the node did not report", s.Images[0].DanglingSince)
	}

	// Tagged again, the clock stops; when it loses the tag again a new grace period starts.
	retagged := old
	retagged.RepoTags = []string{"app:2"}
	stamp(snap(n1, retagged), t0.Add(3*grace))
	s = snap(n1, old)
	stamp(s, t0.Add(4*grace))
	if !s.Images[0].DanglingSince.Equal(t0.Add(4 * grace)) {
		t.Fatalf("dangling since = %v, want a new clock after the image had a name again", s.Images[0].DanglingSince)
	}

	// Gone from a node that reports, its clock is removed.
	stamp(snap(n1), t0.Add(5*grace))
	rows, err := st.RQ.ListImageDanglingSeen(ctx, "env-1")
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows = %+v, %v; want none once the image is gone", rows, err)
	}

	// A store that is not there keeps no clock, and the build time alone counts.
	s = snap(n1, old)
	if err := (DanglingStore{}).Stamp(ctx, "env-1", s, t0); err != nil || !s.Images[0].DanglingSince.IsZero() {
		t.Fatalf("Stamp with no store = %v, dangling since %v; want nothing", err, s.Images[0].DanglingSince)
	}
}
