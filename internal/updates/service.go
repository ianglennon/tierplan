// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
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
	// Aggregator reads the platforms and containers of an environment; the per-platform judgement and
	// the container checks use it.
	Aggregator *envs.Aggregator
	Checker    *Checker
	Now        func() time.Time
	// StaleAfter is how old a successful check may be before an unchanged digest reads check_stale, per
	// environment. Nil means 24 hours.
	StaleAfter func(envID string) time.Duration
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
// modes are not in the answer. Nothing is sent to a registry.
func (s *Service) ServiceVerdicts(ctx context.Context, envID string, rows []dockerx.ServiceRow) (map[string]Verdict, error) {
	ws := ServiceWorkloads(rows)
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
	for i, w := range ws {
		w.EnvID = envID
		var chk *Check
		if c, ok := stored[keyOf[i]]; ok && w.Checkable() {
			chk = &c
		}
		out[w.ID] = Judge(JudgeInput{Workload: w, Check: chk, Now: now, StaleAfter: stale})
	}
	return out, nil
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
