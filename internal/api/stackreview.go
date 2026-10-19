// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// StackReviewRequest is the body of POST .../review.
type StackReviewRequest struct {
	Mode     string   `json:"mode,omitempty" enum:"deploy,update" doc:"deploy (the default) reviews the draft; update reviews a newly looked-up digest for the services named in services, with every other service keeping the image it runs"`
	Services []string `json:"services,omitempty" maxItems:"200" doc:"For mode update: the services to look up again, by the name in the compose file or by the swarm name"`
}

// StackReviewInput is the path and body of the review route.
type StackReviewInput struct {
	StackPathInput
	Body StackReviewRequest
}

// StackReviewCountsBody is how many of a kind a deploy would add, change and remove.
type StackReviewCountsBody struct {
	Added   int `json:"added" doc:"How many a deploy would create"`
	Changed int `json:"changed" doc:"How many it would change"`
	Removed int `json:"removed" doc:"How many it would remove"`
}

// StackReviewSummaryBody is the counts the review opens with.
type StackReviewSummaryBody struct {
	Services     StackReviewCountsBody `json:"services" doc:"Services"`
	Networks     StackReviewCountsBody `json:"networks" doc:"Networks the stack owns"`
	Volumes      StackReviewCountsBody `json:"volumes" doc:"Volumes the stack's deploy would create on a standalone host. Always zero on a swarm: the engine of each node makes a named volume when a task first uses it"`
	Secrets      StackReviewCountsBody `json:"secrets" doc:"Secrets Bayplan makes from the stack's files; a changed file counts as changed"`
	Configs      StackReviewCountsBody `json:"configs" doc:"Configs Bayplan makes from the stack's files; a changed file counts as changed"`
	ImagesPinned int                   `json:"imagesPinned" doc:"Services that keep or carry a digest"`
	ImagesNew    int                   `json:"imagesNew" doc:"Services whose digest the review just looked up"`
}

// StackReviewImageBody is one service's image.
type StackReviewImageBody struct {
	Service string `json:"service" doc:"The service's name in the compose file"`
	Image   string `json:"image" doc:"The image reference as written"`
	Digest  string `json:"digest" doc:"The digest the service will run with (an image ID for a local image); empty when the image could not be looked up"`
	Current string `json:"current" doc:"The digest the service runs with now; empty for a new service"`
	State   string `json:"state" enum:"pinned,new,as_written,local,unreachable,unresolved" doc:"pinned (keeps the running digest), new (just looked up), as_written (the reference carries its digest), local (no registry has it; pinned to the image ID), unreachable (the registry could not be reached; blocks) or unresolved (the registry did not give it; blocks)"`
	Note    string `json:"note" doc:"A short sentence for the row, or empty"`
}

// StackReviewServiceBody is one service a deploy would add, change or remove.
type StackReviewServiceBody struct {
	Name          string         `json:"name" doc:"The swarm service name"`
	Short         string         `json:"short" doc:"The service's name in the compose file"`
	Change        string         `json:"change" enum:"added,changed,removed" doc:"What a deploy would do"`
	Current       map[string]any `json:"current" nullable:"true" doc:"The running service's spec, masked: a value derived from a secret of the stack is <hidden>. Null for an added or removed service"`
	Next          map[string]any `json:"next" nullable:"true" doc:"The spec a deploy would send, masked the same way. Null for a removed service"`
	HiddenChanges []string       `json:"hiddenChanges" doc:"Paths of settings whose value changed to or from one that is hidden, such as TaskTemplate.ContainerSpec.Env[API_KEY]. The values are never sent"`
	Settings      int            `json:"settings" doc:"How many settings differ between current and next, hidden ones included"`
}

// StackReviewObjectBody is a network, secret or config a deploy would add, change or remove.
type StackReviewObjectBody struct {
	Change string `json:"change" enum:"added,changed,removed,external" doc:"What a deploy would do; external is an object the stack only names and Bayplan never touches"`
	Kind   string `json:"kind" enum:"network,volume,secret,config" doc:"What it is. A volume is listed for a standalone host only: a swarm creates its volumes on the node that first uses one"`
	Name   string `json:"name" doc:"Its name in the compose file (the swarm name for an external one)"`
	Detail string `json:"detail" doc:"A sentence for the row, or empty"`
}

// StackDiffLineBody is one line of a hunk.
type StackDiffLineBody struct {
	Kind string `json:"kind" enum:"context,added,removed" doc:"context (in both texts), added (only in the newer) or removed (only in the older)"`
	Old  int    `json:"old" doc:"The line's number in the older text, 0 for an added line"`
	New  int    `json:"new" doc:"The line's number in the newer text, 0 for a removed line"`
	Text string `json:"text" doc:"The line, without its line break. A secret value is <hidden>"`
}

