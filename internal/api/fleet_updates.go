// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/updates"
)

// The fleet's pending image updates (plan 12-28, ENV-10, IMG-04, IMG-05, D-17): every update the
// caller may see across the environments they may read, grouped by environment, with the counts and
// the coverage the Updates tab needs to word its empty states honestly.

const (
	// fleetCheckEvery is how often one person may check the whole fleet (UI-SPEC interpretation 11, T-12-86).
	fleetCheckEvery = 30 * time.Second

	problemTooManyRequests = "too_many_requests"

	fleetSkipNoPermission = "no_permission"
	fleetSkipOffline      = "offline"
)

// intervalLimiter lets each key through once in every interval. The clock is given by the caller, so a test needs no wait.
type intervalLimiter struct {
	every time.Duration
	mu    sync.Mutex
	last  map[string]time.Time
}

// allow reports whether key may go ahead at now, and counts it if so. A key that was let through less than the interval ago
// is refused. A clock that went back lets the key through.
func (l *intervalLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if at, ok := l.last[key]; ok && !now.Before(at) && now.Sub(at) < l.every {
		return false
	}
	// Forget the keys that are free again, so the map holds only the people who checked lately.
	if len(l.last) >= 256 {
		for k, at := range l.last {
			if now.Sub(at) >= l.every || now.Before(at) {
				delete(l.last, k)
			}
		}
	}
	l.last[key] = now
	return true
}

// The values of the show query.
const (
	fleetShowUpdates  = "updates"
	fleetShowProblems = "problems"
	fleetShowIgnored  = "ignored"
	fleetShowAll      = "all"
)

// FleetUpdateItem is one workload with an update, a problem or a skip or an ignore.
type FleetUpdateItem struct {
	Kind             string         `json:"kind" enum:"service,container" doc:"service for a swarm service, container for a standalone container"`
	ID               string         `json:"id" doc:"The service or container ID"`
	Name             string         `json:"name" doc:"The service or container name"`
	Stack            string         `json:"stack" doc:"The stack or Compose project it belongs to; empty when it has none"`
	Image            string         `json:"image" doc:"The image as written, without a digest"`
	FromDigest       string         `json:"fromDigest" doc:"The digest it runs; empty when its spec names none"`
	ToDigest         string         `json:"toDigest" doc:"The digest the tag pointed at when it was last checked successfully; empty before any check did"`
	ChangedPlatforms []string       `json:"changedPlatforms" doc:"For a multi-platform image, the platforms it runs on whose image changed. Empty when none is named"`
	Update           UpdateInfoBody `json:"update" doc:"The same update object the services and containers lists carry: state, reason, checked and last-success times, skip or ignore and failure details"`
	Selectable       bool           `json:"selectable" doc:"True when it can be updated now: an update is available, nothing runs or holds the environment, and it is not a container that must be adopted first. Skipped, ignored, rolled back and failed ones are never selectable"`
}

// FleetUpdateGroup is the items of one environment.
type FleetUpdateGroup struct {
	EnvID         string            `json:"envId" doc:"The environment's ID"`
	EnvName       string            `json:"envName" doc:"The environment's name"`
	Type          string            `json:"type" enum:"swarm,standalone" doc:"swarm for a Swarm cluster, standalone for a single Docker host"`
	LastCheckedAt string            `json:"lastCheckedAt" format:"date-time" doc:"When the newest check of an image the environment runs was made, whatever came of it; empty when none was"`
	CheckFailed   *string           `json:"checkFailed" nullable:"true" enum:"auth,rate_limited,unreachable,not_found,unsupported,other" doc:"Why a check of this environment's images failed, in TierPlan's words; null when none did"`
	Items         []FleetUpdateItem `json:"items" doc:"The environment's items for the show filter, by name and then kind"`
}

// FleetUpdateCounts is how many items each show filter lists.
type FleetUpdateCounts struct {
	Updates  int `json:"updates" minimum:"0" doc:"Updates available, queued, updating or held back by another operation"`
	Problems int `json:"problems" minimum:"0" doc:"Failed checks and failed updates (check_failed, rolled_back, rollback_failed)"`
	Ignored  int `json:"ignored" minimum:"0" doc:"Skipped and ignored updates"`
	All      int `json:"all" minimum:"0" doc:"All of them: updates, problems and ignored"`
}

