// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/moby/moby/api/types/swarm"
)

// The task history retention limit a swarm update may set: the range the UI offers. The engine also
// takes -1 (never remove a finished task), which TierPlan does not offer.
const (
	minTaskHistoryLimit = 0
	maxTaskHistoryLimit = 1000
)

// errSwarmLookup is the engine not answering the lookup of its own swarm spec. Like
// errContainerLookup it is an engine fault, not a policy refusal: the handler answers 502 and
// forwards nothing, so an engine that is down is never read as permission.
var errSwarmLookup = errors.New("the engine did not answer a swarm lookup")

// swarmLookup reads the swarm spec of the agent's own engine. manager is false when the engine is
// not a swarm manager or is not in a swarm, in which case spec is empty. A non-nil err is an engine
// fault.
type swarmLookup func(ctx context.Context) (spec swarm.Spec, manager bool, err error)

// maxInfoAnswer caps what is read of /info, whose swarm part is read for the spec only.
const maxInfoAnswer = 8 << 20

// swarmLookupOver asks the engine behind transport for its swarm spec: one GET /info, read for
// Swarm.ControlAvailable and Swarm.Cluster.Spec. It is not GET /swarm, whose answer carries the join
// tokens and which the agent never forwards; /info carries the cluster without them. A status other
// than 200, a timeout or an answer that cannot be read is an error.
func swarmLookupOver(transport http.RoundTripper) swarmLookup {
	client := &http.Client{Transport: transport}
	return func(ctx context.Context) (swarm.Spec, bool, error) {
		ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		// G704: the URL is fixed and the host is the placeholder; the transport dials the engine's
		// unix socket whatever the URL says.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/info", nil) //nolint:gosec // G704, see above
		if err != nil {
			return swarm.Spec{}, false, err
		}
		resp, err := client.Do(req) //nolint:gosec // G704, see above
		if err != nil {
			return swarm.Spec{}, false, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return swarm.Spec{}, false, fmt.Errorf("swarm lookup answered %d", resp.StatusCode)
		}
		var info struct {
			Swarm swarm.Info
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxInfoAnswer)).Decode(&info); err != nil {
			return swarm.Spec{}, false, err
		}
		if !info.Swarm.ControlAvailable || info.Swarm.Cluster == nil {
			return swarm.Spec{}, false, nil
		}
		return info.Swarm.Cluster.Spec, true, nil
	}
}

// validateSwarmUpdate judges the body of a swarm update and returns the spec to forward in its place.
// TierPlan changes one thing about a swarm through an agent: the task history retention limit. So the
// body is decoded into the engine's own swarm.Spec with unknown fields refused, the limit must be
// present and within the range, and the spec the agent's own engine reports is read. The body must
// equal that spec in every field but the limit; any other difference (a CA setting, the raft or
// dispatcher settings, the swarm's name or labels, the task defaults, auto-lock) is a refusal. What is
// returned is the engine's own spec with only the limit replaced, so the bytes the server sent never
// reach the engine. An engine that does not answer is an error wrapping errSwarmLookup, and a node
// that is not a manager, or not in a swarm, is a refusal; neither is ever read as permission.
func validateSwarmUpdate(raw []byte, env bodyEnv) (swarm.Spec, error) {
	var in swarm.Spec
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	limit := in.Orchestration.TaskHistoryRetentionLimit
	switch {
	case limit == nil:
		return in, refuse("the task history retention limit is absent")
	case *limit < minTaskHistoryLimit || *limit > maxTaskHistoryLimit:
		return in, refuse("the task history retention limit is outside %d to %d", minTaskHistoryLimit, maxTaskHistoryLimit)
	}
	if env.swarm == nil {
		return in, refuse("no swarm lookup")
	}
	ctx := env.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	current, manager, err := env.swarm(ctx)
	if err != nil {
		return in, fmt.Errorf("%w: %w", errSwarmLookup, err)
	}
	if !manager {
		return in, refuse("the engine is not a swarm manager")
	}
	// The engine's own spec with the one field replaced. The limit is copied so the forwarded value
	// shares nothing with the body.
	want := current
	v := *limit
	want.Orchestration.TaskHistoryRetentionLimit = &v
	submitted, err := json.Marshal(sameSwarmSpec(in))
	if err != nil {
		return in, refuse("encoding the submitted spec: %v", err)
	}
	expected, err := json.Marshal(sameSwarmSpec(want))
	if err != nil {
		return in, refuse("encoding the engine's spec: %v", err)
	}
	if !bytes.Equal(submitted, expected) {
		return in, refuse("the spec differs from the engine's own in more than the task history retention limit")
	}
	return want, nil
}

// sameSwarmSpec returns spec in the one form two equal specs share when encoded. The engine reports
// the labels of a swarm with no label as an empty map or as null and the encoder writes the two
// differently, though they say the same thing, so an empty map is made null on both sides.
func sameSwarmSpec(spec swarm.Spec) swarm.Spec {
	if len(spec.Labels) == 0 {
		spec.Labels = nil
	}
	return spec
}
