// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

const (
	// lastSeenEvery is the shortest time between two writes of one node's last_seen_at.
	lastSeenEvery = time.Minute
	// maxBundleIDLen caps a bundle ID a node reports, which is untrusted input.
	maxBundleIDLen = 128

	healthOK        = "ok"
	healthUnhealthy = "unhealthy"
)

// forgetSeen drops a node from the once-a-minute map, so the memory of a revoked node does not stay.
func (s *Service) forgetSeen(nodeID string) {
	s.seenMu.Lock()
	delete(s.seen, nodeID)
	s.seenMu.Unlock()
}

// decodeHeld reads a held_hostnames column. It is never nil: a node that was never sent a bundle, and a
// column that cannot be read, are the empty list.
func decodeHeld(col string) []string {
	var out []string
	if err := json.Unmarshal([]byte(col), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// RecordSent adds the hostnames of a bundle's routes to the set of hostnames whose keys the node was ever
// sent (D-24). The hub calls it before a bundle frame leaves, so a revoke lists every hostname the node could
// have held, whether or not the frame arrived. The set is sorted, has no duplicates and only grows; the row is
// written only when the set changes, so a bundle that is sent again costs the single writer nothing.
func (s *Service) RecordSent(ctx context.Context, nodeID string, hostnames []string) error {
	row, err := s.st.RQ.GetIngressNode(ctx, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNodeNotFound
	} else if err != nil {
		return fmt.Errorf("load the ingress node: %w", err)
	}
	if _, grew := mergeHeld(decodeHeld(row.HeldHostnames), hostnames); !grew {
		return nil
	}

	// The set grew: merge again inside the write transaction, so two sends at once lose no name.
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	row, err = q.GetIngressNode(ctx, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNodeNotFound
	} else if err != nil {
		return fmt.Errorf("load the ingress node: %w", err)
	}
	merged, grew := mergeHeld(decodeHeld(row.HeldHostnames), hostnames)
	if !grew {
		return nil
	}
	enc, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("encode the held hostnames: %w", err)
	}
	if err := q.UpdateIngressNodeHeld(ctx, sqlc.UpdateIngressNodeHeldParams{HeldHostnames: string(enc), ID: nodeID, EnvID: row.EnvID}); err != nil {
		return fmt.Errorf("record the held hostnames: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// mergeHeld returns the sorted, de-duplicated union of held and add, and whether it has a name held did not.
func mergeHeld(held, add []string) (merged []string, grew bool) {
	base := slices.Compact(slices.Sorted(slices.Values(held)))
	merged = slices.Clone(base)
	for _, h := range add {
		if h != "" {
			merged = append(merged, h)
		}
	}
	slices.Sort(merged)
	merged = slices.Compact(merged)
	return merged, len(merged) > len(base)
}

// cleanText makes a string a node reported fit its column: valid UTF-8, no control characters, at most limit
// bytes, cut on a character boundary. A node is not trusted to send something short or printable.
func cleanText(s string, limit int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// nodeRow reads a node on the read pool. The reports name only the node, so its environment comes from the
// row, which every update is scoped by.
func (s *Service) nodeRow(ctx context.Context, nodeID string) (sqlc.IngressNode, error) {
	row, err := s.st.RQ.GetIngressNode(ctx, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.IngressNode{}, ErrNodeNotFound
	} else if err != nil {
		return sqlc.IngressNode{}, fmt.Errorf("load the ingress node: %w", err)
	}
	return row, nil
}

// RecordHello stores the version and the bundle schemas a node said hello with. Every report below reads the
// row first and writes only when a value changes (D-12, D-18): a node says the same thing at every
// connection, and the store has one writer that sign-ins share. At most MaxSchemas schemas and MaxVersionLen
// bytes of the version are kept.
func (s *Service) RecordHello(ctx context.Context, nodeID, version string, schemas []int) error {
	row, err := s.nodeRow(ctx, nodeID)
	if err != nil {
		return err
	}
	version = cleanText(version, ingresswire.MaxVersionLen)
	if len(schemas) > ingresswire.MaxSchemas {
		schemas = schemas[:ingresswire.MaxSchemas]
	}
	if schemas == nil {
		schemas = []int{}
	}
	enc, err := json.Marshal(schemas)
	if err != nil {
		return fmt.Errorf("encode the schemas: %w", err)
	}
	if row.Version == version && row.Schemas == string(enc) {
		return nil
	}
	if err := s.st.Q.UpdateIngressNodeHello(ctx, sqlc.UpdateIngressNodeHelloParams{
		Version: version, Schemas: string(enc), LastSeenAt: s.now().UTC().UnixNano(), ID: nodeID, EnvID: row.EnvID,
	}); err != nil {
		return fmt.Errorf("record the hello: %w", err)
	}
	return nil
}

// RecordApplied stores the bundle the node says it applied.
func (s *Service) RecordApplied(ctx context.Context, nodeID, bundleID string) error {
	row, err := s.nodeRow(ctx, nodeID)
	if err != nil {
		return err
	}
	bundleID = cleanText(bundleID, maxBundleIDLen)
	if row.AppliedBundleID == bundleID {
		return nil
	}
	if err := s.st.Q.UpdateIngressNodeApplied(ctx, sqlc.UpdateIngressNodeAppliedParams{
		AppliedBundleID: bundleID, AppliedAt: s.now().UTC().UnixNano(), ID: nodeID, EnvID: row.EnvID,
	}); err != nil {
		return fmt.Errorf("record the applied bundle: %w", err)
	}
	return nil
}

// RecordRejected stores the bundle the node refused and why, the reason cut to MaxReasonLen bytes.
func (s *Service) RecordRejected(ctx context.Context, nodeID, bundleID, reason string) error {
	row, err := s.nodeRow(ctx, nodeID)
	if err != nil {
		return err
	}
	bundleID = cleanText(bundleID, maxBundleIDLen)
	reason = cleanText(reason, ingresswire.MaxReasonLen)
	if row.RejectedBundleID == bundleID && row.RejectedReason == reason {
		return nil
	}
	if err := s.st.Q.UpdateIngressNodeRejected(ctx, sqlc.UpdateIngressNodeRejectedParams{
		RejectedBundleID: bundleID, RejectedReason: reason, RejectedAt: s.now().UTC().UnixNano(), ID: nodeID, EnvID: row.EnvID,
	}); err != nil {
		return fmt.Errorf("record the rejected bundle: %w", err)
	}
	return nil
}

// RecordHealth stores whether the node's Traefik is answering: ok or unhealthy.
func (s *Service) RecordHealth(ctx context.Context, nodeID string, traefikOK bool) error {
	row, err := s.nodeRow(ctx, nodeID)
	if err != nil {
		return err
	}
	health := healthUnhealthy
	if traefikOK {
		health = healthOK
	}
	if row.Health == health {
		return nil
	}
	if err := s.st.Q.UpdateIngressNodeHealth(ctx, sqlc.UpdateIngressNodeHealthParams{
		Health: health, HealthAt: s.now().UTC().UnixNano(), ID: nodeID, EnvID: row.EnvID,
	}); err != nil {
		return fmt.Errorf("record the health: %w", err)
	}
	return nil
}

// TouchLastSeen records that a node was just heard from, in ingress_nodes.last_seen_at. A node's messages
// arrive every few seconds and the store has one writer, so a write per message would starve sign-ins: the
// time is written at most once a minute per node, and the rest are dropped here, in memory. A failed write is
// forgotten, so the next message tries again.
func (s *Service) TouchLastSeen(ctx context.Context, nodeID string) {
	now := s.now()
	s.seenMu.Lock()
	if last, ok := s.seen[nodeID]; ok && now.Sub(last) < lastSeenEvery {
		s.seenMu.Unlock()
		return
	}
	s.seen[nodeID] = now
	s.seenMu.Unlock()

	row, err := s.st.RQ.GetIngressNode(ctx, nodeID)
	if err == nil {
		err = s.st.Q.TouchIngressNodeLastSeen(ctx, sqlc.TouchIngressNodeLastSeenParams{
			LastSeenAt: now.UTC().UnixNano(), ID: nodeID, EnvID: row.EnvID,
		})
	}
	if err != nil {
		s.forgetSeen(nodeID)
	}
}
