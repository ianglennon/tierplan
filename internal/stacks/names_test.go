// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// agentObjectName is the rule the agent holds a secret or config name to
// (agent/internal/proxy/body.go secretNameRe).
var agentObjectName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,62}[A-Za-z0-9])?$`)

var hex8 = regexp.MustCompile(`^[0-9a-f]{8}$`)

func TestObjectNameIsStackNameAndEightHex(t *testing.T) {
	t.Parallel()
	full, hash := stacks.ObjectName("web", "tok", []byte("secret"))
	if !hex8.MatchString(hash) {
		t.Fatalf("hash = %q, want 8 lowercase hex digits", hash)
	}
	if want := "web_tok_" + hash; full != want {
		t.Errorf("name = %q, want %q", full, want)
	}
	if !agentObjectName.MatchString(full) {
		t.Errorf("name %q does not match the agent's object-name rule", full)
	}
}

func TestObjectNameFitsSixtyFourCharactersKeepsTheHashAndTheAgentRule(t *testing.T) {
	t.Parallel()
	stack := strings.Repeat("s", 63)
	name := strings.Repeat("n", 60)
	full, hash := stacks.ObjectName(stack, name, []byte("x"))
	if len(full) != 64 {
		t.Errorf("len(name) = %d, want 64", len(full))
	}
	if !strings.HasSuffix(full, "_"+hash) {
		t.Errorf("name %q does not end in _%s", full, hash)
	}
	if !agentObjectName.MatchString(full) {
		t.Errorf("name %q does not match the agent's object-name rule", full)
	}
}

func TestObjectNameDropsSeparatorsTheCutLeavesAtTheEnd(t *testing.T) {
	t.Parallel()
	// 55 characters of room for "<stack>_<name>": put a run of separators across the cut.
	stack := strings.Repeat("s", 40)
	name := strings.Repeat("a", 13) + "-._-." + strings.Repeat("b", 20) // the cut falls inside the run
	full, hash := stacks.ObjectName(stack, name, []byte("x"))
	prefix := strings.TrimSuffix(full, "_"+hash)
	if last := prefix[len(prefix)-1]; last == '_' || last == '.' || last == '-' {
		t.Errorf("name %q ends its prefix with %q before the hash", full, last)
	}
	if len(full) > 64 || !agentObjectName.MatchString(full) {
		t.Errorf("name %q (len %d) breaks the agent's rule", full, len(full))
	}
}

func TestObjectNameIsDeterministicAndFollowsTheContentAndTheName(t *testing.T) {
	t.Parallel()
	a1, _ := stacks.ObjectName("web", "tok", []byte("one"))
	a2, _ := stacks.ObjectName("web", "tok", []byte("one"))
	b, _ := stacks.ObjectName("web", "tok", []byte("two"))
	c, _ := stacks.ObjectName("web", "other", []byte("one"))
	if a1 != a2 {
		t.Errorf("the same stack, name and content gave %q and %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("different content gave the same name %q", a1)
	}
	if a1 == c {
		t.Errorf("a different object name gave the same name %q", a1)
	}
}

func TestObjectHashSeparatesNameFromContentWithANUL(t *testing.T) {
	t.Parallel()
	if stacks.ObjectHash("ab", []byte("c")) == stacks.ObjectHash("a", []byte("bc")) {
		t.Error(`("ab", "c") and ("a", "bc") hash alike: the name and the content are not kept apart`)
	}
}
