// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// encodeECKey writes an ECDSA key as PKCS#8 PEM, the form every sealed key of this package takes.
func encodeECKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("ingresscert: encode the key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// decodeECKey reads an ECDSA P-256 key from its PKCS#8 PEM encoding: the key of an ACME account or of an order,
// which acme.Orderer and acme.Client type as *ecdsa.PrivateKey.
func decodeECKey(data []byte) (*ecdsa.PrivateKey, error) {
	signer, err := decodeKey(data)
	if err != nil {
		return nil, err
	}
	key, ok := signer.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("ingresscert: the stored key is not an ECDSA P-256 key")
	}
	return key, nil
}
