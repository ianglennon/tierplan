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

	AutoDeploy       bool `json:"autoDeploy" doc:"True when a commit that changes the folder is deployed automatically"`
	AutoDeployPaused bool `json:"autoDeployPaused" doc:"True when automatic deploys are paused: after a roll back, or because the owner lacks a permission"`

	PollEnabled bool `json:"pollEnabled" doc:"True when Bayplan checks the repository on a schedule. A stack that follows a fixed commit is never polled, whatever this says"`
	PollMinutes int  `json:"pollMinutes" minimum:"1" maximum:"1440" doc:"How often the repository is checked, in minutes"`
}

// gitSummaryOf builds the summary of a stack from its settings, the state of its last check and its repository. A zero
// repo leaves the repository's name and address empty.
func gitSummaryOf(sg stackops.StackGit, st stackops.GitState, repo gitconn.Repo, checking bool) StackGitSummary {
	out := StackGitSummary{
		RepoID: sg.RepoID, RepoName: repo.Name, RefKind: sg.RefKind, Ref: sg.Ref, Folder: sg.Folder, ComposeFiles: composeFilesOf(sg.ComposeFiles),
		DeployedSha: st.DeployedCommit, HeadSha: st.HeadCommit, Behind: st.Behind, BehindCapped: st.Behind >= gitsync.BehindCap,
		Status: st.Status, ReasonCode: st.ReasonCode, ReasonDetail: st.ReasonDetail, Checking: checking,
		AutoDeploy: sg.AutoDeploy, AutoDeployPaused: sg.AutoDeployPausedReason != "",
		PollEnabled: sg.PollEnabled, PollMinutes: sg.PollIntervalSeconds / 60,
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
	repo := d.repo()
	sg, err := repo.StackGit(ctx, stackID)
	if errors.Is(err, stackops.ErrNoGit) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st, err := repo.GitState(ctx, stackID)
	if err != nil {
		return nil, err
	}
	return d.gitSummary(ctx, sg, st, map[string]gitconn.Repo{}), nil
}

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

// problemWebhookPlacement and problemWebhookExists are the problem types of the create-webhook route.
const (
	problemWebhookPlacement = "webhook_placement"
	problemWebhookExists    = "webhook_exists"
)

// registerStackGit registers the routes that read a git stack's sync state and ask for a check.
func registerStackGit(r *httpx.Router, s StacksDeps, g GitDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getStackGit", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/git",
		Summary: "Where a git stack stands against its repository",
		Description: "The stack's git settings (repository with its name, host and path, ref, folder, auto-deploy) and the answer of its last check: the commit that is deployed, the head the check saw, how many commits that changed the stack's folder it is behind, " +
			"its status (unknown, in_sync, behind or error) with the reason of an error, when it was checked, and whether a check runs now. Nothing is read from the repository: this answers from what the last check stored. " +
			"A stack that is not from a repository is 409 stack_not_git; a stack ID of another environment is 404 stack_not_found. Needs stacks.read (Viewer).",
		Tags: []string{"stacks", "git"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackPathInput) (*StackGitOutput, error) {
			return getStackGit(ctx, s, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "checkStackGit", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/git/check",
		Summary: "Check a git stack against its repository now",
		Description: "Starts a check and answers 202 at once: it fetches the tracked ref and compares the hash of the stack's folder at the head with the deployed revision's, then stores the head, the count of commits behind, the status and the time. " +
			"It deploys nothing and makes no draft and no revision. A request that arrives while a check of the stack runs joins it, and the fetch has a deadline, so asking again and again costs one fetch. " +
			"Read the answer from the stack's git route or its list entry, whose checking is true until the check ends. A stack that is not from a repository is 409 stack_not_git; 503 git_unavailable when this server has no git support. Needs stacks.read (Viewer).",
		Tags: []string{"stacks", "git"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackPathInput) (*StackGitCheckOutput, error) {
			return checkStackGit(ctx, s, g, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "pullStackGit", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/git/pull",
		Summary: "Pull a git stack's newest commit into its draft",
		Description: "Fetches the head of the ref the stack follows (a stack that follows a fixed commit imports that commit again, with no fetch of a moving ref) and replaces the draft's repository files with the commit's: only the files the compose files name, the folder's .env and the secrets and configs files. " +
			"Bayplan's own files (overlays) and the variables stay. The draft records the commit, the folder's tree hash and the source, and its version goes up by one. " +
			"A pull deploys nothing and changes nothing that runs: review the draft and deploy it as any stack (D-17). " +
			"draftVersion must be the version you read, or the answer is 409 draft_changed and nothing is stored. A failed fetch is 422 with the code of the git failure as its type (unreachable, auth_refused, ref_missing, folder_missing ...) and leaves the draft as it was; " +
			"422 file_too_large or stack_too_large for a commit whose files break the stack's limits. A stack that is not from a repository is 409 stack_not_git; 503 git_unavailable when this server has no git support. Audited as stack.draft_saved. Needs stacks.deploy (Operator).",
		Tags: []string{"stacks", "git"},
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitPullInput) (*StackGitPullOutput, error) {
			return pullStackGit(ctx, s, g, in)
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
			"A webhook, when one is set up, does not replace polling. A stack that follows a fixed commit is never polled, whatever is set here. A poll only records the new head and how far behind the stack is; it deploys nothing. " +
			"Answers the stack's git summary. A change is audited as git.updates_changed with the old and the new values; the same values again change and record nothing. " +
			"A stack that is not from a repository is 409 stack_not_git. Needs stacks.deploy (Operator).",
		Tags: []string{"stacks", "git"},
	}, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitUpdatesInput) (*StackGitOutput, error) {
			return updateStackGitUpdates(ctx, s, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "createStackGitWebhook", Method: http.MethodPost, Path: "/api/env/{envId}/stacks/{stackId}/git/webhook",
		Summary: "Give a git stack a webhook",
		Description: "Creates the stack's one webhook, the address a git host calls when something is pushed, and answers it once: the full address ({publicURL}/hooks/git/{token}) and the secret, with Cache-Control no-store. " +
			"Neither can be read again. Bayplan keeps the token only as a hash and the secret sealed. A webhook only asks Bayplan to fetch and check this one stack: nothing in the request is read, it never deploys, and it names no ref or commit. " +
			"flavour is github, gitlab, gitea or generic; placement is only for generic (path puts the secret in the address, header expects Authorization: Bearer). The webhook acts on the authority of the person who creates it. " +
			"A stack that already has a webhook is 409 webhook_exists; a placement the flavour can't use is 422 webhook_placement; a stack that is not from a repository is 409 stack_not_git. Needs git.manage (Admin of the environment).",
		Tags: []string{"stacks", "git"}, DefaultStatus: http.StatusCreated,
	}, rbac.Requirement{Perm: rbac.PermGitManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackGitWebhookInput) (*StackGitWebhookOutput, error) {
			return createStackGitWebhook(ctx, s, g, in)
		})
}

func updateStackGitUpdates(ctx context.Context, d StacksDeps, in *StackGitUpdatesInput) (*StackGitOutput, error) {
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
	sum, err := d.gitSummaryOfStack(ctx, st.ID)
	if err != nil {
		return nil, stackInternal(d, "read the git state", env.ID, err)
	}
	if sum == nil {
		return nil, stackNotGit()
	}
	return &StackGitOutput{CacheControl: "no-store", Body: *sum}, nil
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
	return &StackGitWebhookOutput{CacheControl: "no-store", Body: StackGitWebhookBody{
		URL: g.webhookAddress(ctx, in.host, token), Secret: shown, Flavour: string(flavour), Placement: string(placement),
	}}, nil
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

func getStackGit(ctx context.Context, d StacksDeps, in *StackPathInput) (*StackGitOutput, error) {
	st, err := d.repo().Stack(ctx, in.EnvID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, in.EnvID, err)
	}
	sum, err := d.gitSummaryOfStack(ctx, st.ID)
	if err != nil {
		return nil, stackInternal(d, "read the git state", in.EnvID, err)
	}
	if sum == nil {
		return nil, stackNotGit()
	}
	return &StackGitOutput{CacheControl: "no-store", Body: *sum}, nil
}

func checkStackGit(ctx context.Context, d StacksDeps, g GitDeps, in *StackPathInput) (*StackGitCheckOutput, error) {
	if g.Sync == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
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
	// A request while a check runs joins it and starts nothing, so a reader cannot pile up fetches (T-09-44).
	if !g.Sync.InFlight(st.ID) {
		go func() {
			out, err := g.Sync.Check(context.WithoutCancel(ctx), st.EnvID, st.ID, gitsync.TriggerManual)
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
	if res.DraftVersion != in.Body.DraftVersion {
		draft, err := repo.Draft(ctx, st.ID)
		if err != nil {
			return nil, stackInternal(d, "read the refreshed draft", env.ID, err)
		}
		recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionStackDraftSaved, st.ID, audit.ResultSuccess, "", map[string]string{
			"stack": st.Name, "version": itoa(draft.Version), "files": itoa(int64(len(draft.Copy.Files))), "vars": itoa(int64(len(draft.Copy.Vars))),
		})
	}
	return &StackGitPullOutput{CacheControl: "no-store", Body: StackGitPullBody{
		DraftVersion: res.DraftVersion, Commit: res.Commit.SHA, Ref: res.Ref, DeployedCommit: res.DeployedCommit, Touched: res.Touched,
	}}, nil
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
