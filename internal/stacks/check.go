// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/bayplan/internal/stacks/lint"
)

// CheckInput is what Check needs: the stack's name and target, and the working copy to check. It
// has the fields of LoadInput, in the same order, because a check starts with a load.
type CheckInput struct {
	Name   string
	Target Target
	Copy   WorkingCopy
	// Deadline bounds the check. Zero is five seconds.
	Deadline time.Duration
}

// CheckResult is what a check found. Loaded is set when the compose file loaded, and is what the
// review and the deploy convert; Problems holds everything that was found, errors and warnings.
type CheckResult struct {
	Problems Problems
	Loaded   *Loaded
	// UnusedFiles are the stack's files the compose file never refers to. compose.yaml is never
	// one of them.
	UnusedFiles []string
	// Variables are the rows of the Variables pane: every variable the compose file refers to and
	// every variable of the table, with the value a deploy will use and where it comes from. It is
	// empty when the compose file is not valid YAML or was refused before it was read.
	Variables []ResolvedVar
	// SecretFiles are the stack files that are secret: the ones the compose file uses under
	// secrets:, and the ones already flagged. Sorted. It is set whenever the compose file is
	// valid YAML.
	SecretFiles []string
}

// Check is the one pipeline that judges a stack's working copy: the editor's lint, the review and
// the deploy all call it, so what a person sees while they type is what the deploy will hold them
// to. It is pure: it reads only the bytes it is given, opens no file and calls no engine.
func Check(ctx context.Context, in CheckInput) CheckResult {
	res := check(ctx, in)
	// No secret reaches an answer: the sentences of the problems and the values of the rows are
	// passed through the stack's masker whatever produced them (RESEARCH Pitfall 10). The loaded
	// project is not an answer; it is what a deploy reads, and it keeps the real values.
	m := NewMasker(in.Copy.Vars, in.Copy.MaskFiles())
	MaskProblems(res.Problems, m)
	for i := range res.Variables {
		res.Variables[i].Default = m.Mask(res.Variables[i].Default)
		if !res.Variables[i].Secret {
			res.Variables[i].Value = m.Mask(res.Variables[i].Value)
		}
	}
	return res
}

// MaskProblems passes the Message and the Key of every problem through m, in place. It is the one
// way a problem list is masked before it leaves a pipeline: Check and Deployable both use it, so no
// answer carries a value of the stack.
func MaskProblems(ps Problems, m Masker) {
	for i := range ps {
		ps[i].Message = m.Mask(ps[i].Message)
		ps[i].Key = m.Mask(ps[i].Key)
	}
}

func check(ctx context.Context, in CheckInput) CheckResult {
	var res CheckResult
	if in.Deadline <= 0 {
		in.Deadline = checkTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, in.Deadline)
	defer cancel()
	contents := in.Copy.ComposeContents()
	file := contents[0].Path
	res.SecretFiles = secretFilePaths(in.Copy)
	// The committed .env of a stack from a repository is read once here and layered under the table
	// (D-07). An unreadable one is reported below, before the load.
	repoEnv, repoEnvErr := RepoEnv(in.Copy)
	gitCopy := hasRepoFiles(in.Copy.Files)
	if ps := composeFileProblems(in.Copy, contents); len(ps) > 0 {
		res.Problems = ps.Sorted()
		return res
	}
	idx, parseErr := parseIndexOf(contents)
	if parseErr == nil {
		// The raw YAML is judged before the loader sees it: a file that names a file of the
		// server, or that is too complex to read, is refused here and never loaded.
		found, refs := PrescanFiles(contents, in.Copy.multiFile(), NewFileSet(in.Copy.Files))
		if _, ok := repoDotEnv(in.Copy.Files); ok {
			// A .env the repository holds is read (D-07), so it is not unused.
			refs = append(refs, DotEnvFile)
		}
		res.UnusedFiles = unusedFiles(in.Copy.Files, in.Copy.ComposePaths(), refs)
		if found.Errors() > 0 {
			res.Problems = found.Sorted()
			return res
		}
		res.Problems = found
		if slices.Contains(res.UnusedFiles, dotenvFile) {
			res.Problems = append(res.Problems, dotenvNotRead())
		}
		res.Variables = ResolveVarsLayered(Refs(in.Copy), in.Copy.Vars, repoEnv)
		res.Problems = append(res.Problems, variableProblems(res.Variables, file)...)
	}
	if repoEnvErr != nil {
		res.Problems = append(res.Problems, repoEnvProblem(repoEnvErr))
		res.Problems = res.Problems.Sorted()
		return res
	}
	loaded, err := Load(ctx, LoadInput(in))
	if err != nil {
		// compose-go says where a syntax error is better than the parser used for positions does
		// (it names the line the parser stopped at, not the line the block started on), so its
		// text is used whenever it refused the file too.
		// Only a refused file pays for knowing which values come from a secret variable.
		res.Problems = append(res.Problems, loadProblems(err, idx, secretUseOf(idx, in.Copy.Vars), in.Copy.ComposePaths())...).Sorted()
		return res
	}
	if parseErr != nil {
		// compose-go accepted what the position parser did not: report the parser's error, and
		// do not hand a file whose keys cannot be located on to the later checks.
		bad := file
		var ie *indexError
		if errors.As(parseErr, &ie) {
			bad = ie.File
		}
		res.Problems = yamlProblems(parseErr.Error(), bad).Sorted()
		return res
	}
	binds := relativeBinds(loaded.Project, idx, in.Copy.Vars)
	if err := assertSandboxed(loaded.Project, NewFileSet(in.Copy.Files)); err != nil && (!errors.Is(err, errRelativeBind) || len(binds) == 0) {
		// Nothing the prescan lets through should fail here. If something does, the project is
		// withheld and the person is told it is a Bayplan bug, not their file.
		res.Problems = Problems{internalCheckFailed(file)}
		return res
	}
	res.Problems = append(res.Problems, unsetProblems(loaded.Unset, res.Variables, res.Problems, file)...)
	// env_file is read here, once, from the stack's own files (D-05, Pitfall 4): after this the
	// project has none, so no engine, SDK or converter reads one a second way. An unreadable file
	// is a blocking error at its key; the rest of the check goes on without it.
	if err := ResolveEnvFiles(loaded.Project, NewFileSet(in.Copy.Files), layeredValues(in.Copy.Vars, repoEnv)); err != nil {
		res.Problems = append(res.Problems, envFileProblem(err, idx))
	}
	if in.Target == TargetStandalone {
		res.Problems = append(res.Problems, oversizeObjects(loaded.Project, NewFileSet(in.Copy.Files), in.Copy.Vars, idx)...)
	}
	// What the target will ignore or refuse is judged on the project as loaded, with build still
	// in it: that is how a build key is seen.
	found := lint.Lint(lint.Input{Target: lint.Target(in.Target), Project: loaded.Project, Index: idx})
	// Bayplan deploys images and never builds one (A5): build leaves the project here, before any
	// engine or any later step can see it. A service with a build and no image is a blocking error
	// above, so what is left to deploy always names an image.
	stripBuild(loaded.Project)
	res.Loaded = loaded
	res.Problems = append(append(res.Problems, binds...), lintProblems(found, file)...)
	if gitCopy {
		res.Problems = append(res.Problems, GitLint(in.Copy, loaded)...)
	}
	if in.Target == TargetSwarm {
		res.Problems = append(res.Problems, swarmNameProblems(loaded.Project, idx, in.Name)...)
	}
	res.Problems = res.Problems.Sorted()
	return res
}

