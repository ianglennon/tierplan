// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
)

// The HTTPS status carries the clock offset exactly while the bell's clock condition holds, in any ACME
// mode (phase 17, D-07, D-29). Every CA response is a measurement, the injected clock stands still, and
// nothing here waits.
func TestHTTPSStatusClockOffset(t *testing.T) {
	t.Parallel()

	t.Run("clock off while Let's Encrypt is in use", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{fake: acmetest.Options{DateOffset: -10 * time.Minute}})
		f.issued(t)
		got := f.status(t).ACME
		if got.Mode != "active" {
			t.Fatalf("mode = %q, want active", got.Mode)
		}
		if got.ClockOffsetSeconds == nil || *got.ClockOffsetSeconds != 600 {
			t.Errorf("clockOffsetSeconds = %v, want 600 (TierPlan ahead)", got.ClockOffsetSeconds)
		}
	})

	t.Run("clock behind the CA's is negative", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{fake: acmetest.Options{DateOffset: 3 * time.Hour}})
		f.issued(t)
		got := f.status(t).ACME
		if got.ClockOffsetSeconds == nil || *got.ClockOffsetSeconds != -10800 {
			t.Errorf("clockOffsetSeconds = %v, want -10800 (TierPlan behind)", got.ClockOffsetSeconds)
		}
	})

	t.Run("clock in step leaves the field out", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.issued(t)
		rec := f.get(t, "/api/https/status")
		got := f.status(t).ACME
		if got.ClockOffsetSeconds != nil {
			t.Errorf("clockOffsetSeconds = %d, want it absent", *got.ClockOffsetSeconds)
		}
		var raw struct {
			ACME map[string]json.RawMessage `json:"acme"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw.ACME["clockOffsetSeconds"]; ok {
			t.Errorf("the status body names clockOffsetSeconds with the clock in step: %s", rec.Body)
		}
	})

	t.Run("no sample leaves the field out", func(t *testing.T) {
		t.Parallel()
		// The CA's clock is off, but no call has read a CA response yet and Let's Encrypt is off.
		f := newACMEFix(t, acmeOpts{fake: acmetest.Options{DateOffset: -10 * time.Minute}})
		got := f.status(t).ACME
		if got.Mode != "off" {
			t.Fatalf("mode = %q, want off", got.Mode)
		}
		if got.ClockOffsetSeconds != nil {
			t.Errorf("clockOffsetSeconds = %d, want it absent with no sample", *got.ClockOffsetSeconds)
		}
	})

	t.Run("a failed first Issue while Let's Encrypt is off carries the error and the offset", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{fake: acmetest.Options{DateOffset: -10 * time.Minute}})
		f.register(t, "")
		f.fake.FailValidation("tierplan.example.com", acmetest.Problem{Type: "connection", Detail: "refused"})
		f.awaitRun(t, startRun(t, f.issue(t)))

		got := f.status(t).ACME
		if got.Mode != "off" || got.Certificate != nil {
			t.Fatalf("mode %q certificate %+v, want Let's Encrypt off and no certificate", got.Mode, got.Certificate)
		}
		if got.LastError == nil {
			t.Fatalf("the status has no lastError after a failed first Issue: %+v", got)
		}
		if got.ClockOffsetSeconds == nil || *got.ClockOffsetSeconds != 600 {
			t.Errorf("clockOffsetSeconds = %v, want 600 beside the error", got.ClockOffsetSeconds)
		}
	})
}
