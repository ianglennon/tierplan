// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package compose is the D-13 privileged-compose checker. Given the text of a
// compose file it returns every host-escalation feature in it (privileged
// containers, added capabilities, devices, host namespaces, disabled
// confinement, binds of any host path outside the data roots (/srv, /mnt,
// /media, below /var/log), local-driver volumes that bind or mount such paths
// or block devices, and top-level secrets and configs whose file: source is
// outside the project directory), each with the service, key, value, line and
// column, so a deploy by anyone below env Admin can be refused with the
// offending keys named (AUTH-10).
//
// Check is a first pass over the raw YAML. It resolves anchors, aliases and <<
// merge keys, but it cannot see include: or extends: targets or the values
// environment interpolation would produce. A checked field that contains an
// interpolation is therefore its own finding (rule "unresolvable") and the
// check fails closed. CheckResolved is the second pass the stack phases run: it
// takes the variables of the stack and judges a value by what interpolation
// gives, at the line and column the value was written, so a bind source written
// as ${DATA} is judged by DATA's value. A name that has no value and no default
// stays "unresolvable".
//
// The package is pure: it reads bytes and returns findings. It deliberately
// does not import internal/rbac; the policy hook in package rbac imports this
// package, not the other way round.
package compose

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/compose-spec/compose-go/v2/template"
	"go.yaml.in/yaml/v3"
)

// MaxSize is the largest compose file Check will read. A larger file is
// refused before it is parsed.
const MaxSize = 1 << 20

var (
	// ErrTooLarge is returned for a file larger than MaxSize.
	ErrTooLarge = errors.New("compose: file larger than 1 MiB")
	// ErrInvalid is returned for YAML that does not parse, or that is so
	// deeply aliased that it cannot be checked within the work budget. Either
	// way the file is refused, never silently passed.
	ErrInvalid = errors.New("compose: not valid compose YAML")
)

// Work limits. A hostile file can build an exponential number of merges out of
// a handful of aliases; the budget turns that into a refusal.
const (
	maxDepth  = 32
	maxBudget = 250_000
)

// Finding is one host-escalation feature found in a compose file.
type Finding struct {
	// Service is the service name; empty for a top-level volume finding.
	Service string
	// Key is the offending key: the service attribute (privileged, cap_add,
	// ...) or, for a top-level volume, its full path (volumes.data.driver_opts.device).
	Key string
	// Value is the offending value as written (a list entry for list keys).
	Value string
	// Line and Column locate the value (1-based). A value that came from an
	// alias or a merge key is reported at the alias.
	Line   int
	Column int
	// Rule names the rule that fired, as listed in rules.go, or "unresolvable".
	Rule string
	// Why says in a few words why the feature needs env Admin.
	Why string
}

// Check returns every host-escalation finding in src, ordered by line, then
// column, then rule. A file with no services returns no findings and no error.
// A file over MaxSize returns ErrTooLarge; YAML that does not parse returns an
// error wrapping ErrInvalid.
func Check(src []byte) ([]Finding, error) { return check(src, nil) }

// CheckResolved returns every host-escalation finding in src, judging each
// interpolated value by what lookup gives for its variables. lookup answers a
// variable's value and whether it has one; a blank value counts as none when
// the caller says so, as the stack's table does. A nil lookup is Check. The
// findings, their order and their errors are Check's.
func CheckResolved(src []byte, lookup func(name string) (string, bool)) ([]Finding, error) {
	return check(src, lookup)
}

func check(src []byte, lookup func(name string) (string, bool)) ([]Finding, error) {
	if len(src) > MaxSize {
		return nil, ErrTooLarge
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	c := &checker{budget: maxBudget, lookup: lookup}
	// compose-go merges every document of a multi-document file, so every
	// document is checked.
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		c.document(&doc)
		if c.err != nil {
			return nil, c.err
		}
	}
	return finish(c.found), nil
}

