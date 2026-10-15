// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/rbac/compose"
)

// ReviewInput is a spec to review: the service, the version the caller read, the spec they edited
// (masked unless they reveal) and who they are. Client reaches a manager; Review only reads.
type ReviewInput struct {
	EnvID      string
	Client     *dockerx.Client
	ServiceID  string
	Version    uint64
	Spec       json.RawMessage
	Caller     Caller
	HostPolicy HostPolicy
}

// ReviewResult is what a review found. Spec is the merged, normalised spec as the caller may see it:
// masked unless Caller.Reveal. Nothing was changed in Docker.
type ReviewResult struct {
	Spec       swarm.ServiceSpec
	Version    uint64
	Changed    bool
	Image      ImageInfo
	ImageError *ImageError
	Findings   []compose.Finding
	Allowed    bool
	Reason     string
	Refusal    string
	Missing    Missing
	HostPolicy HostPolicy
	Warnings   []string
}

// Review judges a spec without changing anything.
func Review(ctx context.Context, deps Deps, in ReviewInput) (ReviewResult, error) {
	return ReviewResult{}, errors.New("swarmops: not implemented")
}
