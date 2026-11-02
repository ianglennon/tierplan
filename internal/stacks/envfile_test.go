// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/convert"
)

// env_file (plan 07-12, D-05, Pitfall 4): TierPlan reads an env_file once, from the stack's own
// files, with compose-go's .env parser and the variable table as the lookup. Nothing else reads it,
// so a swarm and a standalone host can never see different values.

// stackWith is a working copy of a compose file, the files it carries and the table.
func stackWith(compose string, vars []stacks.Var, files ...stacks.File) stacks.WorkingCopy {
	return stacks.WorkingCopy{
		Files: append([]stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, files...),
		Vars:  vars,
	}
}

func file(path, content string) stacks.File { return stacks.File{Path: path, Content: []byte(content)} }

// tableLookup is the lookup Check gives the resolver: the table's non-empty values.
func tableLookup(vars []stacks.Var) func(string) (string, bool) {
	return func(name string) (string, bool) {
		for _, v := range vars {
			if v.Name == name && v.Value != "" {
				return v.Value, true
			}
		}
		return "", false
	}
}

// resolve loads the working copy and resolves its env_files, as Check does.
func resolve(t *testing.T, wc stacks.WorkingCopy) (*types.Project, error) {
	t.Helper()
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{Name: "probe", Target: stacks.TargetSwarm, Copy: wc})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return loaded.Project, stacks.ResolveEnvFiles(loaded.Project, stacks.NewFileSet(wc.Files), tableLookup(wc.Vars))
}

// environment flattens a service's environment; an unset entry is absent.
func environment(svc types.ServiceConfig) map[string]string {
	out := map[string]string{}
	for k, v := range svc.Environment {
		if v != nil {
			out[k] = *v
		}
	}
	return out
}

func wantEnv(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Errorf("environment = %v, want %v", got, want)
	}
}

func TestResolveEnvFilesReadsTheStacksFilesUnderTheExplicitEnvironment(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file:\n      - app.env\n    environment:\n      B: \"9\"\n      C: c\n"
	vars := []stacks.Var{{Name: "X", Value: "7"}}
	p, err := resolve(t, stackWith(compose, vars, file("app.env", "A=1\nB=${X}\nD=d\n")))
	if err != nil {
		t.Fatalf("ResolveEnvFiles: %v", err)
	}
	wantEnv(t, environment(p.Services["web"]), map[string]string{"A": "1", "B": "9", "C": "c", "D": "d"})
	if n := len(p.Services["web"].EnvFiles); n != 0 {
		t.Errorf("EnvFiles = %d entries, want none", n)
	}
}

func TestResolveEnvFilesFillsAValueFromTheTable(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file: app.env\n"
	p, err := resolve(t, stackWith(compose, []stacks.Var{{Name: "X", Value: "7"}}, file("app.env", "A=1\nB=${X}\nC=${NOT_IN_THE_TABLE}\n")))
	if err != nil {
		t.Fatalf("ResolveEnvFiles: %v", err)
	}
	wantEnv(t, environment(p.Services["web"]), map[string]string{"A": "1", "B": "7", "C": ""})
}

func TestResolveEnvFilesNeverReadsTheServersEnvironment(t *testing.T) {
	// Not parallel: t.Setenv changes the test process's environment.
	t.Setenv("TIERPLAN_ENVFILE_PROBE", "leaked")
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file: app.env\n"
	p, err := resolve(t, stackWith(compose, nil, file("app.env", "P=${TIERPLAN_ENVFILE_PROBE}\nQ=${TIERPLAN_ENVFILE_PROBE:-fallback}\n")))
	if err != nil {
		t.Fatalf("ResolveEnvFiles: %v", err)
	}
	wantEnv(t, environment(p.Services["web"]), map[string]string{"P": "", "Q": "fallback"})
}

func TestResolveEnvFilesLetsALaterFileWin(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file:\n      - a.env\n      - path: b.env\n        required: true\n"
	p, err := resolve(t, stackWith(compose, nil, file("a.env", "A=a\nONLY_A=1\n"), file("b.env", "A=b\n")))
	if err != nil {
		t.Fatalf("ResolveEnvFiles: %v", err)
	}
	wantEnv(t, environment(p.Services["web"]), map[string]string{"A": "b", "ONLY_A": "1"})
}

func TestResolveEnvFilesReadsQuotesCommentsAndExport(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file: app.env\n"
	p, err := resolve(t, stackWith(compose, nil, file("app.env", "# a comment\nexport A=1\nB='x y'\nC=\"two\\nlines\"\nD=e # trailing\n\n")))
	if err != nil {
		t.Fatalf("ResolveEnvFiles: %v", err)
	}
	wantEnv(t, environment(p.Services["web"]), map[string]string{"A": "1", "B": "x y", "C": "two\nlines", "D": "e"})
}

