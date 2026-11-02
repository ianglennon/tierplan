// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/stackstest"
)

// The roll back route and the start route both answer a refused revision through rollbackPrepareProblem with
// the revision's masker, so a call with the masker of the PrepareError covers both answers. For a field fed
// by a secret variable the answer holds the fixed sentence and no run of the value (07-44, D-02, T-07-187).
func TestTheRollbackAndStartRefusalsNeverHoldARunOfASecret(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	cases := map[string]struct {
		compose, secret string
		// detail is the Detail the answer must equal; empty means the load stage, answered per field.
		detail string
		// field is the Message an entry of Errors must equal when detail is empty.
		field string
	}{
		"restart, refused by the converter": {head + "    restart: ${PW}\n", "sometimes:3", stacks.SecretValueMessage("services.web.restart"), ""},
		"ports, refused by the converter":   {head + "    ports: [\"${PW}\"]\n", "127.0.0.1:8000-8001:80", stacks.SecretValueMessage("services.web.ports"), ""},
		"mem_limit, refused by the loader":  {head + "    mem_limit: ${PW}\n", "Password2024!", "", stacks.SecretValueMessage("services.web.mem_limit")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			target := stackops.Snapshot{
				Seq: 2, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood,
				Copy: stacks.WorkingCopy{
					Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(tc.compose)}},
					Vars:  []stacks.Var{{Name: "PW", Value: tc.secret, Secret: true}},
				},
			}
			stack := stackops.Stack{ID: "stack-1", Name: "web", Target: stacks.TargetSwarm}
			_, err := stackops.Deps{}.PrepareRollback(context.Background(), stack, target, map[string]string{})
			var pe *stackops.PrepareError
			if !errors.As(err, &pe) {
				t.Fatalf("PrepareRollback: %v, want a *stackops.PrepareError", err)
			}
			var em *huma.ErrorModel
			if !errors.As(rollbackPrepareProblem(err, pe.Masker), &em) {
				t.Fatalf("rollbackPrepareProblem(%v) is not a *huma.ErrorModel", err)
			}
			texts := []string{em.Detail}
			found := false
			for _, d := range em.Errors {
				texts = append(texts, d.Message)
				found = found || d.Message == tc.field
			}
			if tc.detail != "" {
				if em.Detail != tc.detail {
					t.Errorf("detail = %q, want %q", em.Detail, tc.detail)
				}
			} else if em.Type != problemStackInvalid || !found {
				t.Errorf("answer = %+v, want %s with an error %q", em, problemStackInvalid, tc.field)
			}
			stackstest.NoSecretRun(t, tc.secret, texts...)
		})
	}
}
