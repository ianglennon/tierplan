// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SecretUse knows which values of one compose file come from a secret variable (D-02). The loader
// and the converter describe a value they cannot use in their own words, and those words can hold
// part of the value (go-units quotes the digits of "Password2024!" as parsing "Password2024"). A
// replacement masker cannot hide an arbitrary part of a secret, so a problem about a field that
// uses a secret variable never carries those words: it carries a general sentence instead.
//
// The decision reads only the structure of the file: which nodes use a secret variable. It never
// reads the loader's or the converter's text. That text echoes the person's own input, so deciding
// on it would tell them whether their input is part of a secret (07-REVIEW-gaps2 CR-01). The masker
// stays as the second layer: it replaces whole values and their quoted and per-line forms in text,
// and makes no decision.
//
// A list item is judged by its whole list across every document of the file, because compose-go moves
// items between the file and its model (listPath).
//
// The zero SecretUse knows no secret. A SecretUse is a value, safe to share between goroutines once
// built.
type SecretUse struct {
	// ix is the compose file's index, nil when the file could not be read as YAML.
	ix *PositionIndex
	// used are the scalar nodes whose text uses a secret variable.
	used map[*yaml.Node]struct{}
	any  bool
}

// NewSecretUse indexes the working copy's compose files and finds the values that use one of its
// secret variables. When the file cannot be indexed (empty, over MaxFileBytes or not YAML), it
// answers as if every field used a secret whenever a secret variable has a value.
//
// Secret files are not included: the content of one never becomes the value of a compose field (it
// reaches a Docker object, or the container environment through env_file), so no loader or
// converter text can carry it. The masker still hides it everywhere.
func NewSecretUse(c WorkingCopy) SecretUse {
	contents := c.ComposeContents()
	for _, cf := range contents {
		if len(cf.Content) == 0 || len(cf.Content) > MaxFileBytes {
			return secretUseOf(nil, c.Vars)
		}
	}
	ix, err := parseIndexOf(contents)
	if err != nil {
		return secretUseOf(nil, c.Vars)
	}
	return secretUseOf(ix, c.Vars)
}

// secretUseOf is NewSecretUse over an index the caller already holds. A nil index stands for a file
// that could not be read.
func secretUseOf(ix *PositionIndex, vars []Var) SecretUse {
	s := SecretUse{ix: ix}
	var names []string
	for _, v := range vars {
		if v.Secret && v.Value != "" {
			names = append(names, v.Name)
		}
	}
	if len(names) == 0 {
		return SecretUse{}
	}
	if ix == nil {
		s.any = true
		return s
	}
	w := secretWalk{
		names:  map[string]struct{}{},
		used:   map[*yaml.Node]struct{}{},
		walked: map[walkKey]struct{}{},
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		w.names[n] = struct{}{}
		quoted[i] = regexp.QuoteMeta(n)
	}
	// $NAME or ${NAME, then a character that cannot continue a name (or the end). It reads the
	// text and never the template grammar, so a name inside a default or a ":+" value is found, and
	// "$$NAME", which is escaped, only fails closed.
	w.re = regexp.MustCompile(`\$\{?(?:` + strings.Join(quoted, "|") + `)(?:[^A-Za-z0-9_]|$)`)
	for _, root := range ix.docs {
		w.walk(root, false, 0)
	}
	s.used = w.used
	s.any = len(w.used) > 0
	return s
}

type walkKey struct {
	n   *yaml.Node
	env bool
}

// secretWalk records the nodes that use a secret variable.
type secretWalk struct {
	names  map[string]struct{}
	re     *regexp.Regexp
	used   map[*yaml.Node]struct{}
	walked map[walkKey]struct{}
}

