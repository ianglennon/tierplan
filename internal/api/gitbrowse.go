// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/gitconn"
	"github.com/ianglennon/coaming/internal/gitx"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
)

// browseEntryLimit is how many entries of one folder a tree answers with.
const browseEntryLimit = 500

// GitRefBody is a branch or a tag and the commit it points at.
type GitRefBody struct {
	Name   string `json:"name" doc:"The short name: main, v1.2"`
	Commit string `json:"commit" doc:"The 40-hex commit ID; for an annotated tag the commit the tag points at"`
}

// GitRefsBody is every branch and tag of a repository.
type GitRefsBody struct {
	DefaultBranch string       `json:"defaultBranch" doc:"The branch HEAD points at; empty when the server does not say"`
	Branches      []GitRefBody `json:"branches" doc:"Every branch, in name order"`
	Tags          []GitRefBody `json:"tags" doc:"Every tag, in name order"`
}

// GitRefsOutput wraps GitRefsBody for huma.
type GitRefsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         GitRefsBody
}

// GitTreeEntryBody is one thing in a folder.
type GitTreeEntryBody struct {
	Name       string `json:"name" doc:"The entry's name within the folder"`
	Kind       string `json:"kind" enum:"dir,file,symlink,submodule" doc:"A symlink and a submodule are only reported; they are never followed"`
	HasCompose bool   `json:"hasCompose" doc:"For a folder, whether it directly holds compose.yaml, compose.yml, docker-compose.yaml or docker-compose.yml; false for everything else"`
}

// GitTreeBody is a folder of a commit.
type GitTreeBody struct {
	Commit      string             `json:"commit" doc:"The full 40-hex commit ID the ref resolved to"`
	CommittedAt time.Time          `json:"committedAt" format:"date-time" doc:"When that commit was made"`
	Path        string             `json:"path" doc:"The folder, without a leading or trailing slash; empty for the root"`
	Entries     []GitTreeEntryBody `json:"entries" doc:"Folders first, then everything else, each by name; at most 500"`
	Total       int                `json:"total" doc:"How many entries the folder has"`
	Capped      bool               `json:"capped" doc:"True when entries holds fewer than total"`
}

// GitTreeOutput wraps GitTreeBody for huma.
type GitTreeOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         GitTreeBody
}

// BrowseGitTreeInput is the request of the tree route.
type BrowseGitTreeInput struct {
	EnvironmentPathInput
	RepoID  string `path:"repoId" maxLength:"64" doc:"The repository's ID"`
	RefKind string `query:"refKind" required:"true" enum:"branch,tag,commit" doc:"What ref names: a branch, a tag, or a commit by its ID or a prefix of 7 to 40 hex characters"`
	Ref     string `query:"ref" required:"true" maxLength:"2048" doc:"The branch, tag or commit. 422 ref_invalid for a name git would refuse, one with control characters or '..', one starting with '-', or one over 255 bytes"`
	Path    string `query:"path" maxLength:"4096" doc:"The folder to list; the root when empty"`
}

// browseRepo reads the repository a browse route may use: one of this environment or an instance
// repository, which applies to every environment. Another environment's repository is answered
// exactly like one that does not exist.
func (d GitDeps) browseRepo(ctx context.Context, envID, id string) (gitconn.Repo, error) {
	if d.Git == nil || d.Conns == nil {
		return gitconn.Repo{}, httpx.Problem(http.StatusServiceUnavailable, problemGitUnavailable, "Git repositories are not available on this server.")
	}
	r, err := d.Conns.GetRepo(ctx, id)
	if err == nil && r.EnvID != "" && r.EnvID != envID {
		err = gitconn.ErrNotFound
	}
	if err != nil {
		return gitconn.Repo{}, d.repoFail("get repository to browse", err)
	}
	return r, nil
}

