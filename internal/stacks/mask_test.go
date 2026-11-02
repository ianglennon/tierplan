// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/stacks"
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

// A secret file's content is hidden like a secret variable's value (plan 07-13, D-02, T-07-62).

func TestMaskerHidesTheContentOfASecretFile(t *testing.T) {
	t.Parallel()
	m := stacks.NewMasker(nil, []stacks.File{
		{Path: "tok.txt", Content: []byte("tok-abcdef-12345\n"), Secret: true},
		{Path: "notes.txt", Content: []byte("visible-notes")},
	})
	for name, tc := range map[string]struct{ in, want string }{
		"the content":               {"token is tok-abcdef-12345 here", "token is <hidden> here"},
		"with its trailing line":    {"tok-abcdef-12345\n", "<hidden>\n"},
		"a file that is not secret": {"visible-notes", "visible-notes"},
	} {
		if got := m.Mask(tc.in); got != tc.want {
			t.Errorf("%s: Mask(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

func TestMaskerHidesAFileTheComposeFileUsesAsASecretEvenBeforeItIsFlagged(t *testing.T) {
	t.Parallel()
	files := []stacks.File{
		{Path: stacks.ComposeFile, Content: []byte("secrets:\n  tok:\n    file: tok.txt\n")},
		{Path: "tok.txt", Content: []byte("typed-but-not-saved-yet")},
	}
	if got := stacks.NewMasker(nil, files).Mask("x typed-but-not-saved-yet y"); got != "x <hidden> y" {
		t.Errorf("Mask = %q, want the content of a file used under secrets: hidden before the first save", got)
	}
}

func TestMaskerHidesEachLineAndEachValueOfASecretFile(t *testing.T) {
	t.Parallel()
	m := stacks.NewMasker(nil, []stacks.File{
		{Path: "app.env", Content: []byte("TOKEN=\"tok-abcdef-12345\"\nDB=postgres-pass-9\nQ=1\n"), Secret: true},
	})
	for name, tc := range map[string]struct{ in, want string }{
		"a value alone":     {"TOKEN: tok-abcdef-12345", "TOKEN: <hidden>"},
		"another value":     {"DB: postgres-pass-9", "DB: <hidden>"},
		"a value too short": {"Q: 1", "Q: 1"},
	} {
		if got := m.Mask(tc.in); got != tc.want {
			t.Errorf("%s: Mask(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

func TestMaskerLeavesAFileTooShortToHideAlone(t *testing.T) {
	t.Parallel()
	m := stacks.NewMasker(nil, []stacks.File{{Path: "pin.txt", Content: []byte("123\n"), Secret: true}, {Path: "empty", Secret: true}})
	if got := m.Mask("pin 123 and ''"); got != "pin 123 and ''" {
		t.Errorf("Mask = %q: a secret under 4 characters is not masked, as for a variable", got)
	}
}

func TestACheckAndARenderNeverCarryASecretFilesContent(t *testing.T) {
	t.Parallel()
	// The secret file is also the env_file of the service, so its values land in the environment
	// of the rendered stack.
	compose := "services:\n  web:\n    image: nginx:1.27\n    env_file: app.env\nsecrets:\n  tok:\n    file: app.env\n"
	wc := stacks.WorkingCopy{Files: []stacks.File{
		{Path: stacks.ComposeFile, Content: []byte(compose)},
		{Path: "app.env", Content: []byte("TOKEN=tok-abcdef-12345\n")},
	}}
	out := renderOf(t, wc, nil)
	if want := "tok-abcdef-12345"; strings.Contains(out, want) {
		t.Errorf("the rendered stack carries the content of a secret file:\n%s", out)
	}
	if !strings.Contains(out, stacks.Hidden) {
		t.Errorf("no <hidden> in the rendered stack:\n%s", out)
	}
}

func TestMaskerHidesTheQuotedFormsOfASecret(t *testing.T) {
	t.Parallel()
	m := stacks.NewMasker([]stacks.Var{{Name: "PW", Value: escapedSecret, Secret: true}},
		[]stacks.File{{Path: "tls.key", Content: []byte("pa\"ss-file-12345\n"), Secret: true}})
	for name, tc := range map[string]struct{ in, want string }{
		"Go-quoted":     {strconv.Quote(escapedSecret), `"<hidden>"`},
		"in a %q error": {fmt.Sprintf("parsing %q: invalid syntax", escapedSecret), `parsing "<hidden>": invalid syntax`},
		"schema-quoted": {`'hunter2-alpha\\hunter2-bravo"hunter2-charlie\'hunter2-delta\nhunter2-echo\n'`, `'<hidden>'`},
		"trimmed":       {strings.TrimSpace(escapedSecret), stacks.Hidden},
		"file, quoted":  {strconv.Quote("pa\"ss-file-12345"), `"<hidden>"`},
	} {
		if got := m.Mask(tc.in); got != tc.want {
			t.Errorf("%s: Mask(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

func TestMaskerHidesEachLineOfAMultiLineSecretVariable(t *testing.T) {
	t.Parallel()
	mask := func(value string) stacks.Masker {
		return stacks.NewMasker([]stacks.Var{{Name: "KEY", Value: value, Secret: true}}, nil)
	}
	multi := "first-line-1234\nx\n\nab\nlast-line-5678\n"
	m := mask(multi)
	if got := m.Mask("saw first-line-1234 and last-line-5678"); got != "saw <hidden> and <hidden>" {
		t.Errorf("each line alone: got %q", got)
	}
	if got := m.Mask("x ab box cab"); got != "x ab box cab" {
		t.Errorf("short and empty lines must not be replacements: got %q", got)
	}
	if got := m.Mask(strconv.Quote(multi)); got != `"<hidden>"` {
		t.Errorf("the whole form must win over its lines: got %q", got)
	}
	if got := mask("first-line-1234\r\nlast-line-5678").Mask("first-line-1234"); got != stacks.Hidden {
		t.Errorf("a CRLF line: got %q, want the line hidden without its carriage return", got)
	}

	var b strings.Builder
	for i := 1; i <= 201; i++ {
		fmt.Fprintf(&b, "cap-line-%04d\n", i)
	}
	over := b.String()
	m = mask(over)
	if got := m.Mask("cap-line-0001"); got != stacks.Hidden {
		t.Errorf("a line within the cap: got %q", got)
	}
	if got := m.Mask("cap-line-0201"); got != "cap-line-0201" {
		t.Errorf("a line past the line cap is only hidden with the whole value: got %q", got)
	}
	if got := m.Mask(strconv.Quote(over)); got != `"<hidden>"` {
		t.Errorf("above the line cap the whole value is still hidden: got %q", got)
	}
}

func TestMaskerDoesNotSplitASecretVariableAtEquals(t *testing.T) {
	t.Parallel()
	m := stacks.NewMasker([]stacks.Var{{Name: "MODE", Value: "mode=readonly", Secret: true}}, nil)
	if got := m.Mask("set mode=readonly now"); got != "set <hidden> now" {
		t.Errorf("the whole value: got %q", got)
	}
	if got := m.Mask("the volume is readonly"); got != "the volume is readonly" {
		t.Errorf("the text after '=' must not be a replacement: got %q", got)
	}
}
