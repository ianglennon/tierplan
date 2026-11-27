// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

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
	pv, _, err := s.checkSelfRoute(ctx, envID, req)
	return pv, err
}

// selfCheck is what the checks behind a self route preview found that a switch-on stores: the environment, the registered
// domain the hostname sits under ("" when the hostname item refused), where TierPlan runs and the instance's own addresses.
type selfCheck struct {
	info     EnvInfo
	domainID string
	place    SelfPlace
	urls     SelfURLs
}

// checkSelfRoute is PreviewSelfRoute with what the switch-on job stores besides (EnableSelfRoute).
func (s *Service) checkSelfRoute(ctx context.Context, envID string, req SelfRouteRequest) (SelfRoutePreview, selfCheck, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return SelfRoutePreview{}, selfCheck{}, err
	}
	host := ingress.NormalizeHost(req.Hostname)
	urls := s.selfURLs()
	pv := SelfRoutePreview{Host: host, Direct: directAddress(urls, host)}
	sc := selfCheck{info: info, urls: urls}

	hostItem, domainID, err := s.selfHostnameItem(ctx, info, host, urls)
	if err != nil {
		return SelfRoutePreview{}, selfCheck{}, err
	}
	sc.domainID = domainID
	pv.Items = append(pv.Items, hostItem, s.selfCertificateItem(ctx, info, host))
	netItem, place, err := s.selfNetworkItem(ctx, info)
	if err != nil {
		return SelfRoutePreview{}, selfCheck{}, err
	}
	sc.place = place
	pv.Items = append(pv.Items, netItem, selfOriginItem(host, urls))
	if s.ownHTTP01(ctx) {
		pv.Items = append(pv.Items, SelfRouteItem{Code: ItemSelfCarveOut, State: SelfItemOK, Message: selfCarveOutText(selfURLHosts(urls))})
	}
	return pv, sc, nil
}

// selfHostnameItem judges the hostname. Ingress must be on (and not paused) for the environment, the install must hold no
// self route yet (ingressstore.ErrSystemClaimHeld), and the name must pass the rules every claim is judged by
// (ingress.CheckSystemClaim) over the environment's world with the host agents and proxies dial (D-10). Only an instance
// Admin previews the route (D-17), so the holder of a taken name is named whatever its environment. A host agents dial only
// because TIERPLAN_PUBLIC_URL fixes it is left to the origin item, which says how to move it (Pitfall 10).
func (s *Service) selfHostnameItem(ctx context.Context, info EnvInfo, host string, urls SelfURLs) (SelfRouteItem, string, error) {
	item := SelfRouteItem{Code: ItemSelfHostname}
	refuse := func(code, msg string) (SelfRouteItem, string, error) {
		item.State, item.Reason, item.Message = SelfItemRefused, code, msg
		return item, "", nil
	}
	state, err := s.d.Store.DeployState(ctx, info.ID)
	if err != nil {
		return item, "", err
	}
	if !state.Enabled {
		return refuse(CodeVIPIngressOff, selfIngressOffText)
	}
	if paused, err := checkPaused(ctx, s, checkInput{Info: info}); err != nil {
		return item, "", err
	} else if len(paused) > 0 {
		return refuse(paused[0].Code, paused[0].Message)
	}
	if _, held, err := s.d.Store.SystemClaim(ctx); err != nil {
		return item, "", err
	} else if held {
		return refuse(CodeSelfRouteAlreadyOn, selfAlreadyOnText)
	}
	if strings.Contains(host, ":") {
		return refuse(ingress.CodeHostInvalid, selfHostPortText(host))
	}
	w, err := s.d.Store.World(ctx, info.ID, func(string) bool { return true })
	if err != nil {
		return item, "", err
	}
	w.AgentHosts = withAgentHost(w.AgentHosts, urls)
	if lockedPublicHost(urls, host) {
		w.AgentHosts = slices.DeleteFunc(slices.Clone(w.AgentHosts), func(h string) bool { return h == host })
	}
	dom, p, bad := ingress.CheckSystemClaim(host, w)
	if bad {
		return refuse(p.Code, p.Message)
	}
	item.State, item.Message = SelfItemOK, selfHostnameOKText(host, dom.Suffix)
	return item, dom.ID, nil
}

