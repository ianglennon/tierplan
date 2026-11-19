// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/vault"
)

// The reasons Serving gives for a hostname it has no certificate pair for. They are the same strings the
// renderer uses as its skip reasons (D-09).
const (
	// ReasonNoCertificate: the environment has no certificate row for the hostname.
	ReasonNoCertificate = "no_certificate"
	// ReasonNotReady: the environment has rows for the hostname and none of them may be served now.
	ReasonNotReady = "certificate_not_ready"
)

// ServingCert is what the reconciler hands the renderer for one claimed hostname: the chain and the key
// exactly as stored, or the reason there is none. Reason is empty when the pair is servable. KeyPEM is the PKCS#8
// PEM as it was sealed, byte for byte, so the content-addressed file name made from it never changes between
// renders (Pitfall 6). It is never printed: String, GoString and LogValue show the hostname, the reason, the
// source and the length of the key only (T-21-06).
type ServingCert struct {
	Hostname string
	ChainPEM []byte
	KeyPEM   []byte
	NotAfter time.Time
	Source   string
	Reason   string
}

// String shows the hostname, the reason, the source and the key length, never the key or the chain.
func (c ServingCert) String() string {
	return fmt.Sprintf("ServingCert{hostname=%q reason=%q source=%q key_len=%d}", c.Hostname, c.Reason, c.Source, len(c.KeyPEM))
}

// GoString is String, so %#v cannot print the key either.
func (c ServingCert) GoString() string { return c.String() }

// LogValue is what a structured log shows of a ServingCert: the same four facts as String.
func (c ServingCert) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("hostname", c.Hostname),
		slog.String("reason", c.Reason),
		slog.String("source", c.Source),
		slog.Int("key_len", len(c.KeyPEM)),
	)
}

// Serving returns, for each requested hostname of environment envID, exactly one entry sorted by hostname: the
// servable chain and key PEM, or Reason ReasonNoCertificate when the environment has no row for it, or
// ReasonNotReady when it has rows and none may be served at now.
//
// It reads certs_version first and the certificate rows after, once, over EnvCerts, so the two cannot disagree
// in the unsafe direction: a certificate written between the reads is rendered again under its newer version
// (Pitfall 5). now is the caller's, never the store's own clock, so the reconciler owns time and a test is
// deterministic. The candidates and their order are Servable's: the live row, then the retired rows newest
// first, and the first that Ready accepts and whose key opens wins. A staging certificate is never ready, so
// it is never returned (PROH-21-01).
func (s *Store) Serving(ctx context.Context, envID string, hostnames []string, now time.Time) (certsVersion int64, certs []ServingCert, err error) {
	certsVersion, err = s.CertsVersion(ctx, envID)
	if err != nil {
		return 0, nil, err
	}
	rows, err := s.EnvCerts(ctx, envID)
	if err != nil {
		return 0, nil, err
	}
	byHost := map[string][]Cert{}
	for _, c := range rows {
		byHost[c.Hostname] = append(byHost[c.Hostname], c)
	}

	hosts := make([]string, 0, len(hostnames))
	for _, h := range hostnames {
		hosts = append(hosts, ingress.NormalizeHost(h))
	}
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)

	certs = make([]ServingCert, 0, len(hosts))
	for _, host := range hosts {
		cands := byHost[host]
		if len(cands) == 0 {
			certs = append(certs, ServingCert{Hostname: host, Reason: ReasonNoCertificate})
			continue
		}
		// The live row first, then the retired ones, newest retirement first (the order Servable uses).
		slices.SortStableFunc(cands, func(a, b Cert) int {
			switch {
			case a.RetiredAt.IsZero() && !b.RetiredAt.IsZero():
				return -1
			case !a.RetiredAt.IsZero() && b.RetiredAt.IsZero():
				return 1
			}
			return b.RetiredAt.Compare(a.RetiredAt)
		})
		entry := ServingCert{Hostname: host, Reason: ReasonNotReady}
		for _, c := range cands {
			if !Ready(c, now, true) {
				continue
			}
			keyPEM, err := s.openKeyPEM(c)
			if err != nil {
				continue
			}
			entry = ServingCert{Hostname: host, ChainPEM: c.ChainPEM, KeyPEM: keyPEM, NotAfter: c.NotAfter, Source: c.Source}
			break
		}
		certs = append(certs, entry)
	}
	return certsVersion, certs, nil
}

// openKeyPEM opens the sealed key of c under its row id and returns the PEM exactly as it was sealed, after
// checking that it is a PKCS#8 private key. It is never a re-marshal of a parsed key (Pitfall 6). The error
// names the row id only, never the key.
func (s *Store) openKeyPEM(c Cert) ([]byte, error) {
	if c.KeySealed == "" {
		return nil, ErrNoKey
	}
	keyPEM, err := s.v.Open(vault.ContextIngressCertKey+c.ID, c.KeySealed)
	if err != nil {
		return nil, fmt.Errorf("ingresscert: open the key of certificate %q: %w", c.ID, err)
	}
	if _, err := decodeKey(keyPEM); err != nil {
		return nil, fmt.Errorf("ingresscert: the key of certificate %q: %w", c.ID, err)
	}
	return keyPEM, nil
}
