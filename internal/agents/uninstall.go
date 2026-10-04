// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package agents

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/proto/wire"
)

// uninstallKey identifies one uninstall request: the agent it went to and the id the
// agent echoes in its answer. An answer matches only both.
type uninstallKey struct{ agentID, id string }

// uninstallWait is an uninstall that was sent and has not been answered.
type uninstallWait struct {
	answer chan wire.UpdateResult
}

// uninstallFraction is the part of the timeout the first round, to every agent but the
// one that removes the swarm service, may use; the rest is left for the service removal.
const uninstallFraction = 2.0 / 3.0

// UninstallAll tells every online agent of an environment to remove itself from its
// host (D-30) and waits up to timeout for their answers, then returns the IDs of the
// agents that confirmed it. An answer means the agent accepted the removal and started
// it, not that its host is clean: a host that cannot be reached, whose owner turned
// self-update off, or whose agent does not answer is left for the manual cleanup
// command. Each confirmation is audited as agent.uninstalled under the admin who
// asked. It never fails: whatever it could not do, the caller still revokes the agents
// and removes the environment.
//
// A standalone agent is sent its own message. A swarm is two rounds: every agent but
// one is sent the plain message and starts removing its own state volume, and once
// they have answered one connected manager is sent the message with Service set, which
// removes the swarm service and its token secret and so ends the agent on every node.
// Sending it earlier would kill agents before their helpers exist. A swarm with no
// connected manager that allows self-update is sent nothing: helpers on its workers
// would wait for a service nobody removes. Agents whose host turned self-update off
// are not asked, since they would refuse.
func (u *Updater) UninstallAll(ctx context.Context, actor audit.Actor, envID, ip, ua string, timeout time.Duration) []string {
	connected := u.hub.Connected(envID)
	var willing []Agent
	for _, a := range connected {
		if a.SelfUpdate {
			willing = append(willing, a)
		}
	}
	if len(willing) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var confirmed []Agent
	if !isSwarm(connected) {
		confirmed = u.uninstallRound(ctx, willing, false)
	} else {
		via, ok := u.pickManager(envID, willing)
		if !ok {
			u.log.Info("a swarm has no connected manager that may remove its service, so its agents were not told to uninstall", "environment_id", envID)
			return nil
		}
		var others []Agent
		for _, a := range willing {
			if a.ID != via.ID {
				others = append(others, a)
			}
		}
		first, cancelFirst := context.WithTimeout(ctx, time.Duration(float64(timeout)*uninstallFraction))
		confirmed = u.uninstallRound(first, others, false)
		cancelFirst()
		confirmed = append(confirmed, u.uninstallRound(ctx, []Agent{via}, true)...)
	}

	ids := make([]string, 0, len(confirmed))
	for _, a := range confirmed {
		ids = append(ids, a.ID)
		mode := "standalone"
		if isSwarm(connected) {
			mode = "swarm"
		}
		u.record(audit.Entry{
			Actor: actor, Action: audit.ActionAgentUninstalled, TargetType: targetAgent, TargetID: a.ID,
			EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
			Details: map[string]string{"node": a.Node, "version": a.Version, "mode": mode},
		})
	}
	sort.Strings(ids)
	return ids
}

// uninstallRound sends an uninstall to each agent at once and waits until each has
// answered or ctx ends. It returns the agents that answered ok. service sets the
// message's Service flag.
func (u *Updater) uninstallRound(ctx context.Context, agents []Agent, service bool) []Agent {
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		confirmed []Agent
	)
	for _, a := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if u.uninstallOne(ctx, a, service) {
				mu.Lock()
				confirmed = append(confirmed, a)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return confirmed
}

// uninstallOne sends one agent its uninstall and waits for its answer. The wait is
// registered before the message goes out, so an answer cannot arrive unnoticed.
func (u *Updater) uninstallOne(ctx context.Context, a Agent, service bool) bool {
	id := newUpdateID()
	key := uninstallKey{a.ID, id}
	w := &uninstallWait{answer: make(chan wire.UpdateResult, 1)}
	u.mu.Lock()
	u.un[key] = w
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		delete(u.un, key)
		u.mu.Unlock()
	}()

	if err := u.hub.SendUninstall(a.ID, wire.Uninstall{T: wire.TypeUninstall, ID: id, RemoveData: true, Service: service}); err != nil {
		u.log.Warn("could not tell an agent to uninstall", "agent_id", a.ID, "environment_id", a.EnvID, "error", err)
		return false
	}
	select {
	case res := <-w.answer:
		if !res.OK {
			u.log.Warn("an agent did not uninstall", "agent_id", a.ID, "environment_id", a.EnvID, "reason", updateErrorCode(res.Error))
		}
		return res.OK
	case <-ctx.Done():
		u.log.Warn("an agent did not answer the uninstall in time", "agent_id", a.ID, "environment_id", a.EnvID)
		return false
	}
}

// deliverUninstall hands an update_result to the uninstall waiting for it, if one is.
// It reports whether it was one: only an answer from the agent the request went to,
// echoing that request's id, is.
func (u *Updater) deliverUninstall(agentID string, res wire.UpdateResult) bool {
	u.mu.Lock()
	w := u.un[uninstallKey{agentID, res.ID}]
	u.mu.Unlock()
	if w == nil {
		return false
	}
	select {
	case w.answer <- res:
	default: // already answered once
	}
	return true
}
