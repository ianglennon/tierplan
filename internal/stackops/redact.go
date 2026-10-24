// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"bytes"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// RedactChanges returns copies of changes for a reader who may not reveal secrets (D-47, AUTH-07). The spec of a
// change holds the values of a running service, which the secret masker of the stack does not know: the values of
// its environment, its command line, the command of its health check and the values of its log options. They are
// the fields redact.MaskServiceSpec withholds, and each is replaced here by redact.Placeholder in Current and in
// Next: an environment entry keeps its NAME, a command or arguments list becomes one Placeholder, a health check
// keeps its mode (CMD, CMD-SHELL, NONE) and a log option keeps its key.
//
// Whether two sides differ was decided before the values were withheld, so a setting that differed only in such a
// value reads alike on both sides now; its path is added to HiddenChanges (sorted, once), the way the review lists a
// setting that differs in a secret of the stack. Settings is not touched. A shape the engine would not send (an
// environment that is not a list or a map) is withheld whole rather than passed through. The input is not modified.
func RedactChanges(changes []stacks.ServiceChange, target stacks.Target) []stacks.ServiceChange {
	out := make([]stacks.ServiceChange, len(changes))
	for i, c := range changes {
		out[i] = c
		out[i].HiddenChanges = slices.Clone(c.HiddenChanges)
		cur, curSlots := redactSpec(c.Current, target)
		next, nextSlots := redactSpec(c.Next, target)
		out[i].Current, out[i].Next = cur, next
		out[i].HiddenChanges = mergePaths(out[i].HiddenChanges, differingSlots(curSlots, nextSlots))
	}
	return out
}

// withheldSlot is one value of a spec that was replaced: the JSON it held and the JSON that replaced it.
type withheldSlot struct{ was, now string }

// redactSpec returns spec with its withheld fields replaced, and each replaced field by its path. A spec that is
// empty, null or not an object is returned as it is: it holds no field to withhold.
func redactSpec(spec json.RawMessage, target stacks.Target) (json.RawMessage, map[string]withheldSlot) {
	if len(spec) == 0 {
		return spec, nil
	}
	dec := json.NewDecoder(bytes.NewReader(spec))
	dec.UseNumber()
	var tree map[string]any
	if err := dec.Decode(&tree); err != nil || tree == nil {
		return spec, nil
	}
	w := &withholder{slots: map[string]withheldSlot{}}
	if target == stacks.TargetStandalone {
		w.standalone(tree)
	} else {
		w.swarm(tree)
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		// Never pass a spec on that could not be re-encoded: the values in it were not withheld.
		return json.RawMessage("null"), w.slots
	}
	return raw, w.slots
}

// withholder replaces the withheld fields of one spec tree in place and remembers what it replaced.
type withholder struct{ slots map[string]withheldSlot }

func (w *withholder) record(path string, was, now any) {
	w.slots[path] = withheldSlot{was: jsonOf(was), now: jsonOf(now)}
}

func jsonOf(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// swarm withholds the fields of a swarm.ServiceSpec tree.
func (w *withholder) swarm(spec map[string]any) {
	tt, ok := spec["TaskTemplate"].(map[string]any)
	if !ok {
		return
	}
	if cs, ok := tt["ContainerSpec"].(map[string]any); ok {
		const base = "TaskTemplate.ContainerSpec"
		w.environment(cs, "Env", base+".Env")
		w.argv(cs, "Command", base+".Command")
		w.argv(cs, "Args", base+".Args")
		if hc, ok := cs["Healthcheck"].(map[string]any); ok {
			w.healthcheck(hc, base+".Healthcheck.Test", "Test")
		}
	}
	if ld, ok := tt["LogDriver"].(map[string]any); ok {
		w.options(ld, "Options", "TaskTemplate.LogDriver.Options")
	}
}

// standalone withholds the fields of a compose service tree (stacks.ServiceJSON).
func (w *withholder) standalone(svc map[string]any) {
	w.environment(svc, "environment", "environment")
	w.argv(svc, "command", "command")
	w.argv(svc, "entrypoint", "entrypoint")
	if hc, ok := svc["healthcheck"].(map[string]any); ok {
		w.healthcheck(hc, "healthcheck.test", "test")
	}
	if lg, ok := svc["logging"].(map[string]any); ok {
		w.options(lg, "options", "logging.options")
	}
}

// environment withholds the values of an environment: a list of "NAME=value" (keyed in a path as the review's diff
// keys it, path[NAME]) or, for a compose service, a map from NAME to its value, where null is a variable that
// takes its value from the host and holds nothing to withhold.
func (w *withholder) environment(parent map[string]any, key, path string) {
	switch env := parent[key].(type) {
	case nil:
	case []any:
		for i, entry := range env {
			s, ok := entry.(string)
			if !ok {
				env[i] = redact.Placeholder
				w.record(path+"["+strconv.Itoa(i)+"]", entry, redact.Placeholder)
				continue
			}
			name, _, hasValue := strings.Cut(s, "=")
			if !hasValue {
				continue
			}
			env[i] = name + "=" + redact.Placeholder
			w.record(path+"["+name+"]", s, env[i])
		}
	case map[string]any:
		for name, value := range env {
			if value == nil {
				continue
			}
			env[name] = redact.Placeholder
			w.record(path+"."+name, value, redact.Placeholder)
		}
	default:
		parent[key] = redact.Placeholder
		w.record(path, env, redact.Placeholder)
	}
}

// argv withholds a command line: a list that holds anything becomes one Placeholder.
func (w *withholder) argv(parent map[string]any, key, path string) {
	switch v := parent[key].(type) {
	case nil:
	case []any:
		if len(v) == 0 {
			return
		}
		parent[key] = []any{redact.Placeholder}
		w.record(path, v, parent[key])
	default:
		parent[key] = redact.Placeholder
		w.record(path, v, redact.Placeholder)
	}
}

// healthcheck withholds the command of a health check; the first element is its mode and stays.
func (w *withholder) healthcheck(hc map[string]any, path, key string) {
	switch v := hc[key].(type) {
	case nil:
	case []any:
		if len(v) < 2 {
			return
		}
		hc[key] = []any{v[0], redact.Placeholder}
		w.record(path, v, hc[key])
	default:
		hc[key] = redact.Placeholder
		w.record(path, v, redact.Placeholder)
	}
}

// options withholds every value of the options of a log driver; the keys stay.
func (w *withholder) options(parent map[string]any, key, path string) {
	switch opts := parent[key].(type) {
	case nil:
	case map[string]any:
		for name, value := range opts {
			opts[name] = redact.Placeholder
			w.record(path+"."+name, value, redact.Placeholder)
		}
	default:
		parent[key] = redact.Placeholder
		w.record(path, opts, redact.Placeholder)
	}
}

// differingSlots are the paths replaced on both sides whose values differed and now read alike.
func differingSlots(cur, next map[string]withheldSlot) []string {
	var paths []string
	for path, c := range cur {
		n, ok := next[path]
		if ok && c.was != n.was && c.now == n.now {
			paths = append(paths, path)
		}
	}
	return paths
}

// mergePaths is the union of two lists of paths, sorted, each once. It is nil when both are empty.
func mergePaths(a, b []string) []string {
	all := append(slices.Clone(a), b...)
	if len(all) == 0 {
		return nil
	}
	sort.Strings(all)
	return slices.Compact(all)
}
