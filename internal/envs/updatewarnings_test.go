// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The bell items of failed image updates (plan 12-44, IMG-07, D-14, UI-SPEC defaults 14).

func rolledBack(name string, at time.Time) UpdateRollback {
	return UpdateRollback{
		Kind: "service", Workload: name, WorkloadID: "svc-" + name, Digest: "sha256:" + strings.Repeat("c", 64), Reason: "unhealthy", Since: at,
	}
}

func TestUpdateRollbackWarnings(t *testing.T) {
	t.Parallel()
	env := warnEnv("env-1", "Prod", StateOnline)
	at := warnNow.Add(-time.Minute)

	t.Run("one rolled back update is one warning with name, short digest and environment", func(t *testing.T) {
		t.Parallel()
		got := UpdateRollbackWarnings(env, []UpdateRollback{rolledBack("web", at)})
		if len(got) != 1 {
			t.Fatalf("got %+v, want one item", got)
		}
		w := got[0]
		if w.ID != WarnUpdateRolledBack || w.Severity != SeverityWarning || w.EnvID != "env-1" || w.EnvName != "Prod" ||
			w.Service != "web" || w.ServiceID != "svc-web" || w.Digest != strings.Repeat("c", 12) || w.Reason != "unhealthy" || !w.Since.Equal(at) {
			t.Errorf("item = %+v", w)
		}
	})

	t.Run("a failed rollback is an error that says the workload may be partly updated", func(t *testing.T) {
		t.Parallel()
		it := rolledBack("web", at)
		it.RollbackFailed = true
		got := UpdateRollbackWarnings(env, []UpdateRollback{it})
		if len(got) != 1 || got[0].ID != WarnUpdateRollbackFailed || got[0].Severity != SeverityError || got[0].Service != "web" {
			t.Errorf("got %+v, want one update_rollback_failed error for web", got)
		}
	})

	t.Run("a container's item names the container", func(t *testing.T) {
		t.Parallel()
		it := rolledBack("db", at)
		it.Kind, it.WorkloadID = "container", "c-1"
		got := UpdateRollbackWarnings(env, []UpdateRollback{it})
		if len(got) != 1 || got[0].Container != "db" || got[0].ContainerID != "c-1" || got[0].Service != "" {
			t.Errorf("got %+v, want a container item", got)
		}
	})

	t.Run("five rolled back updates are five items and six are one with a count", func(t *testing.T) {
		t.Parallel()
		var items []UpdateRollback
		for i := 0; i < 5; i++ {
			items = append(items, rolledBack(fmt.Sprintf("s%d", i), at.Add(time.Duration(i)*time.Second)))
		}
		if got := UpdateRollbackWarnings(env, items); len(got) != 5 {
			t.Fatalf("five: got %d items, want 5", len(got))
		}
		items = append(items, rolledBack("s5", at.Add(time.Hour)))
		got := UpdateRollbackWarnings(env, items)
		if len(got) != 1 || got[0].ID != WarnUpdateRolledBack || got[0].Count != 6 || got[0].Service != "" || got[0].Digest != "" ||
			got[0].Severity != SeverityWarning || !got[0].Since.Equal(at.Add(time.Hour)) {
			t.Errorf("six: got %+v, want one item with count 6 since the newest", got)
		}
	})

	t.Run("a failed rollback is never folded into the count", func(t *testing.T) {
		t.Parallel()
		var items []UpdateRollback
		for i := 0; i < 7; i++ {
			items = append(items, rolledBack(fmt.Sprintf("s%d", i), at))
		}
		bad := rolledBack("bad", at)
		bad.RollbackFailed = true
		got := UpdateRollbackWarnings(env, append(items, bad))
		if len(got) != 2 || got[0].ID != WarnUpdateRollbackFailed || got[0].Service != "bad" || got[1].Count != 7 {
			t.Errorf("got %+v, want the error first, then one item for 7", got)
		}
	})

	t.Run("an image that could not be pulled keeps its own item and is not counted as rolled back", func(t *testing.T) {
		t.Parallel()
		var items []UpdateRollback
		for i := 0; i < 5; i++ {
			it := rolledBack(fmt.Sprintf("p%d", i), at)
			it.Reason = "pull_failed"
			items = append(items, it)
		}
		items = append(items, rolledBack("web", at))
		got := UpdateRollbackWarnings(env, items)
		if len(got) != 6 {
			t.Fatalf("got %d items, want 6, none folded", len(got))
		}
		pulls := 0
		for _, w := range got {
			if w.Reason == "pull_failed" {
				pulls++
			}
			if w.Count != 0 {
				t.Errorf("item %+v is folded", w)
			}
		}
		if pulls != 5 {
			t.Errorf("pull_failed items = %d, want 5", pulls)
		}
	})

	t.Run("names and digests from the host are cleaned", func(t *testing.T) {
		t.Parallel()
		it := rolledBack("we\x1b[31mb\nx"+strings.Repeat("y", 100), at)
		it.Digest = "sha256:ab\x00cd" + strings.Repeat("e", 100)
		got := UpdateRollbackWarnings(env, []UpdateRollback{it})
		if len(got) != 1 {
			t.Fatalf("got %+v", got)
		}
		w := got[0]
		if strings.ContainsAny(w.Service, "\x1b\n") || len([]rune(w.Service)) > maxNodeLen {
			t.Errorf("service = %q, want control characters gone and at most %d characters", w.Service, maxNodeLen)
		}
		if strings.ContainsRune(w.Digest, 0) || len(w.Digest) > maxShortDigest {
			t.Errorf("digest = %q", w.Digest)
		}
	})

	t.Run("nothing to tell is no items", func(t *testing.T) {
		t.Parallel()
		if got := UpdateRollbackWarnings(env, nil); len(got) != 0 {
			t.Errorf("got %+v, want none", got)
		}
	})
}
