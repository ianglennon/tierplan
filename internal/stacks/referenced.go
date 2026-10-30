// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stacks

import (
	"sort"
	"strings"
)

// Referenced is the set of files of a repository folder that make up a stack (D-27).
type Referenced struct {
	// Paths are the stack's files, relative to the stack root, sorted and without repeats: the compose
	// files, the root .env and every env_file, secrets file: and configs file: path.
	Paths []string
	// Escapes are the references, as written, that name no path inside the stack (absolute, "..", a
	// drive). They are never in Paths.
	Escapes []string
	// Optional are the Paths that may be absent: the root .env, and an env_file with required: false.
	Optional []string
}

// ReferencedPaths names the files a stack needs (D-27): the compose files, the root .env and every
// env_file, secrets file: and configs file: path the compose files name, sorted and without repeats.
// Nothing else in the folder is a stack file, so nothing else is stored.
//
// The paths are read from the compose files' YAML, so the answer does not depend on whether the project
// loaded, and an env_file Check has already resolved is still named. loaded, when set, adds the paths
// as compose-go read them, after interpolation. A reference that is not a path inside the stack (an
// absolute path, one with "..") is returned as written in Escapes and never in Paths. The root .env is
// listed whether or not the folder has it and is Optional, as is an env_file with required: false that no
// other reference requires.
func ReferencedPaths(loaded *Loaded, c WorkingCopy) Referenced {
	paths := map[string]struct{}{DotEnvFile: {}}
	escapes := map[string]struct{}{}
	required := map[string]struct{}{}
	optional := map[string]struct{}{DotEnvFile: {}}
	for _, p := range c.ComposePaths() {
		paths[p] = struct{}{}
		required[p] = struct{}{}
	}
	add := func(ref string, isOptional bool) {
		path := strings.TrimPrefix(ref, "./")
		if ValidPath(path) != nil {
			escapes[ref] = struct{}{}
			return
		}
		paths[path] = struct{}{}
		if isOptional {
			optional[path] = struct{}{}
		} else {
			required[path] = struct{}{}
		}
	}
	for _, u := range referencedUses(c.ComposeContents()) {
		add(u.Ref, u.Optional)
	}
	if loaded != nil && loaded.Project != nil {
		for _, svc := range loaded.Project.Services {
			for _, ef := range svc.EnvFiles {
				add(ef.Path, !bool(ef.Required))
			}
		}
		for _, sc := range loaded.Project.Secrets {
			if sc.File != "" {
				add(sc.File, false)
			}
		}
		for _, cfg := range loaded.Project.Configs {
			if cfg.File != "" {
				add(cfg.File, false)
			}
		}
	}
	out := Referenced{Paths: sortedKeys(paths), Escapes: sortedKeys(escapes)}
	for p := range optional {
		if _, req := required[p]; !req {
			out.Optional = append(out.Optional, p)
		}
	}
	sort.Strings(out.Optional)
	return out
}
