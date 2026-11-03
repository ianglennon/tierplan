// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/ianglennon/tierplan/internal/janitor"
)

// previewGate bounds what the previews of the janitor cost the engines (WR-08). A preview reads every node
// fresh, its clock, its networks and up to janitor.MaxInspectsPerRun container inspects with their sizes, and
// any reader of the environment may ask for one. So the gate runs one preview of an environment at a time, and a
// preview asked for while the same one (same environment, same candidate policy) is running is not run again:
// it gets that one's answer, which is not the caller's own (a preview's body depends on nothing but the
// environment and the policy).
type previewGate struct {
	mu    sync.Mutex
	calls map[string]*previewCall
	envs  map[string]chan struct{}
	// onJoin and onWait, when set, are told that a caller joined a running preview, or found its
	// environment's preview slot taken and waits for it. Tests use them to know where a caller is.
	onJoin, onWait func()
}

// previewCall is one preview being run, and its answer once done is closed.
type previewCall struct {
	done chan struct{}
	body JanitorPreviewBody
	err  error
}

func newPreviewGate() *previewGate {
	return &previewGate{calls: map[string]*previewCall{}, envs: map[string]chan struct{}{}}
}

// previewInputs is what makes two previews of one environment the same: the candidate policy and the candidate
// node settings, written out in a stable form (encoding/json sorts the keys of a map).
func previewInputs(candidate *janitor.Overrides, nodes map[string]janitor.NodeOverrides) string {
	b, err := json.Marshal(struct {
		Policy *janitor.Overrides
		Nodes  map[string]janitor.NodeOverrides
	}{candidate, nodes})
	if err != nil {
		// Overrides are plain values and always encode; a failure only stops this preview being shared.
		return "\x00unshared"
	}
	return string(b)
}

// run answers the preview of envID for inputs: the one already running for them, or fn's once the environment's
// slot is free. A caller whose context ends stops waiting. A preview that ended because the context of the caller
// that ran it ended is not an answer for the others, who run it again.
func (g *previewGate) run(ctx context.Context, envID, inputs string, fn func(context.Context) (JanitorPreviewBody, error)) (JanitorPreviewBody, error) {
	key := envID + "\x00" + inputs
	for {
		g.mu.Lock()
		c, running := g.calls[key]
		if !running {
			break // still locked: this caller runs it
		}
		g.mu.Unlock()
		if g.onJoin != nil {
			g.onJoin()
		}
		select {
		case <-c.done:
		case <-ctx.Done():
			return JanitorPreviewBody{}, ctx.Err()
		}
		if c.err != nil && ctx.Err() == nil && (errors.Is(c.err, context.Canceled) || errors.Is(c.err, context.DeadlineExceeded)) {
			continue
		}
		return c.body, c.err
	}
	c := &previewCall{done: make(chan struct{})}
	g.calls[key] = c
	slot := g.envs[envID]
	if slot == nil {
		slot = make(chan struct{}, 1)
		g.envs[envID] = slot
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		close(c.done)
	}()

	select {
	case slot <- struct{}{}:
	default:
		if g.onWait != nil {
			g.onWait()
		}
		select {
		case slot <- struct{}{}:
		case <-ctx.Done():
			c.err = ctx.Err()
			return JanitorPreviewBody{}, c.err
		}
	}
	defer func() { <-slot }()
	c.body, c.err = fn(ctx)
	return c.body, c.err
}
