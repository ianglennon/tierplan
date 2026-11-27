// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"strconv"
	"strings"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// The items of the self route's previews, in the order the switch-on preview lists them (D-17), then the two of the
// switch-off preview (D-11). An item the preview does not send is not drawn.
const (
	ItemSelfHostname       = "self_hostname"
	ItemSelfCertificate    = "self_certificate"
	ItemSelfNetwork        = "self_network"
	ItemSelfOrigin         = "self_origin"
	ItemSelfCarveOut       = "self_carve_out"
	ItemSelfOriginRemoved  = "self_origin_removed"
	ItemSelfNetworkRemoved = "self_network_removed"
)

// The states of an item: passed, a warning that lets the switch go on, or a refusal that stops it.
const (
	SelfItemOK      = "ok"
	SelfItemWarning = "warning"
	SelfItemRefused = "refused"
)

// The kinds of the D-03 snippet (UI-SPEC A-04): the stack file lines for a service deployed from a stack, the service update
// command for one that was not.
const (
	SelfSnippetStackFile     = "stack_file"
	SelfSnippetServiceUpdate = "service_update"
)

// The codes of the self route's warnings and refusals that this file owns. The location refusals are selfloc.go's and the
// hostname refusals the ingress policy's.
const (
	// CodeSelfRouteBasePath refuses the route while the public URL has a path: the route serves TierPlan at the root of its
	// hostname, and a prefix rewrite is not rendered (Pitfall 9).
	CodeSelfRouteBasePath = "self_route_base_path"
	// CodeSelfRouteNoURL refuses the route while neither a public URL nor an agent URL is set: the route has no address of
	// TierPlan's to name (Pitfall 10).
	CodeSelfRouteNoURL = "self_route_no_url"
	// CodeSelfRouteAlreadyOn refuses a second route: the install holds one (ingressstore.ErrSystemClaimHeld).
	CodeSelfRouteAlreadyOn = "self_route_already_on"
	// CodeSelfCertificateTerms and CodeSelfCertificatePaused refuse the route while no certificate can be ordered for it.
	CodeSelfCertificateTerms  = "self_certificate_terms"
	CodeSelfCertificatePaused = "self_certificate_paused"
	// CodeSelfCertificatePending warns that the hostname has no valid certificate yet.
	CodeSelfCertificatePending = "self_certificate_pending"
	// CodeSelfNotAttached warns that TierPlan's swarm service is not on the self network yet, so the route serves nothing
	// until it is (D-03). It is the route's skip reason too.
	CodeSelfNotAttached = "self_not_attached"
)

// The sentences of the self route that carry no value (UI-SPEC S1b, S1c).
const (
	selfIngressOffText      = "Turn ingress on for this environment first. TierPlan's own route is served by the proxies."
	selfAlreadyOnText       = "TierPlan's own route is already on. Turn it off first to serve TierPlan on another hostname."
	selfTermsText           = "No certificate is ordered until the terms are accepted. Accept Let's Encrypt's terms under Ingress certificates on the HTTPS page, then try again."
	selfOnNetworkText       = "TierPlan is on " + ingress.SelfNetworkName + "."
	selfStandaloneText      = "TierPlan connects its own container to " + ingress.SelfNetworkName + "."
	selfNotAttachedText     = "TierPlan's service isn't on " + ingress.SelfNetworkName + " yet. The route is switched on, but it serves nothing until the service is on the network."
	selfNoURLText           = "Set TierPlan's public URL in Settings first. The route needs an address of TierPlan's to start from."
	selfNetworkKeptText     = "The " + ingress.SelfNetworkName + " network stays, because the port-80 carve-out uses it."
	selfNetworkRemovedText  = "The " + ingress.SelfNetworkName + " network is removed, because the port-80 carve-out doesn't need it."
	selfOwnHostnamesDefault = "TierPlan's own hostnames"
)

// selfNetworkInUseText is the switch-off preview's network sentence while TierPlan's own swarm service is on the network, which
// the switch-off keeps (A-06): the stack-file lines for a service of stack namespace, else the service update for service.
func selfNetworkInUseText(namespace, service string) string {
	const head = "The " + ingress.SelfNetworkName + " network stays while TierPlan's service is on it. "
	const tail = "; a later pass then removes the network."
	if namespace != "" || service == "" {
		return head + "Take the " + ingress.SelfNetworkName + " lines out of TierPlan's stack file and run docker stack deploy again" + tail
	}
	return head + "Run docker service update --network-rm " + ingress.SelfNetworkName + " " + service + " on a manager" + tail
}

// selfHostnameOKText is the hostname item's sentence when the name is free.
func selfHostnameOKText(host, domain string) string {
	return host + " is free and under " + domain + "."
}

// selfHostPortText is the refusal of a hostname typed with a port.
func selfHostPortText(host string) string {
	return "“" + host + "” is not a valid hostname: give it without a port, such as tierplan.example.org."
}

