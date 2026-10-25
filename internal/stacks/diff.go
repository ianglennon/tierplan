// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/swarm"
)

// The changes a review lists for a service or another resource.
const (
	ChangeAdded   = "added"
	ChangeChanged = "changed"
	ChangeRemoved = "removed"
	// ChangeRecreated and ChangeRestarted are the changes of a standalone service the file did not change:
	// the deploy recreates it because a service it shares a namespace or volumes with is recreated, or
	// stops it and starts it again because a service it depends on (restart: true) is recreated.
	ChangeRecreated = "recreated"
	ChangeRestarted = "restarted"
)

// ServiceChange is one service the review says a deploy would add, change or remove. Current and
// Next are the service's spec before and after as JSON, masked: a value derived from a secret of
// the stack reads Hidden, on both sides, so the browser's diff (which lists nothing for Hidden
// against Hidden) never shows a secret and never shows that one changed except through
// HiddenChanges. Removed services carry neither spec, and added ones no Current.
type ServiceChange struct {
	// Name is the swarm service name (stack_service), Short its name in the compose file.
	Name, Short string
	Change      string
	Current     json.RawMessage
	Next        json.RawMessage
	// HiddenChanges are the paths of the settings that changed to or from a value the masker
	// hides, such as TaskTemplate.ContainerSpec.Env[API_KEY]. The values are never listed.
	HiddenChanges []string
	// Settings counts the settings that differ between Current and Next, hidden ones included.
	Settings int
	// Because names the service that is recreated and carries this one with it, for a recreated or
	// restarted row. Swarm never sets it.
	Because string `json:"because,omitempty"`
	// OwnershipOnly is set by MarkOwnershipOnly (and by an adoption check of a standalone host) on a changed
	// service whose only differences are Bayplan's ownership labels: the first deploy recreates it to put them on
	// and changes nothing else (D-16).
	OwnershipOnly bool `json:"ownershipOnly,omitempty"`
}

// MaskSpec walks every string of a value as it appears in JSON and replaces what m hides with
// Hidden. It returns the masked tree and, for each path that held something m hides, the raw
// string that was there, so a caller can decide equality before it shows anything. Paths are the
// ones the browser's diff uses: the keys of objects joined with ".", an environment entry as
// Env[NAME] and any other list entry as [index].
func MaskSpec(spec any, m Masker) (masked map[string]any, hidden map[string]string) {
	tree, _ := toTree(spec)
	t := &maskPass{m: m, hidden: map[string]string{}, leaves: map[string]string{}}
	out, _ := t.walk("", tree).(map[string]any)
	return out, t.hidden
}

// maskPass is one walk of a tree. leaves holds every string seen by path, hidden the raw strings
// the masker changed, and override a display to use at a path in place of the masker's.
type maskPass struct {
	m        Masker
	hidden   map[string]string
	leaves   map[string]string
	override map[string]string
}

func (t *maskPass) walk(path string, node any) any {
	switch v := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			out[k] = t.walk(joinPath(path, k), child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = t.walk(entryPath(path, i, child), child)
		}
		return out
	case string:
		t.leaves[path] = v
		if forced, ok := t.override[path]; ok {
			return forced
		}
		if shown := t.m.Mask(v); shown != v {
			t.hidden[path] = v
			return shown
		}
		return v
	}
	return node
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

const envPath = "TaskTemplate.ContainerSpec.Env"

// entryPath is the path of the i-th entry of a list: an environment entry is named by its
// variable, any other entry by its place.
func entryPath(path string, i int, entry any) string {
	if path == envPath {
		if s, ok := entry.(string); ok {
			name, _, _ := strings.Cut(s, "=")
			return fmt.Sprintf("%s[%s]", path, name)
		}
	}
	return fmt.Sprintf("%s[%d]", path, i)
}

