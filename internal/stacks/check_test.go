// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

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
		{Path: "compose.yaml", Content: []byte("services:\n  web:\n    image: x:1\n    env_file: app.env\n")},
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
		{Path: "compose.yaml", Content: []byte("services:\n  web:\n    image: x:1\n    env_file: gone.env\n")},
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

// Relative bind mounts (plan 07-06, D-06): a bind source that is not an absolute path is blocked on
// both targets, with the volume's line.

const relativeBindWording = "“%s” is a relative bind mount. Use a named volume, or an absolute path on the host (which needs Admin)."

func withVolumes(entries string, top string) string {
	return "services:\n  web:\n    image: x:1\n    volumes:\n      " + entries + "\n" + top
}

func TestCheckBlocksARelativeBindMountOnBothTargets(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		volume string
		source string
		line   int
	}{
		"a dot-slash path":                     {"- ./data:/data", "./data", 5},
		"a parent path":                        {"- ../shared:/shared:ro", "../shared", 5},
		"a home path":                          {"- ~/data:/data", "~/data", 5},
		"a name that is not a declared volume": {"- data:/data", "data", 5},
		"a long form bind":                     {"- type: bind\n        source: ./x\n        target: /x", "./x", 5},
		"a long form bind with a bare name":    {"- type: bind\n        source: x\n        target: /x", "x", 5},
	} {
		for _, target := range []stacks.Target{stacks.TargetSwarm, stacks.TargetStandalone} {
			t.Run(name+" on "+string(target), func(t *testing.T) {
				t.Parallel()
				res := stacks.Check(context.Background(), stacks.CheckInput{Name: "probe", Target: target, Copy: copyOf(withVolumes(tc.volume, ""))})
				p := onlyProblem(t, res)
				if p.Severity != stacks.SeverityError || p.Code != stacks.CodeRelativeBind || p.File != stacks.ComposeFile || p.Line != tc.line {
					t.Errorf("problem = %+v, want an error relative_bind at line %d", p, tc.line)
				}
				if want := fmt.Sprintf(relativeBindWording, tc.source); p.Message != want {
					t.Errorf("message = %q, want %q", p.Message, want)
				}
			})
		}
	}
}

func TestCheckAllowsAnAbsoluteBindAndNamedVolumes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ volume, top string }{
		"an absolute bind":          {"- /srv/data:/data", ""},
		"an anonymous volume":       {"- /data", ""},
		"a declared named volume":   {"- data:/data", "volumes:\n  data: {}\n"},
		"a long form named volume":  {"- type: volume\n        source: data\n        target: /data", "volumes:\n  data: {}\n"},
		"a long form absolute bind": {"- type: bind\n        source: /srv/x\n        target: /x", ""},
		"a tmpfs":                   {"- type: tmpfs\n        target: /tmp", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := checkOf(t, withVolumes(tc.volume, tc.top))
			if len(res.Problems) != 0 || res.Loaded == nil {
				t.Errorf("problems = %+v loaded = %v, want none and a loaded project", res.Problems, res.Loaded)
			}
		})
	}
}

func TestCheckNamesEveryRelativeBindOfEveryService(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  a:\n    image: x:1\n    volumes:\n      - ./one:/one\n      - /srv/ok:/ok\n      - ./two:/two\n  b:\n    image: y:1\n    volumes:\n      - ./three:/three\n"
	res := checkOf(t, compose)
	var lines []int
	for _, p := range res.Problems {
		if p.Code != stacks.CodeRelativeBind {
			t.Errorf("problem = %+v, want only relative_bind", p)
		}
		lines = append(lines, p.Line)
	}
	if !slices.Equal(lines, []int{5, 7, 11}) {
		t.Errorf("lines = %v, want [5 7 11]", lines)
	}
}

// What is asserted after the load, as defence in depth: none of this should ever fire, because the
// prescan refuses it first.

