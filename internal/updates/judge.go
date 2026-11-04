// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"slices"
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

// NoteRunningIndexGone is the Note of a workload whose running image index the registry no longer
// serves: what changed cannot be told, so it reads update_available.
const NoteRunningIndexGone = "running_index_gone"

// NoteMayBeStale is the Note of an update_available verdict whose check is older than the bound the schedule
// gives: the update was there when the tag was last asked about, and may have been followed or taken back
// since. The state is kept; the page says so (D-01).
const NoteMayBeStale = "may_be_stale"

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
	// RunPlatforms maps each platform (linux/amd64) of the index the workload runs to its manifest
	// digest, read from that index (D-02). Nil when the workload runs one image, or when the index could
	// not be read: the judgement then compares the index digests.
	RunPlatforms map[string]string
	// RunIndexGone is true when the registry no longer serves the index the workload runs.
	RunIndexGone bool
	// NextCheckAt is when the schedule will check the workload's environment next, zero when it will
	// not. Only a workload that has not been checked carries it out.
	NextCheckAt time.Time
	// Ignore is the skip or ignore that applies to the workload, from Ignores.Match, nil when none does
	// (D-15, D-16).
	Ignore *Ignore
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
	// NextCheckAt is when the schedule will check the workload next, set with StateNotChecked when the
	// environment has a schedule.
	NextCheckAt time.Time
	// Ignore is the skip or ignore the state is skipped or ignored because of, with who set it and when.
	Ignore *Ignore
}

// Judge decides what to say about a workload's image. The rules run in this order, and the first that
// applies answers:
//
//  1. The reference as the user wrote it has a digest and no tag: pinned_in_file; it can never update.
//     A container image the classic store holds with no repo digest is a local build: not_from_registry,
//     with no check (D-03).
//  2. No check is stored: not_checked.
//  3. The last check failed (it is newer than the last success): check_failed with its reason. It is
//     never up to date, whatever an earlier check found. The one exception is a container image on the
//     containerd store whose only digest is its own ID, anonymously asked and answered not_found or auth:
//     that is what a build that was never pushed does, so it is not_from_registry (D-20). A check made
//     with a credential, or failed for any other reason, is never read that way.
//  4. The workload runs no digest the registry could be compared with: not_from_registry.
//  5. The tag points at another digest than the one the workload runs: update_available, unless both
//     are multi-platform indexes and none of the platforms the workload runs on changed (D-02), which
//     reads as the same digest does. A running index the registry no longer serves is
//     update_available with the note running_index_gone.
//  6. The same digest, or none of the workload's platforms changed, found within StaleAfter:
//     up_to_date; found longer ago: check_stale.
//
// A skip or an ignore (D-15, D-16) then holds back what rules 2 to 6 found: an ignore reads ignored for any
// workload that can be checked, and a skip reads skipped when the verdict is update_available or check_failed and
// the stored remote digest is the one the skip passes over. A workload pinned in its file or not from a registry
// is never ignored or skipped: there is nothing to hide. The label, then an ignore, then a skip is the order
// Ignores.Match answers in, so exactly one applies.
//
// An update_available verdict whose check is older than StaleAfter keeps its state and gains the note
// may_be_stale, unless it already carries the note running_index_gone. A not_checked verdict carries
// NextCheckAt.
func Judge(in JudgeInput) Verdict {
	v := hold(in, judge(in))
	if v.State == StateNotChecked {
		v.NextCheckAt = in.NextCheckAt
	}
	if v.State == StateUpdateAvailable && v.Note == "" && !v.SuccessAt.IsZero() && in.Now.Sub(v.SuccessAt) > in.StaleAfter {
		v.Note = NoteMayBeStale
	}
	return v
}

// hold applies the skip or ignore of in to a verdict.
func hold(in JudgeInput, v Verdict) Verdict {
	ig := in.Ignore
	if ig == nil || v.State == StatePinned || v.State == StateNotFromRegistry {
		return v
	}
	switch ig.Mode {
	case ModeIgnore:
		v.State = StateIgnored
	case ModeSkip:
		// A skip passes over one remote digest and lifts itself when the tag points at another (D-15).
		if in.Check == nil || in.Check.Digest == "" || in.Check.Digest != ig.Digest ||
			(v.State != StateUpdateAvailable && v.State != StateCheckFailed) {
			return v
		}
		v.State, v.RemoteDigest = StateSkipped, in.Check.Digest
	default:
		return v
	}
	v.Reason, v.Note, v.ChangedPlatforms = "", "", nil
	v.Ignore = ig
	return v
}

// judge is Judge before the notes that depend on the clock and the schedule.
func judge(in JudgeInput) Verdict {
	w, c := in.Workload, in.Check
	v := Verdict{State: StateNotChecked, RunningDigest: w.RunningDigest}
	if w.PinnedInFile() {
		v.State = StatePinned
		return v
	}
	if w.LocalStore == LocalClassic {
		v.State = StateNotFromRegistry
		return v
	}
	if c == nil {
		return v
	}
	v.CheckedAt, v.SuccessAt = c.CheckedAt, c.SuccessAt
	if c.Error != "" && c.CheckedAt.After(c.SuccessAt) {
		if w.LocalStore == LocalContainerd && c.CredID == "" && (c.Error == ReasonNotFound || c.Error == ReasonAuth) {
			v.State = StateNotFromRegistry
			return v
		}
		v.State, v.Reason = StateCheckFailed, c.Error
		return v
	}
	if c.SuccessAt.IsZero() || c.Digest == "" {
		// A row that never had an answer and names no error: nothing to say yet.
		return v
	}
	v.RemoteDigest = c.Digest
	changed := false
	switch {
	case w.RunningDigest == "":
		v.State = StateNotFromRegistry
		return v
	case c.Digest != w.RunningDigest && in.RunIndexGone:
		v.State, v.Note = StateUpdateAvailable, NoteRunningIndexGone
		return v
	case c.Digest != w.RunningDigest:
		v.ChangedPlatforms, changed = changedPlatforms(w, c, in.RunPlatforms)
		if changed {
			v.State = StateUpdateAvailable
			return v
		}
	}
	if in.Now.Sub(c.SuccessAt) <= in.StaleAfter {
		v.State = StateUpToDate
	} else {
		v.State = StateCheckStale
	}
	return v
}

// changedPlatforms decides whether a tag that moved to another digest changed anything the workload
// runs. It can say no only when everything it needs is known: the stored check is an index with its
// platforms, the workload's platforms are known, and so are those of the index it runs. Then the
// workload's platforms whose manifest differs, sorted, are the change; none means no change. Anything
// less known than that is a change, with no platforms named.
func changedPlatforms(w Workload, c *Check, run map[string]string) (names []string, changed bool) {
	if !c.Index || len(c.Platforms) == 0 || len(w.Platforms) == 0 || len(run) == 0 {
		return nil, true
	}
	for _, p := range w.Platforms {
		if now := c.Platforms[p]; now == "" || now != run[p] {
			names = append(names, p)
		}
	}
	slices.Sort(names)
	return names, len(names) > 0
}
