// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package agents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ianglennon/coaming/internal/store/sqlc"
	"github.com/ianglennon/coaming/internal/tunnel"
	"github.com/ianglennon/coaming/proto/wire"
)

// The identity rules of an agent environment (ENV-02, D-11, D-12, D-14, D-23).
//
// The admin never says whether an environment is a swarm or a single host: the
// first agent that enrols says so, from facts its own Docker engine reported, and
// the environment then locks to it. A swarm is bound to its cluster ID by its
// first manager, and a standalone environment to its host's engine ID. A host that
// already belongs to another environment, by engine ID, swarm cluster ID or swarm
// node ID, is refused naming that environment. A manager of a different cluster is
// refused cluster_changed and the new cluster waits for an admin to adopt it;
// nothing is ever rebound silently.

const (
	// placeholderName is the name of an environment created without one. It is
	// replaced by the first host that can name it.
	placeholderName = "New environment"
	// swarmSuffix follows a manager's hostname in the name it gives its environment.
	swarmSuffix = " swarm"
	// maxManagerIDs is how many manager node IDs a handshake may carry.
	maxManagerIDs = 64
)

// Decision is what an accepted identity changes in the environment it enrols into.
// The zero value changes nothing.
type Decision struct {
	// EnvType, EngineID and ClusterID are bound to the environment when it has none
	// yet. Each is empty when there is nothing to bind.
	EnvType, EngineID, ClusterID string
	// Name is the automatic name for an environment still carrying the placeholder,
	// empty when the host cannot name it or it is not waiting for a name.
	Name string
	// PendingCluster is, with a cluster_changed refusal, the cluster the manager
	// reported. RecordChange says whether it is to be recorded: only the first
	// change is, so the record is stable until an admin adopts it.
	PendingCluster string
	RecordChange   bool
}

// identityRefusal makes a refusal of the identity checks. The text goes to the
// agent and the reason stays in the audit log.
func identityRefusal(code, reason, format string, args ...any) error {
	return &tunnel.Refusal{Code: code, Detail: fmt.Sprintf(format, args...), AuditReason: reason}
}

// CheckIdentity decides whether a host that proved it holds a valid join token may
// enrol into an environment, from the facts its engine reported, and says what the
// enrolment binds. It reads and never writes: the caller applies the Decision in
// the transaction that inserts the agent, which is what makes two hosts racing to
// bind the same identity one winner and one refusal. A refusal is a
// *tunnel.Refusal; any other error is a fault.
//
// The order is: facts that are malformed or missing, a host or swarm that belongs
// to another environment, the type, the host (standalone) or the cluster (swarm).
func CheckIdentity(ctx context.Context, q *sqlc.Queries, envID string, f wire.Facts) (Decision, error) {
	if err := checkFacts(f); err != nil {
		return Decision{}, err
	}
	switch {
	case !f.EngineReachable:
		return Decision{}, identityRefusal(wire.ReasonEngineUnreachable, "engine_unreachable",
			"The agent could not read this host's Docker engine, so Coaming cannot tell what the host is. Check that the Docker socket is mounted into the agent and that Docker is running.")
	case f.EngineID == "":
		return Decision{}, identityRefusal(wire.ReasonAuthRejected, "no_engine_id",
			"The agent did not report an engine ID for this host.")
	}
	env, err := q.GetEnvironment(ctx, envID)
	if err != nil {
		return Decision{}, fmt.Errorf("load the environment: %w", err)
	}

	if err := checkDuplicates(ctx, q, envID, f); err != nil {
		return Decision{}, err
	}

	want := EnvTypeStandalone
	if f.Swarm != nil {
		want = EnvTypeSwarm
	}
	if env.EnvType.Valid && env.EnvType.String != want {
		if env.EnvType.String == EnvTypeSwarm {
			return Decision{}, identityRefusal(wire.RefuseTypeMismatch, "type_mismatch",
				"%q manages a swarm, and this host is not part of one.", envLabel(env))
		}
		return Decision{}, identityRefusal(wire.RefuseTypeMismatch, "type_mismatch",
			"%q manages a single host, and this host is part of a swarm.", envLabel(env))
	}

	var dec Decision
	if !env.EnvType.Valid {
		dec.EnvType = want
	}
	if want == EnvTypeStandalone {
		switch {
		case env.EngineID.Valid && env.EngineID.String != f.EngineID:
			return Decision{}, identityRefusal(wire.RefuseHostMismatch, "host_mismatch",
				"%q already manages a different host.", envLabel(env))
		case !env.EngineID.Valid:
			dec.EngineID = f.EngineID
		}
	} else if f.Swarm.Control && f.Swarm.ClusterID != "" {
		changed, err := clusterDecision(env, f)
		if err != nil {
			return changed, err
		}
		if !env.SwarmClusterID.Valid {
			dec.ClusterID = f.Swarm.ClusterID
		}
	}
	dec.Name = autoName(env, f)
	return dec, nil
}

