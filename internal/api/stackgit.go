// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitconn"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// problemStackNotGit is the problem type of a git route asked about a stack that is not from a repository.
const problemStackNotGit = "stack_not_git"

// StackGitSummary is where a git stack stands against its repository: what it follows, the commit it deployed, the
// head of the ref the last check saw, how far behind that is and why a check failed (D-12, D-13, D-16).
type StackGitSummary struct {
	RepoID   string `json:"repoId" doc:"The repository the stack follows"`
	RepoName string `json:"repoName" doc:"The repository's name; empty when it could not be read"`
	RepoHost string `json:"repoHost" doc:"The host of the repository's address, such as github.com; empty when it could not be read"`
	RepoPath string `json:"repoPath" doc:"The repository's path on that host without the leading slash and the .git, such as acme/stacks; empty when it could not be read"`
	RefKind  string `json:"refKind" enum:"branch,tag,commit" doc:"What ref names: a branch or a tag, which are followed, or a commit, which is fixed and never behind"`
	Ref      string `json:"ref" doc:"The branch, tag or commit ID the stack follows"`
	Folder   string `json:"folder" doc:"The folder of the repository the stack lives in; empty for the root"`

	ComposeFiles []string `json:"composeFiles" doc:"The compose files of the folder the stack merges, in merge order: a key of a later file wins over the same key of an earlier one"`

	DeployedSha  string `json:"deployedSha" doc:"The full ID of the commit the stack's newest good revision was made from; empty when nothing was deployed from the repository"`
	HeadSha      string `json:"headSha" doc:"The full ID of the commit the ref pointed at when the stack was last checked; empty when it never was"`
	Behind       int    `json:"behind" minimum:"0" doc:"How many commits that changed the stack's folder the head has beyond the deployed commit (newest first, so a commit that changed only other folders is not counted). 0 unless status is behind, where it is at least 1"`
	BehindCapped bool   `json:"behindCapped" doc:"True when behind is the most that is counted: there may be more commits"`
	Status       string `json:"status" enum:"unknown,in_sync,behind,error" doc:"unknown: never checked, or nothing deployed to compare with. in_sync: the stack's folder at the head is the deployed one, even if the head has moved. behind: the folder has changed. error: the last check could not be made, and reasonCode says why; what is deployed keeps running"`
	ReasonCode   string `json:"reasonCode,omitempty" enum:"fetch_failed,host_key_changed,host_key_untrusted,ref_gone,folder_gone,checks_failed,source_changed,auto_deploy_blocked,auto_deploy_failed" doc:"Why the status is error, or a note on behind (source_changed); left out when there is no reason. fetch_failed: the repository can't be reached or refused the credential. host_key_changed, host_key_untrusted: the ssh host key. ref_gone: the branch or tag no longer exists. folder_gone: the folder is not in the repository. checks_failed: the new commit fails the checks. source_changed: the stack follows another repository, ref or folder than the one deployed. auto_deploy_blocked, auto_deploy_failed: an automatic deploy was held back or failed"`
	ReasonDetail string `json:"reasonDetail,omitempty" doc:"A sentence on the reason, never text from the server; left out when there is no reason"`
	CheckedAt    string `json:"checkedAt,omitempty" format:"date-time" doc:"When the stack was last checked, RFC 3339 UTC; left out when it never was"`
	Checking     bool   `json:"checking" doc:"True while a check of the stack runs now"`

	AutoDeploy               bool                     `json:"autoDeploy" doc:"True when a commit that changes the folder is deployed automatically"`
	AutoDeployPaused         bool                     `json:"autoDeployPaused" doc:"True when automatic deploys are paused: after a roll back, or because the owner lacks a permission"`
	AutoDeployPausedReason   string                   `json:"autoDeployPausedReason,omitempty" enum:"owner,rollback" doc:"Why automatic deploys are paused: owner (the person whose authority they run on no longer holds the permission to deploy here) or rollback (the stack was rolled back, so the repository head is not deployed over it). Left out unless autoDeployPaused"`
	AutoDeployPausedRevision int64                    `json:"autoDeployPausedRevision,omitempty" minimum:"1" doc:"For autoDeployPausedReason rollback: the number of the revision the stack was rolled back to. Left out otherwise"`
	AutoDeployOwner          *StackGitAutoDeployOwner `json:"autoDeployOwner,omitempty" doc:"The person whose authority automatic deploys run on; only this route and the polling route answer it. Left out when auto-deploy is off or has no owner on record"`

	PollEnabled bool `json:"pollEnabled" doc:"True when Bayplan checks the repository on a schedule. A stack that follows a fixed commit is never polled, whatever this says"`
	PollMinutes int  `json:"pollMinutes" minimum:"1" maximum:"1440" doc:"How often the repository is checked, in minutes"`

	Webhook *StackGitWebhookSummary `json:"webhook,omitempty" doc:"The stack's webhook; left out when it has none, and on the stack list and detail, which don't carry it. Only this route and the polling route answer it. It never holds the secret or the routing token"`
}

// StackGitWebhookSummary is a stack's webhook as it can be read after it was made: the secret and the routing token were
// shown once, when it was created or rotated, and cannot be read again (D-15; UI-SPEC default 9).
type StackGitWebhookSummary struct {
	Flavour    string               `json:"flavour" enum:"github,gitlab,gitea,generic" doc:"The git host the webhook is for"`
	Placement  string               `json:"placement" enum:"path,header" doc:"Where the secret goes: path (in the address) or header"`
	Address    string               `json:"address" doc:"The address the git host calls with its routing token masked: {publicURL}/hooks/git/••••. The token is kept only as a hash, so the full address was shown once, when the webhook was created or rotated"`
	Last4      string               `json:"last4" doc:"The last four characters of the secret (of the token, for a webhook whose address is the secret)"`
	LastAt     string               `json:"lastAt,omitempty" format:"date-time" doc:"When the webhook last saw a request that named it, RFC 3339 UTC; left out when it never did"`
	LastResult string               `json:"lastResult,omitempty" enum:"accepted,rejected" doc:"What became of that request: accepted, or rejected for a signature that did not match; left out when there was none"`
	Owner      StackGitWebhookOwner `json:"owner" doc:"The person whose authority the webhook acts on"`
	Paused     bool                 `json:"paused" doc:"True when the webhook does nothing: its owner no longer holds the permission that made it, is disabled or deleted. Every request to it is a 404 until it is handed to an Admin of the environment"`
}

// StackGitAutoDeployOwner names the owner of a stack's automatic deploys.
type StackGitAutoDeployOwner struct {
	ID   string `json:"id" doc:"The user's ID"`
	Name string `json:"name" doc:"The user's display name, else the username; empty when the user is gone"`
}

// StackGitWebhookOwner names the owner of a webhook.
type StackGitWebhookOwner struct {
	ID   string `json:"id" doc:"The user's ID; empty when the webhook has no owner on record"`
	Name string `json:"name" doc:"The user's display name, else the username; empty when the user is gone"`
}

// gitSummaryOf builds the summary of a stack from its settings, the state of its last check and its repository. A zero
// repo leaves the repository's name and address empty.
func gitSummaryOf(sg stackops.StackGit, st stackops.GitState, repo gitconn.Repo, checking bool) StackGitSummary {
	out := StackGitSummary{
		RepoID: sg.RepoID, RepoName: repo.Name, RefKind: sg.RefKind, Ref: sg.Ref, Folder: sg.Folder, ComposeFiles: composeFilesOf(sg.ComposeFiles),
		DeployedSha: st.DeployedCommit, HeadSha: st.HeadCommit, Behind: st.Behind, BehindCapped: st.Behind >= gitsync.BehindCap,
		Status: st.Status, ReasonCode: st.ReasonCode, ReasonDetail: st.ReasonDetail, Checking: checking,
		AutoDeploy: sg.AutoDeploy, AutoDeployPaused: sg.AutoDeployPausedReason != "", AutoDeployPausedReason: sg.AutoDeployPausedReason,
		PollEnabled: sg.PollEnabled, PollMinutes: sg.PollIntervalSeconds / 60,
	}
	if sg.AutoDeployPausedReason == stackops.PausedRollback {
		out.AutoDeployPausedRevision = sg.AutoDeployPausedSeq
	}
	if out.Status == "" {
		out.Status = stackops.GitStatusUnknown
	}
	if st.CheckedAt != nil {
		out.CheckedAt = rfc3339(*st.CheckedAt)
	}
	if repo.URL != "" {
		if u, err := gitx.ValidateURL(repo.URL); err == nil {
			out.RepoHost = u.Host
			out.RepoPath = strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
		}
	}
	return out
}

