// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// Traefik's file provider does not read the dynamic file the way dynamicDoc does. It decodes it with a
// parser that matches field names case-insensitively (paerser's findTypedField uses strings.EqualFold), and
// it resolves YAML anchors and merge keys first. A struct decode with exact tags therefore sees a different
// document from the one Traefik runs: "CertFile" or "ROOTCAS" is invisible to it, and so is a key that came
// in through a merge. The two scans below read the whole YAML tree instead, with the same folding, so Check
// holds for what Traefik reads and not only for the spelling the renderer happens to emit.
//
// refscanBudget bounds the nodes the scans visit and refscanDepth the nesting they follow, so an alias
// bomb that the parser let through cannot make Check slow; a document past either is yaml_invalid.
const (
	refscanBudget = 200_000
	refscanDepth  = 100
)

// pathFileKeys are the field names Traefik reads a file from, whatever they sit under.
var pathFileKeys = []string{"certFile", "keyFile", "rootCAs", "caFiles"}

// tlsFileKeys are file fields that are only files directly under a "tls" key (a forwardAuth tls block).
var tlsFileKeys = []string{"ca", "cert", "key"}

// kv is one entry of a mapping, merge keys already folded in.
type kv struct {
	key string
	val *yaml.Node
}

// scan walks a YAML tree. It counts what it visits and gives up (fail) at the budget or the depth.
type scan struct {
	visited int
	fail    bool
}

// resolve follows a document node and aliases to the node they stand for.
func (s *scan) resolve(n *yaml.Node) *yaml.Node {
	for range refscanDepth {
		if n == nil {
			return nil
		}
		switch n.Kind {
		case yaml.DocumentNode:
			if len(n.Content) == 0 {
				return nil
			}
			n = n.Content[0]
		case yaml.AliasNode:
			n = n.Alias
		default:
			return n
		}
	}
	s.fail = true
	return nil
}

// entries lists the key-value pairs of a mapping node as a decoder sees them: a merge key ("<<") adds the
// pairs of the mapping, or each mapping of the sequence, it names.
func (s *scan) entries(m *yaml.Node, depth int) []kv {
	m = s.resolve(m)
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	if depth > refscanDepth {
		s.fail = true
		return nil
	}
	var out []kv
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			continue
		}
		if k.Tag == "!!merge" {
			target := s.resolve(v)
			switch {
			case target == nil:
			case target.Kind == yaml.MappingNode:
				out = append(out, s.entries(target, depth+1)...)
			case target.Kind == yaml.SequenceNode:
				for _, item := range target.Content {
					out = append(out, s.entries(item, depth+1)...)
				}
			}
			continue
		}
		out = append(out, kv{key: k.Value, val: v})
	}
	return out
}

// isName says whether key is name in some spelling (matches) and whether it is spelled exactly so (exact).
func isName(key, name string) (matches, exact bool) {
	return strings.EqualFold(key, name), key == name
}

// misspelled reports whether m holds a key that is one of names in a spelling other than the canonical one.
func (s *scan) misspelled(m *yaml.Node, depth int, names ...string) bool {
	for _, e := range s.entries(m, depth) {
		for _, name := range names {
			if matches, exact := isName(e.key, name); matches && !exact {
				return true
			}
		}
	}
	return false
}

// child is the value of the entry of m named exactly name, or nil.
func (s *scan) child(m *yaml.Node, depth int, name string) []*yaml.Node {
	var out []*yaml.Node
	for _, e := range s.entries(m, depth) {
		if e.key == name {
			out = append(out, e.val)
		}
	}
	return out
}

// keysCanonical reports whether every key Check reads (tls, http and the keys under them it looks at) is
// spelled exactly as Check reads it. Traefik matches field names without regard to case, so a sibling
// spelled "SNIStrict" or "Stores" would be read by Traefik and not by Check.
func keysCanonical(root *yaml.Node) bool {
	var s scan
	top := s.resolve(root)
	if top == nil || top.Kind != yaml.MappingNode {
		return !s.fail
	}
	if s.misspelled(top, 0, "tls", "http") {
		return false
	}
	for _, tls := range s.child(top, 0, "tls") {
		if s.misspelled(tls, 1, "options", "stores", "certificates") {
			return false
		}
		for _, opts := range s.child(tls, 1, "options") {
			for _, opt := range s.entries(opts, 2) {
				if s.misspelled(opt.val, 3, "sniStrict") {
					return false
				}
			}
		}
	}
	for _, httpNode := range s.child(top, 0, "http") {
		if s.misspelled(httpNode, 1, "serversTransports") {
			return false
		}
	}
	return !s.fail
}

// fileRefs lists, sorted and without repeats, every value of a file-naming field anywhere in the tree, in
// any spelling of its name, and every scalar that points under the node root whatever field holds it. A
// file field whose value is not a string is listed as "", which is never a bundle path. ok is false when
// the scan could not finish (budget or depth).
func fileRefs(root *yaml.Node) (refs []string, ok bool) {
	var s scan
	var walk func(n *yaml.Node, parent string, depth int)
	walk = func(n *yaml.Node, parent string, depth int) {
		s.visited++
		if s.visited > refscanBudget || depth > refscanDepth {
			s.fail = true
			return
		}
		n = s.resolve(n)
		if n == nil || s.fail {
			return
		}
		switch n.Kind {
		case yaml.ScalarNode:
			if strings.HasPrefix(n.Value, ingressbundle.NodeRoot+"/") {
				refs = append(refs, n.Value)
			}
		case yaml.SequenceNode:
			for _, item := range n.Content {
				walk(item, parent, depth+1)
			}
		case yaml.MappingNode:
			for _, e := range s.entries(n, depth) {
				if isFileField(e.key, parent) {
					refs = append(refs, valuesOf(&s, e.val)...)
				}
				walk(e.val, e.key, depth+1)
			}
		}
	}
	walk(root, "", 0)
	slices.Sort(refs)
	return slices.Compact(refs), !s.fail
}

// isFileField is whether a key, in any spelling, names a file Traefik reads.
func isFileField(key, parent string) bool {
	for _, name := range pathFileKeys {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	if strings.EqualFold(parent, "tls") {
		for _, name := range tlsFileKeys {
			if strings.EqualFold(key, name) {
				return true
			}
		}
	}
	return false
}

// valuesOf is the strings of a file field's value: one scalar, or the scalars of a sequence. Anything else
// (a mapping, a null) yields the empty string, which is never a bundle path.
func valuesOf(s *scan, n *yaml.Node) []string {
	n = s.resolve(n)
	if n == nil {
		return []string{""}
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		for _, item := range n.Content {
			out = append(out, valuesOf(s, item)...)
		}
		return out
	}
	return []string{""}
}
