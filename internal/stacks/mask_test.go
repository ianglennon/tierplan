// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
)

func TestMaskerReplacesEverySecretValueAndNothingElse(t *testing.T) {
	t.Parallel()
	m := stacks.NewMasker([]stacks.Var{
		{Name: "DB_PASS", Value: "s3cret-pass", Secret: true},
		{Name: "TOKEN", Value: "tok-12345", Secret: true},
		{Name: "PLAIN", Value: "visible-value"},
	}, nil)
	for name, tc := range map[string]struct{ in, want string }{
		"one value":              {"password is s3cret-pass here", "password is <hidden> here"},
		"every occurrence":       {"s3cret-pass and s3cret-pass", "<hidden> and <hidden>"},
		"two secrets":            {"s3cret-pass/tok-12345", "<hidden>/<hidden>"},
		"a plain value stays":    {"visible-value", "visible-value"},
		"nothing to hide":        {"nothing here", "nothing here"},
		"the empty string":       {"", ""},
		"a part of a secret":     {"s3cret", "s3cret"},
		"inside a longer string": {"postgres://u:s3cret-pass@db/x", "postgres://u:<hidden>@db/x"},
	} {
		if got := m.Mask(tc.in); got != tc.want {
			t.Errorf("%s: Mask(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

func TestMaskerHidesTheLongerSecretWhenOneContainsAnother(t *testing.T) {
	t.Parallel()
	m := stacks.NewMasker([]stacks.Var{
		{Name: "SHORT", Value: "abcd", Secret: true},
		{Name: "LONG", Value: "abcdefgh", Secret: true},
	}, nil)
	if got := m.Mask("x abcdefgh y"); got != "x <hidden> y" {
		t.Errorf("Mask = %q, want the whole longer secret hidden and no tail of it left", got)
	}
}

func TestMaskerLeavesAValueShorterThanTheMinimumAlone(t *testing.T) {
	t.Parallel()
	if stacks.MinSecretLen != 4 {
		t.Fatalf("MinSecretLen = %d, want 4", stacks.MinSecretLen)
	}
	m := stacks.NewMasker([]stacks.Var{{Name: "PIN", Value: "123", Secret: true}, {Name: "EMPTY", Value: "", Secret: true}}, nil)
	if got := m.Mask("pin 123 and ''"); got != "pin 123 and ''" {
		t.Errorf("Mask = %q: a value under 4 characters is not masked, so it cannot hide common substrings", got)
	}
}

func TestMaskerHasNothingToHideWhenNoVariableIsSecret(t *testing.T) {
	t.Parallel()
	for name, m := range map[string]stacks.Masker{
		"the zero value": {},
		"no variables":   stacks.NewMasker(nil, nil),
		"plain ones":     stacks.NewMasker([]stacks.Var{{Name: "A", Value: "visible-value"}}, nil),
	} {
		if got := m.Mask("visible-value"); got != "visible-value" {
			t.Errorf("%s: Mask = %q", name, got)
		}
	}
}

func TestMaskValueHidesWhatIsKnownToBeSecret(t *testing.T) {
	t.Parallel()
	var m stacks.Masker
	if got := m.MaskValue("anything"); got != stacks.Hidden {
		t.Errorf("MaskValue(anything) = %q, want %q", got, stacks.Hidden)
	}
	if got := m.MaskValue(""); got != "" {
		t.Errorf("MaskValue of an empty value = %q, want it empty: nothing is there to hide", got)
	}
}