// finish orders findings by (line, column, rule) with a stable sort and drops
// exact duplicates, which a value merged in twice would otherwise repeat.
func finish(fs []Finding) []Finding {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		return a.Rule < b.Rule
	})
	var out []Finding
	seen := make(map[Finding]struct{}, len(fs))
	for _, f := range fs {
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

// NeedsEnvAdmin reports whether the findings require env Admin: true exactly
// when there is at least one.
func NeedsEnvAdmin(fs []Finding) bool { return len(fs) > 0 }

// Format renders a finding the way the stack phases name an offending key:
// services.<name>.<key> (line N): <value>. A top-level volume finding has no
// service and renders its full key path.
func Format(f Finding) string {
	if f.Service == "" {
		return fmt.Sprintf("%s (line %d): %s", f.Key, f.Line, f.Value)
	}
	return fmt.Sprintf("services.%s.%s (line %d): %s", f.Service, f.Key, f.Line, f.Value)
}

// checker walks one or more YAML documents. err holds the first refusal (work
// limit exceeded); once set, every walker stops.
type checker struct {
	budget int
	err    error
	found  []Finding
	// lookup gives the value of a variable, or nil when the caller has no table.
	lookup func(name string) (string, bool)
}

// pos is a reporting position that overrides a node's own.
type pos struct{ line, col int }

// val is a YAML node plus the position to report for anything found under it.
// at is set once the walk has gone through an alias, so a value that is
// physically elsewhere in the file is reported where the file used it.
type val struct {
	n  *yaml.Node
	at *pos
}

func (v val) line() int {
	if v.at != nil {
		return v.at.line
	}
	return v.n.Line
}

func (v val) column() int {
	if v.at != nil {
		return v.at.col
	}
	return v.n.Column
}

// entry is one key of a mapping after merge keys are applied.
type entry struct {
	name string
	val  val
}

// spend charges one unit of work and reports whether the walk may continue.
func (c *checker) spend() bool {
	if c.err != nil {
		return false
	}
	c.budget--
	if c.budget < 0 {
		c.err = fmt.Errorf("%w: aliases and merges are nested too deeply to check", ErrInvalid)
		return false
	}
	return true
}

// resolve follows alias nodes to the node they name, remembering the position
// of the outermost alias.
func (c *checker) resolve(v val) val {
	for steps := 0; v.n != nil && v.n.Kind == yaml.AliasNode; steps++ {
		if steps > maxDepth || !c.spend() {
			c.err = fmt.Errorf("%w: aliases are nested too deeply to check", ErrInvalid)
			return val{n: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}}
		}
		if v.at == nil {
			v.at = &pos{v.n.Line, v.n.Column}
		}
		v.n = v.n.Alias
	}
	if v.n == nil {
		return val{n: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}}
	}
	return v
}

// entries returns the keys of a mapping with its merge keys applied: explicit
// keys win over merged ones and an earlier merge source wins over a later one,
// as in the YAML merge-key type. Duplicate explicit keys are all kept, so an
// evasion by repeating a key is still seen. A value that is not a mapping has
// no entries.
func (c *checker) entries(v val, depth int) []entry {
	v = c.resolve(v)
	if v.n.Kind != yaml.MappingNode || !c.spend() {
		return nil
	}
	if depth > maxDepth {
		c.err = fmt.Errorf("%w: merge keys are nested too deeply to check", ErrInvalid)
		return nil
	}
	var out []entry
	var merges []val
	have := map[string]struct{}{}
	for i := 0; i+1 < len(v.n.Content); i += 2 {
		if !c.spend() {
			return nil
		}
		k := c.resolve(val{n: v.n.Content[i]})
		x := val{n: v.n.Content[i+1], at: v.at}
		if k.n.Kind == yaml.ScalarNode && k.n.ShortTag() == "!!merge" {
			merges = append(merges, x)
			continue
		}
		name := ""
		if k.n.Kind == yaml.ScalarNode {
			name = k.n.Value
		}
		have[name] = struct{}{}
		out = append(out, entry{name: name, val: x})
	}
	for _, m := range merges {
		m = c.resolve(m)
		sources := []val{m}
		if m.n.Kind == yaml.SequenceNode {
			sources = sources[:0]
			for _, item := range m.n.Content {
				if !c.spend() {
					return nil
				}
				sources = append(sources, val{n: item, at: m.at})
			}
		}
		for _, src := range sources {
			var add []entry
			for _, e := range c.entries(src, depth+1) {
				if _, ok := have[e.name]; !ok {
					add = append(add, e)
				}
			}
			for _, e := range add {
				have[e.name] = struct{}{}
				out = append(out, e)
			}
			if c.err != nil {
				return nil
			}
		}
	}
	return out
}

