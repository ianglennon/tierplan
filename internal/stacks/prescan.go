// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Prescan reads the raw YAML of a compose file before anything loads it, and refuses what could
// make a load reach outside the stack (RESEARCH Q5, layers 1 to 4; T-07-26, T-07-27):
//
//   - include, extends and label_file are errors: each names a file (or a URL) the loader would
//     open, and a stack has no directory to open it in;
//   - every env_file entry, and every top-level secrets.*.file and configs.*.file, must name a
//     file of the stack by a path ValidPath accepts, and the file must be one of files. A path
//     that is not valid is path_illegal, one that is valid but not there is file_missing;
//   - a file whose aliases and merges expand past the work budget is compose_too_complex.
//
// It returns the problems, and refs: every stack file the compose file refers to, so the caller
// can tell which files are unused. It reads only the bytes it is given and never opens a file. A
// file that is not YAML returns nothing: the load reports that.
func Prescan(compose []byte, files FileSet) (problems Problems, refs []string) {
	if len(compose) > MaxFileBytes {
		return Problems{tooLargeProblem()}, nil
	}
	w := &walker{budget: maxBudget, files: files, refs: map[string]struct{}{}}
	roots, ok := decodeRoots(compose)
	if !ok {
		return nil, nil
	}
	// The whole tree is measured first, following every alias, so a file built to expand into
	// millions of nodes is stopped here and not by the first check that happens to look at it.
	for _, root := range roots {
		w.measure(root, 0)
		if w.err != nil {
			return Problems{tooComplexProblem()}, nil
		}
	}
	for _, root := range roots {
		w.document(root)
		if w.err != nil {
			return Problems{tooComplexProblem()}, nil
		}
	}
	refs = make([]string, 0, len(w.refs))
	for p := range w.refs {
		refs = append(refs, p)
	}
	sort.Strings(refs)
	return w.found.Sorted(), refs
}

// decodeRoots reads every YAML document of a compose file and returns the root node of each. It
// reports false for text that is not YAML, which the load reports.
func decodeRoots(compose []byte) ([]*yaml.Node, bool) {
	dec := yaml.NewDecoder(bytes.NewReader(compose))
	var roots []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return roots, true
		}
		if err != nil {
			return nil, false
		}
		if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
			roots = append(roots, doc.Content[0])
		}
	}
}

func tooLargeProblem() Problem {
	return Problem{
		Severity: SeverityError, Code: CodeComposeTooLarge, File: ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: "compose.yaml is over 1 MiB. Split it up or remove what it doesn't need.",
	}
}

func tooComplexProblem() Problem {
	return Problem{
		Severity: SeverityError, Code: CodeComposeTooComplex, File: ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		Message: "The compose file is too complex to check.",
	}
}

// Work limits. A hostile file can build an exponential number of nodes out of a handful of
// aliases; the budget turns that into a refusal. The numbers are those of internal/rbac/compose.
const (
	maxDepth   = 32
	maxBudget  = 250_000
	maxNesting = 128
)

// pos is a reporting position that overrides a node's own.
type pos struct{ line, col int }

// val is a YAML node plus the position to report for anything found under it. at is set once the
// walk has gone through an alias, so a value that is physically elsewhere in the file is reported
// where the file used it.
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

func (v val) col() int {
	if v.at != nil {
		return v.at.col
	}
	return v.n.Column
}

// entry is one key of a mapping after merge keys are applied.
type entry struct {
	name string
	key  *yaml.Node
	val  val
}

// where is the position to report for the key of an entry: the alias that brought it in, or the
// key itself.
func (e entry) where() (line, col int) {
	if e.val.at != nil {
		return e.val.at.line, e.val.at.col
	}
	return e.key.Line, e.key.Column
}

type walker struct {
	budget int
	err    error
	files  FileSet
	refs   map[string]struct{}
	// secrets are the paths named by a top-level secrets.*.file, whether or not the stack has the
	// file. Nil when the walk does not collect them.
	secrets map[string]struct{}
	found   Problems
}

// spend charges one unit of work and reports whether the walk may continue.
func (w *walker) spend() bool {
	if w.err != nil {
		return false
	}
	w.budget--
	if w.budget < 0 {
		w.err = errors.New("work budget exceeded")
		return false
	}
	return true
}

