// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/envs"
)

// The every-manager gate of the virtual IP (VIP-05, D-19, A-12).

func TestVIPGateRefusesWhenOneManagerRefuses(t *testing.T) {
	rows := []envs.ManagerPolicyRow{
		{NodeID: "n1", Hostname: "mgr-1", State: envs.ManagerAllows},
		{NodeID: "n2", Hostname: "mgr-2", State: envs.ManagerRefuses},
		{NodeID: "n3", Hostname: "mgr-3", State: envs.ManagerUnknown},
	}
	ok, f := EvaluateManagerGate(rows)
	if ok || f == nil {
		t.Fatalf("EvaluateManagerGate = %v, %+v, want a refusal", ok, f)
	}
	if f.Code != "vip_privileged_not_allowed" || f.Severity != SeverityError {
		t.Errorf("finding = %+v, want vip_privileged_not_allowed at error severity", f)
	}
	if !strings.Contains(f.Message, "TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true") {
		t.Errorf("message %q does not name the setting", f.Message)
	}
	want := []string{"mgr-1: allows", "mgr-2: refuses", "mgr-3: unknown"}
	if !reflect.DeepEqual(f.Items, want) {
		t.Errorf("items = %q, want %q", f.Items, want)
	}

	// The same table with every manager allowing, one of them through the local socket, passes.
	rows[1].State, rows[2].State, rows[2].LocalSocket = envs.ManagerAllows, envs.ManagerAllows, true
	if ok, f := EvaluateManagerGate(rows); !ok || f != nil {
		t.Errorf("EvaluateManagerGate over all-allow rows = %v, %+v, want ok and no finding", ok, f)
	}
}

func TestVIPGateAllUnknown(t *testing.T) {
	rows := []envs.ManagerPolicyRow{
		{NodeID: "n1", Hostname: "mgr-1", State: envs.ManagerUnknown},
		{NodeID: "n2", Hostname: "mgr-2", State: envs.ManagerUnknown},
		{NodeID: "n3", Hostname: "mgr-3", State: envs.ManagerUnknown},
	}
	ok, f := EvaluateManagerGate(rows)
	if ok || f == nil || f.Code != CodeVIPPrivilegedNotAllowed {
		t.Fatalf("EvaluateManagerGate = %v, %+v, want the refusal", ok, f)
	}
	if want := []string{"mgr-1: unknown", "mgr-2: unknown", "mgr-3: unknown"}; !reflect.DeepEqual(f.Items, want) {
		t.Errorf("items = %q, want %q", f.Items, want)
	}

	// A swarm with no readable manager list has no rows: refused too, whether nil or empty.
	for name, none := range map[string][]envs.ManagerPolicyRow{"nil": nil, "empty": {}} {
		ok, f := EvaluateManagerGate(none)
		if ok || f == nil || f.Code != CodeVIPPrivilegedNotAllowed || f.Severity != SeverityError {
			t.Errorf("%s rows: EvaluateManagerGate = %v, %+v, want the refusal", name, ok, f)
		}
	}

	// A host name is quoted back cut and without control characters; a blank one falls back to the node ID.
	_, f = EvaluateManagerGate([]envs.ManagerPolicyRow{
		{NodeID: "n1", Hostname: "bad\nname", State: envs.ManagerRefuses},
		{NodeID: "node-2", Hostname: "", State: envs.ManagerUnknown},
		{NodeID: "n3", Hostname: strings.Repeat("h", 100), State: envs.ManagerUnknown},
	})
	if want := []string{"badname: refuses", "node-2: unknown", strings.Repeat("h", 64) + ": unknown"}; !reflect.DeepEqual(f.Items, want) {
		t.Errorf("items = %q, want %q", f.Items, want)
	}
}
