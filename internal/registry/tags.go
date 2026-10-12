// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// nameListMax and nameBytesMax cap one repository's tag name list: a hostile or huge
	// repository cannot make the server hold more (T-05-63).
	nameListMax  = 10_000
	nameBytesMax = 5 << 20
	// nameTTL is how long a tag name list is served from memory.
	nameTTL = 60 * time.Second
	// nameCacheEntries is how many name lists the cache keeps.
	nameCacheEntries = 64
	// TagPageSize is how many tags one page of the list holds.
	TagPageSize = 100
)

// The codes a tag listing fails with, besides the step codes of a credential test. The SPA
// words each of them; no registry text is ever carried.
const (
	// CodeNeedsLogin is a registry that asked for a login when no stored credential matched.
	CodeNeedsLogin = "needs_login"
	// CodeLoginRefused is a registry that refused the sign-in of the credential that matched.
	CodeLoginRefused = "login_refused"
)

// TagError is a tag listing that failed. Code is one of CodeNeedsLogin, CodeLoginRefused,
// CodeRepositoryNotFound, CodeRegistryUnreachable, CodeRegistryTLS and CodeRateLimited. Its
// text is the code alone, because what the registry said may hold anything; Unwrap reaches
// the cause for errors.Is.
type TagError struct {
	Code string
	Err  error
}

func (e *TagError) Error() string { return "registry: list tags: " + e.Code }
func (e *TagError) Unwrap() error { return e.Err }

// TagName is one tag of a repository. Digest, creation time and platforms come with 05-19.
type TagName struct {
	Name string
}

// TagPage is one page of a repository's tag list.
type TagPage struct {
	// Repo is the repository below the host: acme/app. Host is the registry host.
	Repo, Host string
	Tags       []TagName
	// Hidden is how many signature, attestation and cache tags the filter text matches;
	// Total is how many tags the list has once the hide switch and the filter are applied.
	Hidden, Total int
	// More is true when a later page exists.
	More bool
	// Capped is true when the repository has more tags than the server reads.
	Capped bool
}

// signatureSuffixes end the name of a signature, attestation or software bill of materials tag.
var signatureSuffixes = []string{".sig", ".att", ".sbom"}

// digestTagSuffixes may follow a sha256-<64 hex> tag, which is how cosign and BuildKit attach
// a signature, attestation or provenance to an image by its digest.
var digestTagSuffixes = []string{"", ".sig", ".att", ".sbom", ".sbom.json", ".prov"}

// HiddenTag reports whether a tag is a signature, attestation or build cache tag, which the
// tag list hides by default (D-21): sha256-<64 hex> with an optional .sig, .att, .sbom,
// .sbom.json or .prov, any tag ending .sig, .att or .sbom, and the tags buildcache and cache
// or ending -buildcache or .cache.
func HiddenTag(name string) bool {
	if rest, ok := strings.CutPrefix(name, "sha256-"); ok && len(rest) >= 64 && isHex(rest[:64]) {
		if slices.Contains(digestTagSuffixes, rest[64:]) {
			return true
		}
	}
	for _, suffix := range signatureSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return name == "buildcache" || name == "cache" || strings.HasSuffix(name, "-buildcache") || strings.HasSuffix(name, ".cache")
}

