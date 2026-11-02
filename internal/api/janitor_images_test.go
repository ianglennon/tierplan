// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// The janitor's images (plan 11-10, JAN-05, D-07). A fake daemon behind the real monitor and
// aggregator; the janitor judges images by its strict dangling rule and removes them by the Images
// page's own code.

// imageIDOf is a full image ID built from one hex digit.
func imageIDOf(digit string) string { return "sha256:" + strings.Repeat(digit, 64) }

// janitorImageEngine is a standalone host holding the given images and nothing else.
func janitorImageEngine(images ...dockertest.FakeImage) dockertest.FakeConfig {
	cfg := orphanEngine()
	cfg.Images = images
	return cfg
}

// imageAge is the Created of an image that is ago old at the fixture's clock.
func imageAge(ago time.Duration) int64 { return janitorNow.Add(-ago).Unix() }

// The tracer: an untagged, undigested image past the grace period that no container uses is in the
// preview, and Clean now removes it through the Images page's code, once and without force.
func TestJanitorRemovesDanglingImage(t *testing.T) {
	t.Parallel()
	id := imageIDOf("a")
	f, e := dashEnv(t, janitorImageEngine(dockertest.FakeImage{ID: id, Size: 4096, Created: imageAge(72 * time.Hour)}))
	preview := f.previewJanitor(t, e)
	if len(preview.Items) != 1 || preview.Summary.Images != 1 {
		t.Fatalf("preview items = %+v summary = %+v, want the one dangling image", preview.Items, preview.Summary)
	}
	it := preview.Items[0]
	if it.Type != "image" || it.Ref != id || it.Class != "removable" || it.Reason.Code != "dangling_image" || it.SizeBytes == nil || *it.SizeBytes != 4096 {
		t.Fatalf("item = %+v, want the image removable dangling_image holding 4096 bytes", it)
	}
	if preview.Summary.EstBytes == nil || *preview.Summary.EstBytes != 4096 {
		t.Errorf("estimate = %v, want 4096", preview.Summary.EstBytes)
	}

	job, _ := f.cleanNow(t, e, it)
	if job.State != jobs.JobDone || len(job.Items) != 1 || job.Items[0].State != jobs.ItemDone || job.Items[0].Kind != "image" {
		t.Fatalf("job = %s items %+v, want a done job with one done image", job.State, job.Items)
	}
	deletes := f.fake.Writes()
	if len(deletes) != 1 || !strings.HasPrefix(deletes[0], "DELETE /images/"+id) || strings.Contains(deletes[0], "force") {
		t.Fatalf("writes = %v, want exactly one DELETE of the image, with no force", deletes)
	}
	if after := f.previewJanitor(t, e); len(after.Items) != 0 {
		t.Errorf("preview after = %+v, want the image gone", after.Items)
	}
}
