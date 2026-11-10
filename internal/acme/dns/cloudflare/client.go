// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package cloudflare is TierPlan's own client for the part of the Cloudflare v4 API a DNS-01
// challenge needs: find the zone that holds a name, create a TXT record carrying a TierPlan
// comment, read it back and delete it, each by the record ID Cloudflare returned.
//
// It does not use lego's Cloudflare provider, which keeps record IDs in memory, sets no comment and
// finds the zone through the system resolver. It sends Authorization: Bearer and nothing else, so a
// Global API Key is rejected rather than used.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/ianglennon/tierplan/internal/acme/dns"
)

const (
	// DefaultBaseURL is the Cloudflare v4 API.
	DefaultBaseURL = "https://api.cloudflare.com/client/v4"

	providerName = "cloudflare"
	// bodyMax caps what is read of any response, so a hostile or broken answer cannot make the
	// server buffer an unbounded body.
	bodyMax = 1 << 20
	// defaultTimeout bounds a call when the HTTP client given has no timeout of its own.
	defaultTimeout = 15 * time.Second
	// recordTTL is the TTL of a challenge record: 60 seconds, the shortest Cloudflare allows
	// for a proxied-or-not record on every plan.
	recordTTL = 60

	// retryAfterDefault is the wait assumed when a 429 gives no usable Retry-After, and
	// retryAfterMax is the longest wait honoured, so a hostile value cannot park a run for hours.
	retryAfterDefault = 60 * time.Second
	retryAfterMax     = 5 * time.Minute
)

// The operations a failure can come from.
const (
	opFindZone = "find zone"
	opCreate   = "create record"
	opRead     = "read record"
	opDelete   = "delete record"
)

// Cloudflare error codes the classification keys on (RESEARCH Pattern 6). The first group was read
// from the live API with dummy tokens; the others are documented or reported and fall back safely.
const (
	codeBadAuthHeader  = 6003  // malformed Authorization header (chain code 6111)
	codeBadAuthFormat  = 6111  // "Invalid format for Authorization header"
	codeInvalidToken   = 9109  // "Invalid access token"
	codeIdentical      = 81058 // an identical record already exists
	codeCNAMEExists    = 81053 // a CNAME record at that name
	codeCNAMEExistsToo = 81054 // a CNAME record at that name
	codeNoRecord       = 81044 // the record does not exist
)

// Options configure New.
type Options struct {
	// BaseURL is the API root. Empty means DefaultBaseURL. It is a Go option and nothing else, so
	// no setting or environment variable can send the token somewhere else.
	BaseURL string
	// HTTPClient makes the requests. Nil means a client on the default transport. Its copy used
	// here never follows a redirect and has a 15 second timeout when it has none.
	HTTPClient *http.Client
	// UserAgent is sent on every request. Empty means "TierPlan".
	UserAgent string
}

// New returns the Factory that builds a Cloudflare provider from an API token.
func New(opts Options) dns.Factory {
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	hc := &http.Client{}
	if opts.HTTPClient != nil {
		cp := *opts.HTTPClient
		hc = &cp
	}
	// The bearer token must never follow a redirect to another host.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if hc.Timeout == 0 {
		hc.Timeout = defaultTimeout
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "TierPlan"
	}
	return func(token string) dns.Provider {
		return &provider{base: base, hc: hc, ua: ua, token: token, zoneNames: map[string]string{}, hosts: map[dns.RecordRef]string{}}
	}
}

type provider struct {
	base  string
	hc    *http.Client
	ua    string
	token string

	// zoneNames and hosts remember what this provider learned, so an error from a later call
	// that holds only IDs can still name the zone and the host.
	mu        sync.Mutex
	zoneNames map[string]string
	hosts     map[dns.RecordRef]string
}

func (p *provider) Name() string { return providerName }

// apiError is one entry of Cloudflare's errors array.
type apiError struct {
	Code       int        `json:"code"`
	Message    string     `json:"message"`
	ErrorChain []apiError `json:"error_chain"`
}

// envelope is the shape every Cloudflare answer has.
type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

// response is one answer after the body was read and decoded.
type response struct {
	status     int
	header     http.Header
	env        envelope
	decodeFail bool
}