// FleetUpdateCoverage is what the checks have covered, for the honest empty states.
type FleetUpdateCoverage struct {
	Checkable     int    `json:"checkable" minimum:"0" doc:"Workloads that can be checked (they follow a tag), in the environments that were read"`
	Checked       int    `json:"checked" minimum:"0" doc:"Of those, how many have been checked, whatever came of it"`
	Failed        int    `json:"failed" minimum:"0" doc:"Of those, how many have a failed check"`
	NeverChecked  int    `json:"neverChecked" minimum:"0" doc:"Of those, how many have never been checked"`
	Skipped       int    `json:"skipped" minimum:"0" doc:"How many updates are skipped or ignored"`
	Environments  int    `json:"environments" minimum:"0" doc:"How many environments were read"`
	LastCheckedAt string `json:"lastCheckedAt" format:"date-time" doc:"The newest check anywhere, whatever came of it; empty when none was made"`
	FirstCheckAt  string `json:"firstCheckAt" format:"date-time" doc:"When the schedule next checks an environment that has workloads never checked; empty when none has or no schedule will"`
}

// FleetUpdatesBody is the answer of GET /api/fleet/updates.
type FleetUpdatesBody struct {
	Groups      []FleetUpdateGroup     `json:"groups" doc:"The environments that have items for the show filter: those with a failed check first, then by name"`
	Counts      FleetUpdateCounts      `json:"counts" doc:"How many items each show value lists, over every environment that was read"`
	Coverage    FleetUpdateCoverage    `json:"coverage" doc:"What the checks have covered"`
	NotIncluded []FleetNotIncludedBody `json:"notIncluded" doc:"The environments the caller may read that could not be read, or only in part. Environments the caller may not read are never named"`
	ReadAt      string                 `json:"readAt" format:"date-time" doc:"When the answer was put together"`
}

// FleetUpdatesOutput wraps FleetUpdatesBody for huma.
type FleetUpdatesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         FleetUpdatesBody
}

// FleetUpdatesInput is the query of GET /api/fleet/updates.
type FleetUpdatesInput struct {
	Env  []string `query:"env,explode" maxItems:"100" doc:"Only these environments. An ID the caller may not read is ignored, as if it were not given. Empty means every environment the caller may read"`
	Show string   `query:"show" enum:"updates,problems,ignored,all" default:"updates" doc:"Which items to list: updates (available, queued, updating, held back), problems (failed checks and failed updates), ignored (skipped and ignored) or all of them"`
}

// fleetUpdatesEnv is what the read of one environment gave.
type fleetUpdatesEnv struct {
	Swarm    bool
	Verdicts []updates.WorkloadVerdict
	Coverage updates.Coverage
	Known    stackIDs
}

// readFleetUpdates reads the verdicts of one environment. It returns how many nodes did not report.
func readFleetUpdates(ctx context.Context, d FleetDeps, env envs.Environment) (fleetUpdatesEnv, int, error) {
	var v fleetUpdatesEnv
	agg, _, err := d.Aggregator.Snapshot(ctx, env.ID)
	if err != nil {
		return v, 0, err
	}
	if v.Swarm, err = d.Clusters.IsSwarm(ctx, env.ID); err != nil {
		return v, 0, err
	}
	if v.Verdicts, v.Coverage, err = d.Updates.EnvVerdicts(ctx, env.ID); err != nil {
		return v, 0, err
	}
	// A container of a managed stack reads "adopt first" without the stacks, so a failed read fails the environment.
	if v.Known, err = readStackIDs(ctx, d.Store, env.ID); err != nil {
		return v, 0, err
	}
	return v, len(agg.NotReporting), nil
}

// fleetShowOf is which show filter an item belongs to, empty when it belongs to none (up to date,
// stale, not checked, pinned and the like are not listed).
func fleetShowOf(s updates.State) string {
	switch s {
	case updates.StateUpdateAvailable, updates.StateUpdating, updates.StateBusy, updates.StateQueued:
		return fleetShowUpdates
	case updates.StateCheckFailed, updates.StateRolledBack, updates.StateRollbackFailed:
		return fleetShowProblems
	case updates.StateSkipped, updates.StateIgnored:
		return fleetShowIgnored
	}
	return ""
}