func TestResolveEnvFilesSkipsAMissingOptionalFileAndRefusesAMissingRequiredOne(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27\n    env_file:\n"
	p, err := resolve(t, stackWith(head+"      - path: opt.env\n        required: false\n", nil))
	if err != nil {
		t.Fatalf("an optional file that is not there: %v", err)
	}
	wantEnv(t, environment(p.Services["web"]), map[string]string{})
	if n := len(p.Services["web"].EnvFiles); n != 0 {
		t.Errorf("EnvFiles = %d entries, want none", n)
	}

	_, err = resolve(t, stackWith(head+"      - gone.env\n", nil))
	var ef *stacks.EnvFileError
	if !errors.Is(err, stacks.ErrEnvFileMissing) || !errors.As(err, &ef) || ef.Service != "web" || ef.Path != "gone.env" {
		t.Errorf("a required file that is not there: err = %v, want ErrEnvFileMissing for web gone.env", err)
	}
}

func TestResolveEnvFilesOnlyLooksInTheStacksFiles(t *testing.T) {
	t.Parallel()
	// envfile_test.go and go.mod exist next to the test; the stack does not have them.
	for _, path := range []string{"envfile_test.go", "../../go.mod", "/etc/hostname"} {
		compose := "services:\n  web:\n    image: nginx:1.27\n    env_file: " + path + "\n"
		_, err := resolve(t, stackWith(compose, nil))
		if !errors.Is(err, stacks.ErrEnvFileMissing) {
			t.Errorf("env_file %s: err = %v, want ErrEnvFileMissing", path, err)
		}
	}
}

func TestResolveEnvFilesRefusesALineItCannotReadWithoutQuotingIt(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file: app.env\n"
	_, err := resolve(t, stackWith(compose, nil, file("app.env", "A=1\nB=\"hunter2-never-closed\n")))
	var ef *stacks.EnvFileError
	if !errors.Is(err, stacks.ErrEnvFileInvalid) || !errors.As(err, &ef) {
		t.Fatalf("err = %v, want ErrEnvFileInvalid", err)
	}
	if ef.Line != 2 || ef.Path != "app.env" {
		t.Errorf("error = %+v, want line 2 of app.env", ef)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error carries a value of the file: %q", err)
	}
}

func TestResolveEnvFilesRefusesAFormatItDoesNotKnow(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file:\n      - path: app.env\n        format: unknown\n"
	_, err := resolve(t, stackWith(compose, nil, file("app.env", "A=1\n")))
	if !errors.Is(err, stacks.ErrEnvFileInvalid) {
		t.Errorf("err = %v, want ErrEnvFileInvalid", err)
	}
}

func TestResolveEnvFilesLeavesNoEnvFileOnAnyServiceEvenWhenOneFails(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  a:\n    image: a:1\n    env_file: ok.env\n  b:\n    image: b:1\n    env_file: gone.env\n  c:\n    image: c:1\n"
	p, err := resolve(t, stackWith(compose, nil, file("ok.env", "K=v\n")))
	if err == nil {
		t.Fatal("a missing required file was not reported")
	}
	for name, svc := range p.Services {
		if len(svc.EnvFiles) != 0 {
			t.Errorf("service %s still has %d env_file entries", name, len(svc.EnvFiles))
		}
	}
	wantEnv(t, environment(p.Services["a"]), map[string]string{"K": "v"})
}

// Check calls it, for both targets, and what comes out converts for a swarm.

func TestCheckResolvesEnvFileFromTheStacksFilesOnBothTargets(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file: app.env\n    environment:\n      B: explicit\n"
	wc := stackWith(compose, []stacks.Var{{Name: "X", Value: "7"}}, file("app.env", "A=${X}\nB=file\n"))
	var envs []map[string]string
	for _, target := range []stacks.Target{stacks.TargetSwarm, stacks.TargetStandalone} {
		res := stacks.Check(context.Background(), stacks.CheckInput{Name: "probe", Target: target, Copy: wc})
		if res.Problems.Errors() != 0 || res.Loaded == nil {
			t.Fatalf("%s: problems = %+v loaded = %v", target, res.Problems, res.Loaded)
		}
		svc := res.Loaded.Project.Services["web"]
		if len(svc.EnvFiles) != 0 {
			t.Errorf("%s: EnvFiles = %+v, want none on the project handed to a deploy", target, svc.EnvFiles)
		}
		envs = append(envs, environment(svc))
	}
	wantEnv(t, envs[0], map[string]string{"A": "7", "B": "explicit"})
	wantEnv(t, envs[1], envs[0])
}

