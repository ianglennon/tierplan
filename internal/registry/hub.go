// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

const (
	// hubBase is the Docker Hub web API, which lists tags with their digests, dates and
	// platforms and costs no pulls (IMG-03, T-05-61).
	hubBase = "https://hub.docker.com"
	// hubPageSize is the most tags the API answers a page with.
	hubPageSize = 100
	// hubListMax is how many of a repository's newest tags the server reads from the API. The
	// API orders newest first and is paced at two requests a second, so a larger repository
	// is listed to its newest tags and says so.
	hubListMax = 2000
	// hubBurst and hubRate (below) pace every call to the API, whatever the caller.
	hubBurst = 10
	// hubCallTimeout bounds one call to the API.
	hubCallTimeout = 20 * time.Second
	// hubRetryAfterMax is the longest a 429's Retry-After is waited for inside one request.
	hubRetryAfterMax = 5 * time.Second
	// hubBodyMax caps one answer of the API.
	hubBodyMax = 8 << 20
	// hubTokenTTL is how long a token made from a credential is reused.
	hubTokenTTL = 5 * time.Minute
	// hubTokenEntries bounds the tokens held.
	hubTokenEntries = 64
	// hubTagTTL is how long a tag read from the API is served from memory.
	hubTagTTL = 5 * time.Minute
)

// hubRate is how many requests a second the API is called with.
const hubRate rate.Limit = 2

// The ways a call to the Docker Hub API fails. A refusal or a missing repository sends a
// listing to the registry path, which words them (a private repository, a wrong credential);
// a rate limit is answered as it is.
var (
	errHubDenied      = errors.New("registry: docker hub refused the request")
	errHubNotFound    = errors.New("registry: docker hub has no such repository or tag")
	errHubRateLimited = errors.New("registry: docker hub is limiting requests")
)

// hubTag is a tag as the Hub API describes it.
type hubTag struct {
	Name      string
	Digest    string
	Created   *time.Time
	Platforms []Platform
}

type hubKey struct {
	cred, repo, tag string
}

type hubToken struct {
	value string
	exp   time.Time
}

// hubClient talks to the Docker Hub API through the egress policy. It is paced as a whole.
type hubClient struct {
	base string
	http *http.Client
	lim  *rate.Limiter
	// sleep waits for d or until ctx ends; a test replaces it.
	sleep func(ctx context.Context, d time.Duration)

	mu     sync.Mutex
	tokens map[string]hubToken
}

func newHubClient(egress EgressPolicy, version string) *hubClient {
	return &hubClient{
		base: hubBase,
		http: &http.Client{
			Transport: &userAgentTransport{base: egress.Transport(), ua: "bayplan/" + version},
			// An answer that redirects is reported, not followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		lim: rate.NewLimiter(hubRate, hubBurst),
		sleep: func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		},
		tokens: map[string]hubToken{},
	}
}

