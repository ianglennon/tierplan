// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"bytes"
	"sort"
	"strings"
	"unicode/utf8"
)

// secretWords are the _-separated segments of a variable name that make it look like a secret.
var secretWords = map[string]struct{}{
	"PASSWORD": {}, "PASSWD": {}, "PWD": {}, "SECRET": {}, "SECRETS": {}, "TOKEN": {}, "KEY": {},
	"APIKEY": {}, "CREDENTIAL": {}, "CREDENTIALS": {}, "PRIVATE": {},
}

// SecretLikeName reports whether a variable name looks like the name of a secret: one of its
// _-separated segments, in either case, is PASSWORD, PASSWD, PWD, SECRET, SECRETS, TOKEN, KEY, APIKEY,
// CREDENTIAL, CREDENTIALS or PRIVATE. It is a whole-segment rule, so KEYCLOAK_URL and MONKEY are not
// secret-like and DB_PASSWORD and API_KEY are (D-07).
func SecretLikeName(name string) bool {
	for seg := range strings.SplitSeq(name, "_") {
		if _, ok := secretWords[strings.ToUpper(seg)]; ok {
			return true
		}
	}
	return false
}

// envSecretLikeMessage is the warning for a secret-like key committed in the repository's .env
// (UI-SPEC, "Lint copy for git stacks"). It names the key and never its value.
func envSecretLikeMessage(name string) string {
	return name + " looks like a secret and is committed in the repository's .env. Set a secret variable here to override it."
}

// GitLint judges the rules that only a stack made from a git repository has (D-07, D-09, D-10). It is
// pure: it reads the working copy it is given, whose files carry their Origin, and opens nothing.
// loaded may be nil, for a copy whose compose files did not load. The problems are sorted by file and
// line.
//
//   - overlay_clash: a path that is both a repository file and an overlay file (D-09);
//   - repo_file_missing: a file an env_file, a secrets file: or a configs file: names that is neither a
//     repository file nor an overlay file; an env_file with required: false may be absent;
//   - git_lfs_pointer: a repository file whose content is a Git LFS pointer (D-10);
//   - env_secret_like: a key of the repository's .env whose name looks like a secret (D-07). A key the
//     table already overrides with a value is not reported: the warning's own advice has been followed.
func GitLint(c WorkingCopy, _ *Loaded) []Problem {
	var out Problems
	out = append(out, overlayClashProblems(c.Files)...)
	out = append(out, repoFileMissingProblems(c)...)
	out = append(out, lfsPointerProblems(c.Files)...)
	out = append(out, envSecretLikeProblems(c)...)
	return out.Sorted()
}

