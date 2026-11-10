// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-acme/lego/v5/acme/api"

	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// LetsEncryptDirectory is the ACME directory TierPlan talks to unless the test-only override names
// another (D-19).
const LetsEncryptDirectory = "https://acme-v02.api.letsencrypt.org/directory"

const (
	// httpTimeout bounds every request to the CA, including the directory fetch lego makes with a
	// background context.
	httpTimeout = 30 * time.Second
	// metaTTL is how long the directory's terms and profiles are kept, metaErrTTL how long a failure
	// to read them is remembered, so a page that polls does not hammer a CA that is down.
	metaTTL    = 24 * time.Hour
	metaErrTTL = time.Minute
)

// Directory is the ACME directory TierPlan talks to and the roots that vouch for its HTTPS
// certificate.
type Directory struct {
	URL string
	// Overridden is true when TIERPLAN_ACME_DIRECTORY_URL named the directory: a test setup, which the
	// UI says loudly.
	Overridden bool
	// Roots is the pool the directory's certificate is verified against; nil means the system pool.
	Roots *x509.CertPool
}

// NewDirectory resolves the directory from the configuration: Let's Encrypt production when nothing
// is set. The CA file, when there is one, becomes a private pool for the ACME client only. It is never
// added to the system pool, and verification is never skipped.
func NewDirectory(cfg config.Config) (Directory, error) {
	if cfg.ACMEDirectoryURL == "" {
		if cfg.ACMECAFile != "" {
			return Directory{}, errors.New("TIERPLAN_ACME_CA_FILE is set without TIERPLAN_ACME_DIRECTORY_URL")
		}
		return Directory{URL: LetsEncryptDirectory}, nil
	}
	d := Directory{URL: cfg.ACMEDirectoryURL, Overridden: true}
	if cfg.ACMECAFile == "" {
		return d, nil
	}
	pem, err := os.ReadFile(cfg.ACMECAFile)
	if err != nil {
		return Directory{}, fmt.Errorf("TIERPLAN_ACME_CA_FILE: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return Directory{}, fmt.Errorf("TIERPLAN_ACME_CA_FILE %q holds no PEM certificate", cfg.ACMECAFile)
	}
	d.Roots = pool
	return d, nil
}

// newHTTPClient is a new client over a new transport on every call: lego wraps the transport it is
// given and keeps the client, so one shared client would be rewrapped by every Core.
func newHTTPClient(roots *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		},
	}
}

// DirectoryMeta is what the directory says about itself.
type DirectoryMeta struct {
	// TermsOfService is the URL of the terms an account agrees to; empty when the CA names none.
	TermsOfService string
	// Profiles maps each certificate profile the CA offers to its description (ACME-09).
	Profiles map[string]string
	// RenewalInfo is whether the CA offers ACME renewal information.
	RenewalInfo bool
}

// Client talks to one ACME directory on behalf of TierPlan.
type Client struct {
	dir   Directory
	store *Store
	clock stackops.Clock
	ua    string

	// register serialises Register and Forget so two admins cannot create two accounts.
	register sync.Mutex

	metaMu  sync.Mutex
	meta    DirectoryMeta
	metaErr error
	metaTil time.Time // zero until the first fetch
}

// NewClient is a Client for dir, keeping its account in store and judging time by clock.
func NewClient(dir Directory, store *Store, clock stackops.Clock, userAgent string) *Client {
	return &Client{dir: dir, store: store, clock: clock, ua: userAgent}
}

// Directory is the directory this client talks to.
func (c *Client) Directory() Directory { return c.dir }

// Meta returns the directory's terms of service, profiles and whether it offers renewalInfo. The answer
// is kept for 24 hours and a failure for a minute, both on the injected clock.
func (c *Client) Meta(ctx context.Context) (DirectoryMeta, error) {
	c.metaMu.Lock()
	defer c.metaMu.Unlock()
	now := c.clock.Now()
	if !c.metaTil.IsZero() && now.Before(c.metaTil) {
		return cloneMeta(c.meta), c.metaErr
	}
	m, err := c.fetchMeta(ctx)
	c.meta, c.metaErr = m, err
	if err != nil {
		c.metaTil = now.Add(metaErrTTL)
	} else {
		c.metaTil = now.Add(metaTTL)
	}
	return cloneMeta(m), err
}

func (c *Client) fetchMeta(ctx context.Context) (DirectoryMeta, error) {
	// Reading the directory signs nothing, so the key is a throwaway.
	key, err := newKey()
	if err != nil {
		return DirectoryMeta{}, err
	}
	core, err := c.newCore(ctx, "", key)
	if err != nil {
		return DirectoryMeta{}, fmt.Errorf("read the ACME directory %s: %w", c.dir.URL, err)
	}
	d := core.GetDirectory()
	profiles := make(map[string]string, len(d.Meta.Profiles))
	for name, desc := range d.Meta.Profiles {
		profiles[name] = desc
	}
	return DirectoryMeta{TermsOfService: d.Meta.TermsOfService, Profiles: profiles, RenewalInfo: d.RenewalInfo != ""}, nil
}

func cloneMeta(m DirectoryMeta) DirectoryMeta {
	if m.Profiles == nil {
		return m
	}
	p := make(map[string]string, len(m.Profiles))
	for k, v := range m.Profiles {
		p[k] = v
	}
	m.Profiles = p
	return m
}

// newCore opens the directory with a client of its own. lego fetches the directory with a background
// context, so the client's timeout is what bounds it; ctx is honoured before the call starts.
func (c *Client) newCore(ctx context.Context, kid string, key *ecdsa.PrivateKey) (*api.Core, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return api.New(newHTTPClient(c.dir.Roots), c.ua, c.dir.URL, kid, key)
}

// core returns a lego Core signed as the stored account for this directory, or ErrNoAccount.
func (c *Client) core(ctx context.Context) (*api.Core, Account, *ecdsa.PrivateKey, error) {
	acct, key, found, err := c.store.Account(ctx, c.dir.URL)
	if err != nil {
		return nil, Account{}, nil, err
	}
	if !found {
		return nil, Account{}, nil, ErrNoAccount
	}
	core, err := c.newCore(ctx, acct.URL, key)
	if err != nil {
		return nil, Account{}, nil, err
	}
	return core, acct, key, nil
}