// lockedPublicHost is true when host is the host of a public URL TIERPLAN_PUBLIC_URL fixes and no agent URL is set, so agents
// and proxies dial it and Settings cannot move it.
func lockedPublicHost(urls SelfURLs, host string) bool {
	return urls.PublicLocked && urlHost(urls.AgentURL) == "" && host != "" && urlHost(urls.PublicURL) == host
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
	switch {
	case st.Valid:
		return SelfRouteItem{Code: ItemSelfCertificate, State: SelfItemOK, Message: selfCertValidText(host)}
	case st.TermsNeeded:
		return SelfRouteItem{Code: ItemSelfCertificate, State: SelfItemRefused, Reason: CodeSelfCertificateTerms, Message: selfTermsText}
	case st.Paused:
		return SelfRouteItem{Code: ItemSelfCertificate, State: SelfItemRefused, Reason: CodeSelfCertificatePaused, Message: selfPausedText(host)}
	}
	return pending
}

// selfNetworkItem says whether TierPlan is, or will be, on the self network (D-01, D-03, D-04, D-05). A place SelfLocation
// refuses (TierPlan elsewhere, or where it runs cannot be told, or a plain container on a swarm) refuses the item.
func (s *Service) selfNetworkItem(ctx context.Context, info EnvInfo) (SelfRouteItem, SelfPlace, error) {
	place, findings, err := s.SelfLocation(ctx, info)
	if err != nil {
		return SelfRouteItem{}, SelfPlace{}, err
	}
	item := SelfRouteItem{Code: ItemSelfNetwork, State: SelfItemOK}
	if f, ok := firstError(findings); ok {
		item.State, item.Reason, item.Message = SelfItemRefused, f.Code, f.Message
		return item, place, nil
	}
	switch {
	case place.Kind == SelfKindStandalone:
		item.Message = selfStandaloneText
	case place.Attached:
		item.Message = selfOnNetworkText
	default:
		item.State, item.Reason, item.Message, item.Snippet = SelfItemWarning, CodeSelfNotAttached, selfNotAttachedText, selfSnippet(place)
	}
	return item, place, nil
}

// selfOriginItem says which origin TierPlan will also accept (D-07). It refuses when neither URL is set (Pitfall 10), when
// the public URL has a path (Pitfall 9), and when the route host is the host of a public URL TIERPLAN_PUBLIC_URL fixes with no
// agent URL set (D-10).
func selfOriginItem(host string, urls SelfURLs) SelfRouteItem {
	item := SelfRouteItem{Code: ItemSelfOrigin, State: SelfItemRefused}
	pub, hasPublic := parseSelfURL(urls.PublicURL)
	_, hasAgent := parseSelfURL(urls.AgentURL)
	switch {
	case !hasPublic && !hasAgent:
		item.Reason, item.Message = CodeSelfRouteNoURL, selfNoURLText
	case hasPublic && strings.TrimSuffix(pub.Path, "/") != "":
		item.Reason, item.Message = CodeSelfRouteBasePath, selfBasePathText(pub.Path)
	case lockedPublicHost(urls, host):
		item.Reason, item.Message = ingress.CodeHostIsAgentURL, selfLockedPublicText(host)
	default:
		item.State, item.Message = SelfItemOK, selfOriginText(host)
	}
	return item
}