// toTree turns a value into the generic tree of its JSON form, with numbers kept exact.
func toTree(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// DiffServices compares the services a deploy would send, by swarm name, with the services the
// swarm holds. Both sides go through NormalizeSpec first, and equality is decided on the
// normalised raw values, before anything is masked: a changed secret is a change even though both
// sides will read Hidden. It lists added and changed services in name order, then the removed
// ones, in name order. A service that is equal is not listed.
func DiffServices(desired map[string]swarm.ServiceSpec, live map[string]swarm.ServiceSpec, m Masker) []ServiceChange {
	var out, removed []ServiceChange
	for _, name := range sortedKeys(desired) {
		next := NormalizeSpec(desired[name])
		cur, ok := live[name]
		if !ok {
			masked, _ := MaskSpec(next, m)
			out = append(out, ServiceChange{Name: name, Short: shortName(name, next), Change: ChangeAdded, Next: mustJSON(masked)})
			continue
		}
		cur = NormalizeSpec(cur)
		if sameSpec(cur, next) {
			continue
		}
		out = append(out, changedService(name, cur, next, m))
	}
	for _, name := range sortedKeys(live) {
		if _, ok := desired[name]; ok {
			continue
		}
		removed = append(removed, ServiceChange{Name: name, Short: shortName(name, live[name]), Change: ChangeRemoved})
	}
	return append(out, removed...)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := slices.Collect(maps.Keys(m))
	sort.Strings(keys)
	return keys
}

// DiffComposeServices compares the services a standalone deploy would send with the services the host runs,
// both already through NormalizeComposeService (the desired ones from the prepared project, the running ones
// rebuilt from the containers' inspect documents by RunningService). It is DiffServices for compose
// services: equality is decided on the normalised raw values before anything is masked, so a changed secret
// is a change though both sides read Hidden. It lists added and changed services in name order, then the
// removed ones, in name order. A service that is equal is not listed. A removed service needs no spec: its
// entry in running can be a service with only a name.
func DiffComposeServices(desired, running map[string]types.ServiceConfig, m Masker) []ServiceChange {
	var out, removed []ServiceChange
	for _, name := range sortedKeys(desired) {
		next := desired[name]
		cur, ok := running[name]
		if !ok {
			tree, _ := toTree(next)
			masked, _ := MaskSpec(tree, m)
			out = append(out, ServiceChange{Name: name, Short: name, Change: ChangeAdded, Next: mustJSON(masked)})
			continue
		}
		if SameComposeService(cur, next) {
			continue
		}
		out = append(out, ChangedComposeService(name, cur, next, m))
	}
	for _, name := range sortedKeys(running) {
		if _, ok := desired[name]; !ok {
			removed = append(removed, ServiceChange{Name: name, Short: name, Change: ChangeRemoved})
		}
	}
	return append(out, removed...)
}

// SameComposeService reports whether two normalised services are equal in everything they carry.
func SameComposeService(a, b types.ServiceConfig) bool {
	ja, err1 := ServiceJSON(a)
	jb, err2 := ServiceJSON(b)
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
}

// ChangedComposeService is the change row of a service listed as changed whether or not its normalised
// settings differ (a service whose hash differs from the one on its containers can differ in nothing that is
// listed, such as the content of a secret): Current and Next are the masked settings, Settings counts what
// differs, and HiddenChanges names the settings that changed to or from a value the masker hides.
func ChangedComposeService(name string, cur, next types.ServiceConfig, m Masker) ServiceChange {
	curTree, _ := toTree(cur)
	nextTree, _ := toTree(next)
	return changedTrees(name, name, curTree, nextTree, m)
}

// shortName is the service's name in the compose file: its swarm name without the stack's
// namespace prefix.
func shortName(name string, spec swarm.ServiceSpec) string {
	if ns := spec.Labels[LabelSwarmNamespace]; ns != "" {
		if short, ok := strings.CutPrefix(name, ns+"_"); ok {
			return short
		}
	}
	return name
}

func sameSpec(a, b swarm.ServiceSpec) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
}

// changedService builds the change for a service whose two normalised specs differ. A path whose
// value the masker hides on either side and whose raw values differ is a hidden change: both
// sides then show the masked form of whichever side the masker recognised, so a secret the
// masker no longer knows (the old value of a variable that changed) is not shown either.
func changedService(name string, cur, next swarm.ServiceSpec, m Masker) ServiceChange {
	curTree, _ := toTree(cur)
	nextTree, _ := toTree(next)
	return changedTrees(name, shortName(name, next), curTree, nextTree, m)
}

