// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"database/sql"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// WipeTick is how often RunWipes takes a pass on its own, on the injected clock. An agent that connects takes one sooner.
const WipeTick = time.Minute

// wipeHolder says who holds the operation lock while a pass wipes the volumes of one environment.
const wipeHolder = "system:ingress-wipe"

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

// RunWipes runs the pending wipes until ctx ends: a pass now, one on every WipeTick of the injected clock and one whenever
// an agent connects (the Deps.Events stream), because a node that was away at the switch is wiped as soon as it is back. A
// pass never writes while the environment is paused or its operation lock is held, never wipes a node that runs an ingress
// container, and drops, without wiping, the entries of an environment whose ingress is on again.
func (s *Service) RunWipes(ctx context.Context) {
	var events <-chan envs.HealthEvent
	if s.d.Events != nil {
		ch, cancel := s.d.Events(ctx)
		defer cancel()
		events = ch
	}
	for {
		s.wipeRound(ctx)
		timer := s.d.Clock.After(WipeTick)
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer:
				break wait
			case ev, ok := <-events:
				switch {
				case !ok:
					// The broker dropped a subscriber that fell behind; the tick still takes the passes.
					s.log().Warn("the ingress wipe loop lost the agent event stream; it runs on its tick")
					events = nil
				case ev.Type == envs.EventAgentConnected:
					s.wipeRound(ctx)
				}
			}
		}
	}
}

// wipeRound takes one pass over every environment that has a pending wipe.
func (s *Service) wipeRound(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	all, err := s.d.Store.AllPendingWipes(ctx)
	if err != nil {
		s.log().Warn("the ingress wipe loop could not list the pending wipes", "error", err)
		return
	}
	var order []string
	for _, w := range all {
		if len(order) == 0 || order[len(order)-1] != w.EnvID {
			order = append(order, w.EnvID)
		}
	}
	for _, envID := range order {
		if ctx.Err() != nil {
			return
		}
		s.wipeEnv(ctx, envID)
	}
	if s.wipeHook != nil {
		s.wipeHook()
	}
}

// wipeEnv takes one environment's pending wipes.
func (s *Service) wipeEnv(ctx context.Context, envID string) {
	state, err := s.d.Store.DeployState(ctx, envID)
	if err != nil {
		s.log().Warn("the ingress wipe loop could not read an environment's state", "environment", envID, "error", err)
		return
	}
	if state.Paused {
		return
	}
	release, held, ok := s.d.Locks.TryAcquire(envID, stackops.Holder{
		Kind: stackops.HolderKindIngress, By: wipeHolder, Workload: "ingress", Started: s.d.Clock.Now(),
	})
	if !ok {
		s.log().Debug("the ingress wipe loop left an environment for the next pass: it is busy", "environment", envID, "holder", held.Kind)
		return
	}
	defer release()
	// The state is read again under the lock, which an enable and a disable also hold while they switch.
	if state, err = s.d.Store.DeployState(ctx, envID); err != nil || state.Paused {
		return
	}
	pending, err := s.d.Store.PendingWipes(ctx, envID)
	if err != nil {
		s.log().Warn("the ingress wipe loop could not list an environment's pending wipes", "environment", envID, "error", err)
		return
	}
	if state.Enabled {
		// Ingress is on again, so the proxy owns the volume: nothing is wiped and the entries are of no use.
		for _, w := range pending {
			if _, err := s.d.Store.ClearPendingWipe(ctx, envID, w.SwarmNodeID, w.Volume); err != nil {
				s.log().Warn("the ingress wipe loop could not drop a pending wipe", "environment", envID, "error", err)
			}
		}
		return
	}
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		s.log().Warn("the ingress wipe loop could not read an environment", "environment", envID, "error", err)
		return
	}
	for _, w := range pending {
		node, found := nodeByKey(info.Nodes, w.SwarmNodeID)
		if !found || !node.Connected {
			continue
		}
		out, err := s.wipeNode(ctx, envID, node, false)
		if err != nil {
			s.log().Warn("the ingress wipe loop could not wipe a node's cache volume", "environment", envID, "node", node.Name, "error", err)
			continue
		}
		if out != wipeDone {
			continue
		}
		if err := s.clearWipe(ctx, audit.SystemIngress, envID, node, w.SwarmNodeID); err != nil {
			s.log().Warn("the ingress wipe loop could not record a wipe", "environment", envID, "node", node.Name, "error", err)
		}
	}
}

// nodeByKey finds the node a pending wipe was queued under.
func nodeByKey(nodes []NodeInfo, key string) (NodeInfo, bool) {
	for _, n := range nodes {
		if pendingKey(n) == key {
			return n, true
		}
	}
	return NodeInfo{}, false
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
