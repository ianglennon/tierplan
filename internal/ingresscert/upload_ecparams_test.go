// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"crypto/elliptic"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"testing"
)

// Phase 20 review WR-08: `openssl ecparam -name prime256v1 -genkey` writes an EC PARAMETERS block before the EC
// PRIVATE KEY. Such a key is read like any SEC1 key, alone or after the certificates; a parameters block with no
// key after it is still unsupported.
func TestValidateUploadReadsAKeyAfterECParameters(t *testing.T) {
	ca := newTestCA(t)
	const host = "app.example.com"
	key := ecKey(t, elliptic.P256())
	chain := ca.issue(t, &key.PublicKey, leafOpts{sans: []string{host}})

	oid, err := asn1.Marshal(asn1.ObjectIdentifier{1, 2, 840, 10045, 3, 1, 7}) // prime256v1
	if err != nil {
		t.Fatal(err)
	}
	params := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: oid})
	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ecKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})

	for name, keyPEM := range map[string][]byte{
		"openssl ecparam -genkey output":         append(append([]byte{}, params...), ecKeyPEM...),
		"certificates, parameters, then the key": append(append(append([]byte{}, chain...), params...), ecKeyPEM...),
	} {
		t.Run(name, func(t *testing.T) {
			res, err := ValidateUpload(host, chain, keyPEM, fixedNow, ca.pool)
			if err != nil {
				t.Fatalf("ValidateUpload = %v, want the key read", err)
			}
			if res.Leaf == nil || len(res.KeyPKCS8PEM) == 0 {
				t.Fatalf("ValidateUpload = %+v, want a leaf and the key", res)
			}
		})
	}

	t.Run("parameters and no key", func(t *testing.T) {
		_, err := ValidateUpload(host, chain, params, fixedNow, ca.pool)
		var ue *UploadError
		if !errors.As(err, &ue) || ue.Code != UploadKeyUnsupported {
			t.Fatalf("ValidateUpload = %v, want %s", err, UploadKeyUnsupported)
		}
	})
}
