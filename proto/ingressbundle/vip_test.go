// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const testNode = "abcdefghij0123456789abcde" // 25 lower-case alphanumerics, the shape of a swarm node ID

func TestVIPEnvNames(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"EnvVIPAddress", EnvVIPAddress, "TIERPLAN_VIP_ADDRESS"},
		{"EnvVIPRouterID", EnvVIPRouterID, "TIERPLAN_VIP_ROUTER_ID"},
		{"EnvVIPProbePort", EnvVIPProbePort, "TIERPLAN_VIP_PROBE_PORT"},
		{"EnvVIPKeyFile", EnvVIPKeyFile, "TIERPLAN_VIP_KEY_FILE"},
		{"EnvVIPNodeID", EnvVIPNodeID, "TIERPLAN_VIP_NODE_ID"},
		{"EnvVIPNodeName", EnvVIPNodeName, "TIERPLAN_VIP_NODE_NAME"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if VIPAuthWindow.Seconds() != 30 {
		t.Errorf("VIPAuthWindow = %s, want 30s (A-04)", VIPAuthWindow)
	}
	if VIPProbeTimeout.Milliseconds() != 800 {
		t.Errorf("VIPProbeTimeout = %s, want 800ms (D-12)", VIPProbeTimeout)
	}
}

func TestReadyPublishedPortIsDistinct(t *testing.T) {
	if ReadyPublishedPort != 7483 {
		t.Fatalf("ReadyPublishedPort = %d, want 7483 (D-13)", ReadyPublishedPort)
	}
	for name, other := range map[string]int{"80": 80, "443": 443, "ReadyPort": ReadyPort, "PingPort": PingPort} {
		if ReadyPublishedPort == other {
			t.Errorf("ReadyPublishedPort equals %s", name)
		}
	}
	if ReadyPublishedPort >= 32768 {
		t.Errorf("ReadyPublishedPort = %d, want below the ephemeral range (32768)", ReadyPublishedPort)
	}
}