// document checks one YAML document.
func (c *checker) document(doc *yaml.Node) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return
	}
	root := val{n: doc.Content[0]}
	for _, top := range c.entries(root, 0) {
		switch top.name {
		case "services":
			for _, svc := range c.entries(top.val, 0) {
				c.service(svc)
			}
		case "volumes":
			for _, vol := range c.entries(top.val, 0) {
				c.volume(vol)
			}
		case "secrets":
			for _, sec := range c.entries(top.val, 0) {
				c.fileSource(secretFileRule, "secrets", sec)
			}
		case "configs":
			for _, cfg := range c.entries(top.val, 0) {
				c.fileSource(configFileRule, "configs", cfg)
			}
		}
	}
}

// service runs the service rule table over one service.
func (c *checker) service(svc entry) {
	fields := c.entries(svc.val, 0)
	for _, r := range serviceRules {
		for _, f := range fields {
			if f.name != r.Key {
				continue
			}
			for _, l := range r.Leaves(c, f.val) {
				c.report(r, svc.name, r.Key, l)
			}
		}
	}
}

// volume runs the top-level volume rule over one named volume.
func (c *checker) volume(vol entry) {
	for _, l := range volumeRule.Leaves(c, vol.val) {
		c.report(volumeRule, "", "volumes."+vol.name+"."+l.key, l)
	}
}

// fileSource runs a file: source rule over one top-level secret or config.
func (c *checker) fileSource(r rule, section string, e entry) {
	for _, l := range r.Leaves(c, e.val) {
		c.report(r, "", section+"."+e.name+"."+l.key, l)
	}
}

// report turns a checked leaf into a finding: unresolvable when the value
// holds an interpolation that cannot be filled in, the rule's own finding when
// the value (as filled in) is dangerous, and nothing otherwise. A value that
// was filled in is reported as the value it became, at the place it was
// written.
func (c *checker) report(r rule, service, key string, l leaf) {
	f := Finding{Service: service, Key: key, Value: l.text, Line: l.line, Column: l.col}
	text := l.text
	if resolved, ok := c.expand(l.text); ok && resolved != l.text {
		text = resolved
		if l.short {
			// A short-syntax volume entry was read whole; its source is what is judged.
			src, has := shortSource(text)
			if !has {
				return
			}
			text = src
		}
		f.Value = text
	}
	switch {
	case interpolated(text):
		f.Rule, f.Why = ruleUnresolvable, whyUnresolvable
		f.Value = l.text
	case l.forced:
		f.Rule, f.Why = r.Name, r.Why
	case r.Bad(text):
		f.Rule, f.Why = r.Name, r.Why
	default:
		return
	}
	c.found = append(c.found, f)
}

// expand fills in the interpolations of text from the caller's table. ok is
// false when there is no table, or a name has no value and no default, or a
// required name is missing: the text cannot be verified. A text with no
// interpolation is returned as it is.
func (c *checker) expand(text string) (string, bool) {
	if !interpolated(text) {
		return text, true
	}
	if c.lookup == nil {
		return text, false
	}
	missing := false
	out, err := template.SubstituteWithOptions(text, template.Mapping(c.lookup), template.WithoutLogging,
		template.WithReplacementFunction(func(sub string, m template.Mapping, cfg *template.Config) (string, error) {
			value, applied, err := template.DefaultReplacementAppliedFunc(sub, m, cfg)
			if err == nil && !applied {
				missing = true
			}
			return value, err
		}))
	if err != nil || missing {
		return text, false
	}
	return out, true
}
