// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"slices"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx"
)

// usageReadTimeout bounds the read of a swarm's services and tasks for one ResolveUsage.
const usageReadTimeout = 30 * time.Second

// NetKey names one network for the purpose of who uses it. A swarm-scoped network is one
// network for the whole cluster, so its NodeID is empty; a local network belongs to the
// node it is on, since another node's network can carry the same ID.
type NetKey struct {
	NodeID, NetworkID string
}

// Usage is who uses each network of an environment, read fresh from three sources: every
// reporting node's containers (stopped ones included), every service's spec as the swarm
// holds it (the current one, the previous one a rollback would return to, and the virtual
// IPs of its endpoint) and every task that is still alive (RESEARCH F6, Pitfall 7).
//
// It is built to be deleted on, so what it could not read is as much a part of it as what
// it did: Known says whether the answer for a row is complete, and a row that is not Known
// must never be treated as unused (P2 D-46).
type Usage struct {
	// Users are the descriptions of what uses each network ("container web", "service db",
	// "service db (previous spec)", "service db (virtual IP)", "task of service db on
	// node-2"), unsorted and possibly repeated. Use UsersOf.
	Users map[NetKey][]string
	// Reporting are the IDs of the nodes whose containers were read, as the aggregate names
	// them.
	Reporting map[string]bool
	// Missing names the nodes expected to answer that did not (the aggregate's
	// NotReporting). Never nil.
	Missing []string
	// Swarm is true when the environment is a swarm, so the services are part of the answer.
	Swarm bool
	// ManagerRead is true when a manager answered the read of the services and tasks. It is
	// false for an environment that is not a swarm.
	ManagerRead bool
	// FetchedAt is when the oldest of the reads behind this was taken.
	FetchedAt time.Time
}

// ResolveUsage works out who uses each network of an environment now: one fresh snapshot of
// every node, and for a swarm one read through a manager of the services (and their tasks).
// A manager that cannot answer is not an error: ManagerRead is false and the swarm-scoped
// rows are not Known, so a caller can say it cannot tell. The aggregate it read is returned
// as well, so a caller does not read it twice. An error is the environment itself not being
// readable.
func ResolveUsage(ctx context.Context, agg *Aggregator, clusters *Clusters, envID string) (Usage, Aggregate, error) {
	fresh, _, err := agg.FreshSnapshot(ctx, envID)
	if err != nil {
		return Usage{}, Aggregate{}, err
	}
	u := Usage{
		Users:     map[NetKey][]string{},
		Reporting: make(map[string]bool, len(fresh.Nodes)),
		Missing:   slices.Clone(fresh.NotReporting),
		Swarm:     fresh.Target.Probe.Type == dockerx.TypeSwarmManager || fresh.Target.Probe.Type == dockerx.TypeSwarmWorker,
		FetchedAt: fresh.FetchedAt,
	}
	if u.Missing == nil {
		u.Missing = []string{}
	}
	for _, n := range fresh.Nodes {
		u.Reporting[n.ID] = true
	}
	// A container is recorded against the network of its own node and against the cluster-wide
	// one, so whichever scope the row has finds it: network IDs are unique, so the other key
	// never matches a different network.
	for _, c := range fresh.Containers {
		for _, cn := range c.Networks {
			user := "container " + c.Name
			u.add(NetKey{NodeID: c.NodeID, NetworkID: cn.NetworkID}, user)
			u.add(NetKey{NetworkID: cn.NetworkID}, user)
		}
	}
	if u.Swarm {
		u.readSwarm(ctx, agg, clusters, envID)
	}
	return u, fresh, nil
}

// readSwarm adds the users the services say, through one read of a manager. A failed read
// is logged and leaves ManagerRead false.
func (u *Usage) readSwarm(ctx context.Context, agg *Aggregator, clusters *Clusters, envID string) {
	if clusters == nil {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, usageReadTimeout)
	defer cancel()
	var (
		refs  []dockerx.ServiceRef
		tasks []dockerx.TaskNetworkRef
	)
	// One manager answers both reads: the services and the tasks that are still attached.
	err := clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) (e error) {
		if refs, e = c.ServiceRefs(readCtx); e != nil {
			return e
		}
		tasks, e = c.TaskNetworkRefs(readCtx)
		return e
	})
	if err != nil {
		agg.svc.log.Warn("could not read the services to tell what uses a network", "environment", envID, "error", err)
		return
	}
	u.ManagerRead = true
	for _, ref := range refs {
		for _, id := range ref.Networks {
			u.add(NetKey{NetworkID: id}, "service "+ref.Name)
		}
		for _, id := range ref.PreviousNetworks {
			u.add(NetKey{NetworkID: id}, "service "+ref.Name+" (previous spec)")
		}
		for _, id := range ref.VIPNetworks {
			u.add(NetKey{NetworkID: id}, "service "+ref.Name+" (virtual IP)")
		}
	}
	// The node a task is on is named as the cluster read last called it; an unknown one stays its ID.
	hostOf := map[string]string{}
	if st, ok := clusters.Cached(envID); ok {
		for _, n := range st.Read.Nodes {
			hostOf[n.ID] = n.Hostname
		}
	}
	for _, t := range tasks {
		user := "task of service " + t.ServiceName
		if t.ServiceName == "" {
			user = "task " + t.ID
		}
		if t.NodeID != "" {
			node := t.NodeID
			if h := hostOf[node]; h != "" {
				node = h
			}
			user += " on " + node
		}
		for _, id := range t.Networks {
			u.add(NetKey{NetworkID: id}, user)
		}
	}
}

func (u *Usage) add(k NetKey, user string) { u.Users[k] = append(u.Users[k], user) }

// key is where the users of a row are recorded.
func (u Usage) key(row NetworkRow) NetKey {
	if row.Scope == dockerx.ScopeSwarm {
		return NetKey{NetworkID: row.ID}
	}
	return NetKey{NodeID: row.NodeID, NetworkID: row.ID}
}

// Known reports whether the answer for a row is complete. A swarm-scoped network can be
// attached on any node, so every node expected to answer must have, and on a swarm a
// manager must have answered for the services; a local network can only be attached on its
// own node, which must have answered.
func (u Usage) Known(row NetworkRow) bool {
	if row.Scope == dockerx.ScopeSwarm {
		return len(u.Missing) == 0 && (!u.Swarm || u.ManagerRead)
	}
	return u.Reporting[row.NodeID]
}

// UsersOf is what uses a row, sorted and without repeats. It is only the whole answer when
// Known is true.
func (u Usage) UsersOf(row NetworkRow) []string {
	users := slices.Clone(u.Users[u.key(row)])
	slices.Sort(users)
	return slices.Compact(users)
}

// InUse reports whether anything is known to use a row. False is only "unused" when Known
// is true.
func (u Usage) InUse(row NetworkRow) bool { return len(u.Users[u.key(row)]) > 0 }