func TestAssertSandboxedAcceptsWhatTheStackOwns(t *testing.T) {
	t.Parallel()
	files := stacks.NewFileSet([]stacks.File{{Path: "compose.yaml"}, {Path: "app.env"}, {Path: "secret.txt"}, {Path: "app.conf"}})
	project := &types.Project{
		Services: types.Services{"web": {
			Name:     "web",
			EnvFiles: []types.EnvFile{{Path: "app.env"}, {Path: "./app.env"}},
			Volumes:  []types.ServiceVolumeConfig{{Type: "bind", Source: "/srv/data", Target: "/data"}, {Type: "volume", Source: "data", Target: "/d"}},
		}},
		Secrets: types.Secrets{"s": {File: "./secret.txt"}, "e": {Environment: "X"}},
		Configs: types.Configs{"c": {File: "app.conf"}, "i": {Content: "hello"}},
	}
	if err := stacks.AssertSandboxed(project, files); err != nil {
		t.Errorf("AssertSandboxed = %v, want nil", err)
	}
}

func TestAssertSandboxedRefusesWhatShouldHaveBeenRefusedEarlier(t *testing.T) {
	t.Parallel()
	files := stacks.NewFileSet([]stacks.File{{Path: "compose.yaml"}, {Path: "app.env"}})
	web := func(mut func(*types.ServiceConfig)) *types.Project {
		svc := types.ServiceConfig{Name: "web"}
		mut(&svc)
		return &types.Project{Services: types.Services{"web": svc}}
	}
	for name, project := range map[string]*types.Project{
		"extends": web(func(s *types.ServiceConfig) { s.Extends = &types.ExtendsConfig{File: "x.yaml", Service: "y"} }),
		"a relative bind": web(func(s *types.ServiceConfig) {
			s.Volumes = []types.ServiceVolumeConfig{{Type: "bind", Source: "./x", Target: "/x"}}
		}),
		"an env file outside":    web(func(s *types.ServiceConfig) { s.EnvFiles = []types.EnvFile{{Path: "/proc/self/environ"}} }),
		"an env file not ours":   web(func(s *types.ServiceConfig) { s.EnvFiles = []types.EnvFile{{Path: "gone.env"}} }),
		"a label file":           web(func(s *types.ServiceConfig) { s.LabelFiles = []string{"labels.env"} }),
		"a secret file":          {Secrets: types.Secrets{"s": {File: "/etc/shadow"}}},
		"a secret file not ours": {Secrets: types.Secrets{"s": {File: "gone.txt"}}},
		"a config file":          {Configs: types.Configs{"c": {File: "../app.conf"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := stacks.AssertSandboxed(project, files); !errors.Is(err, stacks.ErrNotSandboxed) {
				t.Errorf("AssertSandboxed = %v, want ErrNotSandboxed", err)
			}
		})
	}
}

func TestCheckEndsWithCheckTimeoutPastItsDeadline(t *testing.T) {
	t.Parallel()
	// A deadline that has passed before the check starts: no sleep, no race.
	res := stacks.Check(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetSwarm, Copy: copyOf(stacks.StarterCompose), Deadline: time.Nanosecond,
	})
	p := onlyProblem(t, res)
	if p.Severity != stacks.SeverityError || p.Code != stacks.CodeCheckTimeout || res.Loaded != nil {
		t.Errorf("problem = %+v loaded = %v, want one error check_timeout and no project", p, res.Loaded)
	}
}

func TestCheckPassesTheFilesAComposeFileOwns(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: x:1\n    env_file:\n      - ./app.env\n      - path: config/db.env\n        required: false\n    secrets: [db]\n    configs: [app]\nsecrets:\n  db:\n    file: ./secret.txt\nconfigs:\n  app:\n    file: app.conf\n"
	in := stacks.CheckInput{Name: "probe", Target: stacks.TargetSwarm, Copy: stacks.WorkingCopy{Files: []stacks.File{
		{Path: "compose.yaml", Content: []byte(compose)}, {Path: "app.env"}, {Path: "config/db.env"}, {Path: "secret.txt", Secret: true}, {Path: "app.conf"},
	}}}
	res := stacks.Check(context.Background(), in)
	if len(res.Problems) != 0 || res.Loaded == nil || len(res.UnusedFiles) != 0 {
		t.Errorf("problems %+v loaded %v unused %v, want a clean project that uses every file", res.Problems, res.Loaded, res.UnusedFiles)
	}
}

