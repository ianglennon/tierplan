// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acmetest

import (
	"crypto"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/go-jose/go-jose/v4"
)

// jwsAlgorithms are the signature algorithms the fake accepts, the ones an ACME client uses for an
// RSA, P-256 or P-384 account key.
var jwsAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384,
}

// signedRequest is one ACME request body (RFC 8555 section 6.2): a flattened JWS whose protected
// header names the nonce, the URL and either the account key (jwk) or the account (kid).
type signedRequest struct {
	jws   *jose.JSONWebSignature
	nonce string
	url   string
	kid   string
	jwk   *jose.JSONWebKey
}

// parseSigned reads a flattened JWS request body. It checks the shape of the protected header; the
// signature is checked by verify once the key is known.
func parseSigned(body []byte) (*signedRequest, error) {
	jws, err := jose.ParseSigned(string(body), jwsAlgorithms)
	if err != nil {
		return nil, fmt.Errorf("parse JWS: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("request must carry exactly one signature")
	}
	h := jws.Signatures[0].Header
	u, _ := h.ExtraHeaders[jose.HeaderKey("url")].(string)
	r := &signedRequest{jws: jws, nonce: h.Nonce, url: u, kid: h.KeyID, jwk: h.JSONWebKey}
	switch {
	case r.url == "":
		return nil, errors.New(`protected header has no "url"`)
	case r.jwk != nil && r.kid != "":
		return nil, errors.New(`protected header has both "jwk" and "kid"`)
	case r.jwk == nil && r.kid == "":
		return nil, errors.New(`protected header has neither "jwk" nor "kid"`)
	}
	if r.jwk != nil && (!r.jwk.Valid() || !r.jwk.IsPublic()) {
		return nil, errors.New(`"jwk" is not a valid public key`)
	}
	return r, nil
}

// verify checks the signature against key and returns the payload (empty for a POST-as-GET).
func (r *signedRequest) verify(key *jose.JSONWebKey) ([]byte, error) {
	payload, err := r.jws.Verify(key)
	if err != nil {
		return nil, fmt.Errorf("verify JWS: %w", err)
	}
	return payload, nil
}

// thumbprint is the RFC 7638 SHA-256 thumbprint of key, base64url without padding: the half of an
// http-01 key authorization that comes from the account.
func thumbprint(key *jose.JSONWebKey) (string, error) {
	sum, err := key.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}
