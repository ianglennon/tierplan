// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"bytes"
	"cmp"
	"encoding/pem"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// SkipReason says why a route is not in the bundle. The values are stable: the API and the UI key on
// them.
type SkipReason string

// The reasons a route is left out of a bundle.
const (
	// SkipNoCertificate: the hostname has no certificate at all.
	SkipNoCertificate SkipReason = "no_certificate"
	// SkipCertificateNotReady: the hostname has a certificate that is not servable yet (issuing, or
	// within the renewal margin of its expiry).
	SkipCertificateNotReady SkipReason = "certificate_not_ready"
	// SkipNoBackend: a stack route's backend cannot be named (no owning stack, no port, or a scheme
	// that is neither http nor https), or the hostname is not claimed at all.
	SkipNoBackend SkipReason = "no_backend"
	// SkipSelfNotAttached: the route is TierPlan's own (a system claim) and TierPlan cannot be reached
	// from the proxy: the render was given no self input, or TierPlan is not on the self network yet
	// (D-03). Nothing is served on the hostname until it is.
	SkipSelfNotAttached SkipReason = "self_not_attached"
)

// The entry point every route's router is attached to, and the TLS floor of the default options
// (D-04). The web entry point redirects and carries one router only: TierPlan's own HTTP-01 carve-out
// (RTE-02, PROH-21-03, Phase 28 D-12).
const (
	entryPointSecure = "websecure"
	entryPointWeb    = "web"
	minTLSVersion    = "VersionTLS12"
)

// CarveOutPriority is the priority of TierPlan's own HTTP-01 carve-out router on the web entry point
// (D-12). The static file puts the port-80 redirect at priority 1, so the carve-out wins on TierPlan's
// own names and the challenge path and every other plain request is still redirected (28-S12-RESULT.md,
// traefik v3.7.19). The redirect is the only other router on web, so any value above 1 would do; 10000
// is the value S12 proved.
const CarveOutPriority = 10000

// acmeChallengePath is where an ACME HTTP-01 challenge is fetched, the one path TierPlan's plain
// listener answers (internal/server/plainchallenge.go).
const acmeChallengePath = "/.well-known/acme-challenge/"

// HostCert is one hostname's certificate as the renderer is given it: the chain and the private key as
// PEM, and a Reason when the certificate cannot be served. The certificate is servable when Reason is
// empty. The renderer never parses either PEM: the store validated them, and the layout needs only
// their bytes.
type HostCert struct {
	Hostname string
	ChainPEM []byte
	KeyPEM   []byte
	Reason   SkipReason
}

// String prints the hostname and the sizes, never the key (a HostCert holds a private key).
func (c HostCert) String() string {
	return fmt.Sprintf("cert %s (chain %d bytes, key %d bytes, reason %q)", c.Hostname, len(c.ChainPEM), len(c.KeyPEM), c.Reason)
}

// GoString is String, so %#v shows no key material either.
func (c HostCert) GoString() string { return c.String() }

// LogValue is a group of the hostname, the sizes and the reason, so a HostCert given to slog never
// writes a key.
func (c HostCert) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("hostname", c.Hostname),
		slog.Int("chain_bytes", len(c.ChainPEM)),
		slog.Int("key_bytes", len(c.KeyPEM)),
		slog.String("reason", string(c.Reason)),
	)
}

// servable reports whether the certificate can be written to a node.
func (c HostCert) servable() bool {
	return c.Reason == "" && len(c.ChainPEM) > 0 && len(c.KeyPEM) > 0
}

// Input is everything one render reads: the environment's snapshot, its certificates, and the version
// counters that travel beside the bundle. There is no clock, store or network handle (the package is
// pure).
type Input struct {
	Snapshot ingress.Snapshot
	Certs    []HostCert
	// CertsVersion is the certificate set version the Certs were read at.
	CertsVersion int64
	// OwnVersion is the version of the node's own settings; 0 until Phase 28.
	OwnVersion int64
	// Self is how the proxy reaches TierPlan itself, for the system claim's route (D-09). It is nil in
	// every environment but the one that carries TierPlan, and a system route with no Self is skipped
	// as self_not_attached.
	Self *SelfInput
}

