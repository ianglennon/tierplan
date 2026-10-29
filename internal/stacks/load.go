// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/types"
)

// ErrTooLarge means the compose file is over MaxFileBytes.
var ErrTooLarge = errors.New("stacks: the compose file is too large")

// sandboxDir is the working directory a load runs in. It does not exist on any host Bayplan
// runs on: a relative path in a compose file resolves under it and so reads nothing.
const sandboxDir = "/bayplan-stack-sandbox"

// ErrLoadPanicked means compose-go stopped with a panic. The panic value is never kept: it can
// quote the file or a variable of the stack.
var ErrLoadPanicked = errors.New("stacks: compose-go stopped with a panic")

// loaderKey is the context key under which a test puts a stand-in for compose-go (export_test.go).
// Production code never sets it.
type loaderKey struct{}

// loadFunc is the shape of loader.LoadWithContext.
type loadFunc = func(context.Context, types.ConfigDetails, ...func(*loader.Options)) (*types.Project, error)

// loaderFrom returns the stand-in a test put in the context, or compose-go's own loader.
func loaderFrom(ctx context.Context) loadFunc {
	if fn, ok := ctx.Value(loaderKey{}).(loadFunc); ok {
		return fn
	}
	return loader.LoadWithContext
}

// loadTimeout bounds one load; compose-go walks a YAML tree that a user wrote.
const loadTimeout = 5 * time.Second

// LoadError is a compose file compose-go refused: Err is compose-go's own error, so Check can read
// the position and the sentence out of it. Its text is the one Load has always returned.
type LoadError struct{ Err error }

func (e *LoadError) Error() string { return fmt.Sprintf("stacks: load %s: %v", ComposeFile, e.Err) }

// Unwrap returns compose-go's error.
func (e *LoadError) Unwrap() error { return e.Err }

// LoadInput is what Load needs: the stack's name (it becomes the compose project name), its
// target and the working copy to load.
type LoadInput struct {
	Name   string
	Target Target
	Copy   WorkingCopy
	// Deadline bounds the load. Zero is loadTimeout.
	Deadline time.Duration
}

// Loaded is a compose file read by compose-go, with its variables interpolated.
type Loaded struct {
	Project *types.Project
	// Unset are the names of the variables the file refers to plainly (${NAME} or $NAME) that have
	// no value in the table and no default, sorted. compose-go fills them with a blank and only
	// logs; Bayplan names them so a check can refuse the deploy (Pitfall 3).
	Unset []string
}

