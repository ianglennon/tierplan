// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"strings"
	"time"
)

// maxRolledBackItems is how many rolled-back updates of one environment get an item each; more are told as one (UI-SPEC defaults 14).
const maxRolledBackItems = 5

// maxShortDigest is how many characters of a digest the bell shows, after the algorithm.
const maxShortDigest = 12

// reasonPullFailed is the reason of an update whose image could not be pulled: nothing was changed, so nothing was rolled back.
const reasonPullFailed = "pull_failed"

// UpdateRollback is an image update that failed and still applies: the digest that failed is the one the tag points at.
type UpdateRollback struct {
	// Kind is "service" or "container"; Workload is its name and WorkloadID its ID.
	Kind, Workload, WorkloadID string
	// Digest is the remote digest the update failed with; Reason why it failed.
	Digest, Reason string
	// RollbackFailed is true when putting the previous image back failed too.
	RollbackFailed bool
	// Since is when the update failed.
	Since time.Time
}

// UpdateRollbackWarnings are the bell items of the failed image updates of one environment: a warning for each update that failed
// and was put back, an error for each whose rollback failed (the workload may be partly updated), and one warning for the count when
// more than five were put back. An update that failed only because its image could not be pulled changed nothing; it keeps its own
// item, carries reason pull_failed and is not counted with the rest. The caller passes only the items the reader may see and only
// those that still apply, so an item goes when its cause does and nothing is stored to dismiss.
func UpdateRollbackWarnings(e Environment, items []UpdateRollback) []EnvWarning {
	var out []EnvWarning
	var rolled []UpdateRollback
	for _, it := range items {
		if it.RollbackFailed || it.Reason == reasonPullFailed {
			out = append(out, updateWarning(e, it))
			continue
		}
		rolled = append(rolled, it)
	}
	if len(rolled) <= maxRolledBackItems {
		for _, it := range rolled {
			out = append(out, updateWarning(e, it))
		}
	} else {
		w := EnvWarning{ID: WarnUpdateRolledBack, Severity: SeverityWarning, EnvID: e.ID, EnvName: cleanText(e.Name, maxNodeLen), Count: len(rolled)}
		for _, it := range rolled {
			if it.Since.After(w.Since) {
				w.Since = it.Since
			}
		}
		out = append(out, w)
	}
	SortWarnings(out)
	return out
}

// updateWarning is the item of one failed update.
func updateWarning(e Environment, it UpdateRollback) EnvWarning {
	w := EnvWarning{
		ID: WarnUpdateRolledBack, Severity: SeverityWarning, EnvID: e.ID, EnvName: cleanText(e.Name, maxNodeLen),
		Reason: cleanText(it.Reason, maxNodeLen), Digest: shortDigest(it.Digest), Since: it.Since,
	}
	if it.RollbackFailed {
		w.ID, w.Severity = WarnUpdateRollbackFailed, SeverityError
	}
	name, id := cleanText(it.Workload, maxNodeLen), cleanText(it.WorkloadID, maxNodeLen)
	if it.Kind == "container" {
		w.Container, w.ContainerID = name, id
	} else {
		w.Service, w.ServiceID = name, id
	}
	return w
}

// shortDigest is a digest without its algorithm, cut to twelve characters, as the pages show one. The text comes from a registry
// and is cleaned.
func shortDigest(d string) string {
	if _, rest, ok := strings.Cut(d, ":"); ok {
		d = rest
	}
	return cleanText(d, maxShortDigest)
}
