// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
)

// lockedBuffer is a log sink a test can read while the store writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// blockCheckpoints makes every WAL checkpoint of s report that another connection blocked it, and logs to the
// returned buffer.
func blockCheckpoints(s *Store) *lockedBuffer {
	buf := &lockedBuffer{}
	s.log = slog.New(slog.NewTextHandler(buf, nil))
	s.walCheckpoint = func(context.Context) (int, error) { return 1, nil }
	return buf
}

// Phase 20 review WR-07: a checkpoint that is blocked after an upload has committed does not turn the upload into a
// failure: Upload returns the stored certificate, its audit hook ran once, and the blocked checkpoint is a warning.
func TestABlockedCheckpointDoesNotFailACommittedUpload(t *testing.T) {
	c := newClockStore(t)
	seedEnv(t, c.st, "env-1", "one")
	logs := blockCheckpoints(c.Store)
	ca := newTestCA(t)

	got, replaced, err := c.upload(t, ca, "env-1", "app.example.com", uploadWorld("env-1", "dom-1"))
	if err != nil {
		t.Fatalf("Upload = %v, want the committed upload reported as done", err)
	}
	if replaced != ReplacedNone || got.ID == "" || got.Source != SourceUpload {
		t.Fatalf("Upload = %+v, replaced %q", got, replaced)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM ingress_certs WHERE id = ? AND key_sealed <> ''`, got.ID); n != 1 {
		t.Fatalf("the uploaded row with its key = %d rows, want 1", n)
	}
	if !strings.Contains(logs.String(), "not yet durable") {
		t.Errorf("no warning for the blocked checkpoint in %q", logs.String())
	}
}

// The same for an issued certificate: StoreIssued returns nil for a certificate that is stored, counted and audited
// once, and the run is not charged a failure for it.
func TestABlockedCheckpointDoesNotFailAStoredCertificate(t *testing.T) {
	ctx := context.Background()
	const host = "app.example.com"
	r := newIssueRig(t)
	c := r.pending(host)
	order, _ := r.insertOrder(c.ID, host)
	logs := blockCheckpoints(r.s)

	key, chain, _ := selfSigned(t, host)
	if err := r.s.OrderStore(c.ID).StoreIssued(ctx, order.ID, chain, key, r.clock.Now()); err != nil {
		t.Fatalf("StoreIssued = %v, want the committed certificate reported as stored", err)
	}
	if n := r.count(`SELECT COUNT(*) FROM ingress_certs WHERE id = ? AND chain_pem <> ''`, c.ID); n != 1 {
		t.Errorf("the row holds no chain")
	}
	if n := r.count(`SELECT COUNT(*) FROM ingress_cert_issuances`); n != 1 {
		t.Errorf("%d issuance rows, want 1", n)
	}
	if n := r.count(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, audit.ActionIngressCertIssued); n != 1 {
		t.Errorf("%d ingress.cert_issued rows, want 1", n)
	}
	if !strings.Contains(logs.String(), "not yet durable") {
		t.Errorf("no warning for the blocked checkpoint in %q", logs.String())
	}
}

// An order key is the exception: it must be durable before the order can be finalized, so a blocked checkpoint
// after InsertOrder is still an error and the run does not go on to use the key.
func TestABlockedCheckpointStillFailsAnOrderKey(t *testing.T) {
	const host = "app.example.com"
	r := newIssueRig(t)
	c := r.pending(host)
	blockCheckpoints(r.s)
	key, _, _ := selfSigned(t, host)
	now := r.clock.Now()
	ord := acme.Order{
		ID: "order-blocked", DirectoryURL: r.fake.DirectoryURL(), URL: "https://ca.test/order/blocked", Kind: acme.KindIssue,
		Names: []string{host}, Stage: acme.StageCreated, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	if err := r.s.OrderStore(c.ID).InsertOrder(context.Background(), ord, key); err == nil {
		t.Fatalf("InsertOrder = nil with the checkpoint blocked, want an error")
	}
}