func TestCheckStripsBuildFromTheProjectItReturns(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: registry.example/web:1.2
    build:
      context: .
      dockerfile: Dockerfile
  worker:
    image: registry.example/worker:3
`
	for _, target := range []stacks.Target{stacks.TargetSwarm, stacks.TargetStandalone} {
		res := stacks.Check(context.Background(), stacks.CheckInput{Name: "probe", Target: target, Copy: copyOf(compose)})
		if res.Loaded == nil {
			t.Fatalf("%s: no project; problems %+v", target, res.Problems)
		}
		for name, svc := range res.Loaded.Project.Services {
			if svc.Build != nil {
				t.Errorf("%s: service %s still has a build: the engine must never see one", target, name)
			}
			if svc.Image == "" {
				t.Errorf("%s: service %s lost its image", target, name)
			}
		}
		if res.Problems.Errors() != 0 || res.Problems.Warnings() != 1 {
			t.Errorf("%s: problems = %+v, want one warning and no error", target, res.Problems)
		}
	}
}

func TestCheckAWarningDoesNotBlockButAnErrorDoes(t *testing.T) {
	t.Parallel()
	warn := checkOf(t, "services:\n  web:\n    image: registry.example/web:1\n    container_name: front\n")
	if warn.Problems.Warnings() != 1 || warn.Problems.Errors() != 0 || warn.Loaded == nil {
		t.Errorf("warning only: problems %+v loaded %v", warn.Problems, warn.Loaded)
	}
	blocked := checkOf(t, "services:\n  web:\n    build: .\n")
	if blocked.Problems.Errors() != 1 {
		t.Errorf("build without image: problems %+v, want one error", blocked.Problems)
	}
}

func TestPositionIndexAnswersExactPathsOnly(t *testing.T) {
	t.Parallel()
	ix, err := stacks.NewPositionIndex([]byte("services:\n  web:\n    image: nginx\n    ports:\n      - target: 80\n        mode: host\n"))
	if err != nil {
		t.Fatal(err)
	}
	if pos, ok := ix.Lookup("services.web.ports.0.mode"); !ok || pos.Line != 6 || pos.Col != 9 {
		t.Errorf("Lookup(mode) = %+v, %v, want line 6 col 9", pos, ok)
	}
	if _, ok := ix.Lookup("services.web.ports.0.protocol"); ok {
		t.Error("Lookup of a key the file does not have must be false, not the nearest key")
	}
	if got := ix.Keys("services.web"); !slices.Equal(got, []string{"image", "ports"}) {
		t.Errorf("Keys(web) = %v", got)
	}
	if got := ix.Keys("services.web.ports"); !slices.Equal(got, []string{"0"}) {
		t.Errorf("Keys(ports) = %v, want the item indexes", got)
	}
	if v, ok := ix.Scalar("services.web.ports.0.mode"); !ok || v != "host" {
		t.Errorf("Scalar(mode) = %q, %v", v, ok)
	}
}

func TestCheckAnswersTheResolvedVariablesOfTheStack(t *testing.T) {
	t.Parallel()
	check := func(vars ...stacks.Var) stacks.CheckResult {
		return stacks.Check(context.Background(), stacks.CheckInput{
			Name: "probe", Target: stacks.TargetSwarm, Copy: copyOf(stacks.StarterCompose, vars...),
		})
	}
	res := check()
	if len(res.Variables) != 1 {
		t.Fatalf("variables = %+v, want WEB_PORT", res.Variables)
	}
	if v := res.Variables[0]; v.Name != "WEB_PORT" || v.Value != "8080" || v.Source != stacks.SourceDefault {
		t.Errorf("with an empty table: %+v, want 8080 from the default", v)
	}
	res = check(stacks.Var{Name: "WEB_PORT", Value: "9090"})
	if v := res.Variables[0]; v.Value != "9090" || v.Source != stacks.SourceTable {
		t.Errorf("with WEB_PORT=9090: %+v, want 9090 from the table", v)
	}
	if res.Loaded == nil || res.Loaded.Project.Services["web"].Ports[0].Published != "9090" {
		t.Errorf("the project was not loaded with the table's port: %+v", res.Loaded)
	}
}

func TestCheckAnswersVariablesEvenWhenTheFileIsRefusedLater(t *testing.T) {
	t.Parallel()
	// A schema error: the file parses, so the variables it refers to are still listed.
	res := checkOf(t, "services:\n  web:\n    imag: nginx:${TAG:-1}\n")
	if len(res.Variables) != 1 || res.Variables[0].Name != "TAG" {
		t.Errorf("variables = %+v, want TAG", res.Variables)
	}
	// A file that does not parse names none.
	if res := checkOf(t, "services: [\n"); len(res.Variables) != 0 {
		t.Errorf("variables = %+v, want none for a file that is not YAML", res.Variables)
	}
}

func withVars(t *testing.T, compose string, vars ...stacks.Var) stacks.CheckResult {
	t.Helper()
	return stacks.Check(context.Background(), stacks.CheckInput{Name: "probe", Target: stacks.TargetSwarm, Copy: copyOf(compose, vars...)})
}

// problemsOf are the problems of a result with this code, in the order Check lists them.
func problemsOf(res stacks.CheckResult, code string) []stacks.Problem {
	var out []stacks.Problem
	for _, p := range res.Problems {
		if p.Code == code {
			out = append(out, p)
		}
	}
	return out
}

func TestCheckBlocksAPlainVariableWithNoValueAtEachPlaceItIsUsed(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    environment:
      PASS: ${DB_PASS}
    labels:
      again: ${DB_PASS}
      other: ${REGION}
`
	res := withVars(t, compose, stacks.Var{Name: "REGION", Value: "eu"})
	got := problemsOf(res, stacks.CodeUnsetVariable)
	if len(got) != 2 {
		t.Fatalf("unset_variable problems = %+v, want one at each of the two places", got)
	}
	for i, line := range []int{5, 7} {
		p := got[i]
		if p.Severity != stacks.SeverityError || p.File != stacks.ComposeFile || p.Line != line ||
			p.Message != "DB_PASS has no value and no default. Add a value in Variables." {
			t.Errorf("problem %d = %+v, want the Variables error at line %d", i, p, line)
		}
	}
	if res.Problems.Errors() < 2 {
		t.Errorf("errors = %d: an undefined variable must block", res.Problems.Errors())
	}
	for _, v := range res.Variables {
		if v.Name == "DB_PASS" && (!v.Required || v.Source != stacks.SourceUnset) {
			t.Errorf("DB_PASS = %+v, want required and unset", v)
		}
	}
}

