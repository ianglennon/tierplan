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
	// ProjectName is the compose project of the standalone host's proxy. It is reserved on standalone
	// targets: no stack may use it (D-10), so the proxy's containers are never confused with a user's.
	ProjectName = "tierplan-ingress"
	// ProxyComposeService is the one service of ProjectName: the proxy.
	ProxyComposeService = "proxy"
	// SecretPrefix starts the name of every swarm secret that carries a proxy join token.
	SecretPrefix = "tierplan-ingress-token-" //nolint:gosec // G101: a name prefix, not a credential
	// ServiceNameKeepalived is the swarm service that runs keepalived for the virtual IP. Like the names above it
	// never changes once it is in the field.
	ServiceNameKeepalived = "tierplan-keepalived"
	// SecretPrefixKeepalived starts the name of every swarm secret that carries the virtual IP's VRRP key. Each
	// generation has its own secret, so it is matched by prefix. It never changes once it is in the field.
	SecretPrefixKeepalived = "tierplan-keepalived-key-" //nolint:gosec // G101: a name prefix, not a credential
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

// KeepalivedLabels returns the labels every keepalived object carries: the keepalived component
// marker, the janitor's keep label and the update detector's ignore label (D-14). The map is a fresh
// copy on each call, so a caller may add its own labels to it.
func KeepalivedLabels() map[string]string {
	return map[string]string{
		LabelComponent: ComponentKeepalived,
		labelKeep:      "true",
		labelUpdates:   "ignore",
	}
}

// HasComponentLabel reports whether labels carry the marker of a TierPlan ingress component: the
// proxy's (ingress) or the virtual IP's (keepalived). The label alone is enough only for an exemption
// that fails safe, such as the janitor leaving an object alone or the update detector skipping it
// (D-41, A-13): a user who forges the label on their own object only exempts that object from cleanup.
// A write must not rely on it; use the Owns functions.
func HasComponentLabel(labels map[string]string) bool {
	switch labels[LabelComponent] {
	case ComponentIngress, ComponentKeepalived:
		return true
	}
	return false
}

// hasIngressLabel reports whether labels carry the proxy's component marker and no other. The Owns
// functions of the proxy's objects use it, not HasComponentLabel, so the keepalived label on the
// proxy's reserved name owns nothing (A-13).
func hasIngressLabel(labels map[string]string) bool {
	return labels[LabelComponent] == ComponentIngress
}

// hasKeepalivedLabel reports whether labels carry the keepalived component marker and no other.
func hasKeepalivedLabel(labels map[string]string) bool {
	return labels[LabelComponent] == ComponentKeepalived
}

// OwnsService reports whether a service is the ingress proxy: the component label and the exact
// reserved name. A write refusal needs both (D-41), because the label alone is user-controlled.
func OwnsService(name string, labels map[string]string) bool {
	return hasIngressLabel(labels) && name == ServiceName
}

// OwnsKeepalivedService reports whether a service is the virtual IP's keepalived service: the
// keepalived component label and the exact reserved name (D-14, A-13).
func OwnsKeepalivedService(name string, labels map[string]string) bool {
	return hasKeepalivedLabel(labels) && name == ServiceNameKeepalived
}

// OwnsAnyService reports whether a service is one of TierPlan's own ingress services, the proxy or
// keepalived. Each component is paired with its own name only, so the ingress label on
// tierplan-keepalived, or the keepalived label on tierplan-ingress, owns nothing (A-13).
func OwnsAnyService(name string, labels map[string]string) bool {
	return OwnsService(name, labels) || OwnsKeepalivedService(name, labels)
}

// OwnsNetwork reports whether a network is the ingress network: the component label and the exact
// reserved name.
func OwnsNetwork(name string, labels map[string]string) bool {
	return hasIngressLabel(labels) && name == NetworkName
}

// OwnsVolume reports whether a volume is the proxy's certificate cache: the component label and the
// exact reserved name.
func OwnsVolume(name string, labels map[string]string) bool {
	return hasIngressLabel(labels) && name == VolumeName
}

// OwnsSecret reports whether a secret is a proxy join token: the component label and a name that
// starts with SecretPrefix. Each generation of a token has its own secret, so the name is matched by
// prefix, not exactly.
func OwnsSecret(name string, labels map[string]string) bool {
	return hasIngressLabel(labels) && strings.HasPrefix(name, SecretPrefix)
}

// OwnsKeepalivedSecret reports whether a secret is a keepalived VRRP key: the keepalived component
// label and a name that starts with SecretPrefixKeepalived (D-14, A-13).
func OwnsKeepalivedSecret(name string, labels map[string]string) bool {
	return hasKeepalivedLabel(labels) && strings.HasPrefix(name, SecretPrefixKeepalived)
}

// OwnsAnySecret reports whether a secret is one of TierPlan's own ingress secrets, a proxy join token
// or a keepalived key. Each component is paired with its own prefix only (A-13).
func OwnsAnySecret(name string, labels map[string]string) bool {
	return OwnsSecret(name, labels) || OwnsKeepalivedSecret(name, labels)
}

// The compose labels a standalone host's proxy container carries. They are compose's own strings;
// internal/ingress imports no compose module, so they are spelled out here.
const (
	composeLabelProject = "com.docker.compose.project"
	composeLabelService = "com.docker.compose.service"
)

// OwnsProxyContainer reports whether a container is the standalone proxy: the component label, the
// reserved compose project and the proxy's compose service. The label alone is user-controlled, so a
// write refusal needs all three (D-41).
func OwnsProxyContainer(labels map[string]string) bool {
	return hasIngressLabel(labels) &&
		labels[composeLabelProject] == ProjectName &&
		labels[composeLabelService] == ProxyComposeService
}
