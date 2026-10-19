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

// Load reads the working copy's compose file with compose-go, in a sandbox:
//
//   - the content is in memory and the environment is the stack's variable table only, so a
//     variable set in the Bayplan server's own environment never reaches interpolation (D-01). A
//     table entry with no value is no value: the default written in the file is used;
//   - include and extends are not followed, and env_file and label_file are not resolved, so the
//     load opens no file;
//   - paths are not resolved, and the working directory does not exist.
//
// A compose file over MaxFileBytes is refused with ErrTooLarge before it is parsed.
func Load(ctx context.Context, in LoadInput) (*Loaded, error) {
	content := in.Copy.Compose()
	if len(content) > MaxFileBytes {
		return nil, ErrTooLarge
	}
	env := make(types.Mapping, len(in.Copy.Vars))
	for _, v := range in.Copy.Vars {
		if v.Value != "" {
			env[v.Name] = v.Value
		}
	}
	unset := &unsetNames{}
	deadline := in.Deadline
	if deadline <= 0 {
		deadline = loadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	details := types.ConfigDetails{
		WorkingDir:  sandboxDir,
		ConfigFiles: []types.ConfigFile{{Filename: ComposeFile, Content: content}},
		Environment: env,
	}
	project, err := loader.LoadWithContext(ctx, details, func(o *loader.Options) {
		o.SetProjectName(in.Name, true)
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
	// compose-go looks at the context only at some points, so a load that ran out of time is
	// ended here whether or not it noticed: the result is then the same on every machine.
	if cerr := ctx.Err(); cerr != nil {
		return nil, &LoadError{Err: cerr}
	}
	if err != nil {
		return nil, &LoadError{Err: err}
	}
	return &Loaded{Project: project, Unset: unset.sorted()}, nil
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
