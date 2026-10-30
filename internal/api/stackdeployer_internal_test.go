// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// envStub is an httpx.EnvResolver that answers one error.
type envStub struct{ err error }

func (e envStub) Resolve(context.Context, string) (httpx.Env, error) { return httpx.Env{}, e.err }

// What an automatic deploy holds back on is stored as the reason of the stack's sync error and shown on the stack and
// the bell, where nobody is editing a draft and nobody can be asked at once (plan 09-34, G-09-8). So every refusal an
// automatic deploy holds back on has a sentence written for an automatic deploy. Each row below builds the error from
// the constructor the deploy path calls and passes it through the one reader that serves only automatic deploys. A
// refusal that is retried (a held lock, a full job slot) and draft_changed are not held back (autodeploy.go) and have no
// row.
func TestAutomaticRefusalsAreWordedForAutomation(t *testing.T) {
	t.Parallel()
	d := StacksDeps{Log: slog.New(slog.DiscardHandler)}
	s := NewStackDeployer(d)
	ctx := context.Background()
	var masker stacks.Masker

	// The person's sentence of the concurrent gate: refusal replaces it, so it is the input (the gate itself needs a
	// store; the end-to-end sentence is checked through the deployer in TestStackDeployerRefusals).
	concurrent := &huma.ErrorModel{
		Type: problemConcurrentUnacknowledged, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: "ada deployed this stack after you started editing. Review what changed and confirm that you want to deploy your draft over it.",
	}
	bind := stackops.PolicyResult{Refused: true, Entries: []stackops.PolicyEntry{{Text: "a bind mount of /etc", File: "compose.yaml", Line: 5}}}
	_, envNotFound := s.environment(ctx, "env-1", "stack-1")

	rows := []struct {
		name string
		code string
		err  error
	}{
		{"forbidden: no principal", "forbidden", func() error {
			_, err := s.DeployDraft(ctx, nil, "env-1", "stack-1", 1, "poll")
			return err
		}()},
		{"environment_not_found: no environments", "environment_not_found", envNotFound},
		{"environment_not_found: gone", "environment_not_found", func() error {
			_, err := NewStackDeployer(StacksDeps{Log: d.Log, Envs: envStub{httpx.ErrEnvNotFound}}).environment(ctx, "env-1", "stack-1")
			return err
		}()},
		{"environment_disabled", "environment_disabled", func() error {
			_, err := NewStackDeployer(StacksDeps{Log: d.Log, Envs: envStub{httpx.ErrEnvDisabled}}).environment(ctx, "env-1", "stack-1")
			return err
		}()},
		{"policy_refused", "policy_refused", refuse(refusalPolicy, policyProblem(bind))},
		{"policy_refused: cannot be checked", "policy_refused", refuse(refusalPolicy, policyProblem(stackops.PolicyResult{Refused: true}))},
		{"not_deployable", "not_deployable", invalidStackProblem(stacks.Problems{{Severity: stacks.SeverityError, File: "compose.yaml", Line: 2, Message: "image is not valid"}})},
		{"name_clash", "name_clash", nameClashProblem("taken")},
		{"objects_invalid", "objects_invalid", refuse(refusalObjects, objectProblem(&stacks.ObjectError{Kind: stacks.KindSecret, Name: "token", Err: stacks.ErrObjectEmpty}, masker))},
		{"convert_failed", "convert_failed", refuse(refusalConvert, convertProblem(&convert.NotConvertedError{Service: "web", Fields: []string{"build"}}, masker))},
		{"pins_invalid: rollback", "pins_invalid", rollbackPrepareProblem(&stackops.PrepareError{Stage: stackops.PreparePins, Err: &stackops.PinsInvalidError{Service: "web"}}, masker)},
		{"pins_invalid: reviewed pins", "pins_invalid", pinsProblem(d, "env-1", &stackops.PinsInvalidError{Service: "web"})},
		{"target_changed", "target_changed", targetProblem(d, "env-1", stackops.ErrTargetChanged)},
		{"concurrent_unacknowledged", "concurrent_unacknowledged", concurrent},
		{"stack_not_found", "stack_not_found", stackNotFound()},
		{"services_invalid: none named", "services_invalid", servicesProblem(&stackops.ServicesInvalidError{Empty: true})},
		{"services_invalid: unknown", "services_invalid", servicesProblem(&stackops.ServicesInvalidError{Unknown: []string{"ghost"}})},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var ref *DeployRefusal
			if !errors.As(passed(s, row.err), &ref) {
				t.Fatalf("refusal of %v is not a *DeployRefusal", row.err)
			}
			if ref.Code != row.code || ref.Retry {
				t.Errorf("refusal = %+v, want %s without retry", ref, row.code)
			}
			if strings.TrimSpace(ref.Detail) == "" {
				t.Errorf("refusal %s has no sentence", row.code)
			}
			for _, phrase := range []string{"you started editing", "your draft", "ask an administrator"} {
				if strings.Contains(strings.ToLower(ref.Detail), phrase) {
					t.Errorf("refusal %s says %q, a sentence for a person at the screen: %q", row.code, phrase, ref.Detail)
				}
			}
		})
	}

	// The wording is the one the stack's sync error shows: what stops it, in one sentence.
	var ref *DeployRefusal
	if !errors.As(passed(s, refuse(refusalPolicy, policyProblem(bind))), &ref) || ref.Detail != "This needs Admin: a bind mount of /etc at compose.yaml:5." {
		t.Errorf("policy refusal = %+v, want the Admin sentence with the setting and its place", ref)
	}
	two := stackops.PolicyResult{Refused: true, Entries: []stackops.PolicyEntry{
		{Text: "a bind mount of /etc", File: "compose.yaml", Line: 5}, {Text: "privileged mode", File: "compose.extra.yaml", Line: 9},
	}}
	if !errors.As(passed(s, refuse(refusalPolicy, policyProblem(two))), &ref) ||
		ref.Detail != "This needs Admin: a bind mount of /etc at compose.yaml:5; privileged mode at compose.extra.yaml:9." {
		t.Errorf("policy refusal of two settings = %+v, want both, joined", ref)
	}
	if !errors.As(passed(s, refuse(refusalPolicy, policyProblem(stackops.PolicyResult{Refused: true}))), &ref) ||
		ref.Detail != "The compose file can't be checked for host access, so it can't be deployed." {
		t.Errorf("policy refusal with no setting = %+v, want its own sentence", ref)
	}
	if !errors.As(passed(s, concurrent), &ref) || ref.Detail != concurrentAutomationDetail {
		t.Errorf("concurrent refusal = %+v, want the automatic deploy's sentence", ref)
	}
}

// passed is the refusal of err as the deployer answers it, or err itself when the deployer's own code made it.
func passed(s *StackDeployer, err error) error {
	var ref *DeployRefusal
	if errors.As(err, &ref) {
		return err
	}
	return s.refusal("stack-1", err)
}
