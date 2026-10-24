// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// maxStackQuery is the longest search the stack lists take, in characters.
const maxStackQuery = 128

// The status chips of the stack lists, in the order they show.
const (
	stackStatusDegraded     = "degraded"
	stackStatusStopped      = "stopped"
	stackStatusDraft        = "draft"
	stackStatusDeployFailed = "deploy_failed"

	stackKindManaged   = "managed"
	stackKindUnmanaged = "unmanaged"

	stackSortName         = "name"
	stackSortState        = "state"
	stackSortLastDeployed = "lastDeployed"
)

// problemKindInvalid is the problem type of a kind filter that is neither managed nor unmanaged.
const problemKindInvalid = "kind_invalid"

const (
	reasonOwnStack   = "This is Bayplan's own stack."
	reasonAgentStack = "This stack runs Bayplan's agent."
)

var stackStatuses = []string{stackStatusDegraded, stackStatusStopped, stackStatusDraft, stackStatusDeployFailed}

// stateRank is the order the State column sorts in, the priority of UI-SPEC "Color": an operation under
// way, then stopped, deploy failed, not running, degraded, running, and a stack never deployed last.
// A state Bayplan could not read sits with deploy failed: it needs a look.
var stateRank = map[string]int{
	string(stackops.StateRemoving):     0,
	string(stackops.StateDeploying):    1,
	string(stackops.StateRollingBack):  1,
	string(stackops.StateStarting):     1,
	string(stackops.StateStopping):     1,
	string(stackops.StateStopped):      2,
	string(stackops.StateDeployFailed): 3,
	string(stackops.StateUnknown):      4,
	string(stackops.StateNotRunning):   5,
	string(stackops.StateDegraded):     6,
	string(stackops.StateRunning):      7,
	string(stackops.StateNotDeployed):  8,
}

// StackStatusCountsBody is how many stacks each status chip of the lists counts. A stack that is both
// stopped and has a draft is in both.
type StackStatusCountsBody struct {
	Degraded     int `json:"degraded" doc:"Stacks with some but not all of their services running"`
	Stopped      int `json:"stopped" doc:"Stacks a person stopped"`
	Draft        int `json:"draft" doc:"Stacks whose draft holds changes that have not been deployed"`
	DeployFailed int `json:"deployFailed" doc:"Stacks whose newest deploy, update or roll back failed, was cancelled or was interrupted"`
}

// StackQueryInput is the search, status filter and sort both stack lists take.
type StackQueryInput struct {
	Q      string `query:"q" doc:"Search: a case-insensitive substring of the stack name or of one of its service names, at most 128 characters (else 422 query_too_long). Applied before the 500-row cap"`
	Status string `query:"status" maxLength:"64" doc:"Only stacks in one of these statuses, comma separated: degraded, stopped, draft (a draft with changes not yet deployed) or deploy_failed. Applied before the 500-row cap; any other value is refused 422 status_invalid"`
	Kind   string `query:"kind" maxLength:"16" doc:"Only stacks Bayplan manages (managed) or only stacks found on the engine that Bayplan did not make (unmanaged); absent lists both. Any other value is refused 422 kind_invalid. The status filter narrows managed stacks only, so a status with kind=unmanaged lists nothing. Applied before the 500-row cap"`
	Sort   string `query:"sort" enum:"name,state,lastDeployed" doc:"What the rows are ordered by: name (the default), state (by the priority the State column sorts in: an operation under way, stopped, deploy failed, not running, degraded, running, not deployed) or lastDeployed (a stack never deployed comes last whichever way). Ties break by name"`
	Dir    string `query:"dir" enum:"asc,desc" doc:"asc (the default) or desc"`
}

// stackRow is a stack of a list with what the search and the sort read.
type stackRow struct {
	item StackItem
	// services are the lower-cased full and short names of the services the stack owns on the engine.
	services []string
	// deployedAt is when the newest successful deploy ended, zero for none.
	deployedAt time.Time
	// envID and envName say which environment the stack is on. The name breaks a tie between stacks of two
	// environments; both are empty on an environment's own list.
	envID, envName string
}

// stackQuery is a parsed StackQueryInput.
type stackQuery struct {
	q        string
	statuses []string
	kind     string
	sort     string
	desc     bool
}

