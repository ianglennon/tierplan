// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"database/sql"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// wipeOutcome is how one node's cache volume wipe ended.
type wipeOutcome int

const (
	// wipeDone: the volume is gone, whether this call removed it or it was not there.
	wipeDone wipeOutcome = iota
	// wipeBusy: a proxy container is still on the node, so the volume was left (D-27, PROH-24-12).
	wipeBusy
	// wipeUnreachable: the node's agent is not connected, or a read or the removal failed. The volume may still be there.
	wipeUnreachable
)

// pendingKey is the key a node's pending wipe is stored under: the swarm node ID, or the agent ID for a node that has none
// (the server's own engine).
func pendingKey(n NodeInfo) string {
	if n.SwarmNodeID != "" {
		return n.SwarmNodeID
	}
	return n.AgentID
}

// nodeHoldsProxy reports whether the node runs, or still holds, a container of the ingress proxy, or any container that
// mounts the cache volume: either stops the volume's removal. It also reports whether the node has the volume, and
// whether it is TierPlan's. It reads only.
func nodeHoldsProxy(ctx context.Context, c *dockerx.Client) (busy, hasVolume bool, err error) {
	rows, err := c.ContainersWithLabel(ctx, ingress.LabelComponent, ingress.ComponentIngress)
	if err != nil {
		return false, false, err
	}
	vols, err := c.VolumesWithLabel(ctx, ingress.LabelComponent, ingress.ComponentIngress)
	if err != nil {
		return false, false, err
	}
	for _, v := range vols {
		if ingress.OwnsVolume(v.Name, v.Labels) {
			hasVolume = true
		}
	}
	if len(rows) > 0 {
		return true, hasVolume, nil
	}
	if !hasVolume {
		return false, false, nil
	}
	users, err := c.VolumeUsers(ctx, ingress.VolumeName)
	if err != nil {
		return false, false, err
	}
	return len(users) > 0, true, nil
}

// wipeNode removes the certificate cache volume of one node, the one path disable and the reconnect pass share. The
// volume holds every private key of the environment's routes, so it is never left behind silently: a node that cannot be
// reached, or whose removal failed, is reported as wipeUnreachable and the caller queues it. A node that runs an ingress
// container is never wiped (PROH-24-12): with wait set the call waits, on the injected clock and for at most
// DisableTaskDeadline, for the containers to go; without it the node is wipeBusy at once. A volume the node does not
// have, or one that is not TierPlan's, is wipeDone: there is nothing of TierPlan's to wipe. Nothing is retried.
func (s *Service) wipeNode(ctx context.Context, envID string, n NodeInfo, wait bool) (wipeOutcome, error) {
	if !n.Connected || n.AgentID == "" {
		return wipeUnreachable, nil
	}
	clock := s.d.Clock
	deadline := clock.Now().Add(DisableTaskDeadline)
	for {
		var busy, has bool
		if err := s.d.Engine.OnNode(ctx, envID, n.AgentID, func(c *dockerx.Client) (err error) {
			busy, has, err = nodeHoldsProxy(ctx, c)
			return err
		}); err != nil {
			return wipeUnreachable, err
		}
		if !has {
			return wipeDone, nil
		}
		if !busy {
			break
		}
		if !wait || !clock.Now().Before(deadline) {
			return wipeBusy, nil
		}
		select {
		case <-ctx.Done():
			return wipeUnreachable, ctx.Err()
		case <-clock.After(stackops.PollInterval):
		}
	}
	err := s.d.Engine.WriteOnNode(ctx, envID, n.AgentID, func(c *dockerx.Client) error {
		return c.VolumeRemove(ctx, ingress.VolumeName)
	})
	switch {
	case err == nil, dockerx.IsNotFound(err):
		return wipeDone, nil
	case dockerx.IsConflict(err):
		// A container started on the volume between the read and the removal, which the engine refuses.
		return wipeBusy, nil
	}
	return wipeUnreachable, err
}

// queueWipe records that the node's cache volume is still to be wiped, with ingress.wipe_queued in the same transaction,
// once: a wipe that is queued already is left as it is and not audited again.
func (s *Service) queueWipe(ctx context.Context, actor audit.Actor, envID string, n NodeInfo, queued map[string]bool) error {
	key := pendingKey(n)
	if queued[key] {
		return nil
	}
	err := s.d.Store.QueueWipe(ctx, envID, key, ingress.VolumeName, s.d.Clock.Now(), func(ctx context.Context, tx *sql.Tx) error {
		return s.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionIngressWipeQueued, TargetType: "environment", TargetID: envID, EnvID: envID,
			Result: audit.ResultSuccess, Details: map[string]string{"node": n.Name},
		})
	})
	if err != nil {
		return err
	}
	queued[key] = true
	return nil
}

// clearWipe takes a done wipe off the queue and records ingress.wipe_done in the same transaction. A wipe that was not
// queued records nothing.
func (s *Service) clearWipe(ctx context.Context, actor audit.Actor, envID string, n NodeInfo, key string) error {
	_, err := s.d.Store.CompleteWipe(ctx, envID, key, ingress.VolumeName, func(ctx context.Context, tx *sql.Tx) error {
		return s.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionIngressWipeDone, TargetType: "environment", TargetID: envID, EnvID: envID,
			Result: audit.ResultSuccess, Details: map[string]string{"node": n.Name},
		})
	})
	return err
}
