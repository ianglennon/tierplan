// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"regexp"
	"strconv"
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

func TestNameValidAcceptsWhatDockerNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"MyApp", "web", "my_app-2", "0abc", "A", strings.Repeat("a", 61), strings.Repeat("Z", 61)} {
		if !stacks.NameValid(stacks.TargetSwarm, name) {
			t.Errorf("NameValid(swarm, %q) = false, want true", name)
		}
	}
	for _, name := range []string{"web_1", "my-stack", "a", "0abc", strings.Repeat("a", 255)} {
		if !stacks.NameValid(stacks.TargetStandalone, name) {
			t.Errorf("NameValid(standalone, %q) = false, want true", name)
		}
	}
}

func TestNameValidRefuses(t *testing.T) {
	t.Parallel()
	both := []string{
		"", "my stack", `my"stack`, "my'stack", "a/b", `a\b`, "a\x00b", "a\nb", "a\xffb", "ünï",
		"Shop.Front", "shop.front", "a.b", ".web", "-web", "_web",
	}
	for _, target := range []stacks.Target{stacks.TargetSwarm, stacks.TargetStandalone} {
		for _, name := range both {
			if stacks.NameValid(target, name) {
				t.Errorf("NameValid(%s, %q) = true, want false", target, name)
			}
		}
	}
	for _, name := range []string{strings.Repeat("a", 62), strings.Repeat("a", 64)} {
		if stacks.NameValid(stacks.TargetSwarm, name) {
			t.Errorf("NameValid(swarm, %d bytes) = true, want false", len(name))
		}
	}
	for _, name := range []string{"Web", "MyApp", "-web", strings.Repeat("a", 256)} {
		if stacks.NameValid(stacks.TargetStandalone, name) {
			t.Errorf("NameValid(standalone, %q) = true, want false", name)
		}
	}
}

func TestSwarmObjectNameValid(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"MyApp_web", "app-_web", "a", strings.Repeat("a", 63)} {
		if !stacks.SwarmObjectNameValid(name) {
			t.Errorf("SwarmObjectNameValid(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"Shop.Front_web", "app_web.v2", "web-", "a_.b", "", "_web", strings.Repeat("a", 64)} {
		if stacks.SwarmObjectNameValid(name) {
			t.Errorf("SwarmObjectNameValid(%q) = true, want false", name)
		}
	}
}

func TestNameRuleTextMatchesTheRule(t *testing.T) {
	t.Parallel()
	for target, cap := range map[stacks.Target]int{stacks.TargetSwarm: 61, stacks.TargetStandalone: 255} {
		text := stacks.NameRuleText(target)
		if want := "up to " + strconv.Itoa(cap) + " characters"; !strings.Contains(text, want) {
			t.Errorf("NameRuleText(%s) = %q, want it to say %q", target, text, want)
		}
		if !stacks.NameValid(target, strings.Repeat("a", cap)) || stacks.NameValid(target, strings.Repeat("a", cap+1)) {
			t.Errorf("NameValid(%s) does not stop at the %d characters the text names", target, cap)
		}
	}
	if strings.Contains(stacks.NameRuleText(stacks.TargetSwarm), "lowercase") {
		t.Errorf("the swarm text says lowercase, but the swarm takes both cases")
	}
	if !strings.Contains(stacks.NameRuleText(stacks.TargetStandalone), "lowercase") {
		t.Errorf("the standalone text does not say lowercase")
	}
}