// SelfInput is everything the self route needs (D-09, D-14). The renderer stays pure: the reconciler
// reads TierPlan's own certificate, its place on the self network and its URLs and fills this in.
type SelfInput struct {
	// Host is the system claim's hostname, the route host.
	Host string
	// BackendHost is TierPlan's name on the self network: on a swarm the service name from the task's
	// labels, on a standalone host the connect alias.
	BackendHost string
	// BackendPort is TierPlan's listen port.
	BackendPort int
	// ServerName is the name the proxy verifies TierPlan's certificate against: the host of the public
	// URL, a name both TierPlan's Let's Encrypt and its self-signed certificate carry (D-14).
	ServerName string
	// TrustPEM is the trust bundle the proxy verifies TierPlan against: the self-signed leaf and the
	// served chain, concatenated as PEM CERTIFICATE blocks. It is written as one file named by its
	// checksum, so the bundle ID changes whenever it does.
	TrustPEM []byte
	// ACMEHosts are the hosts of the public URL and the agent URL, for TierPlan's own HTTP-01 carve-out
	// on port 80 (D-12); empty when no carve-out is needed.
	ACMEHosts []string
	// BasePath is the public URL's path. It must be empty: the self route is host-rooted (Pitfall 9).
	BasePath string
	// Attached says TierPlan's own task or container is on the self network, so the proxy can reach it.
	// While it is false the system route is skipped as self_not_attached (D-03).
	Attached bool
}

// Skipped is a route that is not in the bundle, with the stable reason.
type Skipped struct {
	Hostname   string
	PathPrefix string
	Stack      string
	Service    string
	Reason     SkipReason
}

// Result is a verified bundle and the routes left out of it.
type Result struct {
	Bundle  ingressbundle.Bundle
	Skipped []Skipped
}

// The typed shape of the dynamic file. YAML is produced only by marshalling these (D-07): there is no
// template and no string-built YAML, so a stored value can never change the structure of the file.

type dynamicFile struct {
	TLS  tlsSection   `yaml:"tls"`
	HTTP *httpSection `yaml:"http,omitempty"`
}

type tlsSection struct {
	Options      map[string]tlsOptions `yaml:"options"`
	Certificates []certPair            `yaml:"certificates,omitempty"`
}

type tlsOptions struct {
	SNIStrict  bool   `yaml:"sniStrict"`
	MinVersion string `yaml:"minVersion"`
}

type certPair struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
}

type httpSection struct {
	Routers     map[string]router     `yaml:"routers,omitempty"`
	Services    map[string]service    `yaml:"services,omitempty"`
	Middlewares map[string]middleware `yaml:"middlewares,omitempty"`
	// ServersTransports holds the two transport types, serversTransport (t-user-https) and
	// selfServersTransport (t-self), keyed by name. The map is marshalled in key order.
	ServersTransports map[string]transport `yaml:"serversTransports,omitempty"`
}

// transport is a servers-transport type the dynamic file may carry. Only the two typed structs of this
// file implement it, so no other shape can reach the serversTransports mapping.
type transport interface{ isTransport() }

type router struct {
	Rule        string   `yaml:"rule"`
	EntryPoints []string `yaml:"entryPoints"`
	Service     string   `yaml:"service"`
	Priority    int      `yaml:"priority"`
	Middlewares []string `yaml:"middlewares,omitempty"`
	// TLS is set to an empty mapping on every route's router, so it encodes as `tls: {}`: the router is
	// served over TLS with the default options. It is nil only on the carve-out, which is plain HTTP on
	// the web entry point (D-12) and so has no tls key.
	TLS *struct{} `yaml:"tls,omitempty"`
}

// tlsDefault is the tls value of every router served over TLS: an empty mapping, `tls: {}`.
func tlsDefault() *struct{} { return &struct{}{} }

type service struct {
	LoadBalancer loadBalancer `yaml:"loadBalancer"`
}

type loadBalancer struct {
	Servers          []server `yaml:"servers"`
	ServersTransport string   `yaml:"serversTransport,omitempty"`
}

type server struct {
	URL string `yaml:"url"`
}

