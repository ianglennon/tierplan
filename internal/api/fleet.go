// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/updates"
)

// The fleet views read across the environments a person may read (plan 12-26, ENV-10, D-17). They
// read live, through the 5 s caches the environment pages use, and name every environment they
// could not read; an environment the caller may not read is neither shown, counted nor named.

const (
	// fleetParallel is how many environments one request reads at once (T-12-79).
	fleetParallel = 8
	// fleetEnvTimeout is how long one environment may take before it is named not reporting.
	fleetEnvTimeout = 5 * time.Second

	fleetKindService   = "service"
	fleetKindContainer = "container"

	// Why an environment is not (fully) included, in the P2 words.
	fleetOffline           = "offline"
	fleetNotReporting      = "not_reporting"
	fleetWaitingFirst      = "waiting_first_agent"
	fleetNodesNotReporting = "nodes_not_reporting"
)

// FleetEnvLister lists the environments (envs.Service).
type FleetEnvLister interface {
	List(ctx context.Context) ([]envs.Environment, error)
}

// FleetSnapshots reads an environment's resources and health (envs.Aggregator).
type FleetSnapshots interface {
	Snapshot(ctx context.Context, envID string) (envs.Aggregate, envs.Health, error)
}

// FleetClusters reads an environment's cluster (envs.Clusters).
type FleetClusters interface {
	IsSwarm(ctx context.Context, envID string) (bool, error)
	State(ctx context.Context, envID string) (envs.ClusterState, error)
	Policies(envID string) *envs.PolicyCache
}

// FleetUpdates adds up an environment's image update verdicts (updates.Service).
type FleetUpdates interface {
	Tally(ctx context.Context, envID string) (updates.Tally, error)
	// EnvVerdicts judges every checkable workload of an environment and says how far the checks reach.
	EnvVerdicts(ctx context.Context, envID string) ([]updates.WorkloadVerdict, updates.Coverage, error)
	// Workloads are what a check of an environment asks the registries about.
	Workloads(ctx context.Context, envID string) ([]updates.Workload, error)
	// CheckFleet checks the workloads of several environments now, forced.
	CheckFleet(ctx context.Context, workloads map[string][]updates.Workload) (map[string]updates.CheckResult, error)
}

// FleetAddresses reads an environment's address space (envs.AddressSpaces).
type FleetAddresses interface {
	Read(ctx context.Context, envID string) (envs.AddressSpace, error)
}

