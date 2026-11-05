// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

// StateNone is the state of a stack none of whose workloads can be checked: every one is pinned in its file
// or comes from no registry. It is not a verdict about any image and is never read as up to date (UI-SPEC
// "Stack aggregate", D-03).
const StateNone State = "none"

// StackVerdict is what the page says about the images of a whole stack.
type StackVerdict struct {
	// State is the one state the stack shows. StateNone when no workload can be checked.
	State State
	// Count is how many workloads have an update available, set with StateUpdateAvailable.
	Count int
	// NotCheckable are the states (pinned_in_file, not_from_registry) of the workloads that cannot be
	// checked, each once, in that order. They explain a StateNone and never change the state.
	NotCheckable []State
	// PullFailed is set with StateRolledBack when every workload that reads rolled back failed at the pull of its image: the
	// update changed nothing, so nothing was rolled back. A stack with one workload that really was put back is not
	// PullFailed.
	PullFailed bool
}

// Aggregate rolls the verdicts of a stack's workloads up into one (UI-SPEC "Stack aggregate"). Only the
// workloads that can be checked count. The first that applies answers:
//
//  1. none: no workload can be checked.
//  2. updating, busy, queued: an update is under way or waiting.
//  3. update_available, with how many.
//  4. rollback_failed, rolled_back, check_failed, not_checked. A rolled_back that is only workloads whose image could not
//     be pulled is PullFailed: nothing was changed.
//  5. When every remaining workload is skipped or ignored: ignored if any is, else skipped.
//  6. check_stale when any remaining workload is.
//  7. A mix of skipped or ignored workloads with up to date ones reads skipped or ignored as well: not
//     every checkable workload is up to date.
//  8. up_to_date, only when every checkable workload is up_to_date.
//
// A stack never reads up_to_date unless every one of its checkable workloads does.
func Aggregate(vs []Verdict) StackVerdict {
	var out StackVerdict
	var pinned, local bool
	count := map[State]int{}
	checkable, pulled := 0, 0
	for _, v := range vs {
		switch v.State {
		case StatePinned:
			pinned = true
			continue
		case StateNotFromRegistry:
			local = true
			continue
		}
		checkable++
		count[v.State]++
		if v.State == StateRolledBack && v.Failure != nil && v.Failure.Reason == ReasonPullFailed {
			pulled++
		}
	}
	if pinned {
		out.NotCheckable = append(out.NotCheckable, StatePinned)
	}
	if local {
		out.NotCheckable = append(out.NotCheckable, StateNotFromRegistry)
	}
	if checkable == 0 {
		out.State = StateNone
		return out
	}
	for _, s := range []State{
		StateUpdating, StateBusy, StateQueued, StateUpdateAvailable,
		StateRollbackFailed, StateRolledBack, StateCheckFailed, StateNotChecked,
	} {
		if count[s] > 0 {
			out.State = s
			if s == StateUpdateAvailable {
				out.Count = count[s]
			}
			out.PullFailed = s == StateRolledBack && pulled == count[s]
			return out
		}
	}
	held := count[StateSkipped] + count[StateIgnored]
	pick := StateSkipped
	if count[StateIgnored] > 0 {
		pick = StateIgnored
	}
	switch {
	case held == checkable:
		out.State = pick
	case count[StateCheckStale] > 0:
		out.State = StateCheckStale
	case held > 0:
		out.State = pick
	case count[StateUpToDate] == checkable:
		out.State = StateUpToDate
	default:
		// A state this function does not know is never read as up to date.
		out.State = StateNotChecked
	}
	return out
}

// rankOf orders the states by how much a person needs to look at them: Aggregate's order, with the states
// of a workload that cannot be checked last.
func rankOf(s State) int {
	order := []State{
		StateUpdating, StateBusy, StateQueued, StateUpdateAvailable, StateRollbackFailed, StateRolledBack,
		StateCheckFailed, StateNotChecked, StateIgnored, StateSkipped, StateCheckStale, StateUpToDate,
		StatePinned, StateNotFromRegistry,
	}
	for i, o := range order {
		if o == s {
			return i
		}
	}
	return len(order)
}