type middleware struct {
	StripPrefix *stripPrefix `yaml:"stripPrefix,omitempty"`
}

type stripPrefix struct {
	Prefixes []string `yaml:"prefixes"`
}

// serversTransport is how the proxy connects to a backend. The one user transport (D-13) skips
// verification of the backend's certificate and turns off HTTP/2: the backend is a user's own service
// on the overlay network and a route label carries no CA to verify it against. TierPlan's own route
// verifies instead (Phase 28). The Go field is SkipVerify and only its YAML tag spells Traefik's key,
// so scripts/insecure-tls-check.sh stays green.
type serversTransport struct {
	SkipVerify   bool `yaml:"insecureSkipVerify"`
	DisableHTTP2 bool `yaml:"disableHTTP2"`
}

func (serversTransport) isTransport() {}

// selfServersTransport is how the proxy connects to TierPlan itself (D-14): it verifies TierPlan's
// certificate by name against TierPlan's own trust file. It is a type of its own with no field that can
// write the skip-verification key, so the self route can never skip verification
// (TestSelfTransportHasNoSkipVerificationField). HTTP/2 stays on: S9 held an event feed and a shell for
// 21 minutes with h2 negotiated to the backend (28-S9-RESULT.md, A7), so disableHTTP2 is not written.
type selfServersTransport struct {
	ServerName string   `yaml:"serverName"`
	RootCAs    []string `yaml:"rootCAs"`
}

func (selfServersTransport) isTransport() {}

// UserHTTPSTransport is the name of the one transport every user https backend uses (D-13).
const UserHTTPSTransport = "t-user-https"

// SelfTransport is the name of the transport of TierPlan's own route (D-14).
const SelfTransport = "t-self"

// The Traefik names of TierPlan's own route. They hold no hash, so they cannot be taken by a stack
// route (whose names start "r-" and "s-").
const (
	selfRouterName  = "tierplan-self"
	selfServiceName = "svc-tierplan-self"
	// The carve-out router and its service, TierPlan's plain listener (D-12).
	acmeRouterName   = "tierplan-acme"
	plainServiceName = "svc-tierplan-plain"
)

// templateOpen is the opening delimiter of Go templates, which Traefik runs over dynamic files. It is
// built from two parts so this source never holds the pair literally.
const templateOpen = "{" + "{"

// Render turns one environment's snapshot and certificates into a verified bundle of the latest
// schema; it is RenderSchema(Latest, in).
//
// A route is rendered only when its hostname is claimed, its hostname has a servable certificate and
// its backend can be named; every other route is returned in Skipped with the reason. The dynamic file
// always holds the default TLS options with strict SNI, even when no route is rendered, so a node with
// an empty bundle still refuses a handshake for a name it does not serve (D-04, D-23).
//
// The same input always gives the same bytes and the same bundle ID: the maps are marshalled in key
// order and every list is sorted.
func Render(in Input) (Result, error) {
	return RenderSchema(Latest, in)
}

