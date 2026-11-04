// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"strings"
)

// FailedUpdate is an update that failed and still applies: the workload it was for is still there and the tag still points at the
// digest that failed.
type FailedUpdate struct {
	Failure Failure
	// WorkloadID is the ID of the service or container the failure is about, as the last read holds it.
	WorkloadID string
}

// FailedUpdates are the failed updates of one environment that still apply (IMG-07, D-14), for the header bell. A failure applies
// while its workload exists and the stored remote digest of the workload's tag is the digest that failed, so it ends when a newer
// digest appears, when the workload is gone and when a good update clears it.
//
// It reads the failure rows, not the badges. After a rollback the swarm refused, the workload runs the digest that failed and its
// badge reads up to date, but the failure is as true as ever: the row, the run and the audit are what say so. Nothing is read from a
// registry; the cluster and the containers are read only when a failure of that kind exists.
func (s *Service) FailedUpdates(ctx context.Context, envID string) ([]FailedUpdate, error) {
	if s.Failures == nil {
		return nil, nil
	}
	failures, err := s.Failures.ForEnv(ctx, envID)
	if err != nil || len(failures) == 0 {
		return nil, err
	}
	var want = map[string]bool{}
	for _, f := range failures {
		want[f.Kind] = true
	}
	var ws []Workload
	if want["service"] && s.Clusters != nil {
		// A cluster that cannot be read has no services to name, so its failures say nothing for now.
		if st, err := s.Clusters.State(ctx, envID); err == nil || !st.FetchedAt.IsZero() {
			ws = append(ws, ServiceWorkloads(st.Read.Services)...)
		}
	}
	if want["container"] && s.Aggregator != nil {
		agg, _, err := s.Aggregator.Snapshot(ctx, envID)
		if err != nil {
			return nil, err
		}
		ws = append(ws, ContainerWorkloads(agg, s.textFunc(ctx, envID, agg))...)
	}
	var failing []Workload
	for _, w := range ws {
		if _, ok := failures[FailureKey(w.Kind, strings.TrimPrefix(w.Name, "/"))]; ok {
			failing = append(failing, w)
		}
	}
	if len(failing) == 0 {
		return nil, nil
	}
	verdicts, err := s.judgeWith(ctx, envID, failing, false)
	if err != nil {
		return nil, err
	}
	var out []FailedUpdate
	for _, w := range failing {
		f := failures[FailureKey(w.Kind, strings.TrimPrefix(w.Name, "/"))]
		if v := verdicts[w.ID]; v.RemoteDigest != "" && v.RemoteDigest == f.Digest {
			out = append(out, FailedUpdate{Failure: f, WorkloadID: w.ID})
		}
	}
	return out, nil
}