// do sends one request to the API, paced, and decodes a 200 answer into out. A 429 waits
// for its Retry-After (at most hubRetryAfterMax) and retries once.
func (h *hubClient) do(ctx context.Context, method, path string, query url.Values, token string, body []byte, out any) error {
	for attempt := 0; ; attempt++ {
		if err := h.lim.Wait(ctx); err != nil {
			return err
		}
		wait, err := h.once(ctx, method, path, query, token, body, out)
		if !errors.Is(err, errHubRateLimited) || attempt > 0 {
			return err
		}
		h.sleep(ctx, wait)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// once is one request. For a 429 it returns the time to wait.
func (h *hubClient) once(ctx context.Context, method, path string, query url.Values, token string, body []byte, out any) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	u := h.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("registry: build the docker hub request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("registry: docker hub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		raw, err := io.ReadAll(io.LimitReader(resp.Body, hubBodyMax+1))
		if err != nil {
			return 0, fmt.Errorf("registry: docker hub: read the answer: %w", err)
		}
		if len(raw) > hubBodyMax {
			return 0, errors.New("registry: docker hub: the answer is too large")
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return 0, fmt.Errorf("registry: docker hub: decode the answer: %w", err)
		}
		return 0, nil
	case http.StatusTooManyRequests:
		wait := time.Second
		if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs >= 0 {
			wait = time.Duration(secs) * time.Second
		}
		return min(wait, hubRetryAfterMax), errHubRateLimited
	case http.StatusNotFound:
		return 0, errHubNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return 0, errHubDenied
	}
	return 0, fmt.Errorf("registry: docker hub answered %d", resp.StatusCode)
}

// token makes a bearer for the Hub API from a credential's username and token, and reuses it
// for hubTokenTTL. A nil credential has none: the API is read anonymously. A refusal is
// errHubDenied.
func (h *hubClient) token(ctx context.Context, cred *ResolvedCredential) (string, error) {
	if cred == nil {
		return "", nil
	}
	id := credIdentity(cred)
	now := time.Now()
	h.mu.Lock()
	if t, ok := h.tokens[id]; ok && now.Before(t.exp) {
		h.mu.Unlock()
		return t.value, nil
	}
	h.mu.Unlock()
	body, err := json.Marshal(map[string]string{"identifier": cred.Username, "secret": cred.Password})
	if err != nil {
		return "", fmt.Errorf("registry: build the docker hub login: %w", err)
	}
	var out struct {
		AccessToken string `json:"access_token"` //nolint:gosec // G117: a response field read once and held in memory
	}
	if err := h.do(ctx, http.MethodPost, "/v2/auth/token", nil, "", body, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errHubDenied
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.tokens) >= hubTokenEntries {
		h.tokens = map[string]hubToken{}
	}
	h.tokens[id] = hubToken{value: out.AccessToken, exp: now.Add(hubTokenTTL)}
	return out.AccessToken, nil
}

// hubTagJSON is a tag in the API's answer.
type hubTagJSON struct {
	Name        string `json:"name"`
	Digest      string `json:"digest"`
	LastUpdated string `json:"last_updated"`
	Images      []struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant"`
		Digest       string `json:"digest"`
		Size         int64  `json:"size"`
	} `json:"images"`
}

func (j hubTagJSON) tag() hubTag {
	t := hubTag{Name: j.Name, Digest: j.Digest}
	if c, err := time.Parse(time.RFC3339Nano, j.LastUpdated); err == nil && !c.IsZero() {
		t.Created = &c
	}
	for _, im := range j.Images {
		if !isRealPlatform(im.OS, im.Architecture) {
			continue
		}
		t.Platforms = append(t.Platforms, Platform{OS: im.OS, Arch: im.Architecture, Variant: im.Variant, Digest: im.Digest, Size: im.Size})
	}
	return t
}

// repoPath is the API path of a repository's tags. A Docker Hub repository is a namespace and
// a name; anything else is not one.
func hubRepoPath(repo string) (string, error) {
	ns, name, ok := strings.Cut(repo, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return "", errHubNotFound
	}
	return "/v2/namespaces/" + url.PathEscape(ns) + "/repositories/" + url.PathEscape(name) + "/tags", nil
}

// list reads a repository's newest tags, a page of hubPageSize at a time, up to max. capped is
// true when the repository has more.
func (h *hubClient) list(ctx context.Context, token, repo string, limit int) (tags []hubTag, capped bool, err error) {
	path, err := hubRepoPath(repo)
	if err != nil {
		return nil, false, err
	}
	for page := 1; ; page++ {
		var out struct {
			Next    *string      `json:"next"`
			Results []hubTagJSON `json:"results"`
		}
		q := url.Values{"page_size": {strconv.Itoa(hubPageSize)}, "page": {strconv.Itoa(page)}}
		if err := h.do(ctx, http.MethodGet, path, q, token, nil, &out); err != nil {
			return nil, false, err
		}
		for _, r := range out.Results {
			tags = append(tags, r.tag())
		}
		more := out.Next != nil && *out.Next != "" && len(out.Results) > 0
		if len(tags) >= limit {
			capped = len(tags) > limit || more
			return tags[:limit], capped, nil
		}
		if !more {
			return tags, false, nil
		}
	}
}