// do makes one request. A transport failure is returned as a *dns.Error already; an HTTP answer of
// any status is returned for the caller to classify.
func (p *provider) do(ctx context.Context, op, method, path string, query url.Values, body any) (response, error) {
	u := p.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return response{}, &dns.Error{Provider: providerName, Op: op, Err: err}
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return response{}, &dns.Error{Provider: providerName, Op: op, Class: dns.ClassUnavailable, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("User-Agent", p.ua)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := p.hc.Do(req)
	if err != nil {
		// The cause is kept for errors.Is and errors.As and is never part of Error(); the
		// request itself is not wrapped.
		return response{}, &dns.Error{Provider: providerName, Op: op, Class: dns.ClassUnavailable, Err: errorCause(err)}
	}
	defer func() { _ = res.Body.Close() }()

	out := response{status: res.StatusCode, header: res.Header}
	raw, err := io.ReadAll(io.LimitReader(res.Body, bodyMax+1))
	if err != nil || len(raw) > bodyMax {
		out.decodeFail = true
		return out, nil
	}
	if err := json.Unmarshal(raw, &out.env); err != nil {
		out.decodeFail = true
	}
	return out, nil
}

// errorCause is err without the *url.Error wrapper, which holds the request URL.
func errorCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// failure turns an answer that is not a success into one *dns.Error, keyed on the HTTP status and
// the Cloudflare error codes. An answer this build does not know gets the safest class its status
// allows, and a 4xx with no known meaning gets none (Classify then reports it as internal). The text
// of the error holds the operation, status and code only; no message, header or body is kept.
func (p *provider) failure(op string, res response, host, zone string) error {
	e := &dns.Error{Provider: providerName, Op: op, Status: res.status, Host: host, Zone: zone}
	if len(res.env.Errors) > 0 {
		e.Code = res.env.Errors[0].Code
	}
	has := func(codes ...int) bool {
		for _, ae := range res.env.Errors {
			if slices.Contains(codes, ae.Code) {
				return true
			}
			for _, c := range ae.ErrorChain {
				if slices.Contains(codes, c.Code) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case res.status == http.StatusTooManyRequests:
		e.Class = dns.ClassRateLimited
		e.RetryAfter = retryAfter(res.header.Get("Retry-After"))
	case res.status >= 500 || (res.status >= 300 && res.status < 400):
		e.Class = dns.ClassUnavailable
	case res.status == http.StatusUnauthorized:
		e.Class = dns.ClassTokenInvalid
	case res.decodeFail, res.status >= 200 && res.status < 300:
		// An answer that cannot be read, or that says success:false under a 2xx.
		e.Class = dns.ClassUnavailable
	case has(codeIdentical):
		e.Err = dns.ErrIdentical
	case has(codeCNAMEExists, codeCNAMEExistsToo):
		e.Class = dns.ClassDelegationTarget
	case res.status == http.StatusBadRequest && has(codeBadAuthHeader, codeBadAuthFormat):
		e.Class = dns.ClassTokenInvalid
	case res.status == http.StatusForbidden && has(codeInvalidToken):
		e.Class = dns.ClassTokenInvalid
	case res.status == http.StatusForbidden:
		// A token that lacks Zone:Zone:Read or Zone:DNS:Edit, or a code this build does not know.
		e.Class = dns.ClassTokenScope
	case res.status == http.StatusNotFound && has(codeNoRecord):
		e.Class = dns.ClassUnavailable
	case res.status == http.StatusNotFound && (op == opCreate || op == opRead):
		// The zone ID is gone (codes 7000 and 7003, or no code).
		e.Class = dns.ClassZoneNotFound
	}
	return e
}

// retryAfter reads a Retry-After given as whole seconds. Anything else, or a wait of nothing, is
// retryAfterDefault, and a long wait is cut to retryAfterMax.
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return retryAfterDefault
	}
	return min(time.Duration(n)*time.Second, retryAfterMax)
}

func (p *provider) ok(res response) bool {
	return res.status >= 200 && res.status < 300 && !res.decodeFail && res.env.Success
}

type zoneJSON struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	NameServers []string `json:"name_servers"`
}

// Zone walks the candidates from fqdn down to its registrable domain and returns the first zone the
// token can see.
func (p *provider) Zone(ctx context.Context, fqdn string) (dns.Zone, error) {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fqdn), "."))
	floor, err := publicsuffix.EffectiveTLDPlusOne(name)
	if err != nil || !(name == floor || strings.HasSuffix(name, "."+floor)) {
		return dns.Zone{}, &dns.Error{Provider: providerName, Op: opFindZone, Class: dns.ClassZoneNotFound, Host: name}
	}
	for candidate := name; ; {
		res, err := p.do(ctx, opFindZone, http.MethodGet, "/zones", url.Values{"name": {candidate}}, nil)
		if err != nil {
			return dns.Zone{}, err
		}
		if !p.ok(res) {
			return dns.Zone{}, p.failure(opFindZone, res, name, "")
		}
		var zones []zoneJSON
		if err := json.Unmarshal(res.env.Result, &zones); err != nil {
			return dns.Zone{}, &dns.Error{Provider: providerName, Op: opFindZone, Status: res.status, Class: dns.ClassUnavailable}
		}
		for _, z := range zones {
			if strings.EqualFold(strings.TrimSuffix(z.Name, "."), candidate) {
				zone := dns.Zone{ID: z.ID, Name: strings.ToLower(candidate), NameServers: z.NameServers}
				p.mu.Lock()
				p.zoneNames[zone.ID] = zone.Name
				p.mu.Unlock()
				return zone, nil
			}
		}
		if candidate == floor {
			break
		}
		candidate = candidate[strings.IndexByte(candidate, '.')+1:]
	}
	return dns.Zone{}, p.noZone(ctx, name)
}

