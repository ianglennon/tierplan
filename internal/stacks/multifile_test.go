// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// twoFiles is a working copy of the compose files base.yaml and override.yaml, in that merge order.
func twoFiles(base, override string, order ...string) stacks.WorkingCopy {
	if len(order) == 0 {
		order = []string{"base.yaml", "override.yaml"}
	}
	return stacks.WorkingCopy{
		Files: []stacks.File{
			{Path: "base.yaml", Content: []byte(base)},
			{Path: "override.yaml", Content: []byte(override)},
		},
		ComposeFiles: order,
	}
}

const (
	mfBase = `services:
  web:
    image: nginx:1.26-alpine
    deploy:
      replicas: 1
`
	mfOverride = `services:
  web:
    image: nginx:1.27-alpine
    deploy:
      replicas: 3
`
)

func TestMultiFileMergeOrder(t *testing.T) {
	t.Parallel()
	load := func(c stacks.WorkingCopy) (string, uint64) {
		t.Helper()
		loaded, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "m", Target: stacks.TargetSwarm, Copy: c})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		svc := loaded.Project.Services["web"]
		var replicas uint64
		if svc.Deploy != nil && svc.Deploy.Replicas != nil {
			replicas = uint64(*svc.Deploy.Replicas) //nolint:gosec // a small test value
		}
		return svc.Image, replicas
	}
	image, replicas := load(twoFiles(mfBase, mfOverride))
	if image != "nginx:1.27-alpine" || replicas != 3 {
		t.Errorf("base then override = %s x%d, want the override's nginx:1.27-alpine x3", image, replicas)
	}
	image, replicas = load(twoFiles(mfBase, mfOverride, "override.yaml", "base.yaml"))
	if image != "nginx:1.26-alpine" || replicas != 1 {
		t.Errorf("override then base = %s x%d, want the base's nginx:1.26-alpine x1: reversing the order reverses the winner", image, replicas)
	}
}

func TestMultiFileCheckPasses(t *testing.T) {
	t.Parallel()
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "m", Target: stacks.TargetSwarm, Copy: twoFiles(mfBase, mfOverride)})
	if res.Problems.Errors() != 0 {
		t.Fatalf("Check problems = %+v, want none that block", res.Problems)
	}
	if res.Loaded == nil {
		t.Fatal("Check withheld the project of a valid two-file copy")
	}
	for _, f := range res.UnusedFiles {
		t.Errorf("%s reported unused: a compose file is never unused", f)
	}
}

func TestMultiFileIncludeStillBlocked(t *testing.T) {
	t.Parallel()
	const withInclude = "include:\n  - other.yaml\nservices:\n  web:\n    image: nginx:1.27-alpine\n"
	const withExtends = "services:\n  web:\n    image: nginx:1.27-alpine\n  api:\n    image: busybox:1.37\n    extends:\n      service: web\n"
	const plain = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	cases := []struct {
		name, override, code, git, inline string
	}{
		{"include", withInclude, stacks.CodeIncludeUnsupported,
			"include isn't supported. Add the file to the stack's compose files instead.",
			"include isn't supported. Put everything in compose.yaml."},
		{"extends", withExtends, stacks.CodeExtendsUnsupported,
			"extends isn't supported. Add the file to the stack's compose files instead.",
			"extends isn't supported. Copy the settings you need into the service."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := stacks.Check(context.Background(), stacks.CheckInput{Name: "m", Target: stacks.TargetSwarm, Copy: twoFiles(plain, tc.override)})
			var got *stacks.Problem
			for i := range res.Problems {
				if res.Problems[i].Code == tc.code {
					got = &res.Problems[i]
				}
			}
			if got == nil {
				t.Fatalf("no %s problem in %+v", tc.code, res.Problems)
			}
			if got.Message != tc.git || got.File != "override.yaml" || got.Line < 1 {
				t.Errorf("problem = %+v, want the git wording %q in override.yaml at its real line", *got, tc.git)
			}
			if res.Loaded != nil {
				t.Error("a copy with a refused directive was loaded")
			}
			// An inline copy keeps the Phase 7 wording.
			inline := stacks.Check(context.Background(), stacks.CheckInput{Name: "m", Target: stacks.TargetSwarm, Copy: copyOf(tc.override)})
			var text string
			for _, p := range inline.Problems {
				if p.Code == tc.code {
					text = p.Message
				}
			}
			if text != tc.inline {
				t.Errorf("inline message = %q, want %q", text, tc.inline)
			}
		})
	}
}

