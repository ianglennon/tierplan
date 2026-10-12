// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Credential types: the presets of D-04. dockerhub, ghcr and gitlab fill in the host;
// generic is any registry that speaks the v2 API with a static username and token.
const (
	TypeDockerHub = "dockerhub"
	TypeGHCR      = "ghcr"
	TypeGitLab    = "gitlab"
	TypeGeneric   = "generic"
)

// Credential layers (D-01): an instance credential applies to every environment, an
// environment credential to its own environment only.
const (
	LayerInstance    = "instance"
	LayerEnvironment = "environment"
)

const (
	hubHost    = "docker.io"
	ghcrHost   = "ghcr.io"
	gitlabHost = "registry.gitlab.com"

	maxNameLen     = 64
	maxUsernameLen = 256
	maxPrefixLen   = 255
	maxHostLen     = 253
	// shortSecretLen is the length below which a secret keeps no last characters.
	shortSecretLen = 12
	lastChars      = 4
	// maxExpiryYear keeps an expiry inside what a unix-nanosecond timestamp holds (to 2262).
	maxExpiryYear = 2200
)

var (
	// A name is shown as text everywhere, but markup never reaches a stored row all the
	// same (T-05-24): letters, digits, and a few separators, starting with a letter or digit.
	namePattern = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._()-]*$`)
	// hostPattern is a DNS name or IPv4 address with an optional port; the port range is
	// checked separately.
	hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)
	// prefixPattern is one or more lower-case path segments, as a repository path is.
	prefixPattern = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
)

// hubAliases are the other names Docker Hub is reached by; a reference normalises to docker.io.
var hubAliases = map[string]bool{"index.docker.io": true, "registry-1.docker.io": true, "registry.hub.docker.com": true}

// CreateInput is what a person types to add a credential.
type CreateInput struct {
	// EnvID is empty for an instance credential.
	EnvID string
	Name  string
	Type  string
	// Host is ignored for dockerhub and ghcr, defaults to registry.gitlab.com for gitlab
	// and is required for generic.
	Host   string
	Prefix string
	// Username and Secret are the registry sign-in. The secret is never logged.
	Username, Secret string
	// HubFallback uses the credential for every Docker Hub pull no more specific credential
	// matches (D-03).
	HubFallback bool
	// Expires is an optional day, YYYY-MM-DD (D-07).
	Expires string
}

// FieldError is one problem with a field of a submission. Code is stable: the SPA words it.
type FieldError struct {
	Field, Code string
}

// Last4 is the part of a secret that may be shown after it is saved (D-05). The task 1 answer
// of plan 05-05 is "short-mask": a secret shorter than 12 characters keeps no last
// characters, so a short htpasswd password is never shown in large part or whole and the
// UI shows only the mask; a secret of 12 or more characters keeps its last 4. The
// answer to the sealing question is "context": the secret itself is sealed under
// vault.ContextRegistryCred and the credential's ID. It counts characters, not bytes.
func Last4(secret string) string {
	if utf8.RuneCountInString(secret) < shortSecretLen {
		return ""
	}
	r := []rune(secret)
	return string(r[len(r)-lastChars:])
}

// ExpiryEnd parses an expiry day and returns the last second of that day in UTC.
func ExpiryEnd(day string) (time.Time, bool) {
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return time.Time{}, false
	}
	return d.UTC().Add(24*time.Hour - time.Second), true
}

// Validate checks a submission and returns it cleaned: names and hosts trimmed, the host of a
// preset filled in and every host lower-cased. It reports every field at fault at once, with a
// stable code each. now decides whether an expiry day has passed.
func Validate(in CreateInput, now time.Time) (CreateInput, []FieldError) {
	var errs []FieldError
	bad := func(field, code string) { errs = append(errs, FieldError{field, code}) }

	in.Name = strings.TrimSpace(in.Name)
	switch {
	case in.Name == "":
		bad("name", "name_required")
	case utf8.RuneCountInString(in.Name) > maxNameLen || !namePattern.MatchString(in.Name):
		bad("name", "name_invalid")
	}

	switch in.Type {
	case TypeDockerHub:
		in.Host = hubHost
	case TypeGHCR:
		in.Host = ghcrHost
	case TypeGitLab, TypeGeneric:
		host := strings.ToLower(strings.TrimSpace(in.Host))
		switch {
		case host == "" && in.Type == TypeGitLab:
			host = gitlabHost
		case host == "":
			bad("host", "host_required")
		case !validHost(host):
			bad("host", "host_invalid")
		}
		if hubAliases[host] {
			host = hubHost
		}
		in.Host = host
	default:
		bad("type", "type_invalid")
		in.Host = strings.ToLower(strings.TrimSpace(in.Host))
	}

	in.Prefix = strings.TrimSpace(in.Prefix)
	if in.Prefix != "" && (len(in.Prefix) > maxPrefixLen || !prefixPattern.MatchString(in.Prefix)) {
		bad("prefix", "prefix_invalid")
	}

	in.Username = strings.TrimSpace(in.Username)
	switch {
	case in.Username == "":
		bad("username", "username_required")
	case utf8.RuneCountInString(in.Username) > maxUsernameLen || strings.IndexFunc(in.Username, unicode.IsControl) >= 0:
		bad("username", "username_invalid")
	}

	// A token pasted with the newline it was copied with would never sign in.
	in.Secret = strings.TrimSpace(in.Secret)
	if in.Secret == "" {
		bad("secret", "secret_required")
	}

	isHub := in.Host == hubHost
	if in.HubFallback && !isHub {
		bad("hubFallback", "hub_fallback_not_hub")
	}
	if isHub && in.Prefix == "" && !in.HubFallback {
		bad("prefix", "hub_prefix_required")
	}

	in.Expires = strings.TrimSpace(in.Expires)
	if in.Expires != "" {
		if end, ok := ExpiryEnd(in.Expires); !ok || end.Year() > maxExpiryYear {
			bad("expires", "expiry_invalid")
		} else if end.Before(now) {
			bad("expires", "expiry_past")
		}
	}
	return in, errs
}

// validHost reports whether s is a host[:port] with a usable port.
func validHost(s string) bool {
	if len(s) > maxHostLen || !hostPattern.MatchString(s) {
		return false
	}
	if _, port, ok := strings.Cut(s, ":"); ok {
		n, err := strconv.Atoi(port)
		return err == nil && n >= 1 && n <= 65535
	}
	return true
}