// renderSchema2 is the rendering of bundle schema 2.
func renderSchema2(in Input) (Result, error) {
	certs := make(map[string]HostCert, len(in.Certs))
	for _, c := range in.Certs {
		certs[c.Hostname] = c
	}
	claimed := make(map[string]struct{}, len(in.Snapshot.Claims))
	for _, c := range in.Snapshot.Claims {
		claimed[c.Hostname] = struct{}{}
	}

	routes := slices.Clone(in.Snapshot.Routes)
	slices.SortFunc(routes, func(a, b ingress.SnapshotRoute) int {
		return cmp.Or(cmp.Compare(a.Hostname, b.Hostname), cmp.Compare(a.PathPrefix, b.PathPrefix))
	})
	// Every route is checked, rendered or not: a refusal fails the whole render, so the node keeps its
	// last good bundle.
	for _, r := range routes {
		if err := refuseRoute(r); err != nil {
			return Result{}, err
		}
	}
	if err := refuseSelf(in.Self); err != nil {
		return Result{}, err
	}

	var (
		http = httpSection{
			Routers:     map[string]router{},
			Services:    map[string]service{},
			Middlewares: map[string]middleware{},
		}
		files    = map[string]ingressbundle.File{}
		pairs    = map[certPair]struct{}{}
		entries  []ingressbundle.RouteEntry
		names    []namedObject
		anyHTTPS bool
		selfTr   *selfServersTransport
		skipped  []Skipped
		rendered int
	)
	for _, r := range routes {
		skip := func(reason SkipReason) {
			skipped = append(skipped, Skipped{
				Hostname: r.Hostname, PathPrefix: r.PathPrefix, Stack: r.StackName, Service: r.Service, Reason: reason,
			})
		}
		cert, reason := selectRoute(r, claimed, certs, in.Self)
		if reason != "" {
			skip(reason)
			continue
		}
		if r.OwnerKind == ingress.OwnerSystem {
			// TierPlan's own route (D-09): selectRoute has checked that the self input is attached and
			// names this host. Its router and service have fixed names, and its transport verifies
			// TierPlan against the trust file (D-14).
			http.Routers[selfRouterName] = router{
				Rule:        selfRule(in.Self.Host),
				EntryPoints: []string{entryPointSecure},
				Service:     selfServiceName,
				Priority:    Priority(r.PathPrefix),
				TLS:         tlsDefault(),
			}
			http.Services[selfServiceName] = service{LoadBalancer: loadBalancer{
				Servers:          []server{{URL: selfURL(in.Self)}},
				ServersTransport: SelfTransport,
			}}
			trust := ingressbundle.CertsDir + "/" + ingressbundle.Checksum(in.Self.TrustPEM) + ".crt"
			files[trust] = ingressbundle.File{Path: trust, Data: slices.Clone(in.Self.TrustPEM)}
			selfTr = &selfServersTransport{
				ServerName: in.Self.ServerName,
				RootCAs:    []string{ingressbundle.CertRoot + "/" + path2name(trust)},
			}
			hash := routeHash(r.Hostname, r.PathPrefix)
			names = append(names,
				namedObject{kind: "router", name: selfRouterName, identity: hash, route: r},
				namedObject{kind: "service", name: selfServiceName, identity: hash, route: r},
			)
			addCert(files, pairs, cert)
			entries = append(entries, ingressbundle.RouteEntry{
				Router: selfRouterName, Hostname: r.Hostname, PathPrefix: r.PathPrefix, Service: r.Service, Hash: hash,
			})
			rendered++
			continue
		}
		url, ok := backendURL(r)
		if !ok {
			skip(SkipNoBackend)
			continue
		}

		svc := ServiceName(schemeOf(r), url)
		rname := RouterName(r.Hostname, r.PathPrefix)
		rt := router{
			Rule:        Rule(r.Hostname, r.PathPrefix),
			EntryPoints: []string{entryPointSecure},
			Service:     svc,
			Priority:    Priority(r.PathPrefix),
			TLS:         tlsDefault(),
		}
		if r.Strip {
			// The route forwards without its prefix through its own middleware (D-17). The root prefix was
			// refused above, so the prefix here is always a real one.
			mname := MiddlewareName(r.Hostname, r.PathPrefix)
			http.Middlewares[mname] = middleware{StripPrefix: &stripPrefix{Prefixes: []string{r.PathPrefix}}}
			rt.Middlewares = []string{mname}
		}
		http.Routers[rname] = rt
		if _, shared := http.Services[svc]; !shared {
			lb := loadBalancer{Servers: []server{{URL: url}}}
			if schemeOf(r) == ingress.SchemeHTTPS {
				lb.ServersTransport = UserHTTPSTransport
			}
			http.Services[svc] = service{LoadBalancer: lb}
		}
		anyHTTPS = anyHTTPS || schemeOf(r) == ingress.SchemeHTTPS
		hash := routeHash(r.Hostname, r.PathPrefix)
		names = append(names,
			namedObject{kind: "router", name: rname, identity: hash, route: r},
			namedObject{kind: "service", name: svc, identity: string(schemeOf(r)) + "\x00" + url, shared: true, route: r},
		)
		if r.Strip {
			names = append(names, namedObject{kind: "middleware", name: MiddlewareName(r.Hostname, r.PathPrefix), identity: hash, route: r})
		}

		addCert(files, pairs, cert)

		entries = append(entries, ingressbundle.RouteEntry{
			Router:     rname,
			Hostname:   r.Hostname,
			PathPrefix: r.PathPrefix,
			Stack:      r.StackName,
			Service:    r.Service,
			Hash:       hash,
		})
		rendered++
	}

	// TierPlan's own HTTP-01 carve-out (D-12), whether or not the self route is on: one router on web
	// for TierPlan's own names on the challenge path, to its plain listener over the self network.
	if hosts := carveOutHosts(in.Self); len(hosts) > 0 {
		http.Routers[acmeRouterName] = router{
			Rule:        carveOutRule(hosts),
			EntryPoints: []string{entryPointWeb},
			Service:     plainServiceName,
			Priority:    CarveOutPriority,
		}
		http.Services[plainServiceName] = service{LoadBalancer: loadBalancer{
			Servers: []server{{URL: plainURL(in.Self)}},
		}}
		names = append(names,
			namedObject{kind: "router", name: acmeRouterName, identity: acmeRouterName},
			namedObject{kind: "service", name: plainServiceName, identity: plainServiceName},
		)
		rendered++
	}

	doc := dynamicFile{TLS: tlsSection{
		Options: map[string]tlsOptions{"default": {SNIStrict: true, MinVersion: minTLSVersion}},
	}}
	for p := range pairs {
		doc.TLS.Certificates = append(doc.TLS.Certificates, p)
	}
	slices.SortFunc(doc.TLS.Certificates, func(a, b certPair) int {
		return cmp.Or(cmp.Compare(a.CertFile, b.CertFile), cmp.Compare(a.KeyFile, b.KeyFile))
	})
	if anyHTTPS || selfTr != nil {
		http.ServersTransports = map[string]transport{}
	}
	if anyHTTPS {
		http.ServersTransports[UserHTTPSTransport] = serversTransport{SkipVerify: true, DisableHTTP2: true}
	}
	if selfTr != nil {
		http.ServersTransports[SelfTransport] = *selfTr
	}
	if err := checkUnique(names); err != nil {
		return Result{}, err
	}
	if rendered > 0 {
		doc.HTTP = &http
	}

	yml, err := marshalDynamic(doc)
	if err != nil {
		return Result{}, err
	}
	out := make([]ingressbundle.File, 0, len(files)+1)
	for _, f := range files {
		out = append(out, f)
	}
	out = append(out, ingressbundle.File{Path: ingressbundle.DynamicFile, Data: yml})
	if err := checkNoTemplateDelimiters(out); err != nil {
		return Result{}, err
	}

	bundle, err := ingressbundle.Build(ingressbundle.Schema2, ingressbundle.Version{
		Config: in.Snapshot.Settings.ConfigVersion,
		Certs:  in.CertsVersion,
		Own:    in.OwnVersion,
	}, out, entries)
	if err != nil {
		return Result{}, fmt.Errorf("ingressrender: build bundle: %w", err)
	}

	slices.SortFunc(skipped, func(a, b Skipped) int {
		return cmp.Or(cmp.Compare(a.Hostname, b.Hostname), cmp.Compare(a.PathPrefix, b.PathPrefix))
	})
	return Result{Bundle: bundle, Skipped: skipped}, nil
}

