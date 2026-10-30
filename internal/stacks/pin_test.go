// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stacks_test

import (
	"testing"

	"github.com/ianglennon/coaming/internal/stacks"
)

func TestDecidePin(t *testing.T) {
	t.Parallel()
	running := &stacks.LivePin{AsWritten: "nginx:1.27", Image: "nginx:1.27@sha256:aaa"}
	lastGood := &stacks.LivePin{AsWritten: "nginx:1.27", Image: "nginx:1.27@sha256:bbb"}
	cases := []struct {
		name string
		in   stacks.PinInput
		want stacks.PinDecision
	}{
		{"a new service resolves", stacks.PinInput{AsWritten: "nginx:1.27"}, stacks.PinDecision{Action: stacks.PinResolve}},
		{"a changed text resolves", stacks.PinInput{AsWritten: "nginx:1.28", Live: running}, stacks.PinDecision{Action: stacks.PinResolve}},
		{"an unchanged text keeps the running image", stacks.PinInput{AsWritten: "nginx:1.27", Live: running},
			stacks.PinDecision{Action: stacks.PinKeep, Ref: "nginx:1.27@sha256:aaa"}},
		{"a digest is used as written", stacks.PinInput{AsWritten: "nginx@sha256:ccc", Live: running},
			stacks.PinDecision{Action: stacks.PinAsWritten, Ref: "nginx@sha256:ccc"}},
		{"a tag and a digest is used as written", stacks.PinInput{AsWritten: "nginx:1.27@sha256:ccc"},
			stacks.PinDecision{Action: stacks.PinAsWritten, Ref: "nginx:1.27@sha256:ccc"}},
		{"an update resolves even when unchanged", stacks.PinInput{AsWritten: "nginx:1.27", Live: running, Update: true},
			stacks.PinDecision{Action: stacks.PinResolve}},
		{"an update of a digest still uses it as written", stacks.PinInput{AsWritten: "nginx@sha256:ccc", Update: true},
			stacks.PinDecision{Action: stacks.PinAsWritten, Ref: "nginx@sha256:ccc"}},
		{"a service the engine lacks keeps what the last good revision ran", stacks.PinInput{AsWritten: "nginx:1.27", Stored: lastGood},
			stacks.PinDecision{Action: stacks.PinKeep, Ref: "nginx:1.27@sha256:bbb"}},
		{"the stored pin is not used for a changed text", stacks.PinInput{AsWritten: "nginx:1.28", Stored: lastGood},
			stacks.PinDecision{Action: stacks.PinResolve}},
		{"the stored pin is not used while the service runs", stacks.PinInput{AsWritten: "nginx:1.27", Live: &stacks.LivePin{AsWritten: "nginx:1.26", Image: "nginx:1.26@sha256:ddd"}, Stored: lastGood},
			stacks.PinDecision{Action: stacks.PinResolve}},
		{"a running service with no record of its text resolves", stacks.PinInput{AsWritten: "nginx:1.27", Live: &stacks.LivePin{Image: "nginx:1.27"}},
			stacks.PinDecision{Action: stacks.PinResolve}},
		{"an update of a service the engine lacks resolves", stacks.PinInput{AsWritten: "nginx:1.27", Stored: lastGood, Update: true},
			stacks.PinDecision{Action: stacks.PinResolve}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stacks.DecidePin(tc.in); got != tc.want {
				t.Errorf("DecidePin(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestHasDigest(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string]bool{
		"nginx":                         false,
		"nginx:1.27":                    false,
		"registry:5000/app:1":           false,
		"nginx@sha256:abc":              true,
		"nginx:1.27@sha256:abc":         true,
		"registry:5000/app@sha256:abcd": true,
		"":                              false,
	} {
		if got := stacks.HasDigest(ref); got != want {
			t.Errorf("HasDigest(%q) = %v, want %v", ref, got, want)
		}
	}
}