// one reads a single tag.
func (h *hubClient) one(ctx context.Context, token, repo, tag string) (hubTag, error) {
	path, err := hubRepoPath(repo)
	if err != nil {
		return hubTag{}, err
	}
	var out hubTagJSON
	if err := h.do(ctx, http.MethodGet, path+"/"+url.PathEscape(tag), nil, token, nil, &out); err != nil {
		return hubTag{}, err
	}
	if out.Name == "" {
		out.Name = tag
	}
	return out.tag(), nil
}

// hubCode words a failed call to the API the way a listing is worded.
func hubCode(err error, signedIn bool) string {
	switch {
	case errors.Is(err, errHubRateLimited):
		return CodeRateLimited
	case errors.Is(err, errHubNotFound):
		return CodeTagNotFound
	case errors.Is(err, errHubDenied):
		if signedIn {
			return CodeLoginRefused
		}
		return CodeNeedsLogin
	}
	return tagCode(err, signedIn)
}

// hubCodeAfterSignIn is hubCode for a call made with a token the credential was just issued. A
// denial then is the repository's, not the credential's, because the token endpoint accepted the
// credential: Docker Hub hides whether a repository exists behind the same denial (G-05-17a). An
// anonymous call has no sign-in to vouch for it and keeps needs_login.
func hubCodeAfterSignIn(err error, signedIn bool) string {
	if signedIn && errors.Is(err, errHubDenied) {
		return CodeRepositoryUnavailable
	}
	return hubCode(err, signedIn)
}

// hubNames reads the newest tags of a Docker Hub repository for the name cache. A failure is
// returned as it is; the caller decides whether the registry path should answer instead.
func (s *TagService) hubNames(ctx context.Context, cred *ResolvedCredential, r Ref) (nameList, error) {
	token, err := s.hub.token(ctx, cred)
	if err != nil {
		return nameList{}, err
	}
	tags, capped, err := s.hub.list(ctx, token, r.Path, hubListMax)
	if err != nil {
		return nameList{}, err
	}
	id := credIdentity(cred)
	l := nameList{capped: capped, hub: make(map[string]hubTag, len(tags))}
	for _, t := range tags {
		if t.Name == "" {
			continue
		}
		l.names = append(l.names, t.Name)
		l.hub[t.Name] = t
		s.hubTags.Put(hubKey{cred: id, repo: r.Path, tag: t.Name}, t, hubTagTTL)
	}
	return sortNames(l), nil
}

// hubDetails answers the details of tags of a Docker Hub repository from what the listing
// read and, for a tag it did not, from the API's tag endpoint. It sends no request to the
// registry, so it costs no pulls (T-05-61). A refused token is login_refused; an API denial
// after the token was issued is repository_unavailable.
func (s *TagService) hubDetails(ctx context.Context, cred *ResolvedCredential, r Ref, tags []string) []TagDetail {
	out := make([]TagDetail, len(tags))
	id := credIdentity(cred)
	var (
		once  sync.Once
		token string
		terr  error
	)
	login := func() (string, error) {
		once.Do(func() { token, terr = s.hub.token(ctx, cred) })
		return token, terr
	}
	var g errgroup.Group
	g.SetLimit(reqConcurrent)
	for i, name := range tags {
		g.Go(func() error {
			d := TagDetail{Tag: name}
			t, ok := s.hubTags.Get(hubKey{cred: id, repo: r.Path, tag: name})
			if !ok {
				tok, err := login()
				if err != nil {
					// The token itself failed: a refusal here is the credential's.
					d.Code = hubCode(err, cred != nil)
					out[i] = d
					return nil
				}
				if t, err = s.hub.one(ctx, tok, r.Path, name); err != nil {
					d.Code = hubCodeAfterSignIn(err, cred != nil)
					out[i] = d
					return nil
				}
				s.hubTags.Put(hubKey{cred: id, repo: r.Path, tag: name}, t, hubTagTTL)
			}
			d.Digest, d.Created, d.Platforms = t.Digest, t.Created, t.Platforms
			if d.Created == nil {
				d.Code = CodeDateUnavailable
			}
			out[i] = d
			return nil
		})
	}
	_ = g.Wait()
	return out
}
