// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingressrender"
)

// selfReadyTimeout is how long the holder waits for the first observation of its claim's environment before it says it is
// ready anyway (A-08): the proxy observer holds its bell until then, and must not hold it forever when no pass reports.
const selfReadyTimeout = 2 * time.Minute

// selfReloadTimeout bounds the claim read Observe makes. Observe is called from the ensure loop with no context of its own.
const selfReloadTimeout = 5 * time.Second

// selfSnapshot is what the holder knows, swapped whole so a request reads one consistent state. It is never changed after
// it is published.
type selfSnapshot struct {
	// ClaimRead is whether the stored claim was read at least once.
	ClaimRead bool
	// Claimed is whether the system claim exists; EnvID and Host are its environment and hostname.
	Claimed bool
	EnvID   string
	Host    string
	// Origins are the extra origins the claim gives a mutation: the route host's, while the claim exists (D-07). ExtraOrigins
	// adds the agent URL's, the direct address, whether or not the claim exists.
	Origins []string
	// Obs is the latest observation of TierPlan's own environment, HasObs whether there is one.
	Obs    ingressdeploy.SelfObservation
	HasObs bool
	// Prefixes are the live trusted-proxy prefixes (D-06).
	Prefixes []netip.Prefix
	// Attached, Observed, Backend and Kind are the observation's, as later readers want them.
	Attached bool
	Observed bool
	Backend  string
	Kind     string
	// WaitSince is when the holder began waiting for the current claim's first observation; the ready fallback counts from it.
	WaitSince time.Time
}

// selfRoute holds the live trusted-proxy set and the extra origin of TierPlan's own route (D-06, D-07). It is filled from
// the stored system claim, a database read, and from the observations of TierPlan's own self network the ingress ensure
// loop publishes. Nothing a request carries reaches it, and an observation of an environment other than TierPlan's own
// changes nothing (SELF-03).
type selfRoute struct {
	systemClaim func(ctx context.Context) (ingressstore.SystemClaim, bool, error)
	now         func() time.Time
	log         *slog.Logger

	// mu serialises writers; readers load snap.
	mu   sync.Mutex
	snap atomic.Pointer[selfSnapshot]
	// reloadMu makes a reload's read and its publish one step, so a reload whose read began first can never publish its older
	// answer over a later one's (WR-02). It is taken before mu, never inside it.
	reloadMu sync.Mutex

	// in is what SelfInput and the install hook read besides the snapshot, set once by wire. Nil until then: SelfInput
	// answers nothing and the hook does nothing.
	in atomic.Pointer[selfInputs]
	// inMu guards http01, the cached answer of in.http01, and seen, the last SelfInput answer per environment.
	inMu   sync.Mutex
	http01 selfHTTP01
	seen   map[string]string
}

// selfInputs are the server's facts SelfInput builds TierPlan's own route from, besides the claim and the observation.
type selfInputs struct {
	// trust is TierPlan's own trust set: the self-signed anchor and the served leaf, never its issuer (D-14, WR-06).
	trust *ownTrust
	// urls are the instance's own addresses and TIERPLAN_LISTEN's port, read on every call.
	urls func() ingressdeploy.SelfURLs
	// http01 is whether TierPlan's own certificate renews over HTTP-01 (acme.HTTP01InUse), so the carve-out is needed
	// (D-12). Nil is never.
	http01 func(ctx context.Context) (bool, error)
	// notify wakes the reconciler of an environment (ingress.Notifier.Notify).
	notify func(envID string)
}

// selfHTTP01 is the cached answer of selfInputs.http01.
type selfHTTP01 struct {
	have bool
	val  bool
	at   time.Time
}

// selfHTTP01TTL is how long SelfInput trusts its last answer of whether TierPlan renews over HTTP-01. SelfInput runs on
// every 2 s poll of the reconciler, and the answer is a database read; a change of challenge type reaches the carve-out
// within this long.
const selfHTTP01TTL = 10 * time.Second

// newSelfRoute builds the holder. now nil is time.Now and log nil is slog.Default(). It reads nothing until Reload.
func newSelfRoute(systemClaim func(ctx context.Context) (ingressstore.SystemClaim, bool, error), now func() time.Time, log *slog.Logger) *selfRoute {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	h := &selfRoute{systemClaim: systemClaim, now: now, log: log}
	h.snap.Store(&selfSnapshot{WaitSince: now()})
	return h
}

