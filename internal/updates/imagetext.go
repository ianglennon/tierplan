// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
)

// MaxImageInspects is how many containers one read inspects for the name of their image. A host with
// hundreds of containers whose tags moved is covered over a few reads, not by a storm of inspects
// (RESEARCH Pitfall 7, T-12-13); the P2 policy cache is bounded the same way.
const MaxImageInspects = 50

// retryAfter is how long a container whose inspect failed is left alone before it is asked again, so a
// container that cannot be inspected does not take a place in every read.
const retryAfter = time.Minute

// ImageTextCache remembers, by container ID, the image a container was created with (Config.Image), for
// the containers whose list row shows an image ID instead (RESEARCH Pitfall 7). A container is inspected
// once, so the name is read only for a container not seen before.
type ImageTextCache struct {
	// Inspect reads the image text of containers of an environment by ID. It answers only the ones it could
	// read; an error means none of the answer is trusted.
	Inspect func(ctx context.Context, envID string, ids []string) (map[string]string, error)
	// Max is how many unknown containers one read asks about; zero means MaxImageInspects.
	Max int
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	texts map[string]map[string]imageTextEntry // environment -> container ID -> entry
}

type imageTextEntry struct {
	text string
	// retryAt is, for a container that could not be inspected, when it may be asked about again; zero for
	// one that was.
	retryAt time.Time
}

func (c *ImageTextCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *ImageTextCache) max() int {
	if c.Max > 0 {
		return c.Max
	}
	return MaxImageInspects
}

// Texts returns the image text of the containers of ids that is known, asking Inspect for at most Max of
// those not known yet, in the order of ids. A container that is not in ids any more is forgotten. A
// container that could not be read is not asked about again for a minute. It never fails: what could not
// be read is left out, and its workload reads not_checked.
func (c *ImageTextCache) Texts(ctx context.Context, envID string, ids []string) map[string]string {
	now := c.now()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make(map[string]string, len(ids))
	var todo []string

	c.mu.Lock()
	if c.texts == nil {
		c.texts = map[string]map[string]imageTextEntry{}
	}
	env := c.texts[envID]
	if env == nil {
		env = map[string]imageTextEntry{}
		c.texts[envID] = env
	}
	for id := range env {
		if !want[id] {
			delete(env, id)
		}
	}
	for _, id := range ids {
		e, known := env[id]
		switch {
		case known && e.retryAt.IsZero():
			out[id] = e.text
		case known && now.Before(e.retryAt):
		case len(todo) < c.max():
			todo = append(todo, id)
		}
	}
	c.mu.Unlock()

	if len(todo) == 0 || c.Inspect == nil {
		return out
	}
	got, err := c.Inspect(ctx, envID, todo)
	if err != nil {
		got = nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	env = c.texts[envID]
	if env == nil {
		return out
	}
	for _, id := range todo {
		if t, ok := got[id]; ok && t != "" {
			env[id] = imageTextEntry{text: t}
			out[id] = t
		} else if ctx.Err() == nil {
			env[id] = imageTextEntry{retryAt: now.Add(retryAfter)}
		}
	}
	return out
}

// NodeReader is what InspectImageTexts reads: the aggregate of an environment, to find the node of a
// container, and that node's engine. *envs.Aggregator is one.
type NodeReader interface {
	Snapshotter
	OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error
}

// inspectTimeout bounds the inspects of one read.
const inspectTimeout = 10 * time.Second

// InspectImageTexts is the Inspect of an ImageTextCache over the nodes of an environment: it finds the
// node each container is on from the aggregate and reads the image the container was created with from
// that node's engine. A container that is not found, or whose node does not answer, is left out of the
// answer.
func InspectImageTexts(src NodeReader) func(ctx context.Context, envID string, ids []string) (map[string]string, error) {
	return func(ctx context.Context, envID string, ids []string) (map[string]string, error) {
		agg, _, err := src.Snapshot(ctx, envID)
		if err != nil {
			return nil, err
		}
		nodeOf := make(map[string]string, len(agg.Containers))
		for _, c := range agg.Containers {
			nodeOf[c.ID] = c.NodeID
		}
		ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
		defer cancel()
		out := make(map[string]string, len(ids))
		for _, id := range ids {
			node, ok := nodeOf[id]
			if !ok {
				continue
			}
			err := src.OnNode(ctx, envID, node, func(c *dockerx.Client) error {
				o, err := c.ContainerOwnership(ctx, id)
				if err == nil {
					out[id] = o.Image
				}
				return err
			})
			if err != nil && ctx.Err() != nil {
				break
			}
		}
		return out, nil
	}
}

// textFunc is the lookup ContainerWorkloads takes, over the texts of the containers that need one.
func (s *Service) textFunc(ctx context.Context, envID string, agg envs.Aggregate) func(string) (string, bool) {
	if s.ImageTexts == nil {
		return nil
	}
	got := s.ImageTexts.Texts(ctx, envID, ImageTextWanted(agg))
	return func(id string) (string, bool) {
		t, ok := got[id]
		return t, ok
	}
}
