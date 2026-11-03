// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"time"
)

// State is what the page says about a workload's image (D-03). The states are the whole vocabulary of
// the design: later plans feed the inputs that produce the ones Judge cannot reach yet. None of them is
// ever "up to date" by default: only a successful check that found the digest the workload runs is.
type State string

const (
	StateUpdateAvailable State = "update_available"
	StateUpdating        State = "updating"
	StateBusy            State = "busy"
	StateQueued          State = "queued"
	StateUpToDate        State = "up_to_date"
	StateCheckStale      State = "check_stale"
	StateCheckFailed     State = "check_failed"
	StateRolledBack      State = "rolled_back"
	StateRollbackFailed  State = "rollback_failed"
	StateNotFromRegistry State = "not_from_registry"
	StatePinned          State = "pinned_in_file"
	StateNotChecked      State = "not_checked"
	StateSkipped         State = "skipped"
	StateIgnored         State = "ignored"
)

// Reason is why a check failed, in the words of the design and never in the registry's own (RESEARCH
// Pitfall 4). It is what a row stores.
type Reason string

const (
	ReasonAuth        Reason = "auth"
	ReasonRateLimited Reason = "rate_limited"
	ReasonUnreachable Reason = "unreachable"
	ReasonNotFound    Reason = "not_found"
	ReasonUnsupported Reason = "unsupported"
	ReasonOther       Reason = "other"
)

// ReasonOf maps a code of internal/registry (registry.Classify, or the tag-detail codes) to the reason
// a check stores. A code it does not know is ReasonOther.
func ReasonOf(registryCode string) Reason {
	switch registryCode {
	case "sign_in_refused", "no_pull_access":
		return ReasonAuth
	case "rate_limited":
		return ReasonRateLimited
	case "registry_unreachable", "registry_tls", "registry_http":
		return ReasonUnreachable
	case "repository_not_found", "tag_not_found":
		return ReasonNotFound
	}
	return ReasonOther
}

// JudgeInput is everything a verdict depends on. Judge reads no clock, store or network: Now and
// StaleAfter are given.
type JudgeInput struct {
	Workload Workload
	// Check is the stored check of the workload's tag under the credential its environment resolves,
	// nil when there is none.
	Check *Check
	// Now is the time the verdict is for; StaleAfter how old the last successful check may be before an
	// unchanged digest reads check_stale.
	Now        time.Time
	StaleAfter time.Duration
}

// Verdict is what Judge decides about one workload.
type Verdict struct {
	State State
	// Reason is why the check failed, set with StateCheckFailed.
	Reason Reason
	// RunningDigest is the digest the workload runs, RemoteDigest the one the tag points at now, set
	// when a check found one.
	RunningDigest, RemoteDigest string
	// ChangedPlatforms are the platforms the workload runs on whose manifest changed (D-02).
	ChangedPlatforms []string
	// CheckedAt is when the tag was last checked, SuccessAt when a check last succeeded.
	CheckedAt, SuccessAt time.Time
	// Note is a plain-words addition for the page, empty for most states.
	Note string
}

// Judge decides what to say about a workload's image. The rules run in this order, and the first that
// applies answers:
//
//  1. The reference as the user wrote it has a digest and no tag: pinned_in_file; it can never update.
//  2. No check is stored: not_checked.
//  3. The last check failed (it is newer than the last success): check_failed with its reason. It is
//     never up to date, whatever an earlier check found.
//  4. The workload runs no digest the registry could be compared with: not_from_registry.
//  5. The tag points at another digest than the one the workload runs: update_available.
//  6. The same digest, found within StaleAfter: up_to_date; found longer ago: check_stale.
func Judge(in JudgeInput) Verdict {
	w, c := in.Workload, in.Check
	v := Verdict{State: StateNotChecked, RunningDigest: w.RunningDigest}
	if w.PinnedInFile() {
		v.State = StatePinned
		return v
	}
	if c == nil {
		return v
	}
	v.CheckedAt, v.SuccessAt = c.CheckedAt, c.SuccessAt
	if c.Error != "" && c.CheckedAt.After(c.SuccessAt) {
		v.State, v.Reason = StateCheckFailed, c.Error
		return v
	}
	if c.SuccessAt.IsZero() || c.Digest == "" {
		// A row that never had an answer and names no error: nothing to say yet.
		return v
	}
	v.RemoteDigest = c.Digest
	switch {
	case w.RunningDigest == "":
		v.State = StateNotFromRegistry
		v.Note = "The service names no digest, so there is nothing to compare the registry's answer with."
	case c.Digest != w.RunningDigest:
		v.State = StateUpdateAvailable
	case in.Now.Sub(c.SuccessAt) <= in.StaleAfter:
		v.State = StateUpToDate
	default:
		v.State = StateCheckStale
	}
	return v
}