// newStackQuery checks the search and the status filter the caller sent.
func newStackQuery(in StackQueryInput) (stackQuery, error) {
	if utf8.RuneCountInString(in.Q) > maxStackQuery {
		return stackQuery{}, httpx.Problem(http.StatusUnprocessableEntity, problemQueryTooLong,
			"The search can be at most "+strconv.Itoa(maxStackQuery)+" characters.")
	}
	q := stackQuery{q: strings.ToLower(strings.TrimSpace(in.Q)), sort: in.Sort, desc: in.Dir == "desc"}
	switch in.Kind {
	case "", stackKindManaged, stackKindUnmanaged:
		q.kind = in.Kind
	default:
		return stackQuery{}, httpx.Problem(http.StatusUnprocessableEntity, problemKindInvalid,
			"kind "+strconv.Quote(in.Kind)+" is not one of "+stackKindManaged+" or "+stackKindUnmanaged+".")
	}
	if q.sort == "" {
		q.sort = stackSortName
	}
	for part := range strings.SplitSeq(in.Status, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if !slices.Contains(stackStatuses, part) {
			return stackQuery{}, httpx.Problem(http.StatusUnprocessableEntity, problemStatusInvalid,
				"status "+strconv.Quote(part)+" is not one of "+strings.Join(stackStatuses, ", ")+".")
		}
		if !slices.Contains(q.statuses, part) {
			q.statuses = append(q.statuses, part)
		}
	}
	return q, nil
}

// hasStackStatus says whether a row counts under one status value. The chips are for stacks Bayplan
// manages: an unmanaged stack has no draft, no deploys and no stop of Bayplan's, and is in none of them.
func hasStackStatus(it StackItem, status string) bool {
	if !it.Managed {
		return false
	}
	switch status {
	case stackStatusDegraded:
		return it.State == string(stackops.StateDegraded)
	case stackStatusStopped:
		return it.State == string(stackops.StateStopped)
	case stackStatusDraft:
		return it.HasDraft
	case stackStatusDeployFailed:
		return it.State == string(stackops.StateDeployFailed)
	}
	return false
}

// stackStatusCounts counts the rows by status. It is called on every row, before the search and the
// status filter, so the numbers on the chips never shrink with what the caller typed.
func stackStatusCounts(rows []stackRow) StackStatusCountsBody {
	var c StackStatusCountsBody
	for _, r := range rows {
		if hasStackStatus(r.item, stackStatusDegraded) {
			c.Degraded++
		}
		if hasStackStatus(r.item, stackStatusStopped) {
			c.Stopped++
		}
		if hasStackStatus(r.item, stackStatusDraft) {
			c.Draft++
		}
		if hasStackStatus(r.item, stackStatusDeployFailed) {
			c.DeployFailed++
		}
	}
	return c
}

// stackKindCounts counts the rows by kind, before the search, the status filter and the kind itself.
func stackKindCounts(rows []stackRow) StackKindCountsBody {
	c := StackKindCountsBody{All: len(rows)}
	for _, r := range rows {
		if r.item.Managed {
			c.Managed++
		} else {
			c.Unmanaged++
		}
	}
	return c
}

// matches says whether a row passes the search, the kind and the status filter.
func (q stackQuery) matches(r stackRow) bool {
	if q.kind == stackKindManaged && !r.item.Managed || q.kind == stackKindUnmanaged && r.item.Managed {
		return false
	}
	if q.q != "" && !strings.Contains(strings.ToLower(r.item.Name), q.q) &&
		!slices.ContainsFunc(r.services, func(s string) bool { return strings.Contains(s, q.q) }) {
		return false
	}
	return len(q.statuses) == 0 || slices.ContainsFunc(q.statuses, func(s string) bool { return hasStackStatus(r.item, s) })
}

// compare orders two rows by the sort and direction asked for, and breaks a tie by name, then
// environment name, then ID, so the order never depends on how the rows arrived. A direction turns the
// key round and never the tie-break; a stack never deployed comes after every deployed one either way.
func (q stackQuery) compare(a, b stackRow) int {
	key := 0
	switch q.sort {
	case stackSortState:
		key = stateRank[a.item.State] - stateRank[b.item.State]
	case stackSortLastDeployed:
		switch {
		case a.deployedAt.IsZero() && b.deployedAt.IsZero():
		case a.deployedAt.IsZero():
			return 1
		case b.deployedAt.IsZero():
			return -1
		default:
			key = a.deployedAt.Compare(b.deployedAt)
		}
	case stackSortName:
		key = compareText(a.item.Name, b.item.Name)
	}
	if q.desc {
		key = -key
	}
	if key != 0 {
		return key
	}
	if c := compareText(a.item.Name, b.item.Name); c != 0 {
		return c
	}
	if c := compareText(a.envName, b.envName); c != 0 {
		return c
	}
	return strings.Compare(a.item.ID, b.item.ID)
}

