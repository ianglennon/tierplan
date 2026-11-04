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
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// Search across the environments (plan 12-32, ENV-11, D-18). It is a live fan-out with no index of its own: each
// environment is read through the short-lived snapshots and the cluster state the environment pages use, and the
// stacks table. A type is searched in an environment only where the caller holds that type's read permission.

const (
	// searchEnvTimeout is how long the search may take to read one environment.
	searchEnvTimeout = 3 * time.Second
	// searchMinQuery and searchMaxQuery bound the trimmed query, in characters.
	searchMinQuery = 2
	searchMaxQuery = 100
	// searchPageRows is the most rows a page answer holds, and searchPalettePerType the most of each type the palette
	// holds.
	searchPageRows       = 200
	searchPalettePerType = 5

	searchTypeAll       = "all"
	searchTypeStack     = "stack"
	searchTypeService   = "service"
	searchTypeContainer = "container"
	searchTypeImage     = "image"
	searchTypeNetwork   = "network"

	searchLimitPage    = "page"
	searchLimitPalette = "palette"

	// searchBadQuery is the problem type of a query that is too short or too long once trimmed.
	searchBadQuery = "invalid_search_query"
)

// searchTypes are the kinds of thing the search finds, in the order the counts list them.
var searchTypes = []string{searchTypeStack, searchTypeService, searchTypeContainer, searchTypeImage, searchTypeNetwork}

// searchPerm is the permission that lets a caller see each type in an environment.
var searchPerm = map[string]rbac.Perm{
	searchTypeStack:     rbac.PermStacksRead,
	searchTypeService:   rbac.PermServicesRead,
	searchTypeContainer: rbac.PermContainersRead,
	searchTypeImage:     rbac.PermImagesRead,
	searchTypeNetwork:   rbac.PermNetworksRead,
}

// SearchInput is the query of GET /api/search.
type SearchInput struct {
	Q     string   `query:"q" required:"true" minLength:"2" maxLength:"100" doc:"What to look for: a name, an image, a stack, or four or more hexadecimal characters of a container or image ID. Trimmed, then 2 to 100 characters. Case does not matter"`
	Type  string   `query:"type" enum:"all,stack,service,container,image,network" default:"all" doc:"Only this kind of thing; all searches every kind"`
	Env   []string `query:"env,explode" maxItems:"100" doc:"Only these environments. An ID the caller may not read is ignored, as if it were not given. Empty means every environment the caller may read"`
	Limit string   `query:"limit" enum:"page,palette" default:"page" doc:"page answers with at most 200 rows. palette answers with at most 5 rows of each type. The counts are the full totals either way"`
}

// SearchResult is one thing the search found, and what the palette and the results page show of it.
type SearchResult struct {
	Type     string `json:"type" enum:"stack,service,container,image,network" doc:"What it is"`
	EnvID    string `json:"envId" doc:"The environment it is in"`
	EnvName  string `json:"envName" doc:"The name of that environment"`
	ID       string `json:"id" doc:"Its ID. Empty for a stack TierPlan does not manage, which is known by its name"`
	Name     string `json:"name" doc:"Its name. An image: the tag that matched, or its short ID when it has none"`
	Managed  bool   `json:"managed" doc:"A stack: true when TierPlan manages it. False for everything else"`
	Stack    string `json:"stack" doc:"A service or container: the stack or Compose project it belongs to; empty when it has none"`
	Image    string `json:"image" doc:"A service or container: its image as written; empty for the other types"`
	Node     string `json:"node" doc:"A container, image or network: the node it was read from; empty for the other types"`
	NodeID   string `json:"nodeId" doc:"The ID of that node, for the link to the thing; empty when there is no node"`
	Driver   string `json:"driver" doc:"A network: its driver; empty for the other types"`
	Subnet   string `json:"subnet" doc:"A network: its first subnet; empty when it has none or for the other types"`
	Size     int64  `json:"size" minimum:"0" doc:"An image: its size in bytes; 0 for the other types"`
	Services int    `json:"services" minimum:"0" doc:"A stack: how many services (swarm) or containers (standalone host) it has; 0 for the other types"`
}

// SearchCounts is how many things matched, by type, before any cap.
type SearchCounts struct {
	Stack     int `json:"stack" minimum:"0"`
	Service   int `json:"service" minimum:"0"`
	Container int `json:"container" minimum:"0"`
	Image     int `json:"image" minimum:"0"`
	Network   int `json:"network" minimum:"0"`
	All       int `json:"all" minimum:"0" doc:"The sum of the others"`
}

// SearchBody is the answer of GET /api/search.
type SearchBody struct {
	Results     []SearchResult         `json:"results" doc:"The best matches first: the name is the query, the name starts with it, the name holds it, then an image or stack that holds it. Equal matches go by environment name, then name"`
	Counts      SearchCounts           `json:"counts" doc:"How many things matched, by type. A type that was not searched, because type named another, counts 0"`
	Truncated   bool                   `json:"truncated" doc:"True when there are more matches than results holds"`
	NotSearched []FleetNotIncludedBody `json:"notSearched" doc:"The environments the caller may read that could not be searched, or only in part. Environments the caller may not read are never named"`
	SearchedAt  string                 `json:"searchedAt" format:"date-time" doc:"When the search was made"`
}