// FleetDeps are the dependencies of the fleet routes. A nil Updates or AddressSpaces leaves
// its tile null.
type FleetDeps struct {
	Envs          FleetEnvLister
	Aggregator    FleetSnapshots
	Clusters      FleetClusters
	Updates       FleetUpdates
	AddressSpaces FleetAddresses
	// Checker says which environments the caller may read.
	Checker *rbac.Checker
	// Store reads the stacks TierPlan manages, which decide whether a Compose container can be updated
	// on its own. Nil means none is known, so a container of a Compose project is not selectable.
	Store *store.Store
	// EnvTimeout is how long one environment may take; zero means five seconds.
	EnvTimeout time.Duration
	// SearchTimeout is how long the search may take to read one environment; zero means three seconds.
	SearchTimeout time.Duration
	// Now stamps the answer. Nil means time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

func (d FleetDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d FleetDeps) timeout() time.Duration {
	if d.EnvTimeout > 0 {
		return d.EnvTimeout
	}
	return fleetEnvTimeout
}

func (d FleetDeps) searchTimeout() time.Duration {
	if d.SearchTimeout > 0 {
		return d.SearchTimeout
	}
	return searchEnvTimeout
}

// FleetNotIncludedBody names an environment a fleet view could not read, or could read only in part.
type FleetNotIncludedBody struct {
	EnvID   string `json:"envId" doc:"The environment's ID"`
	EnvName string `json:"envName" doc:"The environment's name"`
	Reason  string `json:"reason" enum:"offline,not_reporting,waiting_first_agent,nodes_not_reporting" doc:"offline: the environment cannot be reached. not_reporting: it did not answer in time or its cluster cannot be read. waiting_first_agent: no agent has enrolled yet. nodes_not_reporting: it was read, but the nodes in nodes did not answer, so what they run is missing"`
	Nodes   int    `json:"nodes" minimum:"0" doc:"How many nodes did not report; 0 unless the reason is nodes_not_reporting"`
}

// FleetFailingItem is one failing swarm service or standalone container.
type FleetFailingItem struct {
	Kind      string  `json:"kind" enum:"service,container" doc:"service for a swarm service, container for a standalone container"`
	ID        string  `json:"id" doc:"The service or container ID"`
	Name      string  `json:"name" doc:"The service or container name"`
	EnvID     string  `json:"envId" doc:"The environment it is in"`
	EnvName   string  `json:"envName" doc:"The name of that environment"`
	Stack     string  `json:"stack" doc:"The stack or Compose project it belongs to; empty when it has none"`
	Node      string  `json:"node" doc:"The node a container runs on; empty for a service"`
	NodeID    string  `json:"nodeId" doc:"The ID of that node, for the link to the container; empty for a service"`
	State     string  `json:"state" doc:"A service: '{running} of {desired} running'. A container: restarting, exited or unhealthy"`
	Running   *uint64 `json:"running" nullable:"true" doc:"A service's running tasks; null for a container"`
	Desired   *uint64 `json:"desired" nullable:"true" doc:"A service's desired tasks; null for a container"`
	Since     string  `json:"since" format:"date-time" doc:"A service: when its newest failed task failed, else when it was last updated. A container: when it reached its state, only about right because the engine rounds the age. Empty when not known"`
	LastError string  `json:"lastError" doc:"A service: the error of its newest failed task. A container: why it is listed, with its exit code. Plain text with control characters removed, at most 200 characters; empty when there is none"`
}

// FleetFailingBody is the answer of GET /api/fleet/failing.
type FleetFailingBody struct {
	Items       []FleetFailingItem     `json:"items" doc:"Every failing service and container of the environments that were read, newest first, then by environment name, then by name"`
	NotIncluded []FleetNotIncludedBody `json:"notIncluded" doc:"The environments the caller may read that could not be read, or only in part. Environments the caller may not read are never named"`
	ReadAt      string                 `json:"readAt" format:"date-time" doc:"When the answer was put together"`
}

// FleetFailingOutput wraps FleetFailingBody for huma.
type FleetFailingOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         FleetFailingBody
}

// FleetFailingInput is the query of GET /api/fleet/failing.
type FleetFailingInput struct {
	Env []string `query:"env,explode" maxItems:"100" doc:"Only these environments. An ID the caller may not read is ignored, as if it were not given. Empty means every environment the caller may read"`
}

// FleetEnvironmentsTile is the Environments tile.
type FleetEnvironmentsTile struct {
	Total         int `json:"total" minimum:"0" doc:"The environments the caller may read"`
	NeedAttention int `json:"needAttention" minimum:"0" doc:"How many of them are not online, did not answer, or answered for only some of their nodes"`
}

// FleetFailingTile is the Failing services tile.
type FleetFailingTile struct {
	Count        *int `json:"count" nullable:"true" minimum:"0" doc:"Failing services and containers in the environments that were read. Null when it is not known: nothing failed in what was read but an environment was not read"`
	Environments *int `json:"environments" nullable:"true" minimum:"0" doc:"How many environments have any. Null when count is"`
}

