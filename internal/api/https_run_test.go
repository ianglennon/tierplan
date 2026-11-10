// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/api"
)

// The Let's Encrypt run actions and their stream (phase 14, ACME-04, ACME-06, ACME-07, ACME-08, ACME-10, D-09, D-14).

// acmeStepIDs are the steps of a run, in order.
var acmeStepIDs = []string{"account", "order", "challenge", "validate", "finalize", "stored"}

// unmarshalFrame decodes the data of a stream frame.
func unmarshalFrame[T any](t *testing.T, data string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("decode the frame %q: %v", data, err)
	}
	return v
}

func stepIDsOf(items []api.HTTPSACMEStepBody) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

func TestIssueACMEThroughTheAPIStreamsTheSteps(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	f.register(t, "")
	// The run waits at finalize until the stream is open, so the stream sees it in progress.
	release := f.fake.HoldFinalize()

	rec := f.send(t, http.MethodPost, "/api/https/acme/issue", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST issue: %d %s, want 202", rec.Code, rec.Body)
	}
	var started api.HTTPSACMERunStartBody
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.JobID == "" || started.Reused || started.ValidUntil != nil {
		t.Fatalf("start = %+v, want a job id and no reuse", started)
	}

	c := openSSE(t, f.authFixture, "/api/https/acme/runs/"+started.JobID+"/stream", f.cookie, "")
	if c.status0() != http.StatusOK || c.header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status = %d headers = %v", c.status0(), c.header)
	}
	snap := nextFrame(t, c)
	if snap.event != "snapshot" || snap.id != "" {
		t.Fatalf("first frame = %+v, want a snapshot without an id", snap)
	}
	first := unmarshalFrame[api.HTTPSACMERunFrameBody](t, snap.data)
	if first.JobID != started.JobID || first.Kind != "issue" || first.Trigger != "admin" || first.State != "running" || first.StartedAt.IsZero() {
		t.Errorf("snapshot = %+v, want the running admin issue", first)
	}
	if got := stepIDsOf(first.Items); !slices.Equal(got, acmeStepIDs) {
		t.Fatalf("snapshot steps = %v, want %v", got, acmeStepIDs)
	}

	release()
	var done api.HTTPSACMERunFrameBody
	for {
		fr := nextFrame(t, c)
		if fr.event == "item" {
			it := unmarshalFrame[api.HTTPSACMEStepBody](t, fr.data)
			if !slices.Contains(acmeStepIDs, it.ID) {
				t.Errorf("item frame for an unknown step: %+v", it)
			}
			continue
		}
		if fr.event == "done" {
			done = unmarshalFrame[api.HTTPSACMERunFrameBody](t, fr.data)
			break
		}
		t.Fatalf("unexpected frame %+v", fr)
	}
	c.awaitEnd(t, "the done frame")

	if got := stepIDsOf(done.Items); !slices.Equal(got, acmeStepIDs) {
		t.Fatalf("done steps = %v, want %v", got, acmeStepIDs)
	}
	for _, it := range done.Items {
		want := "done"
		if it.ID == "account" {
			want = "skipped" // registered before the run
		}
		if it.State != want {
			t.Errorf("step %s = %q, want %q", it.ID, it.State, want)
		}
	}
	// The hub ends a job with a skipped step as partial; the run is reported as what it was.
	if done.State != "done" {
		t.Errorf("done state = %q, want done", done.State)
	}

	got := f.status(t).ACME
	if got.LastRun == nil || got.LastRun.Outcome != "succeeded" || got.LastRun.JobID != started.JobID {
		t.Errorf("lastRun = %+v, want the run that succeeded", got.LastRun)
	}
	if got.Certificate == nil || got.Mode != "active" {
		t.Errorf("mode %q certificate %+v, want a stored certificate and Let's Encrypt in use", got.Mode, got.Certificate)
	}
}