// firstError is the first refusing finding.
func firstError(findings []Finding) (Finding, bool) {
	for _, f := range findings {
		if f.Severity == SeverityError {
			return f, true
		}
	}
	return Finding{}, false
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

// directAddress is the address that keeps reaching TierPlan whatever the route does (UI-SPEC A-03, A6), and one TierPlan
// accepts changes from (D-07): the public URL's origin when its host is not the route's; else the agent URL's, which the
// origin set holds while the public URL is the route host and which Settings require then (D-10), and which with no public
// URL is reached as the page's own origin; else "" (the page's own origin).
func directAddress(urls SelfURLs, host string) string {
	if h := urlHost(urls.PublicURL); h != "" && h != ingress.NormalizeHost(host) {
		return urlOrigin(urls.PublicURL)
	}
	return urlOrigin(urls.AgentURL)
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

// ErrSelfRouteNotEnabled is a switch-off of TierPlan's own route in an environment it is not on in.
var ErrSelfRouteNotEnabled = errors.New("ingressdeploy: TierPlan's own route is not on in this environment")

// PreviewSelfRouteOff says what switching TierPlan's own route off in envID would do, and writes nothing (D-11, UI-SPEC
// S1c): the origin that is no longer accepted, then whether the self network stays for the port-80 carve-out or goes. It
// never refuses: no state of the route, the engine or the environment stops a release. With no route on in envID it is
// ErrSelfRouteNotEnabled.
func (s *Service) PreviewSelfRouteOff(ctx context.Context, envID string) (SelfRoutePreview, error) {
	claim, held, err := s.d.Store.SystemClaim(ctx)
	if err != nil {
		return SelfRoutePreview{}, err
	}
	if !held || claim.EnvID != envID {
		return SelfRoutePreview{}, ErrSelfRouteNotEnabled
	}
	// The claim names the environment, so it exists: a read of it or of where TierPlan runs that fails only leaves the place
	// unknown, which keeps no carve-out, and never stops the release.
	place := s.selfUnknown()
	if info, err := s.d.Envs.Info(ctx, envID); err != nil {
		s.log().Warn("could not read the environment for the self route's switch-off preview", "environment", envID, "error", err)
	} else if p, _, err := s.SelfLocation(ctx, info); err == nil {
		place = p
	}
	network := SelfRouteItem{Code: ItemSelfNetworkRemoved, State: SelfItemOK, Message: selfNetworkRemovedText}
	// The carve-out keeps the network exactly when the ensure loop would still want it without the claim (selfWanted).
	if place.Here == SelfHereYes && s.ownHTTP01(ctx) {
		network.Message = selfNetworkKeptText
	}
	return SelfRoutePreview{
		Host:   claim.Hostname,
		Direct: directAddress(s.selfURLs(), claim.Hostname),
		Items: []SelfRouteItem{
			{Code: ItemSelfOriginRemoved, State: SelfItemOK, Message: selfOriginRemovedText(claim.Hostname)},
			network,
		},
	}, nil
}

// SelfRouteFacts are what the card of TierPlan's own route draws, from one call (UI-SPEC A-01, A-03, A-04).
type SelfRouteFacts struct {
	// Here is whether TierPlan runs in the environment; the card is not drawn when it is no.
	Here SelfHere
	// On is TierPlan's own route held in this environment, and Host its hostname ("" when off).
	On   bool
	Host string
	// Direct is the address that keeps reaching TierPlan (directAddress), "" for the page's own origin.
	Direct string
	// Kind is where TierPlan runs (SelfPlace.Kind), "" unless Here is yes.
	Kind string
	// Attached is TierPlan being on the self network. AttachedKnown is true when it comes from an ensure pass's observation,
	// false when no pass has read it since start and it comes from TierPlan's own container.
	Attached, AttachedKnown bool
	// CarveOut is TierPlan's own certificate renewing over HTTP-01, so the proxy carries the challenge to it (D-12).
	CarveOut bool
	// Refusal is the reason the route cannot be switched on here, known before the switch is pressed; nil for none.
	Refusal *Finding
	// Snippet is the D-03 next step while TierPlan's swarm service is not on the self network; nil otherwise.
	Snippet *SelfSnippet
}

// SelfRouteFacts reads the facts of TierPlan's own route in envID. A read of where TierPlan runs that fails is an unknown
// place with its refusal, never an error.
func (s *Service) SelfRouteFacts(ctx context.Context, envID string) (SelfRouteFacts, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return SelfRouteFacts{}, err
	}
	claim, held, err := s.d.Store.SystemClaim(ctx)
	if err != nil {
		return SelfRouteFacts{}, err
	}
	place, findings, err := s.SelfLocation(ctx, info)
	if err != nil {
		return SelfRouteFacts{}, err
	}
	f := SelfRouteFacts{Here: place.Here, Kind: place.Kind, Attached: place.Attached}
	if held && claim.EnvID == envID {
		f.On, f.Host = true, claim.Hostname
	}
	f.Direct = directAddress(s.selfURLs(), f.Host)
	if o, ok := s.SelfObservationOf(envID); ok && o.Observed {
		f.Attached, f.AttachedKnown = o.Attached, true
	}
	if r, ok := firstError(findings); ok {
		f.Refusal = &r
	}
	if !f.Attached {
		f.Snippet = selfSnippet(place)
	}
	f.CarveOut = place.Here == SelfHereYes && s.ownHTTP01(ctx)
	return f, nil
}