// composeFileProblems are the blocking problems of the compose files themselves, found before any is
// read: one over MaxFileBytes, and, for a stack with named compose files, one the working copy does not
// hold or that is empty. A stack with the one default compose.yaml keeps its earlier behaviour: a
// missing or empty file is the loader's to report.
func composeFileProblems(c WorkingCopy, contents []ComposeContent) Problems {
	var out Problems
	have := map[string]struct{}{}
	for _, f := range c.Files {
		have[f.Path] = struct{}{}
	}
	for _, cf := range contents {
		switch {
		case len(cf.Content) > MaxFileBytes:
			out = append(out, tooLargeProblem(cf.Path))
		case !c.multiFile():
		case !fileIn(have, cf.Path):
			out = append(out, Problem{
				Severity: SeverityError, Code: CodeFileMissing, File: cf.Path, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
				Message: fmt.Sprintf("The compose file “%s” isn't one of this stack's files. Add it under Files.", cf.Path),
			})
		case len(bytes.TrimSpace(cf.Content)) == 0:
			out = append(out, Problem{
				Severity: SeverityError, Code: CodeComposeInvalid, File: cf.Path, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
				Message: cf.Path + " is empty. Add a services section.",
			})
		}
	}
	return out
}

func fileIn(have map[string]struct{}, path string) bool {
	_, ok := have[path]
	return ok
}

// swarmNameProblems reports each service and each network a swarm stack would create whose engine name swarm
// refuses (swarmkit validateAnnotations: letters, digits, "-" and "_" with no dot, at most 63 bytes). Swarm names
// a stack's services <stack>_<key>, and its networks by the name the file gives or <stack>_<key>; a deploy with
// such a name would be refused by swarm part-way, so it stops here, before any engine call (D-48 object
// prefixes, D-53). Every name the stack-name rule takes can hold a short service key, and a name swarm takes today
// is never reported, so no deploy that works today is stopped. External networks are only referenced and never
// created, so they are not checked.
func swarmNameProblems(project *types.Project, ix *PositionIndex, stack string) Problems {
	var out Problems
	report := func(kind, section, key, name string, n *yaml.Node, file string) {
		line, col, endLine, endCol := at(n)
		out = append(out, Problem{
			Severity: SeverityError, Code: CodeSwarmNameInvalid, File: file, Key: section + "." + key,
			Line: line, Col: col, EndLine: endLine, EndCol: endCol,
			Message: fmt.Sprintf("Swarm can't create a %s named “%s”: swarm allows only letters, numbers, dash and underscore in service and network names, up to 63 characters.", kind, name),
		})
	}
	services := make([]string, 0, len(project.Services))
	for key := range project.Services {
		services = append(services, key)
	}
	sort.Strings(services)
	used := map[string]bool{}
	for _, key := range services {
		if name := stack + "_" + key; !SwarmObjectNameValid(name) {
			n, file := ix.locateAny("services." + key)
			report("service", "services", key, name, n, file)
		}
		svc := project.Services[key]
		if len(svc.Networks) == 0 {
			used["default"] = true
		}
		for nk := range svc.Networks {
			used[nk] = true
		}
	}
	networks := make([]string, 0, len(used))
	for key := range used {
		networks = append(networks, key)
	}
	sort.Strings(networks)
	for _, key := range networks {
		name := stack + "_" + key
		decl, declared := project.Networks[key]
		if declared {
			if bool(decl.External) {
				continue
			}
			if decl.Name != "" {
				name = decl.Name
			}
		}
		if !SwarmObjectNameValid(name) {
			n, file := ix.locateAny("networks."+key+".name", "networks."+key, "services")
			report("network", "networks", key, name, n, file)
		}
	}
	return out
}

// secretFilePaths are the paths of the files that are secret in a working copy, sorted.
func secretFilePaths(c WorkingCopy) []string {
	var out []string
	for _, f := range MarkSecretFilesFor(c.Files, nil, c.ComposeFiles) {
		if f.Secret {
			out = append(out, f.Path)
		}
	}
	slices.Sort(out)
	return out
}

