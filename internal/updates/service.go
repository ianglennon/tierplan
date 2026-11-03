// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/registry"
)

// defaultStaleAfter is how old the last successful check may be before an unchanged digest reads
// check_stale while no schedule feeds the bound: the design's bound for "schedule off".
const defaultStaleAfter = 24 * time.Hour

// Service answers what is known about the images of an environment: it matches the stored checks to
// what Docker runs, live.
type Service struct {
	Store    Store
	Resolver *registry.Resolver
	Clusters *envs.Clusters
	// Aggregator reads the nodes and containers of an environment; the per-platform judgement and
	// the container checks use it. Nil means every service is judged by index digest.
	Aggregator Snapshotter
	Checker    *Checker
	// Runs remembers the platforms of the indexes workloads run. Nil means each is read when needed.
	Runs *RunIndex
	Now  func() time.Time
	// StaleAfter is how old a successful check may be before an unchanged digest reads check_stale, per
	// environment. Nil means 24 hours.
	StaleAfter func(envID string) time.Duration
}

// Snapshotter reads what an environment runs, node by node. *envs.Aggregator is one.
type Snapshotter interface {
	Snapshot(ctx context.Context, envID string) (envs.Aggregate, envs.Health, error)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) staleAfter(envID string) time.Duration {
	if s.StaleAfter != nil {
		if d := s.StaleAfter(envID); d > 0 {
			return d
		}
	}
	return defaultStaleAfter
}

// ServiceVerdicts judges every checkable service of rows by the check stored for its tag under the
// credential the environment resolves for it (D-19), keyed by service ID. The agent service and the job
// modes are not in the answer. A multi-platform image is judged platform by platform (D-02): the
// platforms of the index a service runs come from memory, else from one read of that index by digest
// from its registry, at most one for each distinct running digest. Nothing else is sent to a registry.
func (s *Service) ServiceVerdicts(ctx context.Context, envID string, rows []dockerx.ServiceRow) (map[string]Verdict, error) {
	ws := s.withPlatforms(ctx, envID, ServiceWorkloads(rows))
	creds := map[hostRepo]string{}
	keyOf := make([]Key, len(ws))
	var keys []Key
	for i, w := range ws {
		if !w.Checkable() {
			continue
		}
		hr := hostRepo{w.Ref.Domain, w.Ref.Path}
		id, ok := creds[hr]
		if !ok {
			var err error
			if id, err = s.Resolver.CredentialFor(ctx, envID, w.Ref); err != nil {
				return nil, fmt.Errorf("choose the registry credential for %s: %w", w.Ref.Repo(), err)
			}
			creds[hr] = id
		}
		keyOf[i] = Key{Host: hr.host, Repo: hr.repo, Tag: w.Ref.Tag, CredID: id}
		keys = append(keys, keyOf[i])
	}
	stored, err := s.Store.Checks(ctx, keys)
	if err != nil {
		return nil, err
	}
	now, stale := s.now(), s.staleAfter(envID)
	out := make(map[string]Verdict, len(ws))
	runs := map[runKey]runRead{}
	for i, w := range ws {
		w.EnvID = envID
		var chk *Check
		if c, ok := stored[keyOf[i]]; ok && w.Checkable() {
			chk = &c
		}
		in := JudgeInput{Workload: w, Check: chk, Now: now, StaleAfter: stale}
		if chk != nil {
			in.RunPlatforms, in.RunIndexGone = s.runPlatforms(ctx, envID, w, chk, runs)
		}
		out[w.ID] = Judge(in)
	}
	return out, nil
}

// withPlatforms fills the platforms of the workloads from the nodes their tasks run on. An environment
// whose nodes cannot be read leaves them empty.
func (s *Service) withPlatforms(ctx context.Context, envID string, ws []Workload) []Workload {
	if s.Aggregator == nil {
		return ws
	}
	agg, _, err := s.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return ws
	}
	return WithPlatforms(ws, agg)
}

// runRead is what one read of a running index answered, kept for the rest of the call.
type runRead struct {
	platforms map[string]string
	gone      bool
}

// runPlatforms reads the platforms of the index w runs, only when they decide the verdict: the stored
// check is an index with platforms, the workload's platforms are known, and the tag points at another
// digest than the one it runs. A read that fails leaves the platforms nil, which Judge reads as a
// change. Reads are remembered in runs, so a digest many services run is asked about once.
func (s *Service) runPlatforms(ctx context.Context, envID string, w Workload, chk *Check, runs map[runKey]runRead) (map[string]string, bool) {
	if chk.SuccessAt.IsZero() || chk.Error != "" && chk.CheckedAt.After(chk.SuccessAt) {
		return nil, false // Judge answers from the failure, or has no answer, before it compares digests
	}
	if !chk.Index || len(chk.Platforms) == 0 || len(w.Platforms) == 0 || w.RunningDigest == "" || chk.Digest == w.RunningDigest || s.Checker == nil {
		return nil, false
	}
	k := keyOf(w.Ref, w.RunningDigest)
	r, ok := runs[k]
	if !ok {
		p, err := s.Runs.Platforms(ctx, s.Checker.Heads, envID, w.Ref, w.RunningDigest)
		r = runRead{platforms: p, gone: errors.Is(err, ErrIndexGone)}
		runs[k] = r
	}
	return r.platforms, r.gone
}

// CheckNow asks the registries about the images of the environment's services now and stores the
// answers.
func (s *Service) CheckNow(ctx context.Context, envID string) (CheckResult, error) {
	st, err := s.Clusters.State(ctx, envID)
	if err != nil {
		return CheckResult{}, err
	}
	ws := ServiceWorkloads(st.Read.Services)
	for i := range ws {
		ws[i].EnvID = envID
	}
	return s.Checker.CheckEnv(ctx, envID, ws)
}
