// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/backup"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/vault"
)

// A backup that holds a Let's Encrypt account, restored through TierPlan's own restore, serves the
// restored certificate, reports the restore pause and makes no request to the CA until an admin
// resumes it; after the resume the next due renewal happens (ACME-08, D-20).
func TestARestoreWithLetsEncryptPausesACMEUntilResume(t *testing.T) {
	ctx := context.Background()
	r := newACMERig(t, acmetest.Options{}, nil)
	dirA := r.cfg.DataDir

	// Instance A issues Let's Encrypt and exports a backup.
	a := r.startNew()
	r.issue()
	issued := leafOf(t, a.https.Config())
	var file bytes.Buffer
	if _, err := backup.Export(ctx, a.Store(), a.Vault(), dirA, &file, []byte(restoreBackupPass),
		backup.ExportOptions{AppVersion: "v0.1.1", InstanceName: "A"}); err != nil {
		t.Fatal(err)
	}
	keyA := a.Vault().KeyID()
	r.stop()

	// B is a new data directory with a master key of its own; the restore goes through Prepare and the swap.
	dirB := t.TempDir() + "/data"
	if err := os.MkdirAll(dirB, 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := vault.LoadOrCreate("", dirB, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if target.KeyID() == keyA {
		t.Fatal("the new data directory generated the old master key")
	}
	schema, err := store.TargetSchema()
	if err != nil {
		t.Fatal(err)
	}
	staged, err := backup.Stage(ctx, bytes.NewReader(file.Bytes()), []byte(restoreBackupPass), dirB, schema)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer staged.Discard()
	if err := staged.Prepare(ctx, target, backup.PrepareOptions{Source: "cli", AppVersion: "v0.1.1"}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := staged.Swap(ctx, dirB, time.Now().UTC()); err != nil {
		t.Fatalf("Swap: %v", err)
	}

	// B is built on the same fake directory. It serves A's certificate, paused.
	r.setEnv(map[string]string{"TIERPLAN_DATA_DIR": dirB})
	asked := r.counts()
	b := r.startNew()
	if got := leafOf(t, b.https.Config()); !bytes.Equal(got.Raw, issued.Raw) || b.https.Kind() != tlsKindACME {
		t.Error("the restored install does not serve the Let's Encrypt certificate that was in the backup")
	}
	status, err := b.acme.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.PausedReason != acme.PausedRestore {
		t.Fatalf("PausedReason = %q, want %q", status.PausedReason, acme.PausedRestore)
	}

	// The first round after Resume asks the CA for renewal information (none is asked while paused)
	// and draws the renewal instant at random inside the window it gets (RFC 9773). Here the CA's
	// window closes at the renewal point, so once that has passed every draw is in the past and the
	// renewal is due on that round. The fake's default window, two thirds of the lifetime a day
	// either side, would put the draw after the clock about half the time.
	r.fake.SetRenewalWindow(issued, status.RenewAt.Add(-24*time.Hour), status.RenewAt, 0)

	// The renewal point passes, and the paused install asks the CA nothing, not even for its directory.
	r.step(status.RenewAt.Sub(r.clock.Now()) + time.Hour)
	r.awaitRuns()
	if got := r.counts(); !reflect.DeepEqual(got, asked) {
		t.Errorf("the CA was asked while paused after a restore: before %v, after %v", asked, got)
	}
	if got := len(r.fake.Orders()); got != 1 {
		t.Errorf("the CA has seen %d orders, want only A's", got)
	}

	// After Resume the next round renews.
	if err := b.acme.Resume(ctx, "test"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	r.step(acme.Tick)
	r.awaitRuns()
	if got := r.fake.Count(acmetest.EndpointNewOrder); got != asked[acmetest.EndpointNewOrder]+1 {
		t.Errorf("newOrder was asked %d times, want %d: one renewal after the resume", got, asked[acmetest.EndpointNewOrder]+1)
	}
	if renewed := leafOf(t, b.https.Config()); renewed.SerialNumber.Cmp(issued.SerialNumber) == 0 {
		t.Error("the renewal after the resume did not replace the certificate")
	}
	if status, err = b.acme.Status(ctx); err != nil || status.PausedReason != "" {
		t.Errorf("after Resume PausedReason = %q (%v), want none", status.PausedReason, err)
	}
}