// changedTrees builds the change for a service whose two specs, as JSON trees, are listed as different.
func changedTrees(name, short string, curTree, nextTree any, m Masker) ServiceChange {
	pc := &maskPass{m: m, hidden: map[string]string{}, leaves: map[string]string{}}
	pc.walk("", curTree)
	pn := &maskPass{m: m, hidden: map[string]string{}, leaves: map[string]string{}}
	pn.walk("", nextTree)

	var hiddenPaths []string
	seen := map[string]bool{}
	for _, side := range []map[string]string{pc.hidden, pn.hidden} {
		for p := range side {
			if seen[p] {
				continue
			}
			seen[p] = true
			if pc.hidden[p] != pn.hidden[p] || pc.leaves[p] != pn.leaves[p] {
				hiddenPaths = append(hiddenPaths, p)
			}
		}
	}
	sort.Strings(hiddenPaths)

	// A side the masker did not recognise at a hidden path shows what the other side shows.
	curForce, nextForce := map[string]string{}, map[string]string{}
	for _, p := range hiddenPaths {
		curShown, nextShown := m.Mask(pc.leaves[p]), m.Mask(pn.leaves[p])
		if _, ok := pc.leaves[p]; ok && pc.hidden[p] == "" && nextShown != pn.leaves[p] {
			curForce[p] = nextShown
		}
		if _, ok := pn.leaves[p]; ok && pn.hidden[p] == "" && curShown != pc.leaves[p] {
			nextForce[p] = curShown
		}
	}

	fc := &maskPass{m: m, hidden: map[string]string{}, leaves: map[string]string{}, override: curForce}
	fn := &maskPass{m: m, hidden: map[string]string{}, leaves: map[string]string{}, override: nextForce}
	return ServiceChange{
		Name: name, Short: short, Change: ChangeChanged,
		Current: mustJSON(fc.walk("", curTree)), Next: mustJSON(fn.walk("", nextTree)),
		HiddenChanges: hiddenPaths, Settings: countChanges("", curTree, nextTree),
	}
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return raw
}

// keyedLists are the lists whose entries are matched by what they name rather than by their place,
// as the browser's diff does, so a reordered list is not a change.
var keyedLists = map[string]func(entry any) (string, bool){
	envPath: func(e any) (string, bool) {
		s, ok := e.(string)
		name, _, _ := strings.Cut(s, "=")
		return name, ok && name != ""
	},
	"TaskTemplate.ContainerSpec.Mounts": func(e any) (string, bool) { return stringField(e, "Target") },
	"EndpointSpec.Ports": func(e any) (string, bool) {
		m, ok := e.(map[string]any)
		if !ok || m["TargetPort"] == nil {
			return "", false
		}
		proto, _ := m["Protocol"].(string)
		if proto == "" {
			proto = "tcp"
		}
		return fmt.Sprintf("%v/%s", m["TargetPort"], proto), true
	},
	"TaskTemplate.ContainerSpec.Secrets": fileName,
	"TaskTemplate.ContainerSpec.Configs": fileName,
	"TaskTemplate.Networks":              func(e any) (string, bool) { return stringField(e, "Target") },
}