// FleetUpdatesTile is the Updates available tile.
type FleetUpdatesTile struct {
	Count         *int   `json:"count" nullable:"true" minimum:"0" doc:"Workloads with an update available in the environments that were read. Null when it could not be read, or when it is 0 but an environment was not read"`
	ChecksFailed  *int   `json:"checksFailed" nullable:"true" minimum:"0" doc:"Workloads whose last check failed. Null when it could not be read"`
	LastCheckedAt string `json:"lastCheckedAt" format:"date-time" doc:"The newest time any workload was checked, whatever came of it; empty when none was"`
	AllChecked    bool   `json:"allChecked" doc:"True only when every environment was read, no check failed and nothing is waiting for its first check"`
}

// FleetPoolsTile is the Pools near full tile.
type FleetPoolsTile struct {
	NearFull   *int `json:"nearFull" nullable:"true" minimum:"0" doc:"Pools and overlay networks at 70% or more used (the almost full ones included). Null when it could not be read, or when it is 0 but an environment was not read"`
	AlmostFull *int `json:"almostFull" nullable:"true" minimum:"0" doc:"Those at 90% or more. Null when nearFull is"`
}

// FleetSummaryBody is the answer of GET /api/fleet/summary.
type FleetSummaryBody struct {
	Environments FleetEnvironmentsTile  `json:"environments"`
	Failing      FleetFailingTile       `json:"failing"`
	Updates      FleetUpdatesTile       `json:"updates"`
	Pools        FleetPoolsTile         `json:"pools"`
	NotIncluded  []FleetNotIncludedBody `json:"notIncluded" doc:"The environments the caller may read that could not be read, or only in part"`
	ReadAt       string                 `json:"readAt" format:"date-time" doc:"When the answer was put together"`
}

// FleetSummaryOutput wraps FleetSummaryBody for huma.
type FleetSummaryOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         FleetSummaryBody
}

// fleetResult is what the read of one environment gave.
type fleetResult[T any] struct {
	Env envs.Environment
	Val T
	// Nodes is how many nodes of a read environment did not report.
	Nodes int
	Err   error
}

// fleetReadable is the environments the caller may read, in the order the registry lists them,
// narrowed to filter when it is not empty. An ID in filter that is not readable is ignored.
func fleetReadable(ctx context.Context, d FleetDeps, p *rbac.Principal, filter []string) ([]envs.Environment, error) {
	list, _, err := fleetReadableWith(ctx, d, p, filter)
	return list, err
}

// fleetReadableWith is fleetReadable with the permissions it judged them by, for a route that needs another one of the caller.
func fleetReadableWith(ctx context.Context, d FleetDeps, p *rbac.Principal, filter []string) ([]envs.Environment, map[string][]rbac.Perm, error) {
	_, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
	if err != nil {
		return nil, nil, fmt.Errorf("effective permissions: %w", err)
	}
	all, err := d.Envs.List(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list environments: %w", err)
	}
	out := make([]envs.Environment, 0, len(all))
	for _, e := range all {
		if !canRead(perEnv, e.ID) {
			continue
		}
		if len(filter) > 0 && !slices.Contains(filter, e.ID) {
			continue
		}
		out = append(out, e)
	}
	return out, perEnv, nil
}

// fleetFanOut reads every environment the caller may read (envFilter narrows them), at most
// fleetParallel at once and each for at most the environment timeout, and returns one result per
// environment in the order of fleetReadable. A read that fails or runs out of time is in its
// result; it does not stop the others. read returns what it read and how many nodes did not report.
func fleetFanOut[T any](ctx context.Context, d FleetDeps, p *rbac.Principal, envFilter []string, read func(ctx context.Context, env envs.Environment) (T, int, error)) ([]fleetResult[T], error) {
	list, err := fleetReadable(ctx, d, p, envFilter)
	if err != nil {
		return nil, err
	}
	return fleetReadEach(ctx, d, list, read), nil
}

// fleetReadEach is fleetFanOut over environments already judged readable.
func fleetReadEach[T any](ctx context.Context, d FleetDeps, list []envs.Environment, read func(ctx context.Context, env envs.Environment) (T, int, error)) []fleetResult[T] {
	return fleetReadEachWithin(ctx, d, list, d.timeout(), read)
}

