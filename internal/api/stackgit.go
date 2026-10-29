// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitconn"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
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

// stackNotGit is the 409 for a git route asked about a stack that is not from a repository.
func stackNotGit() error {
	return httpx.Problem(http.StatusConflict, problemStackNotGit, "This stack isn't from a git repository.")
}