// browseProblem words a failed browse as a 422 with the gitx code as its type and the host in its
// errors; anything that is not a *gitx.Error is an internal error. what names the ref or path the
// message is about.
func (d GitDeps) browseProblem(repo gitconn.Repo, kind gitx.RefKind, ref, path string, err error) error {
	if errors.Is(err, gitx.ErrRefInvalid) {
		return httpx.Problem(http.StatusUnprocessableEntity, problemRefInvalid, "That isn't a ref name git accepts.")
	}
	var ge *gitx.Error
	if !errors.As(err, &ge) {
		return d.gitFail("browse a repository", err)
	}
	host := ge.Host
	if host == "" {
		host = repo.Host()
	}
	var detail string
	switch ge.Code {
	case gitx.CodeRefMissing:
		what := "Branch"
		if kind == gitx.RefTag {
			what = "Tag"
		}
		detail = what + " " + ref + " isn't in " + repo.Name + "."
	case gitx.CodeCommitMissing:
		detail = "Commit " + ref + " isn't in " + repo.Name + "."
	case gitx.CodeCommitAmbiguous:
		detail = "More than one commit in " + repo.Name + " starts with " + ref + ". Use more characters."
	case gitx.CodeFolderMissing:
		detail = "There is no folder " + path + " at this commit."
	case gitx.CodeAuthRefused:
		detail = host + " refused the credential."
	case gitx.CodeNotFound:
		detail = host + " has no such repository."
	case gitx.CodeCertUntrusted:
		detail = "The certificate of " + host + " isn't trusted."
	case gitx.CodeHostKeyChanged:
		detail = "The host key of " + host + " has changed."
	case gitx.CodeHostKeyUntrusted:
		detail = "The host key of " + host + " isn't trusted yet."
	case gitx.CodeEgressBlocked:
		detail = "Coaming isn't allowed to connect to " + host + "."
	case gitx.CodeTimeout:
		detail = host + " took too long to answer."
	case gitx.CodeTooLarge:
		detail = "The repository on " + host + " is too large to read."
	default:
		detail = host + " couldn't be reached."
	}
	return &huma.ErrorModel{
		Type: string(ge.Code), Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: detail, Errors: []*huma.ErrorDetail{{Location: "host", Message: host}},
	}
}

// recordUse remembers what a browse showed of the repository's credential. A failure to write it is
// logged, never the browser's problem.
func (d GitDeps) recordUse(ctx context.Context, repo gitconn.Repo, err error) {
	var code gitx.Code
	var ge *gitx.Error
	if errors.As(err, &ge) {
		code = ge.Code
	}
	if rerr := d.Conns.RecordUse(ctx, repo.CredentialID, code); rerr != nil {
		d.Log.Error("could not store the use of a git credential", "repository", repo.ID, "error", rerr)
	}
}

// listGitRefs lists every branch and tag of the repository, from the server.
func (d GitDeps) listGitRefs(ctx context.Context, envID, repoID string) (*GitRefsOutput, error) {
	repo, err := d.browseRepo(ctx, envID, repoID)
	if err != nil {
		return nil, err
	}
	rem, err := d.Conns.Remote(ctx, repo)
	if err != nil {
		return nil, d.repoFail("open the credential to list refs", err)
	}
	list, err := d.Git.LsRemote(ctx, rem)
	d.recordUse(ctx, repo, err)
	if err != nil {
		return nil, d.browseProblem(repo, "", "", "", err)
	}
	out := &GitRefsOutput{CacheControl: "no-store", Body: GitRefsBody{
		DefaultBranch: list.Default, Branches: refBodies(list.Branches), Tags: refBodies(list.Tags),
	}}
	return out, nil
}

func refBodies(refs []gitx.Ref) []GitRefBody {
	out := make([]GitRefBody, len(refs))
	for i, r := range refs {
		out[i] = GitRefBody{Name: r.Name, Commit: r.Commit}
	}
	return out
}

