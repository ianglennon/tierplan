// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// netFactsCheckDeadline bounds the parse of one stack when its declared networks are read.
const netFactsCheckDeadline = 2 * time.Second

// NetFacts is what TierPlan's own stacks say about the networks of an environment, for the check
// that lists the networks nothing uses (NET-04): which stacks exist, which networks they declare
// external (and so use without owning) and which stack holds an operation. It implements
// envs.StackFacts.
type NetFacts struct {
	Repo  Repo
	Locks OperationLock
	// Check parses a stack; nil means stacks.Check. Tests count its calls.
	Check func(context.Context, stacks.CheckInput) stacks.CheckResult
}

var _ envs.StackFacts = NetFacts{}

// Stacks implements envs.StackFacts: the names and the IDs of the stacks of the environment.
func (n NetFacts) Stacks(ctx context.Context, envID string) (names, ids map[string]bool, err error) {
	all, err := n.Repo.Stacks(ctx, envID)
	if err != nil {
		return nil, nil, err
	}
	names, ids = make(map[string]bool, len(all)), make(map[string]bool, len(all))
	for _, s := range all {
		names[s.Name] = true
		ids[s.ID] = true
	}
	return names, ids, nil
}

// ExternalNetworks implements envs.StackFacts: the networks the stacks of the environment declare
// external, by the name the engine knows them by. Each stack's draft is read (the last good
// revision when it has none) and parsed, and a stack that does not load is named in unknown. An
// unchanged stack is parsed once, whatever the number of checks.
func (n NetFacts) ExternalNetworks(ctx context.Context, envID string) (names map[string]bool, unknown []string, err error) {
	all, err := n.Repo.Stacks(ctx, envID)
	if err != nil {
		return nil, nil, err
	}
	names, unknown = map[string]bool{}, []string{}
	for _, s := range all {
		snap, err := n.Repo.Draft(ctx, s.ID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			unknown = append(unknown, s.Name)
			continue
		}
		external, ok := n.externalsOf(ctx, s, snap)
		if !ok {
			unknown = append(unknown, s.Name)
			continue
		}
		for _, name := range external {
			names[name] = true
		}
	}
	return names, unknown, nil
}

// Busy implements envs.StackFacts: the stack that holds the environment's operation lock.
func (n NetFacts) Busy(envID string) (string, bool) {
	if n.Locks == nil {
		return "", false
	}
	h, ok := n.Locks.Held(envID)
	if !ok {
		return "", false
	}
	return h.StackName, h.StackName != ""
}

// externalsOf is the external networks one snapshot of a stack declares, and false when the
// stack did not load.
func (n NetFacts) externalsOf(ctx context.Context, s Stack, snap Snapshot) ([]string, bool) {
	key := factsKey{stackID: s.ID, snapshotID: snap.ID, version: snap.Version, baseSeq: snap.BaseSeq, draft: snap.IsDraft}
	if got, ok := factsMemo.get(key); ok {
		return got.names, true
	}
	check := n.Check
	if check == nil {
		check = stacks.Check
	}
	res := check(ctx, stacks.CheckInput{Name: s.Name, Target: s.Target, Copy: snap.Copy, Deadline: netFactsCheckDeadline})
	if ctx.Err() != nil {
		// A check cut short by the caller says nothing about the stack, and is not remembered.
		return nil, false
	}
	out := factsResult{loaded: res.Loaded != nil && res.Loaded.Project != nil}
	if out.loaded {
		for k, cfg := range res.Loaded.Project.Networks {
			if !bool(cfg.External) {
				continue
			}
			name := cfg.Name
			if name == "" {
				name = k
			}
			out.names = append(out.names, name)
		}
	}
	// A stack that did not load is not remembered: the check may only have run out of time, and
	// a broken definition is quick to refuse again.
	if out.loaded {
		factsMemo.put(key, out)
	}
	return out.names, out.loaded
}

// factsKey names one version of one stack's definition.
type factsKey struct {
	stackID, snapshotID string
	version, baseSeq    int64
	draft               bool
}

type factsResult struct {
	names  []string
	loaded bool
}

// maxFactsMemo bounds the memo: a stack keeps one entry, and a server that has seen this many
// stacks starts again rather than grow without bound.
const maxFactsMemo = 4096

// factsMemo remembers what each stack's latest definition declares external. Stack IDs are unique,
// so one memo for the process cannot mix environments.
var factsMemo = &memoTable{m: map[string]memoEntry{}}

type memoEntry struct {
	key factsKey
	res factsResult
}

type memoTable struct {
	mu sync.Mutex
	m  map[string]memoEntry
}

func (t *memoTable) get(k factsKey) (factsResult, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[k.stackID]
	if !ok || e.key != k {
		return factsResult{}, false
	}
	return e.res, true
}

func (t *memoTable) put(k factsKey, r factsResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.m) >= maxFactsMemo {
		clear(t.m)
	}
	t.m[k.stackID] = memoEntry{key: k, res: r}
}