// fleetReadEachWithin is fleetReadEach with a deadline of its own for each environment, for a view that is
// allowed less time than the others (the search, D-18).
func fleetReadEachWithin[T any](ctx context.Context, d FleetDeps, list []envs.Environment, timeout time.Duration, read func(ctx context.Context, env envs.Environment) (T, int, error)) []fleetResult[T] {
	results := make([]fleetResult[T], len(list))
	var g errgroup.Group
	g.SetLimit(fleetParallel)
	for i, env := range list {
		results[i].Env = env
		g.Go(func() error {
			rctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			val, nodes, err := read(rctx, env)
			if err == nil {
				// A read that ran out of time may have given a partial answer without saying so.
				err = rctx.Err()
			}
			results[i].Val, results[i].Nodes, results[i].Err = val, nodes, err
			return nil
		})
	}
	_ = g.Wait()
	return results
}

// fleetReason is why a read failed, in the words of the Overview.
func fleetReason(err error) string {
	var down envs.ErrNotOnline
	if errors.As(err, &down) {
		switch down.Health.State {
		case envs.StatePending:
			return fleetWaitingFirst
		case envs.StateOffline:
			return fleetOffline
		}
	}
	return fleetNotReporting
}

// fleetNotIncluded names the environments of results that were not read, or were read for only
// some of their nodes, by name and then ID.
func fleetNotIncluded[T any](results []fleetResult[T]) []FleetNotIncludedBody {
	out := []FleetNotIncludedBody{}
	for _, r := range results {
		switch {
		case r.Err != nil:
			out = append(out, FleetNotIncludedBody{EnvID: r.Env.ID, EnvName: r.Env.Name, Reason: fleetReason(r.Err)})
		case r.Nodes > 0:
			out = append(out, FleetNotIncludedBody{EnvID: r.Env.ID, EnvName: r.Env.Name, Reason: fleetNodesNotReporting, Nodes: r.Nodes})
		}
	}
	slices.SortFunc(out, func(a, b FleetNotIncludedBody) int {
		return cmp.Or(strings.Compare(a.EnvName, b.EnvName), strings.Compare(a.EnvID, b.EnvID))
	})
	return out
}

// fleetEnv is what the read of one environment gave.
type fleetEnv struct {
	Health     envs.Health
	Services   []envs.FailingServiceRow
	Containers []envs.FailingContainerRow
	// Tally and TallyErr, and Meters and MetersErr, are read only for the summary.
	Tally     updates.Tally
	TallyErr  error
	Meters    []envs.WorstMeter
	MetersErr error
}

// readFleetEnv reads the failing services or containers of one environment, and with summary the
// updates and the address pools too. It returns how many nodes did not report. An error means the
// environment could not be read, and then none of it is used. Anything read after the failing rows
// that cannot be read is in TallyErr or MetersErr, so the one tile says so and the rest stand.
func readFleetEnv(ctx context.Context, d FleetDeps, env envs.Environment, summary bool) (fleetEnv, int, error) {
	var v fleetEnv
	agg, health, err := d.Aggregator.Snapshot(ctx, env.ID)
	if err != nil {
		return v, 0, err
	}
	v.Health = health
	swarm, err := d.Clusters.IsSwarm(ctx, env.ID)
	if err != nil {
		return v, 0, err
	}
	st, err := d.Clusters.State(ctx, env.ID)
	if err != nil {
		return v, 0, err
	}
	now := d.now()
	switch {
	case swarm && !st.Known():
		// No quorum, or no read yet: whether a service fails is not known.
		return v, 0, errors.New("the cluster could not be read")
	case swarm:
		v.Services = envs.FailingServiceRows(st.Read.Services, st.Read.FailedTasks, now, envs.DefaultFailingServiceGrace)
	default:
		v.Containers = envs.FailingContainerRows(agg.Containers, d.Clusters.Policies(env.ID), agg.FetchedAt)
	}
	if !summary {
		return v, len(agg.NotReporting), nil
	}
	if d.Updates == nil {
		v.TallyErr = errors.New("update checks are not available")
	} else {
		v.Tally, v.TallyErr = d.Updates.Tally(ctx, env.ID)
	}
	if d.AddressSpaces == nil {
		v.MetersErr = errors.New("address pools are not available")
	} else if space, err := d.AddressSpaces.Read(ctx, env.ID); err != nil {
		v.MetersErr = err
	} else if space.NotMeasured != "" {
		v.MetersErr = errors.New(space.NotMeasured)
	} else {
		v.Meters = envs.MetersOf(space)
	}
	return v, len(agg.NotReporting), nil
}