func TestAProjectCheckResolvedConvertsForASwarm(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file: app.env\n"
	res := stacks.Check(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetSwarm, Copy: stackWith(compose, nil, file("app.env", "A=1\n")),
	})
	if res.Loaded == nil {
		t.Fatalf("problems = %+v", res.Problems)
	}
	plan, err := convert.Convert(res.Loaded.Project, convert.Options{StackName: "probe", StackID: "id-probe"})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got := plan.Services[0].Spec.TaskTemplate.ContainerSpec.Env; len(got) != 1 || got[0] != "A=1" {
		t.Errorf("the service environment = %v, want [A=1]", got)
	}
}

func TestCheckAcceptsAMissingOptionalEnvFile(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file:\n      - path: opt.env\n        required: false\n"
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "probe", Target: stacks.TargetSwarm, Copy: stackWith(compose, nil)})
	if len(res.Problems) != 0 || res.Loaded == nil {
		t.Errorf("problems = %+v loaded = %v, want a clean check", res.Problems, res.Loaded)
	}
	if res.Loaded != nil && len(res.Loaded.Project.Services["web"].EnvFiles) != 0 {
		t.Error("EnvFiles was not emptied")
	}
}

func TestCheckStillRefusesAMissingRequiredEnvFile(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file:\n      - path: gone.env\n        required: true\n"
	res := stacks.Check(context.Background(), stacks.CheckInput{Name: "probe", Target: stacks.TargetSwarm, Copy: stackWith(compose, nil)})
	if p := onlyProblem(t, res); p.Code != stacks.CodeFileMissing || p.Severity != stacks.SeverityError {
		t.Errorf("problem = %+v, want an error file_missing", p)
	}
}

func TestCheckPutsAnUnreadableEnvFileAtItsKeyWithoutQuotingIt(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file: app.env\n"
	res := stacks.Check(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetSwarm, Copy: stackWith(compose, nil, file("app.env", "A=1\nB=\"hunter2-never-closed\n")),
	})
	got := problemsOf(res, stacks.CodeEnvFileInvalid)
	if len(got) != 1 {
		t.Fatalf("problems = %+v, want one env_file_invalid", res.Problems)
	}
	p := got[0]
	if p.Severity != stacks.SeverityError || p.File != stacks.ComposeFile || p.Line != 4 {
		t.Errorf("problem = %+v, want an error at line 4 of compose.yaml", p)
	}
	if want := "env_file “app.env” has a line TierPlan can't read (line 2). Write each line as NAME=value."; p.Message != want {
		t.Errorf("message = %q, want %q", p.Message, want)
	}
	if strings.Contains(p.Message, "hunter2") {
		t.Errorf("the message carries a value: %q", p.Message)
	}
}

// A .env file in the stack is not read by anything on its own.

func TestCheckWarnsOfADotenvFileNothingUses(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n"
	res := stacks.Check(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetSwarm, Copy: stackWith(compose, nil, file(".env", "A=1\n")),
	})
	p := onlyProblem(t, res)
	if p.Code != stacks.CodeDotenvNotRead || p.Severity != stacks.SeverityWarning || p.File != ".env" || p.Line != 1 {
		t.Errorf("problem = %+v, want a warning dotenv_not_read on line 1 of .env", p)
	}
	if want := ".env isn't read automatically. Import it into Variables, or use it as an env_file."; p.Message != want {
		t.Errorf("message = %q, want %q", p.Message, want)
	}
	if res.Problems.Errors() != 0 || res.Loaded == nil {
		t.Error("the warning must not block")
	}
}

func TestCheckDoesNotWarnOfADotenvFileAnEnvFileUses(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27\n    env_file: .env\n"
	res := stacks.Check(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetSwarm, Copy: stackWith(compose, nil, file(".env", "A=1\n")),
	})
	if len(res.Problems) != 0 {
		t.Errorf("problems = %+v, want none", res.Problems)
	}
	// Another unused file is listed as unused, but only .env is told it is not read.
	res = stacks.Check(context.Background(), stacks.CheckInput{
		Name: "probe", Target: stacks.TargetSwarm, Copy: stackWith("services:\n  web:\n    image: nginx:1.27\n", nil, file("other.env", "A=1\n")),
	})
	if got := problemsOf(res, stacks.CodeDotenvNotRead); len(got) != 0 {
		t.Errorf("other.env got %+v", got)
	}
}
