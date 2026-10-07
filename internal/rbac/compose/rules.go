// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package compose

import (
	"strings"

	"go.yaml.in/yaml/v3"
)

// ruleUnresolvable is the rule reported for a checked value that holds an
// interpolation, which cannot be verified before the variables are known.
const (
	ruleUnresolvable = "unresolvable"
	whyUnresolvable  = "contains an interpolation that cannot be verified, so it is treated as a host-escalation feature"
)

// leaf is one checked value: its text as written and where to report it. key
// is only set by the top-level volume rule, which names a sub-key.
type leaf struct {
	text      string
	line, col int
	key       string
}

// rule is one row of the host-escalation table. Key is the service attribute
// it reads; Leaves extracts the values to check from that attribute; Bad says
// whether one value is a host-escalation feature. A leaf holding an
// interpolation is reported as "unresolvable" before Bad is consulted.
type rule struct {
	Name   string
	Key    string
	Why    string
	Leaves func(c *checker, v val) []leaf
	Bad    func(text string) bool
}

// serviceRules is the table of service attributes that escalate to the host
// (D-13). Rules run in this order; findings are re-sorted by position.
var serviceRules = []rule{
	{
		Name:   "privileged",
		Key:    "privileged",
		Why:    "a privileged container has every capability and every host device",
		Leaves: scalarLeaves,
		Bad:    func(t string) bool { return !clearlyFalse(t) },
	},
}

// volumeRule reads a top-level named volume (Key is unused for it).
var volumeRule = rule{
	Name:   "volume_driver_opts",
	Why:    "the local volume driver turns these options into a bind mount of a host path",
	Leaves: func(*checker, val) []leaf { return nil },
	Bad:    func(string) bool { return false },
}

// at builds a leaf for a node at the position the walk reports for it.
func at(v val, text string) leaf {
	return leaf{text: text, line: v.line(), col: v.column()}
}

// scalarLeaves checks a single-valued attribute. A null or absent value has no
// leaf; a mapping or sequence where a scalar belongs is one leaf with a
// placeholder text, so a rule that fails closed still sees it.
func scalarLeaves(c *checker, v val) []leaf {
	v = c.resolve(v)
	switch v.n.Kind {
	case yaml.ScalarNode:
		if v.n.ShortTag() == "!!null" {
			return nil
		}
		return []leaf{at(v, v.n.Value)}
	case yaml.MappingNode:
		return []leaf{at(v, "<mapping>")}
	case yaml.SequenceNode:
		return []leaf{at(v, "<sequence>")}
	}
	return nil
}

// clearlyFalse reports whether a boolean field is a literal false. YAML 1.1 and
// 1.2 disagree about yes/no/on/off, so only the spellings of false that both
// agree are not "set" are accepted; anything else counts as set.
func clearlyFalse(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "false", "no", "off", "n", "0":
		return true
	}
	return false
}

// interpolated reports whether a value holds a compose interpolation: ${...}
// or a bare $NAME. It over-matches the escaped $$ forms on purpose.
func interpolated(s string) bool {
	if strings.Contains(s, "${") {
		return true
	}
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '$' {
			continue
		}
		n := s[i+1]
		if n == '_' || (n >= 'a' && n <= 'z') || (n >= 'A' && n <= 'Z') {
			return true
		}
	}
	return false
}