// StackDiffHunkBody is a run of changed lines with 3 unchanged lines on either side.
type StackDiffHunkBody struct {
	OldStart int                 `json:"oldStart" doc:"The number of the hunk's first line in the older text; for a hunk with no older line, the number of the line that would come next"`
	OldLines int                 `json:"oldLines" doc:"How many lines of the older text the hunk covers"`
	NewStart int                 `json:"newStart" doc:"The number of the hunk's first line in the newer text, as oldStart is for the older"`
	NewLines int                 `json:"newLines" doc:"How many lines of the newer text the hunk covers"`
	Lines    []StackDiffLineBody `json:"lines" doc:"The hunk's lines in order"`
}

// StackTextDiffBody is a text compared with an older one, computed by the server.
type StackTextDiffBody struct {
	ChangedLines int                 `json:"changedLines" doc:"How many lines the hunks add and remove"`
	Hunks        []StackDiffHunkBody `json:"hunks" doc:"The changes, in order, each with 3 lines of context. Empty when the texts are the same. Over 20,000 lines, or when it takes more than 4,000 added and removed lines to turn one into the other, there is one hunk that removes the whole older text and adds the whole newer"`
	Lines        []string            `json:"lines" doc:"The lines of the newer text, masked, so that the unchanged lines between hunks can be shown; empty when there are no hunks"`
}

// StackFileDiffBody is the change of one file of the stack, or of its variables.
type StackFileDiffBody struct {
	Path         string              `json:"path" doc:"The file's path in the stack; for the variables, variables"`
	Kind         string              `json:"kind" enum:"file,variables" doc:"file, or variables for the stack's variables shown as NAME=value lines"`
	Hidden       bool                `json:"hidden" doc:"True when the file or a variable is secret and differs: its content is never sent, so the hunks do not show it"`
	ChangedLines int                 `json:"changedLines" doc:"How many lines the hunks add and remove"`
	Hunks        []StackDiffHunkBody `json:"hunks" doc:"The changes, as in the rendered YAML"`
	Lines        []string            `json:"lines" doc:"The lines of the newer text, masked; empty when there are no hunks"`
}

// StackConcurrentBody is a deployment made after the draft was started.
type StackConcurrentBody struct {
	Revision       int64               `json:"revision" doc:"The number of the revision that was deployed"`
	DeployedBy     string              `json:"deployedBy" doc:"Who started that deploy"`
	DeployedAt     string              `json:"deployedAt" doc:"When it was started, RFC 3339 UTC"`
	BaseToDeployed []StackFileDiffBody `json:"baseToDeployed" doc:"What that deployment changed since the version the draft started from, file by file, masked"`
	BaseToDraft    []StackFileDiffBody `json:"baseToDraft" doc:"What the draft changed since the version it started from, file by file, masked"`
}

// StackReviewBody is what a deploy of the saved draft would do.
type StackReviewBody struct {
	Problems     []StackProblemBody       `json:"problems" doc:"What the check found and the review added: host access that needs Admin, a name clash, an image that cannot be looked up, an external object that does not exist. An error stops a deploy"`
	Summary      StackReviewSummaryBody   `json:"summary" doc:"The counts of what would change"`
	Images       []StackReviewImageBody   `json:"images" doc:"One row for each service with an image, by name"`
	Services     []StackReviewServiceBody `json:"services" doc:"The services a deploy would add or change, by name, then the ones it would remove"`
	Objects      []StackReviewObjectBody  `json:"objects" doc:"The other resources: networks, secrets and configs"`
	DraftVersion int64                    `json:"draftVersion" doc:"The version of the draft the review was made of: send it with the deploy"`
	Pins         map[string]string        `json:"pins" doc:"The image each service will be deployed with, by swarm service name: send it with the deploy and the deploy uses exactly these digests"`
	NeedsAdmin   []string                 `json:"needsAdmin" doc:"The kinds of host access the stack uses that the caller may not deploy (bind mounts, extra privileges, host networking, device mounts). Empty when the caller may deploy it"`
	YAML         StackTextDiffBody        `json:"yaml" doc:"The rendered stack of the draft, with the images the review chose, against the rendering stored with the last good deploy, masked, as hunks with 3 lines of context. A stack never deployed is compared with an empty document. This is the one comparison that is with the last deploy and not with what is running"`
	Concurrent   *StackConcurrentBody     `json:"concurrent,omitempty" doc:"Left out unless set: set when the last good revision is newer than the revision the draft started from: someone deployed after the draft began. A deploy of the draft then needs acknowledgeConcurrent"`
}