func TestMultiFileComposeMissing(t *testing.T) {
	t.Parallel()
	c := twoFiles(mfBase, mfOverride, "base.yaml", "gone.yaml")

	err := stacks.CheckFilesFor(c.Files, c.ComposeFiles)
	var fe *stacks.FileError
	if !errors.Is(err, stacks.ErrComposeMissing) || !errors.As(err, &fe) || fe.Path != "gone.yaml" {
		t.Errorf("CheckFilesFor = %v, want ErrComposeMissing naming gone.yaml", err)
	}
	if err := stacks.CheckFilesFor(c.Files, []string{"base.yaml", "base.yaml"}); !errors.Is(err, stacks.ErrPathTaken) {
		t.Errorf("a compose file listed twice = %v, want ErrPathTaken", err)
	}
	if err := stacks.CheckFilesFor(c.Files, []string{"base.yaml", "../x.yaml"}); !errors.Is(err, stacks.ErrPathInvalid) {
		t.Errorf("an invalid compose path = %v, want ErrPathInvalid", err)
	}
	if err := stacks.CheckFilesFor(c.Files, []string{"base.yaml", "override.yaml"}); err != nil {
		t.Errorf("a valid list = %v, want nil", err)
	}

	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "m", Target: stacks.TargetSwarm, Copy: c})
	var found bool
	for _, p := range res.Problems {
		if p.Code == stacks.CodeFileMissing && p.File == "gone.yaml" && p.Severity == stacks.SeverityError {
			found = true
		}
	}
	if !found || res.Loaded != nil {
		t.Errorf("Check = %+v loaded=%v, want a blocking file_missing problem naming gone.yaml", res.Problems, res.Loaded != nil)
	}
}

func TestMultiFileTooLargeNamesTheFile(t *testing.T) {
	t.Parallel()
	c := twoFiles(mfBase, "services:\n  web:\n    image: x\n#"+strings.Repeat("a", stacks.MaxFileBytes))
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "m", Target: stacks.TargetSwarm, Copy: c})
	if len(res.Problems) != 1 || res.Problems[0].Code != stacks.CodeComposeTooLarge || res.Problems[0].File != "override.yaml" {
		t.Fatalf("problems = %+v, want one compose_too_large in override.yaml", res.Problems)
	}
	if _, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "m", Target: stacks.TargetSwarm, Copy: c}); !errors.Is(err, stacks.ErrTooLarge) {
		t.Errorf("Load = %v, want ErrTooLarge", err)
	}
}

func TestMultiFilePortsAddUpAcrossFiles(t *testing.T) {
	t.Parallel()
	file := func(lo int) string {
		return "services:\n  web:\n    image: x\n    ports:\n      - \"" + itoa(lo) + "-" + itoa(lo+stacks.MaxPorts/2) + ":" + itoa(lo) + "-" + itoa(lo+stacks.MaxPorts/2) + "\"\n"
	}
	c := twoFiles(file(1000), file(20000))
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "m", Target: stacks.TargetSwarm, Copy: c})
	var got *stacks.Problem
	for i := range res.Problems {
		if res.Problems[i].Code == stacks.CodePortsTooMany {
			got = &res.Problems[i]
		}
	}
	if got == nil || got.File != "override.yaml" {
		t.Fatalf("problems = %+v, want ports_too_many in override.yaml: the ports of all the files add up", res.Problems)
	}
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{digits[n%10]}, b...)
	}
	return string(b)
}

