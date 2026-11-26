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

// TestKeepalivedOwnership pins D-14 and A-13 (T-26-05): the keepalived service and secret are owned only
// with the keepalived label and that component's own exact name or prefix, and the Any forms pair each
// component with its own names, so a cross pair owns nothing.
func TestKeepalivedOwnership(t *testing.T) {
	ka := KeepalivedLabels()
	ing := ObjectLabels()
	keySecret := SecretPrefixKeepalived + "g1-abc"
	tokenSecret := SecretPrefix + "7"

	if ServiceNameKeepalived != "tierplan-keepalived" || SecretPrefixKeepalived != "tierplan-keepalived-key-" {
		t.Errorf("reserved names changed: %q, %q", ServiceNameKeepalived, SecretPrefixKeepalived)
	}
	want := map[string]string{"tierplan.component": "keepalived", "tierplan.keep": "true", "tierplan.updates": "ignore"}
	if !maps.Equal(ka, want) {
		t.Fatalf("KeepalivedLabels() = %v, want %v", ka, want)
	}
	ka["extra"] = "x"
	delete(ka, LabelComponent)
	if again := KeepalivedLabels(); !maps.Equal(again, want) {
		t.Fatalf("KeepalivedLabels() after a caller edited the first result = %v, want %v", again, want)
	}
	ka = KeepalivedLabels()

	if !HasComponentLabel(ing) || !HasComponentLabel(ka) {
		t.Error("HasComponentLabel is true for component ingress and component keepalived")
	}
	if HasComponentLabel(map[string]string{LabelComponent: "other"}) || HasComponentLabel(nil) {
		t.Error("HasComponentLabel is false for another component and for no labels")
	}

	services := []struct {
		name    string
		objN    string
		labels  map[string]string
		keepal  bool // OwnsKeepalivedService
		proxy   bool // OwnsService
		anyWant bool // OwnsAnyService
	}{
		{"keepalived label and name", ServiceNameKeepalived, ka, true, false, true},
		{"proxy label and name", ServiceName, ing, false, true, true},
		{"ingress label on the keepalived name", ServiceNameKeepalived, ing, false, false, false},
		{"keepalived label on the proxy name", ServiceName, ka, false, false, false},
		{"keepalived label on another name", "web", ka, false, false, false},
		{"keepalived name without the label", ServiceNameKeepalived, nil, false, false, false},
		{"keepalived name with an empty map", ServiceNameKeepalived, map[string]string{}, false, false, false},
	}
	for _, c := range services {
		t.Run("service "+c.name, func(t *testing.T) {
			if got := OwnsKeepalivedService(c.objN, c.labels); got != c.keepal {
				t.Errorf("OwnsKeepalivedService = %v, want %v", got, c.keepal)
			}
			if got := OwnsService(c.objN, c.labels); got != c.proxy {
				t.Errorf("OwnsService = %v, want %v", got, c.proxy)
			}
			if got := OwnsAnyService(c.objN, c.labels); got != c.anyWant {
				t.Errorf("OwnsAnyService = %v, want %v", got, c.anyWant)
			}
		})
	}

	secrets := []struct {
		name    string
		objN    string
		labels  map[string]string
		keepal  bool
		proxy   bool
		anyWant bool
	}{
		{"keepalived label and prefix", keySecret, ka, true, false, true},
		{"the bare keepalived prefix", SecretPrefixKeepalived, ka, true, false, true},
		{"proxy label and prefix", tokenSecret, ing, false, true, true},
		{"ingress label on a keepalived key name", keySecret, ing, false, false, false},
		{"keepalived label on a join token name", tokenSecret, ka, false, false, false},
		{"keepalived label on another name", "db-password", ka, false, false, false},
		{"keepalived prefix without the label", keySecret, nil, false, false, false},
		{"a name shorter than the prefix", "tierplan-keepalived", ka, false, false, false},
	}
	for _, c := range secrets {
		t.Run("secret "+c.name, func(t *testing.T) {
			if got := OwnsKeepalivedSecret(c.objN, c.labels); got != c.keepal {
				t.Errorf("OwnsKeepalivedSecret = %v, want %v", got, c.keepal)
			}
			if got := OwnsSecret(c.objN, c.labels); got != c.proxy {
				t.Errorf("OwnsSecret = %v, want %v", got, c.proxy)
			}
			if got := OwnsAnySecret(c.objN, c.labels); got != c.anyWant {
				t.Errorf("OwnsAnySecret = %v, want %v", got, c.anyWant)
			}
		})
	}

	// The proxy's other objects are not the keepalived component's either.
	if OwnsNetwork(NetworkName, ka) || OwnsVolume(VolumeName, ka) {
		t.Error("the keepalived label owns neither the ingress network nor its volume")
	}
	if OwnsProxyContainer(map[string]string{
		LabelComponent:               ComponentKeepalived,
		"com.docker.compose.project": ProjectName,
		"com.docker.compose.service": ProxyComposeService,
	}) {
		t.Error("the keepalived label does not own the standalone proxy container")
	}
}