// measure visits every node under n, following aliases, and charges one unit for each. A file
// nested deeper than maxNesting is over the budget too.
func (w *walker) measure(n *yaml.Node, depth int) {
	if n == nil || !w.spend() {
		return
	}
	if depth > maxNesting {
		w.err = errors.New("nested too deeply")
		return
	}
	if n.Kind == yaml.AliasNode {
		w.measure(n.Alias, depth+1)
		return
	}
	for _, c := range n.Content {
		w.measure(c, depth+1)
		if w.err != nil {
			return
		}
	}
}

// resolve follows alias nodes to the node they name, remembering the position of the outermost
// alias.
func (w *walker) resolve(v val) val {
	for steps := 0; v.n != nil && v.n.Kind == yaml.AliasNode; steps++ {
		if steps > maxDepth || !w.spend() {
			w.err = errors.New("aliases nested too deeply")
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

// entries returns the keys of a mapping with its merge keys applied: explicit keys win over
// merged ones and an earlier merge source wins over a later one, as in the YAML merge-key type.
// Duplicate explicit keys are all kept, so repeating a key does not hide one. A value that is not
// a mapping has no entries.
func (w *walker) entries(v val, depth int) []entry {
	v = w.resolve(v)
	if v.n.Kind != yaml.MappingNode || !w.spend() {
		return nil
	}
	if depth > maxDepth {
		w.err = errors.New("merge keys nested too deeply")
		return nil
	}
	var out []entry
	var merges []val
	have := map[string]struct{}{}
	for i := 0; i+1 < len(v.n.Content); i += 2 {
		if !w.spend() {
			return nil
		}
		k := w.resolve(val{n: v.n.Content[i]})
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
		out = append(out, entry{name: name, key: v.n.Content[i], val: x})
	}
	for _, m := range merges {
		m = w.resolve(m)
		sources := []val{m}
		if m.n.Kind == yaml.SequenceNode {
			sources = sources[:0]
			for _, item := range m.n.Content {
				if !w.spend() {
					return nil
				}
				sources = append(sources, val{n: item, at: m.at})
			}
		}
		for _, src := range sources {
			var add []entry
			for _, e := range w.entries(src, depth+1) {
				if _, ok := have[e.name]; !ok {
					add = append(add, e)
				}
			}
			for _, e := range add {
				have[e.name] = struct{}{}
				out = append(out, e)
			}
			if w.err != nil {
				return nil
			}
		}
	}
	return out
}

// document checks one YAML document.
func (w *walker) document(root *yaml.Node) {
	for _, top := range w.entries(val{n: root}, 0) {
		switch top.name {
		case "include":
			w.blocking(top, CodeIncludeUnsupported, "include isn't supported. Put everything in compose.yaml.")
		case "services":
			for _, svc := range w.entries(top.val, 0) {
				w.service(svc)
			}
		case "secrets", "configs":
			for _, obj := range w.entries(top.val, 0) {
				for _, f := range w.entries(obj.val, 0) {
					if f.name == "file" {
						w.fileRef(top.name+"."+obj.name+".file", f.val, false)
						if top.name == "secrets" {
							w.secretRef(f.val)
						}
					}
				}
			}
		}
		if w.err != nil {
			return
		}
	}
}

// secretRef notes the path a top-level secret's file names, when it is a path a stack could have.
func (w *walker) secretRef(v val) {
	if w.secrets == nil {
		return
	}
	v = w.resolve(v)
	if v.n.Kind != yaml.ScalarNode || v.n.ShortTag() != "!!str" {
		return
	}
	if path := strings.TrimPrefix(v.n.Value, "./"); ValidPath(path) == nil {
		w.secrets[path] = struct{}{}
	}
}

// service checks the keys of one service that name a file.
func (w *walker) service(svc entry) {
	for _, f := range w.entries(svc.val, 0) {
		switch f.name {
		case "extends":
			w.blocking(f, CodeExtendsUnsupported, "extends isn't supported. Copy the settings you need into the service.")
		case "label_file":
			w.blocking(f, CodeLabelFileUnsupported, "label_file isn't supported. Put the labels under labels in compose.yaml.")
		case "env_file":
			w.envFile(f.val)
		}
	}
}

// blocking records an error at the key of an entry.
func (w *walker) blocking(e entry, code, message string) {
	line, col := e.where()
	w.found = append(w.found, Problem{
		Severity: SeverityError, Code: code, File: ComposeFile, Key: e.name, Message: message,
		Line: line, Col: col, EndLine: line, EndCol: col + utf8.RuneCountInString(e.name),
	})
}

// envFile checks every path an env_file value names: a string, a list of strings, or a list of
// maps with a path (the long form also carries required and format).
func (w *walker) envFile(v val) {
	v = w.resolve(v)
	switch v.n.Kind {
	case yaml.ScalarNode:
		w.fileRef("env_file", v, false)
	case yaml.SequenceNode:
		for _, item := range v.n.Content {
			if !w.spend() {
				return
			}
			it := w.resolve(val{n: item, at: v.at})
			switch it.n.Kind {
			case yaml.ScalarNode:
				w.fileRef("env_file", it, false)
			case yaml.MappingNode:
				w.envFileMap(it)
			}
		}
	case yaml.MappingNode:
		w.envFileMap(v)
	}
}

// envFileMap checks the path of a long-form entry. With required: false the file may be absent
// (Bayplan skips it when it resolves the env_file), so a missing one is not an error; a path that
// is not allowed still is.
func (w *walker) envFileMap(v val) {
	entries := w.entries(v, 0)
	optional := false
	for _, f := range entries {
		if f.name == "required" {
			r := w.resolve(f.val).n
			optional = r.Kind == yaml.ScalarNode && r.ShortTag() == "!!bool" && strings.EqualFold(r.Value, "false")
		}
	}
	for _, f := range entries {
		if f.name == "path" {
			w.fileRef("env_file", f.val, optional)
		}
	}
}

// fileRef checks one value that names a stack file. Only a plain string can: anything else is the
// load's to refuse. An optional reference (an env_file with required: false) is not an error when
// the file is absent, and is not counted as used.
func (w *walker) fileRef(key string, v val, optional bool) {
	v = w.resolve(v)
	if v.n.Kind != yaml.ScalarNode || v.n.ShortTag() != "!!str" {
		return
	}
	ref := v.n.Value
	line, col := v.line(), v.col()
	width := utf8.RuneCountInString(ref)
	if v.at != nil {
		width = 1
	}
	path := strings.TrimPrefix(ref, "./")
	problem := Problem{Severity: SeverityError, File: ComposeFile, Key: key, Line: line, Col: col, EndLine: line, EndCol: col + max(width, 1)}
	switch {
	case ValidPath(path) != nil:
		problem.Code = CodePathIllegal
		problem.Message = fmt.Sprintf("“%s” isn't allowed. Stack files use a path inside the stack, without “..” or a leading /.", ref)
	default:
		if _, ok := w.files[path]; !ok {
			if optional {
				return
			}
			problem.Code = CodeFileMissing
			problem.Message = fmt.Sprintf("%s “%s” isn't one of this stack's files. Add it under Files.", key, ref)
			break
		}
		w.refs[path] = struct{}{}
		return
	}
	w.found = append(w.found, problem)
}

// MaxPorts is the most ports a compose file's ports: and expose: ranges may add up to.
const MaxPorts = 20000

// ErrPortsTooMany, ErrPortRangeInvalid and ErrComposeTooComplex are the refusals of PortBudget.
var (
	ErrPortsTooMany      = errors.New("stacks: the port ranges add up to too many ports")
	ErrPortRangeInvalid  = errors.New("stacks: a port range is not valid")
	ErrComposeTooComplex = errors.New("stacks: the compose file is too complex")
)

// PortsError is a refusal of PortBudget, located at the item that caused it.
type PortsError struct {
	Err       error
	Key       string
	Line, Col int
}

func (e *PortsError) Error() string {
	return fmt.Sprintf("stacks: %s at line %d: %v", e.Key, e.Line, e.Err)
}

// Unwrap returns the refusal.
func (e *PortsError) Unwrap() error { return e.Err }

// PortBudget is a stub until the budget is implemented.
func PortBudget(compose []byte, vars []Var) error { return nil }