func isHex(s string) bool {
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// nameKey identifies a cached name list. cred is the ID of the credential the list was
// fetched with and the time it was last changed, or "anon": a list fetched with one sign-in is
// never served to a call that cannot use that sign-in (T-05-60).
type nameKey struct {
	cred, host, repo string
}

// nameList is the cached result of one fetch, in list order. It is shared between calls and
// never changed after it is made.
type nameList struct {
	names []string
	// lower and hidden are what each name is searched with and whether HiddenTag hides it,
	// worked out once for the cache's minute.
	lower  []string
	hidden []bool
	capped bool
}

// TagService lists the tags of a repository for the tag browser. It talks to registries only
// through clients that dial through the egress policy, and keeps no credential.
type TagService struct {
	clients *ClientCache
	log     *slog.Logger
	names   *lru[nameKey, nameList]
}

// NewTagService returns a TagService whose registry calls dial through egress and carry the
// user agent bayplan/<version>.
func NewTagService(clients *ClientCache, egress EgressPolicy, version string, log *slog.Logger) *TagService {
	if log == nil {
		log = slog.Default()
	}
	if clients == nil {
		clients = NewClientCache(egress, version, log)
	}
	return &TagService{clients: clients, log: log, names: newLRU[nameKey, nameList](nameCacheEntries, nil)}
}

// List lists one page of the tags of ref's repository with cred (nil is anonymous). Tags are
// in CompareTags order. q keeps the names that contain it, ignoring case; hide drops the tags
// HiddenTag reports (D-21). page counts from 0, TagPageSize tags to a page; a page before the
// first is the first, and one past the last is empty. The caller must have been authorised,
// and cred must be one the caller may use, before this is called: the cache is keyed by
// credential, not by caller.
func (s *TagService) List(ctx context.Context, cred *ResolvedCredential, ref Ref, q string, hide bool, page int) (TagPage, error) {
	list, err := s.nameList(ctx, cred, ref)
	if err != nil {
		return TagPage{}, err
	}
	q = strings.ToLower(strings.TrimSpace(q))
	out := TagPage{Repo: ref.Path, Host: ref.Domain, Capped: list.capped}
	skip := max(page, 0) * TagPageSize
	for i, name := range list.names {
		if q != "" && !strings.Contains(list.lower[i], q) {
			continue
		}
		if list.hidden[i] {
			out.Hidden++
			if hide {
				continue
			}
		}
		if out.Total >= skip && len(out.Tags) < TagPageSize {
			out.Tags = append(out.Tags, TagName{Name: name})
		}
		out.Total++
	}
	out.More = out.Total > skip+len(out.Tags)
	return out, nil
}

// nameList returns the repository's tag names, from the cache when it holds them.
func (s *TagService) nameList(ctx context.Context, cred *ResolvedCredential, ref Ref) (nameList, error) {
	key := nameKey{cred: "anon", host: ref.Domain, repo: ref.Path}
	if cred != nil {
		key.cred = cred.ID + "@" + strconv.FormatInt(cred.UpdatedAt.UnixNano(), 10)
	}
	if l, ok := s.names.Get(key); ok {
		return l, nil
	}
	var c *Client
	if cred == nil {
		c = s.clients.Anonymous(ref.Domain)
	} else {
		c = s.clients.ForCredential(cred.ID, cred.UpdatedAt, cred.HostOptions)
	}
	// One more than the cap, to know whether the repository has more.
	names, err := c.TagList(ctx, ref.Path, nameListMax+1)
	if err != nil {
		return nameList{}, &TagError{Code: tagCode(err, cred != nil), Err: err}
	}
	l := sortNames(capNames(names))
	s.names.Put(key, l, nameTTL)
	return l, nil
}

// capNames cuts a name list at nameListMax names and nameBytesMax bytes.
func capNames(names []string) nameList {
	l := nameList{names: names}
	if len(names) > nameListMax {
		l.names, l.capped = names[:nameListMax], true
	}
	size := 0
	for i, n := range l.names {
		size += len(n)
		if size > nameBytesMax {
			l.names, l.capped = l.names[:i], true
			break
		}
	}
	return l
}

// sortNames puts a name list in list order and works out what List searches it with.
func sortNames(l nameList) nameList {
	l.names = slices.Clone(l.names)
	slices.SortFunc(l.names, CompareTags)
	l.lower = make([]string, len(l.names))
	l.hidden = make([]bool, len(l.names))
	for i, n := range l.names {
		l.lower[i], l.hidden[i] = strings.ToLower(n), HiddenTag(n)
	}
	return l
}

// tagCode maps an error from a registry call to the code the SPA words. signedIn says a
// stored credential was used, which makes a refused login the credential's.
func tagCode(err error, signedIn bool) string {
	switch code := classify(err); code {
	case CodeSignInRefused:
		if signedIn {
			return CodeLoginRefused
		}
		return CodeNeedsLogin
	case CodeRepositoryNotFound, CodeRegistryTLS, CodeRateLimited:
		return code
	default:
		// Not a registry, a redirect, a timeout, a refused address and anything unforeseen
		// all read the same to a person: the registry could not be reached.
		return CodeRegistryUnreachable
	}
}

// TagErrorCode is the code of a TagError in err's chain, or "" when there is none.
func TagErrorCode(err error) string {
	var te *TagError
	if errors.As(err, &te) {
		return te.Code
	}
	return ""
}