// composeFilesOf is the list as the body sends it: an empty list and not null.
func composeFilesOf(files []string) []string {
	if files == nil {
		return []string{}
	}
	return files
}

// gitSummaries are the git summaries of the git stacks of an environment, by stack ID: one query for their settings, one
// for their states, and one for each repository they follow. An environment with no git stack costs nothing. A stack
// whose state a restore did not carry reads as unknown.
func (d StacksDeps) gitSummaries(ctx context.Context, envID string) (map[string]*StackGitSummary, error) {
	repo := d.repo()
	settings, err := repo.StackGitsOfEnv(ctx, envID)
	if err != nil || len(settings) == 0 {
		return nil, err
	}
	states, err := repo.GitStates(ctx, envID)
	if err != nil {
		return nil, err
	}
	repos := map[string]gitconn.Repo{}
	out := make(map[string]*StackGitSummary, len(settings))
	for _, sg := range settings {
		out[sg.StackID] = d.gitSummary(ctx, sg, states[sg.StackID], repos)
	}
	return out, nil
}

// gitSummary builds one summary, reading the repository once into known.
func (d StacksDeps) gitSummary(ctx context.Context, sg stackops.StackGit, st stackops.GitState, known map[string]gitconn.Repo) *StackGitSummary {
	r, ok := known[sg.RepoID]
	if !ok && d.GitConns != nil {
		var err error
		if r, err = d.GitConns.GetRepo(ctx, sg.RepoID); err != nil {
			log := d.Log
			if log == nil {
				log = slog.Default()
			}
			log.Warn("could not read the repository of a git stack", "repository", sg.RepoID, "error", err)
		}
		known[sg.RepoID] = r
	}
	checking := d.GitSync != nil && d.GitSync.InFlight(sg.StackID)
	s := gitSummaryOf(sg, st, r, checking)
	return &s
}

// gitSummaryOfStack is the summary of one stack, nil for a stack that is not from git.
func (d StacksDeps) gitSummaryOfStack(ctx context.Context, stackID string) (*StackGitSummary, error) {
	sum, _, err := d.gitSummaryAndSettings(ctx, stackID)
	return sum, err
}

// gitSummaryAndSettings is gitSummaryOfStack with the stack's git settings it was made from, which hold the webhook.
func (d StacksDeps) gitSummaryAndSettings(ctx context.Context, stackID string) (*StackGitSummary, stackops.StackGit, error) {
	repo := d.repo()
	sg, err := repo.StackGit(ctx, stackID)
	if errors.Is(err, stackops.ErrNoGit) {
		return nil, stackops.StackGit{}, nil
	}
	if err != nil {
		return nil, stackops.StackGit{}, err
	}
	st, err := repo.GitState(ctx, stackID)
	if err != nil {
		return nil, stackops.StackGit{}, err
	}
	return d.gitSummary(ctx, sg, st, map[string]gitconn.Repo{}), sg, nil
}

// webhookSummary is the webhook of a git stack as it can be read: nothing for a stack with none. The routing token is
// kept only as a hash, so the address shows it masked, and the secret never leaves; the last four characters stay. An
// owner that the access service cannot find (a restore that did not carry the record) reads as a paused webhook, since
// a delivery to it is a 404.
func (g GitDeps) webhookSummary(ctx context.Context, sg stackops.StackGit, host string) (*StackGitWebhookSummary, error) {
	if sg.WebhookTokenHash == "" {
		return nil, nil
	}
	out := &StackGitWebhookSummary{
		Flavour: sg.WebhookFlavour, Placement: sg.WebhookPlacement, Address: g.webhookAddress(ctx, host, webhookMask),
		Last4: sg.WebhookSecretLast4, LastResult: sg.WebhookLastResult,
	}
	if sg.WebhookLastAt != nil {
		out.LastAt = rfc3339(*sg.WebhookLastAt)
	}
	if g.Access == nil {
		return out, nil
	}
	owned, err := g.Access.GetOwned(ctx, gitsync.KindGitWebhook, sg.StackID)
	switch {
	case errors.Is(err, access.ErrOwnedNotFound):
		out.Paused = true
		return out, nil
	case err != nil:
		return nil, err
	}
	out.Paused = owned.State != access.OwnedActive
	out.Owner.ID = owned.OwnerID
	switch u, err := g.Access.Store.RQ.GetUserByID(ctx, owned.OwnerID); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	case u.DeletedAt.Valid:
	case u.DisplayName != "":
		out.Owner.Name = u.DisplayName
	default:
		out.Owner.Name = u.Username
	}
	return out, nil
}

// autoDeployOwner is the person automatic deploys of a git stack run for: nothing while auto-deploy is off or when the
// access service holds no record of it (a roll back forgets it). A user who is gone has an empty name.
func (g GitDeps) autoDeployOwner(ctx context.Context, sg stackops.StackGit) (*StackGitAutoDeployOwner, error) {
	if !sg.AutoDeploy || g.Access == nil {
		return nil, nil
	}
	owned, err := g.Access.GetOwned(ctx, gitsync.KindGitAutoDeploy, sg.StackID)
	switch {
	case errors.Is(err, access.ErrOwnedNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	out := &StackGitAutoDeployOwner{ID: owned.OwnerID}
	switch u, err := g.Access.Store.RQ.GetUserByID(ctx, owned.OwnerID); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	case u.DeletedAt.Valid:
	case u.DisplayName != "":
		out.Name = u.DisplayName
	default:
		out.Name = u.Username
	}
	return out, nil
}

// webhookMask stands in for the routing token in an address that is shown again.
const webhookMask = "\u2022\u2022\u2022\u2022"

// StackGitOutput wraps StackGitSummary for huma.
type StackGitOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackGitSummary
}

// StackGitCheckBody says a check was started.
type StackGitCheckBody struct {
	Checking bool `json:"checking" doc:"True: a check of the stack is running. Read the stack's git route, or its list entry, for the answer"`
}

// StackGitCheckOutput wraps StackGitCheckBody for huma.
type StackGitCheckOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackGitCheckBody
}

// StackGitUpdatesInput is the body of PUT /api/env/{envId}/stacks/{stackId}/git/updates.
type StackGitUpdatesInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		PollEnabled bool    `json:"pollEnabled" doc:"True to check the repository on a schedule"`
		PollMinutes float64 `json:"pollMinutes" doc:"How often to check, in whole minutes from 1 to 1440. Anything else, including a fraction, is 422 poll_interval"`
	}
	host string
}

// Resolve reads the host the request came to, for the masked webhook address of a server that has no public URL set.
func (in *StackGitUpdatesInput) Resolve(ctx huma.Context) []error {
	in.host = ctx.Host()
	return nil
}

// StackGitAutoDeployInput is the request of PUT /api/env/{envId}/stacks/{stackId}/git/auto-deploy.
type StackGitAutoDeployInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Enabled bool `json:"enabled" doc:"True to deploy a commit that changes the stack's folder by itself, on the authority of the person who sends this request; false to have such a commit wait for review"`
	}
	host string
}

// Resolve reads the host the request came to, for the masked webhook address of a server that has no public URL set.
func (in *StackGitAutoDeployInput) Resolve(ctx huma.Context) []error {
	in.host = ctx.Host()
	return nil
}

// StackGitReadInput is the request of GET /api/env/{envId}/stacks/{stackId}/git.
type StackGitReadInput struct {
	StackPathInput
	host string
}

// Resolve reads the host the request came to, for the masked webhook address of a server that has no public URL set.
func (in *StackGitReadInput) Resolve(ctx huma.Context) []error {
	in.host = ctx.Host()
	return nil
}

// problemPollInterval is the problem type of a polling interval that is not a whole number of minutes from 1 to 1440.
const problemPollInterval = "poll_interval"