// StackReviewOutput wraps StackReviewBody for huma.
type StackReviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackReviewBody
}

func registerStackReview(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "reviewStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/review",
		Summary: "Review what a deploy of a stack would do",
		Description: "Reviews the stack's saved draft (the latest good revision when there is no draft) against what is running and saves nothing. " +
			"It runs the same check as the editor, looks up the digests of the images that need one, converts the stack the way a deploy does, reads what the swarm holds under the stack's namespace and answers the problems, a summary, an image table, each service's change with its masked current and next spec, and the other resources. " +
			"A value derived from a secret of the stack is <hidden> on both sides; whether it changed is decided before it is hidden and listed in hiddenChanges as a path. " +
			"Right after a deploy, a review shows no change. yaml is the one comparison with the last deploy and not with what is running: the rendered stack against the masked rendering stored with the last good revision, as hunks computed here with 3 lines of context. " +
			"concurrent is set when someone deployed after the draft was started, with what that deployment changed and what the draft changed, file by file; the deploy then needs acknowledgeConcurrent. Send the answer's draftVersion and pins with the deploy, and the deploy uses exactly these digests. " +
			"For a caller below env Admin, host access (a bind of the Docker socket or of a host path outside /srv, /mnt and /media, an added capability, privileged, the host network, a device), judged on the values of the stack's variables, is a blocking needs_admin problem at its line, and needsAdmin names the kinds. " +
			"A name that something Bayplan does not manage holds is a blocking stack_name_clash problem, and another stack operation running on the environment is a blocking operation_locked problem. " +
			"On a standalone host the review pins the images the same way and compares the config hash of each service, as the Compose SDK computes it from the project a deploy would send, with the hash on each running container: a service with no container is added, one whose hash differs is changed (a changed secret or config is a changed service), and a service of the host's Compose project that the file no longer declares is removed; the changed services carry no field-level difference. An environment that is no longer the kind of target the stack was created for is 409 target_changed. Needs stacks.deploy (Operator).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackReviewInput) (*StackReviewOutput, error) {
			return reviewStack(ctx, d, in)
		})
}