// walk records the scalars under n that use a secret variable. Anchors are walked where they are
// written, so an alias is not followed, except under an environment, where a bare entry names a
// variable and the list may be written once under an anchor. env is true under the value of any
// key named environment, at any depth: a superset of services.<name>.environment, so an
// environment written in an anchor that a service merges is covered too.
func (w *secretWalk) walk(n *yaml.Node, env bool, depth int) {
	if n == nil || depth > maxPathDepth {
		return
	}
	if n.Kind == yaml.AliasNode {
		if env {
			w.walk(deref(n), env, depth+1)
		}
		return
	}
	k := walkKey{n, env}
	if _, done := w.walked[k]; done {
		return
	}
	w.walked[k] = struct{}{}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			w.walk(c, env, depth+1)
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if env && item.Kind == yaml.ScalarNode && !strings.ContainsAny(item.Value, "=$") {
				if _, ok := w.names[item.Value]; ok {
					w.used[item] = struct{}{}
				}
			}
			w.walk(item, env, depth+1)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if env && val.Kind == yaml.ScalarNode && val.Tag == "!!null" {
				if _, ok := w.names[key.Value]; ok {
					w.used[val] = struct{}{}
				}
			}
			w.walk(val, env || key.Value == "environment", depth+1)
		}
	case yaml.ScalarNode:
		if strings.Contains(n.Value, "$") && w.re.MatchString(n.Value) {
			w.used[n] = struct{}{}
		}
	}
}

// Any reports whether the compose file uses a secret variable anywhere.
func (s SecretUse) Any() bool { return s.any }

// Uses reports whether the value at path (a dotted path of the compose file, "" for the whole file)
// comes from a secret variable. It follows YAML aliases and merge keys. The loader names a Go field
// where the file writes a key (`Rate` for `rate`), so a path the file does not have word for word
// is matched to its keys a second time, ignoring case and underscores. Only a path that no key
// matches counts as using a secret variable whenever the file uses any (fail closed).
//
// A list item, written "[]", "[n]" or a number in the path (services.web.ports.[].target,
// services.web.volumes.1.tmpfs.size), is judged by its whole list across every document of the file
// (listPath): the loader's index is a position in compose-go's merged list, not in one document.
func (s SecretUse) Uses(path string) bool {
	if !s.any {
		return false
	}
	if s.ix == nil {
		return true
	}
	nodes := s.nodesAt(listPath(path))
	if len(nodes) == 0 {
		return true
	}
	return s.anyTouched(nodes)
}

// nodesAt are the nodes of the file that path names: word for word, then with keys matched loosely.
func (s SecretUse) nodesAt(path string) []*yaml.Node {
	nodes := s.ix.valuesAt(path)
	if len(nodes) == 0 {
		nodes = looseValuesAt(s.ix, path)
	}
	return nodes
}

// anyTouched reports whether the subtree under one of nodes holds a node that uses a secret variable.
func (s SecretUse) anyTouched(nodes []*yaml.Node) bool {
	seen := map[*yaml.Node]struct{}{}
	for _, n := range nodes {
		if s.touches(n, seen, 0) {
			return true
		}
	}
	return false
}

// listPath is path up to its first list segment, or path as it is when it has none. A list segment is
// "[]", "[" digits "]", or a run of ASCII digits only.
//
// The loader's index is a position in compose-go's merged list, not an item of the file: the documents
// of one file are concatenated, duplicate items are merged by EnforceUnicity even in a one-document
// file (the last one takes effect at the first one's place), and port ranges are expanded. So an index
// never pins one item of the file, and the item is judged by its whole list across every document. A
// mapping key that is all digits (a service named "1") is cut too, which judges a wider part of the
// file and fails closed. strconv.Atoi is not used, because it accepts a sign.
func listPath(path string) string {
	segs := strings.Split(path, ".")
	for i, seg := range segs {
		if isListSegment(seg) {
			return strings.Join(segs[:i], ".")
		}
	}
	return path
}

// isListSegment reports whether seg is "[]", "[n]" or n, where n is one or more ASCII digits.
func isListSegment(seg string) bool {
	if seg == "[]" {
		return true
	}
	if len(seg) >= 2 && seg[0] == '[' && seg[len(seg)-1] == ']' {
		seg = seg[1 : len(seg)-1]
	}
	if seg == "" {
		return false
	}
	for i := 0; i < len(seg); i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return false
		}
	}
	return true
}

// touches reports whether the subtree under n, aliases followed, holds a node that uses a secret
// variable. Each node is visited once, so an alias that doubles a tree cannot blow the walk up.
func (s SecretUse) touches(n *yaml.Node, seen map[*yaml.Node]struct{}, depth int) bool {
	n = deref(n)
	if n == nil || depth > maxPathDepth {
		return true
	}
	if _, done := seen[n]; done {
		return false
	}
	seen[n] = struct{}{}
	if _, ok := s.used[n]; ok {
		return true
	}
	for _, c := range n.Content {
		if s.touches(c, seen, depth+1) {
			return true
		}
	}
	return false
}