// fleetItems are the failing rows of the results that were read, newest first (rows without a
// time last), then by environment name, kind, name and ID, so equal times keep one order.
func fleetItems(results []fleetResult[fleetEnv]) []FleetFailingItem {
	items := []FleetFailingItem{}
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		for _, s := range r.Val.Services {
			running, desired := s.Running, s.Desired
			items = append(items, FleetFailingItem{
				Kind: fleetKindService, ID: s.ID, Name: s.Name, EnvID: r.Env.ID, EnvName: r.Env.Name, Stack: s.Stack,
				State:   fmt.Sprintf("%d of %d running", s.Running, s.Desired),
				Running: &running, Desired: &desired, Since: rfc3339(s.Since), LastError: s.LastError,
			})
		}
		for _, c := range r.Val.Containers {
			items = append(items, FleetFailingItem{
				Kind: fleetKindContainer, ID: c.ID, Name: c.Name, EnvID: r.Env.ID, EnvName: r.Env.Name, Stack: c.Stack,
				Node: c.Node, NodeID: c.NodeID, State: c.State, Since: rfc3339(c.Since), LastError: c.LastError,
			})
		}
	}
	slices.SortFunc(items, func(a, b FleetFailingItem) int {
		// RFC 3339 in UTC sorts as time does; an empty one is not known and goes last.
		switch {
		case a.Since == b.Since:
		case a.Since == "":
			return 1
		case b.Since == "":
			return -1
		default:
			return -strings.Compare(a.Since, b.Since)
		}
		return cmp.Or(
			strings.Compare(a.EnvName, b.EnvName), strings.Compare(a.Name, b.Name),
			strings.Compare(a.Kind, b.Kind), strings.Compare(a.EnvID, b.EnvID), strings.Compare(a.ID, b.ID),
		)
	})
	return items
}

// fleetSummary adds up the tiles of the results.
func fleetSummary(results []fleetResult[fleetEnv], notIncluded []FleetNotIncludedBody) FleetSummaryBody {
	b := FleetSummaryBody{NotIncluded: notIncluded}
	b.Environments.Total = len(results)
	var (
		failing, failingEnvs     int
		available, failed, unset int
		nearFull, almostFull     int
		tallyUnread, poolsUnread bool
		checkedAt                time.Time
	)
	for _, r := range results {
		if r.Err != nil || r.Nodes > 0 || r.Val.Health.State != envs.StateOnline {
			b.Environments.NeedAttention++
		}
		if r.Err != nil {
			continue
		}
		n := len(r.Val.Services) + len(r.Val.Containers)
		failing += n
		if n > 0 {
			failingEnvs++
		}
		if r.Val.TallyErr != nil {
			tallyUnread = true
		} else {
			t := r.Val.Tally
			available += t.Available
			failed += t.Failed
			unset += t.NotChecked
			if t.CheckedAt.After(checkedAt) {
				checkedAt = t.CheckedAt
			}
		}
		if r.Val.MetersErr != nil {
			poolsUnread = true
		}
		for _, m := range r.Val.Meters {
			if m.Percent >= netspace.ThresholdWarn {
				nearFull++
			}
			if m.Percent >= netspace.ThresholdFull {
				almostFull++
			}
		}
	}
	whole := len(notIncluded) == 0
	// A tile that counts to 0 over part of the fleet does not know it is 0.
	count := func(n int, unread bool) *int {
		if unread || n == 0 && !whole {
			return nil
		}
		return &n
	}
	b.Failing = FleetFailingTile{Count: count(failing, false), Environments: count(failingEnvs, false)}
	b.Updates = FleetUpdatesTile{
		Count: count(available, tallyUnread), LastCheckedAt: rfc3339(checkedAt),
		AllChecked: !tallyUnread && whole && failed == 0 && unset == 0,
	}
	if !tallyUnread {
		b.Updates.ChecksFailed = &failed
	}
	b.Pools = FleetPoolsTile{NearFull: count(nearFull, poolsUnread), AlmostFull: count(almostFull, poolsUnread)}
	return b
}