// fleetImage is the image of a workload as written, the digest dropped.
func fleetImage(w updates.Workload) string {
	s, _, _ := strings.Cut(w.Written, "@")
	if s == "" && w.Ref.Tag != "" {
		s = w.Ref.Repo() + ":" + w.Ref.Tag
	}
	return s
}

// fleetUpdateItem builds the item of one workload.
func fleetUpdateItem(wv updates.WorkloadVerdict, known stackIDs) FleetUpdateItem {
	w, v := wv.Workload, wv.Verdict
	info := updateInfoOf(v, w)
	if w.Kind == fleetKindContainer {
		info.Refusal = containerRefusal(w, known)
	}
	return FleetUpdateItem{
		Kind: w.Kind, ID: w.ID, Name: strings.TrimPrefix(w.Name, "/"), Stack: w.Stack, Image: fleetImage(w),
		FromDigest: v.RunningDigest, ToDigest: v.RemoteDigest,
		ChangedPlatforms: append([]string{}, v.ChangedPlatforms...),
		Update:           *info,
		Selectable:       v.State == updates.StateUpdateAvailable && info.Refusal == "",
	}
}

// fleetUpdates puts the answer together from the results that were read.
func fleetUpdates(results []fleetResult[fleetUpdatesEnv], show string) FleetUpdatesBody {
	b := FleetUpdatesBody{Groups: []FleetUpdateGroup{}}
	var lastChecked time.Time
	var firstCheck *time.Time
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		b.Coverage.Environments++
		cov := r.Val.Coverage
		b.Coverage.Checkable += cov.Checkable
		b.Coverage.Checked += cov.Checked
		b.Coverage.Failed += cov.Failed
		b.Coverage.NeverChecked += cov.NeverChecked
		if cov.LastCheckedAt.After(lastChecked) {
			lastChecked = cov.LastCheckedAt
		}
		if cov.NeverChecked > 0 && cov.NextCheckAt != nil && (firstCheck == nil || cov.NextCheckAt.Before(*firstCheck)) {
			at := *cov.NextCheckAt
			firstCheck = &at
		}
		g := FleetUpdateGroup{
			EnvID: r.Env.ID, EnvName: r.Env.Name, Type: "standalone", LastCheckedAt: rfc3339(cov.LastCheckedAt),
			Items: []FleetUpdateItem{},
		}
		if r.Val.Swarm {
			g.Type = "swarm"
		}
		for _, wv := range r.Val.Verdicts {
			if wv.Verdict.State == updates.StateCheckFailed && g.CheckFailed == nil {
				reason := string(wv.Verdict.Reason)
				if reason == "" {
					reason = string(updates.ReasonOther)
				}
				g.CheckFailed = &reason
			}
			which := fleetShowOf(wv.Verdict.State)
			if which == "" {
				continue
			}
			switch which {
			case fleetShowUpdates:
				b.Counts.Updates++
			case fleetShowProblems:
				b.Counts.Problems++
			case fleetShowIgnored:
				b.Counts.Ignored++
				b.Coverage.Skipped++
			}
			b.Counts.All++
			if show == fleetShowAll || show == which {
				g.Items = append(g.Items, fleetUpdateItem(wv, r.Val.Known))
			}
		}
		if len(g.Items) == 0 {
			continue
		}
		slices.SortFunc(g.Items, func(a, b FleetUpdateItem) int {
			return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Kind, b.Kind), strings.Compare(a.ID, b.ID))
		})
		b.Groups = append(b.Groups, g)
	}
	b.Coverage.LastCheckedAt = rfc3339(lastChecked)
	if firstCheck != nil {
		b.Coverage.FirstCheckAt = rfc3339(*firstCheck)
	}
	slices.SortFunc(b.Groups, func(a, b FleetUpdateGroup) int {
		// A failed check first, then by name; the ID keeps two environments of one name in one order.
		return cmp.Or(
			cmp.Compare(boolRank(a.CheckFailed == nil), boolRank(b.CheckFailed == nil)),
			strings.Compare(a.EnvName, b.EnvName), strings.Compare(a.EnvID, b.EnvID),
		)
	})
	return b
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// registerFleetUpdates registers the fleet's update routes on r.
func registerFleetUpdates(r *httpx.Router, d FleetDeps, fail func(what string, err error) error) {
	registerFleetCheck(r, d, fail)
	httpx.Register(r, huma.Operation{
		OperationID: "listFleetUpdates",
		Method:      http.MethodGet,
		Path:        "/api/fleet/updates",
		Summary:     "List the pending image updates across the environments the caller may read",
		Description: "Every workload with an update available, a failed check or update, or a skipped or ignored update, in the environments the caller holds environments.read on, " +
			"grouped by environment (those with a failed check first, then by name), each with its kind, name, stack, image, old and new digest, changed platforms, the update object the lists carry and whether it can be updated now. " +
			"show picks updates (the default), problems, ignored or all; counts gives how many each lists, and coverage what the checks have covered, so an empty list can be worded honestly. " +
			"An environment that could not be read is named in notIncluded; one the caller may not read is neither listed nor named. At most 8 environments are read at once, each for at most 5 seconds.",
		Tags: []string{"fleet"},
	}, rbac.SelfService, func(ctx context.Context, in *FleetUpdatesInput) (*FleetUpdatesOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Updates == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, "", "Update checks are not available on this server.")
		}
		results, err := fleetFanOut(ctx, d, p, in.Env, func(ctx context.Context, env envs.Environment) (fleetUpdatesEnv, int, error) {
			return readFleetUpdates(ctx, d, env)
		})
		if err != nil {
			return nil, fail("read the fleet's updates", err)
		}
		body := fleetUpdates(results, cmp.Or(in.Show, fleetShowUpdates))
		body.NotIncluded, body.ReadAt = fleetNotIncluded(results), rfc3339(d.now())
		return &FleetUpdatesOutput{CacheControl: "no-store", Body: body}, nil
	})
}

