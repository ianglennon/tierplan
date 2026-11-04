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
	// ImageTexts remembers the image a container was created with when its list row shows an image ID.
	// Nil means such a container reads not_checked.
	ImageTexts *ImageTextCache
	Now        func() time.Time
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
	return s.judge(ctx, envID, s.withPlatforms(ctx, envID, ServiceWorkloads(rows)))
}

// judge matches the stored checks to the workloads and judges each, keyed by workload ID.
func (s *Service) judge(ctx context.Context, envID string, ws []Workload) (map[string]Verdict, error) {
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

// ContainerJudgement is what is known about the image of each container of an environment: its verdict
// and the workload the verdict is for, keyed by container ID.
type ContainerJudgement struct {
	Verdicts  map[string]Verdict
	Workloads map[string]Workload
}

// ContainerVerdicts judges the image of every container of agg (not the TierPlan agent's), by the check
// stored for the tag it was created from, under the credential the environment resolves for it. It reads
// the digest a container runs from the repo digests of its image on its own node, never from the image
// ID (D-02, RESEARCH Pitfall 1). A container whose list row shows an image ID is named by a bounded
// inspect, or reads not_checked (RESEARCH Pitfall 7). Nothing is sent to a registry but, as for a
// service, the read of an index a workload still runs.
func (s *Service) ContainerVerdicts(ctx context.Context, envID string, agg envs.Aggregate) (map[string]Verdict, error) {
	j, err := s.ContainerJudgements(ctx, envID, agg)
	return j.Verdicts, err
}

// ContainerJudgements is ContainerVerdicts with the workload each verdict is for, which names the image
// that is followed.
func (s *Service) ContainerJudgements(ctx context.Context, envID string, agg envs.Aggregate) (ContainerJudgement, error) {
	ws := ContainerWorkloads(agg, s.textFunc(ctx, envID, agg))
	verdicts, err := s.judge(ctx, envID, ws)
	if err != nil {
		return ContainerJudgement{}, err
	}
	byID := make(map[string]Workload, len(ws))
	for _, w := range ws {
		w.EnvID = envID
		byID[w.ID] = w
	}
	return ContainerJudgement{Verdicts: verdicts, Workloads: byID}, nil
}

// Count is how many things in the environment have an update available: its swarm services, and its
// containers that are not task containers (a task's update is its service's). It counts verdicts, so a
// rebuild of a platform nobody runs, a skipped version or an ignored image is not counted (IMG-05).
func (s *Service) Count(ctx context.Context, envID string) (int, error) {
	if s.Aggregator == nil {
		return 0, errors.New("updates: no aggregator to read the containers with")
	}
	agg, _, err := s.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return 0, err
	}
	n := 0
	if agg.Counts.Services != nil && s.Clusters != nil {
		st, err := s.Clusters.State(ctx, envID)
		if err != nil && st.FetchedAt.IsZero() {
			return 0, err
		}
		verdicts, err := s.ServiceVerdicts(ctx, envID, st.Read.Services)
		if err != nil {
			return 0, err
		}
		for _, v := range verdicts {
			if v.State == StateUpdateAvailable {
				n++
			}
		}
	}
	j, err := s.ContainerJudgements(ctx, envID, agg)
	if err != nil {
		return 0, err
	}
	for id, v := range j.Verdicts {
		if v.State == StateUpdateAvailable && !j.Workloads[id].Task {
			n++
		}
	}
	return n, nil
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

// CheckNow asks the registries about the images of the environment's services and containers now and
// stores the answers. A container and a service of one image are one request.
func (s *Service) CheckNow(ctx context.Context, envID string) (CheckResult, error) {
	var ws []Workload
	swarm, err := s.Clusters.IsSwarm(ctx, envID)
	if err != nil {
		return CheckResult{}, err
	}
	if swarm {
		st, err := s.Clusters.State(ctx, envID)
		if err != nil {
			return CheckResult{}, err
		}
		ws = ServiceWorkloads(st.Read.Services)
	}
	if s.Aggregator != nil {
		agg, _, err := s.Aggregator.Snapshot(ctx, envID)
		switch {
		case err == nil:
			ws = append(ws, ContainerWorkloads(agg, s.textFunc(ctx, envID, agg))...)
		case !swarm:
			return CheckResult{}, err
		}
	}
	for i := range ws {
		ws[i].EnvID = envID
	}
	return s.Checker.CheckEnv(ctx, envID, ws)
}
