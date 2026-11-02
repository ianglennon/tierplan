// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/gitsync"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// StackReviewRequest is the body of POST .../review.
type StackReviewRequest struct {
	Mode     string   `json:"mode,omitempty" enum:"deploy,update,rollback,restore" doc:"deploy (the default) reviews the draft; update reviews a newly looked-up digest for the services named in services, with every other service keeping the image it runs; rollback reviews restoring the revision named in revision, with the digests it stored, against what is running; restore reviews putting back the stack's deployed revision, so what it lists is what changed outside the stack"`
	Services []string `json:"services,omitempty" maxItems:"200" doc:"For mode update: the services to look up again, by the name in the compose file or by the swarm name"`
	Revision int64    `json:"revision,omitempty" minimum:"1" doc:"For mode rollback: the number of the revision to restore. Required for it (422 revision_required); a revision the stack does not hold is 404 revision_not_found, one that did not end good 409 revision_not_good. Not read for mode restore, which takes the deployed revision itself"`
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
	Services      StackReviewCountsBody `json:"services" doc:"Services"`
	Networks      StackReviewCountsBody `json:"networks" doc:"Networks the stack owns"`
	Volumes       StackReviewCountsBody `json:"volumes" doc:"Volumes the stack's deploy would create on a standalone host. Always zero on a swarm: the engine of each node makes a named volume when a task first uses it"`
	Secrets       StackReviewCountsBody `json:"secrets" doc:"Secrets TierPlan makes from the stack's files; a changed file counts as changed"`
	Configs       StackReviewCountsBody `json:"configs" doc:"Configs TierPlan makes from the stack's files; a changed file counts as changed"`
	ImagesPinned  int                   `json:"imagesPinned" doc:"Services that keep or carry a digest"`
	ImagesNew     int                   `json:"imagesNew" doc:"Services whose digest the review just looked up"`
	OwnershipOnly int                   `json:"ownershipOnly" doc:"Services of an adopted stack's first deploy that differ from what runs in nothing but TierPlan's ownership labels: the deploy recreates each of them (a brief restart) and changes nothing else. They are not counted in services.changed. Zero for any other review"`
	RevertsDrift  int                   `json:"revertsDrift" doc:"How many changes made outside the stack this deploy, update or roll back would put back (revertsDrift in the answer). Zero for a restore, which is the drift itself"`
}

