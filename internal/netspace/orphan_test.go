// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package netspace

import (
	"testing"
	"time"
)

var orphanNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// idle is an input for a network that nothing uses, whose picture is complete, that has been idle
// for two days and that no stack owns: the plain unused network. A case changes what it tests.
func idle() OrphanInput {
	return OrphanInput{Known: true, Created: orphanNow.Add(-48 * time.Hour)}
}

// TestClassifyOrder fixes the order of the rules (RESEARCH Pattern 4): each case sets two or more
// reasons and names which one wins, so a later change to the order fails here.
func TestClassifyOrder(t *testing.T) {
	cases := []struct {
		name string
		set  func(*OrphanInput)
		want Class
	}{
		{"nothing special is unused", func(*OrphanInput) {}, ClassUnused},
		{"an owner whose stack is gone is orphaned", func(in *OrphanInput) { in.OwnerLabel = "shop" }, ClassOrphaned},
		{"an owner whose stack exists is owned", func(in *OrphanInput) { in.OwnerLabel, in.StackExists = "shop", true }, ClassOwned},
		{"a stack that exists does not matter without a label", func(in *OrphanInput) { in.StackExists = true }, ClassUnused},
		{"built-in beats in use", func(in *OrphanInput) { in.Builtin, in.InUse = true, true }, ClassExcluded},
		{"built-in beats an incomplete picture", func(in *OrphanInput) { in.Builtin, in.Known = true, false }, ClassExcluded},
		{"in use beats the keep label", func(in *OrphanInput) { in.InUse, in.KeepLabel = true, true }, ClassInUse},
		{"in use beats an incomplete picture", func(in *OrphanInput) { in.InUse, in.Known = true, false }, ClassInUse},
		{"the keep label keeps", func(in *OrphanInput) { in.KeepLabel = true }, ClassKept},
		{"a keep row keeps", func(in *OrphanInput) { in.KeepRow = true }, ClassKept},
		{"a keep row beats external", func(in *OrphanInput) { in.KeepRow, in.External = true, true }, ClassKept},
		{"external is excluded", func(in *OrphanInput) { in.External = true }, ClassExcluded},
		{"external beats busy", func(in *OrphanInput) { in.External, in.Busy = true, true }, ClassExcluded},
		{"busy skips", func(in *OrphanInput) { in.Busy, in.OwnerLabel = true, "web" }, ClassSkipped},
		{"busy beats grace", func(in *OrphanInput) { in.Busy, in.Created = true, orphanNow }, ClassSkipped},
		{"grace beats an incomplete picture", func(in *OrphanInput) { in.Created, in.Known = orphanNow, false }, ClassGrace},
		{"an incomplete picture is cant_tell", func(in *OrphanInput) { in.Known = false }, ClassCantTell},
		{"an incomplete picture beats an owner that is gone", func(in *OrphanInput) { in.Known, in.OwnerLabel = false, "shop" }, ClassCantTell},
		{"an unknown external declaration makes an unlabelled network cant_tell", func(in *OrphanInput) { in.ExternalsUnknown = true }, ClassCantTell},
		{"an unknown external declaration makes an orphan cant_tell", func(in *OrphanInput) { in.ExternalsUnknown, in.OwnerLabel = true, "shop" }, ClassCantTell},
		{"an owner that exists is owned whatever stack did not load", func(in *OrphanInput) {
			in.ExternalsUnknown, in.OwnerLabel, in.StackExists = true, "shop", true
		}, ClassOwned},
		{"last seen in use recently is grace", func(in *OrphanInput) { in.LastInUse = orphanNow.Add(-time.Hour) }, ClassGrace},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := idle()
			c.set(&in)
			if got := Classify(in, orphanNow); got != c.want {
				t.Errorf("Classify = %s, want %s (input %+v)", got, c.want, in)
			}
		})
	}
}

// TestClassifyGraceBoundary: a network is listed when now >= idleSince + 24h, so at 24 hours
// less a nanosecond it is not and at exactly 24 hours it is (D-08).
func TestClassifyGraceBoundary(t *testing.T) {
	for _, c := range []struct {
		name         string
		created, use time.Time
		want         Class
	}{
		{"created exactly 24h ago", orphanNow.Add(-GracePeriod), time.Time{}, ClassUnused},
		{"created 24h less 1ns ago", orphanNow.Add(-GracePeriod + 1), time.Time{}, ClassGrace},
		{"created 24h and 1ns ago", orphanNow.Add(-GracePeriod - 1), time.Time{}, ClassUnused},
		{"old network last in use exactly 24h ago", orphanNow.Add(-72 * time.Hour), orphanNow.Add(-GracePeriod), ClassUnused},
		{"old network last in use 24h less 1ns ago", orphanNow.Add(-72 * time.Hour), orphanNow.Add(-GracePeriod + 1), ClassGrace},
		{"last in use before it was created counts the creation", orphanNow.Add(-GracePeriod + 1), orphanNow.Add(-72 * time.Hour), ClassGrace},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := OrphanInput{Known: true, Created: c.created, LastInUse: c.use}
			if got := Classify(in, orphanNow); got != c.want {
				t.Errorf("Classify = %s, want %s", got, c.want)
			}
		})
	}
}

func TestIdleSince(t *testing.T) {
	a, b := orphanNow.Add(-time.Hour), orphanNow.Add(-2*time.Hour)
	if got := IdleSince(b, a); !got.Equal(a) {
		t.Errorf("IdleSince(older creation, newer use) = %v, want the use", got)
	}
	if got := IdleSince(a, b); !got.Equal(a) {
		t.Errorf("IdleSince(newer creation, older use) = %v, want the creation", got)
	}
	if got := IdleSince(a, time.Time{}); !got.Equal(a) {
		t.Errorf("IdleSince(creation, never used) = %v, want the creation", got)
	}
}
