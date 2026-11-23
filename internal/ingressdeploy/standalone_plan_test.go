// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// TestStandalonePlanTable: when the proxy's project needs an up and when the up needs a fresh join token (D-26).
func TestStandalonePlanTable(t *testing.T) {
	const image = "ghcr.io/example/ingress:1.2.3"
	current := ingresshub.TokenStatus{Exists: true, ID: "tok-2", Generation: 2}
	disabled := ingresshub.TokenStatus{Exists: true, ID: "tok-2", Generation: 2, Disabled: true}
	// good is a host that holds everything as wanted, with the proxy created with the current token.
	good := func() standaloneLive {
		return standaloneLive{
			proxy:   &dockerx.ComposeContainer{State: "running", Labels: map[string]string{stacks.LabelStackImage: image, TokenLabel: "tok-2"}},
			network: &dockerx.NetworkRow{Name: "tierplan-ingress"}, volume: true,
		}
	}
	for _, tc := range []struct {
		name      string
		mutate    func(*standaloneLive)
		accessLog bool
		enrolled  bool
		tok       ingresshub.TokenStatus
		want      standalonePlan
	}{
		{name: "everything as wanted, enrolled", enrolled: true, tok: current, want: standalonePlan{}},
		{name: "everything as wanted, not enrolled, current token label", tok: current, want: standalonePlan{}},
		{name: "container missing", mutate: func(l *standaloneLive) { l.proxy = nil }, tok: current,
			want: standalonePlan{Up: true, Token: true, Reason: "missing_container"}},
		{name: "container missing, enrolled", mutate: func(l *standaloneLive) { l.proxy = nil }, enrolled: true, tok: current,
			want: standalonePlan{Up: true, Reason: "missing_container"}},
		{name: "network missing", mutate: func(l *standaloneLive) { l.network = nil }, tok: current,
			want: standalonePlan{Up: true, Token: true, Reason: "missing_network"}},
		{name: "network missing, enrolled", mutate: func(l *standaloneLive) { l.network = nil }, enrolled: true, tok: current,
			want: standalonePlan{Up: true, Reason: "missing_network"}},
		{name: "exited", mutate: func(l *standaloneLive) { l.proxy.State = "exited" }, enrolled: true, tok: current,
			want: standalonePlan{Up: true, Reason: "not_running"}},
		{name: "created but never started", mutate: func(l *standaloneLive) { l.proxy.State = "created" }, tok: current,
			want: standalonePlan{Up: true, Token: true, Reason: "not_running"}},
		{name: "restarting counts as running", mutate: func(l *standaloneLive) { l.proxy.State = "restarting" }, enrolled: true, tok: current, want: standalonePlan{}},
		{name: "image label old", mutate: func(l *standaloneLive) { l.proxy.Labels[stacks.LabelStackImage] = "ghcr.io/example/ingress:1.2.2" }, tok: current,
			want: standalonePlan{Up: true, Token: true, Reason: "image"}},
		{name: "image label old, enrolled", mutate: func(l *standaloneLive) { l.proxy.Labels[stacks.LabelStackImage] = "ghcr.io/example/ingress:1.2.2" }, enrolled: true, tok: current,
			want: standalonePlan{Up: true, Reason: "image"}},
		{name: "access log switched on", accessLog: true, tok: current, want: standalonePlan{Up: true, Token: true, Reason: "access_log"}},
		{name: "access log switched on, enrolled", accessLog: true, enrolled: true, tok: current, want: standalonePlan{Up: true, Reason: "access_log"}},
		{name: "access log switched off", mutate: func(l *standaloneLive) { l.accessLog = true }, enrolled: true, tok: current,
			want: standalonePlan{Up: true, Reason: "access_log"}},
		{name: "not enrolled, superseded token label", mutate: func(l *standaloneLive) { l.proxy.Labels[TokenLabel] = "tok-1" }, tok: current,
			want: standalonePlan{Up: true, Token: true, Reason: "token"}},
		{name: "not enrolled, no token label", mutate: func(l *standaloneLive) { delete(l.proxy.Labels, TokenLabel) }, tok: current,
			want: standalonePlan{Up: true, Token: true, Reason: "token"}},
		{name: "not enrolled, disabled token label", tok: disabled, want: standalonePlan{Up: true, Token: true, Reason: "token"}},
		{name: "not enrolled, no token ever made", tok: ingresshub.TokenStatus{}, want: standalonePlan{Up: true, Token: true, Reason: "token"}},
		{name: "enrolled, old token label", mutate: func(l *standaloneLive) { l.proxy.Labels[TokenLabel] = "tok-1" }, enrolled: true, tok: current, want: standalonePlan{}},
		{name: "enrolled, disabled token", enrolled: true, tok: disabled, want: standalonePlan{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := good()
			if tc.mutate != nil {
				tc.mutate(&l)
			}
			if got := planStandalone(l, image, tc.accessLog, tc.enrolled, tc.tok); got != tc.want {
				t.Errorf("planStandalone = %+v, want %+v", got, tc.want)
			}
		})
	}
}
