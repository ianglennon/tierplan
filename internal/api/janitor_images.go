// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/ianglennon/tierplan/internal/janitor"
)

// RemoveImage implements janitor.Engine: the Images page's own removal of one image copy, as the
// janitor's strict dangling rule has it (D-07, JAN-05). The image check is read fresh (every node's
// images, and on a swarm the service specs from a manager now), then runImageOp does what the page's
// remove route does: the node's containers, running or not, are read again inside the write call,
// the image's names are read again and an image that has gained a tag or a digest, or a tierplan.keep
// label, is refused as not dangling, and force is never sent. Nothing is retried.
//
// The engine's refusal because a container or a service uses the image is ErrInUse, an image that
// is no longer dangling is ErrNoLongerStale, one that is not on the node is ErrGone, and a node that
// is not connected is ErrNodeOffline. Any other failure is a *janitor.CodedError with the Images
// API's own code.
func (e *janitorEngine) RemoveImage(ctx context.Context, envID, nodeID, imageID string) error {
	if err := e.stillNotKept(ctx, envID, janitor.KindImage, nodeID, imageID); err != nil {
		return err
	}
	d := ImagesDeps{Aggregator: e.agg, Clusters: e.clusters, Log: e.log}
	chk, err := readImageCheck(ctx, d, envID)
	if err != nil {
		return &janitor.CodedError{Code: janitor.CodeNodeUnreachable, Err: err}
	}
	out := runImageOp(ctx, d, envID, chk, imageOp{nodeID: nodeID, imageID: imageID, strictDangling: true})
	switch out.code {
	case "":
		return nil
	case codeImageInUse:
		return fmt.Errorf("%w: %s", janitor.ErrInUse, out.detail)
	case codeNotDangling:
		return fmt.Errorf("%w: %s", janitor.ErrNoLongerStale, out.detail)
	case codeNotFound:
		return fmt.Errorf("%w: %s", janitor.ErrGone, out.detail)
	case codeNodeUnreachable:
		if chk.svcErr != nil {
			// Every node is fine; no manager could say whether a service needs the image. That is a
			// failure of this removal, not a node to stop sending to.
			return &janitor.CodedError{Code: janitor.CodeNodeUnreachable, Err: errors.New(out.detail)}
		}
		return fmt.Errorf("%w: %s", janitor.ErrNodeOffline, out.detail)
	}
	return &janitor.CodedError{Code: out.code, Err: errors.New(out.detail)}
}