// StackGitPullInput is the body of POST /api/env/{envId}/stacks/{stackId}/git/pull.
type StackGitPullInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		DraftVersion int64 `json:"draftVersion" minimum:"0" doc:"The version of the draft you read. If the draft moved since (a save, or another pull), the answer is 409 draft_changed and nothing is stored. 0 for a stack whose draft was discarded"`
	}
}

// StackGitPullBody is what a pull did to the draft.
type StackGitPullBody struct {
	DraftVersion   int64  `json:"draftVersion" doc:"The version of the draft after the pull: review it and deploy it at this version"`
	Commit         string `json:"commit" doc:"The full ID of the commit the draft was refreshed from"`
	Ref            string `json:"ref" doc:"The branch, tag or commit ID the stack follows"`
	DeployedCommit string `json:"deployedCommit" doc:"The full ID of the commit the stack's newest good revision was made from; empty when nothing was deployed from the repository"`
	Touched        int    `json:"touched" minimum:"0" doc:"How many commits changed the stack's folder between deployedCommit and commit; 0 when nothing is deployed to compare with"`
}

// StackGitSourceInput is the body of POST /api/env/{envId}/stacks/{stackId}/git/source.
type StackGitSourceInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		DraftVersion int64    `json:"draftVersion" minimum:"0" doc:"The version of the draft you read. If the draft moved since (a save, a pull, another change of source), the answer is 409 draft_changed and nothing is stored. 0 for a stack whose draft was discarded"`
		RepoID       string   `json:"repoId" maxLength:"64" doc:"The repository: one of this environment's, or an instance repository. Another environment's is 404 repository_not_found"`
		RefKind      string   `json:"refKind" enum:"branch,tag,commit" doc:"What ref names: a branch, a tag, or a commit by its ID or a prefix of 7 to 40 hex characters"`
		Ref          string   `json:"ref" maxLength:"2048" doc:"The branch, tag or commit. 422 ref_invalid for a name git would refuse"`
		Folder       string   `json:"folder,omitempty" maxLength:"4096" doc:"The folder of the repository the stack lives in, without a leading or trailing slash; the root when empty (422 folder_invalid for a path outside the repository)"`
		ComposeFiles []string `json:"composeFiles" minItems:"1" maxItems:"50" doc:"The compose files, relative to the folder, in merge order. 422 compose_path_invalid for a path outside the folder or one named twice; 422 compose_file_missing for one the folder does not hold at the commit"`
	}
}

// StackGitSourceBody is what a change of source did to the draft.
type StackGitSourceBody struct {
	DraftVersion int64  `json:"draftVersion" doc:"The version of the draft after the change: review it and deploy it at this version"`
	Commit       string `json:"commit" doc:"The full ID of the commit the draft was made from, at the head of the new source"`
	Ref          string `json:"ref" doc:"The branch, tag or commit ID the new source follows"`
}

// StackGitSourceOutput wraps StackGitSourceBody for huma.
type StackGitSourceOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackGitSourceBody
}

// StackGitConnectInput is the body of POST /api/env/{envId}/stacks/{stackId}/connect-git.
type StackGitConnectInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		DraftVersion int64                     `json:"draftVersion" minimum:"0" doc:"The version of the draft you read. If the draft moved since, the answer is 409 draft_changed and nothing is stored. 0 for a stack whose draft was discarded"`
		RepoID       string                    `json:"repoId" maxLength:"64" doc:"The repository: one of this environment's, or an instance repository. Another environment's is 404 repository_not_found"`
		RefKind      string                    `json:"refKind" enum:"branch,tag,commit" doc:"What ref names: a branch, a tag, or a commit by its ID or a prefix of 7 to 40 hex characters"`
		Ref          string                    `json:"ref" maxLength:"2048" doc:"The branch, tag or commit. 422 ref_invalid for a name git would refuse"`
		Folder       string                    `json:"folder,omitempty" maxLength:"4096" doc:"The folder of the repository the stack lives in, without a leading or trailing slash; the root when empty (422 folder_invalid for a path outside the repository)"`
		ComposeFiles []string                  `json:"composeFiles" minItems:"1" maxItems:"50" doc:"The compose files, relative to the folder, in merge order. 422 compose_path_invalid for a path outside the folder or one named twice; 422 compose_file_missing for one the folder does not hold at the commit"`
		Files        []StackGitConnectFileBody `json:"files,omitempty" maxItems:"50" doc:"What becomes of the files of the stack's draft: each one named is kept as an overlay file or dropped; one not named is dropped. 422 file_path_invalid for a path that is not a file of the draft, file_name_taken for one named twice, overlay_clash for a file to keep that the repository holds"`
		Vars         []DraftVarBody            `json:"vars,omitempty" maxItems:"500" doc:"Values to set over the stack's variables, by name. A variable the stack holds that is not named stays; a secret variable sent as <hidden> keeps its saved value"`
		Overlays     []GitStackOverlayBody     `json:"overlays,omitempty" maxItems:"50" doc:"New overlay files for paths the repository does not have, beside the files kept from the draft. Each is stored sealed as a secret file. 422 file_name_taken for a path named twice or also kept, file_path_invalid for a bad path, file_too_large over 1 MiB"`
	}
}

// StackGitConnectFileBody is the choice for one file of the stack as it is today.
type StackGitConnectFileBody struct {
	Path string `json:"path" maxLength:"1024" doc:"The path of a file of the stack's draft"`
	Keep bool   `json:"keep" doc:"True to keep the file as an overlay file (sealed, secret); false to drop it"`
}

// StackGitConnectBody is what pointing a stack at a repository did to its draft.
type StackGitConnectBody struct {
	DraftVersion int64  `json:"draftVersion" doc:"The version of the draft after the change: review it and deploy it at this version"`
	Commit       string `json:"commit" doc:"The full ID of the commit the draft was made from, at the head of the ref"`
	Ref          string `json:"ref" doc:"The branch, tag or commit ID the stack will follow"`
}

// StackGitConnectOutput wraps StackGitConnectBody for huma.
type StackGitConnectOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackGitConnectBody
}

// StackGitCommitsInput is the query of GET /api/env/{envId}/stacks/{stackId}/git/commits.
type StackGitCommitsInput struct {
	StackPathInput
	Head string `query:"head" required:"true" pattern:"^[0-9a-f]{40}$" doc:"The full ID of the newest commit to list, 40 lowercase hexadecimal characters"`
}

// StackGitCommitBody is one commit that changed the stack's folder.
type StackGitCommitBody struct {
	SHA     string `json:"sha" doc:"The full ID of the commit"`
	Subject string `json:"subject" doc:"The first line of the commit message, at most 200 characters. Text, never markup"`
	Author  string `json:"author" doc:"The author's name, at most 200 characters. Text, never markup"`
	When    string `json:"when" format:"date-time" doc:"When the commit was authored, RFC 3339 UTC"`
}

// StackGitCommitsBody is the commits between the deployed commit and a head that changed the stack's folder.
type StackGitCommitsBody struct {
	Commits   []StackGitCommitBody `json:"commits" doc:"The commits that changed the stack's folder, newest first; commits of the same time are ordered by ID. At most 50 are named: total is how many there are"`
	Total     int                  `json:"total" minimum:"0" doc:"How many commits changed the folder between the deployed commit and head"`
	Capped    bool                 `json:"capped" doc:"True when the walk stopped at its limit of 2000 commits: there may be more than total"`
	Available bool                 `json:"available" doc:"False when Bayplan can't list the commits: nothing is deployed from the repository, the stack follows another source than the one deployed, the mirror doesn't hold the commits, or the deployed commit is not an ancestor of head. commits is then empty"`
}

// StackGitCommitsOutput wraps StackGitCommitsBody for huma.
type StackGitCommitsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackGitCommitsBody
}

// StackGitPullOutput wraps StackGitPullBody for huma.
type StackGitPullOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackGitPullBody
}

// StackGitWebhookInput is the request of POST /api/env/{envId}/stacks/{stackId}/git/webhook.
type StackGitWebhookInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Flavour   string `json:"flavour" enum:"github,gitlab,gitea,generic" doc:"The git host that will call the address: github, gitlab, gitea (Gitea, Forgejo and Gogs) or generic (any tool that can send a POST request). It decides which header proves a request came from the host"`
		Placement string `json:"placement,omitempty" enum:"path,header" doc:"Only for generic: where the secret goes. path (the default) puts it in the address, so the address is the secret; header expects it as Authorization: Bearer <secret>. The other flavours always send a signature in a header, so they take header or nothing. Anything else is 422 webhook_placement"`
	}
	host string
}