func TestCheckDoesNotBlockWhatHasADefaultOrAValue(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    environment:
      A: ${A_VAR:-}
      B: ${B_VAR-x}
      C: ${C_VAR:-y}
      D: ${D_VAR}
      E: ${E_VAR:-${F_VAR}}
`
	res := withVars(t, compose, stacks.Var{Name: "D_VAR", Value: "d"}, stacks.Var{Name: "E_VAR", Value: "e"})
	if got := problemsOf(res, stacks.CodeUnsetVariable); len(got) != 0 {
		t.Errorf("unset_variable = %+v, want none: a default, an empty default, a value and a nested default that is not read do not block", got)
	}
	if res.Problems.Errors() != 0 {
		t.Errorf("problems = %+v, want no error", res.Problems)
	}
}

func TestCheckBlocksAnUndefinedVariableEvenWhenTheRestOfTheFileDoesNotLoad(t *testing.T) {
	t.Parallel()
	// ${IMAGE} fills the image with nothing: compose-go refuses the service, and the person still
	// needs to be told which variable is missing.
	res := withVars(t, "services:\n  web:\n    image: ${IMAGE}\n")
	if got := problemsOf(res, stacks.CodeUnsetVariable); len(got) != 1 || got[0].Line != 3 {
		t.Errorf("unset_variable = %+v, want one at line 3", got)
	}
}

func TestCheckKeepsComposeGosOwnTextForARequiredVariable(t *testing.T) {
	t.Parallel()
	res := withVars(t, "services:\n  web:\n    image: nginx:1.27\n    environment:\n      PASS: ${DB_PASS:?set it}\n")
	p := onlyProblem(t, res)
	if p.Severity != stacks.SeverityError || p.Code != stacks.CodeComposeInvalid || !strings.Contains(p.Message, "set it") {
		t.Errorf("problem = %+v, want compose-go's own blocking error with its message", p)
	}
	if got := problemsOf(res, stacks.CodeUnsetVariable); len(got) != 0 {
		t.Errorf("unset_variable = %+v: a ${NAME:?message} has its own error and is not reported twice", got)
	}
}

func TestCheckTakesABareEnvironmentEntryFromTheTable(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    environment:
      - FOO
      - BAR=x
    labels:
      k: v
  api:
    image: nginx:1.27
    environment:
      QUX:
`
	res := withVars(t, compose, stacks.Var{Name: "FOO", Value: "from-table"})
	got := problemsOf(res, stacks.CodeUnsetVariable)
	if len(got) != 1 || got[0].Line != 11 || got[0].Message != "QUX has no value and no default. Add a value in Variables." {
		t.Fatalf("unset_variable = %+v, want only QUX, at line 11", got)
	}
	if res.Loaded == nil {
		t.Fatalf("the file did not load: %+v", res.Problems)
	}
	env := res.Loaded.Project.Services["web"].Environment
	if v := env["FOO"]; v == nil || *v != "from-table" {
		t.Errorf("FOO = %v, want from-table: a bare entry is filled from the table", v)
	}
	if v := env["BAR"]; v == nil || *v != "x" {
		t.Errorf("BAR = %v, want x", v)
	}
	if v := res.Loaded.Project.Services["api"].Environment["QUX"]; v != nil {
		t.Errorf("QUX = %q, want it left unset", *v)
	}
	if !slices.Contains(res.Loaded.Unset, "QUX") || slices.Contains(res.Loaded.Unset, "FOO") {
		t.Errorf("Unset = %v, want QUX and not FOO", res.Loaded.Unset)
	}
}