// dotenvFile is the name of the file a Compose project directory reads on its own, and Bayplan
// does not.
const dotenvFile = DotEnvFile

// dotenvNotRead is the warning for a stack file called .env that nothing uses. Compose reads a .env
// beside the compose file by itself; Bayplan reads variables from the table only (D-01), so a
// person who brought one over should know it does nothing as it stands.
func dotenvNotRead() Problem {
	return Problem{
		Severity: SeverityWarning, Code: CodeDotenvNotRead, File: dotenvFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: ".env isn't read automatically. Import it into Variables, or use it as an env_file.",
	}
}

// tableValues is the lookup an env_file's ${NAME} uses: the table's values, a blank one being no
// value, as in Load.
func tableValues(vars []Var) func(string) (string, bool) {
	table := make(map[string]string, len(vars))
	for _, v := range vars {
		if v.Value != "" {
			table[v.Name] = v.Value
		}
	}
	return func(name string) (string, bool) {
		v, ok := table[name]
		return v, ok
	}
}

// layeredValues is tableValues with the repository's .env under the table: the lookup an env_file's
// ${NAME} uses in a stack from a repository, in the order the compose files are interpolated.
func layeredValues(vars []Var, repoEnv map[string]string) func(string) (string, bool) {
	table := tableValues(vars)
	return func(name string) (string, bool) {
		if v, ok := table(name); ok {
			return v, true
		}
		v, ok := repoEnv[name]
		return v, ok && v != ""
	}
}

// hasRepoFiles reports whether any of the files came from a git repository, which is what makes the
// working copy a git stack for the rules of GitLint.
func hasRepoFiles(files []File) bool {
	for _, f := range files {
		if f.Origin == OriginRepo {
			return true
		}
	}
	return false
}

// repoEnvProblem is the blocking error for a repository .env that cannot be read. It names the file and,
// when known, the line, and never quotes the file.
func repoEnvProblem(err error) Problem {
	p := Problem{
		Severity: SeverityError, Code: CodeEnvFileInvalid, File: DotEnvFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: ".env can't be read. Write each line as NAME=value.",
	}
	var ef *EnvFileError
	if errors.As(err, &ef) && ef.Line > 0 {
		p.Line, p.EndLine = ef.Line, ef.Line
		p.Message = fmt.Sprintf(".env has a line Bayplan can't read (line %d). Write each line as NAME=value.", ef.Line)
	}
	if errors.Is(err, ErrFileTooLarge) {
		p.Code = CodeComposeTooLarge
		p.Message = ".env is over 1 MiB. Split it up or remove what it doesn't need."
	}
	return p
}

// envFileProblem is the blocking error for an env_file ResolveEnvFiles could not resolve, marked at
// the service's env_file key. It names the file and, for an unreadable line, its number, and never
// quotes the file.
func envFileProblem(err error, ix *PositionIndex) Problem {
	var ef *EnvFileError
	if !errors.As(err, &ef) {
		return Problem{
			Severity: SeverityError, Code: CodeEnvFileInvalid, File: ix.defaultFile(), Line: 1, Col: 1, EndLine: 1, EndCol: 2,
			Message: "An env_file can't be read. Write each line as NAME=value.",
		}
	}
	n, file := ix.locateAny("services."+ef.Service+".env_file", "services."+ef.Service)
	line, col, endLine, endCol := at(n)
	p := Problem{
		Severity: SeverityError, Code: CodeEnvFileInvalid, File: file, Key: "services." + ef.Service + ".env_file",
		Line: line, Col: col, EndLine: endLine, EndCol: endCol,
	}
	switch {
	case errors.Is(err, ErrEnvFileMissing):
		p.Code = CodeFileMissing
		p.Message = fmt.Sprintf("env_file “%s” isn't one of this stack's files. Add it under Files.", ef.Path)
	case ef.Format != "":
		p.Message = fmt.Sprintf("env_file “%s” asks for the “%s” format, which Bayplan can't read. Remove format to use the .env format.", ef.Path, ef.Format)
	case ef.Line > 0:
		p.Message = fmt.Sprintf("env_file “%s” has a line Bayplan can't read (line %d). Write each line as NAME=value.", ef.Path, ef.Line)
	default:
		p.Message = fmt.Sprintf("env_file “%s” can't be read. Write each line as NAME=value.", ef.Path)
	}
	return p
}

// variableProblems are what the rows of the Variables pane say is wrong with the files. A variable
// that has no value in the table and no default is a blocking error at every place it is used
// plainly (STK-03, Pitfall 3); a reference with a default or a ":?" message has its own answer. A
// variable of the table that no file refers to is a warning that never blocks (D-03).
func variableProblems(vars []ResolvedVar, file string) Problems {
	var out Problems
	for _, v := range vars {
		switch {
		case v.Unused:
			out = append(out, Problem{
				Severity: SeverityWarning, Code: CodeUnusedVariable, File: file, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
				Message: v.Name + " is in Variables but the compose file doesn't use it.",
			})
		case v.Required:
			for _, pl := range v.Places {
				if pl.Plain {
					out = append(out, unsetProblem(v.Name, pl))
				}
			}
		}
	}
	return out
}

// unsetProblem is the blocking error for a variable with no value and no default used at pl.
func unsetProblem(name string, pl Place) Problem {
	col := pl.Col
	if col < 1 {
		col = 1
	}
	return Problem{
		Severity: SeverityError, Code: CodeUnsetVariable, File: pl.File, Line: pl.Line, Col: col, EndLine: pl.Line, EndCol: col + 1,
		Message: name + " has no value and no default. Add a value in Variables.",
	}
}