// SearchOutput wraps SearchBody for huma.
type SearchOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SearchBody
}

// searchGrants says which types the caller may search in each environment.
type searchGrants map[string]map[string]bool

// searchGrantsOf is the types of want the caller holds the read permission of, in each environment of perEnv.
func searchGrantsOf(perEnv map[string][]rbac.Perm, list []envs.Environment, want map[string]bool) searchGrants {
	out := make(searchGrants, len(list))
	for _, e := range list {
		for typ := range want {
			perm := searchPerm[typ]
			if slices.Contains(perEnv[e.ID], perm) || slices.Contains(perEnv[rbac.AllEnvironments], perm) {
				if out[e.ID] == nil {
					out[e.ID] = map[string]bool{}
				}
				out[e.ID][typ] = true
			}
		}
	}
	return out
}

// searchEnv reads one environment for the search and gives every thing in it that matches q, of the types in
// may. q is trimmed and lowercased already. An error means the environment could not be searched, and none of it is used.
func searchEnv(ctx context.Context, d FleetDeps, env envs.Environment, q string, may map[string]bool) ([]searchHit, int, error) {
	agg, _, err := d.Aggregator.Snapshot(ctx, env.ID)
	if err != nil {
		return nil, 0, err
	}
	var hits []searchHit
	add := func(rank int, r SearchResult) {
		r.EnvID, r.EnvName = env.ID, env.Name
		hits = append(hits, searchHit{SearchResult: r, rank: rank})
	}

	if may[searchTypeStack] || may[searchTypeService] {
		swarm, err := d.Clusters.IsSwarm(ctx, env.ID)
		if err != nil {
			return nil, 0, err
		}
		var held stackops.Held
		if swarm {
			st, err := d.Clusters.State(ctx, env.ID)
			if err != nil {
				return nil, 0, err
			}
			if !st.Known() {
				// No quorum, or no read yet: which services and stacks there are is not known.
				return nil, 0, errors.New("the cluster could not be read")
			}
			held.Services = st.Read.Services
		} else {
			held.Containers = make([]dockerx.ContainerRow, len(agg.Containers))
			for i, c := range agg.Containers {
				held.Containers[i] = c.ContainerRow
			}
		}
		if may[searchTypeService] {
			for _, s := range held.Services {
				if rank, ok := matchRank(q, s.Name, s.Image, s.Stack); ok {
					add(rank, SearchResult{Type: searchTypeService, ID: s.ID, Name: s.Name, Stack: s.Stack, Image: s.Image})
				}
			}
		}
		if may[searchTypeStack] {
			target := stacks.TargetStandalone
			if swarm {
				target = stacks.TargetSwarm
			}
			var managed []stackops.Stack
			if d.Store != nil {
				if managed, err = (stackops.Repo{Store: d.Store}).Stacks(ctx, env.ID); err != nil {
					return nil, 0, err
				}
			}
			for _, s := range managed {
				if rank, ok := matchRank(q, s.Name); ok {
					add(rank, SearchResult{Type: searchTypeStack, ID: s.ID, Name: s.Name, Managed: true, Services: len(held.RowsOf(s))})
				}
			}
			for _, u := range stackops.Discover(held, managed, target, stackops.SelfMarks{}) {
				if rank, ok := matchRank(q, u.Name); ok {
					add(rank, SearchResult{Type: searchTypeStack, Name: u.Name, Services: u.Total})
				}
			}
		}
	}

	if may[searchTypeContainer] {
		for _, c := range agg.Containers {
			name := strings.TrimPrefix(c.Name, "/")
			stack := cmp.Or(c.Labels[stacks.LabelComposeProject], c.Labels[stacks.LabelSwarmNamespace])
			rank, ok := matchRank(q, name, c.Image, stack)
			if idPrefixMatch(q, c.ID) && (!ok || rank > rankPrefix) {
				rank, ok = rankPrefix, true
			}
			if ok {
				add(rank, SearchResult{Type: searchTypeContainer, ID: c.ID, Name: name, Stack: stack, Image: c.Image, Node: c.Node, NodeID: c.NodeID})
			}
		}
	}

	if may[searchTypeImage] {
		seen := map[string]bool{}
		for _, im := range agg.Images {
			if seen[im.ID] {
				continue
			}
			if rank, name, ok := matchImage(q, im); ok {
				seen[im.ID] = true
				add(rank, SearchResult{Type: searchTypeImage, ID: im.ID, Name: name, Size: im.Size, Node: im.Node, NodeID: im.NodeID})
			}
		}
	}

	if may[searchTypeNetwork] {
		seen := map[string]bool{}
		for _, n := range agg.Networks {
			if seen[n.ID] {
				continue
			}
			if rank, ok := matchRank(q, n.Name); ok {
				seen[n.ID] = true
				r := SearchResult{Type: searchTypeNetwork, ID: n.ID, Name: n.Name, Driver: n.Driver, Node: n.Node, NodeID: n.NodeID}
				if len(n.Subnets) > 0 {
					r.Subnet = n.Subnets[0]
				}
				add(rank, r)
			}
		}
	}
	return hits, len(agg.NotReporting), nil
}

