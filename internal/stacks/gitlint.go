// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"sort"
	"strings"
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
// loaded may be nil, for a copy whose compose files did not load. The problems are not sorted.
//
// This part of the rules warns about a secret-like key in the repository's .env. A key the table
// already overrides with a value is not reported: the warning's own advice has been followed.
func GitLint(c WorkingCopy, _ *Loaded) []Problem {
	var out []Problem
	out = append(out, envSecretLikeProblems(c)...)
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