// unsetProblems adds an error for each name the load found unset that the rows did not already
// flag, so a plain reference the position scan could not place is still refused. It is placed at
// the first place the name is used, or at line 1.
func unsetProblems(unset []string, vars []ResolvedVar, have Problems, file string) Problems {
	flagged := map[string]bool{}
	for _, p := range have {
		if p.Code == CodeUnsetVariable {
			if name, _, ok := strings.Cut(p.Message, " has no value"); ok {
				flagged[name] = true
			}
		}
	}
	places := map[string][]Place{}
	for _, v := range vars {
		places[v.Name] = v.Places
	}
	var out Problems
	for _, name := range unset {
		if flagged[name] {
			continue
		}
		pl := Place{File: file, Line: 1, Col: 1}
		if list := places[name]; len(list) > 0 {
			pl = list[0]
		}
		out = append(out, unsetProblem(name, pl))
	}
	return out
}

// stripBuild removes the build section of every service of the project.
func stripBuild(project *types.Project) {
	for name, svc := range project.Services {
		if svc.Build != nil {
			svc.Build = nil
			project.Services[name] = svc
		}
	}
}

// oversizeObjects reports a secret or config of a standalone stack whose content is over what the agent's
// archive route writes into a container (MaxStandaloneFileBytes), at the object's key in the file. It
// reports no other trouble with an object: the prescan and the review do.
func oversizeObjects(p *types.Project, files FileSet, vars []Var, ix *PositionIndex) Problems {
	table := make(map[string]string, len(vars))
	for _, v := range vars {
		table[v.Name] = v.Value
	}
	var out Problems
	for _, section := range []struct {
		word  string
		names []string
		kind  ObjectKind
	}{{"secrets", sortedNames(p.Secrets), KindSecret}, {"configs", sortedNames(p.Configs), KindConfig}} {
		used := map[string]bool{}
		for _, svc := range p.Services {
			if section.kind == KindSecret {
				for _, s := range svc.Secrets {
					used[s.Source] = true
				}
			} else {
				for _, c := range svc.Configs {
					used[c.Source] = true
				}
			}
		}
		for _, name := range section.names {
			if !used[name] {
				continue
			}
			one := &types.Project{Name: p.Name, Services: p.Services}
			if section.kind == KindSecret {
				one.Secrets = types.Secrets{name: p.Secrets[name]}
			} else {
				one.Configs = types.Configs{name: p.Configs[name]}
			}
			if _, err := ResolveObjectsFor(TargetStandalone, one, files, table); err != nil && errors.Is(err, ErrObjectTooLarge) {
				out = append(out, keyProblem(ix, CodeObjectTooLarge, section.word+"."+name, fmt.Sprintf(
					"The %s %s is over 1,000 KB, the most a standalone host can write into a container. Use a smaller file.", section.kind, name)))
			}
		}
	}
	return out
}

// lintProblems turns lint's findings into problems of the compose file each is in; file is where a
// finding with no position of its own goes.
func lintProblems(found []lint.Finding, file string) Problems {
	out := make(Problems, 0, len(found))
	for _, f := range found {
		at := file
		if f.Pos.File != "" {
			at = f.Pos.File
		}
		out = append(out, Problem{
			Severity: Severity(f.Severity), Code: f.Code, File: at, Key: f.Key, Message: f.Message,
			Line: f.Pos.Line, Col: f.Pos.Col, EndLine: f.Pos.EndLine, EndCol: f.Pos.EndCol,
		})
	}
	return out
}

// checkTimeout bounds one check when the caller names no deadline.
const checkTimeout = 5 * time.Second

var relativeBindWording = "“%s” is a relative bind mount. Use a named volume, or an absolute path on the host (which needs Admin)."

// relativeBinds reports every bind mount whose source is not an absolute path (D-06): the stack has
// no directory for a relative path to be relative to, so it would resolve against the host the
// engine runs on. compose-go's short-syntax parse tells a bind from a named volume, so "./data" is
// found and "data" with a volume of that name is not. Both targets are held to it.
//
// A bind whose source comes from a secret variable keeps its key and position, but its message is
// the withheld sentence: the loader's sentence quotes the source (D-02). svc.Volumes[i] is a position
// in compose-go's merged list, not an item of the file, so a bind is judged by its service's whole
// volumes list across every document (SecretUse.Withholds, through listPath). A withheld bind says the
// file uses a secret variable, and its marker line from ix.findAny may sit on a neighbouring item of
// the same list; the marker carries no text.
func relativeBinds(project *types.Project, ix *PositionIndex, vars []Var) Problems {
	var out Problems
	var s SecretUse
	built := false
	for name, svc := range project.Services {
		for i, v := range svc.Volumes {
			if v.Type != types.VolumeTypeBind || v.Source == "" || strings.HasPrefix(v.Source, "/") {
				continue
			}
			if !built {
				// Only a stack with a relative bind pays for knowing which values come from a secret.
				s, built = secretUseOf(ix, vars), true
			}
			path := "services." + name + ".volumes." + strconv.Itoa(i)
			n, file := ix.locateAny(path, "services."+name+".volumes", "services."+name)
			p := relativeBindAt(n, file, "services."+name+".volumes", v.Source)
			if s.Withholds(path) {
				p.Message = s.WithheldMessage(path)
			}
			out = append(out, p)
		}
	}
	return out
}

// relativeBindAt is the D-06 error for a bind with this source, marked at n (line 1 when n is nil).
func relativeBindAt(n *yaml.Node, file, key, source string) Problem {
	line, col, endLine, endCol := at(n)
	return Problem{
		Severity: SeverityError, Code: CodeRelativeBind, File: file, Key: key, Message: fmt.Sprintf(relativeBindWording, source),
		Line: line, Col: col, EndLine: endLine, EndCol: endCol,
	}
}

// ErrNotSandboxed means a loaded project holds something the prescan should have refused.
var ErrNotSandboxed = errors.New("stacks: the loaded project is not sandboxed")

