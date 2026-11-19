// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

const (
	// stillActiveTimeout bounds the status read made as a session registers.
	stillActiveTimeout = 5 * time.Second

	// detailKeyNotAccepted is the one outward detail of every refusal of a connect that proves nothing, so a
	// probe learns neither which node IDs exist nor why a key failed. The audit reason says which it was
	// (D-27, D-35).
	detailKeyNotAccepted = "the ingress node's key was not accepted"
	// detailConnectRevoked is told only to a node that proved its key (D-06).
	detailConnectRevoked = "an administrator revoked this ingress node"
)

// ConnectRequest is a later connection of an enrolled node: its ID, the public key it presents and its
// signature over Transcript. As with enrolment, Transcript is the digest the hub rebuilt from its own nonce,
// the node's nonce, the audience, the key and the channel binding; the service never builds or trusts one
// from the node.
type ConnectRequest struct {
	NodeID               string
	Pub, Sig, Transcript []byte
	// Version is the node's supervisor version and IP the address it connected from; the hub audits refusals
	// with them.
	Version, IP string
}

// ConnectResult is a node that proved its key.
type ConnectResult struct {
	NodeID, EnvID, Name string
}

// Connect checks the reconnect of an enrolled node (D-06). The node proves the key stored on its row by
// signing the hub's transcript. The checks run cheapest and least revealing first, and everything that would
// let a probe learn something (a malformed or unknown node ID, a key that differs from the stored one, a bad
// signature) is the same auth_rejected with the same text. Only a node that holds the stored key and signs
// correctly can learn that it was revoked.
//
// Connect writes no audit row: the hub records its refusals under ingress.node_enrol_refused with the detail
// mode connect, since both handshakes are the same unauthenticated exchange behind one throttle (D-35).
func (s *Service) Connect(ctx context.Context, req ConnectRequest) (ConnectResult, error) {
	reject := func(reason string) error {
		return &RefuseError{Code: ingresswire.RefuseAuthRejected, Detail: detailKeyNotAccepted, AuditReason: reason}
	}

	if _, err := uuid.Parse(req.NodeID); err != nil {
		return ConnectResult{}, reject("malformed")
	}
	row, err := s.st.RQ.GetIngressNode(ctx, req.NodeID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The signature is checked all the same, so an unknown node ID costs what a known one does.
		_ = authn.Verify(req.Pub, req.Transcript, req.Sig)
		return ConnectResult{}, reject("unknown_node")
	case err != nil:
		return ConnectResult{}, fmt.Errorf("load the ingress node: %w", err)
	}
	if subtle.ConstantTimeCompare(row.PublicKey, req.Pub) != 1 {
		return ConnectResult{}, reject("key_mismatch")
	}
	if !authn.Verify(row.PublicKey, req.Transcript, req.Sig) {
		return ConnectResult{}, reject("bad_signature")
	}
	// The key and the signature hold, so this is the node itself and may be told.
	if row.Status != statusActive {
		return ConnectResult{}, &RefuseError{
			Code: ingresswire.RefuseRevoked, Detail: detailConnectRevoked, AuditReason: ingresswire.RefuseRevoked,
		}
	}
	return ConnectResult{NodeID: row.ID, EnvID: row.EnvID, Name: row.Name}, nil
}

// StillActive reports whether the node exists and has not been revoked. The hub asks it when it registers a
// session (D-09), after a revoke has committed.
func (s *Service) StillActive(nodeID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), stillActiveTimeout)
	defer cancel()
	row, err := s.st.RQ.GetIngressNode(ctx, nodeID)
	return err == nil && row.Status == statusActive
}
