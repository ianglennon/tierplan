// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"net/url"
	"slices"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// SelfRouteRequest is what an Admin asks for when they switch TierPlan's own route on.
type SelfRouteRequest struct {
	// Hostname is the name TierPlan is to be served on, as typed.
	Hostname string
}

// SelfSnippet is the next step that puts TierPlan's swarm service on the self network (D-03, UI-SPEC A-04): Kind is
// SelfSnippetStackFile or SelfSnippetServiceUpdate and Text the lines or the command, with the service name filled in.
type SelfSnippet struct {
	Kind, Text string
}

// SelfRouteItem is one check of a self route preview: Code names the item (ItemSelf...), State is SelfItemOK,
// SelfItemWarning or SelfItemRefused, and Message is the server's sentence. Reason is the stable code of a warning or a
// refusal (such as self_route_not_here or ingress_host_is_agent_url), empty for an item that passed. Snippet is the D-03
// next step, only on the self-network item of a swarm service that is not on the network.
type SelfRouteItem struct {
	Code, State, Message, Reason string
	Snippet                      *SelfSnippet
}

// SelfRoutePreview is what a self route preview tells a person before anything changes (D-17): the items in their order,
// the hostname as judged (lower-cased), and the direct address that keeps reaching TierPlan whatever the route does (UI-SPEC
// A-03), empty when the web is to use the page's own origin.
type SelfRoutePreview struct {
	Items        []SelfRouteItem
	Host, Direct string
}

// Refused is true when any item refuses the switch.
func (p SelfRoutePreview) Refused() bool {
	return slices.ContainsFunc(p.Items, func(it SelfRouteItem) bool { return it.State == SelfItemRefused })
}

// PreviewSelfRoute says what switching TierPlan's own route on in envID would do and what would stop it, and writes nothing
// (D-17): no claim, no network, no certificate. The items are, in this order and only when they apply, the hostname, its
// certificate, the self network, the origin that is added and the HTTP-01 carve-out. Every item is computed even when an
// earlier one refuses, so the dialog tells the whole story. An error is a read that failed, not a refusal.
func (s *Service) PreviewSelfRoute(ctx context.Context, envID string, req SelfRouteRequest) (SelfRoutePreview, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return SelfRoutePreview{}, err
	}
	host := ingress.NormalizeHost(req.Hostname)
	urls := s.selfURLs()
	pv := SelfRoutePreview{Host: host, Direct: directAddress(urls, host)}

	hostItem, err := s.selfHostnameItem(ctx, info, host, urls)
	if err != nil {
		return SelfRoutePreview{}, err
	}
	pv.Items = append(pv.Items, hostItem, s.selfCertificateItem(ctx, info, host))
	netItem, err := s.selfNetworkItem(ctx, info)
	if err != nil {
		return SelfRoutePreview{}, err
	}
	pv.Items = append(pv.Items, netItem, selfOriginItem(host, urls, hostItem))
	if s.ownHTTP01(ctx) {
		pv.Items = append(pv.Items, SelfRouteItem{Code: ItemSelfCarveOut, State: SelfItemOK, Message: selfCarveOutText(selfURLHosts(urls))})
	}
	return pv, nil
}

// selfHostnameItem judges the hostname by the rules every claim is judged by (ingress.CheckSystemClaim), over the
// environment's world with the hosts agents and proxies dial.
func (s *Service) selfHostnameItem(ctx context.Context, info EnvInfo, host string, urls SelfURLs) (SelfRouteItem, error) {
	item := SelfRouteItem{Code: ItemSelfHostname}
	refuse := func(code, msg string) (SelfRouteItem, error) {
		item.State, item.Reason, item.Message = SelfItemRefused, code, msg
		return item, nil
	}
	w, err := s.d.Store.World(ctx, info.ID, nil)
	if err != nil {
		return item, err
	}
	w.AgentHosts = withAgentHost(w.AgentHosts, urls)
	dom, p, bad := ingress.CheckSystemClaim(host, w)
	if bad {
		return refuse(p.Code, p.Message)
	}
	item.State, item.Message = SelfItemOK, selfHostnameOKText(host, dom.Suffix)
	return item, nil
}

