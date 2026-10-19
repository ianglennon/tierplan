// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
)

func checkOf(t *testing.T, compose string) stacks.CheckResult {
	t.Helper()
	return stacks.Check(context.Background(), stacks.CheckInput{Name: "probe", Target: stacks.TargetSwarm, Copy: copyOf(compose)})
}

func onlyProblem(t *testing.T, res stacks.CheckResult) stacks.Problem {
	t.Helper()
	if len(res.Problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one", res.Problems)
	}
	return res.Problems[0]
}

func TestCheckPositionsAYAMLError(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		compose string
		line    int
		message string
	}{
		"a tab as indentation": {"services:\n\tweb:\n    image: x\n", 2, "Line 2: found character that cannot start any token"},
		"an unclosed flow":     {"services:\n  web:\n    image: [x\n", 4, "Line 4: did not find expected ',' or ']'"},
		"a bad indentation":    {"services:\n  web:\n    image: nginx\n   ports: x\n", 4, "Line 4: did not find expected key"},
		"a repeated key":       {"services:\n  web:\n    image: a\n    image: b\n", 4, `Line 4: mapping key "image" already defined at line 3`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := checkOf(t, tc.compose)
			p := onlyProblem(t, res)
			if p.Severity != stacks.SeverityError || p.Code != stacks.CodeYAMLInvalid || p.File != stacks.ComposeFile {
				t.Errorf("problem = %+v, want an error yaml_invalid in compose.yaml", p)
			}
			if p.Line != tc.line || p.Message != tc.message {
				t.Errorf("line %d %q, want line %d %q", p.Line, p.Message, tc.line, tc.message)
			}
			if res.Loaded != nil {
				t.Error("a file that does not parse must not come back loaded")
			}
		})
	}
}

func TestCheckPositionsASchemaErrorAtTheKey(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		compose string
		line    int
		col     int
		message string
	}{
		"an unknown service key": {"services:\n  web:\n    imag: nginx\n", 3, 5, "services.web: unknown key “imag”."},
		"a wrong type":           {"services:\n  web:\n    image: nginx\n    ports: 80\n", 4, 5, "services.web.ports: must be an array"},
		"an unknown top key":     {"services:\n  web:\n    image: nginx\nfoo: bar\n", 4, 1, "unknown key “foo”."},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := onlyProblem(t, checkOf(t, tc.compose))
			if p.Severity != stacks.SeverityError || p.Code != stacks.CodeSchemaInvalid {
				t.Errorf("problem = %+v, want an error schema_invalid", p)
			}
			if p.Line != tc.line || p.Col != tc.col || p.Message != tc.message {
				t.Errorf("at %d:%d %q, want %d:%d %q", p.Line, p.Col, p.Message, tc.line, tc.col, tc.message)
			}
			if p.EndLine != p.Line || p.EndCol <= p.Col {
				t.Errorf("range %d:%d-%d:%d is not a mark on the key", p.Line, p.Col, p.EndLine, p.EndCol)
			}
		})
	}
}

func TestCheckOfAGoodFileHasNoProblemsAndComesBackLoaded(t *testing.T) {
	t.Parallel()
	for _, target := range []stacks.Target{stacks.TargetSwarm, stacks.TargetStandalone} {
		res := stacks.Check(context.Background(), stacks.CheckInput{Name: "starter", Target: target, Copy: copyOf(stacks.StarterCompose)})
		if len(res.Problems) != 0 || res.Loaded == nil {
			t.Errorf("%s: problems %+v loaded %v, want none and a loaded project", target, res.Problems, res.Loaded)
		}
	}
}