// Reload reads the stored claim. A read that fails leaves what the holder knew in place. Reloads run one at a time, read
// and publish together, so the last to read is the last to publish.
func (h *selfRoute) Reload(ctx context.Context) error {
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	c, ok, err := h.systemClaim(ctx)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := *h.snap.Load()
	next := cur
	next.ClaimRead = true
	next.Claimed = ok
	next.EnvID, next.Host = "", ""
	if ok {
		next.EnvID, next.Host = c.EnvID, strings.ToLower(ingress.NormalizeHost(c.Hostname))
	}
	// A claim that moves to another environment waits afresh for that environment's first observation.
	if next.Claimed && (!cur.Claimed || cur.EnvID != next.EnvID) {
		next.WaitSince = h.now()
	}
	h.publish(cur, next)
	return nil
}

// claimChanged is the claim store's change hook (WR-01): a set or clear of the system claim reloads the holder at once, so
// the route's origin and the claim's environment follow the stored claim even when no ensure pass publishes an observation
// (TierPlan's environment unreachable, or ingress switched off part-way). A read that fails is logged and keeps what the
// holder knew; the next observation reads it again.
func (h *selfRoute) claimChanged() {
	ctx, cancel := context.WithTimeout(context.Background(), selfReloadTimeout)
	defer cancel()
	if err := h.Reload(ctx); err != nil {
		h.log.Warn("the system claim could not be read after it changed; TierPlan's own route keeps what it knew until the next pass", "error", err)
	}
}

