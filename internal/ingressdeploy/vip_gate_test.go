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
