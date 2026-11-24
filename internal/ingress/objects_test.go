// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"maps"
	"testing"
)

// TestObjectLabels pins the three labels and that every call returns its own map.
func TestObjectLabels(t *testing.T) {
	want := map[string]string{
		"tierplan.component": "ingress",
		"tierplan.keep":      "true",
		"tierplan.updates":   "ignore",
	}
	got := ObjectLabels()
	if !maps.Equal(got, want) {
		t.Fatalf("ObjectLabels() = %v, want %v", got, want)
	}
	got["extra"] = "x"
	delete(got, LabelComponent)
	if again := ObjectLabels(); !maps.Equal(again, want) {
		t.Fatalf("ObjectLabels() after a caller edited the first result = %v, want %v", again, want)
	}
}

// TestOwnershipNeedsLabelAndName pins T-24-05: a write refusal needs the label and the reserved
// name, so a forged label on another name, or the name without the label, is not owned.
func TestOwnershipNeedsLabelAndName(t *testing.T) {
	label := ObjectLabels()
	forged := map[string]string{LabelComponent: ComponentIngress}
	none := map[string]string{}

	owns := map[string]func(string, map[string]string) bool{
		"service": OwnsService,
		"network": OwnsNetwork,
		"volume":  OwnsVolume,
		"secret":  OwnsSecret,
	}
	reserved := map[string]string{
		"service": ServiceName,
		"network": NetworkName,
		"volume":  VolumeName,
		"secret":  SecretPrefix + "7",
	}
	for kind, fn := range owns {
		t.Run(kind, func(t *testing.T) {
			cases := []struct {
				name   string
				objN   string
				labels map[string]string
				want   bool
			}{
				{"label and reserved name", reserved[kind], label, true},
				{"only the component label", reserved[kind], forged, true},
				{"label on another name", "my-" + kind, label, false},
				{"forged label on another name", "my-" + kind, forged, false},
				{"reserved name without the label", reserved[kind], none, false},
				{"reserved name with a nil map", reserved[kind], nil, false},
				{"label with another value", reserved[kind], map[string]string{LabelComponent: "other"}, false},
			}
			for _, c := range cases {
				if got := fn(c.objN, c.labels); got != c.want {
					t.Errorf("%s: owns(%q, %v) = %v, want %v", c.name, c.objN, c.labels, got, c.want)
				}
			}
		})
	}

	if OwnsSecret(SecretPrefix, label) != true {
		t.Error("the bare prefix with the label is a secret of the ingress")
	}
	if OwnsSecret("tierplan-ingress", label) {
		t.Error("a name shorter than the secret prefix is not a join token secret")
	}
	if !HasComponentLabel(forged) || HasComponentLabel(none) {
		t.Error("HasComponentLabel reads the label alone")
	}
}

// TestOwnsProxyContainer pins D-06 and D-10: the standalone proxy is the container with the component
// label, the reserved compose project and the proxy's compose service, all three.
func TestOwnsProxyContainer(t *testing.T) {
	compose := func(project, service string) map[string]string {
		return map[string]string{"com.docker.compose.project": project, "com.docker.compose.service": service}
	}
	with := func(m map[string]string) map[string]string {
		m[LabelComponent] = ComponentIngress
		return m
	}
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"label, project and service", with(compose(ProjectName, ProxyComposeService)), true},
		{"the component label alone", ObjectLabels(), false},
		{"project and service without the label", compose(ProjectName, ProxyComposeService), false},
		{"another service of the project", with(compose(ProjectName, "other")), false},
		{"another project's proxy service", with(compose("shop", ProxyComposeService)), false},
		{"no labels", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := OwnsProxyContainer(c.labels); got != c.want {
				t.Errorf("OwnsProxyContainer(%v) = %v, want %v", c.labels, got, c.want)
			}
		})
	}
	if ProjectName != "tierplan-ingress" || ProxyComposeService != "proxy" {
		t.Errorf("reserved names changed: %q, %q", ProjectName, ProxyComposeService)
	}
}
