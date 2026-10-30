// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"crypto/x509"
	"regexp"
	"slices"
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
	// maxCAPEMLen caps a custom CA bundle: a handful of certificates, never a way to store files.
	maxCAPEMLen = 64 << 10
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
	// CAPEM is an optional certificate authority, in PEM, that Bayplan's own calls to this
	// registry trust. It is public material and never joins the server's own trust store.
	CAPEM string
	// Insecure lets Bayplan's own calls to this registry skip TLS verification and fall back
	// to plain HTTP when the server answers plain HTTP (D-14). It affects nothing on a node.
	Insecure bool
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

	in.CAPEM = strings.TrimSpace(in.CAPEM)
	if in.CAPEM != "" && !validCAPEM(in.CAPEM) {
		bad("caPem", "ca_invalid")
	}
	return in, errs
}

// MaxCAPEMBytes is the longest custom certificate authority bundle accepted: 64 KiB. Git
// repository connections use the same limit as registry credentials (Phase 9 D-05).
const MaxCAPEMBytes = maxCAPEMLen

// ValidCAPEM reports whether s is at most MaxCAPEMBytes and adds at least one certificate to a
// pool (T-05-73). Text around the certificates is ignored, as a pool does.
func ValidCAPEM(s string) bool {
	return len(s) <= MaxCAPEMBytes && x509.NewCertPool().AppendCertsFromPEM([]byte(s))
}

// validCAPEM is ValidCAPEM for this package's own validators.
func validCAPEM(s string) bool { return ValidCAPEM(s) }

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

// UpdateInput is what a person types to change a credential: every field of CreateInput
// except the layer, which a credential never leaves. Secret empty means keep the stored one.
type UpdateInput struct {
	Name   string
	Type   string
	Host   string
	Prefix string
	// Username is the registry sign-in.
	Username string
	// Secret is a replacement. Empty keeps the stored secret, which is neither read nor
	// sent anywhere, and is refused by the store when the host, type, username, CA or insecure
	// switch changes. It is never logged.
	Secret      string
	HubFallback bool
	// Expires is an optional day, YYYY-MM-DD; empty clears it.
	Expires string
	// CAPEM is the custom certificate authority; empty clears it.
	CAPEM string
	// Insecure is the plain HTTP or skip-verification switch.
	Insecure bool
}

// SecretDestinationChanged reports whether in points the stored secret at somewhere it was not
// stored for: a different host, type, username, certificate authority or insecure switch.
// The secret is write-only, so it may only ever go where its owner last typed it; once the
// destination moves, a freshly typed secret is required (CR-A01, CR-B01). in is expected clean
// (as Validate and ValidateUpdate return it) and cur is the stored credential.
func SecretDestinationChanged(cur Credential, host, typ, username, caPEM string, insecure bool) bool {
	return host != cur.Host || typ != cur.Type || username != cur.Username ||
		strings.TrimSpace(caPEM) != cur.CAPEM || insecure != cur.Insecure
}

// ValidateUpdate checks a change as Validate checks an addition, with two differences: an
// empty secret is fine (it keeps the stored one), and an expiry day equal to prevExpires (the
// stored one, as YYYY-MM-DD, or empty) may have passed, so a credential that has expired can
// still have its name or secret corrected. It returns the input cleaned.
func ValidateUpdate(in UpdateInput, prevExpires string, now time.Time) (UpdateInput, []FieldError) {
	keepSecret := in.Secret == ""
	forCreate := CreateInput{
		Name: in.Name, Type: in.Type, Host: in.Host, Prefix: in.Prefix, Username: in.Username,
		Secret: in.Secret, HubFallback: in.HubFallback, Expires: in.Expires, CAPEM: in.CAPEM, Insecure: in.Insecure,
	}
	if keepSecret {
		forCreate.Secret = "placeholder" // so Validate does not ask for a secret that is not changing
	}
	clean, errs := Validate(forCreate, now)
	if keepSecret {
		clean.Secret = ""
	}
	if strings.TrimSpace(in.Expires) != "" && strings.TrimSpace(in.Expires) == prevExpires {
		errs = slices.DeleteFunc(errs, func(e FieldError) bool { return e.Field == "expires" && e.Code == "expiry_past" })
	}
	return UpdateInput{
		Name: clean.Name, Type: clean.Type, Host: clean.Host, Prefix: clean.Prefix, Username: clean.Username,
		Secret: clean.Secret, HubFallback: clean.HubFallback, Expires: clean.Expires, CAPEM: clean.CAPEM, Insecure: clean.Insecure,
	}, errs
}
