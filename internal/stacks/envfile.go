// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"bytes"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/types"
)

var (
	// ErrEnvFileMissing means a required env_file is not one of the stack's files.
	ErrEnvFileMissing = errors.New("stacks: an env_file is not one of the stack's files")
	// ErrEnvFileInvalid means an env_file could not be read as a .env file.
	ErrEnvFileInvalid = errors.New("stacks: an env_file could not be read")
)

// EnvFileError is the env_file that could not be resolved: the service, the path as the compose
// file wrote it and, for a file that could not be read, the 1-based line (0 when none is known).
// It never holds a value from the file: a parser's own text can quote the line it stopped at, and
// the file may be a secret, so only the line number is kept.
type EnvFileError struct {
	Service string
	Path    string
	Line    int
	// Format is set when the file asked for a format TierPlan does not read.
	Format string
	Err    error
}

func (e *EnvFileError) Error() string {
	if e.Service == "" {
		return "stacks: " + strconv.Quote(e.Path) + ": " + e.Err.Error()
	}
	return "stacks: service " + e.Service + ": env_file " + strconv.Quote(e.Path) + ": " + e.Err.Error()
}

// Unwrap returns ErrEnvFileMissing or ErrEnvFileInvalid.
func (e *EnvFileError) Unwrap() error { return e.Err }

// parserLine finds the line number in the text of compose-go's .env parser ("line 2: unterminated
// quoted value ..."). Only the number is used; the rest of the text may quote the file.
var parserLine = regexp.MustCompile(`\bline (\d+):`)

// errorLine is the 1-based line of a file the .env parser refused, or 0 when it cannot tell. The
// parser counts the line break that ends an unterminated quote, so for that error it names the line
// after the one at fault; the line is found by matching the text the parser quotes against the
// file, and the text is only compared, never kept. For any other error the parser's number is used,
// held to the file's own line count.
func errorLine(err error, content []byte) int {
	text := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	msg := err.Error()
	if _, quoted, ok := strings.Cut(msg, "unterminated quoted value "); ok {
		for i, l := range text {
			if strings.HasSuffix(strings.TrimSuffix(l, "\r"), quoted) {
				return i + 1
			}
		}
	}
	m := parserLine.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return min(n, len(text))
}

// ResolveEnvFiles is the one place an env_file is read (D-05, Pitfall 4). For each service it takes
// every env_file entry from the stack's own files (files.Lookup; the server's filesystem is never
// opened), parses it with compose-go's .env parser, and merges the result under the service's
// explicit environment: a later file beats an earlier one and an explicit environment entry beats
// them all. A ${NAME} in a file is looked up in the table (lookup), then in the service's explicit
// environment, then in the earlier lines and files; the server's own environment is never read.
// A file that is not there is skipped when its entry says required: false, and is
// ErrEnvFileMissing otherwise.
//
// Whatever the outcome, EnvFiles is empty on every service when it returns, so the project that is
// handed to a deploy has no env_file left for an engine, the Compose SDK or the converter to read
// a second way: a swarm and a standalone host see the same values. The first failure, in service
// order, is returned as an *EnvFileError.
func ResolveEnvFiles(p *types.Project, files FileSet, lookup func(string) (string, bool)) error {
	if p == nil {
		return nil
	}
	names := make([]string, 0, len(p.Services))
	for name := range p.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	var first error
	for _, name := range names {
		svc := p.Services[name]
		if len(svc.EnvFiles) > 0 {
			env, err := resolveService(name, svc, files, lookup)
			if err != nil && first == nil {
				first = err
			}
			if err == nil {
				svc.Environment = env
			}
		}
		svc.EnvFiles = nil
		p.Services[name] = svc
	}
	return first
}

