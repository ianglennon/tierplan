// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sync/singleflight"

	"github.com/ianglennon/tierplan/internal/registry"
)

// runIndexSize is how many indexes a RunIndex keeps.
const runIndexSize = 2048

// ErrIndexGone is the error of an index the registry no longer serves by its digest. The workload that
// runs it cannot be compared platform by platform; it reads update_available with a note.
var ErrIndexGone = errors.New("updates: the registry no longer serves the running index")

// runKey names an index by where it lives and what it is. A digest is a content address, so the entry
// never goes stale and is never invalidated; it is only pushed out when the cache is full.
type runKey struct{ host, repo, digest string }

type runEntry struct {
	key       runKey
	platforms map[string]string
}

// RunIndex remembers the platform manifests of the indexes workloads run: platform (linux/amd64) to
// manifest digest. Judging a multi-platform tag needs them for the index a workload runs, which is not
// the one the tag points at now. The checker fills it with the platforms of an index it is about to
// overwrite, and Platforms reads one from the registry, by digest, only when it is not here (RESEARCH
// Pattern 1, Pitfall 2). It is safe for concurrent use.
type RunIndex struct {
	mu     sync.Mutex
	size   int
	order  *list.List // front is the most recently used
	items  map[runKey]*list.Element
	flight singleflight.Group
}

// NewRunIndex returns an empty RunIndex of the default size.
func NewRunIndex() *RunIndex {
	return &RunIndex{size: runIndexSize, order: list.New(), items: map[runKey]*list.Element{}}
}

func keyOf(ref registry.Ref, digest string) runKey { return runKey{ref.Domain, ref.Path, digest} }

func (r *RunIndex) get(k runKey) (map[string]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.items[k]
	if !ok {
		return nil, false
	}
	r.order.MoveToFront(el)
	return el.Value.(*runEntry).platforms, true //nolint:errcheck,forcetypeassert // only this type is ever stored
}

func (r *RunIndex) put(k runKey, platforms map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.items[k]; ok {
		el.Value.(*runEntry).platforms = platforms //nolint:errcheck,forcetypeassert // only this type is ever stored
		r.order.MoveToFront(el)
		return
	}
	r.items[k] = r.order.PushFront(&runEntry{key: k, platforms: platforms})
	for r.order.Len() > r.size {
		back := r.order.Back()
		delete(r.items, r.order.Remove(back).(*runEntry).key) //nolint:errcheck,forcetypeassert // only this type is ever stored
	}
}

// Put remembers the platforms of the index digest of ref's repository. A nil receiver remembers nothing.
func (r *RunIndex) Put(ref registry.Ref, digest string, platforms map[string]string) {
	if r == nil || digest == "" {
		return
	}
	r.put(keyOf(ref, digest), platforms)
}

// Platforms returns the platform manifests of the index digest of ref's repository: from memory, else
// with one read of the manifest by digest through heads, remembered. The answer is empty for a digest
// that names one image. An index the registry does not have is ErrIndexGone; any other failure is
// returned and remembered for nothing, so a later call asks again.
func (r *RunIndex) Platforms(ctx context.Context, heads Heads, envID string, ref registry.Ref, digest string) (map[string]string, error) {
	k := keyOf(ref, digest)
	if r != nil {
		if p, ok := r.get(k); ok {
			return p, nil
		}
	}
	read := func() (any, error) {
		_, p, err := heads.Index(ctx, envID, ref, digest)
		if err != nil {
			if registry.Classify(err) == registry.CodeRepositoryNotFound {
				return nil, ErrIndexGone
			}
			return nil, fmt.Errorf("read the index %s@%s: %w", ref.Repo(), digest, err)
		}
		if p == nil {
			p = map[string]string{}
		}
		return p, nil
	}
	if r == nil {
		v, err := read()
		if err != nil {
			return nil, err
		}
		return v.(map[string]string), nil //nolint:errcheck,forcetypeassert // read returns this type
	}
	v, err, _ := r.flight.Do(fmt.Sprintf("%q", []string{k.host, k.repo, k.digest}), func() (any, error) {
		if p, ok := r.get(k); ok {
			return p, nil
		}
		v, err := read()
		if err == nil {
			r.put(k, v.(map[string]string)) //nolint:errcheck,forcetypeassert // read returns this type
		}
		return v, err
	})
	if err != nil {
		return nil, err
	}
	return v.(map[string]string), nil //nolint:errcheck,forcetypeassert // read returns this type
}