// Observe takes an observation the ingress Service published. It reads the claim first, since a switch on or off wakes the
// ensure loop, then keeps the observation only when it is of TierPlan's own environment: the claim's while one exists,
// else the one TierPlan runs in (the carve-out case), as the observation's Kind says.
func (h *selfRoute) Observe(o ingressdeploy.SelfObservation) {
	ctx, cancel := context.WithTimeout(context.Background(), selfReloadTimeout)
	defer cancel()
	if err := h.Reload(ctx); err != nil {
		h.log.Warn("the system claim could not be read; TierPlan's own route keeps what it knew", "error", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := *h.snap.Load()
	switch {
	case cur.Claimed && o.EnvID != cur.EnvID:
		return
	case !cur.Claimed && o.Kind == "" && (!cur.HasObs || cur.Obs.EnvID != o.EnvID):
		// No claim, and TierPlan does not run here: the observation is of another environment. One of the environment
		// already held, saying TierPlan is no longer there, replaces it.
		return
	}
	next := cur
	next.Obs, next.HasObs = o, true
	h.publish(cur, next)
}

// publish derives the sets from next and stores it, logging a change of the trusted set (D-18: logged, not audited; the
// audited switch actions precede it). The caller holds mu.
func (h *selfRoute) publish(cur, next selfSnapshot) {
	next.Origins = nil
	if next.Claimed && next.Host != "" {
		next.Origins = []string{"https://" + next.Host}
	}
	next.Attached, next.Observed, next.Backend, next.Kind = false, false, "", ""
	next.Prefixes = nil
	own := next.HasObs && (!next.Claimed || next.Obs.EnvID == next.EnvID)
	if own {
		o := next.Obs
		next.Attached, next.Observed, next.Backend, next.Kind = o.Attached, o.Observed, o.BackendHost, o.Kind
		// A member of the network that is neither the proxy nor TierPlan could forge any client address: nothing is trusted
		// until it leaves (CR-02).
		if o.Exists && o.Attached && !o.Foreign && len(o.ForeignMembers) == 0 {
			next.Prefixes = ingress.SelfTrustedPrefixes(o.Facts())
		}
	}
	h.snap.Store(&next)
	if !slices.Equal(cur.Prefixes, next.Prefixes) {
		h.log.Info("the trusted proxies on TierPlan's own network changed",
			"env_id", next.Obs.EnvID, "prefixes_before", len(cur.Prefixes), "prefixes_after", len(next.Prefixes))
	}
}

// TrustedPrefixes is the live trusted-proxy set (httpx.Options.LiveTrustedProxies): TierPlan's own self network's subnet
// without its gateway, while TierPlan is attached to it; else none.
func (h *selfRoute) TrustedPrefixes() []netip.Prefix {
	return slices.Clone(h.snap.Load().Prefixes)
}

// ExtraOrigins is the set a mutation and the shell WebSocket may also come from besides the public URL's origin
// (httpx.Options.ExtraOrigins, D-07): the route host's origin while the system claim exists, and the agent URL's origin
// whenever an agent URL is set. The agent URL is TierPlan's own direct address, which Settings never let be an ingress host
// (D-10), so it stays accepted with the route on or off and whatever the public URL names: no release, no claim that cannot
// be read and no deleted environment can leave a browser without an address it may change things from, sign-in included
// (D-11). It comes from the stored claim and the stored URLs only, read on every call, never from a request.
func (h *selfRoute) ExtraOrigins() []string {
	out := slices.Clone(h.snap.Load().Origins)
	in := h.in.Load()
	if in == nil || in.urls == nil {
		return out
	}
	if o := httpx.OriginOf(in.urls().AgentURL); o != "" && !slices.Contains(out, o) {
		out = append(out, o)
	}
	return out
}

// Ready is whether the trusted set can be believed complete (A-08): the claim was read and, when it exists, the first
// observation of its environment arrived. selfReadyTimeout after the wait began it is true regardless.
func (h *selfRoute) Ready() bool {
	s := h.snap.Load()
	if s.ClaimRead && (!s.Claimed || (s.HasObs && s.Obs.EnvID == s.EnvID && s.Obs.Observed)) {
		return true
	}
	return !h.now().Before(s.WaitSince.Add(selfReadyTimeout))
}

// HoldFirstRender is ingressrecon.Deps.HoldFirstRender (28-UAT 9a): whether the first render of envID since start must
// wait. It holds while the claim names envID and the holder is not Ready: the claim's environment has not been observed yet,
// so SelfInput would say TierPlan is not on its own network and the render would leave its own route out. The reconnecting
// proxies would apply that and drop a route that was serving before the restart. Holding keeps their cached bundle until
// the first ensure pass that reaches the manager says where TierPlan is, attached or not; selfReadyTimeout bounds it.
// Nothing is guessed: no attachment is assumed, and the trusted set stays empty until that observation (WR-03).
func (h *selfRoute) HoldFirstRender(envID string) bool {
	if h == nil {
		return false
	}
	s := h.snap.Load()
	return s.Claimed && envID != "" && s.EnvID == envID && !h.Ready()
}

// wire gives the holder the facts SelfInput and the install hook read. The server calls it once, after the ACME manager
// exists and before the reconciler runs.
func (h *selfRoute) wire(in selfInputs) {
	h.in.Store(&in)
}

// SelfInput is ingressrecon.Deps.Self (D-12, D-14): TierPlan's own route and trust for the one environment TierPlan's
// claim names, or with no claim the one TierPlan runs in when its own certificate renews over HTTP-01 (the carve-out). The
// host is the claim's (empty for the carve-out alone), the backend and attachment the observation's, the port
// TIERPLAN_LISTEN's, the server name the public URL's host else the agent URL's (Pitfall 10), and the trust and the version
// ownTrust's, so the version moves whenever the trust does. Every other environment, an instance with neither URL, an
// unknown listen port and an environment with no claim and no carve-out need answer ok false: their bundles carry no self
// input at all.
//
// A change of anything but the trust (an observation, a URL, the challenge type) does not move the version, so SelfInput
// wakes the reconciler of the environment itself when its answer differs from the one before: the reconciler calls it for
// every environment on every poll, so the change is rendered within one poll. A nil holder answers nothing.
func (h *selfRoute) SelfInput(ctx context.Context, envID string) (ingressrender.SelfInput, int64, bool) {
	if h == nil {
		return ingressrender.SelfInput{}, 0, false
	}
	in := h.in.Load()
	if in == nil {
		return ingressrender.SelfInput{}, 0, false
	}
	out, ver, ok := h.selfInput(ctx, in, envID)
	h.remember(in, envID, out, ok)
	return out, ver, ok
}

func (h *selfRoute) selfInput(ctx context.Context, in *selfInputs, envID string) (ingressrender.SelfInput, int64, bool) {
	s := h.snap.Load()
	if envID == "" || h.selfEnv(s) != envID {
		return ingressrender.SelfInput{}, 0, false
	}
	urls := in.urls()
	pub, agent := hostOf(urls.PublicURL), hostOf(urls.AgentURL)
	serverName := pub
	if serverName == "" {
		serverName = agent
	}
	if serverName == "" || urls.ListenPort <= 0 {
		return ingressrender.SelfInput{}, 0, false
	}
	var acmeHosts []string
	if s.Attached && h.http01InUse(ctx, in) {
		acmeHosts = selfACMEHosts(pub, agent)
	}
	if !s.Claimed && len(acmeHosts) == 0 {
		return ingressrender.SelfInput{}, 0, false
	}
	trust, ver := in.trust.PEM()
	out := ingressrender.SelfInput{
		BackendHost: s.Backend, BackendPort: urls.ListenPort, ServerName: serverName,
		TrustPEM: trust, ACMEHosts: acmeHosts, Attached: s.Attached,
		// The renderer refuses a base path, which leaves TierPlan's own route out with a reason rather than serving the
		// host root and the connect paths at the wrong place (WR-07, WR-09). Settings refuse one while the route is on,
		// so it reaches here only through an environment-fixed public URL.
		BasePath: basePathOf(urls.PublicURL),
	}
	if s.Claimed {
		out.Host = s.Host
	}
	return out, ver, true
}

// basePathOf is the path of a URL without its trailing slash, "" for the root or a URL that does not parse.
func basePathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}

// selfEnv is the environment TierPlan's own route or carve-out can be in: the claim's while one exists, else the one the
// observation says TierPlan runs in, else none.
func (h *selfRoute) selfEnv(s *selfSnapshot) string {
	switch {
	case s.Claimed:
		return s.EnvID
	case s.HasObs && s.Obs.Kind != "":
		return s.Obs.EnvID
	}
	return ""
}

// selfACMEHosts are the public and agent URL hosts, once each, leaving out an empty one.
func selfACMEHosts(hosts ...string) []string {
	var out []string
	for _, x := range hosts {
		if x != "" && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// http01InUse is in.http01's answer, read at most once per selfHTTP01TTL. A read that fails keeps the last answer (false
// before the first), so a passing database error neither adds nor drops the carve-out.
func (h *selfRoute) http01InUse(ctx context.Context, in *selfInputs) bool {
	if in.http01 == nil {
		return false
	}
	now := h.now()
	h.inMu.Lock()
	c := h.http01
	h.inMu.Unlock()
	if c.have && now.Sub(c.at) < selfHTTP01TTL && !now.Before(c.at) {
		return c.val
	}
	v, err := in.http01(ctx)
	if err != nil {
		h.log.Debug("could not read whether TierPlan's own certificate renews over HTTP-01; the carve-out keeps its last answer", "error", err)
		h.inMu.Lock()
		v = h.http01.val
		h.inMu.Unlock()
		return v
	}
	h.inMu.Lock()
	h.http01 = selfHTTP01{have: true, val: v, at: now}
	h.inMu.Unlock()
	return v
}

// http01Cached is the last answer of in.http01, without reading it: the install hook runs where it must not block.
func (h *selfRoute) http01Cached() bool {
	h.inMu.Lock()
	defer h.inMu.Unlock()
	return h.http01.val
}

// remember records SelfInput's answer for the environment, the trust left out (its version is the reconciler's own
// version), and wakes the environment's reconciler when it differs from the one recorded before. The first answer for an
// environment is recorded quietly: whoever asked is rendering it.
func (h *selfRoute) remember(in *selfInputs, envID string, out ingressrender.SelfInput, ok bool) {
	key := ""
	if ok {
		key = fmt.Sprintf("%q|%q|%d|%q|%q|%t|%q", out.Host, out.BackendHost, out.BackendPort, out.ServerName, strings.Join(out.ACMEHosts, ","), out.Attached, out.BasePath)
	}
	h.inMu.Lock()
	if h.seen == nil {
		h.seen = map[string]string{}
	}
	prev, had := h.seen[envID]
	h.seen[envID] = key
	h.inMu.Unlock()
	if had && prev != key && in.notify != nil {
		in.notify(envID)
	}
}

// registerSelfInstallHook adds the self-route holder's install hook to the HTTPS runtime, after the HSTS policy's (D-14): an
// ACME install or the fallback to self-signed reaches the proxies in the same step, not on the next poll.
func registerSelfInstallHook(rt *httpsRuntime, h *selfRoute) {
	rt.AddOnInstall(h.onInstall)
}

// onInstall is called after every install of a certificate TierPlan serves. It has ownTrust look at what is served now and,
// when the trust set changed, wakes the reconciler of TierPlan's own environment, so the new trust is rendered at once. The
// re-sign for new names and a replaced supplied pair do not install; the reconciler's poll of the own version catches them.
//
// It runs on the installing goroutine, with the self-signed source's lock held, so it reads nothing that can block: no
// database, and whether TierPlan renews over HTTP-01 from SelfInput's last answer.
func (h *selfRoute) onInstall() {
	in := h.in.Load()
	if in == nil || in.trust == nil {
		return
	}
	if !in.trust.Observe() {
		return
	}
	if env := h.wakeEnv(in); env != "" && in.notify != nil {
		in.notify(env)
	}
}

// wakeEnv is the environment whose bundle carries TierPlan's trust or carve-out, without a database read: the claim's, or
// with no claim the one TierPlan runs in while the carve-out is needed. "" is none.
func (h *selfRoute) wakeEnv(in *selfInputs) string {
	s := h.snap.Load()
	env := h.selfEnv(s)
	if env == "" || s.Claimed {
		return env
	}
	if !s.Attached || !h.http01Cached() || in.urls == nil {
		return ""
	}
	u := in.urls()
	if len(selfACMEHosts(hostOf(u.PublicURL), hostOf(u.AgentURL))) == 0 {
		return ""
	}
	return env
}
