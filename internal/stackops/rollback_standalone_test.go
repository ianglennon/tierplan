// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Preparing a standalone revision to be restored (plan 08-05, STK-08, D-06): the revision's own file and the
// references it stored, with nothing asked of an engine or a registry.

const restorableServices = "services:\n  web:\n    image: nginx:1.27-alpine\n  db:\n    image: postgres:16\n"

func TestRestoredStandalonePinsUseTheStoredRefs(t *testing.T) {
	t.Parallel()
	checked := stacks.Check(testsupport.Context(t), stacks.CheckInput{
		Name: "demo", Target: stacks.TargetStandalone,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(restorableServices)}}},
	})
	if checked.Loaded == nil {
		t.Fatalf("check: %v", checked.Problems)
	}
	webRef := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	dbRef := "sha256:" + strings.Repeat("c", 64) // an image ID, as a local image is stored

	pins, err := stackops.RestoredStandalonePins(checked.Loaded.Project, map[string]string{"web": webRef, "db": dbRef, "gone": "busybox:1"})
	if err != nil {
		t.Fatalf("pins: %v", err)
	}
	// A stored pin for a service the file no longer has is not sent.
	if len(pins) != 2 {
		t.Fatalf("pins = %+v, want the web and db services alone", pins)
	}
	if p := pins["web"]; p.Ref != webRef || p.AsWritten != "nginx:1.27-alpine" || p.Action != stacks.PinKeep || p.Service != "web" {
		t.Errorf("web = %+v, want the stored reference kept, keyed by the service's name in the compose file", p)
	}
	if p := pins["db"]; p.Ref != dbRef || p.Action != stacks.PinKeep {
		t.Errorf("db = %+v, want the stored image ID kept", p)
	}
}

func TestPrepareRollbackStandaloneRefusesAMissingPin(t *testing.T) {
	t.Parallel()
	target := stackops.Snapshot{
		Seq: 3, Outcome: stackops.OutcomeGood,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(restorableServices)}}},
	}
	webRef := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)

	_, err := stackops.Deps{}.PrepareRollbackStandalone(testsupport.Context(t), standaloneStackOf("demo"), target, map[string]string{"web": webRef})

	var pe *stackops.PrepareError
	var invalid *stackops.PinsInvalidError
	if !errors.As(err, &pe) || pe.Stage != stackops.PreparePins || !errors.As(pe.Err, &invalid) || invalid.Service != "db" {
		t.Fatalf("err = %v, want a pins refusal naming db", err)
	}
}

func TestPrepareRollbackStandaloneHandsBackTheRevisionAsItWasStored(t *testing.T) {
	t.Parallel()
	target := stackops.Snapshot{
		Seq: 3, Outcome: stackops.OutcomeGood,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(webStandalone)}}},
	}
	webRef := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)

	prep, err := stackops.Deps{}.PrepareRollbackStandalone(testsupport.Context(t), standaloneStackOf("demo"), target, map[string]string{"web": webRef})

	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prep.Loaded == nil || prep.Loaded.Project.Services["web"].Image != "nginx:1.27-alpine" || prep.Pins["web"] != webRef || len(prep.Plan.Services) != 0 {
		t.Errorf("plan = %+v, want the loaded project as written, the stored pins and no swarm services", prep)
	}
}

func TestPrepareRollbackStandaloneRefusesARevisionThatNoLongerLoads(t *testing.T) {
	t.Parallel()
	target := stackops.Snapshot{
		Seq: 3, Outcome: stackops.OutcomeGood,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte("services: [")}}},
	}

	_, err := stackops.Deps{}.PrepareRollbackStandalone(testsupport.Context(t), standaloneStackOf("demo"), target, nil)

	var pe *stackops.PrepareError
	if !errors.As(err, &pe) || pe.Stage != stackops.PrepareLoad {
		t.Fatalf("err = %v, want a refusal at the load stage", err)
	}
}