func TestMultiFileProblemsNameTheirFile(t *testing.T) {
	t.Parallel()
	// A schema error and a YAML error each belong to the file that holds them.
	c := twoFiles(mfBase, "services:\n  web:\n    image: x\n    replicas: nope\n")
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "m", Target: stacks.TargetSwarm, Copy: c})
	if res.Problems.Errors() == 0 {
		t.Fatalf("no error for an unknown key: %+v", res.Problems)
	}
	for _, p := range res.Problems {
		if p.Severity == stacks.SeverityError && p.File != "override.yaml" {
			t.Errorf("problem %+v is in %s, want override.yaml", p, p.File)
		}
	}
	bad := twoFiles(mfBase, "services:\n  web: [\n")
	res = stacks.Check(context.Background(), stacks.CheckInput{Name: "m", Target: stacks.TargetSwarm, Copy: bad})
	if len(res.Problems) == 0 || res.Problems[0].Code != stacks.CodeYAMLInvalid || res.Problems[0].File != "override.yaml" {
		t.Errorf("problems = %+v, want yaml_invalid in override.yaml", res.Problems)
	}
}

func TestInlineCopyUnchanged(t *testing.T) {
	t.Parallel()
	c := copyOf("services:\n  web:\n    image: nginx:1.27-alpine\n")
	if got := c.ComposePaths(); !reflect.DeepEqual(got, []string{stacks.ComposeFile}) {
		t.Errorf("ComposePaths = %v, want [compose.yaml]", got)
	}
	contents := c.ComposeContents()
	if len(contents) != 1 || contents[0].Path != stacks.ComposeFile || string(contents[0].Content) != string(c.Compose()) {
		t.Errorf("ComposeContents = %+v, want the one compose.yaml", contents)
	}
	// A copy with no compose file still has a non-nil, empty content: compose-go reads a file from
	// disk when a config file has nil content.
	empty := stacks.WorkingCopy{}
	if got := empty.ComposeContents(); len(got) != 1 || got[0].Content == nil {
		t.Errorf("ComposeContents of an empty copy = %+v, want one entry with a non-nil content", got)
	}
	// ComposePaths hands out a copy: changing it does not change the working copy.
	two := twoFiles(mfBase, mfOverride)
	paths := two.ComposePaths()
	slices.Reverse(paths)
	if !slices.Equal(two.ComposeFiles, []string{"base.yaml", "override.yaml"}) {
		t.Errorf("ComposeFiles = %v after reversing a returned slice, want it unchanged", two.ComposeFiles)
	}
	// Compose is the first compose file.
	if string(two.Compose()) != mfBase {
		t.Error("Compose() is not the first compose file")
	}
}