func reviewStack(ctx context.Context, d StacksDeps, in *StackReviewInput) (*StackReviewOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	draft, err := repo.Draft(ctx, st.ID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	ri := stackops.ReviewInput{
		EnvID: env.ID, Stack: st, Copy: draft.Copy, DraftVersion: draft.Version,
		Mode: in.Body.Mode, Services: in.Body.Services, Pins: repo,
		Caller: stackops.Caller{Principal: p, Actor: p.Name},
	}
	if good, ok, err := repo.LatestGoodRevision(ctx, st.ID); err != nil {
		return nil, stackInternal(d, "read the running revision", env.ID, err)
	} else if ok {
		ri.Previous = &good.Copy
		ri.Deployed = &stackops.DeployedRevision{Seq: good.Seq, By: good.CreatedBy, At: good.CreatedAt}
		ri.BaseSeq = draft.BaseSeq
		if ri.Rendering, err = repo.GoodRendering(ctx, st.ID); err != nil {
			return nil, stackInternal(d, "read the last rendering", env.ID, err)
		}
		if good.Seq > draft.BaseSeq && draft.BaseSeq > 0 {
			base, err := repo.Revision(ctx, st.ID, draft.BaseSeq)
			switch {
			case errors.Is(err, stackops.ErrRevisionNotFound):
				// The revision the draft started from is gone: the draft is compared with nothing.
			case err != nil:
				return nil, stackInternal(d, "read the revision the draft started from", env.ID, err)
			default:
				ri.Base = &base.Copy
			}
		}
	}
	rv, err := d.ops().Review(ctx, ri)
	if err != nil {
		return nil, targetProblem(d, env.ID, err)
	}
	// Another operation holds the environment: a deploy now would be refused, so the review says so
	// as a blocking problem that has no place in the files.
	if h, ok := d.Locks.Held(env.ID); ok {
		rv.Problems = append(rv.Problems, stacks.Problem{
			Severity: stacks.SeverityError, Code: problemOperationLocked,
			Message: "Another stack operation is running on " + env.Name + ": " + h.StackName + ", started " + rfc3339(h.Started) + ".",
		})
	}
	return &StackReviewOutput{CacheControl: "no-store", Body: stackReviewBody(rv)}, nil
}

// stackReviewBody turns a review into its answer. Every list is present, empty when there is nothing.
func stackReviewBody(rv stackops.Review) StackReviewBody {
	b := StackReviewBody{
		Problems: make([]StackProblemBody, 0, len(rv.Problems)),
		Summary: StackReviewSummaryBody{
			Services: countsBody(rv.Summary.Services), Networks: countsBody(rv.Summary.Networks), Volumes: countsBody(rv.Summary.Volumes),
			Secrets: countsBody(rv.Summary.Secrets), Configs: countsBody(rv.Summary.Configs),
			ImagesPinned: rv.Summary.ImagesPinned, ImagesNew: rv.Summary.ImagesNew,
		},
		Images:       make([]StackReviewImageBody, 0, len(rv.Images)),
		Services:     make([]StackReviewServiceBody, 0, len(rv.Services)),
		Objects:      make([]StackReviewObjectBody, 0, len(rv.Objects)),
		DraftVersion: rv.DraftVersion, Pins: rv.Pins, NeedsAdmin: rv.NeedsAdmin,
		YAML: textDiffBody(rv.YAML),
	}
	if c := rv.Concurrent; c != nil {
		b.Concurrent = &StackConcurrentBody{
			Revision: c.Revision, DeployedBy: c.DeployedBy, DeployedAt: rfc3339(c.DeployedAt),
			BaseToDeployed: fileDiffBodies(c.BaseToDeployed), BaseToDraft: fileDiffBodies(c.BaseToDraft),
		}
	}
	if b.Pins == nil {
		b.Pins = map[string]string{}
	}
	if b.NeedsAdmin == nil {
		b.NeedsAdmin = []string{}
	}
	for _, p := range rv.Problems {
		b.Problems = append(b.Problems, StackProblemBody{
			Severity: string(p.Severity), Code: p.Code, Message: p.Message, File: p.File,
			Line: p.Line, Col: p.Col, EndLine: p.EndLine, EndCol: p.EndCol,
		})
	}
	for _, r := range rv.Images {
		b.Images = append(b.Images, StackReviewImageBody{Service: r.Service, Image: r.Image, Digest: r.Digest, Current: r.Current, State: r.State, Note: r.Note})
	}
	for _, c := range rv.Services {
		hidden := c.HiddenChanges
		if hidden == nil {
			hidden = []string{}
		}
		b.Services = append(b.Services, StackReviewServiceBody{
			Name: c.Name, Short: c.Short, Change: c.Change, Current: specTree(c.Current), Next: specTree(c.Next),
			HiddenChanges: hidden, Settings: c.Settings,
		})
	}
	for _, o := range rv.Objects {
		b.Objects = append(b.Objects, StackReviewObjectBody{Change: o.Change, Kind: o.Kind, Name: o.Name, Detail: o.Detail})
	}
	return b
}

func countsBody(c stackops.Counts) StackReviewCountsBody {
	return StackReviewCountsBody{Added: c.Added, Changed: c.Changed, Removed: c.Removed}
}

// specTree reads a masked spec back into the tree the answer carries, nil for none.
func specTree(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// diffKinds names the kinds of a diff line as the answer spells them.
var diffKinds = map[byte]string{stacks.DiffContext: "context", stacks.DiffAdded: "added", stacks.DiffRemoved: "removed"}

func hunkBodies(hunks []stacks.Hunk) []StackDiffHunkBody {
	out := make([]StackDiffHunkBody, 0, len(hunks))
	for _, h := range hunks {
		lines := make([]StackDiffLineBody, 0, len(h.Lines))
		for _, l := range h.Lines {
			lines = append(lines, StackDiffLineBody{Kind: diffKinds[l.Kind], Old: l.Old, New: l.New, Text: l.Text})
		}
		out = append(out, StackDiffHunkBody{OldStart: h.OldStart, OldLines: h.OldLines, NewStart: h.NewStart, NewLines: h.NewLines, Lines: lines})
	}
	return out
}

func textDiffBody(td stackops.TextDiff) StackTextDiffBody {
	lines := td.Lines
	if lines == nil {
		lines = []string{}
	}
	return StackTextDiffBody{ChangedLines: td.ChangedLines, Hunks: hunkBodies(td.Hunks), Lines: lines}
}

func fileDiffBodies(files []stackops.FileHunks) []StackFileDiffBody {
	out := make([]StackFileDiffBody, 0, len(files))
	for _, f := range files {
		td := textDiffBody(f.TextDiff)
		kind := "file"
		if f.Variables {
			kind = "variables"
		}
		out = append(out, StackFileDiffBody{Path: f.Path, Kind: kind, Hidden: f.Hidden, ChangedLines: td.ChangedLines, Hunks: td.Hunks, Lines: td.Lines})
	}
	return out
}