// Resolve reads the host the request came to, for the address of a server that has no public URL set.
func (in *StackGitWebhookInput) Resolve(ctx huma.Context) []error {
	in.host = ctx.Host()
	return nil
}

// StackGitWebhookBody is a new webhook, shown once.
type StackGitWebhookBody struct {
	URL       string `json:"url" doc:"The full address the git host calls, built on the public URL: {publicURL}/hooks/git/{token}. It holds the routing token and is shown only in this answer. For a generic webhook with the secret in the address, the address is the secret"`
	Secret    string `json:"secret" doc:"The secret to give the git host (GitHub's Secret, GitLab's Secret token, Gitea's Secret, a generic webhook's bearer token). Shown only in this answer. Empty for a generic webhook with the secret in the address"`
	Flavour   string `json:"flavour" enum:"github,gitlab,gitea,generic" doc:"The git host the webhook is for"`
	Placement string `json:"placement" enum:"path,header" doc:"Where the secret goes: path (in the address) or header"`
}

// StackGitWebhookOutput wraps StackGitWebhookBody for huma.
type StackGitWebhookOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackGitWebhookBody
}

// StackGitWebhookRotateInput is the request of POST /api/env/{envId}/stacks/{stackId}/git/webhook/rotate.
type StackGitWebhookRotateInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	host      string
}

// Resolve reads the host the request came to, for the address of a server that has no public URL set.
func (in *StackGitWebhookRotateInput) Resolve(ctx huma.Context) []error {
	in.host = ctx.Host()
	return nil
}