func TestCheckWarnsOfATableVariableNoFileUsesAndDoesNotBlock(t *testing.T) {
	t.Parallel()
	res := withVars(t, stacks.StarterCompose, stacks.Var{Name: "OLD_THING", Value: "x"})
	got := problemsOf(res, stacks.CodeUnusedVariable)
	if len(got) != 1 || got[0].Severity != stacks.SeverityWarning || got[0].File != stacks.ComposeFile ||
		got[0].Message != "OLD_THING is in Variables but the compose file doesn't use it." {
		t.Fatalf("unused_variable = %+v, want one warning with the Variables wording", got)
	}
	if res.Problems.Errors() != 0 {
		t.Errorf("problems = %+v: an unused variable never blocks", res.Problems)
	}
	var unused bool
	for _, v := range res.Variables {
		if v.Name == "OLD_THING" {
			unused = v.Unused
		}
	}
	if !unused {
		t.Errorf("variables = %+v, want OLD_THING marked unused", res.Variables)
	}
	// A variable the file uses is not unused, whatever is in the table.
	res = withVars(t, stacks.StarterCompose, stacks.Var{Name: "WEB_PORT", Value: "9090"})
	if got := problemsOf(res, stacks.CodeUnusedVariable); len(got) != 0 {
		t.Errorf("unused_variable = %+v, want none", got)
	}
}

func TestCheckNeverCarriesASecretValueInAnAnswer(t *testing.T) {
	t.Parallel()
	const secret = "registry.example/very-secret-image"
	// The moving-tag warning quotes the image after variables: a secret that is the image is the
	// case the masker exists for.
	res := withVars(t, "services:\n  web:\n    image: ${IMG}\n", stacks.Var{Name: "IMG", Value: secret, Secret: true})
	if got := problemsOf(res, stacks.CodeMovingTag); len(got) != 1 {
		t.Fatalf("moving_tag = %+v, want the warning that quotes the image", got)
	}
	for _, p := range res.Problems {
		if strings.Contains(p.Message, secret) || strings.Contains(p.Key, secret) {
			t.Errorf("problem %+v carries the secret", p)
		}
	}
	if got := problemsOf(res, stacks.CodeMovingTag)[0].Message; !strings.Contains(got, stacks.Hidden) {
		t.Errorf("message = %q, want the secret shown as %s", got, stacks.Hidden)
	}
	for _, v := range res.Variables {
		if strings.Contains(v.Value, secret) || strings.Contains(v.Default, secret) {
			t.Errorf("variable %+v carries the secret", v)
		}
	}
}