func TestVIPEventRoundTrip(t *testing.T) {
	events := []VIPEvent{
		{V: 1, Event: VIPEventStart, Node: testNode, Name: "node-1", Router: 51, Seq: 1, Iface: "eth0", Stale: true},
		{V: 1, Event: VIPEventState, State: VIPStateMaster, Node: testNode, Name: "node-1", Router: 51, Seq: 2},
		{V: 1, Event: VIPEventState, State: VIPStateFault, Reason: VIPReasonProbeFailing, Node: testNode, Router: 51, Seq: 3},
		{V: 1, Event: VIPEventFault, Reason: VIPReasonAmbiguousInterface, Node: testNode, Router: 51, Seq: 4, Ifaces: []string{"eth0", "eth1.20", "bond_0"}},
		{V: 1, Event: VIPEventFault, Reason: VIPReasonAddressInUse, Node: testNode, Seq: 5},
		{V: 1, Event: VIPEventForeignRouter, Node: testNode, Router: 51, Seq: 6, Counter: VIPCounterInvalidHMAC, Addr: "192.168.1.9", Active: true},
		{V: 1, Event: VIPEventForeignRouter, Node: testNode, Router: 51, Seq: 7, Counter: VIPCounterUnknownKey, Addr: "fe80::1"},
		{V: 1, Event: VIPEventClockSkew, Node: testNode, Router: 51, Seq: 8, Active: true},
	}
	for _, want := range events {
		t.Run(want.Event, func(t *testing.T) {
			line, err := want.MarshalLine()
			if err != nil {
				t.Fatalf("MarshalLine: %v", err)
			}
			if bytes.ContainsAny(line, "\r\n") {
				t.Fatalf("line %q has a line break inside", line)
			}
			if len(line) > VIPLineMax {
				t.Fatalf("line is %d bytes, over the cap %d", len(line), VIPLineMax)
			}
			got, err := ParseVIPLine(line)
			if err != nil {
				t.Fatalf("ParseVIPLine(%q): %v", line, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip = %+v, want %+v", got, want)
			}
			// The reader of a log hands over a line with its newline.
			if _, err := ParseVIPLine(append(line, '\n')); err != nil {
				t.Fatalf("ParseVIPLine with a trailing newline: %v", err)
			}
		})
	}
}

func TestVIPEventMarshalSetsTheVersion(t *testing.T) {
	line, err := VIPEvent{Event: VIPEventState, State: VIPStateBackup, Node: testNode, Seq: 1}.MarshalLine()
	if err != nil {
		t.Fatalf("MarshalLine: %v", err)
	}
	got, err := ParseVIPLine(line)
	if err != nil || got.V != VIPLineVersion {
		t.Fatalf("ParseVIPLine = %+v, %v, want V %d", got, err, VIPLineVersion)
	}
}

func TestVIPEventMarshalRefusesWhatTheParserRefuses(t *testing.T) {
	for name, e := range map[string]VIPEvent{
		"unknown event":    {V: 1, Event: "reboot", Node: testNode, Seq: 1},
		"unknown state":    {V: 1, Event: VIPEventState, State: "master", Node: testNode, Seq: 1},
		"short node":       {V: 1, Event: VIPEventStart, Node: "abc", Seq: 1},
		"bad addr":         {V: 1, Event: VIPEventForeignRouter, Node: testNode, Addr: "999.1.1.1", Seq: 1},
		"version 2":        {V: 2, Event: VIPEventStart, Node: testNode, Seq: 1},
		"router above 255": {V: 1, Event: VIPEventStart, Node: testNode, Router: 256, Seq: 1},
	} {
		if _, err := e.MarshalLine(); !errors.Is(err, ErrVIPLine) {
			t.Errorf("%s: MarshalLine err = %v, want ErrVIPLine", name, err)
		}
	}
}

func TestParseVIPLine(t *testing.T) {
	valid := `{"v":1,"event":"state","state":"MASTER","node":"` + testNode + `","seq":1}`
	if _, err := ParseVIPLine([]byte(valid)); err != nil {
		t.Fatalf("the valid base line is refused: %v", err)
	}

	// SECRET is a marker: no refusal may repeat any of the input.
	const secret = "SECRETTEXT"
	node := func(n string) string { return `"node":"` + n + `"` }
	mk := func(fields ...string) string { return "{" + strings.Join(fields, ",") + "}" }
	base := []string{`"v":1`, `"event":"state"`, `"state":"MASTER"`, node(testNode), `"seq":1`}
	with := func(replace map[int]string, extra ...string) string {
		f := append([]string(nil), base...)
		for i, v := range replace {
			f[i] = v
		}
		return mk(append(f, extra...)...)
	}

	manyIfaces := make([]string, 9)
	for i := range manyIfaces {
		manyIfaces[i] = `"eth` + string(rune('0'+i)) + `"`
	}

	cases := []struct{ name, line string }{
		{"over-long line", valid + strings.Repeat(" ", VIPLineMax)},
		{"not json", secret},
		{"empty", ""},
		{"unknown event", with(map[int]string{1: `"event":"` + secret + `"`})},
		{"unknown field", with(nil, `"`+secret+`":1`)},
		{"state outside the four", with(map[int]string{2: `"state":"master"`})},
		{"state event without a state", mk(`"v":1`, `"event":"state"`, node(testNode), `"seq":1`)},
		{"reason outside the list", with(nil, `"reason":"`+secret+`"`)},
		{"fault without a reason", mk(`"v":1`, `"event":"fault"`, node(testNode), `"seq":1`)},
		{"counter outside the list", with(nil, `"counter":"`+secret+`"`)},
		{"version 2", with(map[int]string{0: `"v":2`})},
		{"version missing", mk(`"event":"state"`, `"state":"MASTER"`, node(testNode), `"seq":1`)},
		{"node with upper case", with(map[int]string{3: node("ABCDEFGHIJ0123456789ABCDE")})},
		{"node too short", with(map[int]string{3: node("abc" + secret)})},
		{"node too long", with(map[int]string{3: node(testNode + "x")})},
		{"node missing", mk(`"v":1`, `"event":"state"`, `"state":"MASTER"`, `"seq":1`)},
		{"name of 65 characters", with(nil, `"name":"`+strings.Repeat("n", 65)+`"`)},
		{"name with a control character", with(nil, `"name":"a\u001b[2Jb"`)},
		{"addr 999.1.1.1", with(nil, `"addr":"999.1.1.1"`)},
		{"addr with text", with(nil, `"addr":"`+secret+`"`)},
		{"nine interface names", with(nil, `"ifaces":[`+strings.Join(manyIfaces, ",")+`]`)},
		{"interface name with a space", with(nil, `"ifaces":["eth 0"]`)},
		{"interface name of 16 characters", with(nil, `"iface":"`+strings.Repeat("e", 16)+`"`)},
		{"empty interface name", with(nil, `"ifaces":[""]`)},
		{"interface name with a slash", with(nil, `"iface":"../`+secret+`"`)},
		{"router 256", with(nil, `"router":256`)},
		{"router negative", with(nil, `"router":-1`)},
		{"seq negative", with(map[int]string{4: `"seq":-1`})},
		{"trailing data", valid + `{}`},
		{"line break inside", "{\"v\":1,\n\"event\":\"state\",\"state\":\"MASTER\"," + node(testNode) + ",\"seq\":1}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseVIPLine([]byte(tc.line))
			if err == nil {
				t.Fatalf("ParseVIPLine accepted %.120q", tc.line)
			}
			if !errors.Is(err, ErrVIPLine) {
				t.Fatalf("error %q does not wrap ErrVIPLine", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), testNode) {
				t.Fatalf("error %q repeats the input", err)
			}
		})
	}
}

func TestParseVIPLineAcceptsTheBounds(t *testing.T) {
	name := strings.Repeat("n", 64)
	ifaces := make([]string, 8)
	for i := range ifaces {
		ifaces[i] = "eth" + strings.Repeat("x", 12) // 15 characters, the Linux limit
	}
	line, err := VIPEvent{V: 1, Event: VIPEventFault, Reason: VIPReasonNoInterface, Node: testNode, Name: name, Router: 255, Seq: 1 << 40, Ifaces: ifaces}.MarshalLine()
	if err != nil {
		t.Fatalf("MarshalLine at the bounds: %v", err)
	}
	if len(line) > VIPLineMax {
		t.Fatalf("a line at every bound is %d bytes, over VIPLineMax %d", len(line), VIPLineMax)
	}
	if _, err := ParseVIPLine(line); err != nil {
		t.Fatalf("ParseVIPLine at the bounds: %v", err)
	}
}