// apply keeps the rows that match, in the order asked for, and returns the first maxListRows of them
// with how many matched. It runs before the cap, so the cap applies to what the caller asked for.
func (q stackQuery) apply(rows []stackRow) (shown []stackRow, total int) {
	matched := make([]stackRow, 0, len(rows))
	for _, r := range rows {
		if q.matches(r) {
			matched = append(matched, r)
		}
	}
	slices.SortStableFunc(matched, q.compare)
	return matched[:min(len(matched), maxListRows)], len(matched)
}

// AllStacksGroup is the stacks of one environment in the answer of the All stacks list.
type AllStacksGroup struct {
	EnvID   string                `json:"envId" doc:"The environment"`
	EnvName string                `json:"envName" doc:"Its name"`
	Target  string                `json:"target" enum:"swarm,standalone" doc:"Where the environment's stacks deploy: swarm or standalone"`
	Stacks  []StackItem           `json:"stacks" doc:"The environment's stacks among the rows returned, in the order asked for"`
	Counts  StackStatusCountsBody `json:"counts" doc:"How many of the environment's stacks each status chip counts, before the search and the status filter"`
	// KindCounts and DiscoveryFailed are the environment's own.
	KindCounts      StackKindCountsBody `json:"kindCounts" doc:"How many of the environment's stacks are managed and unmanaged, before the search, the status filter and the kind"`
	DiscoveryFailed bool                `json:"discoveryFailed" doc:"True when the look for unmanaged stacks on this environment failed just now: its managed stacks are listed all the same"`
}

// FailedEnvironmentBody names an environment whose stacks could not be read, so the list can say so
// in place of its group while the others render.
type FailedEnvironmentBody struct {
	EnvID   string `json:"envId" doc:"The environment"`
	EnvName string `json:"envName" doc:"Its name"`
}

// AllStacksBody is the stacks of every environment the caller may read them on.
type AllStacksBody struct {
	Groups             []AllStacksGroup        `json:"groups" doc:"One group for each environment that has stacks among the rows returned, by environment name. An environment the caller holds no stacks.read on is absent"`
	FailedEnvironments []FailedEnvironmentBody `json:"failedEnvironments" doc:"Environments the caller may read stacks on whose stacks could not be read just now"`
	Total              int                     `json:"total" doc:"How many stacks of the environments read match the search and the status filter, whatever the cap"`
	UnfilteredTotal    int                     `json:"unfilteredTotal" doc:"How many stacks the environments read have before the search and the status filter"`
	Capped             bool                    `json:"capped" doc:"True when total is more than the rows returned, counted over every group together"`
	StatusCounts       StackStatusCountsBody   `json:"statusCounts" doc:"How many stacks each status chip counts, over the environments read and before the search and the status filter"`
	KindCounts         StackKindCountsBody     `json:"kindCounts" doc:"How many stacks the Show toggle counts (all, managed, unmanaged), over the environments read and before the search, the status filter and the kind"`
}

// AllStacksInput is the query of GET /api/stacks.
type AllStacksInput struct {
	Env string `query:"env" maxLength:"2048" doc:"Only these environments, by ID, comma separated. An ID the caller cannot read stacks on is ignored, as one that does not exist"`
	StackQueryInput
}

// AllStacksOutput wraps AllStacksBody for huma.
type AllStacksOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AllStacksBody
}

// canIn says whether the grants listed for each environment include perm for id, directly or
// through an assignment that covers every environment.
func canIn(perEnv map[string][]rbac.Perm, id string, perm rbac.Perm) bool {
	return slices.Contains(perEnv[id], perm) || slices.Contains(perEnv[rbac.AllEnvironments], perm)
}

// splitIDs reads a comma list, once each and in the order written.
func splitIDs(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" && !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	return out
}