// StackRevertsDriftBody is one change made outside the stack that a deploy would put back.
type StackRevertsDriftBody struct {
	Service string               `json:"service" doc:"The service's name in the compose file"`
	Change  string               `json:"change" enum:"changed,created,removed" doc:"changed: a setting that runs other than the deployed revision has it and that the deploy sets back. created: the deployed revision has the service, it no longer runs and the deploy creates it again. removed: the service runs under the stack and neither the deployed revision nor the deploy declares it, so the deploy removes it"`
	Path    string               `json:"path" doc:"For changed, the setting's path in the service's settings (the keys joined with a dot, an environment entry as Env[NAME]); empty otherwise"`
	Live    string               `json:"live" doc:"For changed, the value that runs now as text, masked as the rest of the review; empty when the setting is not there now"`
	Desired string               `json:"desired" doc:"For changed, the value the deploy sets, masked the same way; empty when the deploy removes the setting"`
	Cause   *StackDriftCauseBody `json:"cause,omitempty" doc:"Why the setting differs from the deployed revision, when the audit log (a scale, roll back or edit from TierPlan since the revision deployed) knows; left out when it does not, which a reader says is a change made outside TierPlan"`
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

// StackReviewServiceBody is one service a deploy would add, change or remove, or, on a standalone host, one
// it recreates or restarts because another service is recreated.
type StackReviewServiceBody struct {
	Name          string         `json:"name" doc:"The swarm service name"`
	Short         string         `json:"short" doc:"The service's name in the compose file"`
	Change        string         `json:"change" enum:"added,changed,removed,recreated,restarted" doc:"What a deploy would do. recreated and restarted are listed for a standalone host only: the service is not changed, and the deploy recreates it because it shares the network, IPC, PID namespace or volumes of the service in because, which is recreated, or stops it and starts it again (keeping its container) because it depends on that service with restart: true. They carry no spec and no settings"`
	Current       map[string]any `json:"current" nullable:"true" doc:"The running service's spec, masked: a value derived from a secret of the stack is <hidden>. Null for an added or removed service"`
	Next          map[string]any `json:"next" nullable:"true" doc:"The spec a deploy would send, masked the same way. Null for a removed service"`
	HiddenChanges []string       `json:"hiddenChanges" doc:"Paths of settings whose value changed to or from one that is hidden, such as TaskTemplate.ContainerSpec.Env[API_KEY]. The values are never sent"`
	Settings      int            `json:"settings" doc:"How many settings differ between current and next, hidden ones included"`
	Because       string         `json:"because,omitempty" doc:"Left out unless set: for a recreated or restarted service, the name of the recreated service that carries it"`
	OwnershipOnly bool           `json:"ownershipOnly,omitempty" doc:"Left out unless true: true when the service differs from what runs in nothing but TierPlan's ownership labels, which the first deploy of an adopted stack adds. Its current and next show only those labels. A service with a real change as well is false"`
}

// StackReviewAdoptionBody is what the first deploy of an adopted stack does that the file does not say.
type StackReviewAdoptionBody struct {
	Pending       bool `json:"pending" doc:"True: the deploy is the stack's first since it was adopted, and adds TierPlan's ownership labels to every service"`
	OwnershipOnly int  `json:"ownershipOnly" doc:"How many services the deploy recreates for the ownership labels alone (they restart briefly)"`
}

// StackReviewObjectBody is a network, secret or config a deploy would add, change or remove.
type StackReviewObjectBody struct {
	Change string `json:"change" enum:"added,changed,removed,external" doc:"What a deploy would do; external is an object the stack only names and TierPlan never touches"`
	Kind   string `json:"kind" enum:"network,volume,secret,config" doc:"What it is. A volume is listed for a standalone host only: a swarm creates its volumes on the node that first uses one"`
	Name   string `json:"name" doc:"Its name in the compose file (the swarm name for an external one)"`
	Detail string `json:"detail" doc:"A sentence for the row, or empty"`
	// The subnet of an added network and whether the environment's small stack subnets chose it (D-20).
	Subnet             string `json:"subnet,omitempty" doc:"For an added network, the subnet it will be created with; absent when the file leaves the choice to the engine and for any other row"`
	SmallSubnetDefault bool   `json:"smallSubnetDefault,omitempty" doc:"True when subnet was chosen by the environment's small stack subnets and not by the file: the row shows the Small-subnet default badge. Absent otherwise"`
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

// StackReviewGitBody is where the draft of a git stack came from, and how far that is from what is deployed.
type StackReviewGitBody struct {
	Commit         string `json:"commit" doc:"The full ID of the commit the draft's repository files are from"`
	Ref            string `json:"ref" doc:"The branch, tag or commit ID the draft follows"`
	RefKind        string `json:"refKind" enum:"branch,tag,commit" doc:"What ref names"`
	Folder         string `json:"folder" doc:"The folder of the repository the stack lives in; empty for the root"`
	RepoName       string `json:"repoName" doc:"The repository's name when the draft was made from it"`
	DeployedCommit string `json:"deployedCommit" doc:"The full ID of the commit the stack's newest good revision was made from; empty when nothing was deployed from a repository"`
	Touched        int    `json:"touched" minimum:"0" doc:"How many commits changed the stack's folder between deployedCommit and commit; 0 when nothing is deployed or the source changed"`
	SourceChanged  bool   `json:"sourceChanged" doc:"True when the draft follows another repository, ref or folder than the one deployed, so the two commits are not comparable"`
	HeadCommit     string `json:"headCommit" doc:"The full ID of the commit the ref pointed at when the stack was last checked or pulled; empty when it never was. When it is not commit, the branch moved on after the draft was made: a deploy still deploys commit"`

	ComposeFiles []string `json:"composeFiles" doc:"The compose files of the draft's source, in merge order"`

	Converts bool `json:"converts,omitempty" doc:"Left out unless true: true when the draft points an inline or adopted stack at a repository (D-24), so a good deploy of it replaces the stack's inline files and makes it a git stack. headCommit is then the draft's own commit"`

	Replaces *StackReviewGitSourceBody `json:"replaces,omitempty" doc:"Left out unless set: set when the draft holds another source than the one the stack follows (the repository, ref, folder or compose files were changed through review, D-11), and names the source a good deploy of the draft replaces. headCommit is then the draft's own commit"`
}

// StackReviewGitSourceBody is a source a git stack follows: where its files come from.
type StackReviewGitSourceBody struct {
	RepoName     string   `json:"repoName" doc:"The repository's name; empty when it could not be read"`
	RefKind      string   `json:"refKind" enum:"branch,tag,commit" doc:"What ref names"`
	Ref          string   `json:"ref" doc:"The branch, tag or commit ID"`
	Folder       string   `json:"folder" doc:"The folder of the repository the stack lives in; empty for the root"`
	ComposeFiles []string `json:"composeFiles" doc:"The compose files, in merge order"`
}

// StackReviewBody is what a deploy of the saved draft would do.
type StackReviewBody struct {
	Problems                []StackProblemBody       `json:"problems" doc:"What the check found and the review added: host access that needs Admin, a name clash, an image that cannot be looked up, an external object that does not exist. An error stops a deploy"`
	Summary                 StackReviewSummaryBody   `json:"summary" doc:"The counts of what would change"`
	Images                  []StackReviewImageBody   `json:"images" doc:"One row for each service with an image, by name"`
	Services                []StackReviewServiceBody `json:"services" doc:"The services a deploy would add or change, by name, then the ones it would remove"`
	Objects                 []StackReviewObjectBody  `json:"objects" doc:"The other resources: networks, secrets and configs"`
	DraftVersion            int64                    `json:"draftVersion" doc:"The version of the draft the review was made of: send it with the deploy"`
	Pins                    map[string]string        `json:"pins" doc:"The image each service will be deployed with, by swarm service name: send it with the deploy and the deploy uses exactly these digests"`
	NeedsAdmin              []string                 `json:"needsAdmin" doc:"The kinds of host access the stack uses that the caller may not deploy (bind mounts, extra privileges, host networking, device mounts). Empty when the caller may deploy it"`
	YAML                    StackTextDiffBody        `json:"yaml" doc:"The rendered stack of the draft, with the images the review chose, against the rendering stored with the last good deploy, masked, as hunks with 3 lines of context. A stack never deployed is compared with an empty document. This is the one comparison that is with the last deploy and not with what is running"`
	Concurrent              *StackConcurrentBody     `json:"concurrent,omitempty" doc:"Left out unless set: set when the last good revision is newer than the revision the draft started from: someone deployed after the draft began. A deploy of the draft then needs acknowledgeConcurrent"`
	Revision                int64                    `json:"revision" doc:"For mode rollback and restore, the number of the revision the review restores (for restore, the deployed one); 0 for any other mode"`
	Adoption                *StackReviewAdoptionBody `json:"adoption,omitempty" doc:"Left out unless set: set while the stack is adopted and has not been deployed yet, and says what taking ownership does"`
	DeployedSeq             int64                    `json:"deployedSeq" doc:"The number of the stack's newest good revision when the review was made, 0 when it has none. A roll back sends it as expectedDeployed, so a deploy made after the review is noticed"`
	RevertsDrift            []StackRevertsDriftBody  `json:"revertsDrift" doc:"The changes made outside the stack that this deploy, update or roll back would put back, apart from the draft's own changes: a setting counts when the deployed revision's value differs from what runs and the draft (or the revision rolled back to) has the deployed revision's value. A service of the revision that no longer runs and that is kept, and a service that runs under the stack that neither declares, are listed too. It never blocks a deploy. Empty for a restore, which is the drift itself, and for a stack with no deployed revision"`
	RevertsDriftUnavailable bool                     `json:"revertsDriftUnavailable" doc:"True when the drift could not be computed, so revertsDrift is empty for want of knowing; the rest of the review is accurate"`
	Git                     *StackReviewGitBody      `json:"git,omitempty" doc:"Left out unless set: set for a review of the draft of a stack made from a git repository, and says which commit the draft holds and how far it is from the deployed one"`
}

// StackReviewOutput wraps StackReviewBody for huma.
type StackReviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackReviewBody
}

// problemRevisionRequired is the code of a roll back review that names no revision.
const problemRevisionRequired = "revision_required"

// problemNoDeployedRevision is the code of a restore of a stack that has no deployed revision to put back.
const problemNoDeployedRevision = "no_deployed_revision"

// noDeployedRevision is the sentence for it.
const noDeployedRevision = "This stack has not been deployed yet, so there's nothing to restore."

func registerStackReview(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "reviewStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/review",
		Summary: "Review what a deploy of a stack would do",
		Description: "Reviews the stack's saved draft (the latest good revision when there is no draft) against what is running and saves nothing. " +
			"It runs the same check as the editor, looks up the digests of the images that need one, converts the stack the way a deploy does, reads what the swarm holds under the stack's namespace and answers the problems, a summary, an image table, each service's change with its masked current and next spec, and the other resources. " +
			"A value derived from a secret of the stack is <hidden> on both sides; whether it changed is decided before it is hidden and listed in hiddenChanges as a path. " +
			"A caller who does not hold secrets.reveal gets, in every mode, every environment value, command, argument, health check command and log option of the running service and of the next spec as <hidden> (the names and paths stay), and the live and desired text of a revertsDrift row at such a path as <hidden>, as the drift route does. " +
			"Right after a deploy, a review shows no change. yaml is the one comparison with the last deploy and not with what is running: the rendered stack against the masked rendering stored with the last good revision, as hunks computed here with 3 lines of context. " +
			"concurrent is set when someone deployed after the draft was started, with what that deployment changed and what the draft changed, file by file; the deploy then needs acknowledgeConcurrent. Send the answer's draftVersion and pins with the deploy, and the deploy uses exactly these digests. " +
			"For a caller below env Admin, host access (a bind of the Docker socket or of a host path outside /srv, /mnt and /media, an added capability, privileged, the host network, a device), judged on the values of the stack's variables, is a blocking needs_admin problem at its line, and needsAdmin names the kinds. " +
			"A name that something TierPlan does not manage holds is a blocking stack_name_clash problem, and another stack operation running on the environment is a blocking operation_locked problem. " +
			"On a standalone host the review pins the images the same way and compares each service with the containers that run it: the service a deploy would send, and the service rebuilt from each running container's inspect document, both with what the image or the engine fills in taken out, so that right after a deploy nothing differs. A service with no container is added; one whose containers carry another config hash than the service has now, or whose running configuration is not what the file says (a container changed by hand on the host, which keeps its labels and its hash), is changed, with the masked current and next settings as compose settings (restart, ports, environment, volumes ...); a changed secret or config is a changed service; and a service of the host's Compose project that the file no longer declares is removed. Settings the engine does not record (depends_on, secrets, configs, profiles, pull_policy) are read from the stored revision that made the container, and are the file's own when none did. A deploy that recreates a service carries others with it, as docker compose does: a service that shares its network, IPC or PID namespace or its volumes is recreated and keeps its pinned image, and one that depends on it with restart: true is stopped and started again; they follow as recreated and restarted rows with because naming the service, carry no spec, and are not counted in the summary. A container changed by hand is put right by a deploy of an unedited file. An environment that is no longer the kind of target the stack was created for is 409 target_changed. " +
			"In mode update, only the services named in services look their tag up again and every other service keeps the digest it runs; each row of images then carries the digest it runs now in current and the one it would run in digest, and a tag that did not move reads pinned with the note Already up to date. A request that names no service, or one the stack does not have, is 422 services_invalid and nothing is looked up. " +
			"In mode rollback, revision names the revision to restore (422 revision_required without it, 404 revision_not_found for one the stack does not hold, 409 revision_not_good for one that did not end good). " +
			"The review is of that revision, not of the draft: it is converted, judged for host access for the caller and compared with the services, networks, secrets and configs that run now, with each image set to the digest the revision stored (state pinned, note revision), and no registry is asked. yaml is the revision's rendering against the deployed revision's, and concurrent is never set. deployedSeq is the newest good revision's number, to send as expectedDeployed. " +
			"On a standalone host the revision is compared with the host's containers by config hash and field diff, as a draft's review is, with each image set to the reference the revision stored; a network or volume a later revision added stays on the host and is not listed. " +
			"In mode restore the revision is the stack's deployed one, which the review takes itself (409 no_deployed_revision for a stack with none), and the review is the same as a roll back to it: what it lists is what changed outside the stack, revision and deployedSeq are both the deployed revision's number, and nothing is looked up. " +
			"A revision that cannot be redeployed as it stands is one blocking revision_not_deployable problem. Needs stacks.deploy (Operator).",
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
		Mode: in.Body.Mode, Services: in.Body.Services, Pins: repo, Subnets: repo, Revisions: repo,
		Caller: stackops.Caller{Principal: p, Actor: p.Name}, Audit: d.Store,
	}
	restore := in.Body.Mode == stackops.ReviewModeRestore
	rollback := in.Body.Mode == stackops.ReviewModeRollback || restore
	good, haveGood, err := repo.LatestGoodRevision(ctx, st.ID)
	if err != nil {
		return nil, stackInternal(d, "read the running revision", env.ID, err)
	}
	if rollback {
		// A roll back reviews the revision the person chose, not the draft: it is read by (stack, seq) after the
		// stack by (environment, ID), and only a good one qualifies (D-06, T-08-10, T-08-13). A restore reviews
		// the deployed revision, which the stack itself names (D-23).
		var target stackops.Snapshot
		if restore {
			if !haveGood {
				return nil, httpx.Problem(http.StatusConflict, problemNoDeployedRevision, noDeployedRevision)
			}
			target = good
		} else {
			if in.Body.Revision == 0 {
				return nil, httpx.Problem(http.StatusUnprocessableEntity, problemRevisionRequired, "Say which revision to roll back to.")
			}
			if target, err = restorableRevision(ctx, d, repo, env.ID, st.ID, in.Body.Revision); err != nil {
				return nil, err
			}
		}
		if ri.TargetPins, err = repo.RevisionPins(ctx, target.ID); err != nil {
			return nil, stackInternal(d, "read the pins of the revision to restore", env.ID, err)
		}
		if ri.TargetRendering, err = repo.RevisionRendering(ctx, target.ID); err != nil {
			return nil, stackInternal(d, "read the rendering of the revision to restore", env.ID, err)
		}
		ri.Target, ri.Copy = &target, target.Copy
	}
	if haveGood {
		ri.Previous = &good.Copy
		ri.Deployed = &stackops.DeployedRevision{Seq: good.Seq, By: good.CreatedBy, At: good.CreatedAt}
		ri.BaseSeq = draft.BaseSeq
		if ri.Rendering, err = repo.GoodRendering(ctx, st.ID); err != nil {
			return nil, stackInternal(d, "read the last rendering", env.ID, err)
		}
		if !rollback && good.Seq > draft.BaseSeq && draft.BaseSeq > 0 {
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
	// The running service's settings, and what was changed outside the stack, are withheld from a caller who may not
	// reveal secrets, in every mode, as the drift and the History changes routes withhold them (D-47). Only the
	// answer is masked: the draft version and the pins the deploy takes from a review are not derived from it.
	if !redact.CanReveal(ctx, d.Checker, p, env.ID) {
		rv.Services = stackops.RedactChanges(rv.Services, st.Target)
		rv.RevertsDrift = stackops.RedactReverts(rv.RevertsDrift, st.Target)
	}
	body := stackReviewBody(rv)
	if !rollback && in.Body.Mode != stackops.ReviewModeUpdate {
		if body.Git, err = reviewGitBody(ctx, d, repo, draft); err != nil {
			return nil, stackInternal(d, "read the deployed commit", env.ID, err)
		}
	}
	if ri.Deployed != nil {
		body.DeployedSeq = ri.Deployed.Seq
	}
	if ri.Target != nil {
		body.Revision = ri.Target.Seq
	}
	return &StackReviewOutput{CacheControl: "no-store", Body: body}, nil
}

// reviewGitBody says which commit the draft of a git stack holds and how far it is from the deployed one; nil for a draft
// that is not from a repository. The commits between the two are counted from the server's mirror, which a pull has just
// filled; a mirror that can't be read counts none.
func reviewGitBody(ctx context.Context, d StacksDeps, repo stackops.Repo, draft stackops.Snapshot) (*StackReviewGitBody, error) {
	if draft.Git.Commit == "" {
		return nil, nil
	}
	src := draft.Git.Source
	out := &StackReviewGitBody{
		Commit: draft.Git.Commit, Ref: src.Ref, RefKind: src.RefKind, Folder: src.Folder, RepoName: src.RepoName,
		ComposeFiles: composeFilesOf(src.ComposeFiles),
	}
	// A stack that follows nothing any more has no head to compare with.
	if state, err := repo.GitState(ctx, draft.StackID); err == nil {
		out.HeadCommit = state.HeadCommit
	} else if !errors.Is(err, stackops.ErrNoGit) {
		return nil, err
	}
	// A draft that holds another source than the stack follows says which source it replaces (D-11). The head the sync
	// state holds belongs to that source, so the draft's own commit stands in for it: it was read just now.
	if sg, err := repo.StackGit(ctx, draft.StackID); err == nil {
		if gitsync.PendingSource(sg, draft) {
			out.Replaces = replacedSource(ctx, d, sg, src)
			out.HeadCommit = draft.Git.Commit
		}
	} else if !errors.Is(err, stackops.ErrNoGit) {
		return nil, err
	}
	// A draft that points a stack that is not from git at a repository is the first sync with it (D-24).
	if _, pending, err := repo.PendingGit(ctx, draft.StackID); err != nil {
		return nil, err
	} else if pending {
		out.Converts = true
		out.HeadCommit = draft.Git.Commit
	}
	dep, deployed, err := repo.DeployedGit(ctx, draft.StackID)
	if err != nil {
		return nil, err
	}
	if !deployed {
		return out, nil
	}
	out.DeployedCommit = dep.Commit
	out.SourceChanged = dep.Source.RepoID != src.RepoID || dep.Source.RefKind != src.RefKind || dep.Source.Ref != src.Ref || dep.Source.Folder != src.Folder
	if !out.SourceChanged && d.GitSync != nil {
		out.Touched = d.GitSync.CommitsSince(ctx, src.RepoID, src.Folder, dep.Commit, draft.Git.Commit)
	}
	return out, nil
}

// replacedSource is the source the stack follows now, as a pending source replaces it. The repository's name is read from
// the repository, which may be gone; the draft's own record of its name stands when it is the same repository.
func replacedSource(ctx context.Context, d StacksDeps, sg stackops.StackGit, pending stackops.GitSource) *StackReviewGitSourceBody {
	name := ""
	switch {
	case sg.RepoID == pending.RepoID:
		name = pending.RepoName
	case d.GitConns != nil:
		if r, err := d.GitConns.GetRepo(ctx, sg.RepoID); err == nil {
			name = r.Name
		}
	}
	return &StackReviewGitSourceBody{
		RepoName: name, RefKind: sg.RefKind, Ref: sg.Ref, Folder: sg.Folder, ComposeFiles: composeFilesOf(sg.ComposeFiles),
	}
}

// stackReviewBody turns a review into its answer. Every list is present, empty when there is nothing.
func stackReviewBody(rv stackops.Review) StackReviewBody {
	b := StackReviewBody{
		Problems: make([]StackProblemBody, 0, len(rv.Problems)),
		Summary: StackReviewSummaryBody{
			Services: countsBody(rv.Summary.Services), Networks: countsBody(rv.Summary.Networks), Volumes: countsBody(rv.Summary.Volumes),
			Secrets: countsBody(rv.Summary.Secrets), Configs: countsBody(rv.Summary.Configs),
			ImagesPinned: rv.Summary.ImagesPinned, ImagesNew: rv.Summary.ImagesNew, OwnershipOnly: rv.Summary.OwnershipOnly,
			RevertsDrift: rv.Summary.RevertsDrift,
		},
		Images:       make([]StackReviewImageBody, 0, len(rv.Images)),
		Services:     make([]StackReviewServiceBody, 0, len(rv.Services)),
		Objects:      make([]StackReviewObjectBody, 0, len(rv.Objects)),
		DraftVersion: rv.DraftVersion, Pins: rv.Pins, NeedsAdmin: rv.NeedsAdmin,
		YAML:         textDiffBody(rv.YAML),
		RevertsDrift: make([]StackRevertsDriftBody, 0, len(rv.RevertsDrift)), RevertsDriftUnavailable: rv.RevertsDriftUnavailable,
	}
	for _, r := range rv.RevertsDrift {
		b.RevertsDrift = append(b.RevertsDrift, StackRevertsDriftBody{
			Service: r.Service, Change: r.Change, Path: r.Path, Live: r.Live, Desired: r.Desired, Cause: causeBody(r.Cause),
		})
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
	b.Services = serviceBodies(rv.Services)
	if a := rv.Adoption; a != nil {
		b.Adoption = &StackReviewAdoptionBody{Pending: a.Pending, OwnershipOnly: a.OwnershipOnly}
	}
	for _, o := range rv.Objects {
		b.Objects = append(b.Objects, StackReviewObjectBody{Change: o.Change, Kind: o.Kind, Name: o.Name, Detail: o.Detail, Subnet: o.Subnet, SmallSubnetDefault: o.SmallSubnetDefault})
	}
	return b
}

// serviceBodies turns the service changes of a review into their answer. The list is never null.
func serviceBodies(changes []stacks.ServiceChange) []StackReviewServiceBody {
	out := make([]StackReviewServiceBody, 0, len(changes))
	for _, c := range changes {
		hidden := c.HiddenChanges
		if hidden == nil {
			hidden = []string{}
		}
		out = append(out, StackReviewServiceBody{
			Name: c.Name, Short: c.Short, Change: c.Change, Current: specTree(c.Current), Next: specTree(c.Next),
			HiddenChanges: hidden, Settings: c.Settings, Because: c.Because, OwnershipOnly: c.OwnershipOnly,
		})
	}
	return out
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
