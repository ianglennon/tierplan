// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package auth

import "errors"

// RED stub: signatures only, so the account tests compile and fail on their assertions.

const (
	MinPasswordLength   = 0
	MaxConcurrentHashes = 0
)

var (
	ErrInvalidUsername  = errors.New("invalid username")
	ErrPasswordTooShort = errors.New("password too short")
	ErrPasswordCommon   = errors.New("password too common")
)

func NormalizeUsername(s string) (string, error)  { return "", nil }
func ValidatePassword(pw, username string) error  { return nil }
func HashPassword(pw string) (string, error)      { return "", nil }
func VerifyPassword(pw, enc string) (bool, error) { return false, nil }
func DummyVerify(pw string)                       {}