func TestCheckExplainsTheFilesThatCannotBeLoaded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		compose string
		code    string
		line    int
		want    string
	}{
		"an empty file":    {"", stacks.CodeComposeInvalid, 1, "compose.yaml is empty. Add a services section."},
		"a scalar":         {"just text\n", stacks.CodeComposeInvalid, 1, "The top level of compose.yaml must be a mapping"},
		"an undefined net": {"services:\n  web:\n    image: a\n    networks: [nope]\n", stacks.CodeComposeInvalid, 4, "services.web.networks: network nope isn't defined"},
		"a required var":   {"services:\n  web:\n    image: ${X:?need x}\n", stacks.CodeComposeInvalid, 3, "services.web.image: "},
		"a file over 1MiB": {"services: {}\n# " + strings.Repeat("x", stacks.MaxFileBytes), stacks.CodeComposeTooLarge, 1, "over 1 MiB"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := onlyProblem(t, checkOf(t, tc.compose))
			if p.Code != tc.code || p.Line != tc.line || !strings.Contains(p.Message, tc.want) {
				t.Errorf("problem = %+v, want code %s at line %d containing %q", p, tc.code, tc.line, tc.want)
			}
		})
	}
}

func TestProblemsCountSortAndDoNotAlias(t *testing.T) {
	t.Parallel()
	in := stacks.Problems{
		{Severity: stacks.SeverityWarning, File: "b.env", Line: 1},
		{Severity: stacks.SeverityError, File: "compose.yaml", Line: 9},
		{Severity: stacks.SeverityError, File: "compose.yaml", Line: 2},
	}
	if in.Errors() != 2 || in.Warnings() != 1 {
		t.Errorf("errors %d warnings %d, want 2 and 1", in.Errors(), in.Warnings())
	}
	out := in.Sorted()
	if out[0].File != "b.env" || out[1].Line != 2 || out[2].Line != 9 {
		t.Errorf("sorted = %+v, want b.env then compose.yaml by line", out)
	}
	if in[0].File != "b.env" || in[1].Line != 9 {
		t.Error("Sorted changed its receiver")
	}
}

func TestCheckListsTheFilesNobodyRefersTo(t *testing.T) {
	t.Parallel()
	in := stacks.CheckInput{Name: "probe", Target: stacks.TargetSwarm, Copy: stacks.WorkingCopy{Files: []stacks.File{
		{Path: "compose.yaml", Content: []byte("services:\n  web:\n    image: x\n    env_file: app.env\n")},
		{Path: "app.env"},
		{Path: "notes.txt"},
		{Path: "config/old.conf"},
	}}}
	res := stacks.Check(context.Background(), in)
	if len(res.Problems) != 0 {
		t.Fatalf("problems = %+v", res.Problems)
	}
	if want := []string{"config/old.conf", "notes.txt"}; !slices.Equal(res.UnusedFiles, want) {
		t.Errorf("unused = %v, want %v (compose.yaml and a referenced file are never listed)", res.UnusedFiles, want)
	}
}

func TestCheckStopsBeforeTheLoadOnABlockingPrescanProblem(t *testing.T) {
	t.Parallel()
	// The file also has a schema error. A file that names a file of the server is refused for that
	// alone: the loader is not given it, so nothing else about it is reported.
	res := checkOf(t, "include: [/etc/hostname]\nservices:\n  web:\n    imag: x\n")
	p := onlyProblem(t, res)
	if p.Code != stacks.CodeIncludeUnsupported || res.Loaded != nil {
		t.Errorf("problem = %+v loaded = %v, want only include_unsupported and no load", p, res.Loaded)
	}
}

func TestCheckFilesStillListedUnusedWhenTheLoadWasStopped(t *testing.T) {
	t.Parallel()
	in := stacks.CheckInput{Name: "probe", Target: stacks.TargetSwarm, Copy: stacks.WorkingCopy{Files: []stacks.File{
		{Path: "compose.yaml", Content: []byte("services:\n  web:\n    image: x\n    env_file: gone.env\n")},
		{Path: "other.env"},
	}}}
	res := stacks.Check(context.Background(), in)
	if len(res.Problems) != 1 || res.Problems[0].Code != stacks.CodeFileMissing {
		t.Fatalf("problems = %+v", res.Problems)
	}
	if !slices.Equal(res.UnusedFiles, []string{"other.env"}) {
		t.Errorf("unused = %v, want [other.env]", res.UnusedFiles)
	}
}
