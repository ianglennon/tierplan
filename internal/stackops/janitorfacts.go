// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"fmt"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// ExtensionKey is the top-level compose key a stack uses to opt out of cleanup: x-tierplan: {keep: true}
// excludes the whole stack from the janitor (D-10). It is read from the definition, so a stack from a git
// repository carries the choice in its repository. A service-level tierplan.keep label is not this: it protects
// only the object that carries it (D-19).
const ExtensionKey = "x-tierplan"

// StackExclusion is what a stack's own definition says about the janitor.
type StackExclusion struct {
	ID, Name string
	// Declared is true when the compose file says x-tierplan: {keep: true}.
	Declared bool
	// Unknown is true when the definition could not be read, so nothing it owns can be judged. A stack that
	// is Unknown is never Declared.
	Unknown bool
}

// JanitorFacts is what TierPlan's stacks say about themselves for the janitor, read through the loader NetFacts
// uses (stacks.Check), with the same deadline and the same memo, so an unchanged stack is parsed once whatever the
// number of readers (T-11-74).
type JanitorFacts struct {
	Repo Repo
	// Check parses a stack; nil means stacks.Check. Tests count its calls.
	Check func(context.Context, stacks.CheckInput) stacks.CheckResult
}

// Configured reports whether the facts can be read: a JanitorFacts with no store reads nothing, and the janitor
// then judges without a stack's own word.
func (j JanitorFacts) Configured() bool { return j.Repo.Store != nil }

// Exclusions reads the current definition of every stack of the environment (the draft, or the last good
// revision when it has none, as NetFacts does) and answers each stack's word on the janitor. A stack whose
// definition cannot be read is Unknown and never Declared. A read of the list of stacks that fails, or one cut
// short by ctx, is an error and not a guess.
func (j JanitorFacts) Exclusions(ctx context.Context, envID string) ([]StackExclusion, error) {
	all, err := j.Repo.Stacks(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("read the stacks of the environment: %w", err)
	}
	out := make([]StackExclusion, 0, len(all))
	for _, s := range all {
		ex, err := j.ExclusionOf(ctx, s)
		if err != nil {
			return nil, err
		}
		out = append(out, ex)
	}
	return out, nil
}

// ExclusionOf is Exclusions for the one stack s: its current definition, read the way Exclusions reads each. Only a
// read cut short by ctx is an error; a definition that cannot be read is an answer (Unknown).
func (j JanitorFacts) ExclusionOf(ctx context.Context, s Stack) (StackExclusion, error) {
	ex := StackExclusion{ID: s.ID, Name: s.Name}
	snap, err := j.Repo.Draft(ctx, s.ID)
	if err != nil {
		if ctx.Err() != nil {
			return StackExclusion{}, ctx.Err()
		}
		ex.Unknown = true
		return ex, nil
	}
	res, ok := loadFacts(ctx, j.Check, s, snap)
	if ctx.Err() != nil {
		return StackExclusion{}, ctx.Err()
	}
	if !ok {
		ex.Unknown = true
		return ex, nil
	}
	ex.Declared = res.declared
	return ex, nil
}

// declaresKeep reports whether the top-level extensions say x-tierplan: {keep: true}. Only the YAML boolean true
// or the exact string "true" declares it: a missing key, {} , false, "True", 1 or any other value does not, so a
// stack is never excluded by a misreading (Pitfall 9, T-11-83).
func declaresKeep(ext map[string]any) bool {
	return keepValue(ext[ExtensionKey])
}

// keepValue reads the keep key of an x-tierplan value, whichever map type the YAML decoder gave it.
func keepValue(v any) bool {
	var keep any
	switch m := v.(type) {
	case map[string]any:
		keep = m["keep"]
	case map[any]any:
		keep = m["keep"]
	default:
		return false
	}
	switch k := keep.(type) {
	case bool:
		return k
	case string:
		return k == "true"
	}
	return false
}
