// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops

import (
	"context"
	"encoding/json"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/rbac/compose"
	"github.com/ianglennon/bayplan/internal/redact"
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

// Review judges a spec without changing anything. It reads the service fresh and refuses a version
// that is not the current one, merges the masked values, resolves the image, runs the host-escalation
// policy on the exact spec an apply would send and lists the networks the spec names that do not exist.
// A refusal by the policy is not an error: it is Allowed false with the findings, so a page can show
// what to change. Everything else a spec can be refused for is returned as an error (see Apply).
func Review(ctx context.Context, deps Deps, in ReviewInput) (ReviewResult, error) {
	c := common{envID: in.EnvID, serviceID: in.ServiceID, version: in.Version, spec: in.Spec, caller: in.Caller, client: in.Client}
	p, err := prepare(ctx, deps, c)
	if err != nil {
		return ReviewResult{}, err
	}
	return p.review(ctx, deps, c, in.HostPolicy)
}

// review resolves the image, runs the policy and answers what an apply or a create would do, without
// changing anything.
func (p *prepared) review(ctx context.Context, deps Deps, c common, hp HostPolicy) (ReviewResult, error) {
	info, _, unresolved, err := p.pinImage(ctx, deps, c)
	if err != nil {
		return ReviewResult{}, err
	}
	dec, _, err := p.judge(ctx, deps, c)
	if err != nil {
		return ReviewResult{}, err
	}
	shown, _ := redact.MaskServiceSpec(p.merged, c.caller.Reveal)
	res := ReviewResult{
		Spec: shown, Version: p.detail.Version, Changed: p.changed, Image: info,
		Findings: dec.Findings, Allowed: dec.Allowed, Reason: dec.Reason, Refusal: dec.Message,
		Missing: missingObjects(p.merged, p.have), HostPolicy: hp, Warnings: []string{},
	}
	if res.Findings == nil {
		res.Findings = []compose.Finding{}
	}
	if unresolved != nil {
		res.ImageError = &ImageError{Ref: unresolved.Ref, Host: unresolved.Host, Reason: unresolved.Reason}
	}
	return res, nil
}