// refuseRoute is the defence in depth behind the label parser. It checks every route, rendered or not
// (the parser refuses these values, so a hit means corrupted data, and failing the render keeps the
// node's last good bundle): no field may hold a backtick or a brace (D-08), the stack and service names
// that make up the backend URL hold only what compose allows in them (so no character can give that
// URL another host, userinfo, path, query or fragment: phase 21 review, WR-02), and a route that strips
// the root prefix would forward every request with no path (D-17).
func refuseRoute(r ingress.SnapshotRoute) error {
	for _, f := range []struct{ field, value string }{
		{"hostname", r.Hostname},
		{"pathPrefix", r.PathPrefix},
		{"service", r.Service},
		{"name", r.Name},
		{"stackName", r.StackName},
	} {
		if strings.ContainsAny(f.value, "`{}") {
			return &RefusedError{Code: CodeUnsafeValue, Hostname: r.Hostname, PathPrefix: r.PathPrefix, Field: f.field}
		}
	}
	for _, f := range []struct{ field, value string }{
		{"service", r.Service},
		{"stackName", r.StackName},
	} {
		if !urlLabel(f.value) {
			return &RefusedError{Code: CodeUnsafeValue, Hostname: r.Hostname, PathPrefix: r.PathPrefix, Field: f.field}
		}
	}
	if r.Strip && r.PathPrefix == "/" {
		return &RefusedError{Code: CodeStripRoot, Hostname: r.Hostname, PathPrefix: r.PathPrefix, Field: "strip"}
	}
	return nil
}