// looseKey is a key as the loose match reads it: lower case, without underscores.
func looseKey(k string) string {
	return strings.ReplaceAll(strings.ToLower(k), "_", "")
}

// looseValuesAt is valuesAt with keys matched ignoring case and underscores. It mirrors exact
// (merge keys, aliases, numeric segments for a sequence). When more than one key of a mapping
// matches a segment, it names nothing, so the caller fails closed.
func looseValuesAt(ix *PositionIndex, path string) []*yaml.Node {
	if ix == nil || path == "" {
		return nil
	}
	var out []*yaml.Node
	for _, root := range ix.docs {
		if v := looseNode(root, path, 0); v != nil {
			out = append(out, v)
		}
	}
	return out
}

func looseNode(n *yaml.Node, rest string, depth int) *yaml.Node {
	n = deref(n)
	if n == nil || depth >= maxPathDepth {
		return nil
	}
	switch n.Kind {
	case yaml.MappingNode:
		segs := strings.Split(rest, ".")
		matches, used := 0, 0
		var hit kvPair
		for _, e := range mergedEntries(n, 0) {
			// A key with a dot in it spans several segments of the path.
			k := strings.Count(e.key.Value, ".") + 1
			if k > len(segs) || looseKey(strings.Join(segs[:k], ".")) != looseKey(e.key.Value) {
				continue
			}
			matches++
			used, hit = k, e
		}
		if matches != 1 {
			return nil
		}
		if used == len(segs) {
			return deref(hit.val)
		}
		return looseNode(hit.val, strings.Join(segs[used:], "."), depth+1)
	case yaml.SequenceNode:
		seg, remainder, _ := strings.Cut(rest, ".")
		i, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(seg, "["), "]"))
		if err != nil || i < 0 || i >= len(n.Content) {
			return nil
		}
		if remainder == "" {
			return deref(n.Content[i])
		}
		return looseNode(n.Content[i], remainder, depth+1)
	}
	return nil
}

// Withholds is the one decision every caller uses: whether the words of a loader or converter text
// about the value at path are withheld. They are when the file uses a secret variable and either no
// path is named (the text may be about anything) or the named path uses one (Uses, which fails
// closed on a path it cannot find). It takes a path and no text, so no caller can decide on what a
// message says.
func (s SecretUse) Withholds(path string) bool {
	return s.any && (path == "" || s.Uses(path))
}

// WithheldMessage is the sentence a withheld problem carries in place of the words of the loader or
// the converter. Use it with Withholds. It says a value "comes from a secret variable" only when the
// path names nodes of the file and one of them uses a secret variable. A path the check cannot find,
// a file it cannot index, and a list item ("[]", "[n]" or a number in the path) get SecretFileMessage,
// which says only that the file uses a secret variable, so a plain item is never told its value comes
// from one. A list item is judged by its whole list across every document of the file.
func (s SecretUse) WithheldMessage(path string) string {
	if path == "" {
		return SecretValueMessage("")
	}
	if s.ix == nil || listPath(path) != path {
		return SecretFileMessage(path)
	}
	if nodes := s.nodesAt(path); len(nodes) > 0 && s.anyTouched(nodes) {
		return SecretValueMessage(path)
	}
	return SecretFileMessage(path)
}

// SecretValueMessage is the sentence a problem carries in place of the loader's or the converter's
// words for a value that comes from a secret variable. path is the dotted key of the field, or ""
// when the text names none. Both forms end with "because they could quote it."
func SecretValueMessage(path string) string {
	if path == "" {
		return "A value in compose.yaml isn't valid. The file uses a secret variable, so the details aren't shown here, because they could quote it."
	}
	return path + ": this value isn't valid. It comes from a secret variable, so the details aren't shown here, because they could quote it."
}

// SecretFileMessage is the sentence for a withheld value that the check cannot trace to a secret
// variable: the path is not in the file, the file cannot be read, or the path names an item of a list,
// which is judged by its whole list across every document of the file. It ends like the other two forms with "because they could quote it."
func SecretFileMessage(path string) string {
	if path == "" {
		return SecretValueMessage("")
	}
	return path + ": this value isn't valid. The file uses a secret variable, so the details aren't shown here, because they could quote it."
}