func stringField(e any, key string) (string, bool) {
	m, ok := e.(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := m[key].(string)
	return s, ok && s != ""
}

func fileName(e any) (string, bool) {
	m, ok := e.(map[string]any)
	if !ok {
		return "", false
	}
	return stringField(m["File"], "Name")
}

// countChanges counts the leaves that differ between two trees. A section on one side only counts
// each of its leaves, as the browser's diff lists them.
func countChanges(path string, a, b any) int {
	n := 0
	walkChanges(path, a, b, func(string, any, any) { n++ })
	return n
}

// PathChange is one setting that differs between two specs: its path (the keys of objects joined with ".", an
// environment entry as Env[NAME] and any other keyed or numbered list entry as [key] or [index]) and the JSON text
// of the value on each side, empty for a side that has none. The values are the raw ones, unmasked, so a
// PathChange is for the server only: a caller that stores or sends the paths drops From and To.
type PathChange struct{ Path, From, To string }

// ChangedPaths lists the leaves that differ between two specs, sorted by path, by the same rules countChanges
// counts them by: the same tree, the same keyed lists, so a reordered list is not a change.
func ChangedPaths(cur, next any) []PathChange {
	curTree, err1 := toTree(cur)
	nextTree, err2 := toTree(next)
	if err1 != nil || err2 != nil {
		return nil
	}
	var out []PathChange
	walkChanges("", curTree, nextTree, func(path string, a, b any) {
		out = append(out, PathChange{Path: path, From: leafText(a), To: leafText(b)})
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// leafText is the JSON text of a leaf, or empty for a side that has none.
func leafText(v any) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// walkChanges calls emit for every leaf that differs between two trees, with the leaf's path and the value on
// each side (nil for a side that has none).
func walkChanges(path string, a, b any, emit func(path string, a, b any)) {
	switch av := a.(type) {
	case map[string]any:
		bv, bok := b.(map[string]any)
		if !bok && b != nil {
			walkChanges(path, a, nil, emit)
			walkChanges(path, nil, b, emit)
			return
		}
		for _, k := range unionKeys(av, bv) {
			walkChanges(joinPath(path, k), av[k], bv[k], emit)
		}
		return
	case []any:
		bv, bok := b.([]any)
		if !bok && b != nil {
			walkChanges(path, a, nil, emit)
			walkChanges(path, nil, b, emit)
			return
		}
		walkList(path, av, bv, emit)
		return
	case nil:
		switch bv := b.(type) {
		case nil:
			return
		case map[string]any:
			walkChanges(path, map[string]any{}, bv, emit)
			return
		case []any:
			walkList(path, nil, bv, emit)
			return
		}
		emit(path, a, b)
		return
	}
	switch bv := b.(type) {
	case map[string]any, []any:
		walkChanges(path, a, nil, emit)
		walkChanges(path, nil, bv, emit)
		return
	case nil:
		emit(path, a, b)
		return
	}
	if fmt.Sprint(a) != fmt.Sprint(b) {
		emit(path, a, b)
	}
}

func unionKeys(a, b map[string]any) []string {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func walkList(path string, a, b []any, emit func(path string, a, b any)) {
	if key, ok := keyedLists[path]; ok {
		ka, oka := keyEntries(a, key)
		kb, okb := keyEntries(b, key)
		if oka && okb {
			for _, k := range unionKeys(ka, kb) {
				walkChanges(path+"["+k+"]", ka[k], kb[k], emit)
			}
			return
		}
	}
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y any
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		walkChanges(fmt.Sprintf("%s[%d]", path, i), x, y, emit)
	}
}

// keyEntries lists a keyed list's entries by key, or reports false when an entry names no key or
// two share one.
func keyEntries(list []any, key func(any) (string, bool)) (map[string]any, bool) {
	out := make(map[string]any, len(list))
	for _, e := range list {
		k, ok := key(e)
		if !ok {
			return nil, false
		}
		if _, dup := out[k]; dup {
			return nil, false
		}
		out[k] = e
	}
	return out, true
}

// ownershipLabelPaths are the path prefixes of Bayplan's own labels on a swarm service spec: on the service and on
// its containers.
var ownershipLabelPaths = []string{"Labels.bayplan.", "TaskTemplate.ContainerSpec.Labels.bayplan."}

// ownershipLabelPath reports whether a path is a label of Bayplan's own bookkeeping.
func ownershipLabelPath(path string) bool {
	return slices.ContainsFunc(ownershipLabelPaths, func(prefix string) bool { return strings.HasPrefix(path, prefix) })
}

// OwnershipOnlyPaths reports whether a service differs from its twin only in Bayplan's ownership labels: it has
// at least one difference and every difference is a bayplan.* label of the service or of its containers. Any other
// difference keeps the service a gap (T-08-55).
func OwnershipOnlyPaths(paths []PathChange) bool {
	return len(paths) > 0 && !slices.ContainsFunc(paths, func(p PathChange) bool { return !ownershipLabelPath(p.Path) })
}

// MarkOwnershipOnly sets OwnershipOnly on each changed service of changes whose differences from the running
// service, both normalised as DiffServices compares them, are all Bayplan ownership labels. desired and running are
// the maps DiffServices was given; the decision is made on the raw values, before anything is masked.
func MarkOwnershipOnly(changes []ServiceChange, desired, running map[string]swarm.ServiceSpec) {
	for i, c := range changes {
		if c.Change != ChangeChanged {
			continue
		}
		next, nok := desired[c.Name]
		cur, cok := running[c.Name]
		if !nok || !cok {
			continue
		}
		changes[i].OwnershipOnly = OwnershipOnlyPaths(ChangedPaths(NormalizeSpec(cur), NormalizeSpec(next)))
	}
}
