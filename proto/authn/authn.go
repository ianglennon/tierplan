// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package authn holds what both ends of the agent handshake compute: the signed
// transcript, Ed25519 signing and verification, the SPKI pin of a server key,
// and the TLS channel-binding material. Server and agent share this code so
// they cannot disagree about a byte of it. Standard library only.
package authn

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

const (
	// transcriptDomain separates this signature from any other use of the same
	// key, and carries the transcript version.
	transcriptDomain = "bayplan-agent-auth/v1\x00"
	// ekmLabel is the TLS exporter label of the channel binding.
	ekmLabel = "EXPORTER-bayplan-agent-auth-v1"
	// ekmLen is the length of the exported keying material.
	ekmLen = 32
	// maxField is the longest field a two-byte length prefix can describe.
	maxField = 0xFFFF
)

// Transcript is what an agent signs: SHA-256 over a domain separator and each
// field, in the order mode, proto, serverNonce, agentNonce, aud, certOrPub, ekm,
// each preceded by its length as a two-byte big-endian integer. Changing any
// field changes the digest, so a signature made for one audience, nonce or key
// does not verify for another.
//
// A field longer than 65535 bytes cannot be described by the prefix. The
// handshake limit (16 KiB) means none ever is; if one were, the prefix is
// 0xFFFF followed by the SHA-256 of the field, so two long fields still never
// collide with a short one.
func Transcript(mode string, proto int, serverNonce, agentNonce []byte, aud string, certOrPub, ekm []byte) []byte {
	h := sha256.New()
	h.Write([]byte(transcriptDomain))
	for _, f := range [][]byte{
		[]byte(mode), []byte(strconv.Itoa(proto)), serverNonce, agentNonce, []byte(aud), certOrPub, ekm,
	} {
		if len(f) >= maxField {
			sum := sha256.Sum256(f)
			h.Write([]byte{0xFF, 0xFF})
			h.Write(sum[:])
			continue
		}
		h.Write([]byte{byte(len(f) >> 8), byte(len(f))}) //nolint:gosec // len(f) < maxField, so it fits two bytes
		h.Write(f)
	}
	return h.Sum(nil)
}

// Sign signs a transcript with the agent's private key.
func Sign(priv ed25519.PrivateKey, transcript []byte) []byte {
	return ed25519.Sign(priv, transcript)
}

// Verify reports whether sig is a valid signature of transcript under pub. A
// key of the wrong length verifies nothing, and never panics.
func Verify(pub []byte, transcript, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), transcript, sig)
}

// SPKIPin is the pin of a certificate's public key: "sha256/" and the standard
// base64 of the SHA-256 of its SubjectPublicKeyInfo. A pin stays valid while the
// key does, however often the certificate around it is re-issued.
func SPKIPin(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256/" + base64.StdEncoding.EncodeToString(sum[:])
}

// EKM is the channel-binding material of a TLS connection, or nil when the
// connection has none (plain HTTP, or a state that cannot export).
func EKM(cs *tls.ConnectionState) []byte {
	if cs == nil {
		return nil
	}
	out, err := cs.ExportKeyingMaterial(ekmLabel, nil, ekmLen)
	if err != nil {
		return nil
	}
	return out
}

// EKMEqual compares two exporter values in constant time. Two empty values are
// not equal: "no binding" is never a match.
func EKMEqual(a, b []byte) bool {
	return len(a) > 0 && len(a) == len(b) && bytes.Equal(a, b)
}

// ErrNoHost means a URL has no host to be the audience of a handshake.
var ErrNoHost = errors.New("authn: the URL has no host")

// Audience is the host[:port] a handshake is addressed to, taken from the URL the
// agent dials or the public URL the server holds. Both sides use this one
// function, so they compare the same spelling: the host is lower-case and the
// default port of the scheme (443 for https and wss, 80 for http and ws) is left
// out.
func Audience(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", ErrNoHost
	}
	port := u.Port()
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		if port == "443" {
			port = ""
		}
	case "http", "ws":
		if port == "80" {
			port = ""
		}
	}
	if strings.Contains(host, ":") { // an IPv6 literal keeps its brackets
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return host, nil
}
