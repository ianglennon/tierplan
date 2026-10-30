// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stacks

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/compose-spec/compose-go/v2/template"
	"go.yaml.in/yaml/v3"
)

// Hidden is the sentinel a secret value is shown as wherever a value is listed: the same text the
// API sends for a secret it withholds (redact.Placeholder). stacks imports no other package of the
// server, so the text is written here and a test in package api holds the two equal.
const Hidden = "<hidden>"

// The sources a resolved variable's value comes from (D-01, D-07): the stack's table, the .env
// committed in the repository a git stack came from, the default written in the compose file, or
// nowhere.
const (
	SourceTable   = "table"
	SourceRepoEnv = "repo-env"
	SourceDefault = "default"
	SourceUnset   = "unset"
)

// Place is somewhere a variable is used: a line of a stack file.
type Place struct {
	File string
	Line int
	// Col is the 1-based column the reference's value (or a bare environment name) starts at.
	Col int
	// Plain is true when the reference at this place has no default and no ":?" message, so a
	// variable with no value is an error here and only here.
	Plain bool
}

// VarRef is a variable the compose file refers to, with where it does so. Default is the text
// after ":-" or "-" of the first reference that has one; HasDefault is true when any reference has
// one, even an empty one (${NAME:-}). Required is true when a reference is ${NAME:?message}.
type VarRef struct {
	Name       string
	Default    string
	HasDefault bool
	Required   bool
	Places     []Place
}

// ResolvedVar is one row of the Variables pane as the server computes it: the value a deploy will
// use for the variable and where that value comes from. A secret's Value is Hidden. Unused is true
// for a variable of the table that no file refers to; Required is true when the variable has no
// value in the table and no default, so a deploy cannot go ahead.
type ResolvedVar struct {
	Name   string
	Value  string
	Source string
	Secret bool
	// SecretLike is true for a value taken from the repository's .env whose name looks like a secret
	// (SecretLikeName). Its Value is Hidden, as a secret variable's is.
	SecretLike bool
	Required   bool
	Unused     bool
	Default    string
	HasDefault bool
	Places     []Place
}

var varNameFormRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Refs lists the variables the working copy's compose files refer to, sorted by name: every
// $NAME, ${NAME}, ${NAME:-default}, ${NAME-default}, ${NAME:?message} and the like in a value of
// a compose file, wherever the YAML puts it. Only the compose files are read: they are the files
// compose-go interpolates, so a ${...} in a configuration file the stack carries is that file's own
// text and not a variable of the stack. A reference in a YAML comment is not one either. A file that
// is not valid YAML has no references. Each place carries the compose file it is in and its line;
// places are in file order, then line, then column.
func Refs(c WorkingCopy) []VarRef {
	acc := map[string]*VarRef{}
	order := map[string]int{}
	for i, cf := range c.ComposeContents() {
		order[cf.Path] = i
		if len(cf.Content) == 0 || len(cf.Content) > MaxFileBytes {
			continue
		}
		ix, err := parseIndex(cf.Content)
		if err != nil {
			continue
		}
		for _, root := range ix.docs {
			collectRefs(root, cf.Path, acc, nil, 0)
		}
	}
	out := make([]VarRef, 0, len(acc))
	for _, r := range acc {
		sort.SliceStable(r.Places, func(i, j int) bool {
			a, b := r.Places[i], r.Places[j]
			if order[a.File] != order[b.File] {
				return order[a.File] < order[b.File]
			}
			if a.Line != b.Line {
				return a.Line < b.Line
			}
			return a.Col < b.Col
		})
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// maxRefDepth bounds the walk of a YAML tree.
const maxRefDepth = 64

// collectRefs adds the references of every value under n to acc. path is the keys from the top of
// the document to n, so the environment of a service can be told from any other map. Keys are not
// interpolated by compose-go, so they are not read; an alias is not followed, because the anchor
// it names is walked where it is written.
func collectRefs(n *yaml.Node, file string, acc map[string]*VarRef, path []string, depth int) {
	if n == nil || depth > maxRefDepth {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			collectRefs(c, file, acc, path, depth+1)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			collectRefs(c, file, acc, append(path, "[]"), depth+1)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i].Value, n.Content[i+1]
			if len(path) == 2 && path[0] == "services" && key == "environment" {
				collectEnvironment(val, file, acc, depth+1)
				continue
			}
			collectRefs(val, file, acc, append(path, key), depth+1)
		}
	case yaml.ScalarNode:
		if strings.Contains(n.Value, "$") {
			addScalarRefs(n, file, acc)
		}
	}
}

