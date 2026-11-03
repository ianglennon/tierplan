// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package registrytest is a registry over TLS for tests: it serves manifests and OCI indexes by tag and
// by digest, can ask for a sign-in the way Docker Hub, GitHub and GitLab do, can answer "too many
// requests", and counts the requests it was asked for, HEAD and GET apart, because a registry may count
// a GET as a pull and a HEAD never.
package registrytest

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/registry"
)

const (
	mediaImage = "application/vnd.oci.image.manifest.v1+json"
	mediaIndex = "application/vnd.oci.image.index.v1+json"
	mediaCfg   = "application/vnd.oci.image.config.v1+json"
)

// Manifest is a manifest the registry can serve: one image, or an index of images.
type Manifest struct {
	media    string
	body     []byte
	digest   string
	children []Manifest
}

// Digest is the manifest's digest (sha256:...).
func (m Manifest) Digest() string { return m.digest }

func newManifest(media string, v any, children []Manifest) Manifest {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err) // the shapes below always marshal
	}
	sum := sha256.Sum256(body)
	return Manifest{media: media, body: body, digest: "sha256:" + hex.EncodeToString(sum[:]), children: children}
}

type descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int       `json:"size"`
	Platform  *platform `json:"platform,omitempty"`
}

type platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// Image returns a single-platform image manifest. Two seeds give two different digests.
func Image(seed string) Manifest {
	sum := sha256.Sum256([]byte("config:" + seed))
	return newManifest(mediaImage, map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaImage,
		"config":        descriptor{MediaType: mediaCfg, Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: 2},
		"layers":        []descriptor{},
	}, nil)
}

// PlatformManifest is one entry of an index: the image a platform runs.
type PlatformManifest struct {
	OS, Arch, Variant string
	Image             Manifest
}

// Index returns an OCI index listing platforms. The images it lists are served by digest once the index
// is set under a tag.
func Index(platforms ...PlatformManifest) Manifest {
	ds := make([]descriptor, 0, len(platforms))
	kids := make([]Manifest, 0, len(platforms))
	for _, p := range platforms {
		ds = append(ds, descriptor{
			MediaType: p.Image.media, Digest: p.Image.digest, Size: len(p.Image.body),
			Platform: &platform{Architecture: p.Arch, OS: p.OS, Variant: p.Variant},
		})
		kids = append(kids, p.Image)
	}
	return newManifest(mediaIndex, map[string]any{"schemaVersion": 2, "mediaType": mediaIndex, "manifests": ds}, kids)
}

// Options shape the registry.
type Options struct {
	// User and Password, when User is set, make the registry ask for a sign-in: a Bearer challenge on
	// /v2/ and on every manifest, and a token endpoint that refuses any other credential with 401.
	User, Password string
	// RateLimitAfter, when above zero, makes the registry answer 429 to every manifest request after
	// that many. SetRateLimitAfter changes it later.
	RateLimitAfter int
	// NoDigestHeader leaves Docker-Content-Digest out of every manifest answer.
	NoDigestHeader bool
}

// Registry is the fake. It is safe for concurrent use.
type Registry struct {
	srv  *httptest.Server
	opts Options

	heads, gets atomic.Int64
	// served counts manifest requests, the clock RateLimitAfter reads; limitAfter is -1 for no limit.
	served     atomic.Int64
	limitAfter atomic.Int64

	mu        sync.Mutex
	seen      map[string]int      // repo@ref -> manifest requests
	tags      map[string]string   // repo:tag -> digest
	manifests map[string]Manifest // digest -> manifest
}

