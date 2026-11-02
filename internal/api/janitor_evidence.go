// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// problemJanitorEvidenceNotFound is the 404 for an item that has no crash evidence.
const problemJanitorEvidenceNotFound = "janitor_evidence_not_found"

// JanitorEvidencePathInput is the path of the evidence of one item of a run.
type JanitorEvidencePathInput struct {
	JanitorRunPathInput
	Seq int `path:"seq" minimum:"1" doc:"The item's sequence number in the run, as the run lists it"`
}

// JanitorEvidenceBody is why a crashed container stopped and the tail of its log, saved before the
// janitor removed it. The log lines are masked on a best effort: a secret written in a form the
// masker does not know can still be in them.
type JanitorEvidenceBody struct {
	ExitCode    int      `json:"exitCode" doc:"The container's exit code, which is not 0"`
	Error       string   `json:"error" doc:"The engine's last start error, empty for none"`
	OOMKilled   bool     `json:"oomKilled" doc:"True when the container was killed for running out of memory"`
	StartedAt   *string  `json:"startedAt" nullable:"true" format:"date-time" doc:"When the container last started; null when the engine reported none"`
	FinishedAt  *string  `json:"finishedAt" nullable:"true" format:"date-time" doc:"When the container last stopped; null when the engine reported none"`
	Image       string   `json:"image" doc:"The image reference the container was created with"`
	ImageID     string   `json:"imageId" doc:"The ID of the image the container ran"`
	ImageDigest string   `json:"imageDigest" doc:"The repo@sha256 name of that image on the node, empty when it has none"`
	Node        string   `json:"node" doc:"The name of the node the container ran on"`
	LogLines    []string `json:"logLines" doc:"The last lines of the container's log, oldest first, with the engine's timestamp in front, masked and cut at 2048 characters a line; at most 200 lines and 32 KiB. Draw them as text"`
	LogState    string   `json:"logState" enum:"saved,empty,unavailable,cap_reached" doc:"saved: the lines are here. empty: the container wrote no log. unavailable: its log driver cannot be read back. cap_reached: the run already holds the most evidence it may, so no log was kept"`
}

// JanitorEvidenceOutput wraps JanitorEvidenceBody for huma.
type JanitorEvidenceOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JanitorEvidenceBody
}

// registerJanitorEvidence registers the read of the evidence of one item of a run.
func registerJanitorEvidence(r *httpx.Router, d JanitorDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getJanitorEvidence", Method: http.MethodGet, Path: "/api/env/{envId}/janitor/runs/{runId}/items/{seq}/evidence",
		Summary: "Read the crash evidence of a container a janitor run removed",
		Description: "Before the janitor removes a container that exited with a code other than 0, it saves why the container stopped (its exit code, error, whether it was killed for memory, when it started and stopped, " +
			"its image and the node) and the last 200 lines of its log. This answers that evidence for one item of a run. " +
			"The log lines are masked on a best effort and capped at 32 KiB, and the answer says when the log could not be read or was not kept. " +
			"An item that has no evidence, a run that does not exist or one of another environment is 404 janitor_evidence_not_found. " +
			"The evidence is the container's log, so it needs containers.read on the environment, as reading the container's logs does.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermContainersRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *JanitorEvidencePathInput) (*JanitorEvidenceOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil || d.Runs == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			ev, err := d.Runs.GetEvidence(ctx, env.ID, in.RunID, in.Seq)
			switch {
			case errors.Is(err, janitor.ErrEvidenceNotFound):
				return nil, httpx.Problem(http.StatusNotFound, problemJanitorEvidenceNotFound, "This item has no crash evidence.")
			case err != nil:
				d.log().Error("could not read janitor evidence", "environment", env.ID, "run", in.RunID, "seq", in.Seq, "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			return &JanitorEvidenceOutput{CacheControl: "no-store", Body: janitorEvidenceBody(ev)}, nil
		})
}

// janitorEvidenceBody is the evidence as the API shows it.
func janitorEvidenceBody(ev janitor.Evidence) JanitorEvidenceBody {
	b := JanitorEvidenceBody{
		ExitCode: ev.ExitCode, Error: ev.Error, OOMKilled: ev.OOMKilled, Image: ev.Image, ImageID: ev.ImageID,
		ImageDigest: ev.ImageDigest, Node: ev.Node, LogLines: ev.Lines, LogState: ev.State,
	}
	if b.LogLines == nil {
		b.LogLines = []string{}
	}
	if !ev.StartedAt.IsZero() {
		started := rfc3339(ev.StartedAt)
		b.StartedAt = &started
	}
	if !ev.FinishedAt.IsZero() {
		finished := rfc3339(ev.FinishedAt)
		b.FinishedAt = &finished
	}
	return b
}
