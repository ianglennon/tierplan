// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"time"

	legoacme "github.com/go-acme/lego/v5/acme"
	"github.com/go-acme/lego/v5/acme/api"
)

// RenewalWindow is what the CA suggests for renewing one certificate (RFC 9773): the window to renew
// in and how long to wait before asking again.
type RenewalWindow struct {
	Start, End time.Time
	// RetryAfter is the CA's Retry-After for the answer; zero when it sent none.
	RetryAfter time.Duration
}

// RenewalWindow asks the CA when to renew leaf. ok is false, with a nil error, when there is no
// answer to give: the directory offers no renewal information, the certificate has no renewal
// identifier, or the CA does not know the certificate. A failure to ask (the directory cannot be read,
// the CA answers with an error) is returned, and the caller keeps what it had. A client with no
// account for its directory gets ErrNoAccount.
//
// It is the per-hostname manager's counterpart of the own scheduler's askARI, which stays as it is; the
// error mapping is copied from there on purpose.
func (c *Client) RenewalWindow(ctx context.Context, leaf *x509.Certificate) (RenewalWindow, bool, error) {
	meta, err := c.Meta(ctx)
	if err != nil {
		return RenewalWindow{}, false, err
	}
	if !meta.RenewalInfo {
		return RenewalWindow{}, false, nil
	}
	core, _, _, err := c.core(ctx)
	if err != nil {
		return RenewalWindow{}, false, err
	}
	id, err := api.MakeARICertID(leaf)
	if err != nil {
		return RenewalWindow{}, false, nil
	}
	info, err := core.Certificates.GetRenewalInfo(ctx, id)
	var problem *legoacme.ProblemDetails
	switch {
	case errors.Is(err, api.ErrNoARI), errors.As(err, &problem) && problem.HTTPStatus == http.StatusNotFound:
		return RenewalWindow{}, false, nil
	case err != nil:
		return RenewalWindow{}, false, err
	}
	return RenewalWindow{
		Start: info.SuggestedWindow.Start, End: info.SuggestedWindow.End, RetryAfter: info.RetryAfter,
	}, true, nil
}