// urlLabel reports whether a stack or service name may go into a backend URL: empty (the route then has
// no backend and is skipped), or only ASCII letters, digits, '.', '_' and '-', the characters compose
// allows in a service name and a project name.
func urlLabel(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// addCert adds a hostname's certificate and key files to the bundle and its pair to the TLS section.
// Two routes of one hostname add the same files and the same pair.
func addCert(files map[string]ingressbundle.File, pairs map[certPair]struct{}, cert HostCert) {
	crt := ingressbundle.CertsDir + "/" + ingressbundle.Checksum(cert.ChainPEM) + ".crt"
	key := ingressbundle.CertsDir + "/" + ingressbundle.Checksum(cert.KeyPEM) + ".key"
	files[crt] = ingressbundle.File{Path: crt, Data: slices.Clone(cert.ChainPEM)}
	files[key] = ingressbundle.File{Path: key, Data: slices.Clone(cert.KeyPEM)}
	pairs[certPair{CertFile: ingressbundle.CertRoot + "/" + path2name(crt), KeyFile: ingressbundle.CertRoot + "/" + path2name(key)}] = struct{}{}
}

// selfRule is the self route's rule: the route host, minus the paths agents and ingress proxies dial
// (D-10), so Traefik answers 404 for those on this host. The paths are config's constants, the ones
// the agent and proxy addresses are built from.
func selfRule(host string) string {
	return "Host(`" + host + "`) && !PathPrefix(`" + config.AgentConnectPath + "`) && !PathPrefix(`" + config.IngressConnectPath + "`)"
}

// carveOutHosts is the carve-out's hosts: the self input's ACME hosts, lower-cased, sorted and without
// duplicates, or none when there is no self input, TierPlan is not on the self network or it has no
// ACME host (D-12). refuseSelf has already checked every host.
func carveOutHosts(s *SelfInput) []string {
	if s == nil || !s.Attached || len(s.ACMEHosts) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(s.ACMEHosts))
	for _, h := range s.ACMEHosts {
		hosts = append(hosts, strings.ToLower(h))
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// carveOutRule is the carve-out's rule: the hosts, exactly, and the challenge path (D-12, SELF-04).
func carveOutRule(hosts []string) string {
	matchers := make([]string, 0, len(hosts))
	for _, h := range hosts {
		matchers = append(matchers, "Host(`"+h+"`)")
	}
	host := matchers[0]
	if len(matchers) > 1 {
		host = "(" + strings.Join(matchers, " || ") + ")"
	}
	return host + " && PathPrefix(`" + acmeChallengePath + "`)"
}

// plainURL is TierPlan's plain listener on the self network: the listen port, which answers plain HTTP
// on the challenge path only and closes every other plain request (internal/server/plainchallenge.go).
func plainURL(s *SelfInput) string {
	return "http://" + s.BackendHost + ":" + strconv.Itoa(s.BackendPort)
}

// selfURL is TierPlan's address on the self network. TierPlan serves TLS on its listen port, so the
// scheme is always https.
func selfURL(s *SelfInput) string {
	return "https://" + s.BackendHost + ":" + strconv.Itoa(s.BackendPort)
}

// refuseSelf is refuseRoute for the self input. Every string of it that can reach the dynamic file
// holds no backtick or brace (D-08), the backend host holds only what a service name or an alias may
// (so it cannot give the backend URL another host, userinfo or path), and the base path is empty: the
// self route is host-rooted and the renderer never rewrites a prefix (Pitfall 9). While TierPlan is
// attached, the input must also be complete enough to render: a backend host, a port in range and no
// empty ACME host (the carve-out needs those, D-12), and when it names a host (the self route), a
// server name and a trust bundle of at least one PEM certificate. An attached input with no host is
// the carve-out alone, with no system claim. Not-attached input is allowed to be incomplete (nothing
// of it is rendered), but its strings are checked all the same. A nil input is fine.
func refuseSelf(s *SelfInput) error {
	if s == nil {
		return nil
	}
	refuse := func(code, field string) error {
		return &RefusedError{Code: code, Hostname: s.Host, PathPrefix: "/", Field: "self " + field}
	}
	fields := []struct{ field, value string }{
		{"host", s.Host},
		{"backendHost", s.BackendHost},
		{"serverName", s.ServerName},
		{"basePath", s.BasePath},
	}
	for _, h := range s.ACMEHosts {
		fields = append(fields, struct{ field, value string }{"acmeHost", h})
	}
	for _, f := range fields {
		if strings.ContainsAny(f.value, "`{}") {
			return refuse(CodeUnsafeValue, f.field)
		}
	}
	if !urlLabel(s.BackendHost) {
		return refuse(CodeUnsafeValue, "backendHost")
	}
	if s.BasePath != "" {
		return refuse(CodeSelfInput, "basePath")
	}
	if !s.Attached {
		return nil
	}
	switch {
	case s.BackendHost == "":
		return refuse(CodeSelfInput, "backendHost")
	case s.BackendPort <= 0 || s.BackendPort > maxPort:
		return refuse(CodeSelfInput, "backendPort")
	case slices.Contains(s.ACMEHosts, ""):
		return refuse(CodeSelfInput, "acmeHost")
	case s.Host == "":
		return nil
	case s.ServerName == "":
		return refuse(CodeSelfInput, "serverName")
	case !hasCertificateBlock(s.TrustPEM):
		return refuse(CodeSelfInput, "trustPEM")
	}
	return nil
}

// hasCertificateBlock reports whether data holds at least one PEM CERTIFICATE block. The renderer does
// not parse the certificate itself (the server built the bundle from certificates it serves); this only
// keeps an empty or key-only bundle from being written as the proxy's trust.
func hasCertificateBlock(data []byte) bool {
	for rest := data; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return false
		}
		if b.Type == "CERTIFICATE" {
			return true
		}
	}
}

