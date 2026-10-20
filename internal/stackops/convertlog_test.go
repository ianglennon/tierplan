// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// The text a job logs for a conversion it could not make is the converter's words for a plain value and the
// fixed sentence for a field fed by a secret variable (07-42, D-02, T-07-181).
func TestTheConvertLogTextNeverQuotesPartOfASecret(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	cases := map[string]struct {
		compose string
		secret  string
		// want is the text exactly; contains is a phrase the text holds when want is empty.
		want, contains string
		absent         string
	}{
		"restart":                   {compose: head + "    restart: ${PW}\n", secret: "sometimes:3", want: stacks.SecretValueMessage("services.web.restart"), absent: "sometimes"},
		"ports":                     {compose: head + "    ports:\n      - \"${PW}\"\n", secret: "127.0.0.1:8000-8001:80", absent: "8000-8001"},
		"a plain value keeps words": {compose: head + "    restart: sometimes\n    environment:\n      TOKEN: ${PW}\n", secret: "Password2024!", contains: "unknown policy"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := stacks.WorkingCopy{Files: []stacks.File{compose(tc.compose)}, Vars: []stacks.Var{{Name: "PW", Value: tc.secret, Secret: true}}}
			loaded, err := stacks.Deployable(context.Background(), stacks.CheckInput{Name: "web", Target: stacks.TargetSwarm, Copy: c})
			if err != nil {
				t.Fatalf("Deployable: %v", err)
			}
			_, err = convert.Convert(loaded.Project, convert.Options{StackName: "web", StackID: "stack-1"})
			if err == nil {
				t.Fatal("Convert: want a refusal (the premise of the test)")
			}
			text := stackops.ConvertLogText(err, c)
			if tc.want != "" && text != tc.want {
				t.Errorf("text = %q, want %q", text, tc.want)
			}
			if tc.absent != "" && strings.Contains(text, tc.absent) {
				t.Errorf("text = %q, want no %q", text, tc.absent)
			}
			if tc.contains != "" && !strings.Contains(text, tc.contains) {
				t.Errorf("text = %q, want it to contain %q", text, tc.contains)
			}
			if strings.Contains(text, tc.secret) {
				t.Errorf("text = %q, want no secret", text)
			}
		})
	}
	t.Run("a nil error is empty", func(t *testing.T) {
		t.Parallel()
		if got := stackops.ConvertLogText(nil, stacks.WorkingCopy{}); got != "" {
			t.Errorf("text = %q, want empty", got)
		}
	})
}
