// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stacks_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/stacks"
)

// The raw-YAML prescan (plan 07-06, STK-01, D-05, A4; T-07-26, T-07-27): what a compose file may
// name, judged before anything loads it.

func fileSet(paths ...string) stacks.FileSet {
	files := []stacks.File{{Path: stacks.ComposeFile}}
	for _, p := range paths {
		files = append(files, stacks.File{Path: p})
	}
	return stacks.NewFileSet(files)
}

type want struct {
	code string
	line int
	msg  string
}

func TestPrescanRefusesWhatNamesAFileOfTheServer(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		compose string
		want    []want
	}{
		"include as a list": {
			"include:\n  - other.yaml\nservices:\n  web:\n    image: x\n",
			[]want{{stacks.CodeIncludeUnsupported, 1, "include isn't supported. Put everything in compose.yaml."}},
		},
		"include as a string": {
			"services:\n  web:\n    image: x\ninclude: other.yaml\n",
			[]want{{stacks.CodeIncludeUnsupported, 4, "include isn't supported. Put everything in compose.yaml."}},
		},
		"include as a long form": {
			"include:\n  - path: ./other.yaml\n    env_file: ./o.env\nservices: {}\n",
			[]want{{stacks.CodeIncludeUnsupported, 1, "include isn't supported. Put everything in compose.yaml."}},
		},
		"a remote include": {
			"include:\n  - https://example.com/compose.yaml\nservices: {}\n",
			[]want{{stacks.CodeIncludeUnsupported, 1, "include isn't supported. Put everything in compose.yaml."}},
		},
		"extends with a file": {
			"services:\n  web:\n    image: x\n  api:\n    extends:\n      file: ./base.yaml\n      service: web\n",
			[]want{{stacks.CodeExtendsUnsupported, 5, "extends isn't supported. Copy the settings you need into the service."}},
		},
		"extends within the file": {
			"services:\n  web:\n    image: x\n  api:\n    extends: web\n",
			[]want{{stacks.CodeExtendsUnsupported, 5, "extends isn't supported. Copy the settings you need into the service."}},
		},
		"label_file": {
			"services:\n  web:\n    image: x\n    label_file: labels.env\n",
			[]want{{stacks.CodeLabelFileUnsupported, 4, "label_file isn't supported. Put the labels under labels in compose.yaml."}},
		},
		"label_file as a list": {
			"services:\n  web:\n    image: x\n    label_file:\n      - /etc/passwd\n",
			[]want{{stacks.CodeLabelFileUnsupported, 4, "label_file isn't supported. Put the labels under labels in compose.yaml."}},
		},
		"extends brought in by a merge key": {
			"x-base: &base\n  extends: {file: /etc/hostname, service: x}\nservices:\n  web:\n    <<: *base\n    image: x\n",
			[]want{{stacks.CodeExtendsUnsupported, 5, ""}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			problems, _ := stacks.Prescan([]byte(tc.compose), fileSet())
			if len(problems) != len(tc.want) {
				t.Fatalf("problems = %+v, want %d", problems, len(tc.want))
			}
			for i, w := range tc.want {
				p := problems[i]
				if p.Severity != stacks.SeverityError || p.Code != w.code || p.Line != w.line || p.File != stacks.ComposeFile {
					t.Errorf("problem %d = %+v, want an error %s at line %d", i, p, w.code, w.line)
				}
				if w.msg != "" && p.Message != w.msg {
					t.Errorf("message = %q, want %q", p.Message, w.msg)
				}
			}
		})
	}
}