// errRelativeBind is the violation of a relative bind mount, which Check reports with its own
// wording at its line.
var errRelativeBind = fmt.Errorf("%w: a relative bind mount", ErrNotSandboxed)

// assertSandboxed is the defence in depth after the load (RESEARCH Q5, layer 3): the prescan has
// already refused every one of these, so a violation here means the prescan has a gap. It returns
// an error wrapping ErrNotSandboxed for a service that extends another, one with a label file, an
// env_file that is not a file of the stack, a secret or config whose file is not one, and, last, a
// relative bind mount.
func assertSandboxed(project *types.Project, files FileSet) error {
	ours := func(path string) bool {
		_, ok := files[strings.TrimPrefix(path, "./")]
		return ok && ValidPath(strings.TrimPrefix(path, "./")) == nil
	}
	var binds bool
	for name, svc := range project.Services {
		if svc.Extends != nil {
			return fmt.Errorf("%w: service %s extends another", ErrNotSandboxed, name)
		}
		if len(svc.LabelFiles) > 0 {
			return fmt.Errorf("%w: service %s has a label_file", ErrNotSandboxed, name)
		}
		for _, f := range svc.EnvFiles {
			// A required env_file must be one of the stack's files. An optional one may be absent
			// (ResolveEnvFiles skips it and opens nothing), but its path must still be a path
			// inside the stack.
			if !ours(f.Path) && (bool(f.Required) || ValidPath(strings.TrimPrefix(f.Path, "./")) != nil) {
				return fmt.Errorf("%w: service %s has an env_file that is not a file of the stack", ErrNotSandboxed, name)
			}
		}
		for _, v := range svc.Volumes {
			if v.Type == types.VolumeTypeBind && v.Source != "" && !strings.HasPrefix(v.Source, "/") {
				binds = true
			}
		}
	}
	for name, s := range project.Secrets {
		if s.File != "" && !ours(s.File) {
			return fmt.Errorf("%w: secret %s has a file that is not a file of the stack", ErrNotSandboxed, name)
		}
	}
	for name, c := range project.Configs {
		if c.File != "" && !ours(c.File) {
			return fmt.Errorf("%w: config %s has a file that is not a file of the stack", ErrNotSandboxed, name)
		}
	}
	if binds {
		return errRelativeBind
	}
	return nil
}

// unusedFiles are the paths of files that refs does not name, sorted. The compose files are the files
// everything else is referred to from, so none of them is ever unused.
func unusedFiles(files []File, composePaths []string, refs []string) []string {
	used := make(map[string]struct{}, len(refs)+len(composePaths))
	for _, p := range composePaths {
		used[p] = struct{}{}
	}
	for _, r := range refs {
		used[r] = struct{}{}
	}
	out := []string{}
	for _, f := range files {
		if _, ok := used[f.Path]; !ok {
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out
}

// PositionIndex is the positions of a compose file's keys, read from the stored bytes. It is the only
// place a line number comes from: a position is never taken from a file on the server.
//
// An index over several compose files holds the documents of every file, in merge order, and knows
// which file each document came from, so a position is reported in the file that holds it.
type PositionIndex struct {
	docs []*yaml.Node
	// files is the path of the compose file each document came from, parallel to docs. It is nil for
	// an index over a single compose.yaml.
	files []string
	// def is the file to report a problem in when no key names one; empty is ComposeFile.
	def string
}

// defaultFile is the compose file a problem with no key of its own is reported in.
func (ix *PositionIndex) defaultFile() string {
	if ix == nil || ix.def == "" {
		return ComposeFile
	}
	return ix.def
}

// fileAt is the compose file document i came from.
func (ix *PositionIndex) fileAt(i int) string {
	if ix.files == nil {
		return ix.defaultFile()
	}
	return ix.files[i]
}

// order is the order documents are searched in for a position. In an index over several files the
// last file is first: a later file wins in the merge, so a key it sets is where the effective value
// was written. A single file keeps the order of its documents.
func (ix *PositionIndex) order() []int {
	out := make([]int, len(ix.docs))
	for i := range out {
		if ix.files != nil {
			out[i] = len(ix.docs) - 1 - i
		} else {
			out[i] = i
		}
	}
	return out
}

// forFile is the index of the one compose file path, for a problem that compose-go reported about
// that file alone. An index over a single file is returned as it is.
func (ix *PositionIndex) forFile(path string) *PositionIndex {
	if ix == nil || ix.files == nil {
		return ix
	}
	sub := &PositionIndex{def: path}
	for i, d := range ix.docs {
		if ix.files[i] == path {
			sub.docs = append(sub.docs, d)
			sub.files = append(sub.files, path)
		}
	}
	return sub
}

// indexError is a compose file that is not YAML: the file and the parser's error. Its text is the
// parser's, so yamlProblems reads it as it reads a single file's.
type indexError struct {
	File string
	Err  error
}

func (e *indexError) Error() string { return e.Err.Error() }

// Unwrap returns the parser's error.
func (e *indexError) Unwrap() error { return e.Err }

// parseIndex reads every YAML document of src. compose-go merges every document of a file, so
// every document is indexed.
func parseIndex(src []byte) (*PositionIndex, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	ix := &PositionIndex{}
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return ix, nil
		}
		if err != nil {
			return nil, err
		}
		if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
			ix.docs = append(ix.docs, doc.Content[0])
		}
	}
}

