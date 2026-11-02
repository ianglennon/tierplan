// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// renderOf checks a working copy and renders what it loaded, as the render route does.
func renderOf(t *testing.T, wc stacks.WorkingCopy, pins map[string]stacks.LivePin) string {
	t.Helper()
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "shop", Target: stacks.TargetSwarm, Copy: wc})
	if res.Loaded == nil {
		t.Fatalf("the working copy did not load: %+v", res.Problems)
	}
	out, err := stacks.Render(stacks.RenderInput{Loaded: res.Loaded, Pins: pins, Masker: stacks.NewMasker(wc.Vars, wc.Files)})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return string(out)
}

func lineWith(t *testing.T, text, want string) string {
	t.Helper()
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, want) {
			return l
		}
	}
	t.Fatalf("no line with %q in:\n%s", want, text)
	return ""
}

func TestRenderFillsInTheVariablesAndMarksAnUnpinnedImage(t *testing.T) {
	t.Parallel()
	wc := copyOf(stacks.StarterCompose)
	wc.Vars = []stacks.Var{{Name: "WEB_PORT", Value: "9090"}}
	out := renderOf(t, wc, nil)
	if l := lineWith(t, out, "9090:80"); !strings.Contains(l, `"9090:80"`) {
		t.Errorf("ports line = %q, want the variable filled in as 9090:80", l)
	}
	img := lineWith(t, out, "image:")
	if !strings.Contains(img, "nginx:1.27-alpine") || !strings.Contains(img, "# "+stacks.PinPending) {
		t.Errorf("image line = %q, want the plain reference with the trailing comment %q", img, "# "+stacks.PinPending)
	}
	if strings.Contains(out, "${") {
		t.Errorf("a variable reference is left in the rendered stack:\n%s", out)
	}
}

func TestRenderShowsTheStoredPinWhenTheImageTextIsUnchanged(t *testing.T) {
	t.Parallel()
	pins := map[string]stacks.LivePin{"shop_web": {AsWritten: "nginx:1.27-alpine", Image: "nginx:1.27-alpine@sha256:aaa"}}
	out := renderOf(t, copyOf(stacks.StarterCompose), pins)
	img := lineWith(t, out, "image:")
	if !strings.Contains(img, "nginx:1.27-alpine@sha256:aaa") || strings.Contains(img, "#") {
		t.Errorf("image line = %q, want the pinned reference and no comment", img)
	}
}

func TestRenderTreatsDockersSpellingOfTheSameImageAsUnchanged(t *testing.T) {
	t.Parallel()
	pins := map[string]stacks.LivePin{"shop_web": {AsWritten: "docker.io/library/nginx:1.27-alpine", Image: "docker.io/library/nginx:1.27-alpine@sha256:aaa"}}
	out := renderOf(t, copyOf(stacks.StarterCompose), pins)
	if img := lineWith(t, out, "image:"); !strings.Contains(img, "@sha256:aaa") {
		t.Errorf("image line = %q, want the stored pin for the same image spelled the long way", img)
	}
}

func TestRenderIgnoresAPinForAChangedImageText(t *testing.T) {
	t.Parallel()
	pins := map[string]stacks.LivePin{"shop_web": {AsWritten: "nginx:1.26-alpine", Image: "nginx:1.26-alpine@sha256:old"}}
	out := renderOf(t, copyOf(stacks.StarterCompose), pins)
	img := lineWith(t, out, "image:")
	if strings.Contains(img, "sha256:old") || !strings.Contains(img, "# "+stacks.PinPending) {
		t.Errorf("image line = %q, want the new text unpinned and marked", img)
	}
}

func TestRenderShowsADigestAsWrittenWithNoComment(t *testing.T) {
	t.Parallel()
	compose := "services:\n  web:\n    image: nginx:1.27@sha256:" + strings.Repeat("a", 64) + "\n"
	out := renderOf(t, copyOf(compose), nil)
	img := lineWith(t, out, "image:")
	if !strings.Contains(img, "sha256:"+strings.Repeat("a", 64)) || strings.Contains(img, "#") {
		t.Errorf("image line = %q, want the digest as written and no comment", img)
	}
}

func TestRenderHidesASecretVariableWhereverItLands(t *testing.T) {
	t.Parallel()
	compose := "services:\n  db:\n    image: postgres:16\n    environment:\n      PASSWORD: ${DB_PASS}\n    command: [\"--pw=${DB_PASS}\"]\n"
	wc := copyOf(compose)
	wc.Vars = []stacks.Var{{Name: "DB_PASS", Value: "s3cret-pass", Secret: true}}
	out := renderOf(t, wc, nil)
	if strings.Contains(out, "s3cret-pass") {
		t.Errorf("a secret value is in the rendered stack:\n%s", out)
	}
	if got := strings.Count(out, stacks.Hidden); got < 2 {
		t.Errorf("<hidden> appears %d times, want it for the environment value and the command:\n%s", got, out)
	}
}

func TestRenderHidesASecretYamlWouldEscape(t *testing.T) {
	t.Parallel()
	compose := "services:\n  db:\n    image: postgres:16\n    environment:\n      PASSWORD: ${DB_PASS}\n"
	wc := copyOf(compose)
	wc.Vars = []stacks.Var{{Name: "DB_PASS", Value: "pa\"ss\nword-1", Secret: true}}
	out := renderOf(t, wc, nil)
	for _, leak := range []string{"pa\"ss", `pa\"ss`, "word-1"} {
		if strings.Contains(out, leak) {
			t.Errorf("part of an escaped secret (%q) is in the rendered stack:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, stacks.Hidden) {
		t.Errorf("no <hidden> in the rendered stack:\n%s", out)
	}
}

func TestRenderLeavesThePlainValuesAlone(t *testing.T) {
	t.Parallel()
	wc := copyOf(stacks.StarterCompose)
	wc.Vars = []stacks.Var{{Name: "WEB_PORT", Value: "9090"}, {Name: "OTHER", Value: "visible-value"}}
	if out := renderOf(t, wc, nil); strings.Contains(out, stacks.Hidden) {
		t.Errorf("nothing is secret, yet the rendered stack hides something:\n%s", out)
	}
}

func TestRenderNeverTouchesTheWorkingCopy(t *testing.T) {
	t.Parallel()
	wc := copyOf(stacks.StarterCompose)
	wc.Vars = []stacks.Var{{Name: "WEB_PORT", Value: "9090"}}
	before := bytes.Clone(wc.Compose())
	_ = renderOf(t, wc, map[string]stacks.LivePin{"shop_web": {AsWritten: "nginx:1.27-alpine", Image: "nginx:1.27-alpine@sha256:aaa"}})
	if !bytes.Equal(before, wc.Compose()) {
		t.Errorf("the compose bytes changed:\n%s\nwas:\n%s", wc.Compose(), before)
	}
}

func TestRenderOfNothingIsAnError(t *testing.T) {
	t.Parallel()
	if _, err := stacks.Render(stacks.RenderInput{}); err == nil {
		t.Error("Render of no project is nil, want an error")
	}
}