// FleetCheckSkipped is an environment Check now did not check.
type FleetCheckSkipped struct {
	EnvID   string `json:"envId" doc:"The environment's ID"`
	EnvName string `json:"envName" doc:"The environment's name"`
	Reason  string `json:"reason" enum:"no_permission,offline" doc:"no_permission: the caller does not hold updates.check on it. offline: it could not be read, so there was nothing to check"`
}

// FleetCheckRequest is the body of POST /api/fleet/updates/check.
type FleetCheckRequest struct {
	EnvIDs []string `json:"envIds,omitempty" maxItems:"100" doc:"The environments to check. Empty or left out means every environment the caller may read. An ID the caller may not read is ignored, as if it were not given"`
}

// FleetCheckInput is the request of POST /api/fleet/updates/check. The body is optional.
type FleetCheckInput struct {
	Body *FleetCheckRequest
}

// FleetCheckBody is the answer of POST /api/fleet/updates/check.
type FleetCheckBody struct {
	Checked int                 `json:"checked" minimum:"0" doc:"How many distinct images (registry, repository and tag) were asked about"`
	Updates int                 `json:"updates" minimum:"0" doc:"How many workloads of the environments that were checked have an update available now, judged platform by platform"`
	Failed  int                 `json:"failed" minimum:"0" doc:"How many images could not be checked; the Updates tab says why for each"`
	Skipped []FleetCheckSkipped `json:"skipped" doc:"The environments that were not checked, and why, by name"`
}

// FleetCheckOutput wraps FleetCheckBody for huma.
type FleetCheckOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         FleetCheckBody
}

// canCheckUpdates reports whether the grants listed for each environment include checking its images.
func canCheckUpdates(perEnv map[string][]rbac.Perm, id string) bool {
	return slices.Contains(perEnv[id], rbac.PermUpdatesCheck) || slices.Contains(perEnv[rbac.AllEnvironments], rbac.PermUpdatesCheck)
}

