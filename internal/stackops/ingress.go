// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// Ingress is what the stack pipeline asks of the ingress model (D-18). It is one optional field of Deps, so the
// stack packages know nothing of how ingress is stored or switched: the api package adapts the ingress store to it.
type Ingress interface {
	// AttachFor says how the services of a stack that declare a route join the shared ingress network. It is non-nil
	// only for a swarm target of an environment whose ingress is switched on (D-13, D-19); nil means nothing is
	// attached, so a deploy with ingress off never names a network that does not exist. A read that fails is an
	// error, never a nil answer: a start or a roll back must not silently drop a routed service off the network.
	AttachFor(ctx context.Context, envID string, target stacks.Target) (*stacks.IngressAttach, error)
	// World reads the ingress world of an environment (domains, reserved names, claims), naming another
	// environment's holder only where canView allows.
	World(ctx context.Context, envID string, canView func(string) bool) (ingress.World, error)
	// ReplaceStackRoutes makes the claims of a stack what its declarations say. It records its own audit row
	// (ingress.claim_replaced) in the transaction of the change.
	ReplaceStackRoutes(ctx context.Context, in ingressstore.ReplaceInput) (ingress.Plan, error)
	// ReleaseStack lets go of every hostname a stack holds. It records its own audit row
	// (ingress.claim_released) in the transaction of the change.
	ReleaseStack(ctx context.Context, envID, stackID, actor string) error
	// Notify wakes the reconciler of the environment, for a change the store did not make itself (a stack deleted,
	// whose claims went with its row).
	Notify(envID string)
	// HeldRoutes are the routes the stack holds now, with their hostnames: the "before" of a review (D-22). A stack
	// that holds nothing has none.
	HeldRoutes(ctx context.Context, envID, stackID string) ([]ingress.Route, error)
	// CertStates says, for each hostname, whether the environment can serve a certificate for it now, has none yet or
	// is waiting on one (D-22, D-40). It answers the state and a reason only, never the certificate or its key. A
	// hostname it cannot say anything about is left out of the answer.
	CertStates(ctx context.Context, envID string, hostnames []string) (map[string]CertState, error)
}

// The states of a hostname's certificate in a review.
const (
	// CertReused: a certificate that can be served exists, so the deploy reuses it.
	CertReused = "reused"
	// CertWillIssue: the environment has no certificate for the hostname yet, so one will be ordered.
	CertWillIssue = "will_issue"
	// CertWaiting: there is a certificate order for the hostname and none can be served yet; Reason says why.
	CertWaiting = "waiting"
)

// CertState is the state of the certificate of one hostname. Reason is set for CertWaiting only.
type CertState struct {
	State  string
	Reason string
}

// attachFor is the attachment a conversion of st's project for target carries: nil, nil when no Ingress is wired.
func (d Deps) attachFor(ctx context.Context, envID string, t stacks.Target) (*stacks.IngressAttach, error) {
	if d.Ingress == nil {
		return nil, nil
	}
	return d.Ingress.AttachFor(ctx, envID, t)
}

// ClaimWrite is how a deploy puts a stack's hostname claims in the ingress store (D-15, D-16, D-17). Nil on a
// deploy input means no claim is written and none is reverted: a start, the automatic roll back after a failed
// image update, an adopt and a rebuild leave the claims as they are.
type ClaimWrite struct {
	// Decls are the routes of the revision being deployed.
	Decls []ingress.Declaration
	// Restore are the routes of the stack's last good revision. HasGood false means the stack has no good revision
	// (a first deploy): a failed deploy then releases every claim.
	Restore []ingress.Declaration
	HasGood bool
	// Actor names who the write is audited as: a user id, or "system:..." for a deploy nobody started.
	Actor string
	// CanView says which environments the caller may see, for the message of a refusal that names a holder; nil
	// names none.
	CanView func(envID string) bool
}