// parseIndexOf indexes the compose files of a working copy, in merge order. A single compose.yaml
// gives the same index as parseIndex. A file that is not YAML is an *indexError naming it.
func parseIndexOf(contents []ComposeContent) (*PositionIndex, error) {
	if len(contents) == 1 && contents[0].Path == ComposeFile {
		return parseIndex(contents[0].Content)
	}
	ix := &PositionIndex{files: []string{}}
	if len(contents) > 0 {
		ix.def = contents[0].Path
	}
	for _, c := range contents {
		one, err := parseIndex(c.Content)
		if err != nil {
			return nil, &indexError{File: c.Path, Err: err}
		}
		ix.docs = append(ix.docs, one.docs...)
		for range one.docs {
			ix.files = append(ix.files, c.Path)
		}
	}
	return ix, nil
}

// locateAny is the first node any of the paths names, with the compose file it is in.
func (ix *PositionIndex) locateAny(paths ...string) (*yaml.Node, string) {
	for _, p := range paths {
		if n, file := ix.locate(p); n != nil {
			return n, file
		}
	}
	return nil, ix.defaultFile()
}

// shortVolume finds the short-syntax entry ("name:/target") of a service's volumes that uses a
// named volume source, with the compose file it is in, or nil.
func (ix *PositionIndex) shortVolume(service, source string) (*yaml.Node, string) {
	if ix == nil {
		return nil, ComposeFile
	}
	for _, i := range ix.order() {
		root := ix.docs[i]
		svc := mapValue(mapValue(root, "services"), service)
		list := mapValue(svc, "volumes")
		if list == nil || list.Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range list.Content {
			if item.Kind == yaml.ScalarNode && strings.HasPrefix(item.Value, source+":") {
				return item, ix.fileAt(i)
			}
		}
	}
	return nil, ix.defaultFile()
}

// mapValue is the value of a key of a mapping node, or nil.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// maxPathDepth bounds how far a path lookup follows aliases.
const maxPathDepth = 64

// locate returns the node a dotted path names, with the compose file it is in: the key node of the
// deepest key the path reaches, so a path with a missing tail is reported at the last key that is
// there. A key with a dot in it (a label such as com.example.tier) is matched whole. When the path
// names nothing the node is nil and the file is the index's default.
func (ix *PositionIndex) locate(path string) (*yaml.Node, string) {
	if ix == nil {
		return nil, ComposeFile
	}
	for _, i := range ix.order() {
		if n := walkPath(ix.docs[i], path, 0); n != nil {
			return n, ix.fileAt(i)
		}
	}
	return nil, ix.defaultFile()
}

func walkPath(n *yaml.Node, rest string, depth int) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && depth < maxPathDepth {
		n, depth = n.Alias, depth+1
	}
	if n == nil || rest == "" || depth >= maxPathDepth {
		return nil
	}
	switch n.Kind {
	case yaml.MappingNode:
		best, bestLen := -1, -1
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if (rest == k || strings.HasPrefix(rest, k+".")) && len(k) > bestLen {
				best, bestLen = i, len(k)
			}
		}
		if best < 0 {
			return nil
		}
		key, val := n.Content[best], n.Content[best+1]
		remainder := strings.TrimPrefix(rest[bestLen:], ".")
		if remainder == "" {
			return key
		}
		if deeper := walkPath(val, remainder, depth+1); deeper != nil {
			return deeper
		}
		return key
	case yaml.SequenceNode:
		seg, remainder, _ := strings.Cut(rest, ".")
		seg = strings.TrimSuffix(strings.TrimPrefix(seg, "["), "]")
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i >= len(n.Content) {
			return nil
		}
		if remainder == "" {
			return n.Content[i]
		}
		if deeper := walkPath(n.Content[i], remainder, depth+1); deeper != nil {
			return deeper
		}
		return n.Content[i]
	}
	return nil
}

// at turns a node into the start and end of the range a problem marks. A scalar is marked across
// its text on its line; anything else at its first character.
func at(n *yaml.Node) (line, col, endLine, endCol int) {
	if n == nil {
		return 1, 1, 1, 2
	}
	return posOf(n)
}

// posOf is the range of a node that is there; see at.
func posOf(n *yaml.Node) (line, col, endLine, endCol int) {
	width := 1
	if n.Kind == yaml.ScalarNode && n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0 && !strings.Contains(n.Value, "\n") {
		if w := len([]rune(n.Value)); w > 0 {
			width = w
		}
	}
	return n.Line, n.Column, n.Line, n.Column + width
}

var (
	yamlLineRe = regexp.MustCompile(`line (\d+)(?:, column (\d+))?: `)
	// yamlParserRe is a parser error that has a context: "while parsing a block mapping at line 1,
	// column 1: line 3: did not find expected key". The yaml library counts the second line from
	// zero (a scanner error and a construct error count from one), so the line it names is the one
	// before the line the parser stopped on.
	yamlParserRe = regexp.MustCompile(`at line \d+, column \d+: line (\d+)(?:, column (\d+))?: (.*)$`)
	// yamlLoadRe is the form go-yaml v4 (since rc.6) gives every error of its scanner and parser:
	// "go-yaml load error in parser (while parsing a block mapping) at L2.C3-L4.C4: did not find
	// expected key". Lines and columns count from one. A range is where the construct began and where
	// the parser stopped, and the stop is the line a person looks at; a single position is the place
	// the scanner stopped.
	yamlLoadRe = regexp.MustCompile(`go-yaml load error in \w+ \([^)]*\) at L(\d+)\.C(\d+)(?:-L(\d+)\.C(\d+))?: (.*)$`)
)