// matchImage says whether the image matches q, how well, and by which name: the tag that matched best, or the
// short ID of an image with no tag. The repository of a tag counts as the name, so "nginx" is an exact match
// for nginx:1.27. The digests are the image references that count as related.
func matchImage(q string, im envs.ImageRow) (rank int, name string, ok bool) {
	names := im.RepoTags
	if len(names) == 0 {
		names = []string{shortImageID(im.ID)}
	}
	rank = rankRelated + 1
	for _, tag := range names {
		r, found := matchRank(q, tag, im.RepoDigests...)
		if repo, _, cut := strings.Cut(tag, ":"); cut && strings.EqualFold(repo, q) {
			r, found = rankExact, true
		}
		if found && r < rank {
			rank, name, ok = r, tag, true
		}
	}
	if idPrefixMatch(q, im.ID) && (!ok || rank > rankPrefix) {
		rank, ok = rankPrefix, true
		if name == "" {
			name = names[0]
		}
	}
	return rank, name, ok
}

// searchAnswer puts the hits of every environment that was searched in order, caps them, and counts them.
func searchAnswer(hits []searchHit, limit string) ([]SearchResult, SearchCounts, bool) {
	rankResults(hits)
	var counts SearchCounts
	for _, h := range hits {
		switch h.Type {
		case searchTypeStack:
			counts.Stack++
		case searchTypeService:
			counts.Service++
		case searchTypeContainer:
			counts.Container++
		case searchTypeImage:
			counts.Image++
		case searchTypeNetwork:
			counts.Network++
		}
	}
	counts.All = counts.Stack + counts.Service + counts.Container + counts.Image + counts.Network

	out := make([]SearchResult, 0, min(len(hits), searchPageRows))
	perType := map[string]int{}
	for _, h := range hits {
		if limit == searchLimitPalette {
			if perType[h.Type] >= searchPalettePerType {
				continue
			}
			perType[h.Type]++
		} else if len(out) >= searchPageRows {
			break
		}
		out = append(out, h.SearchResult)
	}
	return out, counts, len(out) < len(hits)
}

// registerSearch registers GET /api/search.
func registerSearch(r *httpx.Router, d FleetDeps, fail func(string, error) error) {
	httpx.Register(r, huma.Operation{
		OperationID: "searchFleet",
		Method:      http.MethodGet,
		Path:        "/api/search",
		Summary:     "Search stacks, services, containers, images and networks across the environments the caller may read",
		Description: "Finds what matches q by name, by image or stack, and for containers and images by four or more hexadecimal characters of the ID, ignoring case. " +
			"A type is searched in an environment only where the caller holds that type's read permission (stacks.read, services.read, containers.read, images.read, networks.read); " +
			"nothing else is returned, counted or named, and an environment the caller may not read is never named. Environments that could not be searched, or only in part, are named in notSearched with their reason. " +
			"The search is live: it reads the same short-lived snapshots as the environment pages, at most 8 environments at once and each for at most 3 seconds.",
		Tags: []string{"fleet"},
	}, rbac.SelfService, func(ctx context.Context, in *SearchInput) (*SearchOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		q := strings.TrimSpace(in.Q)
		if n := utf8.RuneCountInString(q); n < searchMinQuery || n > searchMaxQuery {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, searchBadQuery, "Search for 2 to 100 characters.")
		}
		q = strings.ToLower(q)
		want := map[string]bool{}
		for _, typ := range searchTypes {
			if in.Type == "" || in.Type == searchTypeAll || in.Type == typ {
				want[typ] = true
			}
		}

		readable, perEnv, err := fleetReadableWith(ctx, d, p, in.Env)
		if err != nil {
			return nil, fail("read the fleet", err)
		}
		// An environment where the caller may search none of the types asked for is not read, and is not named.
		grants := searchGrantsOf(perEnv, readable, want)
		list := make([]envs.Environment, 0, len(readable))
		for _, e := range readable {
			if len(grants[e.ID]) > 0 {
				list = append(list, e)
			}
		}
		results := fleetReadEachWithin(ctx, d, list, d.searchTimeout(), func(ctx context.Context, env envs.Environment) ([]searchHit, int, error) {
			return searchEnv(ctx, d, env, q, grants[env.ID])
		})
		var hits []searchHit
		for _, res := range results {
			if res.Err == nil {
				hits = append(hits, res.Val...)
			}
		}
		rows, counts, truncated := searchAnswer(hits, in.Limit)
		return &SearchOutput{CacheControl: "no-store", Body: SearchBody{
			Results: rows, Counts: counts, Truncated: truncated, NotSearched: fleetNotIncluded(results), SearchedAt: rfc3339(d.now()),
		}}, nil
	})
}
