// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package audit_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
)

// phase22DetailKeys maps every ingress node and join-token action of Phase 22 to the detail keys the plans that record
// it must use (D-17, D-27, D-35). No value is ever a token, key or public key: the entries carry names, counts and
// generations only. A key containing a secret word would fail Record, so the whole table is recorded through the real
// Writer. ingress.node_enrol_refused carries the code and the mode ("enrol" or "connect"); its specific reason is the
// Entry's Reason and the caller's address its IP, and it never has an environment.
var phase22DetailKeys = map[string][]string{
	audit.ActionIngressNodeEnrolled:      {"node", "swarm_node"},
	audit.ActionIngressNodeRevoked:       {"node", "swarm_node", "reason", "held", "sample", "reenrol"},
	audit.ActionIngressJoinTokenCreated:  {"generation"},
	audit.ActionIngressJoinTokenRotated:  {"generation"},
	audit.ActionIngressJoinTokenDisabled: {"generation"},
	audit.ActionIngressNodeEnrolRefused:  {"code", "mode"},
}

func TestEveryPhase22ActionIsDefinedAndItsDetailKeysAreRecordable(t *testing.T) {
	f := newFixture(t)
	for action, keys := range phase22DetailKeys {
		details := make(map[string]string, len(keys))
		for _, k := range keys {
			details[k] = "x"
		}
		e := ent(action)
		e.Details = details
		if err := f.w.Record(context.Background(), e); err != nil {
			if errors.Is(err, audit.ErrSecretField) {
				t.Errorf("%s: a documented detail key trips the secret-word filter: %v", action, err)
			} else {
				t.Errorf("%s: Record: %v", action, err)
			}
		}
	}
	if len(phase22DetailKeys) != 6 {
		t.Fatalf("phase22DetailKeys pins %d actions, want the 6 phase-22 ingress actions", len(phase22DetailKeys))
	}
	if got := f.verify(t); got.Broken != nil || got.Entries != len(phase22DetailKeys) {
		t.Fatalf("verify = %+v", got)
	}
	for key, constant := range map[string]string{
		"ingress.node_enrolled":       audit.ActionIngressNodeEnrolled,
		"ingress.node_revoked":        audit.ActionIngressNodeRevoked,
		"ingress.join_token_created":  audit.ActionIngressJoinTokenCreated,
		"ingress.join_token_rotated":  audit.ActionIngressJoinTokenRotated,
		"ingress.join_token_disabled": audit.ActionIngressJoinTokenDisabled,
		"ingress.node_enrol_refused":  audit.ActionIngressNodeEnrolRefused,
	} {
		if key != constant {
			t.Errorf("constant = %q, want %q", constant, key)
		}
		if strings.Count(constant, ".") != 1 {
			t.Errorf("%q must have exactly one dot so the audit page finds its label", constant)
		}
	}
}