// selfCertificateItem says where the hostname's certificate stands (D-09). Anything that cannot be read is a warning, never
// a valid certificate.
func (s *Service) selfCertificateItem(ctx context.Context, info EnvInfo, host string) SelfRouteItem {
	pending := SelfRouteItem{Code: ItemSelfCertificate, State: SelfItemWarning, Reason: CodeSelfCertificatePending, Message: selfCertPendingText(host)}
	if s.d.HostCertificate == nil || host == "" {
		return pending
	}
	st, err := s.d.HostCertificate(ctx, info.ID, host)
	if err != nil {
		s.log().Warn("could not read the certificate of TierPlan's own route", "environment", info.ID, "error", err)
		return pending
	}
	if st.Valid {
		return SelfRouteItem{Code: ItemSelfCertificate, State: SelfItemOK, Message: selfCertValidText(host)}
	}
	return pending
}

// selfNetworkItem says whether TierPlan is, or will be, on the self network (D-01, D-03, D-04, D-05).
func (s *Service) selfNetworkItem(ctx context.Context, info EnvInfo) (SelfRouteItem, error) {
	place, _, err := s.SelfLocation(ctx, info)
	if err != nil {
		return SelfRouteItem{}, err
	}
	item := SelfRouteItem{Code: ItemSelfNetwork, State: SelfItemOK}
	switch {
	case place.Kind == SelfKindStandalone:
		item.Message = selfStandaloneText
	case place.Attached:
		item.Message = selfOnNetworkText
	default:
		item.State, item.Reason, item.Message, item.Snippet = SelfItemWarning, CodeSelfNotAttached, selfNotAttachedText, selfSnippet(place)
	}
	return item, nil
}

// selfOriginItem says which origin TierPlan will also accept (D-07).
func selfOriginItem(host string, _ SelfURLs, _ SelfRouteItem) SelfRouteItem {
	return SelfRouteItem{Code: ItemSelfOrigin, State: SelfItemOK, Message: selfOriginText(host)}
}

// selfURLs reads the instance's own addresses, none when they are not wired.
func (s *Service) selfURLs() SelfURLs {
	if s.d.SelfURLs == nil {
		return SelfURLs{}
	}
	return s.d.SelfURLs()
}

// ownHTTP01 is whether TierPlan's own certificate renews over HTTP-01. A read that fails counts as no.
func (s *Service) ownHTTP01(ctx context.Context) bool {
	if s.d.OwnHTTP01InUse == nil {
		return false
	}
	on, err := s.d.OwnHTTP01InUse(ctx)
	if err != nil {
		s.log().Warn("could not tell whether TierPlan's own certificate uses HTTP-01", "error", err)
		return false
	}
	return on
}

// parseSelfURL parses one of the instance's own URLs; ok is false for an empty or unparsable one, or one with no host.
func parseSelfURL(raw string) (*url.URL, bool) {
	if raw == "" {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, false
	}
	return u, true
}

// urlHost is the lower-cased host of a URL without its port, "" for none.
func urlHost(raw string) string {
	u, ok := parseSelfURL(raw)
	if !ok {
		return ""
	}
	return ingress.NormalizeHost(u.Hostname())
}

// urlOrigin is a URL's scheme and host with its port, "" for none.
func urlOrigin(raw string) string {
	u, ok := parseSelfURL(raw)
	if !ok {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// directAddress is the address that keeps reaching TierPlan whatever the route does (UI-SPEC A-03, A6): the agent URL's
// origin when it is set, else the public URL's when its host is not the route's, else "" (the page's own origin).
func directAddress(urls SelfURLs, host string) string {
	if o := urlOrigin(urls.AgentURL); o != "" {
		return o
	}
	if h := urlHost(urls.PublicURL); h != "" && h != host {
		return urlOrigin(urls.PublicURL)
	}
	return ""
}

// agentDialHost is the host agents and proxies dial: the agent URL's, else the public URL's (D-10).
func agentDialHost(urls SelfURLs) string {
	if h := urlHost(urls.AgentURL); h != "" {
		return h
	}
	return urlHost(urls.PublicURL)
}

// withAgentHost adds the host agents and proxies dial to hosts, once.
func withAgentHost(hosts []string, urls SelfURLs) []string {
	h := agentDialHost(urls)
	if h == "" || slices.Contains(hosts, h) {
		return hosts
	}
	return append(slices.Clone(hosts), h)
}

// selfURLHosts are the hosts of the public and agent URLs, in that order, once each: the names TierPlan's own certificate
// carries, which the carve-out sends to TierPlan.
func selfURLHosts(urls SelfURLs) []string {
	var out []string
	for _, raw := range []string{urls.PublicURL, urls.AgentURL} {
		if h := urlHost(raw); h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}