// registerFleetCheck registers the fleet-wide Check now on r.
func registerFleetCheck(r *httpx.Router, d FleetDeps, fail func(what string, err error) error) {
	limiter := &intervalLimiter{every: fleetCheckEvery}
	httpx.Register(r, huma.Operation{
		OperationID: "checkFleetUpdates",
		Method:      http.MethodPost,
		Path:        "/api/fleet/updates/check",
		Summary:     "Check the images of the environments given, or of all the caller may read, for updates now",
		Description: "Asks the registries what each image tag of the chosen environments points at now, one request for each distinct image across all of them, and stores the answers. " +
			"Only environments the caller holds updates.check on are checked; the others, and any that cannot be reached, are skipped and named in skipped. " +
			"With no environments given, every environment the caller may read is considered. Answers within 60 seconds; a check that takes longer is 504 check_timed_out, and what it learned stays stored. " +
			"One request per 30 seconds per caller: another is 429 " + problemTooManyRequests + ".",
		Tags: []string{"fleet"},
	}, rbac.SelfService, func(ctx context.Context, in *FleetCheckInput) (*FleetCheckOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Updates == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, "", "Update checks are not available on this server.")
		}
		if !limiter.allow(p.ID, d.now()) {
			return nil, httpx.Problem(http.StatusTooManyRequests, problemTooManyRequests, "Too many checks. Wait 30 seconds and try again.")
		}
		var named []string
		if in.Body != nil {
			named = in.Body.EnvIDs
		}
		list, perEnv, err := fleetReadableWith(ctx, d, p, named)
		if err != nil {
			return nil, fail("read the fleet", err)
		}
		body := FleetCheckBody{Skipped: []FleetCheckSkipped{}}
		var permitted []envs.Environment
		for _, e := range list {
			if canCheckUpdates(perEnv, e.ID) {
				permitted = append(permitted, e)
			} else {
				body.Skipped = append(body.Skipped, FleetCheckSkipped{EnvID: e.ID, EnvName: e.Name, Reason: fleetSkipNoPermission})
			}
		}
		read := fleetReadEach(ctx, d, permitted, func(ctx context.Context, env envs.Environment) ([]updates.Workload, int, error) {
			ws, err := d.Updates.Workloads(ctx, env.ID)
			return ws, 0, err
		})
		workloads := map[string][]updates.Workload{}
		var reachable []envs.Environment
		for _, r := range read {
			if r.Err != nil {
				body.Skipped = append(body.Skipped, FleetCheckSkipped{EnvID: r.Env.ID, EnvName: r.Env.Name, Reason: fleetSkipOffline})
				continue
			}
			workloads[r.Env.ID] = r.Val
			reachable = append(reachable, r.Env)
		}
		slices.SortFunc(body.Skipped, func(a, b FleetCheckSkipped) int {
			return cmp.Or(strings.Compare(a.EnvName, b.EnvName), strings.Compare(a.EnvID, b.EnvID))
		})
		if len(reachable) == 0 {
			return &FleetCheckOutput{CacheControl: "no-store", Body: body}, nil
		}
		cctx, cancel := context.WithTimeout(ctx, checkNowTimeout)
		defer cancel()
		results, err := d.Updates.CheckFleet(cctx, workloads)
		switch {
		case err == nil:
		case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
			return nil, httpx.Problem(http.StatusGatewayTimeout, problemCheckTimedOut, "The check took longer than 60 seconds. What it learned is kept; check again.")
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil, httpx.Problem(http.StatusServiceUnavailable, "", "The check was cut short.")
		default:
			return nil, fail("check the fleet's images", err)
		}
		for _, res := range results {
			body.Checked += res.Checked
			body.Failed += res.Failed
		}
		// CheckResult.Updates counts every workload whose digest differs from its tag's, which a rebuild of a platform nobody
		// runs does too. The count shown is the one the verdicts give.
		for _, t := range fleetReadEach(cctx, d, reachable, func(ctx context.Context, env envs.Environment) (updates.Tally, int, error) {
			t, err := d.Updates.Tally(ctx, env.ID)
			return t, 0, err
		}) {
			if t.Err == nil {
				body.Updates += t.Val.Available
			} else {
				d.Log.Warn("could not judge the images to count their updates", "environment", t.Env.ID, "error", t.Err)
			}
		}
		return &FleetCheckOutput{CacheControl: "no-store", Body: body}, nil
	})
}