// RegisterFleet registers the cross-environment views of the Overview (ENV-10, D-17): what is
// failing across the fleet, the four counts of its tiles, and the pending updates and the pools
// near exhaustion behind two of them. Each answers for the environments
// the caller may read and nothing else; a caller with no environment gets empty lists and zeros.
func RegisterFleet(r *httpx.Router, d FleetDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	fail := func(what string, err error) error {
		d.Log.Error("fleet request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}

	httpx.Register(r, huma.Operation{
		OperationID: "listFleetFailing",
		Method:      http.MethodGet,
		Path:        "/api/fleet/failing",
		Summary:     "List what is failing across the environments the caller may read",
		Description: "Every failing swarm service (fewer tasks running than desired) and standalone container (restarting, exited with a non-zero code, or unhealthy, " +
			"and with a restart policy) of the environments the caller holds environments.read on, each with its environment, stack, state, since and last error. " +
			"An environment that could not be read, or only for some nodes, is named in notIncluded with its reason; an environment the caller may not read is neither listed nor named. " +
			"At most 8 environments are read at once, each for at most 5 seconds, from the same short-lived snapshots the environment pages use.",
		Tags: []string{"fleet"},
	}, rbac.SelfService, func(ctx context.Context, in *FleetFailingInput) (*FleetFailingOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		results, err := fleetFanOut(ctx, d, p, in.Env, func(ctx context.Context, env envs.Environment) (fleetEnv, int, error) {
			return readFleetEnv(ctx, d, env, false)
		})
		if err != nil {
			return nil, fail("read the fleet", err)
		}
		return &FleetFailingOutput{CacheControl: "no-store", Body: FleetFailingBody{
			Items: fleetItems(results), NotIncluded: fleetNotIncluded(results), ReadAt: rfc3339(d.now()),
		}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getFleetSummary",
		Method:      http.MethodGet,
		Path:        "/api/fleet/summary",
		Summary:     "The four counts of the Overview's tiles",
		Description: "Environments (total and how many need attention), failing services and containers, image updates available (with how many checks failed and when the last one was made) " +
			"and address pools at 70% or more used, over the environments the caller holds environments.read on. A count that could not be read is null, never 0: " +
			"so is a 0 that is only the count of the environments that were read while others were not (see notIncluded). An environment the caller may not read is not counted and not named.",
		Tags: []string{"fleet"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*FleetSummaryOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		results, err := fleetFanOut(ctx, d, p, nil, func(ctx context.Context, env envs.Environment) (fleetEnv, int, error) {
			return readFleetEnv(ctx, d, env, true)
		})
		if err != nil {
			return nil, fail("read the fleet", err)
		}
		body := fleetSummary(results, fleetNotIncluded(results))
		body.ReadAt = rfc3339(d.now())
		return &FleetSummaryOutput{CacheControl: "no-store", Body: body}, nil
	})

	registerFleetUpdates(r, d, fail)
	registerFleetPools(r, d, fail)
	registerSearch(r, d, fail)
}