func registerAllStacks(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "listAllStacks", Method: http.MethodGet, Path: "/api/stacks",
		Summary: "The stacks of every environment the caller may read them on, managed or found",
		Description: "One group for each environment that has stacks, by environment name, each with its stacks, " +
			"their state and when they were last deployed. An environment the caller does not hold stacks.read on is left out, " +
			"and so is its name: the list never reveals what the caller cannot open. An environment whose stacks could not be read " +
			"is named in failedEnvironments while the others are answered. env chooses environments by ID. " +
			"Stacks Bayplan did not make are listed beside the managed ones (managed false, unmanaged body); kind (managed or unmanaged) lists one kind only, and a group's discoveryFailed says the look for them on that environment failed. " +
			"The search (q: a stack name or one of its service names), the status filter (status: degraded, stopped, draft, deploy_failed) and the sort " +
			"run over every environment together, before the 500-row cap, so at most 500 stacks come back across all the groups, " +
			"with total the number that matched, unfilteredTotal the number before the search and the status filter and statusCounts the chip numbers, counted before them too.",
		Tags: []string{"stacks"},
	}, rbac.SelfService, func(ctx context.Context, in *AllStacksInput) (*AllStacksOutput, error) {
		return listAllStacks(ctx, d, in)
	})
}

func listAllStacks(ctx context.Context, d StacksDeps, in *AllStacksInput) (*AllStacksOutput, error) {
	p := httpx.PrincipalFrom(ctx)
	if p == nil {
		return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
	}
	q, err := newStackQuery(in.StackQueryInput)
	if err != nil {
		return nil, err
	}
	_, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
	if err != nil {
		return nil, stackInternal(d, "effective permissions", "", err)
	}
	all, err := d.Store.RQ.ListEnvironments(ctx)
	if err != nil {
		return nil, stackInternal(d, "list environments", "", err)
	}
	chosen := splitIDs(in.Env)
	body := AllStacksBody{Groups: []AllStacksGroup{}, FailedEnvironments: []FailedEnvironmentBody{}}

	// Every row of every environment read, with each environment's own counts.
	var rows []stackRow
	type read struct {
		name, target    string
		counts          StackStatusCountsBody
		kinds           StackKindCountsBody
		discoveryFailed bool
	}
	readEnvs := map[string]read{}
	// bare are the environments with no stacks at all whose look for unmanaged ones failed: their group holds
	// the retry row alone.
	var bare []string
	for _, e := range all {
		if e.Enabled == 0 || !canIn(perEnv, e.ID, rbac.PermStacksRead) {
			continue
		}
		if len(chosen) > 0 && !slices.Contains(chosen, e.ID) {
			continue
		}
		got, err := d.stackRowsOf(ctx, e.ID, e.Name, e.Kind == envs.KindLocal)
		envRows := got.rows
		if err != nil {
			d.Log.Error("the stacks of an environment could not be read for the all-stacks list", "environment", e.ID, "error", err)
			body.FailedEnvironments = append(body.FailedEnvironments, FailedEnvironmentBody{EnvID: e.ID, EnvName: e.Name})
			continue
		}
		if len(envRows) == 0 && !got.discoveryFailed {
			continue
		}
		target := got.target
		if len(envRows) > 0 {
			target = envRows[0].item.Target
		}
		readEnvs[e.ID] = read{name: e.Name, target: target, counts: stackStatusCounts(envRows), kinds: stackKindCounts(envRows), discoveryFailed: got.discoveryFailed}
		rows = append(rows, envRows...)
		if len(envRows) == 0 {
			bare = append(bare, e.ID)
		}
	}

	shown, total := q.apply(rows)
	body.Total, body.UnfilteredTotal, body.Capped, body.StatusCounts = total, len(rows), total > len(shown), stackStatusCounts(rows)
	body.KindCounts = stackKindCounts(rows)
	index := map[string]int{}
	group := func(envID string) int {
		g, ok := index[envID]
		if !ok {
			e := readEnvs[envID]
			body.Groups = append(body.Groups, AllStacksGroup{
				EnvID: envID, EnvName: e.name, Target: e.target, Stacks: []StackItem{}, Counts: e.counts,
				KindCounts: e.kinds, DiscoveryFailed: e.discoveryFailed,
			})
			g = len(body.Groups) - 1
			index[envID] = g
		}
		return g
	}
	for _, r := range shown {
		g := group(r.envID)
		body.Groups[g].Stacks = append(body.Groups[g].Stacks, r.item)
	}
	for _, envID := range bare {
		group(envID)
	}
	slices.SortStableFunc(body.Groups, func(a, b AllStacksGroup) int { return compareText(a.EnvName, b.EnvName) })
	slices.SortStableFunc(body.FailedEnvironments, func(a, b FailedEnvironmentBody) int { return compareText(a.EnvName, b.EnvName) })
	return &AllStacksOutput{CacheControl: "no-store", Body: body}, nil
}