func TestMultiFileRefs(t *testing.T) {
	t.Parallel()
	const a = `services:
  web:
    image: nginx:${TAG}
    environment:
      SHARED: ${SHARED:-x}
      BARE_A:
`
	const b = `services:
  web:
    ports:
      - "${PORT:-8080}:80"
    environment:
      - SHARED
      - ${ONLY_B}
    command: ["echo", "${SHARED}"]
`
	c := twoFiles(a, b, "a.yaml", "b.yaml")
	c.Files = []stacks.File{{Path: "a.yaml", Content: []byte(a)}, {Path: "b.yaml", Content: []byte(b)}}
	refs := stacks.Refs(c)
	by := map[string]stacks.VarRef{}
	var names []string
	for _, r := range refs {
		by[r.Name] = r
		names = append(names, r.Name)
	}
	if want := []string{"BARE_A", "ONLY_B", "PORT", "SHARED", "TAG"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v: the union of both files", names, want)
	}
	for name, want := range map[string]string{"TAG": "a.yaml", "BARE_A": "a.yaml", "PORT": "b.yaml", "ONLY_B": "b.yaml"} {
		places := by[name].Places
		if len(places) != 1 || places[0].File != want || places[0].Line < 1 {
			t.Errorf("%s places = %+v, want one in %s at a real line", name, places, want)
		}
	}
	// A variable used in both files is one reference with places in both, in file order then line.
	shared := by["SHARED"].Places
	var got []string
	for _, p := range shared {
		got = append(got, p.File)
	}
	if !slices.Equal(got, []string{"a.yaml", "b.yaml", "b.yaml"}) {
		t.Fatalf("SHARED places = %+v, want a.yaml then b.yaml twice", shared)
	}
	if shared[1].Line > shared[2].Line {
		t.Errorf("SHARED places in b.yaml = %+v, want them in line order", shared)
	}
	// Reversing the order reverses the places.
	rev := stacks.Refs(stacks.WorkingCopy{Files: c.Files, ComposeFiles: []string{"b.yaml", "a.yaml"}})
	for _, r := range rev {
		if r.Name == "SHARED" && r.Places[0].File != "b.yaml" {
			t.Errorf("SHARED first place = %s with b.yaml first, want b.yaml", r.Places[0].File)
		}
	}
	// An inline copy still reads compose.yaml.
	for _, r := range stacks.Refs(copyOf("services:\n  web:\n    image: ${IMG}\n")) {
		if r.Places[0].File != stacks.ComposeFile {
			t.Errorf("inline place file = %s, want compose.yaml", r.Places[0].File)
		}
	}
}

func TestMultiFileSecretUse(t *testing.T) {
	t.Parallel()
	const base = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	const override = `services:
  web:
    environment:
      TOKEN: ${PW}
secrets:
  tok:
    file: ./tok.txt
`
	c := stacks.WorkingCopy{
		Files: []stacks.File{
			{Path: "base.yaml", Content: []byte(base)},
			{Path: "override.yaml", Content: []byte(override)},
			{Path: "tok.txt", Content: []byte("s3cret-token-value")},
		},
		Vars:         []stacks.Var{{Name: "PW", Value: "Password2024!", Secret: true}},
		ComposeFiles: []string{"base.yaml", "override.yaml"},
	}
	uses := stacks.SecretFileUses(c.ComposeContents())
	if len(uses) != 1 || uses[0].Path != "tok.txt" || uses[0].Compose != "override.yaml" {
		t.Fatalf("SecretFileUses = %+v, want tok.txt used from override.yaml", uses)
	}
	marked := stacks.MarkSecretFilesFor(c.Files, nil, c.ComposeFiles)
	for _, f := range marked {
		if want := f.Path == "tok.txt"; f.Secret != want {
			t.Errorf("%s secret = %v, want %v", f.Path, f.Secret, want)
		}
	}
	if got := stacks.MarkSecretFiles(c.Files, nil); got[2].Secret {
		t.Error("MarkSecretFiles reads compose.yaml only: it must not see the second file")
	}
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "m", Target: stacks.TargetSwarm, Copy: c})
	if !slices.Equal(res.SecretFiles, []string{"tok.txt"}) {
		t.Errorf("Check.SecretFiles = %v, want [tok.txt]", res.SecretFiles)
	}
	// The secret variable used in the second file is known to the secret-use decision.
	s := stacks.NewSecretUse(c)
	if !s.Any() || !s.Uses("services.web.environment.TOKEN") {
		t.Errorf("SecretUse over both files: Any=%v Uses=%v, want both true for a variable used in override.yaml", s.Any(), s.Uses("services.web.environment.TOKEN"))
	}
	if s.Uses("services.web.image") {
		t.Error("services.web.image reported as using a secret")
	}
	// The same file named by both compose files is attributed to the first.
	both := []stacks.ComposeContent{{Path: "x.yaml", Content: []byte(override)}, {Path: "y.yaml", Content: []byte(override)}}
	if got := stacks.SecretFileUses(both); len(got) != 1 || got[0].Compose != "x.yaml" {
		t.Errorf("SecretFileUses = %+v, want the first compose file", got)
	}
}