// StackGitWebhookRemoveInput is the request of DELETE /api/env/{envId}/stacks/{stackId}/git/webhook.
type StackGitWebhookRemoveInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// StackGitDetachInput is the input of POST /api/env/{envId}/stacks/{stackId}/git/detach.
type StackGitDetachInput struct {
	StackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// The problem types of the webhook routes.
const (
	problemWebhookPlacement = "webhook_placement"
	problemWebhookExists    = "webhook_exists"
	problemWebhookNotFound  = "webhook_not_found"
)

// registerStackGit registers the routes that read a git stack's sync state and ask for a check.
func registerStackGit(r *httpx.Router, s StacksDeps, g GitDeps) {
	limiter := &checkLimiter{perMinute: stackChecksPerMinute}
	httpx.Register(r, huma.Operation{
		OperationID: "getStackGit", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/git",
		Summary: "Where a git stack stands against its repository",
		Description: "The stack's git settings (repository with its name, host and path, ref, folder, auto-deploy) and the answer of its last check: the commit that is deployed, the head the check saw, how many commits that changed the stack's folder it is behind, " +
			"its status (unknown, in_sync, behind or error) with the reason of an error, when it was checked, and whether a check runs now. Nothing is read from the repository: this answers from what the last check stored. " +
			"A webhook, when the stack has one, comes with its git host, placement, address with the routing token masked, the last four characters of its secret, its last delivery, its owner and whether it is paused: the secret and the token are shown once, when the webhook is made, and are never answered again. " +
			"A stack that is not from a repository is 409 stack_not_git; a stack ID of another environment is 404 stack_not_found. Needs stacks.read (Viewer).",
		Tags: []string{"stacks", "git"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitReadInput) (*StackGitOutput, error) {
			return getStackGit(ctx, s, g, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "checkStackGit", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/git/check",
		Summary: "Check a git stack against its repository now",
		Description: "Starts a check and answers 202 at once: it fetches the tracked ref and compares the hash of the stack's folder at the head with the deployed revision's, then stores the head, the count of commits behind, the status and the time. " +
			"It deploys nothing and makes no draft and no revision itself; a stack with auto-deploy on that it finds behind is then deployed (see the auto-deploy route), and Check now tries a commit that auto-deploy held back once more. A request that arrives while a check of the stack runs joins it, and the fetch has a deadline. At most 12 checks a minute per user: 429 rate_limited. " +
			"A check by someone who can't deploy (a Viewer) never gives a commit that auto-deploy held back another try; only a check by someone who can (an Operator) does. " +
			"Read the answer from the stack's git route or its list entry, whose checking is true until the check ends. A stack that is not from a repository is 409 stack_not_git; 503 git_unavailable when this server has no git support. Needs stacks.read (Viewer).",
		Tags: []string{"stacks", "git"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackPathInput) (*StackGitCheckOutput, error) {
			return checkStackGit(ctx, s, g, limiter, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "pullStackGit", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/git/pull",
		Summary: "Pull a git stack's newest commit into its draft",
		Description: "Fetches the head of the ref the stack follows (a stack that follows a fixed commit imports that commit again, with no fetch of a moving ref) and replaces the draft's repository files with the commit's: only the files the compose files name, the folder's .env and the secrets and configs files. " +
			"Bayplan's own files (overlays) and the variables stay. The draft records the commit, the folder's tree hash and the source, and its version goes up by one. " +
			"A pull deploys nothing and changes nothing that runs: review the draft and deploy it as any stack (D-17). " +
			"draftVersion must be the version you read, or the answer is 409 draft_changed and nothing is stored. A failed fetch is 422 with the code of the git failure as its type (unreachable, auth_refused, ref_missing, folder_missing ...) and leaves the draft as it was; " +
			"422 file_too_large or stack_too_large for a commit whose files break the stack's limits; 422 overlay_clash when the commit holds a file at the path of an overlay file, with the draft and the overlay untouched until the overlay file is removed or renamed. A stack that is not from a repository is 409 stack_not_git; 503 git_unavailable when this server has no git support. Audited as git.pulled with the stack, the repository, the ref, the folder and the commit. Needs stacks.deploy (Operator).",
		Tags: []string{"stacks", "git"},
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitPullInput) (*StackGitPullOutput, error) {
			return pullStackGit(ctx, s, g, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "changeStackGitSource", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/git/source",
		Summary: "Change what a git stack follows, through review",
		Description: "Reads the new source (a repository of this environment or of the instance, a branch, tag or commit, a folder and its compose files) at its head into the draft, as a pending source (D-11, D-12): " +
			"the draft's repository files become those of the new commit, the overlay files and the variables stay, and the draft records the commit, the folder's tree hash and the new source. Its version goes up by one. " +
			"Nothing is applied: the stack keeps following its own repository, ref, folder and compose files, and Pull & redeploy and the checks keep using them, until a deploy of the draft ends good, which makes the new source the stack's source. " +
			"Review the draft and deploy it as any stack; discarding the draft, or a pull, drops the pending source. A stack with auto-deploy on does not deploy while a pending source waits. " +
			"draftVersion must be the version you read, or the answer is 409 draft_changed and nothing is stored. The repository must be one of the environment's or the instance's (404 repository_not_found). " +
			"422 ref_invalid, ref_missing, commit_missing, commit_ambiguous, folder_missing, folder_invalid, compose_path_invalid, compose_file_missing, file_too_large or stack_too_large as for creating a stack from a folder, otherwise 422 with the code of the git failure as its type. " +
			"A stack that is not from a repository is 409 stack_not_git; 503 git_unavailable when this server has no git support. Audited as git.source_changed with the stack, the new repository, ref, folder and commit and the ones replaced. Needs git.manage (Admin of the environment), as creating a stack from a repository or pointing one at it does: the new source's files are read into a draft that every Viewer can open.",
		Tags: []string{"stacks", "git"},
	}, rbac.Requirement{Perm: rbac.PermGitManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitSourceInput) (*StackGitSourceOutput, error) {
			return changeStackGitSource(ctx, s, g, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "connectStackGit", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/connect-git",
		Summary: "Point an inline or adopted stack at a git repository, through review",
		Description: "Reads the repository folder at the head of the ref into the stack's draft (D-24): the repository's files, the files of the draft that files says to keep (each becomes a sealed overlay file) and the stack's variables with vars set over them. " +
			"The files not kept are dropped. Nothing is applied: the stack stays inline or adopted, with its revisions, until a deploy of the draft ends good, which makes it a git stack and keeps its history; discarding the draft leaves the stack exactly as it was. " +
			"Review the draft and deploy it as any stack; the review says it replaces the inline files. A stack with a pending conversion can be pointed at another source, which replaces the first. " +
			"draftVersion must be the version you read, or the answer is 409 draft_changed and nothing is stored. A stack that is already a git stack is 409 already_git. The repository must be one of the environment's or the instance's (404 repository_not_found). " +
			"422 overlay_clash for a file to keep that the repository holds, file_path_invalid or file_name_taken for files, ref_invalid, ref_missing, commit_missing, commit_ambiguous, folder_missing, folder_invalid, compose_path_invalid, compose_file_missing, file_too_large or stack_too_large as for creating a stack from a folder, otherwise 422 with the code of the git failure as its type. " +
			"503 git_unavailable when this server has no git support. Audited as stack.pointed_at_git with the stack, the repository, the ref, the folder and the counts of files kept and dropped. Needs git.manage (Admin of the environment).",
		Tags: []string{"stacks", "git"},
	}, rbac.Requirement{Perm: rbac.PermGitManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitConnectInput) (*StackGitConnectOutput, error) {
			return connectStackGit(ctx, s, g, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "listStackGitCommits", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/git/commits",
		Summary: "The commits that changed a git stack's folder since it was deployed",
		Description: "Lists the commits reachable from head and not from the deployed commit that changed the stack's folder, newest first (commits of the same time are ordered by ID), each with its ID, subject, author and time. " +
			"At most 50 are named; total is how many there are, counted over at most 2000 commits (capped says the walk stopped there). It reads the server's mirror of the repository and fetches nothing. " +
			"available is false, with no commits, when the list can't be made: nothing is deployed from the repository, the stack follows another repository, ref or folder than the one deployed, the mirror doesn't hold the commits, or the deployed commit is not an ancestor of head. head must be 40 lowercase hexadecimal characters. " +
			"A stack that is not from a repository is 409 stack_not_git. Needs stacks.read (Viewer).",
		Tags: []string{"stacks", "git"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitCommitsInput) (*StackGitCommitsOutput, error) {
			return listStackGitCommits(ctx, s, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "updateStackGitUpdates", Method: http.MethodPut, Path: "/api/env/{envId}/stacks/{stackId}/git/updates",
		Summary: "Turn a git stack's polling on or off and set how often it is checked",
		Description: "Sets whether Bayplan checks the stack's repository on a schedule (on by default) and every how many minutes: a whole number from 1 to 1440, anything else is 422 poll_interval. " +
			"A webhook, when one is set up, does not replace polling. A stack that follows a fixed commit is never polled, whatever is set here. A poll records the new head and how far behind the stack is, and deploys nothing unless the stack has auto-deploy on. " +
			"Answers the stack's git summary. A change is audited as git.updates_changed with the old and the new values; the same values again change and record nothing. " +
			"A stack that is not from a repository is 409 stack_not_git. Needs stacks.deploy (Operator).",
		Tags: []string{"stacks", "git"},
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitUpdatesInput) (*StackGitOutput, error) {
			return updateStackGitUpdates(ctx, s, g, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "setStackGitAutoDeploy", Method: http.MethodPut, Path: "/api/env/{envId}/stacks/{stackId}/git/auto-deploy",
		Summary: "Turn a git stack's auto-deploy on or off",
		Description: "Off by default: a new commit that changes the stack's folder makes the stack show as behind and waits for someone to review and deploy it. " +
			"With enabled true such a commit deploys by itself, without a review, on the authority of the person who turned it on: Bayplan rechecks that person's permissions before every deploy and judges the stack's rendered settings as that person, " +
			"so an Operator's auto-deploy never deploys host access that needs an Admin, and an owner who loses the permission pauses it. It takes the same operation lock, revision, job and console as a deploy a person starts, and is audited as git.autodeployed. " +
			"A commit that fails a check, a pending draft with unreviewed changes to the variables or overlay files, or a deploy the stack's policy would refuse is not deployed: the stack goes to error auto_deploy_blocked and waits for a person. " +
			"A deploy that fails sets auto_deploy_failed; the last good version keeps running and nothing is rolled back. " +
			"Turning it on makes the caller the owner (and resumes a paused one), registers the automation to them and starts a check; turning it off forgets the owner. Turning it on or off is audited as git.autodeploy_enabled or git.autodeploy_disabled, and a request that changes nothing records nothing. " +
			"Answers the stack's git summary. 403 owner_lacks_permission when the caller could not run it themselves; a stack that is not from a repository is 409 stack_not_git. Needs stacks.deploy (Operator).",
		Tags: []string{"stacks", "git"},
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitAutoDeployInput) (*StackGitOutput, error) {
			return setStackGitAutoDeploy(ctx, s, g, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "createStackGitWebhook", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/git/webhook",
		Summary: "Give a git stack a webhook",
		Description: "Creates the stack's one webhook, the address a git host calls when something is pushed, and answers it once: the full address ({publicURL}/hooks/git/{token}) and the secret, with Cache-Control no-store. " +
			"Neither can be read again. Bayplan keeps the token only as a hash and the secret sealed. A webhook only asks Bayplan to fetch and check this one stack: nothing in the request is read, and it names no ref or commit. The webhook never deploys on its own; a stack with auto-deploy on deploys the commit the check finds, as it does after any check. " +
			"flavour is github, gitlab, gitea or generic; placement is only for generic (path puts the secret in the address, header expects Authorization: Bearer). The webhook acts on the authority of the person who creates it. " +
			"A stack that already has a webhook is 409 webhook_exists; a placement the flavour can't use is 422 webhook_placement; a stack that is not from a repository is 409 stack_not_git. Needs git.manage (Admin of the environment).",
		Tags: []string{"stacks", "git"}, DefaultStatus: http.StatusCreated,
	}, rbac.Requirement{Perm: rbac.PermGitManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitWebhookInput) (*StackGitWebhookOutput, error) {
			return createStackGitWebhook(ctx, s, g, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "rotateStackGitWebhook", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/git/webhook/rotate",
		Summary: "Replace a git stack's webhook address and secret",
		Description: "Makes a new routing token and a new secret for the stack's webhook and stores them in place of the old ones in one step, then answers the new address and secret once, with Cache-Control no-store. " +
			"The old address answers 404 at once, like an address that was never a webhook, and the new one works: update the git host with it. The webhook keeps its git host, its placement and its owner, and counts as having had no delivery. " +
			"For a generic webhook with the secret in the address, the address is the secret and secret is empty. A stack with no webhook is 404 webhook_not_found; a stack that is not from a repository is 409 stack_not_git. " +
			"Audited as git.webhook_rotated, which holds neither the address, the token nor the secret. Needs git.manage (Admin of the environment).",
		Tags: []string{"stacks", "git"},
	}, rbac.Requirement{Perm: rbac.PermGitManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitWebhookRotateInput) (*StackGitWebhookOutput, error) {
			return rotateStackGitWebhook(ctx, s, g, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "detachStackGit", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/git/detach",
		Summary: "Detach a git stack from its repository",
		Description: "Turns the stack back into an inline stack you edit in Bayplan (D-25). The files of the deployed revision become the stack's draft as inline files (overlay files stay secret), and its variables and ordered compose files are kept. " +
			"Bayplan stops checking the repository, removes the webhook (its address answers 404 from now on), turns off auto-deploy and forgets both automations, in one step. Nothing running changes: no deploy starts and no revision is made, so the history keeps every revision. " +
			"The stack can be pointed at a repository again afterwards. A draft the stack had is replaced by the deployed files. A stack that is not from a repository is 409 stack_not_git; 503 git_unavailable when this server has no git support. " +
			"Audited as git.stack_detached with the stack, the revision number and the commit. Needs git.manage (Admin of the environment).",
		Tags: []string{"stacks", "git"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermGitManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitDetachInput) (*struct{}, error) {
			return nil, detachStackGit(ctx, s, g, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "removeStackGitWebhook", Method: http.MethodDelete, Path: "/api/env/{envId}/stacks/{stackId}/git/webhook",
		Summary: "Remove a git stack's webhook",
		Description: "Deletes the webhook's token and secret, so its address answers 404 from now on, and forgets the automation that acted on its owner's authority. Polling is not touched. " +
			"A new webhook can be set up afterwards. A stack with no webhook is 404 webhook_not_found; a stack that is not from a repository is 409 stack_not_git. " +
			"Audited as git.webhook_removed. Needs git.manage (Admin of the environment).",
		Tags: []string{"stacks", "git"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermGitManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitWebhookRemoveInput) (*struct{}, error) {
			return nil, removeStackGitWebhook(ctx, s, g, in)
		})
}

func updateStackGitUpdates(ctx context.Context, d StacksDeps, g GitDeps, in *StackGitUpdatesInput) (*StackGitOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	minutes := in.Body.PollMinutes
	if minutes < 1 || minutes > maxPollMinutes || minutes != math.Trunc(minutes) {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemPollInterval, "Use a whole number from 1 to 1440.")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	sg, err := repo.StackGit(ctx, st.ID)
	if errors.Is(err, stackops.ErrNoGit) {
		return nil, stackNotGit()
	}
	if err != nil {
		return nil, stackInternal(d, "read the git settings", env.ID, err)
	}
	was := sg.PollIntervalSeconds / 60
	now := int(minutes)
	if sg.PollEnabled != in.Body.PollEnabled || was != now {
		if err := repo.SetPolling(ctx, st.ID, in.Body.PollEnabled, time.Duration(now)*time.Minute); err != nil {
			return nil, stackInternal(d, "set the polling", env.ID, err)
		}
		recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionGitUpdatesChanged, st.ID, audit.ResultSuccess, "", map[string]string{
			"stack": st.Name, "poll_enabled": strconv.FormatBool(in.Body.PollEnabled), "poll_minutes": strconv.Itoa(now),
			"poll_enabled_from": strconv.FormatBool(sg.PollEnabled), "poll_minutes_from": strconv.Itoa(was),
		})
	}
	return gitSummaryAnswer(ctx, d, g, st.ID, env.ID, in.host)
}

func setStackGitAutoDeploy(ctx context.Context, d StacksDeps, g GitDeps, in *StackGitAutoDeployInput) (*StackGitOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if g.Access == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	sg, err := repo.StackGit(ctx, st.ID)
	switch {
	case errors.Is(err, stackops.ErrNoGit):
		return nil, stackNotGit()
	case err != nil:
		return nil, stackInternal(d, "read the git settings", env.ID, err)
	}
	_, ownedErr := g.Access.GetOwned(ctx, gitsync.KindGitAutoDeploy, st.ID)
	if ownedErr != nil && !errors.Is(ownedErr, access.ErrOwnedNotFound) {
		return nil, stackInternal(d, "read the auto-deploy owner", env.ID, ownedErr)
	}
	owned := ownedErr == nil

	if in.Body.Enabled {
		// Already running for someone: nothing changes, and nobody else's automation is taken over by asking again.
		if sg.AutoDeploy && sg.AutoDeployPausedReason == "" && owned {
			return gitSummaryAnswer(ctx, d, g, st.ID, env.ID, in.host)
		}
		err = repo.SetAutoDeploy(ctx, st.ID, true, "", 0, func(ctx context.Context, tx *sql.Tx, q *sqlc.Queries) error {
			// A paused one is handed to the caller, who must hold the permission now.
			if err := g.Access.DeregisterOwnedTx(ctx, tx, q, gitsync.KindGitAutoDeploy, st.ID); err != nil {
				return err
			}
			return g.Access.RegisterOwnedTx(ctx, tx, q, access.OwnedResource{
				Kind: gitsync.KindGitAutoDeploy, ResourceID: st.ID, EnvID: env.ID, Name: st.Name, OwnerID: p.ID,
			})
		})
		switch {
		case errors.Is(err, stackops.ErrNoGit):
			return nil, stackNotGit()
		case errors.Is(err, access.ErrOwnerLacksPermission):
			return nil, httpx.Problem(http.StatusForbidden, "owner_lacks_permission", "You can't turn on an automation that acts on your behalf without the permission it needs.")
		case err != nil:
			return nil, stackInternal(d, "turn on the auto-deploy", env.ID, err)
		}
		recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionGitAutoDeployEnabled, st.ID, audit.ResultSuccess, "", map[string]string{"stack": st.Name})
		// A stack that is behind deploys now, and not at the next commit: a person turning it on asked for exactly that.
		if g.Sync != nil {
			g.Sync.Trigger(env.ID, st.ID, gitsync.TriggerManual)
		}
		return gitSummaryAnswer(ctx, d, g, st.ID, env.ID, in.host)
	}

	if !sg.AutoDeploy && !owned {
		return gitSummaryAnswer(ctx, d, g, st.ID, env.ID, in.host)
	}
	err = repo.SetAutoDeploy(ctx, st.ID, false, "", 0, func(ctx context.Context, tx *sql.Tx, q *sqlc.Queries) error {
		return g.Access.DeregisterOwnedTx(ctx, tx, q, gitsync.KindGitAutoDeploy, st.ID)
	})
	switch {
	case errors.Is(err, stackops.ErrNoGit):
		return nil, stackNotGit()
	case err != nil:
		return nil, stackInternal(d, "turn off the auto-deploy", env.ID, err)
	}
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionGitAutoDeployDisabled, st.ID, audit.ResultSuccess, "", map[string]string{"stack": st.Name})
	return gitSummaryAnswer(ctx, d, g, st.ID, env.ID, in.host)
}

func createStackGitWebhook(ctx context.Context, d StacksDeps, g GitDeps, in *StackGitWebhookInput) (*StackGitWebhookOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if g.Access == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	flavour, placement := gitsync.Flavour(in.Body.Flavour), gitsync.Placement(in.Body.Placement)
	switch {
	case placement == "" && flavour == gitsync.FlavourGeneric:
		placement = gitsync.PlacementPath
	case placement == "":
		placement = gitsync.PlacementHeader
	case flavour != gitsync.FlavourGeneric && placement != gitsync.PlacementHeader:
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemWebhookPlacement, "A webhook for this git host always sends its signature in a header.")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}

	token, secret := gitsync.NewWebhookSecrets()
	shown := secret
	if flavour == gitsync.FlavourGeneric && placement == gitsync.PlacementPath {
		shown = "" // the token in the address is the secret
	}
	last4 := lastFour(secret)
	if shown == "" {
		last4 = lastFour(token)
	}
	err = repo.CreateGitWebhook(ctx, stackops.NewGitWebhook{
		StackID: st.ID, Flavour: string(flavour), Placement: string(placement), TokenHash: gitsync.TokenHash(token),
		Secret: shown, Last4: last4, CreatedBy: p.ID,
	}, func(ctx context.Context, tx *sql.Tx, q *sqlc.Queries) error {
		return g.Access.RegisterOwnedTx(ctx, tx, q, access.OwnedResource{
			Kind: gitsync.KindGitWebhook, ResourceID: st.ID, EnvID: env.ID, Name: st.Name, OwnerID: p.ID,
		})
	})
	switch {
	case errors.Is(err, stackops.ErrNoGit):
		return nil, stackNotGit()
	case errors.Is(err, stackops.ErrWebhookExists):
		return nil, httpx.Problem(http.StatusConflict, problemWebhookExists, "This stack already has a webhook.")
	case errors.Is(err, access.ErrOwnerLacksPermission):
		return nil, httpx.Problem(http.StatusForbidden, "owner_lacks_permission", "You can't create a webhook that acts on your behalf without the permission it needs.")
	case err != nil:
		return nil, stackInternal(d, "create the webhook", env.ID, err)
	}
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionGitWebhookCreated, st.ID, audit.ResultSuccess, "", map[string]string{
		"stack": st.Name, "flavour": string(flavour), "placement": string(placement),
	})
	return &StackGitWebhookOutput{CacheControl: "no-store", Body: StackGitWebhookBody{
		URL: g.webhookAddress(ctx, in.host, token), Secret: shown, Flavour: string(flavour), Placement: string(placement),
	}}, nil
}

func rotateStackGitWebhook(ctx context.Context, d StacksDeps, g GitDeps, in *StackGitWebhookRotateInput) (*StackGitWebhookOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	token, secret := gitsync.NewWebhookSecrets()
	flavour, placement, err := repo.RotateWebhook(ctx, st.ID, stackops.WebhookRotation{
		TokenHash: gitsync.TokenHash(token), Secret: secret, SecretLast4: lastFour(secret), TokenLast4: lastFour(token),
	})
	switch {
	case errors.Is(err, stackops.ErrNoGit):
		return nil, stackNotGit()
	case errors.Is(err, stackops.ErrNoWebhook):
		return nil, webhookNotFound()
	case err != nil:
		return nil, stackInternal(d, "rotate the webhook", env.ID, err)
	}
	shown := secret
	if gitsync.Flavour(flavour) == gitsync.FlavourGeneric && gitsync.Placement(placement) == gitsync.PlacementPath {
		shown = "" // the token in the address is the secret
	}
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionGitWebhookRotated, st.ID, audit.ResultSuccess, "", map[string]string{
		"stack": st.Name, "flavour": flavour, "placement": placement,
	})
	return &StackGitWebhookOutput{CacheControl: "no-store", Body: StackGitWebhookBody{
		URL: g.webhookAddress(ctx, in.host, token), Secret: shown, Flavour: flavour, Placement: placement,
	}}, nil
}

// detachStackGit turns a git stack back into an inline one (D-25).
func detachStackGit(ctx context.Context, d StacksDeps, g GitDeps, in *StackGitDetachInput) error {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return huma.Error500InternalServerError("internal error")
	}
	if g.Sync == nil {
		return httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return stackLookupProblem(d, env.ID, err)
	}
	res, err := g.Sync.Detach(ctx, env.ID, st.ID, p.Name)
	switch {
	case errors.Is(err, stackops.ErrNoGit):
		return stackNotGit()
	case err != nil:
		return stackInternal(d, "detach the stack from git", env.ID, err)
	}
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionGitStackDetached, st.ID, audit.ResultSuccess, "", map[string]string{
		"stack": st.Name, "revision": itoa(res.Revision), "commit": res.Commit,
	})
	return nil
}

func removeStackGitWebhook(ctx context.Context, d StacksDeps, g GitDeps, in *StackGitWebhookRemoveInput) error {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return huma.Error500InternalServerError("internal error")
	}
	if g.Access == nil {
		return httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return stackLookupProblem(d, env.ID, err)
	}
	sg, err := repo.StackGit(ctx, st.ID)
	switch {
	case errors.Is(err, stackops.ErrNoGit):
		return stackNotGit()
	case err != nil:
		return stackInternal(d, "read the git settings", env.ID, err)
	}
	err = repo.RemoveGitWebhook(ctx, st.ID, func(ctx context.Context, tx *sql.Tx, q *sqlc.Queries) error {
		return g.Access.DeregisterOwnedTx(ctx, tx, q, gitsync.KindGitWebhook, st.ID)
	})
	switch {
	case errors.Is(err, stackops.ErrNoGit):
		return stackNotGit()
	case errors.Is(err, stackops.ErrNoWebhook):
		return webhookNotFound()
	case err != nil:
		return stackInternal(d, "remove the webhook", env.ID, err)
	}
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionGitWebhookRemoved, st.ID, audit.ResultSuccess, "", map[string]string{
		"stack": st.Name, "flavour": sg.WebhookFlavour, "placement": sg.WebhookPlacement,
	})
	return nil
}

// webhookNotFound is the answer to a rotate or remove of a stack that has no webhook.
func webhookNotFound() error {
	return httpx.Problem(http.StatusNotFound, problemWebhookNotFound, "This stack has no webhook.")
}

// webhookAddress is the address a git host calls: the public URL if one is set, else the origin the request came to.
func (g GitDeps) webhookAddress(ctx context.Context, host, token string) string {
	base := ""
	if g.Instance != nil {
		base = g.Instance.PublicURL()
	}
	if base == "" {
		base = httpx.EffectiveScheme(ctx) + "://" + host
	}
	return strings.TrimSuffix(base, "/") + "/hooks/git/" + token
}

// lastFour is the last four characters of s, or all of it when it is shorter.
func lastFour(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

// maxPollMinutes is the longest polling interval: a day.
const maxPollMinutes = 1440

func getStackGit(ctx context.Context, d StacksDeps, g GitDeps, in *StackGitReadInput) (*StackGitOutput, error) {
	st, err := d.repo().Stack(ctx, in.EnvID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, in.EnvID, err)
	}
	return gitSummaryAnswer(ctx, d, g, st.ID, in.EnvID, in.host)
}

// gitSummaryAnswer is the git summary of a stack with its webhook, as the routes that answer it give it.
func gitSummaryAnswer(ctx context.Context, d StacksDeps, g GitDeps, stackID, envID, host string) (*StackGitOutput, error) {
	sum, sg, err := d.gitSummaryAndSettings(ctx, stackID)
	if err != nil {
		return nil, stackInternal(d, "read the git state", envID, err)
	}
	if sum == nil {
		return nil, stackNotGit()
	}
	if sum.Webhook, err = g.webhookSummary(ctx, sg, host); err != nil {
		return nil, stackInternal(d, "read the webhook", envID, err)
	}
	if sum.AutoDeployOwner, err = g.autoDeployOwner(ctx, sg); err != nil {
		return nil, stackInternal(d, "read the auto-deploy owner", envID, err)
	}
	return &StackGitOutput{CacheControl: "no-store", Body: *sum}, nil
}

// stackChecksPerMinute is how many checks of git stacks one user may start in a minute. A check fetches with the
// repository's credential, and the route needs only stacks.read, so the limit is what keeps one account from driving the
// git server (WR-D03).
const stackChecksPerMinute = 12

func checkStackGit(ctx context.Context, d StacksDeps, g GitDeps, lim *checkLimiter, in *StackPathInput) (*StackGitCheckOutput, error) {
	p := httpx.PrincipalFrom(ctx)
	if p == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if g.Sync == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	if !lim.allow(string(p.Type)+":"+p.ID, now()) {
		return nil, httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many checks. Wait a moment and try again.")
	}
	st, err := d.repo().Stack(ctx, in.EnvID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, in.EnvID, err)
	}
	if _, err := d.repo().StackGit(ctx, st.ID); errors.Is(err, stackops.ErrNoGit) {
		return nil, stackNotGit()
	} else if err != nil {
		return nil, stackInternal(d, "read the git settings", in.EnvID, err)
	}
	// Only someone who can deploy gives a commit that auto-deploy held back another try; a Viewer's check just looks.
	trigger := gitsync.TriggerCheck
	if g.Checker != nil && g.Checker.Check(ctx, p, rbac.Require(rbac.PermStacksDeploy), st.EnvID).Allowed {
		trigger = gitsync.TriggerManual
	}
	// A request while a check runs joins it and starts nothing, so a reader cannot pile up fetches (T-09-44).
	if !g.Sync.InFlight(st.ID) {
		go func() {
			out, err := g.Sync.Check(context.WithoutCancel(ctx), st.EnvID, st.ID, trigger)
			if err != nil {
				d.Log.Error("a sync check of a git stack failed", "stack", st.ID, "error", err)
			}
			if g.OnCheck != nil {
				g.OnCheck(st.ID, out, err)
			}
		}()
	}
	return &StackGitCheckOutput{CacheControl: "no-store", Body: StackGitCheckBody{Checking: true}}, nil
}

func pullStackGit(ctx context.Context, d StacksDeps, g GitDeps, in *StackGitPullInput) (*StackGitPullOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if g.Sync == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	sg, err := repo.StackGit(ctx, st.ID)
	if errors.Is(err, stackops.ErrNoGit) {
		return nil, stackNotGit()
	}
	if err != nil {
		return nil, stackInternal(d, "read the git settings", env.ID, err)
	}
	res, err := g.Sync.Pull(ctx, env.ID, st.ID, p.Name, in.Body.DraftVersion)
	var conflict *stackops.DraftConflictError
	if errors.As(err, &conflict) {
		e := draftChanged(conflict)
		if m, ok := e.(*huma.ErrorModel); ok {
			m.Detail = "The draft changed while you were pulling. Reload it, then pull again."
		}
		return nil, e
	}
	if err != nil {
		var known gitconn.Repo
		if g.Conns != nil {
			known, _ = g.Conns.GetRepo(ctx, sg.RepoID)
		}
		return nil, g.importProblem(known, gitx.RefKind(sg.RefKind), sg.Ref, sg.Folder, err)
	}
	// The pull read the repository whether or not the draft changed, so it is audited either way.
	var repoName string
	if g.Conns != nil {
		if r, err := g.Conns.GetRepo(ctx, sg.RepoID); err == nil {
			repoName = r.Name
		}
	}
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionGitPulled, st.ID, audit.ResultSuccess, "", map[string]string{
		"stack": st.Name, "repository": repoName, "ref": res.Ref, "folder": sg.Folder, "commit": res.Commit.SHA, "version": itoa(res.DraftVersion),
	})
	return &StackGitPullOutput{CacheControl: "no-store", Body: StackGitPullBody{
		DraftVersion: res.DraftVersion, Commit: res.Commit.SHA, Ref: res.Ref, DeployedCommit: res.DeployedCommit, Touched: res.Touched,
	}}, nil
}

func changeStackGitSource(ctx context.Context, d StacksDeps, g GitDeps, in *StackGitSourceInput) (*StackGitSourceOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if g.Sync == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	before, err := repo.StackGit(ctx, st.ID)
	if errors.Is(err, stackops.ErrNoGit) {
		return nil, stackNotGit()
	} else if err != nil {
		return nil, stackInternal(d, "read the git settings", env.ID, err)
	}
	body := in.Body
	// The layer rule: a repository of this environment or of the instance, else it is not found.
	known, err := g.browseRepo(ctx, env.ID, body.RepoID)
	if err != nil {
		return nil, err
	}
	kind := gitx.RefKind(body.RefKind)
	res, err := g.Sync.ChangeSource(ctx, env.ID, st.ID, p.Name, body.DraftVersion, gitsync.SourceChange{
		RepoID: known.ID, RefKind: kind, Ref: body.Ref, Folder: body.Folder, ComposeFiles: body.ComposeFiles,
	})
	var conflict *stackops.DraftConflictError
	if errors.As(err, &conflict) {
		e := draftChanged(conflict)
		if m, ok := e.(*huma.ErrorModel); ok {
			m.Detail = "The draft changed while you were changing the source. Reload it, then change the source again."
		}
		return nil, e
	}
	if err != nil {
		return nil, g.importProblem(known, kind, body.Ref, body.Folder, err)
	}
	// Where the stack pointed before is named too, so the log shows a stack being re-pointed and from what.
	details := map[string]string{
		"stack": st.Name, "repository": known.Name, "ref_kind": string(kind), "ref": res.Ref, "folder": strings.Trim(body.Folder, "/"),
		"commit": res.Commit.SHA, "version": itoa(res.DraftVersion),
		"ref_kind_from": before.RefKind, "ref_from": before.Ref, "folder_from": before.Folder,
	}
	if g.Conns != nil {
		if r, err := g.Conns.GetRepo(ctx, before.RepoID); err == nil {
			details["repository_from"] = r.Name
		}
	}
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionGitSourceChanged, st.ID, audit.ResultSuccess, "", details)
	return &StackGitSourceOutput{CacheControl: "no-store", Body: StackGitSourceBody{
		DraftVersion: res.DraftVersion, Commit: res.Commit.SHA, Ref: res.Ref,
	}}, nil
}

// problemAlreadyGit is the problem type of a convert asked about a stack that is a git stack already.
const problemAlreadyGit = "already_git"

// problemOverlayClash is the problem type of a file to keep that the repository holds.
const problemOverlayClash = "overlay_clash"

func connectStackGit(ctx context.Context, d StacksDeps, g GitDeps, in *StackGitConnectInput) (*StackGitConnectOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if g.Sync == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	repo := d.repo()
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	body := in.Body
	vars, err := connectVars(body.Vars)
	if err != nil {
		return nil, err
	}
	// The layer rule: a repository of this environment or of the instance, else it is not found.
	known, err := g.browseRepo(ctx, env.ID, body.RepoID)
	if err != nil {
		return nil, err
	}
	kind := gitx.RefKind(body.RefKind)
	files := make([]gitsync.FileChoice, len(body.Files))
	for i, f := range body.Files {
		files[i] = gitsync.FileChoice{Path: f.Path, Keep: f.Keep}
	}
	res, err := g.Sync.Convert(ctx, env.ID, st.ID, p.Name, gitsync.ConvertRequest{
		SourceChange: gitsync.SourceChange{RepoID: known.ID, RefKind: kind, Ref: body.Ref, Folder: body.Folder, ComposeFiles: body.ComposeFiles},
		Version:      body.DraftVersion, Files: files, Vars: vars, Overlays: overlayFiles(body.Overlays),
	})
	var conflict *stackops.DraftConflictError
	var clash *gitsync.OverlayClashError
	switch {
	case errors.Is(err, stackops.ErrAlreadyGit):
		return nil, httpx.Problem(http.StatusConflict, problemAlreadyGit, "This stack is already from a git repository.")
	case errors.As(err, &conflict):
		e := draftChanged(conflict)
		if m, ok := e.(*huma.ErrorModel); ok {
			m.Detail = "The stack changed while you were choosing. Reload it, then choose again."
		}
		return nil, e
	case errors.As(err, &clash):
		detail := clash.Path + " exists in the repository. Drop it or rename it first."
		return nil, &huma.ErrorModel{
			Type: problemOverlayClash, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
			Detail: detail, Errors: []*huma.ErrorDetail{{Location: "files", Message: clash.Path}},
		}
	case err != nil:
		return nil, g.importProblem(known, kind, body.Ref, body.Folder, err)
	}
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionStackPointedAtGit, st.ID, audit.ResultSuccess, "", map[string]string{
		"stack": st.Name, "repository": known.Name, "ref": res.Ref, "folder": strings.Trim(body.Folder, "/"),
		"kept": itoa(int64(res.Kept)), "dropped": itoa(int64(res.Dropped)),
	})
	return &StackGitConnectOutput{CacheControl: "no-store", Body: StackGitConnectBody{
		DraftVersion: res.DraftVersion, Commit: res.Commit.SHA, Ref: res.Ref,
	}}, nil
}

// connectVars turns the variables of a convert into the ones to set. A secret variable sent as <hidden> is the one the stack
// already holds, so it is left out, and the rest are held to the rules of a save of a draft.
func connectVars(in []DraftVarBody) ([]stacks.Var, error) {
	set := make([]DraftVarBody, 0, len(in))
	for _, v := range in {
		if v.Secret && v.Value == stacks.Hidden {
			continue
		}
		set = append(set, v)
	}
	return gitStackVars(set)
}

func listStackGitCommits(ctx context.Context, d StacksDeps, in *StackGitCommitsInput) (*StackGitCommitsOutput, error) {
	repo := d.repo()
	st, err := repo.Stack(ctx, in.EnvID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, in.EnvID, err)
	}
	sg, err := repo.StackGit(ctx, st.ID)
	if errors.Is(err, stackops.ErrNoGit) {
		return nil, stackNotGit()
	}
	if err != nil {
		return nil, stackInternal(d, "read the git settings", in.EnvID, err)
	}
	out := &StackGitCommitsOutput{CacheControl: "no-store", Body: StackGitCommitsBody{Commits: []StackGitCommitBody{}}}
	dep, deployed, err := repo.DeployedGit(ctx, st.ID)
	if err != nil {
		return nil, stackInternal(d, "read the deployed commit", in.EnvID, err)
	}
	if !deployed || d.GitSync == nil || dep.Source.RepoID != sg.RepoID || dep.Source.Folder != sg.Folder {
		return out, nil
	}
	touch, ok := d.GitSync.CommitsBetween(ctx, sg.RepoID, sg.Folder, dep.Commit, in.Head)
	if !ok {
		return out, nil
	}
	out.Body.Available, out.Body.Total, out.Body.Capped = true, touch.Count, touch.Capped
	for _, c := range touch.Commits {
		out.Body.Commits = append(out.Body.Commits, StackGitCommitBody{SHA: c.SHA, Subject: c.Subject, Author: c.Author, When: rfc3339(c.When)})
	}
	return out, nil
}

// stackNotGit is the 409 for a git route asked about a stack that is not from a repository.
func stackNotGit() error {
	return httpx.Problem(http.StatusConflict, problemStackNotGit, "This stack isn't from a git repository.")
}
