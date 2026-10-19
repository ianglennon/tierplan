// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/template"
	"go.yaml.in/yaml/v3"
)

// Hidden is the sentinel a secret value is shown as wherever a value is listed: the same text the
// API sends for a secret it withholds (redact.Placeholder). stacks imports no other package of the
// server, so the text is written here and a test in package api holds the two equal.
const Hidden = "<hidden>"

// The sources a resolved variable's value comes from (D-01): the stack's table, the default written
// in the compose file, or nowhere.
const (
	SourceTable   = "table"
	SourceDefault = "default"
	SourceUnset   = "unset"
)

// Place is somewhere a variable is used: a line of a stack file.
type Place struct {
	File string
	Line int
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
	Name       string
	Value      string
	Source     string
	Secret     bool
	Required   bool
	Unused     bool
	Default    string
	HasDefault bool
	Places     []Place
}

var varNameFormRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Refs lists the variables the working copy's compose file refers to, sorted by name: every
// $NAME, ${NAME}, ${NAME:-default}, ${NAME-default}, ${NAME:?message} and the like in a value of
// the file, wherever the YAML puts it. Only compose.yaml is read: it is the one file compose-go
// interpolates, so a ${...} in a configuration file the stack carries is that file's own text and
// not a variable of the stack. A reference in a YAML comment is not one either. A file that is
// not valid YAML has no references.
func Refs(c WorkingCopy) []VarRef {
	content := c.Compose()
	if len(content) == 0 || len(content) > MaxFileBytes {
		return nil
	}
	ix, err := parseIndex(content)
	if err != nil {
		return nil
	}
	acc := map[string]*VarRef{}
	for _, root := range ix.docs {
		collectRefs(root, acc, 0)
	}
	out := make([]VarRef, 0, len(acc))
	for _, r := range acc {
		sort.SliceStable(r.Places, func(i, j int) bool { return r.Places[i].Line < r.Places[j].Line })
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// maxRefDepth bounds the walk of a YAML tree.
const maxRefDepth = 64

// collectRefs adds the references of every value under n to acc. Keys are not interpolated by
// compose-go, so they are not read; an alias is not followed, because the anchor it names is
// walked where it is written.
func collectRefs(n *yaml.Node, acc map[string]*VarRef, depth int) {
	if n == nil || depth > maxRefDepth {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			collectRefs(c, acc, depth+1)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			collectRefs(n.Content[i+1], acc, depth+1)
		}
	case yaml.ScalarNode:
		if strings.Contains(n.Value, "$") {
			addScalarRefs(n, acc)
		}
	}
}

// addScalarRefs reads the references of one scalar value into acc.
func addScalarRefs(n *yaml.Node, acc map[string]*VarRef) {
	found := template.ExtractVariables(map[string]any{"v": n.Value}, nil)
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
		r.Places = append(r.Places, Place{File: ComposeFile, Line: lineOf(n, name), Plain: !hasDefault && !required})
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
// compose file (D-01); the Bayplan server's own environment never takes part. A table entry with
// no value counts as no value, so the default is used and, with no default, the variable is
// required. A secret's value is Hidden.
func ResolveVars(refs []VarRef, vars []Var) []ResolvedVar {
	table := make(map[string]Var, len(vars))
	for _, v := range vars {
		table[v.Name] = v
	}
	lookup := func(name string) (string, bool) {
		v, ok := table[name]
		if !ok || v.Value == "" {
			return "", false
		}
		return v.Value, true
	}
	out := make([]ResolvedVar, 0, len(refs)+len(vars))
	seen := make(map[string]bool, len(refs))
	for _, r := range refs {
		seen[r.Name] = true
		rv := ResolvedVar{Name: r.Name, Default: r.Default, HasDefault: r.HasDefault, Places: r.Places}
		tv := table[r.Name]
		rv.Secret = tv.Secret
		switch value, ok := lookup(r.Name); {
		case ok:
			rv.Value, rv.Source = value, SourceTable
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
