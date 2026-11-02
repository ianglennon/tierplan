// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"time"

	"github.com/ianglennon/tierplan/internal/vault"
)

// A sealed CA key is bound to its environment by the context
// vault.ContextAgentCA + the environment ID: the vault's AEAD authenticates the
// context, so a sealed key moved into another environment's row fails to open
// (D-07, D-55). The prefix is shared with vault.SealedColumns, which re-wraps it.
const (
	// caLifetime is how long an environment's CA is valid. It is created with the
	// environment and destroyed with it (D-07).
	caLifetime = 20 * 365 * 24 * time.Hour
	// leafLifetime is the life of an agent certificate (D-04).
	leafLifetime = 30 * 24 * time.Hour
	// leafBackdate covers the clock skew D-45 tolerates: a host whose clock is a
	// few minutes behind still sees a certificate that is already valid.
	leafBackdate = 5 * time.Minute
)

// ca is one environment's issuing authority.
type ca struct {
	cert    *x509.Certificate
	certPEM []byte
	key     ed25519.PrivateKey
}

// newCA creates an Ed25519 CA for envID that may sign leaves only, never another
// CA (MaxPathLen 0).
func newCA(envID string, now time.Time) (*ca, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate the CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "TierPlan agent CA " + envID},
		NotBefore:             now.Add(-leafBackdate).UTC(),
		NotAfter:              now.Add(caLifetime).UTC(),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("create the CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse the CA certificate: %w", err)
	}
	return &ca{cert: cert, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key: priv}, nil
}

// keyDER is the CA private key as PKCS#8, the form that is sealed.
func (c *ca) keyDER() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(c.key)
	if err != nil {
		return nil, fmt.Errorf("marshal the CA key: %w", err)
	}
	return der, nil
}

// seal encrypts the CA key with the vault. The returned envelope names the key
// that sealed it (v1.<keyID>.…), so master key rotation needs no format change
// (D-56).
func (c *ca) seal(v *vault.Vault, envID string) (string, error) {
	der, err := c.keyDER()
	if err != nil {
		return "", err
	}
	sealed, err := v.Seal(vault.ContextAgentCA+envID, der)
	if err != nil {
		return "", fmt.Errorf("seal the CA key: %w", err)
	}
	return sealed, nil
}

// loadCA opens a stored CA. A sealed key that was moved from another
// environment fails here with vault.ErrDecrypt.
func loadCA(v *vault.Vault, envID, certPEM, sealed string) (*ca, error) {
	der, err := v.Open(vault.ContextAgentCA+envID, sealed)
	if err != nil {
		return nil, fmt.Errorf("open the CA key of environment %s: %w", envID, err)
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse the CA key: %w", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("the stored CA key is not an Ed25519 key")
	}
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, errors.New("the stored CA certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the CA certificate: %w", err)
	}
	return &ca{cert: cert, certPEM: []byte(certPEM), key: priv}, nil
}

// Leaf is a certificate issued to one agent.
type Leaf struct {
	DER      []byte
	Serial   string
	NotAfter time.Time
}

// issue signs a client certificate for an agent: CN is the agent ID, OU the
// environment ID, and a URI SAN names the agent. It is valid from five minutes
// ago for 30 days and for client authentication only.
func (c *ca) issue(agentID, envID string, pub ed25519.PublicKey, now time.Time) (Leaf, error) {
	serial, err := randomSerial()
	if err != nil {
		return Leaf{}, err
	}
	notAfter := now.Add(leafLifetime).UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: agentID, OrganizationalUnit: []string{envID}},
		URIs:                  []*url.URL{{Scheme: "tierplan", Host: "agent", Path: "/" + agentID}},
		NotBefore:             now.Add(-leafBackdate).UTC(),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, pub, c.key)
	if err != nil {
		return Leaf{}, fmt.Errorf("issue the agent certificate: %w", err)
	}
	return Leaf{DER: der, Serial: hex.EncodeToString(serial.Bytes()), NotAfter: notAfter}, nil
}

// ErrLeafExpired is returned by VerifyLeaf for a certificate whose chain to the CA
// was proven and whose NotAfter has passed. It is never returned for a certificate
// the CA did not sign, so reporting it tells a caller nothing it did not already
// hold (T-02-21).
var ErrLeafExpired = errors.New("the agent certificate has expired")

// ErrLeafNotYetValid is returned by VerifyLeaf for a certificate whose chain to the
// CA was proven and whose NotBefore is still ahead of the caller's clock. Like
// ErrLeafExpired it is only ever returned after the chain is proven.
var ErrLeafNotYetValid = errors.New("the agent certificate is not valid yet")

// VerifyLeaf checks that a DER certificate chains to the CA in caPEM, is valid at
// now and is for client authentication. It is the check every connection of an
// enrolled agent passes, and the reason an agent of environment A cannot act as
// one of environment B (D-07).
//
// It proves the chain first, at a time inside the certificate's own validity, and
// decides the validity period itself afterwards. Go checks a leaf's dates before it
// looks at any chain, so verifying at the caller's time would report "expired" for
// a forged certificate whose dates are in the past, and a caller could use that to
// learn which environments exist (T-02-21). Here ErrLeafExpired and
// ErrLeafNotYetValid are returned only for a certificate this CA signed, and every
// other failure is a plain error. The skew allowance stays in the backdated
// NotBefore of issue (D-45).
func VerifyLeaf(caPEM string, der []byte, now time.Time) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(caPEM))
	if block == nil {
		return nil, errors.New("the CA certificate is not PEM")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the CA certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse the agent certificate: %w", err)
	}

	// A genuine leaf is never valid from before its CA, so one second after the
	// later of the two start dates is inside both windows. A window too short to
	// hold that instant cannot be proven, which is the same refusal as a forgery.
	proof := leaf.NotBefore
	if caCert.NotBefore.After(proof) {
		proof = caCert.NotBefore
	}
	proof = proof.Add(time.Second)
	if proof.After(leaf.NotAfter) || proof.After(caCert.NotAfter) {
		return nil, errors.New("verify the agent certificate: the chain cannot be proven in the certificate's validity window")
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, CurrentTime: proof, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, fmt.Errorf("verify the agent certificate: %w", err)
	}

	// The chain is proven, so the dates may now be reported.
	switch {
	case now.After(leaf.NotAfter):
		return nil, fmt.Errorf("verify the agent certificate: %w", ErrLeafExpired)
	case now.Before(leaf.NotBefore):
		return nil, fmt.Errorf("verify the agent certificate: %w", ErrLeafNotYetValid)
	case now.After(caCert.NotAfter) || now.Before(caCert.NotBefore):
		return nil, errors.New("verify the agent certificate: the CA certificate is not valid now")
	}
	return leaf, nil
}

// randomSerial is a random 128-bit positive serial number.
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate a certificate serial: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}