// New starts a registry that stops with the test.
func New(t *testing.T, opts Options) *Registry {
	t.Helper()
	r := &Registry{opts: opts, seen: map[string]int{}, tags: map[string]string{}, manifests: map[string]Manifest{}}
	r.limitAfter.Store(-1)
	if opts.RateLimitAfter > 0 {
		r.limitAfter.Store(int64(opts.RateLimitAfter))
	}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

// SetTag makes repo:tag point at m. A tag set again moves.
func (r *Registry) SetTag(repo, tag string, m Manifest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tags[repo+":"+tag] = m.digest
	r.manifests[m.digest] = m
	for _, c := range m.children {
		r.manifests[c.digest] = c
	}
}

// SetRateLimitAfter makes the registry answer 429 to every manifest request after the next n. Zero
// refuses the very next one; a negative n lifts the limit.
func (r *Registry) SetRateLimitAfter(n int) {
	if n < 0 {
		r.limitAfter.Store(-1)
		return
	}
	r.limitAfter.Store(r.served.Load() + int64(n))
}

// Heads is how many manifest HEAD requests the registry was asked, whatever it answered.
func (r *Registry) Heads() int { return int(r.heads.Load()) }

// Gets is how many manifest GET requests the registry was asked, whatever it answered.
func (r *Registry) Gets() int { return int(r.gets.Load()) }

// Requested is how many manifest requests, HEAD and GET, named repo and ref (a tag or a digest).
func (r *Registry) Requested(repo, ref string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[repo+"@"+ref]
}

// Certificate is the certificate the registry serves, for a policy that must trust several registries.
func (r *Registry) Certificate() *x509.Certificate { return r.srv.Certificate() }

// Host is the registry's address as an image reference writes it: 127.0.0.1:port.
func (r *Registry) Host() string { return strings.TrimPrefix(r.srv.URL, "https://") }

// EgressPolicy is a policy that lets a client reach the registry and trust its certificate.
func (r *Registry) EgressPolicy() registry.EgressPolicy {
	pool := x509.NewCertPool()
	pool.AddCert(r.srv.Certificate())
	return registry.NewEgressPolicy(nil).WithRootCAs(pool)
}

func (r *Registry) challenge(w http.ResponseWriter, scope string) {
	c := `Bearer realm="https://` + r.Host() + `/token",service="fake"`
	if scope != "" {
		c += `,scope="` + scope + `"`
	}
	w.Header().Set("WWW-Authenticate", c)
	w.WriteHeader(http.StatusUnauthorized)
}

func (r *Registry) signedIn(req *http.Request) bool {
	return r.opts.User == "" || req.Header.Get("Authorization") == "Bearer good"
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == "/v2/":
		if !r.signedIn(req) {
			r.challenge(w, "")
			return
		}
		w.WriteHeader(http.StatusOK)
	case req.URL.Path == "/token":
		u, p, ok := req.BasicAuth()
		if !ok || u != r.opts.User || p != r.opts.Password {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"code":"DENIED","message":"denied"}]}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "good", "expires_in": 300})
	case strings.Contains(req.URL.Path, "/manifests/"):
		r.serveManifest(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (r *Registry) serveManifest(w http.ResponseWriter, req *http.Request) {
	rest := strings.TrimPrefix(req.URL.Path, "/v2/")
	i := strings.Index(rest, "/manifests/")
	repo, ref := rest[:i], rest[i+len("/manifests/"):]
	if !r.signedIn(req) {
		r.challenge(w, "repository:"+repo+":pull")
		return
	}
	head := req.Method == http.MethodHead
	if head {
		r.heads.Add(1)
	} else {
		r.gets.Add(1)
	}
	n := r.served.Add(1)
	r.mu.Lock()
	r.seen[repo+"@"+ref]++
	r.mu.Unlock()
	if after := r.limitAfter.Load(); after >= 0 && n > after {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":[{"code":"TOOMANYREQUESTS","message":"slow down"}]}`))
		return
	}
	r.mu.Lock()
	digest := ref
	if !strings.HasPrefix(ref, "sha256:") {
		digest = r.tags[repo+":"+ref]
	}
	m, ok := r.manifests[digest]
	r.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"unknown"}]}`))
		return
	}
	w.Header().Set("Content-Type", m.media)
	if !r.opts.NoDigestHeader {
		w.Header().Set("Docker-Content-Digest", m.digest)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(m.body)))
	w.WriteHeader(http.StatusOK)
	if !head {
		_, _ = w.Write(m.body)
	}
}