// Load reads the working copy's compose files, merged in order, with compose-go, in a sandbox:
//
//   - the content is in memory and the environment is the stack's variable table only, so a
//     variable set in the Bayplan server's own environment never reaches interpolation (D-01). A
//     table entry with no value is no value: the default written in the file is used;
//   - include and extends are not followed, and env_file and label_file are not resolved, so the
//     load opens no file;
//   - paths are not resolved, and the working directory does not exist;
//   - the ports: and expose: ranges are measured before compose-go expands them (PortBudget).
//
// A stack made from a repository also takes the repository's .env as defaults below the table (RepoEnv).
//
// A compose file over MaxFileBytes is refused with ErrTooLarge (in a *FileError naming it) before it is parsed. compose-go runs
// on its own goroutine: Load returns at the deadline without waiting for it, and a panic inside it
// is ErrLoadPanicked.
func Load(ctx context.Context, in LoadInput) (*Loaded, error) {
	contents := in.Copy.ComposeContents()
	for _, c := range contents {
		if len(c.Content) > MaxFileBytes {
			return nil, &FileError{Path: c.Path, Err: ErrTooLarge}
		}
	}
	// compose-go expands a port range into one entry per port and does not stop when its context
	// ends, so the ranges are measured first.
	if err := portBudgetFiles(contents, in.Copy.Vars); err != nil {
		return nil, err
	}
	// The environment is the repository's .env under the table (D-07): a stack made from a repository
	// takes its defaults from the committed .env, and a value in the table beats it. A .env that cannot
	// be read refuses the load, so a deploy never goes ahead with half of its defaults.
	interp, err := InterpolationEnv(in.Copy)
	if err != nil {
		return nil, err
	}
	env := make(types.Mapping, len(interp))
	for name, value := range interp {
		env[name] = value
	}
	unset := &unsetNames{}
	deadline := in.Deadline
	if deadline <= 0 {
		deadline = loadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	// One config file per ComposePaths() entry, in order: compose-go merges them like docker compose -f a -f b,
	// from the content it is given and never from disk.
	configFiles := make([]types.ConfigFile, len(contents))
	for i, c := range contents {
		configFiles[i] = types.ConfigFile{Filename: c.Path, Content: c.Content}
	}
	details := types.ConfigDetails{
		WorkingDir:  sandboxDir,
		ConfigFiles: configFiles,
		Environment: env,
	}
	// compose-go does not stop when its context ends, so it runs on its own goroutine and the
	// request does not wait for it past the deadline. An abandoned load runs to its end into the
	// buffered channel and is then collected; what it can cost is bounded by the port budget above
	// and the prescan's work budget. A panic inside it is an error, never a crash of the server.
	type result struct {
		project *types.Project
		err     error
	}
	done := make(chan result, 1)
	load := loaderFrom(ctx)
	go func() {
		defer func() {
			// The panic value can quote the file or a variable, so it is dropped.
			if recover() != nil {
				done <- result{err: ErrLoadPanicked}
			}
		}()
		project, err := load(ctx, details, func(o *loader.Options) {
			o.SetProjectName(ProjectNameFor(in.Name), true)
			o.SkipInclude = true
			o.SkipExtends = true
			o.SkipResolveEnvironment = true
			o.SkipResolveLabels = true
			o.ResolvePaths = false
			if o.Interpolate != nil {
				// The lookup and the casts stay compose-go's; only the substitution is wrapped, to see
				// which plain references found nothing.
				o.Interpolate.Substitute = unset.substitute
			}
		})
		done <- result{project: project, err: err}
	}()
	var res result
	select {
	case <-ctx.Done():
		return nil, &LoadError{Err: ctx.Err()}
	case res = <-done:
	}
	// A load that finished and a deadline that passed together end the same way on every machine.
	if cerr := ctx.Err(); cerr != nil {
		return nil, &LoadError{Err: cerr}
	}
	if res.err != nil {
		return nil, &LoadError{Err: res.err}
	}
	project := res.project
	keepStackName(project, in.Name, ProjectNameFor(in.Name))
	fillBareEnvironment(project, env, unset)
	return &Loaded{Project: project, Unset: unset.sorted()}, nil
}

// fillBareEnvironment gives a bare environment entry ("- FOO", "FOO:") the value the table holds for
// it. The Compose spec would pass the host's own value through; Bayplan never reads the server's
// environment, so an entry the table has no value for stays unset and is named in unset.
func fillBareEnvironment(project *types.Project, table types.Mapping, unset *unsetNames) {
	for _, svc := range project.Services {
		for name, v := range svc.Environment {
			if v != nil {
				continue
			}
			if value, ok := table[name]; ok {
				svc.Environment[name] = &value
				continue
			}
			unset.add(name)
		}
	}
}

// unsetNames collects the plain references that found no value while a file was interpolated.
type unsetNames struct {
	mu    sync.Mutex
	names map[string]struct{}
}

var plainNameRe = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

// substitute is template.Substitute with a replacement hook: compose-go's own replacement runs,
// and when it reports that a plain ${NAME} or $NAME found no value (nothing applied, no default),
// the name is noted. A reference with a default, a presence form or a ":?" message is not noted:
// it has its own answer.
func (u *unsetNames) substitute(s string, m template.Mapping) (string, error) {
	return template.SubstituteWithOptions(s, m, template.WithoutLogging,
		template.WithReplacementFunction(func(sub string, m template.Mapping, cfg *template.Config) (string, error) {
			out, applied, err := template.DefaultReplacementAppliedFunc(sub, m, cfg)
			if err == nil && !applied {
				if g := plainNameRe.FindStringSubmatch(sub); g != nil {
					u.add(g[1])
				}
			}
			return out, err
		}))
}

func (u *unsetNames) add(name string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.names == nil {
		u.names = map[string]struct{}{}
	}
	u.names[name] = struct{}{}
}

func (u *unsetNames) sorted() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, 0, len(u.names))
	for n := range u.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ProjectNameFor is the compose project name a stack is loaded under: the stack's name when compose-go takes it
// as a project name, else the nearest name compose-go takes (lowercase, with the characters it refuses dropped),
// and "stack" when nothing is left. A swarm stack may hold uppercase letters (D-48, D-53), which compose-go
// refuses in a project name; Load loads such a stack under this name and puts the stack's own name back on
// what compose-go named after the project.
func ProjectNameFor(name string) string {
	norm := loader.NormalizeProjectName(name)
	if norm == "" {
		return "stack"
	}
	return norm
}

// keepStackName renames what compose-go named <projectName>_<key> to <stackName>_<key>: every network, volume,
// secret and config that is not external and was not given an explicit name. It runs only when the stack's name
// is not the project name compose-go was given, so the names of a stack compose-go takes as it is are never
// touched. A name a file wrote out that happens to read <projectName>_<key> is renamed too; it is the stack's
// own prefix by another route, and the stack name is the namespace swarm keeps its objects under (D-48).
func keepStackName(p *types.Project, stackName, projectName string) {
	if stackName == projectName {
		return
	}
	prefix, to := projectName+"_", stackName+"_"
	rename := func(name, key string, external bool) string {
		if !external && name == prefix+key {
			return to + key
		}
		return name
	}
	for key, n := range p.Networks {
		n.Name = rename(n.Name, key, bool(n.External))
		p.Networks[key] = n
	}
	for key, v := range p.Volumes {
		v.Name = rename(v.Name, key, bool(v.External))
		p.Volumes[key] = v
	}
	for key, sc := range p.Secrets {
		sc.Name = rename(sc.Name, key, bool(sc.External))
		p.Secrets[key] = sc
	}
	for key, c := range p.Configs {
		c.Name = rename(c.Name, key, bool(c.External))
		p.Configs[key] = c
	}
	p.Name = stackName
}
