// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitsync_test

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/stackops"
)

// Auto-deploy turned off, or paused by a roll back, while the pull of an automatic deploy runs is not deployed over:
// the settings are read again just before the deploy starts (review WR-B02).
func TestAutoDeployStopsWhenWithdrawnDuringThePull(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		on     bool
		reason string
		seq    int64
	}{
		"turned off":             {on: false},
		"paused for a roll back": {on: false, reason: stackops.PausedRollback, seq: 1},
		"paused for the owner":   {on: true, reason: stackops.PausedOwner},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := newAutoFixture(t, "operator")
			a.dep.set(func(d *fakeDeployer) {
				d.whileWorkingOut = func() {
					if err := a.rr.SetAutoDeploy(context.Background(), a.stackID, tc.on, tc.reason, tc.seq, nil); err != nil {
						t.Errorf("SetAutoDeploy: %v", err)
					}
				}
			})
			a.changeWeb(t, "1")

			a.poll(t)
			if n := a.dep.count(); n != 0 {
				t.Errorf("deploys started = %d, want none: auto-deploy was withdrawn before the deploy began", n)
			}
			if sg := a.settings(t); sg.AutoDeploy != tc.on || sg.AutoDeployPausedReason != tc.reason {
				t.Errorf("settings = %+v, want what the person set (on %v, paused %q) left alone", sg, tc.on, tc.reason)
			}
		})
	}
}

// The writes that pause and resume for the owner's permission touch only the state they mean to: a stack that was
// turned off, or paused for a roll back, since the caller looked is left as it is (review WR-B02).
func TestOwnerPauseWritesAreConditional(t *testing.T) {
	t.Parallel()
	a := newAutoFixture(t, "operator")
	ctx := context.Background()

	// A roll back's pause survives a stale resume.
	if err := a.rr.SetAutoDeploy(ctx, a.stackID, false, stackops.PausedRollback, 3, nil); err != nil {
		t.Fatal(err)
	}
	if done, err := a.rr.ResumeAutoDeployFromOwner(ctx, a.stackID); err != nil || done {
		t.Fatalf("resume of a roll back's pause = %v, %v, want false and no error", done, err)
	}
	if sg := a.settings(t); sg.AutoDeploy || sg.AutoDeployPausedReason != stackops.PausedRollback || sg.AutoDeployPausedSeq != 3 {
		t.Errorf("settings = %+v, want the roll back's pause untouched", sg)
	}
	// And a stale pause for the owner does not turn a stack that is off back on.
	if done, err := a.rr.PauseAutoDeployForOwner(ctx, a.stackID); err != nil || done {
		t.Fatalf("pause of a stack that is off = %v, %v, want false and no error", done, err)
	}
	if sg := a.settings(t); sg.AutoDeploy || sg.AutoDeployPausedReason != stackops.PausedRollback {
		t.Errorf("settings = %+v, want the stack still off", sg)
	}

	// On a running stack the pair works: pause for the owner, then lift exactly that pause.
	if err := a.rr.SetAutoDeploy(ctx, a.stackID, true, "", 0, nil); err != nil {
		t.Fatal(err)
	}
	if done, err := a.rr.PauseAutoDeployForOwner(ctx, a.stackID); err != nil || !done {
		t.Fatalf("pause of a running stack = %v, %v, want true", done, err)
	}
	if sg := a.settings(t); !sg.AutoDeploy || sg.AutoDeployPausedReason != stackops.PausedOwner {
		t.Errorf("settings = %+v, want paused for the owner", sg)
	}
	if done, err := a.rr.ResumeAutoDeployFromOwner(ctx, a.stackID); err != nil || !done {
		t.Fatalf("resume of the owner's pause = %v, %v, want true", done, err)
	}
	if sg := a.settings(t); !sg.AutoDeploy || sg.AutoDeployPausedReason != "" {
		t.Errorf("settings = %+v, want running", sg)
	}
}
