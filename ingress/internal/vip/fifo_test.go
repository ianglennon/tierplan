// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"strings"
	"testing"
)

func TestParseFIFOLine(t *testing.T) {
	cases := []struct {
		line  string
		state string
		ok    bool
	}{
		{`INSTANCE "TIERPLAN_VIP" MASTER 100`, "MASTER", true},
		{`INSTANCE "TIERPLAN_VIP" BACKUP 100`, "BACKUP", true},
		{`INSTANCE "TIERPLAN_VIP" FAULT 100`, "FAULT", true},
		{`INSTANCE "TIERPLAN_VIP" STOP 0`, "STOP", true},
		{`INSTANCE "TIERPLAN_VIP"  MASTER   100`, "MASTER", true},
		{`INSTANCE "OTHER" MASTER 100`, "", false},
		{`GROUP "TIERPLAN_VIP" MASTER 100`, "", false},
		{`INSTANCE TIERPLAN_VIP MASTER 100`, "", false},
		{`INSTANCE "TIERPLAN_VIP" PAUSED 100`, "", false},
		{`INSTANCE "TIERPLAN_VIP" master 100`, "", false},
		{`INSTANCE "TIERPLAN_VIP" MASTER`, "", false},
		{`INSTANCE "TIERPLAN_VIP" MASTER high`, "", false},
		{`INSTANCE "TIERPLAN_VIP" MASTER 100 extra`, "", false},
		{"malformed", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		state, ok := parseFIFOLine(tc.line)
		if state != tc.state || ok != tc.ok {
			t.Errorf("parseFIFOLine(%q) = %q, %v; want %q, %v", tc.line, state, ok, tc.state, tc.ok)
		}
	}
}

func TestReadLinesBoundsALine(t *testing.T) {
	in := "first\n" + strings.Repeat("x", 5000) + "\nsecond\nno newline at the end"
	var got []string
	readLines(strings.NewReader(in), 64, func(l string) { got = append(got, l) })
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("lines = %q, want first and second (the overlong line dropped whole, the unterminated tail ignored)", got)
	}
}
