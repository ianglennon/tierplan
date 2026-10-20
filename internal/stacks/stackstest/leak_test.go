// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackstest_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/stackstest"
)

// recorder is a testing.TB that notes what NoSecretRun reports and never stops the test.
type recorder struct {
	testing.TB
	errors []string
	fatals []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func TestNoSecretRunReportsEveryRunOfTheSecretAndNothingElse(t *testing.T) {
	t.Parallel()
	const secret = "Zq9xkwmpr"
	const wordy = "2024secretword"
	cases := map[string]struct {
		secret string
		texts  []string
		// reports is how many errors are wanted.
		reports int
		// names are the windows each report must name, in any order.
		names []string
		fatal bool
	}{
		"a window of the secret in a loader's words": {
			secret: secret, texts: []string{"invalid size: 'xkwm'"}, reports: 1, names: []string{"xkwm"},
		},
		"the fixed sentence for a path holds no run, though it says secret": {
			secret: wordy, texts: []string{stacks.SecretValueMessage("services.web.mem_limit")},
		},
		"the file's sentence for a path holds no run": {
			secret: wordy, texts: []string{stacks.SecretFileMessage("services.web.ports.[].target")},
		},
		"the general sentence holds no run": {
			secret: wordy, texts: []string{stacks.SecretValueMessage("")},
		},
		"a secret in the key path before the value sentence is reported": {
			secret: secret, texts: []string{stacks.SecretValueMessage(secret)}, reports: 6, names: []string{"Zq9x"},
		},
		"a secret in the key path before the file sentence is reported": {
			secret: secret, texts: []string{stacks.SecretFileMessage(secret)}, reports: 6, names: []string{"Zq9x"},
		},
		"a removal never joins its neighbours into a run": {
			secret: secret, texts: []string{"xk" + stacks.SecretValueMessage("") + "wm"},
		},
		"a run shorter than the minimum is not a run": {
			secret: secret, texts: []string{"Zq9"},
		},
		"two different runs give two reports": {
			secret: secret, texts: []string{"kwmp ... Zq9x"}, reports: 2, names: []string{"kwmp", "Zq9x"},
		},
		"a secret shorter than the minimum is a fatal": {
			secret: "abc", texts: []string{"abc"}, fatal: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &recorder{TB: t}
			stackstest.NoSecretRun(r, tc.secret, tc.texts...)
			if tc.fatal {
				if len(r.fatals) != 1 || len(r.errors) != 0 {
					t.Errorf("fatals = %v, errors = %v, want one fatal and no error", r.fatals, r.errors)
				}
				return
			}
			if len(r.fatals) != 0 {
				t.Errorf("fatals = %v, want none", r.fatals)
			}
			if len(r.errors) != tc.reports {
				t.Errorf("reports = %d %v, want %d", len(r.errors), r.errors, tc.reports)
			}
			for _, n := range tc.names {
				found := false
				for _, e := range r.errors {
					if strings.Contains(e, n) {
						found = true
					}
				}
				if !found {
					t.Errorf("reports = %v, want one naming %q", r.errors, n)
				}
			}
		})
	}
}
