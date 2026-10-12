// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
)

// problemImageNotFound is the problem type of an image page whose image no reporting node has.
const problemImageNotFound = "image_not_found"

// ImagesDeps are the dependencies of the image detail routes. Image reads are not audited
// (D-31), so Audit is for the routes later plans add.
type ImagesDeps struct {
	// Aggregator reaches an engine: every read goes through OnNode, which never drops the
	// cached snapshot.
	Aggregator *envs.Aggregator
	// Clusters reads the services of a swarm live, for the routes that ask what uses an image.
	Clusters *envs.Clusters
	Audit    *audit.Writer
	// Checker decides whether a caller may see the values in an image's configuration.
	Checker *rbac.Checker
	// SelfContainerID marks Bayplan's own container, for the routes that act on images.
	SelfContainerID string
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// ImageDetailInput is the path and query of the image detail, inspect and history routes.
type ImageDetailInput struct {
	EnvironmentPathInput
	ImageID string `path:"imageId" pattern:"^sha256:[a-f0-9]{64}$" maxLength:"71" doc:"The image ID: sha256: and 64 hexadecimal characters"`
	Node    string `query:"node" maxLength:"128" doc:"Read only this node: its nodeId, as the nodes of the detail answer name it. Left out, every reporting node that has the image is read (inspect and history read the first of them)"`
}

// ImageNameBody is one name of an image.
type ImageNameBody struct {
	Name string `json:"name" doc:"repository:tag for a tag, repository@sha256:... for a digest"`
	Kind string `json:"kind" enum:"tag,digest" doc:"tag for a repository:tag name, digest for a repo@sha256 name"`
}

// ImageNodeBody says whether one node holds the image and under which names.
type ImageNodeBody struct {
	NodeID  string   `json:"nodeId" doc:"The ID of the node"`
	Name    string   `json:"name" doc:"The display name of the node"`
	Present bool     `json:"present" doc:"True when the node has the image"`
	Names   []string `json:"names" doc:"The tags and digests the image has on this node; empty when it is not there or has none"`
}

// ImageDetailBody is one image as its detail page shows it.
type ImageDetailBody struct {
	ID           string            `json:"id" doc:"The full image ID"`
	ShortID      string            `json:"shortId" doc:"The first 12 characters of the ID, without the sha256: prefix"`
	Names        []ImageNameBody   `json:"names" doc:"Every tag, then every digest, the image has on the nodes read, each once, by name"`
	Dangling     bool              `json:"dangling" doc:"True when the image has no tag on any node read"`
	Size         int64             `json:"size" doc:"Size in bytes"`
	Created      string            `json:"created,omitempty" format:"date-time"`
	OS           string            `json:"os" doc:"The operating system the image is built for"`
	Architecture string            `json:"architecture" doc:"The CPU architecture the image is built for"`
	Variant      string            `json:"variant" doc:"The architecture variant, for example v8; empty when there is none"`
	Labels       map[string]string `json:"labels" doc:"Every label of the image"`
	Nodes        []ImageNodeBody   `json:"nodes" doc:"One entry per node read, by display name: whether it has the image and its names there"`
	NotReporting []string          `json:"notReporting" doc:"Nodes that should have answered and did not, so names and nodes may be incomplete"`
}

// ImageDetailOutput wraps ImageDetailBody for huma.
type ImageDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ImageDetailBody
}

// ImageInspectBody is the raw inspect document of one image.
type ImageInspectBody struct {
	Masked  bool           `json:"masked" doc:"True when a value was replaced by the placeholder <hidden> because the caller may not reveal secrets on this environment"`
	Inspect map[string]any `json:"inspect" doc:"The engine's inspect document, with the values of the image configuration's environment (Config.Env, and ContainerConfig.Env of an older engine) replaced by <hidden> unless the caller holds secrets.reveal"`
}

// ImageInspectOutput wraps ImageInspectBody for huma.
type ImageInspectOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ImageInspectBody
}