// selfCertValidText, selfCertPendingText and selfPausedText are the certificate item's sentences (D-09).
func selfCertValidText(host string) string { return "A certificate for " + host + " is valid." }

func selfCertPendingText(host string) string {
	return "No certificate yet. TierPlan orders one when you turn the route on. " + host + " isn't served until it is ready."
}

func selfPausedText(host string) string {
	return "Certificate work is paused after a restore, so no certificate is ordered for " + host +
		". Resume it under Ingress certificates on the HTTPS page, then try again."
}

// selfOriginText is the origin item's sentence (D-07).
func selfOriginText(host string) string {
	return "TierPlan will also accept requests from https://" + host +
		". Your public URL isn't changed. Cookies are separate, so you sign in on each address."
}

// selfOriginRemovedText is the switch-off preview's origin sentence (S1c).
func selfOriginRemovedText(host string) string {
	return "TierPlan stops accepting requests from https://" + host + "."
}

// selfBasePathText refuses a public URL with a path (Pitfall 9).
func selfBasePathText(path string) string {
	return "TierPlan's public URL has the path " + path + ". The route serves TierPlan at the root of its hostname, " +
		"so it can't be switched on while the public URL has a path."
}

// selfLockedPublicText refuses a route on the host of a public URL TIERPLAN_PUBLIC_URL fixes, while no agent URL is set:
// agents and proxies dial that host straight (D-10, SELF-05), and Settings cannot move it.
func selfLockedPublicText(host string) string {
	return "“" + host + "” is the host of your public URL, which TIERPLAN_PUBLIC_URL fixes, and no agent URL is set, " +
		"so agents and proxies connect to it straight and never through ingress. Set an agent URL on another host first, or choose another hostname."
}

// selfCarveOutText is the carve-out item's sentence (D-12, S1b item 5). hosts are the hosts of the public and agent URLs.
func selfCarveOutText(hosts []string) string {
	return "TierPlan's own certificate uses the HTTP-01 challenge. The ingress sends only /.well-known/acme-challenge/ requests for " +
		selfHostList(hosts) + " to TierPlan, over " + ingress.SelfNetworkName + ", so it keeps renewing."
}

// selfHostList names hosts for a sentence: "a", "a and b", "a, b and c"; with none, TierPlan's own hostnames.
func selfHostList(hosts []string) string {
	switch len(hosts) {
	case 0:
		return selfOwnHostnamesDefault
	case 1:
		return hosts[0]
	}
	return strings.Join(hosts[:len(hosts)-1], ", ") + " and " + hosts[len(hosts)-1]
}

// StackFileSnippet is the D-03 stack file lines that put service on the self network (UI-SPEC S1a), with two-space YAML
// indentation. service is the key of TierPlan's service in its stack file. The docs test compares against the same text.
func StackFileSnippet(service string) string {
	return "services:\n" +
		"  " + yamlKey(service) + ":\n" +
		"    networks:\n" +
		"      - " + ingress.SelfNetworkName + "\n" +
		"networks:\n" +
		"  " + ingress.SelfNetworkName + ":\n" +
		"    external: true\n"
}

// ServiceUpdateCommand is the D-03 command that puts a swarm service that was not deployed from a stack on the self network
// (UI-SPEC S1a). service is the swarm service's full name.
func ServiceUpdateCommand(service string) string {
	return "docker service update --network-add " + ingress.SelfNetworkName + " " + shellWord(service)
}

// selfSnippet is the snippet of TierPlan's place: the stack file lines when its service carries a stack namespace, the
// command otherwise, and none unless TierPlan runs here as a swarm service.
func selfSnippet(place SelfPlace) *SelfSnippet {
	if place.Kind != SelfKindSwarmService || place.ServiceName == "" {
		return nil
	}
	if place.Namespace == "" {
		return &SelfSnippet{Kind: SelfSnippetServiceUpdate, Text: ServiceUpdateCommand(place.ServiceName)}
	}
	return &SelfSnippet{Kind: SelfSnippetStackFile, Text: StackFileSnippet(stackServiceKey(place.ServiceName, place.Namespace))}
}

// stackServiceKey is the key of a stack's service in its stack file: the swarm service name less "<namespace>_".
func stackServiceKey(service, namespace string) string {
	if key, ok := strings.CutPrefix(service, namespace+"_"); ok && key != "" {
		return key
	}
	return service
}

// safeName is true for a name of letters, digits, '.', '_' and '-' that starts with a letter or digit: the names Docker
// gives services. Any other is quoted, since a service's labels are not trusted to be plain.
func safeName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// yamlKey writes a YAML key, double-quoted unless it is a plain name.
func yamlKey(s string) string {
	if safeName(s) {
		return s
	}
	return strconv.Quote(s)
}

// shellWord writes one shell word, single-quoted unless it is a plain name.
func shellWord(s string) string {
	if safeName(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
