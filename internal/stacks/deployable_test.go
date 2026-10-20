// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/stackstest"
)

func envFileCopy() stacks.WorkingCopy {
	c := copyOf("services:\n  web:\n    image: nginx:1.27-alpine\n    env_file: app.env\n")
	c.Files = append(c.Files, stacks.File{Path: "app.env", Content: []byte("FROM_FILE=1\n")})
	return c
}

func TestDeployableIsTheProjectCheckJudged(t *testing.T) {
	t.Parallel()
	in := stacks.CheckInput{Name: "probe", Target: stacks.TargetSwarm, Copy: envFileCopy()}
	checked := stacks.Check(context.Background(), in)
	if checked.Loaded == nil {
		t.Fatalf("Check refused the working copy: %+v", checked.Problems)
	}
	got, err := stacks.Deployable(context.Background(), in)
	if err != nil {
		t.Fatalf("Deployable: %v", err)
	}
	want, _ := json.Marshal(checked.Loaded.Project)
	have, _ := json.Marshal(got.Project)
	if string(want) != string(have) {
		t.Errorf("Deployable's project differs from Check's:\n got %s\nwant %s", have, want)
	}
	svc := got.Project.Services["web"]
	if len(svc.EnvFiles) != 0 || svc.Environment["FROM_FILE"] == nil || *svc.Environment["FROM_FILE"] != "1" {
		t.Errorf("service = env files %+v, environment %+v, want the file resolved into the environment", svc.EnvFiles, svc.Environment)
	}
}

func TestDeployableStripsBuildBesideAnImage(t *testing.T) {
	t.Parallel()
	got, err := stacks.Deployable(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetStandalone,
		Copy: copyOf("services:\n  web:\n    image: nginx:1.27-alpine\n    build: .\n"),
	})
	if err != nil {
		t.Fatalf("Deployable: %v", err)
	}
	if got.Project.Services["web"].Build != nil {
		t.Error("build was kept, want it stripped (a warning, not a refusal)")
	}
}

func TestDeployableRefusesWhatTheCheckRefuses(t *testing.T) {
	t.Parallel()
	got, err := stacks.Deployable(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetSwarm,
		Copy: copyOf("services:\n  web:\n    image: nginx:${TAG}\n"),
	})
	var nd *stacks.NotDeployableError
	if got != nil || !errors.Is(err, stacks.ErrNotDeployable) || !errors.As(err, &nd) {
		t.Fatalf("got %v, %v, want a *NotDeployableError", got, err)
	}
	found := false
	for _, p := range nd.Problems {
		found = found || p.Code == stacks.CodeUnsetVariable
	}
	if !found {
		t.Errorf("problems = %+v, want an unset_variable", nd.Problems)
	}
}

func TestDeployableRefusalQuotesNoSecret(t *testing.T) {
	t.Parallel()
	const secret = "hunter2secretvalue"
	_, err := stacks.Deployable(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetSwarm,
		Copy: copyOf("services:\n  web:\n    image: nginx:1.27-alpine\n    deploy:\n      replicas: ${PW}\n",
			stacks.Var{Name: "PW", Value: secret, Secret: true}),
	})
	var nd *stacks.NotDeployableError
	if !errors.As(err, &nd) {
		t.Fatalf("err = %v, want a *NotDeployableError", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error text %q quotes the secret", err.Error())
	}
	for _, p := range nd.Problems {
		if strings.Contains(p.Message, secret) || strings.Contains(p.Key, secret) {
			t.Errorf("problem %+v quotes the secret", p)
		}
	}
}

func TestDeployableRefusalQuotesNoEscapedSecret(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	for name, tc := range map[string]struct{ body, path string }{
		"deploy.replicas": {"    deploy:\n      replicas: ${PW}\n", "services.web.deploy.replicas"},
		"pull_policy":     {"    pull_policy: ${PW}\n", "services.web.pull_policy"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := stacks.Deployable(context.Background(), stacks.CheckInput{
				Name: "probe", Target: stacks.TargetSwarm,
				Copy: copyOf(head+tc.body, stacks.Var{Name: "PW", Value: escapedSecret, Secret: true}),
			})
			var nd *stacks.NotDeployableError
			if !errors.As(err, &nd) {
				t.Fatalf("err = %v, want a *NotDeployableError", err)
			}
			stackstest.NoSecretRun(t, escapedSecret, append(messagesOf(nd.Problems), err.Error())...)
			fixed := false
			for _, part := range escapedSecretParts {
				if strings.Contains(err.Error(), part) {
					t.Errorf("error text %q quotes %q", err.Error(), part)
				}
				for _, p := range nd.Problems {
					if strings.Contains(p.Message, part) || strings.Contains(p.Key, part) {
						t.Errorf("problem %+v quotes %q", p, part)
					}
				}
			}
			for _, p := range nd.Problems {
				fixed = fixed || p.Message == stacks.SecretValueMessage(tc.path)
			}
			if !fixed {
				t.Errorf("problems = %+v, want the fixed sentence at %s", nd.Problems, tc.path)
			}
		})
	}
}

func TestDeployableRefusalNeverQuotesPartOfASecret(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	for name, tc := range map[string]struct{ body, path string }{
		"mem_limit":       {"    mem_limit: ${PW}\n", "services.web.mem_limit"},
		"deploy.replicas": {"    deploy:\n      replicas: ${PW}\n", "services.web.deploy.replicas"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := stacks.Deployable(context.Background(), stacks.CheckInput{
				Name: "probe", Target: stacks.TargetSwarm,
				Copy: copyOf(head+tc.body, stacks.Var{Name: "PW", Value: "Password2024!", Secret: true}),
			})
			var nd *stacks.NotDeployableError
			if !errors.As(err, &nd) {
				t.Fatalf("err = %v, want a *NotDeployableError", err)
			}
			if strings.Contains(err.Error(), "Password2024") {
				t.Errorf("error text %q quotes part of the secret", err.Error())
			}
			stackstest.NoSecretRun(t, "Password2024!", append(messagesOf(nd.Problems), err.Error())...)
			fixed := false
			for _, p := range nd.Problems {
				if strings.Contains(p.Message, "Password2024") || strings.Contains(p.Key, "Password2024") {
					t.Errorf("problem %+v quotes part of the secret", p)
				}
				fixed = fixed || p.Message == stacks.SecretValueMessage(tc.path)
			}
			if !fixed {
				t.Errorf("problems = %+v, want the fixed sentence at %s", nd.Problems, tc.path)
			}
		})
	}
}
