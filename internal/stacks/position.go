// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/tierplan/internal/stacks/lint"
)

// NewPositionIndex reads the positions of every key of the YAML in src. It is what Check builds
// for the compose file, exported so the lint tests can index a file the schema would refuse.
func NewPositionIndex(src []byte) (*PositionIndex, error) { return parseIndex(src) }

// NewPositionIndexOf is NewPositionIndex over the compose files of a working copy, in merge order. A
// position it reports names the compose file it is in (lint.Pos.File) when the copy names its compose
// files; the last file that has a key is the one reported, because a later file wins in the merge.
func NewPositionIndexOf(contents []ComposeContent) (*PositionIndex, error) {
	return parseIndexOf(contents)
}

// PositionIndex implements lint.Index over the stored YAML.
var _ lint.Index = (*PositionIndex)(nil)

// kvPair is one key and value of a mapping.
type kvPair struct{ key, val *yaml.Node }

// deref follows an alias to the node it names.
func deref(n *yaml.Node) *yaml.Node {
	for depth := 0; n != nil && n.Kind == yaml.AliasNode && depth < maxPathDepth; depth++ {
		n = n.Alias
	}
	if n != nil && n.Kind == yaml.AliasNode {
		return nil
	}
	return n
}

// mergedEntries are the keys and values of a mapping, in file order, with a merge key (<<: *anchor)
// replaced by the entries of the mapping or mappings it merges. A key the mapping sets itself
// wins over a merged one, as in YAML.
func mergedEntries(n *yaml.Node, depth int) []kvPair {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode || depth >= maxPathDepth {
		return nil
	}
	var out []kvPair
	seen := map[string]bool{}
	var merged []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Tag == "!!merge" || (k.Value == "<<" && k.Tag == "") {
			merged = append(merged, v)
			continue
		}
		out = append(out, kvPair{k, v})
		seen[k.Value] = true
	}
	add := func(m *yaml.Node) {
		for _, e := range mergedEntries(m, depth+1) {
			if !seen[e.key.Value] {
				seen[e.key.Value] = true
				out = append(out, e)
			}
		}
	}
	for _, m := range merged {
		m = deref(m)
		if m == nil {
			continue
		}
		if m.Kind == yaml.SequenceNode {
			for _, item := range m.Content {
				add(item)
			}
			continue
		}
		add(m)
	}
	return out
}

// exact is the node a dotted path names and the value there: the key node for a mapping kvPair, the
// item itself for a sequence item. Unlike find it names nothing when any part of the path is
// missing. Both are nil when the path is not in the file.
func exact(n *yaml.Node, rest string, depth int) (at, val *yaml.Node) {
	n = deref(n)
	if n == nil || depth >= maxPathDepth {
		return nil, nil
	}
	switch n.Kind {
	case yaml.MappingNode:
		var cands []kvPair
		for _, e := range mergedEntries(n, 0) {
			if k := e.key.Value; rest == k || strings.HasPrefix(rest, k+".") {
				cands = append(cands, e)
			}
		}
		// The longest key first: a key with a dot in it is matched whole.
		sort.SliceStable(cands, func(i, j int) bool { return len(cands[i].key.Value) > len(cands[j].key.Value) })
		for _, e := range cands {
			remainder := strings.TrimPrefix(rest[len(e.key.Value):], ".")
			if remainder == "" {
				return e.key, deref(e.val)
			}
			if a, v := exact(e.val, remainder, depth+1); v != nil {
				return a, v
			}
		}
	case yaml.SequenceNode:
		seg, remainder, _ := strings.Cut(rest, ".")
		i, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(seg, "["), "]"))
		if err != nil || i < 0 || i >= len(n.Content) {
			return nil, nil
		}
		item := n.Content[i]
		if remainder == "" {
			return item, deref(item)
		}
		return exact(item, remainder, depth+1)
	}
	return nil, nil
}

// valuesAt are the values the path names in each document. The empty path is the root.
func (ix *PositionIndex) valuesAt(path string) []*yaml.Node {
	if ix == nil {
		return nil
	}
	var out []*yaml.Node
	for _, root := range ix.docs {
		if path == "" {
			out = append(out, deref(root))
			continue
		}
		if _, v := exact(root, path, 0); v != nil {
			out = append(out, v)
		}
	}
	return out
}

// Lookup is the range of the key (or sequence item) the path names, and false when the file does
// not have it.
func (ix *PositionIndex) Lookup(path string) (lint.Pos, bool) {
	if ix == nil || path == "" {
		return lint.Pos{}, false
	}
	for _, i := range ix.order() {
		if at, v := exact(ix.docs[i], path, 0); v != nil {
			line, col, endLine, endCol := posOf(at)
			pos := lint.Pos{Line: line, Col: col, EndLine: endLine, EndCol: endCol}
			if ix.files != nil {
				pos.File = ix.files[i]
			}
			return pos, true
		}
	}
	return lint.Pos{}, false
}

// Keys are the keys of the mapping the path names, in file order and without repeats, or "0", "1",
// ... for a sequence.
func (ix *PositionIndex) Keys(path string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range ix.valuesAt(path) {
		switch v.Kind {
		case yaml.MappingNode:
			for _, e := range mergedEntries(v, 0) {
				if !seen[e.key.Value] {
					seen[e.key.Value] = true
					out = append(out, e.key.Value)
				}
			}
		case yaml.SequenceNode:
			for i := range v.Content {
				if s := strconv.Itoa(i); !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// Scalar is the text of the scalar the path names.
func (ix *PositionIndex) Scalar(path string) (string, bool) {
	values := ix.valuesAt(path)
	if ix != nil && ix.files != nil {
		// A later compose file wins in the merge.
		slices.Reverse(values)
	}
	for _, v := range values {
		if v.Kind == yaml.ScalarNode {
			return v.Value, true
		}
	}
	return "", false
}