// noZone explains why no candidate matched. A token that lists no zone at all is not scoped to any
// (it lacks Zone:Zone:Read or was narrowed to nothing); one that lists some zones is scoped to
// others, so the name is simply in none of them.
func (p *provider) noZone(ctx context.Context, name string) error {
	res, err := p.do(ctx, opFindZone, http.MethodGet, "/zones", url.Values{"per_page": {"5"}}, nil)
	if err != nil {
		return err
	}
	if !p.ok(res) {
		return p.failure(opFindZone, res, name, "")
	}
	var zones []zoneJSON
	if err := json.Unmarshal(res.env.Result, &zones); err != nil {
		return &dns.Error{Provider: providerName, Op: opFindZone, Status: res.status, Class: dns.ClassUnavailable, Host: name}
	}
	if len(zones) == 0 {
		return &dns.Error{Provider: providerName, Op: opFindZone, Status: res.status, Class: dns.ClassTokenScope, Host: name}
	}
	return &dns.Error{Provider: providerName, Op: opFindZone, Class: dns.ClassZoneNotFound, Host: name}
}

// known is the zone name and host this provider learned for ref, for an error that holds IDs only.
// It falls back to the zone ID, and to the zone for the host.
func (p *provider) known(ref dns.RecordRef) (zone, host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	zone = p.zoneNames[ref.ZoneID]
	if zone == "" {
		zone = ref.ZoneID
	}
	host = p.hosts[ref]
	if host == "" {
		host = zone
	}
	return zone, host
}

type recordJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Comment string `json:"comment"`
}

// CreateTXT writes the record with its comment and returns the ID Cloudflare gave it.
func (p *provider) CreateTXT(ctx context.Context, z dns.Zone, fqdn, value, comment string) (dns.RecordRef, error) {
	body := map[string]any{
		"type":    "TXT",
		"name":    fqdn,
		"content": `"` + value + `"`,
		"ttl":     recordTTL,
		"comment": comment,
	}
	res, err := p.do(ctx, opCreate, http.MethodPost, "/zones/"+url.PathEscape(z.ID)+"/dns_records", nil, body)
	if err != nil {
		return dns.RecordRef{}, err
	}
	if !p.ok(res) {
		return dns.RecordRef{}, p.failure(opCreate, res, fqdn, z.Name)
	}
	var rec recordJSON
	if err := json.Unmarshal(res.env.Result, &rec); err != nil || rec.ID == "" {
		return dns.RecordRef{}, &dns.Error{Provider: providerName, Op: opCreate, Status: res.status, Class: dns.ClassUnavailable}
	}
	ref := dns.RecordRef{ZoneID: z.ID, RecordID: rec.ID}
	p.mu.Lock()
	p.zoneNames[z.ID] = z.Name
	p.hosts[ref] = fqdn
	p.mu.Unlock()
	return ref, nil
}

// GetTXT reads the record back and removes the one pair of double quotes around its content.
func (p *provider) GetTXT(ctx context.Context, ref dns.RecordRef) (dns.Record, error) {
	res, err := p.do(ctx, opRead, http.MethodGet, recordPath(ref), nil, nil)
	if err != nil {
		return dns.Record{}, err
	}
	if !p.ok(res) {
		zone, host := p.known(ref)
		return dns.Record{}, p.failure(opRead, res, host, zone)
	}
	var rec recordJSON
	if err := json.Unmarshal(res.env.Result, &rec); err != nil {
		return dns.Record{}, &dns.Error{Provider: providerName, Op: opRead, Status: res.status, Class: dns.ClassUnavailable}
	}
	return dns.Record{Name: rec.Name, Content: unquote(rec.Content), Comment: rec.Comment}, nil
}

// DeleteTXT deletes the record by its ID.
func (p *provider) DeleteTXT(ctx context.Context, ref dns.RecordRef) error {
	res, err := p.do(ctx, opDelete, http.MethodDelete, recordPath(ref), nil, nil)
	if err != nil {
		return err
	}
	if res.status == http.StatusNotFound {
		return nil // already gone
	}
	if !p.ok(res) {
		zone, host := p.known(ref)
		return p.failure(opDelete, res, host, zone)
	}
	return nil
}

func recordPath(ref dns.RecordRef) string {
	return fmt.Sprintf("/zones/%s/dns_records/%s", url.PathEscape(ref.ZoneID), url.PathEscape(ref.RecordID))
}

// unquote removes one pair of surrounding double quotes.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
