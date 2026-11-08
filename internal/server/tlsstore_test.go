// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// newSealedStore is a sealedCertStore over a migrated temporary database and a vault with a
// generated key.
func newSealedStore(t *testing.T) sealedCertStore {
	t.Helper()
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return sealedCertStore{st: storetest.New(t), v: v}
}

func TestSealedCertStoreRoundTripsASealedKey(t *testing.T) {
	ctx := context.Background()
	s := newSealedStore(t)
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := []byte("-----BEGIN CERTIFICATE-----\nfirst\n-----END CERTIFICATE-----\n")
	t0 := time.Unix(1_700_000_000, 0)

	if err := s.Insert(ctx, "t", certPEM, keyPEM, t0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	gotCert, gotKey, found, err := s.Load(ctx, "t")
	if err != nil || !found {
		t.Fatalf("Load = found %v, %v; want found", found, err)
	}
	if !bytes.Equal(gotCert, certPEM) || !bytes.Equal(gotKey, keyPEM) {
		t.Errorf("Load returned a different certificate or key than was inserted")
	}

	var raw string
	if err := s.st.Read.QueryRowContext(ctx, `SELECT key_sealed FROM tls_certs WHERE id = 't'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "PRIVATE KEY") || raw == string(keyPEM) {
		t.Errorf("the key_sealed column holds the key in the clear")
	}

	next := []byte("-----BEGIN CERTIFICATE-----\nsecond\n-----END CERTIFICATE-----\n")
	t1 := t0.Add(time.Hour)
	if err := s.UpdateCert(ctx, "t", next, t1); err != nil {
		t.Fatalf("UpdateCert: %v", err)
	}
	gotCert, gotKey, found, err = s.Load(ctx, "t")
	if err != nil || !found || !bytes.Equal(gotCert, next) || !bytes.Equal(gotKey, keyPEM) {
		t.Errorf("after UpdateCert: found %v, err %v, certificate changed %v, key unchanged %v",
			found, err, bytes.Equal(gotCert, next), bytes.Equal(gotKey, keyPEM))
	}
	var created, updated int64
	if err := s.st.Read.QueryRowContext(ctx, `SELECT created_at, updated_at FROM tls_certs WHERE id = 't'`).Scan(&created, &updated); err != nil {
		t.Fatal(err)
	}
	if created != t0.UnixNano() || updated != t1.UnixNano() {
		t.Errorf("created_at, updated_at = %d, %d; want %d, %d", created, updated, t0.UnixNano(), t1.UnixNano())
	}

	if _, _, found, err := s.Load(ctx, "missing"); found || err != nil {
		t.Errorf("Load of an unknown id = found %v, %v; want not found and no error", found, err)
	}
}
