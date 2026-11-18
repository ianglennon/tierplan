// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"
)

// pkcs8 encodes a key as PKCS#8 PEM.
func pkcs8(t *testing.T, key crypto.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encode a key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// rsaKey generates an RSA key of the given size.
func rsaKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("generate an RSA %d key: %v", bits, err)
	}
	return key
}

// ecKey generates an ECDSA key on curve.
func ecKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("generate an ECDSA key: %v", err)
	}
	return key
}

// pubOf returns the public half of a generated key.
func pubOf(key crypto.PrivateKey) crypto.PublicKey {
	return key.(interface{ Public() crypto.PublicKey }).Public()
}

// TestValidateUpload is the matrix of D-23: every refusal has its stable code, a wildcard covers one label,
// and a chain that does not verify is a warning.
func TestValidateUpload(t *testing.T) {
	ca := newTestCA(t)
	const host = "app.example.com"

	// Keys are made once: an RSA key of several thousand bits is slow to generate.
	rsa2048, rsa4096, rsa1024 := rsaKey(t, 2048), rsaKey(t, 4096), rsaKey(t, 1024)
	rsaHuge := rsaKey(t, 8200)
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate an Ed25519 key: %v", err)
	}
	p256, p384, p521 := ecKey(t, elliptic.P256()), ecKey(t, elliptic.P384()), ecKey(t, elliptic.P521())

	leafFor := func(key crypto.PrivateKey, o leafOpts) []byte { return ca.issue(t, pubOf(key), o) }
	hostOpts := leafOpts{sans: []string{host}}

	good := leafFor(p256, hostOpts)
	goodKey := pkcs8(t, p256)
	other := ecKey(t, elliptic.P256())
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsa2048)})
	sec1DER, err := x509.MarshalECPrivateKey(p384)
	if err != nil {
		t.Fatalf("encode a SEC1 key: %v", err)
	}
	sec1 := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1DER})
	encryptedPKCS8 := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("not a real envelope")})
	encryptedLegacy := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: []byte("not a real envelope"),
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00112233445566778899aabbccddeeff"},
	})
	nineCerts := bytes.Repeat(good, MaxChainCerts+1)

	cases := []struct {
		name     string
		host     string
		chain    []byte
		key      []byte
		roots    *x509.CertPool
		now      time.Time
		wantCode string
		warnings []string
	}{
		{name: "valid chain verifies against the roots", chain: good, key: goodKey, roots: ca.pool},
		{name: "valid chain with the system pool is untrusted", chain: good, key: goodKey, warnings: []string{WarningChainUntrusted}},
		{name: "key of another certificate", chain: good, key: pkcs8(t, other), roots: ca.pool, wantCode: UploadKeyMismatch},
		{name: "leaf for another hostname", chain: leafFor(p256, leafOpts{sans: []string{"other.example.com"}}), key: goodKey, roots: ca.pool, wantCode: UploadHostNotCovered},
		{name: "wildcard covers one label", chain: leafFor(p256, leafOpts{sans: []string{"*.example.com"}}), key: goodKey, roots: ca.pool},
		{name: "wildcard does not cover two labels", host: "a.b.example.com", chain: leafFor(p256, leafOpts{sans: []string{"*.example.com"}}), key: goodKey, roots: ca.pool, now: fixedNow, wantCode: UploadHostNotCovered},
		{name: "expired leaf", chain: leafFor(p256, leafOpts{sans: []string{host}, notBefore: fixedNow.Add(-48 * time.Hour), notAfter: fixedNow.Add(-time.Hour)}), key: goodKey, roots: ca.pool, wantCode: UploadExpired},
		{name: "not yet valid leaf", chain: leafFor(p256, leafOpts{sans: []string{host}, notBefore: fixedNow.Add(time.Hour), notAfter: fixedNow.Add(48 * time.Hour)}), key: goodKey, roots: ca.pool, wantCode: UploadNotYetValid},
		{name: "garbage in the chain", chain: []byte("this is not a certificate"), key: goodKey, roots: ca.pool, wantCode: UploadChainUnparsable},
		{name: "empty chain", chain: nil, key: goodKey, roots: ca.pool, wantCode: UploadChainUnparsable},
		{name: "a non-certificate block first", chain: append(append([]byte{}, goodKey...), good...), key: goodKey, roots: ca.pool, wantCode: UploadChainUnparsable},
		{name: "a damaged certificate", chain: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("damaged")}), key: goodKey, roots: ca.pool, wantCode: UploadChainUnparsable},
		{name: "trailing garbage after the chain", chain: append(append([]byte{}, good...), []byte("tail")...), key: goodKey, roots: ca.pool, wantCode: UploadChainUnparsable},
		{name: "Ed25519 key", chain: leafFor(edKey, hostOpts), key: pkcs8(t, edKey), roots: ca.pool, wantCode: UploadKeyUnsupported},
		{name: "RSA 1024 key", chain: leafFor(rsa1024, hostOpts), key: pkcs8(t, rsa1024), roots: ca.pool, wantCode: UploadKeyUnsupported},
		{name: "RSA above 8192 bits", chain: leafFor(rsaHuge, hostOpts), key: pkcs8(t, rsaHuge), roots: ca.pool, wantCode: UploadKeyUnsupported},
		{name: "a key that is not a key", chain: good, key: []byte("nothing"), roots: ca.pool, wantCode: UploadKeyUnsupported},
		{name: "RSA 2048 key", chain: leafFor(rsa2048, hostOpts), key: pkcs8(t, rsa2048), roots: ca.pool},
		{name: "RSA 4096 key", chain: leafFor(rsa4096, hostOpts), key: pkcs8(t, rsa4096), roots: ca.pool},
		{name: "ECDSA P-384 key", chain: leafFor(p384, hostOpts), key: pkcs8(t, p384), roots: ca.pool},
		{name: "ECDSA P-521 key", chain: leafFor(p521, hostOpts), key: pkcs8(t, p521), roots: ca.pool},
		{name: "RSA key in PKCS#1 form", chain: leafFor(rsa2048, hostOpts), key: pkcs1, roots: ca.pool},
		{name: "ECDSA key in SEC1 form", chain: leafFor(p384, hostOpts), key: sec1, roots: ca.pool},
		{name: "encrypted PKCS#8 key", chain: good, key: encryptedPKCS8, roots: ca.pool, wantCode: UploadKeyEncrypted},
		{name: "legacy encrypted key", chain: good, key: encryptedLegacy, roots: ca.pool, wantCode: UploadKeyEncrypted},
		{name: "a 300 KiB chain", chain: bytes.Repeat([]byte("# padding\n"), 30<<10), key: goodKey, roots: ca.pool, wantCode: UploadTooLarge},
		{name: "a 300 KiB key", chain: good, key: bytes.Repeat([]byte("# padding\n"), 30<<10), roots: ca.pool, wantCode: UploadTooLarge},
		{name: "nine certificates", chain: nineCerts, key: goodKey, roots: ca.pool, wantCode: UploadTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := tc.now
			if now.IsZero() {
				now = fixedNow
			}
			hostname := tc.host
			if hostname == "" {
				hostname = host
			}
			res, err := ValidateUpload(hostname, tc.chain, tc.key, now, tc.roots)
			if tc.wantCode != "" {
				var ue *UploadError
				if !errors.As(err, &ue) || ue.Code != tc.wantCode {
					t.Fatalf("ValidateUpload = %v, want code %s", err, tc.wantCode)
				}
				assertNoKeyBytes(t, err.Error(), tc.key)
				return
			}
			if err != nil {
				t.Fatalf("ValidateUpload refused: %v", err)
			}
			if strings.Join(res.Warnings, ",") != strings.Join(tc.warnings, ",") {
				t.Fatalf("warnings = %v, want %v", res.Warnings, tc.warnings)
			}
			if res.Leaf == nil || !bytes.HasPrefix(res.KeyPKCS8PEM, []byte("-----BEGIN PRIVATE KEY-----")) {
				t.Fatalf("an accepted upload returns the leaf and a PKCS#8 key; got %+v", res)
			}
			if _, err := decodeKey(res.KeyPKCS8PEM); err != nil {
				t.Fatalf("the returned key is not readable: %v", err)
			}
		})
	}
}

// assertNoKeyBytes fails when text holds any byte of the key's body.
func assertNoKeyBytes(t *testing.T, text string, keyPEM []byte) {
	t.Helper()
	block, _ := pem.Decode(keyPEM)
	if block == nil || len(block.Bytes) < 16 {
		return
	}
	body := base64.StdEncoding.EncodeToString(block.Bytes)
	for i := 0; i+16 <= len(body); i += 8 {
		if strings.Contains(text, body[i:i+16]) {
			t.Fatalf("a refusal message holds key bytes: %q", text)
		}
	}
}
