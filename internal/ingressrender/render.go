// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"bytes"
	"cmp"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"go.yaml.in/yaml/v3"

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
	// SkipNoBackend: the route's backend cannot be named (no owning stack, not a swarm target, no
	// port, or a scheme that is neither http nor https).
	SkipNoBackend SkipReason = "no_backend"
)

// The entry point every router is attached to, and the TLS floor of the default options (D-04). The
// web entry point only redirects and never carries a router (RTE-02, PROH-21-03).
const (
	entryPointSecure = "websecure"
	minTLSVersion    = "VersionTLS12"
)

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
	Routers           map[string]router           `yaml:"routers,omitempty"`
	Services          map[string]service          `yaml:"services,omitempty"`
	Middlewares       map[string]middleware       `yaml:"middlewares,omitempty"`
	ServersTransports map[string]serversTransport `yaml:"serversTransports,omitempty"`
}

type router struct {
	Rule        string   `yaml:"rule"`
	EntryPoints []string `yaml:"entryPoints"`
	Service     string   `yaml:"service"`
	Priority    int      `yaml:"priority"`
	Middlewares []string `yaml:"middlewares,omitempty"`
	// TLS is an empty mapping with no omitempty, so it encodes as `tls: {}`: the router is served
	// over TLS with the default options.
	TLS struct{} `yaml:"tls"`
}

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

type serversTransport struct {
	DisableHTTP2 bool `yaml:"disableHTTP2,omitempty"`
}

// Render turns one environment's snapshot and certificates into a verified bundle.
//
// A route is rendered only when its hostname is claimed, its hostname has a servable certificate and
// its backend can be named; every other route is returned in Skipped with the reason. The dynamic file
// always holds the default TLS options with strict SNI, even when no route is rendered, so a node with
// an empty bundle still refuses a handshake for a name it does not serve (D-04, D-23).
//
// The same input always gives the same bytes and the same bundle ID: the maps are marshalled in key
// order and every list is sorted.
func Render(in Input) (Result, error) {
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

	var (
		http = httpSection{
			Routers:     map[string]router{},
			Services:    map[string]service{},
			Middlewares: map[string]middleware{},
		}
		files    = map[string]ingressbundle.File{}
		pairs    = map[certPair]struct{}{}
		entries  []ingressbundle.RouteEntry
		skipped  []Skipped
		rendered int
	)
	for _, r := range routes {
		skip := func(reason SkipReason) {
			skipped = append(skipped, Skipped{
				Hostname: r.Hostname, PathPrefix: r.PathPrefix, Stack: r.StackName, Service: r.Service, Reason: reason,
			})
		}
		cert, reason := selectRoute(r, claimed, certs)
		if reason != "" {
			skip(reason)
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
			http.Services[svc] = service{LoadBalancer: loadBalancer{Servers: []server{{URL: url}}}}
		}

		crt := ingressbundle.CertsDir + "/" + ingressbundle.Checksum(cert.ChainPEM) + ".crt"
		key := ingressbundle.CertsDir + "/" + ingressbundle.Checksum(cert.KeyPEM) + ".key"
		files[crt] = ingressbundle.File{Path: crt, Data: slices.Clone(cert.ChainPEM)}
		files[key] = ingressbundle.File{Path: key, Data: slices.Clone(cert.KeyPEM)}
		pairs[certPair{CertFile: ingressbundle.CertRoot + "/" + path2name(crt), KeyFile: ingressbundle.CertRoot + "/" + path2name(key)}] = struct{}{}

		entries = append(entries, ingressbundle.RouteEntry{
			Router:     rname,
			Hostname:   r.Hostname,
			PathPrefix: r.PathPrefix,
			Stack:      r.StackName,
			Service:    r.Service,
			Hash:       routeHash(r.Hostname, r.PathPrefix),
		})
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

	bundle, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{
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

// refuseRoute is the defence in depth behind the label parser (D-17): a route that strips the root
// prefix would forward every request with no path, so it is never rendered.
func refuseRoute(r ingress.SnapshotRoute) error {
	if r.Strip && r.PathPrefix == "/" {
		return &RefusedError{Code: CodeStripRoot, Hostname: r.Hostname, PathPrefix: r.PathPrefix, Field: "strip"}
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
//  3. A system-owned claim and a standalone target have no backend until Phases 28 and 25, and a stack
//     with no name cannot be addressed: no_backend. There is no fourth reason code.
func selectRoute(r ingress.SnapshotRoute, claimed map[string]struct{}, certs map[string]HostCert) (HostCert, SkipReason) {
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
	if r.OwnerKind != ingress.OwnerStack || r.Target != ingress.TargetSwarm || r.StackName == "" {
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

// backendURL is the URL of a route's backend, or false when it cannot be named. A swarm service is
// reached by its DNS name, <stack>_<service>, on the proxy's overlay network (D-12).
func backendURL(r ingress.SnapshotRoute) (string, bool) {
	scheme := schemeOf(r)
	if scheme != ingress.SchemeHTTP && scheme != ingress.SchemeHTTPS {
		return "", false
	}
	if r.Target != ingress.TargetSwarm || r.StackName == "" || r.Service == "" || r.Port <= 0 {
		return "", false
	}
	return string(scheme) + "://" + r.StackName + "_" + r.Service + ":" + strconv.Itoa(r.Port), true
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
