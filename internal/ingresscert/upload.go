// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"time"
)

// The limits on an uploaded certificate and key (T-20-13): each input is bounded before anything is parsed.
const (
	// MaxUploadBytes is the most bytes the chain, and the most bytes the key, may hold.
	MaxUploadBytes = 256 << 10
	// MaxChainCerts is the most certificates a chain may hold.
	MaxChainCerts = 8
)

// The refusal codes of an upload (D-23). A code is stable: the API and its tests key on it.
const (
	UploadKeyMismatch     = "ingress_cert_key_mismatch"
	UploadHostNotCovered  = "ingress_cert_host_not_covered"
	UploadExpired         = "ingress_cert_expired"
	UploadNotYetValid     = "ingress_cert_not_yet_valid"
	UploadChainUnparsable = "ingress_cert_chain_unparsable"
	UploadKeyUnsupported  = "ingress_cert_key_unsupported"
	UploadKeyEncrypted    = "ingress_cert_key_encrypted"
	UploadTooLarge        = "ingress_cert_too_large"
)

// WarningChainUntrusted is the warning an accepted upload carries when its chain does not verify against the
// trust roots: an internal CA is a valid reason to upload, so it is never a refusal.
const WarningChainUntrusted = "chain_untrusted"

// The key sizes an upload may have.
const (
	minRSABits = 2048
	maxRSABits = 8192
)

// UploadError is the refusal of an upload. Its text is a fixed sentence per code and never holds a byte of
// the input, so a refusal can be logged and shown without leaking the key.
type UploadError struct {
	Code string
}

// Error returns the fixed sentence of the code.
func (e *UploadError) Error() string {
	switch e.Code {
	case UploadKeyMismatch:
		return "The private key does not belong to the certificate."
	case UploadHostNotCovered:
		return "The certificate does not cover the hostname."
	case UploadExpired:
		return "The certificate has expired."
	case UploadNotYetValid:
		return "The certificate is not valid yet."
	case UploadChainUnparsable:
		return "The certificate chain could not be read: it must be PEM certificates, the server certificate first."
	case UploadKeyUnsupported:
		return "The private key is not a supported key: use an unencrypted RSA key of 2048 to 8192 bits, or an ECDSA key on P-256, P-384 or P-521."
	case UploadKeyEncrypted:
		return "The private key is encrypted. Remove its passphrase and upload it again."
	case UploadTooLarge:
		return "The certificate chain or the key is too large."
	default:
		return "The uploaded certificate was refused."
	}
}

// UploadResult is an accepted upload: the leaf, the chain re-encoded as PEM certificates only, the key as
// PKCS#8 PEM (the form every sealed key of this package takes) and the warnings.
type UploadResult struct {
	Leaf        *x509.Certificate
	ChainPEM    []byte
	KeyPKCS8PEM []byte
	Warnings    []string
}

// ValidateUpload judges an uploaded chain and key for hostname at now (D-23). The checks run in this order,
// each refusing with one code: sizes, the chain's syntax, the key (encrypted, then its type and size), the
// key against the leaf, the hostname against the leaf's names, and the validity window. A chain that does not
// verify against roots (nil means the system pool) is accepted with WarningChainUntrusted. It is pure and
// reads only the standard library, because its input is untrusted. hostname must already be normalized.
func ValidateUpload(hostname string, chainPEM, keyPEM []byte, now time.Time, roots *x509.CertPool) (UploadResult, error) {
	if len(chainPEM) > MaxUploadBytes || len(keyPEM) > MaxUploadBytes {
		return UploadResult{}, &UploadError{Code: UploadTooLarge}
	}
	certs, err := parseChain(chainPEM)
	if err != nil {
		return UploadResult{}, err
	}
	key, err := parseUploadKey(keyPEM)
	if err != nil {
		return UploadResult{}, err
	}
	leaf := certs[0]
	pub, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(leaf.PublicKey) {
		return UploadResult{}, &UploadError{Code: UploadKeyMismatch}
	}
	if err := leaf.VerifyHostname(hostname); err != nil {
		return UploadResult{}, &UploadError{Code: UploadHostNotCovered}
	}
	if now.Before(leaf.NotBefore) {
		return UploadResult{}, &UploadError{Code: UploadNotYetValid}
	}
	if now.After(leaf.NotAfter) {
		return UploadResult{}, &UploadError{Code: UploadExpired}
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return UploadResult{}, &UploadError{Code: UploadKeyUnsupported}
	}
	res := UploadResult{
		Leaf:        leaf,
		ChainPEM:    encodeChain(certs),
		KeyPKCS8PEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: intermediates, DNSName: hostname, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		res.Warnings = append(res.Warnings, WarningChainUntrusted)
	}
	return res, nil
}

// parseChain reads the PEM chain: certificates only, the server certificate first. Anything else in it (a
// key, text, a damaged block) makes it unparsable.
func parseChain(chainPEM []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := chainPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, &UploadError{Code: UploadChainUnparsable}
		}
		if len(certs) == MaxChainCerts {
			return nil, &UploadError{Code: UploadTooLarge}
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, &UploadError{Code: UploadChainUnparsable}
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, &UploadError{Code: UploadChainUnparsable}
	}
	return certs, nil
}

// encodeChain writes certificates as PEM.
func encodeChain(certs []*x509.Certificate) []byte {
	var out []byte
	for _, c := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}

// parseUploadKey reads the uploaded private key in PKCS#8, PKCS#1 or SEC1 form. An encrypted key, a key that
// cannot be read and a key of a type or size outside the allow-list are refused with their own codes. The
// first PEM block that is neither a certificate nor EC parameters is the key, so a bundle that lists the
// certificates before the key, or a key with the parameters block openssl writes first, is read the same way a
// pasted key is.
func parseUploadKey(keyPEM []byte) (crypto.Signer, error) {
	rest := keyPEM
	var block *pem.Block
	for {
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, &UploadError{Code: UploadKeyUnsupported}
		}
		// The EC PARAMETERS block that `openssl ecparam -genkey` writes before the key only names the curve,
		// which the key itself names too.
		if block.Type != "CERTIFICATE" && block.Type != "EC PARAMETERS" {
			break
		}
	}
	if block.Type == "ENCRYPTED PRIVATE KEY" || isEncryptedBlock(block) {
		return nil, &UploadError{Code: UploadKeyEncrypted}
	}
	var parsed any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, &UploadError{Code: UploadKeyUnsupported}
	}
	if err != nil {
		return nil, &UploadError{Code: UploadKeyUnsupported}
	}
	switch k := parsed.(type) {
	case *rsa.PrivateKey:
		if bits := k.N.BitLen(); bits < minRSABits || bits > maxRSABits {
			return nil, &UploadError{Code: UploadKeyUnsupported}
		}
		return k, nil
	case *ecdsa.PrivateKey:
		switch k.Curve {
		case elliptic.P256(), elliptic.P384(), elliptic.P521():
			return k, nil
		}
	}
	return nil, &UploadError{Code: UploadKeyUnsupported}
}

// isEncryptedBlock reports whether a legacy PEM block carries a passphrase-encrypted body (the Proc-Type
// header of the old OpenSSL format).
func isEncryptedBlock(b *pem.Block) bool {
	return strings.Contains(strings.ToUpper(b.Headers["Proc-Type"]), "ENCRYPTED")
}
