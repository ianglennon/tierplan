// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"errors"
	"time"
)

// Retention is how long an environment's run history is kept (JAN-10, D-16): a run is kept while it
// started within Days days or is among the newest Runs runs of its environment, whichever keeps more.
// A run that is waiting or running is never deleted, and a run's items and crash evidence go with it.
type Retention struct {
	Days, Runs int
}

// DefaultRetention is the retention until the policy sets another: 90 days or the newest 100 runs.
var DefaultRetention = Retention{Days: 90, Runs: 100}

// The ranges the policy accepts for a retention (UI-SPEC). The prune itself only refuses a retention
// that would keep nothing, so a test can use a small one.
const (
	MinRetentionDays = 7
	MaxRetentionDays = 3650
	MinRetentionRuns = 10
	MaxRetentionRuns = 1000
)

// InRange reports whether both numbers are inside the ranges the policy accepts.
func (r Retention) InRange() bool {
	return r.Days >= MinRetentionDays && r.Days <= MaxRetentionDays && r.Runs >= MinRetentionRuns && r.Runs <= MaxRetentionRuns
}

// ErrRetentionTooLow is returned by the prune calls for a retention of less than 1 day or 1 run: that
// would keep no history at all, which is a bug in the caller and never a request to delete it.
var ErrRetentionTooLow = errors.New("janitor: a retention keeps at least one day and one run")

// cutoff is the instant a run must have started at or after to be kept by the days alone: a run that
// started exactly Days days before now is kept, one a nanosecond older is not.
func (r Retention) cutoff(now time.Time) int64 {
	return now.UTC().Add(-time.Duration(r.Days) * 24 * time.Hour).UnixNano()
}

// lowers reports whether r keeps less history than cur: fewer days or fewer runs. Only then does saving r
// delete anything; raising either never reaches for a run it was not asked about.
func (r Retention) lowers(cur Retention) bool { return r.Days < cur.Days || r.Runs < cur.Runs }

func (r Retention) valid() error {
	if r.Days < 1 || r.Runs < 1 {
		return ErrRetentionTooLow
	}
	return nil
}