// CheckCluster is what a reconnecting agent is held to: only that a manager still
// belongs to the cluster the environment is bound to. A host's type and engine are
// checked when it enrols, not on every connection, so a host that was reinstalled
// or whose Docker was reset is not locked out of an environment it already holds a
// certificate for. A manager of another cluster is refused cluster_changed with the
// new cluster in the Decision. An engine that did not answer, a worker and an
// environment with no cluster yet are all accepted.
func CheckCluster(ctx context.Context, q *sqlc.Queries, envID string, f wire.Facts) (Decision, error) {
	if err := checkFacts(f); err != nil {
		return Decision{}, err
	}
	if !f.EngineReachable || f.Swarm == nil || !f.Swarm.Control || f.Swarm.ClusterID == "" {
		return Decision{}, nil
	}
	env, err := q.GetEnvironment(ctx, envID)
	if err != nil {
		return Decision{}, fmt.Errorf("load the environment: %w", err)
	}
	return clusterDecision(env, f)
}

// clusterDecision refuses a manager whose cluster is not the bound one. The
// Decision it returns with a refusal says which cluster to record.
func clusterDecision(env sqlc.Environment, f wire.Facts) (Decision, error) {
	if !env.SwarmClusterID.Valid || env.SwarmClusterID.String == f.Swarm.ClusterID {
		return Decision{}, nil
	}
	return Decision{PendingCluster: f.Swarm.ClusterID, RecordChange: !env.ClusterChangedAt.Valid},
		identityRefusal(wire.ReasonClusterChanged, "cluster_changed",
			"This swarm is not the one %q was set up with. An administrator must adopt the new cluster in Coaming before its managers can connect.", envLabel(env))
}

// checkFacts refuses identity strings that are too long or outside the alphabet,
// so none reaches SQL or the audit log (T-02-66). Empty ones are allowed here.
func checkFacts(f wire.Facts) error {
	bad := func(s string) bool { return s != "" && !wire.ValidIdentity(s) }
	invalid := bad(f.EngineID)
	if sw := f.Swarm; sw != nil {
		invalid = invalid || bad(sw.NodeID) || bad(sw.ClusterID) || len(sw.ManagerIDs) > maxManagerIDs
		for _, id := range sw.ManagerIDs {
			invalid = invalid || !wire.ValidIdentity(id)
		}
	}
	if invalid {
		return identityRefusal(wire.ReasonAuthRejected, "bad_identity", "The agent reported an engine, node or cluster ID that is not valid.")
	}
	return nil
}

