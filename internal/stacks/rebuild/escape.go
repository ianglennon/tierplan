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
//   - a depends_on that asks for nothing but the start of its services is a plain list, and one that asks for more
//     says only what it asks for (the condition, and restart when it is true);
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
				plainDependsOn(svc)
				memorySizes(svc)
				shortVolumes(svc)
				tidyDurations(svc)
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
	} else if nets != nil && nets.Kind == yaml.MappingNode {
		// A network with no settings beside ones that have some reads as {}, not as null.
		for i := 1; i < len(nets.Content); i += 2 {
			if v := nets.Content[i]; v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
				nets.Content[i] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle}
			}
		}
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

// memorySizes writes deploy.resources.limits.memory, reservations.memory and the size of a tmpfs mount with a unit,
// 512M instead of "536870912". A size that no unit divides evenly stays in bytes.
func memorySizes(svc *yaml.Node) {
	sizeOf := func(n *yaml.Node, key string) {
		if n == nil || n.Kind != yaml.MappingNode {
			return
		}
		if v := mappingValue(n, key); v != nil && v.Kind == yaml.ScalarNode {
			if bytes, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
				v.Value, v.Tag, v.Style = byteSize(bytes), "!!str", 0
			}
		}
	}
	if deploy := mappingValue(svc, "deploy"); deploy != nil && deploy.Kind == yaml.MappingNode {
		if res := mappingValue(deploy, "resources"); res != nil && res.Kind == yaml.MappingNode {
			sizeOf(mappingValue(res, "limits"), "memory")
			sizeOf(mappingValue(res, "reservations"), "memory")
		}
	}
	if vols := mappingValue(svc, "volumes"); vols != nil && vols.Kind == yaml.SequenceNode {
		for _, v := range vols.Content {
			if v.Kind == yaml.MappingNode {
				sizeOf(mappingValue(v, "tmpfs"), "size")
			}
		}
	}
	// The container settings of a standalone service that are a number of bytes.
	for _, key := range []string{"mem_limit", "mem_reservation", "memswap_limit", "shm_size"} {
		sizeOf(svc, key)
	}
}

// plainDependsOn writes depends_on the short way. compose-go writes every dependency as a mapping with
// "required: true", which is the default, so a dependency on the start of a service is its name, and one with a
// condition says only the condition.
func plainDependsOn(svc *yaml.Node) {
	deps := mappingValue(svc, "depends_on")
	if deps == nil || deps.Kind != yaml.MappingNode {
		return
	}
	simple := true
	for i := 0; i+1 < len(deps.Content); i += 2 {
		dep := deps.Content[i+1]
		if dep.Kind != yaml.MappingNode {
			simple = false
			continue
		}
		for j := 0; j+1 < len(dep.Content); j += 2 {
			key, val := dep.Content[j].Value, dep.Content[j+1].Value
			if key == "required" && val == "true" {
				dep.Content = append(dep.Content[:j], dep.Content[j+2:]...)
				j -= 2
				continue
			}
			if key != "condition" || val != "service_started" {
				simple = false
			}
		}
	}
	if !simple {
		return
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for i := 0; i < len(deps.Content); i += 2 {
		seq.Content = append(seq.Content, deps.Content[i])
	}
	replaceValue(svc, "depends_on", seq)
}

// shortVolumes writes a mount as source:target[:ro] when that says everything the long form does: a named volume
// or an absolute bind with no options, and an anonymous volume as its target.
func shortVolumes(svc *yaml.Node) {
	vols := mappingValue(svc, "volumes")
	if vols == nil || vols.Kind != yaml.SequenceNode {
		return
	}
	for i, v := range vols.Content {
		if short, ok := shortVolume(v); ok {
			vols.Content[i] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: short}
		}
	}
}

func shortVolume(v *yaml.Node) (string, bool) {
	if v.Kind != yaml.MappingNode {
		return "", false
	}
	var typ, source, target string
	var readOnly bool
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, val := v.Content[i].Value, v.Content[i+1]
		if val.Kind != yaml.ScalarNode {
			return "", false
		}
		switch k {
		case "type":
			typ = val.Value
		case "source":
			source = val.Value
		case "target":
			target = val.Value
		case "read_only":
			readOnly = val.Value == "true"
		default:
			return "", false
		}
	}
	if target == "" || strings.ContainsAny(target+source, ": ") {
		return "", false
	}
	switch {
	case typ == "volume" && source == "" && !readOnly:
		return target, true
	case typ == "volume" && source != "", typ == "bind" && strings.HasPrefix(source, "/"):
		short := source + ":" + target
		if readOnly {
			short += ":ro"
		}
		return short, true
	}
	return "", false
}

// durationKeys are the keys whose value is a duration.
var durationKeys = map[string]bool{
	"delay": true, "window": true, "monitor": true, "interval": true, "timeout": true,
	"start_period": true, "start_interval": true, "stop_grace_period": true,
}

// tidyDurations writes 2m where Go prints 2m0s and 1h where it prints 1h0m0s.
func tidyDurations(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if v := n.Content[i+1]; durationKeys[n.Content[i].Value] && v.Kind == yaml.ScalarNode {
				v.Value = tidyDuration(v.Value)
			}
		}
	}
	for _, c := range n.Content {
		tidyDurations(c)
	}
}

func tidyDuration(s string) string {
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
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