// namedObject is one Traefik router, service or middleware name in a bundle, with what makes it the
// same object as another of that name: the route it came from, and for a service the backend.
type namedObject struct {
	kind, name string
	// identity is what two objects must share to be one object: a route hash, or a scheme and URL.
	identity string
	// shared says two routes may legitimately use the one object (a service), when identities match.
	shared bool
	route  ingress.SnapshotRoute
}

// checkUnique refuses a bundle in which two different objects of one kind share a name (D-18). Names
// are hash-derived and 48 bits long, so this is not expected to fire; a merged router would silently
// serve one route's backend for another's host, so the render fails instead. The error names the
// second route.
func checkUnique(objs []namedObject) error {
	type key struct{ kind, name string }
	seen := make(map[key]namedObject, len(objs))
	for _, o := range objs {
		k := key{o.kind, o.name}
		if prev, ok := seen[k]; ok && (!o.shared || prev.identity != o.identity) {
			return &RefusedError{
				Code: CodeNameCollision, Hostname: o.route.Hostname, PathPrefix: o.route.PathPrefix,
				Field: o.kind + " " + o.name,
			}
		}
		seen[k] = o
	}
	return nil
}

// checkNoTemplateDelimiters refuses any output file that holds two consecutive opening braces.
// Traefik executes dynamic files as Go templates, so that pair could run user text; no value that
// reaches a file may hold a brace (refuseRoute) and this checks the bytes themselves (D-08).
func checkNoTemplateDelimiters(files []ingressbundle.File) error {
	for _, f := range files {
		if bytes.Contains(f.Data, []byte(templateOpen)) {
			return &RefusedError{Code: CodeTemplateDelimiter, Field: f.Path}
		}
	}
	return nil
}