// ImageLayerBody is one layer of an image's history.
type ImageLayerBody struct {
	ID        string `json:"id" doc:"The layer's ID as the engine gives it: sha256: and 64 hexadecimal characters, <missing> for a layer of a pulled image, or empty"`
	CreatedBy string `json:"createdBy" doc:"The command that made the layer, as the engine reports it. A build can have a secret in one; it is not masked"`
	Size      int64  `json:"size" doc:"The layer's size in bytes; 0 for a layer that only changes metadata"`
	Created   string `json:"created,omitempty" format:"date-time"`
	Comment   string `json:"comment" doc:"The layer's comment, usually empty"`
}

// ImageHistoryBody is the layers of one image.
type ImageHistoryBody struct {
	Layers []ImageLayerBody `json:"layers" doc:"The layers in the order the engine answers: the newest first"`
}

// ImageHistoryOutput wraps ImageHistoryBody for huma.
type ImageHistoryOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ImageHistoryBody
}

// RegisterImages registers the image detail routes. They are reads: nothing here is audited
// (D-31).
func RegisterImages(r *httpx.Router, d ImagesDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	httpx.Register(r, huma.Operation{
		OperationID: "getImage", Method: http.MethodGet, Path: "/api/env/{envId}/images/{imageId}",
		Summary: "One image: its names, details, labels and the nodes that have it",
		Description: "Everything the image detail page shows, read live from the engines. The image is named by its ID (sha256: and 64 hexadecimal characters; anything else is 422). " +
			"Every reporting node whose image list has the ID is asked, and the names (tags, then digests) are merged; node limits the read to one node by its nodeId. " +
			"nodes says, for every node read, whether it has the image and under which names. " +
			"404 image_not_found when no reporting node has the image (it may have been removed, or its tag moved); 503 node_unavailable when node is not a connected node of the environment. " +
			"Needs images.read (Viewer) on the environment. Reads are not audited.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermImagesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ImageDetailInput) (*ImageDetailOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getImage(ctx, d, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ImageDetailOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "inspectImage", Method: http.MethodGet, Path: "/api/env/{envId}/images/{imageId}/inspect",
		Summary: "One image's raw inspect document",
		Description: "The engine's inspect document of the image as it is, for the Inspect tab, read from the first node that has the image or from the node named. " +
			"The image is named as the detail route names it, with the same rules and problems (422 for an ID of the wrong shape, 404 image_not_found, 503 node_unavailable). " +
			"The environment variables of the image's configuration can hold a secret a build baked in: for a caller without secrets.reveal on the environment each value is replaced by the placeholder <hidden> on the server, and masked says something was. " +
			"Needs images.read (Viewer) on the environment. Reads are not audited.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermImagesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ImageDetailInput) (*ImageInspectOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			_, reads, _, err := readImage(ctx, d, env.ID, in.ImageID, in.Node, false)
			if err != nil {
				return nil, err
			}
			shown, masked, err := redact.MaskImageInspect(reads[0].raw, redact.CanReveal(ctx, d.Checker, p, env.ID))
			if err != nil {
				d.Log.Error("masking an image inspect failed", "environment", env.ID, "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			doc, err := decodeInspect(shown)
			if err != nil {
				d.Log.Error("reading an image inspect failed", "environment", env.ID, "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			return &ImageInspectOutput{CacheControl: "no-store", Body: ImageInspectBody{Masked: masked, Inspect: doc}}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "imageHistory", Method: http.MethodGet, Path: "/api/env/{envId}/images/{imageId}/history",
		Summary: "The layers of one image",
		Description: "The image's history from the engine, newest layer first, for the Layers tab, read from the first node that has the image or from the node named. " +
			"The image is named as the detail route names it, with the same rules and problems (422, 404 image_not_found, 503 node_unavailable). " +
			"502 agent_policy_denied when the node's agent is older than this server and does not know the history route. " +
			"The commands are shown as the engine reports them. Needs images.read (Viewer) on the environment. Reads are not audited.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermImagesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ImageDetailInput) (*ImageHistoryOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := imageLayers(ctx, d, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ImageHistoryOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// imageLayers reads the history of the image from the first node that has it, or the node named.
func imageLayers(ctx context.Context, d ImagesDeps, envID string, in *ImageDetailInput) (ImageHistoryBody, error) {
	_, holders, err := imageHolders(ctx, d, envID, in.ImageID, in.Node)
	if err != nil {
		return ImageHistoryBody{}, err
	}
	var rows []dockerx.HistoryRow
	if _, _, err := askImageNodes(ctx, d, envID, holders, false, func(ctx context.Context, c *dockerx.Client, _ envs.NodeRef) error {
		var e error
		rows, e = c.ImageHistory(ctx, in.ImageID)
		return e
	}); err != nil {
		return ImageHistoryBody{}, err
	}
	body := ImageHistoryBody{Layers: make([]ImageLayerBody, 0, len(rows))}
	for _, r := range rows {
		body.Layers = append(body.Layers, ImageLayerBody{ID: r.ID, CreatedBy: r.CreatedBy, Size: r.Size, Created: rfc3339(r.Created), Comment: r.Comment})
	}
	return body, nil
}

// imageRead is what one node answered for an image.
type imageRead struct {
	node   envs.NodeRef
	detail dockerx.ImageDetail
	raw    []byte
}

// imageHolders finds the nodes to ask about an image. With node set that node alone is asked,
// whether or not the list had the image: the list may be a few seconds old, and a node that is
// not the environment's says so itself. Otherwise every reporting node whose image list has
// the ID, by display name. The errors are already problems.
func imageHolders(ctx context.Context, d ImagesDeps, envID, imageID, node string) (envs.Aggregate, []envs.NodeRef, error) {
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return agg, nil, snapshotProblem(d.Log, envID, err)
	}
	if node != "" {
		ref := envs.NodeRef{ID: node}
		for _, n := range agg.Nodes {
			if n.ID == node {
				ref = n
			}
		}
		return agg, []envs.NodeRef{ref}, nil
	}
	var holders []envs.NodeRef
	seen := map[string]bool{}
	for _, row := range agg.Images {
		if row.ID != imageID || seen[row.NodeID] {
			continue
		}
		seen[row.NodeID] = true
		for _, n := range agg.Nodes {
			if n.ID == row.NodeID {
				holders = append(holders, n)
			}
		}
	}
	slices.SortFunc(holders, func(a, b envs.NodeRef) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return agg, holders, nil
}

// askImageNodes runs ask on the nodes in order, once each, and stops after the first node that
// answers unless every is set. A node that no longer has the image is skipped; one that fails
// is named in failed. When no node answered, err is the problem to send: 503 node_unavailable
// when the node is not connected, the environment's own for any other failure, else 404
// image_not_found.
func askImageNodes(ctx context.Context, d ImagesDeps, envID string, nodes []envs.NodeRef, every bool, ask func(ctx context.Context, c *dockerx.Client, n envs.NodeRef) error) (answered []envs.NodeRef, failed []string, err error) {
	var firstErr error
	for _, n := range nodes {
		readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
		e := d.Aggregator.OnNode(readCtx, envID, n.ID, func(c *dockerx.Client) error { return ask(readCtx, c, n) })
		cancel()
		switch {
		case e == nil:
			answered = append(answered, n)
		case dockerx.IsNotFound(e):
			// Removed since the list was read: this node does not have it.
		default:
			if firstErr == nil {
				firstErr = e
			}
			failed = append(failed, n.Name)
		}
		if len(answered) > 0 && !every {
			break
		}
	}
	if len(answered) > 0 {
		return answered, failed, nil
	}
	switch {
	case firstErr == nil:
		return nil, nil, httpx.Problem(http.StatusNotFound, problemImageNotFound, "No node has this image. It may have been removed, or its tag may have moved to another image.")
	case errors.Is(firstErr, envs.ErrNodeUnavailable), errors.Is(firstErr, envs.ErrNotFound):
		return nil, failed, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to Bayplan.")
	}
	return nil, failed, snapshotProblem(d.Log, envID, firstErr)
}

// readImage reads the image live from every node that has it (or the one named). The reads come
// back in the order of the nodes' display names. The errors are already problems: 404
// image_not_found when no node has it, 503 node_unavailable, or the environment's own.
func readImage(ctx context.Context, d ImagesDeps, envID, imageID, node string, every bool) (agg envs.Aggregate, reads []imageRead, failed []string, err error) {
	agg, holders, err := imageHolders(ctx, d, envID, imageID, node)
	if err != nil {
		return agg, nil, nil, err
	}
	byNode := map[string]imageRead{}
	answered, failed, err := askImageNodes(ctx, d, envID, holders, every, func(ctx context.Context, c *dockerx.Client, n envs.NodeRef) error {
		rd := imageRead{node: n}
		var e error
		rd.detail, rd.raw, e = c.ImageDetail(ctx, imageID)
		if e == nil {
			byNode[n.ID] = rd
		}
		return e
	})
	if err != nil {
		return agg, nil, failed, err
	}
	for _, n := range answered {
		reads = append(reads, byNode[n.ID])
	}
	return agg, reads, failed, nil
}

// getImage builds the detail page's answer from the nodes that have the image.
func getImage(ctx context.Context, d ImagesDeps, envID string, in *ImageDetailInput) (ImageDetailBody, error) {
	agg, reads, failed, err := readImage(ctx, d, envID, in.ImageID, in.Node, true)
	if err != nil {
		return ImageDetailBody{}, err
	}
	first := reads[0].detail
	body := ImageDetailBody{
		ID: first.ID, ShortID: shortImageID(first.ID),
		Names: []ImageNameBody{}, Size: first.Size, Created: rfc3339(first.Created),
		OS: first.OS, Architecture: first.Architecture, Variant: first.Variant,
		Labels: first.Labels, Nodes: []ImageNodeBody{}, NotReporting: mergeNames(agg.NotReporting, failed),
	}
	var tags, digests []string
	have := map[string]imageRead{}
	for _, rd := range reads {
		have[rd.node.ID] = rd
		tags = append(tags, rd.detail.RepoTags...)
		digests = append(digests, rd.detail.RepoDigests...)
	}
	for _, name := range sortedUnique(tags) {
		body.Names = append(body.Names, ImageNameBody{Name: name, Kind: "tag"})
	}
	for _, name := range sortedUnique(digests) {
		body.Names = append(body.Names, ImageNameBody{Name: name, Kind: "digest"})
	}
	body.Dangling = len(tags) == 0
	if in.Node != "" {
		// One node was asked for, and it is the only one the page can say anything about.
		for _, rd := range reads {
			body.Nodes = append(body.Nodes, imageNode(rd.node, &rd))
		}
		return body, nil
	}
	for _, n := range agg.Nodes {
		if rd, ok := have[n.ID]; ok {
			body.Nodes = append(body.Nodes, imageNode(n, &rd))
		} else {
			body.Nodes = append(body.Nodes, imageNode(n, nil))
		}
	}
	slices.SortFunc(body.Nodes, func(a, b ImageNodeBody) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.NodeID, b.NodeID))
	})
	return body, nil
}

// imageNode is the entry of a node in the detail: its names for the image when it has it.
func imageNode(n envs.NodeRef, rd *imageRead) ImageNodeBody {
	out := ImageNodeBody{NodeID: n.ID, Name: n.Name, Names: []string{}}
	if rd == nil {
		return out
	}
	out.Present = true
	out.Names = append(out.Names, sortedUnique(rd.detail.RepoTags)...)
	out.Names = append(out.Names, sortedUnique(rd.detail.RepoDigests)...)
	return out
}

// sortedUnique is s sorted, without repeats.
func sortedUnique(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}

// mergeNames is the names in a and b, each once, sorted; never nil.
func mergeNames(a, b []string) []string {
	out := sortedUnique(append(slices.Clone(a), b...))
	if out == nil {
		return []string{}
	}
	return out
}
