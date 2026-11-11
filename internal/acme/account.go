// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"fmt"

	legoacme "github.com/go-acme/lego/v5/acme"

	"github.com/ianglennon/tierplan/internal/config"
)

var (
	// ErrAccountExists is returned by Register when this directory already has an account. Nothing is
	// sent to the CA.
	ErrAccountExists = errors.New("an ACME account is already registered with this directory")
	// ErrTOSNotAccepted is returned by Register when the terms of service were not agreed to.
	ErrTOSNotAccepted = errors.New("the certificate authority's terms of service must be accepted")
	// ErrContactInvalid is returned by Register when the email is not a bare address.
	ErrContactInvalid = errors.New("the contact email is not a valid address")
	// ErrNoAccount is returned when something needs the account and there is none for this directory.
	ErrNoAccount = errors.New("no ACME account is registered with this directory")
)

// Register creates the account for this directory, agreeing to its terms of service, and seals its
// fresh key before it returns. The email is optional (D-06): without one no contact is sent. At most
// one account exists per directory: a second call, or a concurrent one, gets ErrAccountExists and
// sends nothing.
func (c *Client) Register(ctx context.Context, email string, agreeTOS bool) (Account, error) {
	if !agreeTOS {
		return Account{}, ErrTOSNotAccepted
	}
	if email != "" && !config.ValidContactEmail(email) {
		return Account{}, ErrContactInvalid
	}

	c.register.Lock()
	defer c.register.Unlock()

	if _, _, found, err := c.store.Account(ctx, c.dir.URL); err != nil {
		return Account{}, err
	} else if found {
		return Account{}, ErrAccountExists
	}

	key, err := newKey()
	if err != nil {
		return Account{}, err
	}
	core, err := c.newCore(ctx, "", key)
	if err != nil {
		return Account{}, fmt.Errorf("reach the ACME directory %s: %w", c.dir.URL, err)
	}
	req := legoacme.Account{TermsOfServiceAgreed: true}
	contact := ""
	if email != "" {
		contact = "mailto:" + email
		req.Contact = []string{contact}
	}
	created, err := core.Accounts.New(ctx, req)
	if err != nil {
		return Account{}, fmt.Errorf("register an ACME account with %s: %w", c.dir.URL, err)
	}
	if created.Location == "" {
		return Account{}, fmt.Errorf("register an ACME account with %s: the CA gave no account URL", c.dir.URL)
	}
	acct := Account{
		DirectoryURL:   c.dir.URL,
		URL:            created.Location,
		Contact:        contact,
		TermsOfService: core.GetDirectory().Meta.TermsOfService,
		CreatedAt:      c.clock.Now(),
	}
	if err := c.store.SaveAccount(ctx, acct, key); err != nil {
		return Account{}, err
	}
	return acct, nil
}

// Account returns the account registered with this directory, if any. It never carries the key.
func (c *Client) Account(ctx context.Context) (Account, bool, error) {
	acct, _, found, err := c.store.Account(ctx, c.dir.URL)
	if err != nil || !found {
		return Account{}, false, err
	}
	return acct, true, nil
}

// Forget deletes the stored account for this directory, for when the CA says the account is gone, so
// that Register can create a new one.
func (c *Client) Forget(ctx context.Context) error {
	c.register.Lock()
	defer c.register.Unlock()
	return c.store.ForgetAccount(ctx, c.dir.URL)
}