func TestPrescanChecksEveryFormOfEnvFile(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: x\n"
	for name, tc := range map[string]struct {
		env  string
		want []want
		refs []string
	}{
		"a string that is a stack file":     {"    env_file: app.env\n", nil, []string{"app.env"}},
		"a string with ./":                  {"    env_file: ./app.env\n", nil, []string{"app.env"}},
		"a list of stack files":             {"    env_file:\n      - app.env\n      - config/db.env\n", nil, []string{"app.env", "config/db.env"}},
		"a list of maps":                    {"    env_file:\n      - path: app.env\n        required: false\n        format: raw\n", nil, []string{"app.env"}},
		"a list of strings and maps":        {"    env_file:\n      - app.env\n      - path: config/db.env\n", nil, []string{"app.env", "config/db.env"}},
		"a missing string":                  {"    env_file: gone.env\n", []want{{stacks.CodeFileMissing, 4, "env_file “gone.env” isn't one of this stack's files. Add it under Files."}}, nil},
		"a missing entry of a list":         {"    env_file:\n      - app.env\n      - gone.env\n", []want{{stacks.CodeFileMissing, 6, "env_file “gone.env” isn't one of this stack's files. Add it under Files."}}, []string{"app.env"}},
		"a missing path of a map":           {"    env_file:\n      - path: gone.env\n", []want{{stacks.CodeFileMissing, 5, ""}}, nil},
		"a missing optional path":           {"    env_file:\n      - path: gone.env\n        required: false\n", nil, nil},
		"a missing path that is required":   {"    env_file:\n      - path: gone.env\n        required: true\n", []want{{stacks.CodeFileMissing, 5, ""}}, nil},
		"an optional path that is illegal":  {"    env_file:\n      - path: ../gone.env\n        required: false\n", []want{{stacks.CodePathIllegal, 5, ""}}, nil},
		"an optional path that is there":    {"    env_file:\n      - path: app.env\n        required: false\n", nil, []string{"app.env"}},
		"an absolute path":                  {"    env_file: /proc/self/environ\n", []want{{stacks.CodePathIllegal, 4, "“/proc/self/environ” isn't allowed. Stack files use a path inside the stack, without “..” or a leading /."}}, nil},
		"a path with ..":                    {"    env_file:\n      - ../app.env\n", []want{{stacks.CodePathIllegal, 5, "“../app.env” isn't allowed. Stack files use a path inside the stack, without “..” or a leading /."}}, nil},
		"an illegal path in a long form":    {"    env_file:\n      - path: /etc/passwd\n", []want{{stacks.CodePathIllegal, 5, ""}}, nil},
		"a path with a colon":               {"    env_file: C:/x.env\n", []want{{stacks.CodePathIllegal, 4, ""}}, nil},
		"an alias reports where it is used": {"    env_file: *e\n", []want{{stacks.CodeFileMissing, 4, ""}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			compose := head + tc.env
			if name == "an alias reports where it is used" {
				compose = "x-env: &e gone.env\n" + head + tc.env
				tc.want[0].line = 5
			}
			problems, refs := stacks.Prescan([]byte(compose), fileSet("app.env", "config/db.env"))
			if len(problems) != len(tc.want) {
				t.Fatalf("problems = %+v, want %d", problems, len(tc.want))
			}
			for i, w := range tc.want {
				p := problems[i]
				if p.Code != w.code || p.Line != w.line {
					t.Errorf("problem %d = %+v, want %s at line %d", i, p, w.code, w.line)
				}
				if w.msg != "" && p.Message != w.msg {
					t.Errorf("message = %q, want %q", p.Message, w.msg)
				}
			}
			if !slices.Equal(refs, tc.refs) {
				t.Errorf("refs = %v, want %v", refs, tc.refs)
			}
		})
	}
}

func TestPrescanChecksSecretAndConfigFiles(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		compose string
		want    []want
		refs    []string
	}{
		"a secret file of the stack":  {"secrets:\n  db:\n    file: ./secret.txt\n", nil, []string{"secret.txt"}},
		"a config file of the stack":  {"configs:\n  app:\n    file: app.conf\n", nil, []string{"app.conf"}},
		"a missing secret file":       {"secrets:\n  db:\n    file: gone.txt\n", []want{{stacks.CodeFileMissing, 3, "secrets.db.file “gone.txt” isn't one of this stack's files. Add it under Files."}}, nil},
		"a missing config file":       {"configs:\n  app:\n    file: gone.conf\n", []want{{stacks.CodeFileMissing, 3, "configs.app.file “gone.conf” isn't one of this stack's files. Add it under Files."}}, nil},
		"an absolute secret file":     {"secrets:\n  shadow:\n    file: /etc/shadow\n", []want{{stacks.CodePathIllegal, 3, "“/etc/shadow” isn't allowed. Stack files use a path inside the stack, without “..” or a leading /."}}, nil},
		"a parent config file":        {"configs:\n  app:\n    file: ../app.conf\n", []want{{stacks.CodePathIllegal, 3, ""}}, nil},
		"a secret from the variables": {"secrets:\n  db:\n    environment: DB_PASSWORD\n", nil, nil},
		"a config with content":       {"configs:\n  app:\n    content: |\n      hello\n", nil, nil},
		"an external secret":          {"secrets:\n  db:\n    external: true\n", nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			problems, refs := stacks.Prescan([]byte(tc.compose+"services: {}\n"), fileSet("secret.txt", "app.conf"))
			if len(problems) != len(tc.want) {
				t.Fatalf("problems = %+v, want %d", problems, len(tc.want))
			}
			for i, w := range tc.want {
				p := problems[i]
				if p.Code != w.code || p.Line != w.line {
					t.Errorf("problem %d = %+v, want %s at line %d", i, p, w.code, w.line)
				}
				if w.msg != "" && p.Message != w.msg {
					t.Errorf("message = %q, want %q", p.Message, w.msg)
				}
			}
			if !slices.Equal(refs, tc.refs) {
				t.Errorf("refs = %v, want %v", refs, tc.refs)
			}
		})
	}
}

