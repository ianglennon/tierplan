// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import "strings"

// The names of the Docker objects TierPlan creates for an environment's proxy. They never change once
// they are in the field, like the agent's names: a rename would leave the old objects behind on every
// swarm that already has them.
const (
	// NetworkName is the overlay network the proxy and the stacks' routed services share.
	NetworkName = "tierplan-ingress"
	// ServiceName is the swarm service that runs the proxy.
	ServiceName = "tierplan-ingress"
	// VolumeName is the named volume that caches certificates on each proxy node.
	VolumeName = "tierplan-ingress-cache"
	// SecretPrefix starts the name of every swarm secret that carries a proxy join token.
	SecretPrefix = "tierplan-ingress-token-" //nolint:gosec // G101: a name prefix, not a credential
)

// The labels that, with LabelComponent, mark an object as TierPlan's own. They are the stack
// package's keep label and the update detector's ignore label; internal/ingress imports no package of
// this module (D-22), so the strings live here and a test on the other side asserts they are equal.
const (
	labelKeep    = "tierplan.keep"
	labelUpdates = "tierplan.updates"
)

// ObjectLabels returns the labels every object of the ingress carries: the component marker, the
// janitor's keep label and the update detector's ignore label. The map is a fresh copy on each call,
// so a caller may add its own labels to it.
func ObjectLabels() map[string]string {
	return map[string]string{
		LabelComponent: ComponentIngress,
		labelKeep:      "true",
		labelUpdates:   "ignore",
	}
}

// HasComponentLabel reports whether labels carry the ingress component marker. The label alone is
// enough only for an exemption that fails safe, such as the janitor leaving an object alone or the
// update detector skipping it (D-41): a user who forges the label on their own object only exempts
// that object from cleanup. A write must not rely on it; use the Owns functions.
func HasComponentLabel(labels map[string]string) bool {
	return labels[LabelComponent] == ComponentIngress
}

// OwnsService reports whether a service is the ingress proxy: the component label and the exact
// reserved name. A write refusal needs both (D-41), because the label alone is user-controlled.
func OwnsService(name string, labels map[string]string) bool {
	return HasComponentLabel(labels) && name == ServiceName
}

// OwnsNetwork reports whether a network is the ingress network: the component label and the exact
// reserved name.
func OwnsNetwork(name string, labels map[string]string) bool {
	return HasComponentLabel(labels) && name == NetworkName
}

// OwnsVolume reports whether a volume is the proxy's certificate cache: the component label and the
// exact reserved name.
func OwnsVolume(name string, labels map[string]string) bool {
	return HasComponentLabel(labels) && name == VolumeName
}

// OwnsSecret reports whether a secret is a proxy join token: the component label and a name that
// starts with SecretPrefix. Each generation of a token has its own secret, so the name is matched by
// prefix, not exactly.
func OwnsSecret(name string, labels map[string]string) bool {
	return HasComponentLabel(labels) && strings.HasPrefix(name, SecretPrefix)
}