// collectEnvironment reads the environment of a service. Besides the references in its values, a
// bare entry ("- FOO", or "FOO:" with nothing after it) is a reference to the variable FOO: the
// Compose spec passes it through from the environment of the host, and Coaming takes it from the
// table instead.
func collectEnvironment(n *yaml.Node, file string, acc map[string]*VarRef, depth int) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if val.Kind == yaml.ScalarNode && val.Tag == "!!null" {
				addBare(acc, file, key.Value, key.Line, key.Column)
				continue
			}
			collectRefs(val, file, acc, nil, depth+1)
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if item.Kind == yaml.ScalarNode && !strings.ContainsAny(item.Value, "=$") {
				addBare(acc, file, item.Value, item.Line, item.Column)
				continue
			}
			collectRefs(item, file, acc, nil, depth+1)
		}
	}
}

// addBare records a bare environment entry as a plain reference to name.
func addBare(acc map[string]*VarRef, file, name string, line, col int) {
	if !varNameFormRe.MatchString(name) {
		return
	}
	r := acc[name]
	if r == nil {
		r = &VarRef{Name: name}
		acc[name] = r
	}
	r.Places = append(r.Places, Place{File: file, Line: line, Col: col, Plain: true})
}

// addScalarRefs reads the references of one scalar value into acc.
func addScalarRefs(n *yaml.Node, file string, acc map[string]*VarRef) {
	found := template.ExtractVariables(map[string]any{"v": n.Value}, nil)
	// A name that stands inside the default of another is read only when that one is empty, so an
	// unset value there is not an error where the name stands.
	nested := map[string]bool{}
	for _, v := range found {
		for _, text := range []string{v.DefaultValue, v.PresenceValue} {
			if text == "" {
				continue
			}
			for inner := range template.ExtractVariables(map[string]any{"v": text}, nil) {
				nested[inner] = true
			}
		}
	}
	for name, v := range found {
		if !varNameFormRe.MatchString(name) {
			continue
		}
		op := operatorOf(n.Value, name)
		hasDefault := strings.Contains(op, "-") || strings.Contains(op, "+")
		required := strings.Contains(op, "?") || v.Required
		r := acc[name]
		if r == nil {
			r = &VarRef{Name: name}
			acc[name] = r
		}
		if hasDefault {
			r.HasDefault = true
			if r.Default == "" {
				r.Default = v.DefaultValue
			}
		}
		r.Required = r.Required || required
		r.Places = append(r.Places, Place{
			File: file, Line: lineOf(n, name), Col: n.Column,
			Plain: !hasDefault && !required && !nested[name],
		})
	}
}

// operatorOf is the operator (":-", "-", ":+", "+", ":?" or "?") of the first braced reference to
// name in s, or "" for a plain $NAME or ${NAME}.
func operatorOf(s, name string) string {
	prefix := "${" + name
	for from := 0; from < len(s); {
		i := strings.Index(s[from:], prefix)
		if i < 0 {
			return ""
		}
		rest := s[from+i+len(prefix):]
		from += i + len(prefix)
		for _, op := range [...]string{":-", ":+", ":?", "-", "+", "?"} {
			if strings.HasPrefix(rest, op) {
				return op
			}
		}
		if strings.HasPrefix(rest, "}") {
			return ""
		}
	}
	return ""
}

