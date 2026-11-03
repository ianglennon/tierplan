// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"testing"
	"time"
)

// One grace per environment, for everything (plan 11-25, D-20, JAN-04, JAN-06): the Address space page's
// orphan list and the janitor read the same setting, so the two screens never disagree about whether a
// network is old enough.

// orphanNames are the names the Address space page lists as orphaned or unused.
func (f *envFix) orphanNames(t *testing.T, e envBody) (names []string, graceHours int) {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	rec := f.get(t, e, "address-space")
	if rec.code != http.StatusOK {
		t.Fatalf("address space: %d %s", rec.code, rec.body)
	}
	space := decode[addressSpace](t, rec)
	for _, o := range space.Orphans {
		names = append(names, o.Name)
	}
	return names, space.GraceHours
}

// janitorSays is what the janitor's preview makes of the network called name: its class, and "" when the
// preview does not list it.
func (f *envFix) janitorSays(t *testing.T, e envBody, name string) string {
	t.Helper()
	p := f.previewJanitor(t, e)
	for _, it := range append(append([]janitorItem{}, p.Items...), p.NotRemoved...) {
		if it.Type == "network" && it.Name == name {
			return it.Class
		}
	}
	return ""
}

// agree fails unless the Address space list and the janitor treat the network the same way: both hold it
// back for its grace, or both call it removable.
func (f *envFix) agree(t *testing.T, e envBody, name string, wantHeld bool, wantGraceHours int) {
	t.Helper()
	names, grace := f.orphanNames(t, e)
	listed := false
	for _, n := range names {
		listed = listed || n == name
	}
	class := f.janitorSays(t, e, name)
	if grace != wantGraceHours {
		t.Errorf("address space graceHours = %d, want %d", grace, wantGraceHours)
	}
	switch {
	case wantHeld && (listed || class != "grace"):
		t.Errorf("%s: address space lists it = %v, janitor says %q, want it held back by both (grace)", name, listed, class)
	case !wantHeld && (!listed || class != "removable"):
		t.Errorf("%s: address space lists it = %v, janitor says %q, want it listed and removable by both", name, listed, class)
	}
}

// The tracer: a network idle for 30 hours is orphaned on both screens under the default grace; setting the
// environment's grace to 48 hours holds it back on both, and the Address space body says which grace it used.
// A shorter grace releases a network the default holds back, on both.
func TestAddressSpaceAndJanitorShareGrace(t *testing.T) {
	t.Parallel()
	t.Run("a longer grace holds a network back on both screens", func(t *testing.T) {
		t.Parallel()
		f, e := dashEnv(t, orphanEngine(stackNet("old1", "oldshop_default", "oldshop", 30*time.Hour)))
		f.agree(t, e, "oldshop_default", false, 24)

		rec := f.putJanitorPolicy(t, e, policyJSON(0, map[string]any{"graceHours": 48}))
		if rec.code != http.StatusOK {
			t.Fatalf("set the grace: %d %s", rec.code, rec.body)
		}
		f.agree(t, e, "oldshop_default", true, 48)

		// Back to following the default: both release it again.
		rec = f.putJanitorPolicy(t, e, policyJSON(1, nil))
		if rec.code != http.StatusOK {
			t.Fatalf("reset the grace: %d %s", rec.code, rec.body)
		}
		f.agree(t, e, "oldshop_default", false, 24)
	})
	t.Run("a shorter grace releases a network on both screens", func(t *testing.T) {
		t.Parallel()
		f, e := dashEnv(t, orphanEngine(stackNet("young1", "youngshop_default", "youngshop", 10*time.Hour)))
		f.agree(t, e, "youngshop_default", true, 24)

		rec := f.putJanitorPolicy(t, e, policyJSON(0, map[string]any{"graceHours": 6}))
		if rec.code != http.StatusOK {
			t.Fatalf("set the grace: %d %s", rec.code, rec.body)
		}
		f.agree(t, e, "youngshop_default", false, 6)
	})
	t.Run("a network idle exactly the grace is listed by both", func(t *testing.T) {
		t.Parallel()
		f, e := dashEnv(t, orphanEngine(stackNet("edge1", "edgeshop_default", "edgeshop", 48*time.Hour)))
		if rec := f.putJanitorPolicy(t, e, policyJSON(0, map[string]any{"graceHours": 48})); rec.code != http.StatusOK {
			t.Fatalf("set the grace: %d %s", rec.code, rec.body)
		}
		f.agree(t, e, "edgeshop_default", false, 48)
	})
}