// selectRoute decides whether a route is served, in this order (D-09, D-10, D-12, D-28). It returns the
// route's certificate and an empty reason when the route is served, and the stable skip reason when it
// is not.
//
//  1. A route whose hostname is not claimed has no owner to name a backend. The store makes this
//     impossible (ingress_routes references ingress_hostnames), so it is a defensive no_backend; the
//     renderer is pure and logs nothing.
//  2. The hostname's certificate must be servable: none at all is no_certificate; one with a reason
//     keeps that reason (Serving reports a staging, issuing or expiring certificate as
//     certificate_not_ready); one with no reason but no key bytes is certificate_not_ready too, never
//     served. The same answer applies to every route of the hostname, so a host is served whole or not
//     at all and a name with no usable certificate fails at the handshake.
//  3. A system-owned claim is TierPlan's own route (D-09). It is served only when the render has a self
//     input that is attached to the self network and names this hostname; otherwise it is
//     self_not_attached, never no_backend (D-03). That is the fourth reason code, added on purpose: the
//     route's condition says TierPlan is not on the self network yet, which no_backend would not.
//  4. A stack with no name cannot be addressed: no_backend. A swarm stack's service is reached by its
//     service DNS name and a standalone stack's by its alias on the host's ingress bridge (Phase 25,
//     D-15).
func selectRoute(r ingress.SnapshotRoute, claimed map[string]struct{}, certs map[string]HostCert, self *SelfInput) (HostCert, SkipReason) {
	if _, ok := claimed[r.Hostname]; !ok {
		return HostCert{}, SkipNoBackend
	}
	cert, ok := certs[r.Hostname]
	switch {
	case !ok:
		return HostCert{}, SkipNoCertificate
	case cert.Reason != "":
		return HostCert{}, cert.Reason
	case !cert.servable():
		return HostCert{}, SkipCertificateNotReady
	}
	if r.OwnerKind == ingress.OwnerSystem {
		if self == nil || !self.Attached || self.Host != r.Hostname {
			return HostCert{}, SkipSelfNotAttached
		}
		return cert, ""
	}
	if r.OwnerKind != ingress.OwnerStack || !namesABackend(r.Target) || r.StackName == "" {
		return HostCert{}, SkipNoBackend
	}
	return cert, ""
}

// schemeOf is the scheme a route's backend is spoken to over; an unset scheme is http.
func schemeOf(r ingress.SnapshotRoute) ingress.Scheme {
	if r.Scheme == "" {
		return ingress.SchemeHTTP
	}
	return r.Scheme
}

// maxPort is the highest TCP port.
const maxPort = 65535

// backendURL is the URL of a route's backend, or false when it cannot be named (including a port outside
// 1 to 65535). A swarm service is reached by its DNS name, <stack>_<service>, on the proxy's overlay
// network (D-12); a standalone stack's service by its alias on the host's ingress bridge, the one name
// the attach gives it (D-15).
func backendURL(r ingress.SnapshotRoute) (string, bool) {
	scheme := schemeOf(r)
	if scheme != ingress.SchemeHTTP && scheme != ingress.SchemeHTTPS {
		return "", false
	}
	if !namesABackend(r.Target) || r.StackName == "" || r.Service == "" || r.Port <= 0 || r.Port > maxPort {
		return "", false
	}
	port := strconv.Itoa(r.Port)
	if r.Target == ingress.TargetStandalone {
		return string(scheme) + "://" + ingress.Alias(r.StackName, r.Service) + ":" + port, true
	}
	return string(scheme) + "://" + r.StackName + "_" + r.Service + ":" + port, true
}

// namesABackend reports whether a stack route on this target has a backend name: a swarm target and a
// standalone target do, anything else does not.
func namesABackend(t ingress.Target) bool {
	return t == ingress.TargetSwarm || t == ingress.TargetStandalone
}

// path2name is the file name of a bundle path under certs/.
func path2name(p string) string {
	return p[len(ingressbundle.CertsDir)+1:]
}

// marshalDynamic encodes the typed document with two-space indentation.
func marshalDynamic(doc dynamicFile) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("ingressrender: encode dynamic file: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("ingressrender: encode dynamic file: %w", err)
	}
	return buf.Bytes(), nil
}