// envStackRows is the list rows of one environment.
type envStackRows struct {
	rows []stackRow
	// target is where the environment's stacks deploy, known when the look for unmanaged stacks found out.
	target string
	// discoveryFailed is true when what the environment runs could not be read for the look for unmanaged
	// stacks: the managed rows are all there, the unmanaged ones are missing.
	discoveryFailed bool
}

// stackRowsOf builds the list rows of one environment's stacks, by name: the rows Bayplan stores for
// them and, for a stack that has had an operation, what the engine runs, from the caches the lists
// use, then the stacks found on the engine that Bayplan did not make (stackops.Discover) from the same
// caches. envName is kept on each row to break a tie between environments. selfHost is true for the
// Docker host Bayplan itself runs on, the only one whose swarm namespace can be Bayplan's own.
func (d StacksDeps) stackRowsOf(ctx context.Context, envID, envName string, selfHost bool) (envStackRows, error) {
	if d.ReadEnv != nil {
		if err := d.ReadEnv(ctx, envID); err != nil {
			return envStackRows{}, err
		}
	}
	repo := d.repo()
	list, err := repo.Stacks(ctx, envID)
	if err != nil {
		return envStackRows{}, err
	}
	latest, err := repo.LatestDeploys(ctx, envID)
	if err != nil {
		return envStackRows{}, err
	}
	likes, err := repo.LatestDeployLikes(ctx, envID)
	if err != nil {
		return envStackRows{}, err
	}
	done, err := repo.LastDeployed(ctx, envID)
	if err != nil {
		return envStackRows{}, err
	}
	drafts, err := repo.DraftSaves(ctx, envID)
	if err != nil {
		return envStackRows{}, err
	}
	reads := newStackReads(ctx, d, envID)
	out := envStackRows{rows: make([]stackRow, 0, len(list))}
	for _, st := range list {
		l, hasL := latest[st.ID]
		k, hasK := likes[st.ID]
		g, hasG := done[st.ID]
		item, services := stackItemWithServices(reads, st, l, hasL, k, hasK, g, hasG, drafts[st.ID].Pending)
		r := stackRow{item: item, envID: envID, envName: envName}
		if hasG {
			r.deployedAt = g.EndedAt
		}
		for _, s := range services {
			r.services = append(r.services, strings.ToLower(s.Name), strings.ToLower(s.Short))
		}
		out.rows = append(out.rows, r)
	}
	found, target, ok := d.discover(ctx, reads, list, selfHost)
	out.target, out.discoveryFailed = string(target), !ok
	for _, u := range found {
		out.rows = append(out.rows, unmanagedRow(u, envID, envName))
	}
	return out, nil
}

// discover finds the stacks of an environment that Bayplan did not make, in the caches the lists use. The
// bool is false when they could not be read: the caller lists the managed stacks without them.
func (d StacksDeps) discover(ctx context.Context, reads *stackReads, managed []stackops.Stack, selfHost bool) ([]stackops.Discovered, stacks.Target, bool) {
	envID := reads.envID
	var target stacks.Target
	switch {
	case len(managed) > 0:
		target = managed[0].Target
	case d.Clusters == nil:
		return nil, "", false
	default:
		swarm, err := d.Clusters.IsSwarm(ctx, envID)
		if err != nil {
			d.Log.Debug("could not tell what kind of environment this is for its unmanaged stacks", "environment", envID, "error", err)
			return nil, "", false
		}
		target = stacks.TargetStandalone
		if swarm {
			target = stacks.TargetSwarm
		}
	}
	h := reads.heldOf(target == stacks.TargetSwarm)
	if h.err != nil {
		return nil, target, false
	}
	held := h.held
	self := stackops.SelfMarks{ContainerID: d.SelfContainerID}
	if selfHost {
		self.Namespace = d.SelfNamespace
	}
	return stackops.Discover(held, managed, target, self), target, true
}

