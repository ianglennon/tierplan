// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// MaxRevisionsListed is the most revisions ListRevisions answers. A stack keeps far fewer once retention
// runs (default 20); the cap keeps a stack whose retention is off, or very large, from filling a response.
const MaxRevisionsListed = 500

// ImageChange is one service whose image reference differs from the previous good revision's. From is empty
// for a service the previous good revision did not have.
type ImageChange struct {
	Service, From, To string
}

// RevisionRow is one revision of a stack as the History tab lists it.
type RevisionRow struct {
	Seq int64
	// Origin is how the revision came to be (OriginDeploy and the others); RestoredFrom is the number of
	// the revision a roll back copied, and 0 for any other.
	Origin       string
	RestoredFrom int64
	// Outcome is OutcomePending, OutcomeGood or OutcomeFailed.
	Outcome, CreatedBy string
	CreatedAt          time.Time
	// FailedService and Code say where and why the deploy of a failed revision stopped; both are empty
	// for a revision that did not fail, or whose failure left no code.
	FailedService, Code string
	// Deployed marks the newest good revision: the one the stack stands at.
	Deployed bool
	// Images are the services whose pinned image differs from the next older good revision's, by service
	// name. Pinned is how many images a revision pinned when it has no older good revision to compare
	// with (the first revision of a stack, or an adopted one), and 0 otherwise.
	Images []ImageChange
	Pinned int
}

// ListRevisions lists the revisions of a stack, newest first by seq. Seq is unique per stack, so the order
// is total and the same on every read. It answers at most MaxRevisionsListed rows; capped says there were
// more. It only reads: a revision is never rewritten by being listed.
func (r Repo) ListRevisions(ctx context.Context, stackID string) (rows []RevisionRow, capped bool, err error) {
	got, err := r.Store.RQ.ListRevisions(ctx, sqlc.ListRevisionsParams{StackID: stackID, Limit: MaxRevisionsListed + 1})
	if err != nil {
		return nil, false, fmt.Errorf("list revisions: %w", err)
	}
	pins := make([]map[string]string, len(got))
	for i, g := range got {
		if err := json.Unmarshal([]byte(g.Pins), &pins[i]); err != nil {
			return nil, false, fmt.Errorf("read the pins of revision %d: %w", g.Seq, err)
		}
	}
	// The older good revision of each row is found before the extra row that detects the cap is dropped,
	// so the oldest row listed still compares with it.
	out := make([]RevisionRow, 0, len(got))
	deployed := false
	for i, g := range got {
		row := RevisionRow{
			Seq: g.Seq, Origin: g.Origin, RestoredFrom: g.RestoredFrom, Outcome: g.Outcome, CreatedBy: g.CreatedBy,
			CreatedAt: time.Unix(0, g.CreatedAt).UTC(), FailedService: g.FailedService, Code: g.Code,
		}
		if g.Outcome == OutcomeGood && !deployed {
			row.Deployed, deployed = true, true
		}
		base := -1
		for j := i + 1; j < len(got); j++ {
			if got[j].Outcome == OutcomeGood {
				base = j
				break
			}
		}
		if base < 0 {
			row.Pinned = len(pins[i])
		} else {
			row.Images = imageChanges(pins[base], pins[i])
		}
		out = append(out, row)
	}
	if len(out) > MaxRevisionsListed {
		return out[:MaxRevisionsListed], true, nil
	}
	return out, false, nil
}

// imageChanges lists, by service name, the services of now whose pinned reference differs from before's. A
// service before did not have has an empty From.
func imageChanges(before, now map[string]string) []ImageChange {
	var out []ImageChange
	for service, ref := range now {
		if was := before[service]; was != ref {
			out = append(out, ImageChange{Service: service, From: was, To: ref})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}
