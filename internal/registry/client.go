// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/scheme"
	"github.com/regclient/regclient/scheme/reg"
	"github.com/regclient/regclient/types/ref"
)

const (
	// callTimeout bounds every call the server makes to a registry.
	callTimeout = 30 * time.Second
	// reqConcurrent is the number of requests one client has in flight to its registry.
	reqConcurrent = 3
	// manifestPushMax and manifestPullMax cap the size of a manifest the client will send or
	// read, so a hostile registry cannot make the server buffer an unbounded answer (T-05-42).
	manifestPushMax = 1 << 20
	manifestPullMax = 8 << 20
	// maxRetries and the delays keep a refused or slow registry from stalling a call.
	maxRetries = 2
	// clientCacheMax is how many clients the cache keeps before it starts again.
	clientCacheMax = 256
	// probeBodyMax is how much of a /v2/ answer the probe reads and throws away.
	probeBodyMax = 4 << 10
)

// HostOptions is the address and sign-in of one registry host.
type HostOptions struct {
	// Host is the registry host, with a port when it has one: docker.io, ghcr.io or
	// registry.example.org:5000.
	Host string
	// Username and Password are the stored sign-in. Empty for an anonymous client.
	Username string
	Password string //nolint:gosec // G117: a plaintext sign-in held in memory for one call, never marshalled

	// tlsMode and caPEM are set by tests and, by the self-hosted registry options, from the
	// credential's own settings. The zero value is verified TLS against the system roots.
	tlsMode config.TLSConf
	caPEM   string
}

// Client talks to one registry host as one sign-in. It is safe for concurrent use. It reads
// no file on the server: the sign-in is the one it was given, every connection goes through
// the egress policy, and the registry it talks to is always the one it was given.
type Client struct {
	rc   *regclient.RegClient
	host string
	// dial is the name connections are made to: registry-1.docker.io for Docker Hub.
	dial   string
	scheme string
	// probe makes the one raw request regclient does not report: whether the registry asks
	// for a login at all.
	probe *http.Client
}

// NewClient returns a client for one registry host. Its connections dial through egress, its
// requests carry the user agent bayplan/<version>, and what regclient logs goes through log
// with request and response headers removed.
func NewClient(o HostOptions, egress EgressPolicy, version string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	h := config.HostNewName(o.Host)
	h.User, h.Pass = o.Username, o.Password
	h.TLS = o.tlsMode
	if h.TLS == config.TLSUndefined {
		h.TLS = config.TLSEnabled
	}
	h.RegCert = o.caPEM
	h.ReqConcurrent = reqConcurrent
	ua := "bayplan/" + version
	rc := regclient.New(
		regclient.WithConfigHost(*h),
		regclient.WithRegOpts(
			reg.WithTransport(egress.Transport()),
			reg.WithManifestMax(manifestPushMax, manifestPullMax),
			reg.WithRetryLimit(maxRetries),
			reg.WithDelay(200*time.Millisecond, 2*time.Second),
		),
		regclient.WithUserAgent(ua),
		regclient.WithSlog(slog.New(&headerlessHandler{inner: log.Handler()})),
	)

	c := &Client{rc: rc, host: o.Host, dial: h.Hostname, scheme: "https"}
	if c.dial == "" {
		c.dial = o.Host
	}
	if h.TLS == config.TLSDisabled {
		c.scheme = "http"
	}
	t := egress.Transport()
	if o.caPEM != "" {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM([]byte(o.caPEM)) {
			t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
		}
	}
	c.probe = &http.Client{
		Transport: &userAgentTransport{base: t, ua: ua},
		Timeout:   callTimeout,
		// A registry that redirects its /v2/ answer is reported, not followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

// Host is the registry host the client talks to, as it was given.
func (c *Client) Host() string { return c.host }

// ProbeResult is what the registry answered to an unauthenticated GET /v2/.
type ProbeResult struct {
	// Status is the HTTP status.
	Status int
	// AsksLogin is true for a 401 or 403: the registry wants a sign-in. A 200 means it does
	// not.
	AsksLogin bool
}

// Probe sends GET /v2/ with no credentials, once, and reports the status. A 401 is
// "reachable, wants a login", never a failure (RESEARCH Pitfall 7). Only a 200, 401 or 403
// is an answer from a registry; any other status is returned as ErrNotARegistry. Redirects are
// not followed.
func (c *Client) Probe(ctx context.Context) (ProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.scheme+"://"+c.dial+"/v2/", http.NoBody)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("registry: build the probe of %s: %w", c.host, err)
	}
	resp, err := c.probe.Do(req)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("registry: probe %s: %w", c.host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeBodyMax))
	switch resp.StatusCode {
	case http.StatusOK:
		return ProbeResult{Status: resp.StatusCode}, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ProbeResult{Status: resp.StatusCode, AsksLogin: true}, nil
	}
	return ProbeResult{Status: resp.StatusCode}, fmt.Errorf("registry: probe %s: status %d: %w", c.host, resp.StatusCode, ErrNotARegistry)
}