// lineOf is the 1-based line of the first reference to name in the scalar n. A block scalar's text
// starts on the line after its indicator, so its line is found by counting the newlines before the
// reference; any other scalar is reported on its first line.
func lineOf(n *yaml.Node, name string) int {
	if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0 {
		return n.Line
	}
	at := -1
	for _, form := range []string{"${" + name, "$" + name} {
		if i := strings.Index(n.Value, form); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	if at < 0 {
		return n.Line + 1
	}
	return n.Line + 1 + strings.Count(n.Value[:at], "\n")
}

// ResolveVars computes the row of every variable the files refer to and every variable of the
// table, sorted by name. The precedence is the stack's table, then the default written in the
// compose file (D-01); the Coaming server's own environment never takes part. A table entry with
// no value counts as no value, so the default is used and, with no default, the variable is
// required. A secret's value is Hidden. It is ResolveVarsLayered with no repository .env.
func ResolveVars(refs []VarRef, vars []Var) []ResolvedVar {
	return ResolveVarsLayered(refs, vars, nil)
}

// ResolveVarsLayered is ResolveVars for a stack made from a git repository: a value resolves from the
// table first, then from the repository's .env (repoEnv, see RepoEnv), then from the default written in
// the compose file, and a variable with none of them is unset and Required (D-07, GIT-08). A blank
// value in the table or in the .env is no value. A repoEnv value whose name looks like a secret is
// Hidden and marked SecretLike, because it is committed to the repository but must not be echoed.
func ResolveVarsLayered(refs []VarRef, vars []Var, repoEnv map[string]string) []ResolvedVar {
	table := make(map[string]Var, len(vars))
	for _, v := range vars {
		table[v.Name] = v
	}
	fromTable := func(name string) (string, bool) {
		v, ok := table[name]
		if !ok || v.Value == "" {
			return "", false
		}
		return v.Value, true
	}
	fromRepo := func(name string) (string, bool) {
		v, ok := repoEnv[name]
		return v, ok && v != ""
	}
	lookup := func(name string) (string, bool) {
		if v, ok := fromTable(name); ok {
			return v, true
		}
		return fromRepo(name)
	}
	out := make([]ResolvedVar, 0, len(refs)+len(vars))
	seen := make(map[string]bool, len(refs))
	for _, r := range refs {
		seen[r.Name] = true
		rv := ResolvedVar{Name: r.Name, Default: r.Default, HasDefault: r.HasDefault, Places: r.Places}
		tv := table[r.Name]
		rv.Secret = tv.Secret
		tableValue, inTable := fromTable(r.Name)
		repoValue, inRepo := fromRepo(r.Name)
		switch {
		case inTable:
			rv.Value, rv.Source = tableValue, SourceTable
		case inRepo:
			rv.Value, rv.Source = repoValue, SourceRepoEnv
			if SecretLikeName(r.Name) {
				rv.SecretLike, rv.Value = true, Hidden
			}
		case r.HasDefault:
			rv.Value, rv.Source = substituteDefault(r.Default, lookup), SourceDefault
		default:
			rv.Source, rv.Required = SourceUnset, true
		}
		if rv.Secret && rv.Source == SourceTable {
			rv.Value = Hidden
		}
		out = append(out, rv)
	}
	for _, v := range vars {
		if seen[v.Name] {
			continue
		}
		rv := ResolvedVar{Name: v.Name, Value: v.Value, Source: SourceTable, Secret: v.Secret, Unused: true}
		if v.Secret && v.Value != "" {
			rv.Value = Hidden
		}
		out = append(out, rv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// substituteDefault fills the variables a default value refers to ("${HOST:-${DOMAIN}}") from the
// table. A default that cannot be filled is shown as written.
func substituteDefault(def string, lookup func(string) (string, bool)) string {
	if !strings.Contains(def, "$") {
		return def
	}
	out, err := template.SubstituteWithOptions(def, template.Mapping(lookup), template.WithoutLogging)
	if err != nil {
		return def
	}
	return out
}

// The reasons CheckVars refuses a table.
var (
	ErrVarNameInvalid = errors.New("stacks: a variable name is letters, digits and underscore, and does not start with a digit")
	ErrVarNameTaken   = errors.New("stacks: a variable name is used twice")
	ErrSecretTooShort = errors.New("stacks: a secret value is at least 4 characters")
)

// VarError says which variable CheckVars refused and why.
type VarError struct {
	Name string
	Err  error
}

func (e *VarError) Error() string { return "stacks: variable " + e.Name + ": " + e.Err.Error() }

// Unwrap returns the reason.
func (e *VarError) Unwrap() error { return e.Err }

// MaxVarName is the longest variable name, in bytes.
const MaxVarName = 128

// CheckVars holds a variable table to its rules: every name is letters, digits and underscore, does
// not start with a digit and is at most MaxVarName long, no name is used twice, and a secret value
// is at least MinSecretLen characters long. It returns a *VarError for the first variable that
// breaks one.
func CheckVars(vars []Var) error { return checkVars(vars, true) }

// CheckVarNames is CheckVars without the rule about the length of a secret. The check that runs as
// a person types uses it: a secret that is half typed is not yet a mistake.
func CheckVarNames(vars []Var) error { return checkVars(vars, false) }

func checkVars(vars []Var, secrets bool) error {
	seen := make(map[string]struct{}, len(vars))
	for _, v := range vars {
		if len(v.Name) > MaxVarName || !varNameFormRe.MatchString(v.Name) {
			return &VarError{Name: v.Name, Err: ErrVarNameInvalid}
		}
		if _, dup := seen[v.Name]; dup {
			return &VarError{Name: v.Name, Err: ErrVarNameTaken}
		}
		seen[v.Name] = struct{}{}
		if secrets && v.Secret && v.Value != "" && utf8.RuneCountInString(v.Value) < MinSecretLen {
			return &VarError{Name: v.Name, Err: ErrSecretTooShort}
		}
	}
	return nil
}
