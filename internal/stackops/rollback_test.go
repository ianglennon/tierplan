// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

func rollbackTarget(composeText string) stackops.Snapshot {
	return stackops.Snapshot{Seq: 2, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood, Copy: stacks.WorkingCopy{Files: []stacks.File{compose(composeText)}}}
}

var rollbackStack = stackops.Stack{ID: "stack-1", Name: "web", Target: stacks.TargetSwarm}

func TestPrepareRollbackSendsTheImagesTheRevisionStoredAndNothingElse(t *testing.T) {
	t.Parallel()
	pinned := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	plan, err := stackops.Deps{}.PrepareRollback(context.Background(), rollbackStack,
		rollbackTarget("services:\n  web:\n    image: nginx:1.27-alpine\n"), map[string]string{"web_web": pinned})
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	if len(plan.Plan.Services) != 1 || plan.Plan.Services[0].Name != "web_web" || plan.Pins["web_web"] != pinned || plan.Loaded == nil {
		t.Errorf("plan = %+v", plan)
	}
	pins, err := stackops.RestoredPins(plan.Plan.Services, plan.Pins)
	if err != nil || pins["web_web"].Ref != pinned || pins["web_web"].AsWritten != "nginx:1.27-alpine" {
		t.Errorf("RestoredPins = %+v, %v, want the stored reference unchanged", pins, err)
	}
}

func TestPrepareRollbackRefusesARevisionWithNoImageStoredForAService(t *testing.T) {
	t.Parallel()
	_, err := stackops.Deps{}.PrepareRollback(context.Background(), rollbackStack,
		rollbackTarget("services:\n  web:\n    image: nginx:1.27-alpine\n  db:\n    image: postgres:16\n"),
		map[string]string{"web_web": "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)})
	var pe *stackops.PrepareError
	var invalid *stackops.PinsInvalidError
	if !errors.As(err, &pe) || pe.Stage != stackops.PreparePins || !errors.As(err, &invalid) || invalid.Service != "web_db" || invalid.Reason != "missing" {
		t.Fatalf("err = %v, want the pins stage naming web_db as missing", err)
	}
}

func TestPrepareRollbackNamesTheStageThatRefusedARevision(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ compose, stage string }{
		"a file that does not load":    {"services: [", stackops.PrepareLoad},
		"a service with no image":      {"services:\n  web:\n    build: .\n", stackops.PrepareConvert},
		"a secret with no usable file": {"services:\n  web:\n    image: x\n    secrets: [s]\nsecrets:\n  s:\n    file: missing.txt\n", stackops.PrepareObjects},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := stackops.Deps{}.PrepareRollback(context.Background(), rollbackStack, rollbackTarget(tc.compose), map[string]string{})
			var pe *stackops.PrepareError
			if !errors.As(err, &pe) || pe.Stage != tc.stage {
				t.Fatalf("err = %v, want the %s stage", err, tc.stage)
			}
		})
	}
}