// yamlProblems turns the text of a YAML syntax error into problems at their lines: "Line {n}:
// {message}". A text that names no line is reported at line 1.
func yamlProblems(text, file string) Problems {
	text = strings.TrimSpace(text)
	var out Problems
	for _, l := range strings.Split(text, "\n") {
		if m := yamlLoadRe.FindStringSubmatch(l); m != nil {
			line, _ := strconv.Atoi(m[1])
			col, _ := strconv.Atoi(m[2])
			if m[3] != "" {
				line, _ = strconv.Atoi(m[3])
				col, _ = strconv.Atoi(m[4])
			}
			out = append(out, Problem{
				Severity: SeverityError, Code: CodeYAMLInvalid, File: file,
				Line: line, Col: col, EndLine: line, EndCol: col + 1,
				Message: fmt.Sprintf("Line %d: %s", line, strings.TrimSpace(m[5])),
			})
			continue
		}
		if m := yamlParserRe.FindStringSubmatch(l); m != nil {
			zero, _ := strconv.Atoi(m[1])
			line, col := zero+1, 1
			if m[2] != "" {
				col, _ = strconv.Atoi(m[2])
			}
			out = append(out, Problem{
				Severity: SeverityError, Code: CodeYAMLInvalid, File: file,
				Line: line, Col: col, EndLine: line, EndCol: col + 1,
				Message: fmt.Sprintf("Line %d: %s", line, strings.TrimSpace(m[3])),
			})
			continue
		}
		all := yamlLineRe.FindAllStringSubmatchIndex(l, -1)
		if len(all) == 0 {
			continue
		}
		last := all[len(all)-1]
		line, _ := strconv.Atoi(l[last[2]:last[3]])
		col := 1
		if last[4] >= 0 {
			col, _ = strconv.Atoi(l[last[4]:last[5]])
		}
		msg := strings.TrimSpace(l[last[1]:])
		out = append(out, Problem{
			Severity: SeverityError, Code: CodeYAMLInvalid, File: file,
			Line: line, Col: col, EndLine: line, EndCol: col + 1,
			Message: fmt.Sprintf("Line %d: %s", line, msg),
		})
	}
	if len(out) > 0 {
		return out
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(text, "yaml: "), "\n")
	return Problems{{
		Severity: SeverityError, Code: CodeYAMLInvalid, File: file, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: "Line 1: " + first,
	}}
}

var (
	interpolateRe = regexp.MustCompile(`^error while interpolating (\S+): (.*)$`)
	undefinedRe   = regexp.MustCompile(`^service "([^"]+)" refers to undefined (\w+) (.+?): invalid compose project$`)
	extraPropRe   = regexp.MustCompile(`^additional properties '([^']*)' not allowed$`)
)

// loadProblems turns the error compose-go refused a file with into positioned problems. The
// position of a schema error comes from walking the stored YAML to the key path the error names.
//
// A field whose value uses a secret variable is reported with the withheld sentence and never with the
// loader's words, which can hold part of the value (D-02, guardLoaderText).
func loadProblems(err error, ix *PositionIndex, s SecretUse, paths []string) Problems {
	file := ix.defaultFile()
	if len(paths) > 0 {
		file = paths[0]
	}
	switch {
	case errors.Is(err, ErrTooLarge):
		bad := file
		var fe *FileError
		if errors.As(err, &fe) && fe.Path != "" {
			bad = fe.Path
		}
		return Problems{tooLargeProblem(bad)}
	case errors.Is(err, context.DeadlineExceeded):
		return Problems{timeoutProblem(file)}
	}
	var pe *PortsError
	switch {
	case errors.Is(err, ErrLoadPanicked):
		return Problems{internalCheckFailed(file)}
	case errors.As(err, &pe):
		return Problems{portsProblem(pe, file)}
	case errors.Is(err, ErrComposeTooComplex):
		return Problems{tooComplexProblem(file)}
	}
	var le *LoadError
	inner := err
	if errors.As(err, &le) {
		inner = le.Err
	}
	text := strings.TrimSpace(inner.Error())
	// compose-go names the file it was reading: "failed to parse base.yaml: ..." for a syntax error and
	// "validating base.yaml: ..." for the schema.
	for _, path := range paths {
		if rest, ok := strings.CutPrefix(text, "failed to parse "+path+": "); ok {
			return yamlProblems(rest, path)
		}
		if rest, ok := strings.CutPrefix(text, "validating "+path+": "); ok {
			return schemaProblems(rest, ix.forFile(path), s)
		}
	}
	if m := interpolateRe.FindStringSubmatch(text); m != nil {
		return Problems{guardLoaderText(ix, CodeComposeInvalid, m[1], m[1]+": "+m[2], s)}
	}
	if m := undefinedRe.FindStringSubmatch(text); m != nil {
		path := "services." + m[1] + "." + m[2] + "s"
		if m[2] == "volume" {
			// "data:/data" with no volume called data: compose-go reads it as a named volume and
			// refuses it. What a person meant by it is a path, so it is the relative bind mount.
			if n, nfile := ix.shortVolume(m[1], m[3]); n != nil {
				p := relativeBindAt(n, nfile, "services."+m[1]+".volumes", m[3])
				if s.Withholds(path) {
					return Problems{keyProblem(ix, CodeComposeInvalid, path, s.WithheldMessage(path))}
				}
				return Problems{p}
			}
		}
		return Problems{guardLoaderText(ix, CodeComposeInvalid, path, fmt.Sprintf("%s: %s %s isn't defined. Declare it at the top level, or remove it.", path, m[2], m[3]), s)}
	}
	switch text {
	case "empty compose file":
		return Problems{{
			Severity: SeverityError, Code: CodeComposeInvalid, File: file, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
			Message: file + " is empty. Add a services section.",
		}}
	case "top-level object must be a mapping":
		return Problems{{
			Severity: SeverityError, Code: CodeComposeInvalid, File: file, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
			Message: "The top level of " + file + " must be a mapping, such as services: followed by your services.",
		}}
	}
	if rest, ok := strings.CutPrefix(text, decodeHeader); ok {
		if ps := decodeProblems(ix, text, rest, s); ps != nil {
			return ps
		}
	}
	// Text that names no field is withheld whenever the file uses a secret variable (fail closed),
	// whatever the text says. The interpolation format error quotes only the file's own text, which
	// holds no value of a variable.
	if strings.HasPrefix(text, "invalid interpolation format for ") {
		return Problems{keyProblem(ix, CodeComposeInvalid, "", text)}
	}
	return Problems{guardLoaderText(ix, CodeComposeInvalid, "", text, s)}
}

