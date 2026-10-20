// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

func rollbackTarget(composeText string, extra ...stacks.File) stackops.Snapshot {
	files := append([]stacks.File{compose(composeText)}, extra...)
	return stackops.Snapshot{Seq: 2, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood, Copy: stacks.WorkingCopy{Files: files}}
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

func TestPrepareRollbackResolvesAnEnvFileAsTheReviewDoes(t *testing.T) {
	t.Parallel()
	pinned := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	plan, err := stackops.Deps{}.PrepareRollback(context.Background(), rollbackStack,
		rollbackTarget("services:\n  web:\n    image: nginx:1.27-alpine\n    env_file: app.env\n",
			stacks.File{Path: "app.env", Content: []byte("FROM_FILE=1\n")}),
		map[string]string{"web_web": pinned})
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	for name, svc := range plan.Loaded.Project.Services {
		if len(svc.EnvFiles) != 0 {
			t.Errorf("service %s still has env files %+v, want them resolved", name, svc.EnvFiles)
		}
	}
	if len(plan.Plan.Services) != 1 || plan.Plan.Services[0].Name != "web_web" {
		t.Fatalf("services = %+v", plan.Plan.Services)
	}
	if env := plan.Plan.Services[0].Spec.TaskTemplate.ContainerSpec.Env; !slices.Contains(env, "FROM_FILE=1") {
		t.Errorf("env = %v, want FROM_FILE=1 from the env file", env)
	}
}

func TestPrepareRollbackNamesTheStageThatRefusedARevision(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ compose, stage string }{
		"a file that does not load":                {"services: [", stackops.PrepareLoad},
		"a service with no image":                  {"services:\n  web:\n    build: .\n", stackops.PrepareLoad},
		"a secret with no usable file":             {"services:\n  web:\n    image: x\n    secrets: [s]\nsecrets:\n  s:\n    file: missing.txt\n", stackops.PrepareLoad},
		"a secret from a variable the table lacks": {"services:\n  web:\n    image: x\n    secrets: [s]\nsecrets:\n  s:\n    environment: TOK\n", stackops.PrepareObjects},
		"a service the converter refuses":          {"services:\n  web:\n    image: x\n    dns: [not-an-ip]\n", stackops.PrepareConvert},
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