// checkDuplicates refuses a host that another agent environment already owns: by
// engine ID (a cloned VM has the same one, RESEARCH Pitfall 13), by swarm node ID,
// and for a manager by cluster ID. The local socket environment is never one of
// them (D-12, D-48); an agent that was revoked no longer holds its host.
func checkDuplicates(ctx context.Context, q *sqlc.Queries, envID string, f wire.Facts) error {
	owner := func(what string, name string, err error) error {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("look up the %s: %w", what, err)
		}
		return identityRefusal(wire.RefuseDuplicateEnvironment, "duplicate_"+strings.ReplaceAll(what, " ", "_"),
			"This %s is already managed by the environment %q. Remove it there first.", what, label(name))
	}
	name, err := q.OtherEnvironmentWithEngine(ctx, sqlc.OtherEnvironmentWithEngineParams{EnvID: envID, EngineID: sql.NullString{String: f.EngineID, Valid: true}})
	if e := owner("host", name, err); e != nil {
		return e
	}
	if f.Swarm == nil {
		return nil
	}
	if f.Swarm.NodeID != "" {
		name, err = q.OtherEnvironmentWithSwarmNode(ctx, sqlc.OtherEnvironmentWithSwarmNodeParams{EnvID: envID, NodeID: sql.NullString{String: f.Swarm.NodeID, Valid: true}})
		if e := owner("swarm node", name, err); e != nil {
			return e
		}
	}
	if f.Swarm.Control && f.Swarm.ClusterID != "" {
		name, err = q.OtherEnvironmentWithCluster(ctx, sqlc.OtherEnvironmentWithClusterParams{EnvID: envID, ClusterID: sql.NullString{String: f.Swarm.ClusterID, Valid: true}})
		if e := owner("swarm", name, err); e != nil {
			return e
		}
	}
	return nil
}

// autoName is the name a host gives an environment that is still waiting for one
// (D-23): a standalone host's hostname, or a swarm manager's followed by " swarm".
// A worker leaves the placeholder for the first manager to replace. A name the
// admin typed, or one a host already gave, is never changed.
func autoName(env sqlc.Environment, f wire.Facts) string {
	if env.NameAuto == 0 && env.Name != "" {
		return ""
	}
	host := cleanName(f.Hostname)
	switch {
	case host == "":
		return ""
	case f.Swarm == nil:
		return host
	case f.Swarm.Control:
		if room := maxNameLen - utf8.RuneCountInString(swarmSuffix); utf8.RuneCountInString(host) > room {
			host = string([]rune(host)[:room])
		}
		return host + swarmSuffix
	}
	return ""
}

// apply writes the bindings and the name the Decision carries. Each statement
// only fills what is still empty, so two decisions made on the same facts are one.
func (d Decision) apply(ctx context.Context, q *sqlc.Queries, envID string, ts int64) error {
	if d.EnvType != "" {
		if _, err := q.BindEnvironmentType(ctx, sqlc.BindEnvironmentTypeParams{EnvType: sql.NullString{String: d.EnvType, Valid: true}, UpdatedAt: ts, ID: envID}); err != nil {
			return fmt.Errorf("bind the type: %w", err)
		}
	}
	if d.EngineID != "" {
		if _, err := q.BindEnvironmentEngine(ctx, sqlc.BindEnvironmentEngineParams{EngineID: sql.NullString{String: d.EngineID, Valid: true}, UpdatedAt: ts, ID: envID}); err != nil {
			return fmt.Errorf("bind the engine: %w", err)
		}
	}
	if d.ClusterID != "" {
		if _, err := q.BindEnvironmentCluster(ctx, sqlc.BindEnvironmentClusterParams{SwarmClusterID: sql.NullString{String: d.ClusterID, Valid: true}, UpdatedAt: ts, ID: envID}); err != nil {
			return fmt.Errorf("bind the cluster: %w", err)
		}
	}
	if d.Name != "" {
		if _, err := q.NameEnvironmentAuto(ctx, sqlc.NameEnvironmentAutoParams{Name: d.Name, UpdatedAt: ts, ID: envID}); err != nil {
			return fmt.Errorf("name the environment: %w", err)
		}
	}
	return nil
}

// envLabel is how an environment is named in a refusal.
func envLabel(env sqlc.Environment) string { return label(env.Name) }

// label cleans an environment name for a refusal's text: an admin typed it, and a
// blank one shows as the placeholder.
func label(name string) string {
	if name = cleanName(name); name != "" {
		return name
	}
	return placeholderName
}