// Ping signs in the way the registry's own flow does, with this client's credentials, and
// asks it for /v2/. It succeeds when the registry accepts the sign-in.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if _, err := c.rc.Ping(ctx, ref.Ref{Scheme: "reg", Registry: c.host}); err != nil {
		return fmt.Errorf("registry: sign in at %s: %w", c.host, err)
	}
	return nil
}

// TagList lists at most limit tag names of a repository (a path below the host, like
// acme/app). limit zero asks for the registry's own page.
func (c *Client) TagList(ctx context.Context, repo string, limit int) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var opts []scheme.TagOpts
	if limit > 0 {
		opts = append(opts, scheme.WithTagLimit(limit))
	}
	r := ref.Ref{Scheme: "reg", Registry: c.host, Repository: repo}
	l, err := c.rc.TagList(ctx, r, opts...)
	if err != nil {
		return nil, fmt.Errorf("registry: list the tags of %s/%s: %w", c.host, repo, err)
	}
	tags, err := l.GetTags()
	if err != nil {
		return nil, fmt.Errorf("registry: read the tags of %s/%s: %w", c.host, repo, err)
	}
	return tags, nil
}

// userAgentTransport sets the user agent of the probe.
type userAgentTransport struct {
	base http.RoundTripper
	ua   string
}

func (t *userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", t.ua)
	return t.base.RoundTrip(r)
}

// headerlessHandler passes a record to the logger the server uses with every attribute that
// carries request or response headers removed, so a bearer token or a Basic header never
// reaches a log (T-05-43).
type headerlessHandler struct {
	inner slog.Handler
}

func (h *headerlessHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *headerlessHandler) Handle(ctx context.Context, r slog.Record) error {
	kept := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if !isHeaderAttr(a) {
			kept.AddAttrs(a)
		}
		return true
	})
	return h.inner.Handle(ctx, kept)
}

func (h *headerlessHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if !isHeaderAttr(a) {
			out = append(out, a)
		}
	}
	return &headerlessHandler{inner: h.inner.WithAttrs(out)}
}

func (h *headerlessHandler) WithGroup(name string) slog.Handler {
	return &headerlessHandler{inner: h.inner.WithGroup(name)}
}

func isHeaderAttr(a slog.Attr) bool {
	k := a.Key
	return len(k) >= len("headers") && k[len(k)-len("headers"):] == "headers"
}

// ClientCache keeps the clients of stored credentials, so a bearer token is reused between
// calls and an edit to a credential makes a new client. An anonymous client is kept per host.
type ClientCache struct {
	egress  EgressPolicy
	version string
	log     *slog.Logger

	mu      sync.Mutex
	clients map[string]*Client
}

// NewClientCache returns an empty cache whose clients dial through egress.
func NewClientCache(egress EgressPolicy, version string, log *slog.Logger) *ClientCache {
	return &ClientCache{egress: egress, version: version, log: log, clients: map[string]*Client{}}
}

// Egress is the policy the cache's clients dial through.
func (cc *ClientCache) Egress() EgressPolicy { return cc.egress }

// New returns a client that is not kept: for sign-ins that were typed and not saved.
func (cc *ClientCache) New(o HostOptions) *Client {
	return NewClient(o, cc.egress, cc.version, cc.log)
}

// ForCredential returns the client of a stored credential. The key holds the credential's
// ID and the time it was last changed, so an edit never reuses the old sign-in.
func (cc *ClientCache) ForCredential(id string, updatedAt time.Time, o HostOptions) *Client {
	return cc.get("cred:"+id+":"+updatedAt.UTC().Format(time.RFC3339Nano), o)
}

// Anonymous returns the client with no sign-in for a host.
func (cc *ClientCache) Anonymous(host string) *Client {
	return cc.get("anon:"+host, HostOptions{Host: host})
}

func (cc *ClientCache) get(key string, o HostOptions) *Client {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if c, ok := cc.clients[key]; ok {
		return c
	}
	if len(cc.clients) >= clientCacheMax {
		cc.clients = map[string]*Client{}
	}
	c := NewClient(o, cc.egress, cc.version, cc.log)
	cc.clients[key] = c
	return c
}