// overlayClashProblems are the blocking problems of the paths that are both a repository file and an
// overlay file, in path order.
func overlayClashProblems(files []File) Problems {
	repo, overlay := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		switch f.Origin {
		case OriginRepo:
			repo[f.Path] = true
		case OriginOverlay:
			overlay[f.Path] = true
		}
	}
	var paths []string
	for p := range repo {
		if overlay[p] {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	out := make(Problems, 0, len(paths))
	for _, p := range paths {
		out = append(out, Problem{
			Severity: SeverityError, Code: CodeOverlayClash, File: p, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
			Message: p + " exists in the repository and as an overlay file. Remove the overlay file or rename it.",
		})
	}
	return out
}

// refUse is one place a compose file refers to a stack file: an env_file entry, a top-level secrets
// file: or a top-level configs file:.
type refUse struct {
	// Ref is the path as the compose file wrote it and Path is it without a leading "./".
	Ref, Path string
	// Optional is true for an env_file with required: false.
	Optional bool
	// Compose is the compose file, Key what names the file (env_file, secrets.X.file) and Line and Col
	// where the value is written.
	Compose, Key string
	Line, Col    int
}

// referencedUses lists every file the compose files name, in file then document order, whether or not
// the stack has it. It walks the raw YAML, so it needs no load and reads no file. A compose file that
// is not YAML, or is too large or too complex to walk, names none; the others are still read.
func referencedUses(contents []ComposeContent) []refUse {
	var out []refUse
	for _, cf := range contents {
		if len(cf.Content) > MaxFileBytes {
			continue
		}
		roots, ok := decodeRoots(cf.Content)
		if !ok {
			continue
		}
		var uses []refUse
		w := &walker{budget: maxBudget, file: cf.Path, uses: &uses}
		failed := false
		for _, root := range roots {
			if w.measure(root, 0); w.err != nil {
				failed = true
				break
			}
		}
		for _, root := range roots {
			if failed {
				break
			}
			if w.document(root); w.err != nil {
				failed = true
			}
		}
		if !failed {
			out = append(out, uses...)
		}
	}
	return out
}

// repoFileMissingProblems are the blocking problems of the files the compose files need that the
// working copy does not hold, as a repository file or as an overlay file. They offer the overlay.
func repoFileMissingProblems(c WorkingCopy) Problems {
	have := make(map[string]bool, len(c.Files))
	for _, f := range c.Files {
		have[f.Path] = true
	}
	var out Problems
	for _, u := range referencedUses(c.ComposeContents()) {
		if u.Optional || ValidPath(u.Path) != nil || have[u.Path] {
			continue
		}
		out = append(out, Problem{
			Severity: SeverityError, Code: CodeRepoFileMissing, File: u.Compose, Key: u.Key, Action: ActionAddOverlay,
			Line: u.Line, Col: u.Col, EndLine: u.Line, EndCol: u.Col + max(utf8.RuneCountInString(u.Ref), 1),
			Message: u.Path + " isn't in the repository.",
		})
	}
	return out
}

// lfsPointerProblems are the blocking problems of the repository files that are Git LFS pointers, in
// path order. An overlay file is TierPlan's own and is not judged.
func lfsPointerProblems(files []File) Problems {
	var out Problems
	for _, f := range files {
		if f.Origin == OriginRepo && IsLFSPointer(f.Content) {
			out = append(out, Problem{
				Severity: SeverityError, Code: CodeLFSPointer, File: f.Path, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
				Message: f.Path + " is a Git LFS pointer. TierPlan doesn't support Git LFS.",
			})
		}
	}
	return out
}

// envSecretLikeProblems are the env_secret_like warnings of the repository's .env, one for each key
// whose name looks like a secret, at the line the key is written on.
func envSecretLikeProblems(c WorkingCopy) []Problem {
	f, ok := repoDotEnv(c.Files)
	if !ok {
		return nil
	}
	env, err := RepoEnv(c)
	if err != nil {
		// The unreadable .env is reported by the check that reads it.
		return nil
	}
	overridden := map[string]bool{}
	for _, v := range c.Vars {
		if v.Value != "" {
			overridden[v.Name] = true
		}
	}
	lines := envKeyLines(f.Content)
	names := make([]string, 0, len(env))
	for name := range env {
		if SecretLikeName(name) && !overridden[name] {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if lines[names[i]] != lines[names[j]] {
			return lines[names[i]] < lines[names[j]]
		}
		return names[i] < names[j]
	})
	out := make([]Problem, 0, len(names))
	for _, name := range names {
		line := max(lines[name], 1)
		out = append(out, Problem{
			Severity: SeverityWarning, Code: CodeEnvSecretLike, File: DotEnvFile, Key: name,
			Line: line, Col: 1, EndLine: line, EndCol: 1 + len(name),
			Message: envSecretLikeMessage(name),
		})
	}
	return out
}

// envKeyLines maps each key of a .env file to the 1-based line its first assignment is written on.
// A line is "NAME=value" or "export NAME=value"; a comment, a blank line and the continuation of a
// quoted value are not read as keys. It only locates: the values come from the parser.
func envKeyLines(content []byte) map[string]int {
	out := map[string]int{}
	var quote byte
	for i, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if quote != 0 {
			if strings.IndexByte(line, quote) >= 0 {
				quote = 0
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if !varNameFormRe.MatchString(name) {
			continue
		}
		if _, dup := out[name]; !dup {
			out[name] = i + 1
		}
		value = strings.TrimSpace(value)
		if value != "" && (value[0] == '"' || value[0] == '\'') && strings.IndexByte(value[1:], value[0]) < 0 {
			quote = value[0]
		}
	}
	return out
}

// envSecretLikeValues are the values of the repository .env keys whose name looks like a secret, in the
// forms a masker must hide. A value is read through the parser and, as a second line of defence when the
// file does not parse, from the text after the first "=" of each line.
func envSecretLikeValues(files []File) []string {
	f, ok := repoDotEnv(files)
	if !ok || len(f.Content) > MaxFileBytes {
		return nil
	}
	var out []string
	c := WorkingCopy{Files: []File{f}}
	if env, err := RepoEnv(c); err == nil {
		for name, value := range env {
			if SecretLikeName(name) {
				out = append(out, varMaskValues(value)...)
			}
		}
	}
	for _, raw := range strings.Split(string(f.Content), "\n") {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "export "))
		name, value, found := strings.Cut(line, "=")
		if found && SecretLikeName(strings.TrimSpace(name)) {
			out = append(out, unquote(strings.TrimSpace(value)))
		}
	}
	return out
}

// The size and the place of a Git LFS pointer: a pointer is a few lines of text that begins with the
// version line.
const (
	gitLFSPrefix       = "version https://git-lfs.github.com/spec/v1"
	maxLFSPointerBytes = 1024
	lfsPrefixWindow    = 200
)

// IsLFSPointer reports whether content is a Git LFS pointer rather than the file it stands for: under
// 1 KiB, and starting with the LFS version line within its first 200 bytes.
func IsLFSPointer(content []byte) bool {
	if len(content) == 0 || len(content) >= maxLFSPointerBytes {
		return false
	}
	return bytes.HasPrefix(content[:min(len(content), lfsPrefixWindow)], []byte(gitLFSPrefix))
}

// SubmoduleProblem is the blocking problem for a path that is a git submodule (D-10). The importer
// raises it for a path the repository records as a submodule.
func SubmoduleProblem(path string) Problem {
	return Problem{
		Severity: SeverityError, Code: CodeSubmodule, File: path, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: path + " is a submodule. TierPlan doesn't support submodules.",
	}
}

// SymlinkOutsideProblem is the blocking problem for a link in the repository that points outside the
// stack folder (D-10). The importer raises it for a link it will not follow.
func SymlinkOutsideProblem(path string) Problem {
	return Problem{
		Severity: SeverityError, Code: CodeSymlinkOutside, File: path, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: path + " is a link to somewhere outside the stack folder.",
	}
}

// SymlinkUnsupportedProblem is the blocking problem for a link that stays inside the stack folder but is not a link
// to a regular file: a link to another link or to a folder, or to nothing. Only one hop to a file is followed
// (D-10). It has the code of SymlinkOutsideProblem.
func SymlinkUnsupportedProblem(path string) Problem {
	return Problem{
		Severity: SeverityError, Code: CodeSymlinkOutside, File: path, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: path + " is a link that doesn't lead to a file in the stack folder. TierPlan follows a link to a file only.",
	}
}