// unmanagedRow is the list row of a stack found on the engine. It has no ID: it is known by its name on its
// environment, and no label value is ever used as an ID.
func unmanagedRow(u stackops.Discovered, envID, envName string) stackRow {
	running, total := u.Running, u.Total
	body := &StackUnmanagedBody{Orphan: u.OrphanID != "", Portainer: u.Portainer, Owner: u.Owner, Adoptable: u.Owner == ""}
	switch u.Owner {
	case stackops.OwnerBayplan:
		body.Reason = reasonOwnStack
	case stackops.OwnerAgent:
		body.Reason = reasonAgentStack
	}
	r := stackRow{
		item: StackItem{
			Name: u.Name, Target: string(u.Target), Source: "unmanaged", Managed: false, State: string(u.State),
			ServicesRunning: &running, ServicesTotal: &total, Unmanaged: body,
		},
		envID: envID, envName: envName,
	}
	for _, s := range u.Rows {
		r.services = append(r.services, strings.ToLower(s.Name), strings.ToLower(s.Short))
	}
	return r
}

// compareText orders two names case-insensitively, then exactly, so the order never depends on how
// the rows arrived.
func compareText(a, b string) int {
	if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// StackNameCheckBody is the verdict on a stack name for one environment.
type StackNameCheckBody struct {
	Status  string `json:"status" enum:"available,taken,clash,invalid" doc:"available: the name can be used. taken: one of Bayplan's stacks on this environment has it. clash: something Bayplan does not manage has it on this environment (D-12). invalid: the name breaks the rule"`
	Message string `json:"message" doc:"The sentence to show for a name that cannot be used, naming the environment where it matters; empty when it is available"`
}

// StackNameCheckInput is the path and query of the name check.
type StackNameCheckInput struct {
	EnvironmentPathInput
	Name string `query:"name" maxLength:"200" doc:"The name to check"`
}

// StackNameCheckOutput wraps StackNameCheckBody for huma.
type StackNameCheckOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackNameCheckBody
}

func stackNameTakenMessage(name, envName string) string {
	return "A stack named " + name + " already exists on " + envName + "."
}

func registerStackNameCheck(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "checkStackName", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/name-check",
		Summary: "Whether a stack name can be used on an environment",
		Description: "Answers available, taken (a stack of Bayplan's has it on this environment), clash (something Bayplan does not manage has it: a swarm namespace or a Compose project of that name) or invalid, with the sentence to show. " +
			"The Create stack dialog asks when the name field loses focus and again on submit. It only reads: nothing is stored, audited or sent to an engine. " +
			"It needs stacks.create on the environment, so it cannot be used to probe what an environment holds by anyone who may not create there.",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksCreate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackNameCheckInput) (*StackNameCheckOutput, error) {
			return checkStackName(ctx, d, in)
		})
}

func checkStackName(ctx context.Context, d StacksDeps, in *StackNameCheckInput) (*StackNameCheckOutput, error) {
	env := httpx.EnvFrom(ctx)
	if env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	answer := func(status, message string) (*StackNameCheckOutput, error) {
		return &StackNameCheckOutput{CacheControl: "no-store", Body: StackNameCheckBody{Status: status, Message: message}}, nil
	}
	name := in.Name
	if !stacks.NameValid(name) {
		return answer("invalid", stackNameInvalidMessage)
	}
	own, err := d.repo().Stacks(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "name check", env.ID, err)
	}
	if slices.ContainsFunc(own, func(s stackops.Stack) bool { return s.Name == name }) {
		return answer("taken", stackNameTakenMessage(name, env.Name))
	}
	if d.Clusters == nil {
		d.Log.Error("the stack routes have no cluster reader")
		return nil, huma.Error500InternalServerError("internal error")
	}
	swarm, err := d.Clusters.IsSwarm(ctx, env.ID)
	if err != nil {
		return nil, snapshotProblem(d.Log, env.ID, err)
	}
	target := stacks.TargetStandalone
	if swarm {
		target = stacks.TargetSwarm
	}
	foreign, err := d.ops().ForeignStack(ctx, env.ID, target, name, "")
	if err != nil {
		return nil, snapshotProblem(d.Log, env.ID, err)
	}
	if foreign {
		return answer("clash", stackops.NameClashMessage(name))
	}
	return answer("available", "")
}
