// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"maps"
	"net/netip"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

func goodEnv() map[string]string {
	return map[string]string{
		ingressbundle.EnvVIPAddress:   "192.168.1.50/24",
		ingressbundle.EnvVIPRouterID:  "77",
		ingressbundle.EnvVIPProbePort: "7483",
		ingressbundle.EnvVIPKeyFile:   "/etc/tierplan-keepalived/key",
		ingressbundle.EnvVIPNodeID:    "abcdefghijklmnopqrstuvwxy",
		ingressbundle.EnvVIPNodeName:  "node-a",
	}
}

func getenv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoad(t *testing.T) {
	t.Run("a good environment loads", func(t *testing.T) {
		got, err := Load(getenv(goodEnv()))
		if err != nil {
			t.Fatal(err)
		}
		if got != goldenConfig() {
			t.Errorf("Load = %+v, want %+v", got, goldenConfig())
		}
		if got.Address != netip.MustParsePrefix("192.168.1.50/24") {
			t.Errorf("the address lost its host bits: %v", got.Address)
		}
	})

	t.Run("each variable missing in turn is named", func(t *testing.T) {
		for name := range goodEnv() {
			m := goodEnv()
			delete(m, name)
			_, err := Load(getenv(m))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("without %s: err = %v, want one naming it", name, err)
			}
		}
	})

	// secret is a value no error may repeat.
	const secret = "s3cr3t-value"
	bad := []struct {
		name, variable, value string
	}{
		{"an IPv6 address", ingressbundle.EnvVIPAddress, "fd00::1/64"},
		{"an IPv4-mapped IPv6 address", ingressbundle.EnvVIPAddress, "::ffff:192.168.1.50/120"},
		{"an address without a prefix", ingressbundle.EnvVIPAddress, "192.168.1.50"},
		{"an address that is text", ingressbundle.EnvVIPAddress, secret},
		{"router ID 0", ingressbundle.EnvVIPRouterID, "0"},
		{"router ID 256", ingressbundle.EnvVIPRouterID, "256"},
		{"a negative router ID", ingressbundle.EnvVIPRouterID, "-1"},
		{"a router ID that is text", ingressbundle.EnvVIPRouterID, secret},
		{"port 0", ingressbundle.EnvVIPProbePort, "0"},
		{"port 65536", ingressbundle.EnvVIPProbePort, "65536"},
		{"a port that is text", ingressbundle.EnvVIPProbePort, secret},
		{"a relative key file", ingressbundle.EnvVIPKeyFile, "run/secrets/" + secret},
		{"a key file with a line break that would add configuration", ingressbundle.EnvVIPKeyFile, "/k\n}\nvrrp_instance X {" + secret},
		{"a key file with a space", ingressbundle.EnvVIPKeyFile, "/a b/" + secret},
		{"a key file with a comment mark", ingressbundle.EnvVIPKeyFile, "/a#" + secret},
		{"a node ID of the wrong shape", ingressbundle.EnvVIPNodeID, "NOT-A-NODE-" + secret},
		{"a node name with a control character", ingressbundle.EnvVIPNodeName, "node\x1b[31m" + secret},
		{"a node name over 64 characters", ingressbundle.EnvVIPNodeName, strings.Repeat("n", 65) + secret},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			m := goodEnv()
			m[tc.variable] = tc.value
			_, err := Load(getenv(m))
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.variable) {
				t.Errorf("error %q does not name %s", err, tc.variable)
			}
			if strings.Contains(err.Error(), tc.value) || strings.Contains(err.Error(), secret) {
				t.Errorf("error %q holds the value given", err)
			}
		})
	}

	t.Run("the edges are accepted", func(t *testing.T) {
		for _, tc := range []struct{ variable, value string }{
			{ingressbundle.EnvVIPRouterID, "1"}, {ingressbundle.EnvVIPRouterID, "255"},
			{ingressbundle.EnvVIPProbePort, "1"}, {ingressbundle.EnvVIPProbePort, "65535"},
			{ingressbundle.EnvVIPNodeName, strings.Repeat("n", 64)},
		} {
			m := maps.Clone(goodEnv())
			m[tc.variable] = tc.value
			if _, err := Load(getenv(m)); err != nil {
				t.Errorf("%s=%q: %v", tc.variable, tc.value, err)
			}
		}
	})

	t.Run("Load never opens the key file", func(t *testing.T) {
		m := goodEnv()
		m[ingressbundle.EnvVIPKeyFile] = "/does/not/exist/key"
		if _, err := Load(getenv(m)); err != nil {
			t.Errorf("a key file that does not exist must load: %v", err)
		}
	})
}
