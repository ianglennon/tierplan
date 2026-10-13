// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	// The digest algorithm sha256 must be registered for a reference that carries a
	// digest to parse: go-digest registers it only when crypto/sha256 is linked in
	// (RESEARCH Pitfall 12).
	_ "crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/distribution/reference"
)

// maxRefLen caps a reference: a name is at most 255 characters, a tag 128 and a digest
// 71, so nothing longer is a reference.
const maxRefLen = 512

// tagGrammar is what an image tag may be: the reference grammar's, 1 to 128 characters.
var tagGrammar = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// ValidTag reports whether tag is a tag as the reference grammar writes one. A name that is
// not (a path, a query, an escape) must never be put in a registry URL (WR-A02).
func ValidTag(tag string) bool { return tagGrammar.MatchString(tag) }

// Ref is an image reference as the engine will pull it: the domain, repository path,
// tag and digest written out, with docker.io and library/ filled in and latest added
// when neither a tag nor a digest was given.
type Ref struct {
	// Named is the parsed reference.
	Named reference.Named
	// Domain is the registry host, with the port when there is one; Path the repository
	// below it.
	Domain, Path string
	// Tag is the tag, empty for a reference by digest alone. Digest is empty for a
	// reference by tag.
	Tag, Digest string
}

// String is the full normalised reference: docker.io/library/nginx:1.27.
func (r Ref) String() string { return r.Named.String() }

// Repo is the repository without a tag or digest: docker.io/library/nginx.
func (r Ref) Repo() string { return r.Domain + "/" + r.Path }

// ParseRef parses and normalises an image reference a person typed. It refuses an empty
// or over-long reference and one that is not a valid name; it never accepts a
// reference with an upper-case repository, which Docker refuses too.
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, errors.New("an image reference is required")
	}
	if len(s) > maxRefLen {
		return Ref{}, fmt.Errorf("an image reference is at most %d characters", maxRefLen)
	}
	named, err := reference.ParseNormalizedNamed(s)
	if err != nil {
		return Ref{}, fmt.Errorf("not an image reference: %w", err)
	}
	named = reference.TagNameOnly(named)
	out := Ref{Named: named, Domain: reference.Domain(named), Path: reference.Path(named)}
	if t, ok := named.(reference.Tagged); ok {
		out.Tag = t.Tag()
	}
	if d, ok := named.(reference.Digested); ok {
		out.Digest = d.Digest().String()
	}
	return out, nil
}