func TestPrescanNeverLooksOutsideTheFileSet(t *testing.T) {
	t.Parallel()
	// go.mod is a real file at the root of this module, and files_test.go is a real file in the
	// directory the test runs in. Neither is a file of the stack, so neither is found.
	const compose = "services:\n  web:\n    image: x\n    env_file:\n      - files_test.go\n      - ../../go.mod\n"
	problems, refs := stacks.Prescan([]byte(compose), fileSet())
	codes := []string{}
	for _, p := range problems {
		codes = append(codes, p.Code)
	}
	if !slices.Equal(codes, []string{stacks.CodeFileMissing, stacks.CodePathIllegal}) || len(refs) != 0 {
		t.Errorf("codes = %v refs = %v, want file_missing then path_illegal and no refs", codes, refs)
	}
}

func TestPrescanOfAGoodFileHasNothingToSay(t *testing.T) {
	t.Parallel()
	problems, refs := stacks.Prescan([]byte(stacks.StarterCompose), fileSet())
	if len(problems) != 0 || len(refs) != 0 {
		t.Errorf("problems %+v refs %v, want none", problems, refs)
	}
}

func TestPrescanLeavesYAMLErrorsToTheLoad(t *testing.T) {
	t.Parallel()
	problems, refs := stacks.Prescan([]byte("services:\n\tweb:\n"), fileSet())
	if len(problems) != 0 || len(refs) != 0 {
		t.Errorf("problems %+v refs %v, want none: the load reports a syntax error", problems, refs)
	}
}

func TestPrescanReadsEveryDocument(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: x\n---\nservices:\n  api:\n    image: y\n    env_file: /etc/passwd\n"
	problems, _ := stacks.Prescan([]byte(compose), fileSet())
	if len(problems) != 1 || problems[0].Code != stacks.CodePathIllegal || problems[0].Line != 8 {
		t.Errorf("problems = %+v, want path_illegal at line 8 of the second document", problems)
	}
}

// The hostile corpus: each file is a way to read the Coaming server's disk or to make it fetch or
// work without end. Each yields a blocking problem, and the file never reaches the loader.

func TestHostileCorpusYieldsBlockingProblemsAndLoadsNothing(t *testing.T) {
	t.Parallel()
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("no host name to look for: %v", err)
	}
	wantCode := map[string]string{
		"include-etc.yaml":        stacks.CodeIncludeUnsupported,
		"extends-file.yaml":       stacks.CodeExtendsUnsupported,
		"env-file-proc.yaml":      stacks.CodePathIllegal,
		"secret-file-shadow.yaml": stacks.CodePathIllegal,
		"remote-include.yaml":     stacks.CodeIncludeUnsupported,
		"alias-bomb.yaml":         stacks.CodeComposeTooComplex,
	}
	paths, err := filepath.Glob("testdata/hostile/*.yaml")
	if err != nil || len(paths) != len(wantCode) {
		t.Fatalf("hostile corpus = %v (%v), want %d files", paths, err, len(wantCode))
	}
	for _, path := range paths {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			content, err := os.ReadFile(path) //nolint:gosec // a file of the hostile corpus under testdata
			if err != nil {
				t.Fatal(err)
			}
			res := checkOf(t, string(content))
			if res.Loaded != nil {
				t.Fatal("a hostile file came back loaded: the loader must never see it")
			}
			if res.Problems.Errors() == 0 {
				t.Fatalf("problems = %+v, want a blocking problem", res.Problems)
			}
			found := false
			for _, p := range res.Problems {
				found = found || p.Code == wantCode[name]
				if strings.Contains(p.Message, host) {
					t.Errorf("a message holds the host name %q: %q", host, p.Message)
				}
			}
			if !found {
				t.Errorf("problems = %+v, want %s", res.Problems, wantCode[name])
			}
		})
	}
}

func TestAliasBombStopsAtTheWorkBudgetWithTheComplexityMessage(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("testdata/hostile/alias-bomb.yaml")
	if err != nil {
		t.Fatal(err)
	}
	problems, refs := stacks.Prescan(content, fileSet())
	if len(problems) != 1 || problems[0].Code != stacks.CodeComposeTooComplex || problems[0].Message != "The compose file is too complex to check." || refs != nil {
		t.Errorf("problems = %+v refs = %v, want the one compose_too_complex problem", problems, refs)
	}
}
