// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitconn

import (
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

// MaxPrivateKeyBytes is the longest private key accepted: 16 KiB, far above any real key.
const MaxPrivateKeyBytes = 16 << 10

// What a private key is refused with. None carries any part of the key.
var (
	// ErrKeyPassphrase means the key is encrypted with a passphrase, which Bayplan cannot use: a
	// deploy key is a key without one.
	ErrKeyPassphrase = errors.New("gitconn: the key needs a passphrase")
	// ErrKeyInvalid means the text is not a private key Bayplan can read.
	ErrKeyInvalid = errors.New("gitconn: not a private key Bayplan can read")
	// ErrKeyTooLarge means the text is over MaxPrivateKeyBytes.
	ErrKeyTooLarge = errors.New("gitconn: the private key is larger than 16 KiB")
)

// ParsedKey is a private key that was read, and what may be shown of it.
type ParsedKey struct {
	// Signer signs with the key, for the SSH transport.
	Signer ssh.Signer
	// AuthorizedKey is the public key in authorized_keys form, without a trailing newline: what to
	// add to the repository's deploy keys.
	AuthorizedKey string
	// Fingerprint is the SHA256 fingerprint of the public key (SHA256:...).
	Fingerprint string
}

// ParsePrivateKey reads an unencrypted private key (OpenSSH, PKCS#1, PKCS#8 or SEC1 PEM; ed25519,
// RSA, ECDSA). It returns ErrKeyTooLarge, ErrKeyPassphrase or ErrKeyInvalid, never an error that
// carries any part of the text.
func ParsePrivateKey(pemText string) (ParsedKey, error) {
	if len(pemText) > MaxPrivateKeyBytes {
		return ParsedKey{}, ErrKeyTooLarge
	}
	signer, err := ssh.ParsePrivateKey([]byte(pemText))
	var missing *ssh.PassphraseMissingError
	switch {
	case errors.As(err, &missing):
		return ParsedKey{}, ErrKeyPassphrase
	case err != nil:
		// The error is dropped, not wrapped: the parser's text must not become part of ours.
		return ParsedKey{}, ErrKeyInvalid
	}
	pub := signer.PublicKey()
	return ParsedKey{
		Signer:        signer,
		AuthorizedKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		Fingerprint:   ssh.FingerprintSHA256(pub),
	}, nil
}
