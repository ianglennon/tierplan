// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"sync/atomic"

	"filippo.io/age"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// stanzaType is the age recipient stanza type of the backup passphrase.
	stanzaType = "bayplan-argon2id"
	// wrapInfoLabel is the HKDF info prefix of the key that wraps the age file
	// key. The SHA-256 of the outer header follows it (T-03-05).
	wrapInfoLabel = "bayplan/backup-wrap/v1"
	wrapKeyLen    = 32
)

// maxKDFSlots bounds the argon2id derivations running at once. Each can use
// up to maxKDFMemoryKiB, so an unbounded burst of uploads would otherwise be a
// memory-exhaustion lever (RESEARCH Pitfall 7).
const maxKDFSlots = 2

var kdfSlots = make(chan struct{}, maxKDFSlots)

// kdfRuns counts the argon2id derivations started. Tests read it to prove a
// header that breaks a cap is refused before any derivation runs.
var kdfRuns atomic.Int64

// deriveWrapKey runs argon2id over the passphrase with the header's
// parameters, then HKDF-SHA256 over the result with the header hash in the
// info, and returns the key that wraps the age file key.
func deriveWrapKey(pass []byte, p KDFParams, headerHash [sha256.Size]byte) ([]byte, error) {
	kdfSlots <- struct{}{}
	defer func() { <-kdfSlots }()
	kdfRuns.Add(1)

	ikm := argon2.IDKey(pass, p.Salt, p.T, p.MKiB, p.P, wrapKeyLen)
	info := append([]byte(wrapInfoLabel), headerHash[:]...)
	key := make([]byte, wrapKeyLen)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, nil, info), key); err != nil {
		return nil, fmt.Errorf("derive backup wrap key: %w", err)
	}
	return key, nil
}

// stanzaArgs is the argument list of the stanza: salt, t, m_kib and p. They
// repeat the header's parameters so the age header is self-describing, and
// Unwrap insists they match the header, so the two cannot disagree.
func stanzaArgs(p KDFParams) []string {
	return []string{
		base64.RawStdEncoding.EncodeToString(p.Salt),
		strconv.FormatUint(uint64(p.T), 10),
		strconv.FormatUint(uint64(p.MKiB), 10),
		strconv.FormatUint(uint64(p.P), 10),
	}
}

// argonRecipient wraps the age file key under a key derived from the
// passphrase and bound to the outer header.
type argonRecipient struct {
	params     KDFParams
	pass       []byte
	headerHash [sha256.Size]byte
}

var _ age.Recipient = (*argonRecipient)(nil)

// Wrap implements age.Recipient.
func (r *argonRecipient) Wrap(fileKey []byte) ([]*age.Stanza, error) {
	key, err := deriveWrapKey(r.pass, r.params, r.headerHash)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("create backup wrap cipher: %w", err)
	}
	// A zero nonce is safe: the wrap key is unique per file because the salt
	// is random, and it seals exactly one message.
	nonce := make([]byte, aead.NonceSize())
	body := aead.Seal(nil, nonce, fileKey, nil) //nolint:gosec // one message per single-use key, see above
	return []*age.Stanza{{Type: stanzaType, Args: stanzaArgs(r.params), Body: body}}, nil
}

// argonIdentity unwraps the age file key with the passphrase.
type argonIdentity struct {
	params     KDFParams
	pass       []byte
	headerHash [sha256.Size]byte
}

var _ age.Identity = (*argonIdentity)(nil)

// errBadStanza reports a header the identity will not unwrap.
var errBadStanza = errors.New("backup key stanza does not match the file header")

// Unwrap implements age.Identity. It accepts exactly one stanza, of the
// backup type, whose arguments equal the outer header's parameters; anything
// else is an error. A wrong passphrase and a damaged file are deliberately not
// told apart.
func (i *argonIdentity) Unwrap(stanzas []*age.Stanza) ([]byte, error) {
	if len(stanzas) != 1 {
		return nil, errBadStanza
	}
	s := stanzas[0]
	if s.Type != stanzaType || !slices.Equal(s.Args, stanzaArgs(i.params)) {
		return nil, errBadStanza
	}
	key, err := deriveWrapKey(i.pass, i.params, i.headerHash)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("create backup wrap cipher: %w", err)
	}
	fileKey, err := aead.Open(nil, make([]byte, aead.NonceSize()), s.Body, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", age.ErrIncorrectIdentity, err)
	}
	return fileKey, nil
}