// browseGitTree fetches the ref into the server's mirror, then lists a folder of the commit it
// resolved to. The folder is read by that commit's ID, so a fetch that moves the branch meanwhile
// cannot change what is answered.
func (d GitDeps) browseGitTree(ctx context.Context, envID string, in *BrowseGitTreeInput) (*GitTreeOutput, error) {
	repo, err := d.browseRepo(ctx, envID, in.RepoID)
	if err != nil {
		return nil, err
	}
	kind := gitx.RefKind(in.RefKind)
	if err := gitx.ValidRefName(kind, in.Ref); err != nil {
		return nil, d.browseProblem(repo, kind, in.Ref, in.Path, err)
	}
	rem, err := d.Conns.Remote(ctx, repo)
	if err != nil {
		return nil, d.repoFail("open the credential to browse", err)
	}
	commit, err := d.Git.FetchRef(ctx, gitx.MirrorID(repo.ID), rem, kind, in.Ref)
	d.recordUse(ctx, repo, err)
	if err != nil {
		return nil, d.browseProblem(repo, kind, in.Ref, in.Path, err)
	}
	dir, err := d.Git.ReadDir(ctx, gitx.MirrorID(repo.ID), commit.SHA, in.Path, browseEntryLimit)
	if err != nil {
		return nil, d.browseProblem(repo, kind, in.Ref, in.Path, err)
	}
	entries := make([]GitTreeEntryBody, len(dir.Entries))
	for i, e := range dir.Entries {
		entries[i] = GitTreeEntryBody{Name: e.Name, Kind: string(e.Kind), HasCompose: e.HasCompose}
	}
	return &GitTreeOutput{CacheControl: "no-store", Body: GitTreeBody{
		Commit: commit.SHA, CommittedAt: commit.When, Path: dir.Path, Entries: entries, Total: dir.Total, Capped: dir.Capped,
	}}, nil
}

// registerGitBrowse registers the routes a stack creator picks a branch, tag or commit and a folder
// of a repository with.
func registerGitBrowse(r *httpx.Router, d GitDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "listGitRefs",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/git/repos/{repoId}/refs",
		Summary:     "List the branches and tags of a git repository",
		Description: "Every branch and tag of a repository usable in this environment (its own, or an instance repository), each with its commit (an annotated tag is peeled to its commit) and the default branch, " +
			"read from the git server with the repository's credential. Needs stacks.create on the environment. A repository of another environment answers 404 repository_not_found. " +
			"A failure is 422 with the code as its type (unreachable, auth_refused, not_found, cert_untrusted, host_key_changed, host_key_untrusted, egress_blocked, timeout, too_large) and the host in its errors.",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermStacksCreate), func(ctx context.Context, in *GitEnvRepoPathInput) (*GitRefsOutput, error) {
		return d.listGitRefs(ctx, in.EnvID, in.RepoID)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "browseGitTree",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/git/repos/{repoId}/tree",
		Summary:     "List a folder of a git repository at a branch, tag or commit",
		Description: "Fetches the ref into the server's bare mirror of the repository (only that ref, never every branch) and lists a folder of the commit it resolves to: the commit and its time, " +
			"and the entries, folders first and then by name, at most 500 (total and capped say when there are more). A folder with a compose file in it has hasCompose. " +
			"Needs stacks.create on the environment. A repository of another environment answers 404 repository_not_found. " +
			"422 ref_invalid for a ref name git would refuse (before any git call); 422 ref_missing, commit_missing, commit_ambiguous or folder_missing when the ref, commit or folder is not there; " +
			"otherwise 422 with the code of the failure as its type and the host in its errors, as for listing refs.",
		Tags: []string{"git"},
	}, rbac.Require(rbac.PermStacksCreate), func(ctx context.Context, in *BrowseGitTreeInput) (*GitTreeOutput, error) {
		return d.browseGitTree(ctx, in.EnvID, in)
	})
}
