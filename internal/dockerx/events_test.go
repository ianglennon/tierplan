// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"maps"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/events"
)

// The attribute allowlist (D-61, T-01-88, T-04-98) and the event types, tested
// on eventRow itself so the exact set is pinned.

func TestKeptAttributesAreExactlyTheTwelveNonSensitiveKeys(t *testing.T) {
	want := []string{
		"name", "image", "exitCode", "signal",
		"type", "container", "driver", "reason", "destination", "read/write",
		"com.docker.compose.project", "com.docker.stack.namespace",
	}
	got := slices.Clone(keptAttributes)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("keptAttributes = %v, want exactly %v: a new key needs a new decision", got, want)
	}
}

func TestEventRowDropsSecretLabelsEnvironmentValuesAndFreeFormAttributes(t *testing.T) {
	in := map[string]string{
		"name": "web", "image": "nginx:1.27", "exitCode": "1", "signal": "15",
		"type": "bridge", "container": "abc", "driver": "local", "reason": "oom",
		"destination": "/data", "read/write": "true",
		"com.docker.compose.project": "shop", "com.docker.stack.namespace": "prod",
		// What must not reach a browser.
		"secret.token": "hunter2", "API_TOKEN": "hunter2", "DB_PASSWORD": "hunter2",
		"com.docker.compose.service": "web", "com.example.owner": "ian",
		"traefik.http.routers.x.rule": "Host(`secret.example.org`)",
		"execID":                      "abc", "healthcheck": "curl -u admin:hunter2 http://localhost",
		"updatestate.new": "updating", "propagation": "rprivate",
	}
	row, ok := eventRow(events.Message{
		Type: events.ContainerEventType, Action: "die", Scope: "local", TimeNano: 1,
		Actor: events.Actor{ID: "c1", Attributes: in},
	})
	if !ok {
		t.Fatal("a die event was dropped")
	}
	if len(row.Attributes) != 12 {
		t.Errorf("%d attributes passed, want the twelve allowlisted ones: %v", len(row.Attributes), row.Attributes)
	}
	for k, v := range row.Attributes {
		if !slices.Contains(keptAttributes, k) || in[k] != v {
			t.Errorf("attribute %q = %q passed the allowlist unchanged or not at all", k, v)
		}
	}
	for _, k := range []string{"secret.token", "API_TOKEN", "DB_PASSWORD", "com.docker.compose.service", "com.example.owner", "traefik.http.routers.x.rule", "execID", "healthcheck", "updatestate.new", "propagation"} {
		if _, leaked := row.Attributes[k]; leaked {
			t.Errorf("attribute %q reached the row", k)
		}
	}
	if !maps.Equal(row.Attributes, map[string]string{
		"name": "web", "image": "nginx:1.27", "exitCode": "1", "signal": "15",
		"type": "bridge", "container": "abc", "driver": "local", "reason": "oom",
		"destination": "/data", "read/write": "true",
		"com.docker.compose.project": "shop", "com.docker.stack.namespace": "prod",
	}) {
		t.Errorf("Attributes = %v", row.Attributes)
	}
}

func TestEventTypesIncludeTheSwarmOnesAndNothingElse(t *testing.T) {
	want := []string{"container", "image", "network", "volume", "service", "node", "secret", "config"}
	if !slices.Equal(eventTypes, want) {
		t.Errorf("eventTypes = %v, want %v", eventTypes, want)
	}
	for _, typ := range []events.Type{events.ServiceEventType, events.NodeEventType, events.SecretEventType, events.ConfigEventType} {
		if _, ok := eventRow(events.Message{Type: typ, Action: "update", TimeNano: 1, Actor: events.Actor{ID: "x"}}); !ok {
			t.Errorf("a %s event was dropped", typ)
		}
	}
	if _, ok := eventRow(events.Message{Type: events.PluginEventType, Action: "install", TimeNano: 1}); ok {
		t.Error("a plugin event was kept")
	}
}