// resolveService is a service's environment with its env_files merged under it.
func resolveService(name string, svc types.ServiceConfig, files FileSet, lookup func(string) (string, bool)) (types.MappingWithEquals, error) {
	fromFiles := map[string]string{}
	fileLookup := func(key string) (string, bool) {
		if v, ok := lookup(key); ok {
			return v, true
		}
		if v, ok := svc.Environment[key]; ok && v != nil {
			return *v, true
		}
		v, ok := fromFiles[key]
		return v, ok
	}
	for _, ef := range svc.EnvFiles {
		f, ok := files.Lookup(ef.Path)
		if !ok {
			if ef.Required {
				return nil, &EnvFileError{Service: name, Path: ef.Path, Err: ErrEnvFileMissing}
			}
			continue
		}
		if ef.Format != "" && ef.Format != dotenv.DotEnv {
			return nil, &EnvFileError{Service: name, Path: ef.Path, Format: ef.Format, Err: ErrEnvFileInvalid}
		}
		vars, err := dotenv.ParseWithLookup(bytes.NewReader(f.Content), fileLookup)
		if err != nil {
			return nil, &EnvFileError{Service: name, Path: ef.Path, Line: errorLine(err, f.Content), Err: ErrEnvFileInvalid}
		}
		for k, v := range vars {
			fromFiles[k] = v
		}
	}
	env := make(types.MappingWithEquals, len(fromFiles)+len(svc.Environment))
	for k, v := range fromFiles {
		env[k] = &v
	}
	// An explicit entry wins; one with no value (a bare name nothing filled) does not hide the file's.
	for k, v := range svc.Environment {
		if v != nil {
			env[k] = v
		} else if _, ok := env[k]; !ok {
			env[k] = nil
		}
	}
	return env, nil
}

// repoDotEnv is the .env at the stack root when it is a file of the repository. An inline or overlay
// .env is not one (D-07): only a committed .env supplies defaults.
func repoDotEnv(files []File) (File, bool) {
	for _, f := range files {
		if f.Path == DotEnvFile && f.Origin == OriginRepo {
			return f, true
		}
	}
	return File{}, false
}

// RepoEnv parses the .env at the stack root of a stack made from a git repository (D-07). It is read
// only when the file's origin is OriginRepo: an inline stack's .env, and an overlay .env, supply
// nothing. A ${NAME} inside a value is looked up in the stack's table. The server's own environment is
// never read. A working copy with no repository .env returns a nil map.
//
// A file over MaxFileBytes is a *FileError and one the .env parser refuses is an *EnvFileError for
// ".env" with the line (0 when unknown); neither holds a value from the file.
func RepoEnv(c WorkingCopy) (map[string]string, error) {
	f, ok := repoDotEnv(c.Files)
	if !ok {
		return nil, nil
	}
	if len(f.Content) > MaxFileBytes {
		return nil, &FileError{Path: DotEnvFile, Err: ErrFileTooLarge}
	}
	vars, err := dotenv.ParseWithLookup(bytes.NewReader(f.Content), tableValues(c.Vars))
	if err != nil {
		return nil, &EnvFileError{Path: DotEnvFile, Line: errorLine(err, f.Content), Err: ErrEnvFileInvalid}
	}
	return vars, nil
}

// InterpolationEnv is the environment a compose file of the working copy is interpolated with (D-07): the
// repository's .env (RepoEnv) under the table, where a value in the table beats the .env and a blank value in
// either is no value. Load interpolates with it, and so must anything that judges the values a deploy would
// use (the host-access policy), so that they never disagree. A .env that cannot be read is an error, never a
// table-only environment.
func InterpolationEnv(c WorkingCopy) (map[string]string, error) {
	repoEnv, err := RepoEnv(c)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string, len(c.Vars)+len(repoEnv))
	for name, value := range repoEnv {
		if value != "" {
			env[name] = value
		}
	}
	for _, v := range c.Vars {
		if v.Value != "" {
			env[v.Name] = v.Value
		}
	}
	return env, nil
}

// ObjectVars is the variables a secret or config that reads `environment: NAME` is resolved from at a check, a
// review and a deploy: the table over the repository's .env (RepoEnv), the same layering the compose files are
// interpolated with, so the three never disagree about which values exist. A blank table value hides nothing
// the .env holds, but one the .env does not hold stays in the map as blank (an empty secret is refused as
// empty, not reported as missing). A .env that cannot be read adds nothing: the load has already refused a
// stack with one, and a table-only map can only fail closed (a missing variable).
func ObjectVars(c WorkingCopy) map[string]string {
	repoEnv, _ := RepoEnv(c)
	return objectVarsLayered(c.Vars, repoEnv)
}

func objectVarsLayered(vars []Var, repoEnv map[string]string) map[string]string {
	out := make(map[string]string, len(vars)+len(repoEnv))
	for name, value := range repoEnv {
		if value != "" {
			out[name] = value
		}
	}
	for _, v := range vars {
		if _, held := out[v.Name]; v.Value != "" || !held {
			out[v.Name] = v.Value
		}
	}
	return out
}
