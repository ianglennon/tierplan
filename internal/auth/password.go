// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed" // for the common-password list
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// MinPasswordLength is the shortest password accepted (D-07). It is one
// constant on purpose: NIST SP 800-63B-4 asks for 15 characters when a password
// is the only factor, so raising it, or adding TOTP and keeping 12, is a
// one-line change.
const MinPasswordLength = 12

// Username limits (D-35).
const (
	minUsernameLength = 3
	maxUsernameLength = 32
)

// Errors returned by the policy checks. They are stable so the API can map
// each to a field error code.
var (
	ErrInvalidUsername  = errors.New("username must be 3 to 32 characters from a-z, 0-9, dot, dash and underscore")
	ErrPasswordTooShort = errors.New("password is shorter than the minimum length")
	ErrPasswordCommon   = errors.New("password is too common or contains the username")
)

// NormalizeUsername lower-cases a username and checks it is 3 to 32 characters
// from [a-z0-9._-] (D-35). The charset is checked on the input as written, so a
// character that only becomes ASCII under Unicode case folding, such as the
// Kelvin sign, is refused instead of silently turning into "k".
func NormalizeUsername(s string) (string, error) {
	if len(s) < minUsernameLength || len(s) > maxUsernameLength {
		return "", ErrInvalidUsername
	}
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return "", ErrInvalidUsername
		}
		out[i] = c
	}
	return string(out), nil
}

// commonPasswords is the lower-cased list of known-breached passwords of 12 or
// more characters, one per line, sorted and de-duplicated. Because the minimum
// length is 12, shorter entries could never match and are left out.
//
// Source: SecLists, Passwords/Common-Credentials/xato-net-10-million-passwords-1000000.txt
// (https://github.com/danielmiessler/SecLists, MIT licence), filtered to
// entries of 12 or more printable ASCII characters, lower-cased and sorted. The
// licence of the underlying breach data has not been audited; attribution is
// kept in the third-party notices. The file can be replaced without code
// changes.
//
//go:embed commonpw.txt
var commonPasswordsRaw string

var commonPasswords = sync.OnceValue(func() map[string]struct{} {
	lines := strings.Split(commonPasswordsRaw, "\n")
	set := make(map[string]struct{}, len(lines))
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			set[l] = struct{}{}
		}
	}
	return set
})

// ValidatePassword applies the password policy (D-07): at least
// MinPasswordLength characters, not on the embedded common-password list
// (compared case-insensitively) and not containing the username. There are no
// composition rules.
func ValidatePassword(pw, username string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	lower := strings.ToLower(pw)
	if _, ok := commonPasswords()[lower]; ok {
		return ErrPasswordCommon
	}
	if u := strings.ToLower(username); u != "" && strings.Contains(lower, u) {
		return ErrPasswordCommon
	}
	return nil
}

// argon2id parameters (OWASP Password Storage Cheat Sheet minimum: m=19 MiB,
// t=2, p=1). They are written into every hash, so they can be raised later
// without invalidating existing passwords.
const (
	argonMemoryKiB = 19456
	argonTime      = 2
	argonThreads   = 1
	argonSaltLen   = 16
	argonKeyLen    = 32
)

// Upper and lower bounds accepted when parsing a stored hash, so a tampered or
// corrupt row cannot make the server allocate gigabytes or compute for minutes.
const (
	maxArgonMemoryKiB = 256 * 1024
	maxArgonTime      = 10
	maxArgonThreads   = 16
	minArgonSaltLen   = 8
	minArgonKeyLen    = 16
	maxArgonKeyLen    = 64
)

// MaxConcurrentHashes bounds the argon2 computations running at once. Each uses
// about 19 MiB, so an unbounded burst of sign-in or setup requests would
// otherwise be a memory-exhaustion lever (Pitfall 17).
const MaxConcurrentHashes = 4

var hashSlots = make(chan struct{}, MaxConcurrentHashes)

func acquireHashSlot() func() {
	hashSlots <- struct{}{}
	return func() { <-hashSlots }
}

// HashPassword returns the argon2id hash of pw as a PHC string with a fresh
// random salt.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	release := acquireHashSlot()
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	release()
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

var errMalformedHash = errors.New("malformed argon2id hash")

// VerifyPassword reports whether pw matches the PHC string encoded. It uses the
// parameters stored in the hash and compares in constant time. A hash that is
// not a well-formed argon2id string, or whose parameters are outside sane
// bounds, returns false and an error.
func VerifyPassword(pw, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errMalformedHash
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return false, fmt.Errorf("%w: unsupported version %q", errMalformedHash, parts[2])
	}
	var m, t, p uint64
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return false, errMalformedHash
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return false, fmt.Errorf("%w: parameter %q", errMalformedHash, k)
		}
		switch k {
		case "m":
			m = n
		case "t":
			t = n
		case "p":
			p = n
		default:
			return false, fmt.Errorf("%w: unknown parameter %q", errMalformedHash, k)
		}
	}
	if m < 8 || m > maxArgonMemoryKiB || t < 1 || t > maxArgonTime || p < 1 || p > maxArgonThreads || m < 8*p {
		return false, fmt.Errorf("%w: parameters out of range", errMalformedHash)
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < minArgonSaltLen {
		return false, fmt.Errorf("%w: salt", errMalformedHash)
	}
	want, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(want) < minArgonKeyLen || len(want) > maxArgonKeyLen {
		return false, fmt.Errorf("%w: key", errMalformedHash)
	}

	release := acquireHashSlot()
	got := argon2.IDKey([]byte(pw), salt, uint32(t), uint32(m), uint8(p), uint32(len(want))) //nolint:gosec // m, t, p and the key length were bounded above
	release()
	return subtle.ConstantTimeCompare(want, got) == 1, nil
}

// dummyHash is verified against when a sign-in names an unknown user, so a
// missing account costs the same time as a wrong password and cannot be told
// apart by timing (D-30). It is hashed once, lazily, with the current
// parameters, so it stays in step when they are raised.
var dummyHash = sync.OnceValue(func() string {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	h, _ := HashPassword(string(secret))
	return h
})

// DummyVerify spends one argon2id verification on pw and discards the result.
func DummyVerify(pw string) {
	_, _ = VerifyPassword(pw, dummyHash())
}