// decodeHeader starts the text of a decode failure (go-viper/mapstructure): the entries follow, one
// "'<name>' <message>" per line, and several to an error.
const decodeHeader = "decoding failed due to the following error(s):"

var decodeIndexRe = regexp.MustCompile(`\[([^\]]*)\]`)

// decodeProblems reads the entries of a decode failure and reports each one at its field. It
// returns nil when the text is to be reported as it stands: an entry does not parse, or the file
// uses no secret variable. In a file that uses one, each entry is reported
// at its own field even when none was withheld, because the text as a whole names no field and
// would be withheld for every value.
func decodeProblems(ix *PositionIndex, text, rest string, s SecretUse) Problems {
	var entries []string
	for _, line := range strings.Split(rest, "\n") {
		switch {
		case strings.HasPrefix(line, "'"):
			entries = append(entries, line)
		case strings.TrimSpace(line) == "":
		case len(entries) > 0:
			entries[len(entries)-1] += "\n" + line
		}
	}
	if len(entries) == 0 {
		return nil
	}
	out := make(Problems, 0, len(entries))
	for _, e := range entries {
		name, message, ok := strings.Cut(e[1:], "' ")
		if !ok {
			return nil
		}
		// "services[web].volumes[0].tmpfs.size" is the path services.web.volumes.0.tmpfs.size.
		path := decodeIndexRe.ReplaceAllString(name, ".$1")
		words := path + ": " + message
		p := guardLoaderText(ix, CodeComposeInvalid, path, words, s)
		out = append(out, p)
	}
	if !s.Any() {
		return nil
	}
	return out
}

// guardLoaderText is a loader problem at the key path names, with the loader's words, unless
// s.Withholds the path: then it carries s.WithheldMessage instead, never the words, which may hold
// part of a secret value (fail closed). The words do not take part in the decision. An empty path
// puts the problem on line 1.
func guardLoaderText(ix *PositionIndex, code, path, words string, s SecretUse) Problem {
	if s.Withholds(path) {
		words = s.WithheldMessage(path)
	}
	return keyProblem(ix, code, path, words)
}

// portsProblem is the problem a refusal of PortBudget is reported as, at the item that caused it.
// Neither its message nor its key holds a port's text: a value may come from a secret variable.
func portsProblem(pe *PortsError, file string) Problem {
	line, col := max(pe.Line, 1), max(pe.Col, 1)
	if pe.File != "" {
		file = pe.File
	}
	p := Problem{Severity: SeverityError, File: file, Key: pe.Key, Line: line, Col: col, EndLine: line, EndCol: col + 1}
	if errors.Is(pe.Err, ErrPortRangeInvalid) {
		p.Code = CodePortRangeInvalid
		p.Message = "This port range isn't valid: a port is a number from 0 to 65535, and a range runs from the lower port to the higher."
		return p
	}
	p.Code = CodePortsTooMany
	p.Message = fmt.Sprintf("The ports and expose ranges of this file add up to more than %s ports. Use smaller ranges.", thousands(MaxPorts))
	return p
}

// thousands writes n with a comma between thousands.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// timeoutProblem is the one problem of a check that ran out of time.
func internalCheckFailed(file string) Problem {
	return Problem{
		Severity: SeverityError, Code: CodeInternalCheckFailed, File: file, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: "The check found something it should have refused. This is a bug in Bayplan: please report it.",
	}
}

func timeoutProblem(file string) Problem {
	return Problem{
		Severity: SeverityError, Code: CodeCheckTimeout, File: file, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: "The check took too long and was stopped. Simplify the file and try again.",
	}
}

// keyProblem is an error at the key a path names, or at line 1 when the path is not in the file.
func keyProblem(ix *PositionIndex, code, path, message string) Problem {
	n, file := ix.locate(path)
	line, col, endLine, endCol := at(n)
	return Problem{
		Severity: SeverityError, Code: code, File: file, Key: path, Message: message,
		Line: line, Col: col, EndLine: endLine, EndCol: endCol,
	}
}

// schemaProblems reads a compose-go schema error: "{path} {message}", with an empty path for the
// top level. An unknown key reads "{parent}: unknown key “{key}”." at the key.
func schemaProblems(text string, ix *PositionIndex, s SecretUse) Problems {
	path, msg, _ := strings.Cut(text, " ")
	msg = strings.TrimSpace(msg)
	for _, sw := range [][2]string{{"a array", "an array"}, {"a object", "an object"}, {"a integer", "an integer"}} {
		msg = strings.ReplaceAll(msg, sw[0], sw[1])
	}
	if path == "" {
		if m := extraPropRe.FindStringSubmatch(msg); m != nil {
			return Problems{keyProblem(ix, CodeSchemaInvalid, m[1], fmt.Sprintf("unknown key “%s”.", m[1]))}
		}
		// A top-level error names no field, so it is withheld whenever the file uses a secret variable.
		return Problems{guardLoaderText(ix, CodeSchemaInvalid, "", msg, s)}
	}
	if msg == "false schema" {
		parent, key := path, path
		if i := strings.LastIndex(path, "."); i >= 0 {
			parent, key = path[:i], path[i+1:]
		}
		return Problems{keyProblem(ix, CodeSchemaInvalid, path, fmt.Sprintf("%s: unknown key “%s”.", parent, key))}
	}
	// A value that fails the schema is quoted by the validator, so the words are guarded.
	return Problems{guardLoaderText(ix, CodeSchemaInvalid, path, path+": "+msg, s)}
}
