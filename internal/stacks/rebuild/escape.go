// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rebuild

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// Marshal writes a project as the compose file a person would have written, and the file is deployed later, so
// what it says is what runs:
//
//   - every dollar sign of a value is doubled, because compose-go reads a bare $ as the start of a variable on
//     every load (RESEARCH Pitfall 2): pa$word would come back as pa. A key is not interpolated by compose-go
//     (a label key com.example.$$k loads as written, with both signs), so a key is left as it is;
//   - the ports are written in the short form where it says everything the long one does;
//   - the top-level name is dropped, because the stack's name is Bayplan's, not the file's;
//   - a list of networks that carry nothing is a plain list, a secret or a config reference that carries only its
//     name is that name, a memory size is written with its unit, and the keys of a service come in the order
//     people write them (image first, deploy last) instead of compose-go's field order.
func Marshal(p *types.Project) ([]byte, error) {
	raw, err := p.MarshalYAML()
	if err != nil {
		return nil, fmt.Errorf("rebuild: marshal: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("rebuild: read back: %w", err)
	}
	if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		root := doc.Content[0]
		dropKey(root, "name")
		if services := mappingValue(root, "services"); services != nil && services.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(services.Content); i += 2 {
				svc := services.Content[i+1]
				stacks.ShortenPorts(svc)
				plainLists(svc)
				memorySizes(svc)
				orderKeys(svc)
			}
		}
	}
	escapeDollars(&doc)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("rebuild: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("rebuild: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// escapeDollars doubles every dollar sign of every value of the tree. The keys of a mapping are not read as
// variables when a file is loaded, so they are not touched.
func escapeDollars(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "$") {
		n.Value = strings.ReplaceAll(n.Value, "$", "$$")
	}
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			continue
		}
		escapeDollars(c)
	}
}

// mappingValue is the value node of key in a mapping node, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// dropKey removes key and its value from a mapping node.
func dropKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// plainLists writes the lists of a service the short way: networks that carry no settings are a list of names,
// and a secret or a config reference that has only a source is that name.
func plainLists(svc *yaml.Node) {
	if svc.Kind != yaml.MappingNode {
		return
	}
	if nets := mappingValue(svc, "networks"); nets != nil && nets.Kind == yaml.MappingNode && allNull(nets) {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for i := 0; i < len(nets.Content); i += 2 {
			seq.Content = append(seq.Content, nets.Content[i])
		}
		replaceValue(svc, "networks", seq)
	}
	for _, key := range []string{"secrets", "configs"} {
		refs := mappingValue(svc, key)
		if refs == nil || refs.Kind != yaml.SequenceNode {
			continue
		}
		for i, ref := range refs.Content {
			if ref.Kind == yaml.MappingNode && len(ref.Content) == 2 && ref.Content[0].Value == "source" {
				refs.Content[i] = ref.Content[1]
			}
		}
	}
}

// allNull reports whether every value of a mapping node is null.
func allNull(m *yaml.Node) bool {
	for i := 1; i < len(m.Content); i += 2 {
		if v := m.Content[i]; v.Kind != yaml.ScalarNode || v.Tag != "!!null" {
			return false
		}
	}
	return len(m.Content) > 0
}

// replaceValue sets the value node of key in a mapping node.
func replaceValue(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
}

// memorySizes writes deploy.resources.limits.memory and reservations.memory with a unit, 512M instead of
// "536870912". A size that no unit divides evenly stays in bytes.
func memorySizes(svc *yaml.Node) {
	deploy := mappingValue(svc, "deploy")
	if deploy == nil || deploy.Kind != yaml.MappingNode {
		return
	}
	res := mappingValue(deploy, "resources")
	if res == nil || res.Kind != yaml.MappingNode {
		return
	}
	for _, side := range []string{"limits", "reservations"} {
		part := mappingValue(res, side)
		if part == nil || part.Kind != yaml.MappingNode {
			continue
		}
		if mem := mappingValue(part, "memory"); mem != nil && mem.Kind == yaml.ScalarNode {
			if n, err := strconv.ParseInt(mem.Value, 10, 64); err == nil {
				mem.Value, mem.Tag, mem.Style = byteSize(n), "!!str", 0
			}
		}
	}
}

// byteSize is n bytes with the largest unit that divides it evenly, in the spelling compose reads back to the
// same number of bytes (binary multiples).
func byteSize(n int64) string {
	switch {
	case n > 0 && n%(1<<30) == 0:
		return strconv.FormatInt(n>>30, 10) + "G"
	case n > 0 && n%(1<<20) == 0:
		return strconv.FormatInt(n>>20, 10) + "M"
	case n > 0 && n%(1<<10) == 0:
		return strconv.FormatInt(n>>10, 10) + "K"
	}
	return strconv.FormatInt(n, 10)
}

// serviceKeyOrder is the order a person writes the keys of a service in: what it runs, how it is configured, what
// it connects to, how it is checked and how it is deployed. A key not listed keeps its place after these, in the
// order it came.
var serviceKeyOrder = []string{
	"image", "entrypoint", "command", "environment", "labels", "ports", "volumes", "networks", "secrets", "configs",
	"hostname", "extra_hosts", "dns", "dns_search", "dns_opt", "working_dir", "user", "group_add",
	"healthcheck", "logging", "deploy",
}

// orderKeys puts the keys of a service mapping in serviceKeyOrder.
func orderKeys(svc *yaml.Node) {
	if svc.Kind != yaml.MappingNode {
		return
	}
	type pair struct{ k, v *yaml.Node }
	pairs := make([]pair, 0, len(svc.Content)/2)
	for i := 0; i+1 < len(svc.Content); i += 2 {
		pairs = append(pairs, pair{svc.Content[i], svc.Content[i+1]})
	}
	rank := func(key string) int {
		for i, k := range serviceKeyOrder {
			if k == key {
				return i
			}
		}
		return len(serviceKeyOrder)
	}
	slices.SortStableFunc(pairs, func(a, b pair) int { return cmp.Compare(rank(a.k.Value), rank(b.k.Value)) })
	svc.Content = svc.Content[:0]
	for _, p := range pairs {
		svc.Content = append(svc.Content, p.k, p.v)
	}
}
