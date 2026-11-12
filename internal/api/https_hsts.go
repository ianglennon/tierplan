// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The problem types the HSTS routes answer with, and the one Stop answers with while HSTS is on. The
// browser maps each to its own copy.
const (
	// problemHSTSLocked is the 409 TIERPLAN_HSTS answers with when it fixes the toggle (D-10, D-17).
	problemHSTSLocked = "hsts_locked"
	// problemHSTSOn is the 409 Stop using Let's Encrypt answers with while HSTS is on (D-26).
	problemHSTSOn = "hsts_on"
)

// The states of the HSTS header the status reports. They say what TierPlan does with
// Strict-Transport-Security now, and the page words each one.
const (
	// HSTSStateSending is a positive max-age for the verified public host.
	HSTSStateSending = "sending"
	// HSTSStateOffClearing is the toggle off over a certificate that verifies: max-age=0 is sent, so a
	// browser that holds a policy drops it.
	HSTSStateOffClearing = "off_clearing"
	// HSTSStateOffSilent is the toggle off over a certificate that cannot have been sent a policy:
	// nothing is sent.
	HSTSStateOffSilent = "off_silent"
	// HSTSStateNotSent is the toggle on and no header: the certificate is not trusted for the public
	// host, or is not a certificate HSTS may be sent over.
	HSTSStateNotSent = "not_sent"
	// HSTSStateWithdrawn is the toggle on inside the window before the certificate lapses, when max-age=0
	// is sent so a lapse never locks a browser out.
	HSTSStateWithdrawn = "withdrawn"
)

// ErrHSTSLocked is returned by the HSTS policy when TIERPLAN_HSTS fixes the toggle and a different
// value was asked for (D-10, D-17). Only the variable's name is shown.
type ErrHSTSLocked struct {
	// Var is the environment variable that fixes the toggle.
	Var string
}

func (e *ErrHSTSLocked) Error() string {
	return "the HSTS setting is set by the " + e.Var + " environment variable"
}

// HSTSWithdrawal says when and why the header is withdrawn: the end of the certificate it is measured
// from, and where that certificate comes from.
type HSTSWithdrawal struct {
	NotAfter time.Time
	// Source is lets_encrypt or cert_files.
	Source string
}

// HSTSView is everything the HSTS card shows, as the policy knows it. A zero time is absent.
type HSTSView struct {
	// Enabled is the effective toggle; StoredEnabled is the one in the database, which the variable
	// overrides without changing.
	Enabled, StoredEnabled bool
	// State is one of the HSTSState values.
	State string
	// Clearing is true while max-age=0 is sent.
	Clearing bool
	// MaxAge and Step are what is sent now, 0 when nothing positive is.
	MaxAge int64
	Step   int
	// Capped is true when a short-lived certificate holds the ramp below the full year.
	Capped bool
	// NextStepAt is when the next ramp step comes; zero at the last step the certificate may reach.
	NextStepAt time.Time
	// NotSentReason is why HSTS is on but not sent: a trust reason (self_signed, unknown_authority,
	// expired, name_mismatch, no_public_url), or empty.
	NotSentReason string
	// Withdrawal is set while the header is withdrawn ahead of a lapse.
	Withdrawal *HSTSWithdrawal
	// ForgetBy is the time after which no browser can still hold a policy TierPlan sent; zero when one
	// is being sent now or none can be held (D-26).
	ForgetBy time.Time
	// Lock is set while TIERPLAN_HSTS fixes the toggle; Stored is "on" or "off".
	Lock *HTTPSLockBody
	// PublicPort is the public URL's port when it is https on a port other than 443, otherwise 0.
	PublicPort int
}

// HTTPSHSTS is the HSTS policy as the HTTPS routes use it.
type HTTPSHSTS interface {
	// HSTSView describes the header now.
	HSTSView(ctx context.Context) (HSTSView, error)
	// SetHSTSEnabled stores the admin's choice and recomputes the decision before it returns. It
	// returns *ErrHSTSLocked, storing nothing, when TIERPLAN_HSTS fixes a different value.
	SetHSTSEnabled(ctx context.Context, on bool) error
}

// HTTPSHSTSWithdrawalBody is when the header is withdrawn and why.
type HTTPSHSTSWithdrawalBody struct {
	NotAfter time.Time `json:"notAfter" doc:"When the certificate ends. The header is withdrawn from a week before, or a third of its life before for a certificate shorter than three weeks"`
	Source   string    `json:"source" enum:"lets_encrypt,cert_files" doc:"Where that certificate comes from: Let's Encrypt, or the certificate files you supplied"`
}

// HTTPSHSTSBody is the whole HSTS card.
type HTTPSHSTSBody struct {
	Enabled       bool                     `json:"enabled" doc:"The effective HSTS setting: TIERPLAN_HSTS when it is set, otherwise the stored choice"`
	StoredEnabled bool                     `json:"storedEnabled" doc:"The choice kept in the database, which TIERPLAN_HSTS overrides without changing and which comes back when the variable is removed"`
	State         string                   `json:"state" enum:"sending,off_clearing,off_silent,not_sent,withdrawn" doc:"What TierPlan does with Strict-Transport-Security now: sending (a positive max-age for the public host), off_clearing (max-age=0, so a browser drops a policy it holds), off_silent (nothing, because no policy could have been sent), not_sent (on, but the certificate is not one HSTS may be sent over; see notSentReason) or withdrawn (max-age=0 ahead of the certificate's end)"`
	Clearing      bool                     `json:"clearing" doc:"True while max-age=0 is sent"`
	MaxAge        int64                    `json:"maxAge" doc:"The max-age in seconds being sent, 0 when none is or when clearing"`
	Step          int                      `json:"step" doc:"The ramp step being sent: 1 is five minutes, 2 a week, 3 thirty days, 4 a year. 0 when nothing positive is sent"`
	Capped        bool                     `json:"capped" doc:"True when a short-lived certificate stops the ramp at a week"`
	NextStepAt    *time.Time               `json:"nextStepAt,omitempty" doc:"When the next, longer step is sent; left out at the last step this certificate may reach and when nothing positive is sent"`
	NotSentReason string                   `json:"notSentReason,omitempty" enum:",self_signed,unknown_authority,expired,name_mismatch,no_public_url" doc:"Why HSTS is on but not sent: the certificate is the self-signed one, or does not verify for the public URL's host (unknown_authority, expired, name_mismatch, no_public_url). Empty otherwise"`
	Withdrawal    *HTTPSHSTSWithdrawalBody `json:"withdrawal,omitempty" doc:"Set while the header is withdrawn ahead of the certificate's end"`
	ForgetBy      *time.Time               `json:"forgetBy,omitempty" doc:"The time after which no browser can still hold a policy TierPlan sent. Left out while a policy is being sent and when none can be held. Stopping Let's Encrypt is safe once HSTS is off and this has passed"`
	Lock          *HTTPSLockBody           `json:"lock,omitempty" doc:"Set while the TIERPLAN_HSTS environment variable fixes the setting. Value and stored are on or off"`
	PublicPort    int                      `json:"publicPort,omitempty" doc:"The public URL's port when it is not 443. HSTS covers every port of the host name, so a policy sent here also makes browsers use HTTPS for other services on the same name. Left out on port 443"`
}

// hstsBody maps the policy's view onto the contract.
func hstsBody(v HSTSView) *HTTPSHSTSBody {
	b := &HTTPSHSTSBody{
		Enabled: v.Enabled, StoredEnabled: v.StoredEnabled, State: v.State, Clearing: v.Clearing,
		MaxAge: v.MaxAge, Step: v.Step, Capped: v.Capped, NotSentReason: v.NotSentReason,
		NextStepAt: utcOrNil(v.NextStepAt), ForgetBy: utcOrNil(v.ForgetBy), Lock: v.Lock, PublicPort: v.PublicPort,
	}
	if w := v.Withdrawal; w != nil {
		b.Withdrawal = &HTTPSHSTSWithdrawalBody{NotAfter: w.NotAfter.UTC(), Source: w.Source}
	}
	return b
}

// PutHTTPSHSTSInput is the body of the HSTS switch.
type PutHTTPSHSTSInput struct {
	Body struct {
		Enabled bool `json:"enabled" doc:"True to send Strict-Transport-Security over a certificate that verifies for the public host; false to stop and tell browsers to forget it"`
	}
}

// HTTPSHSTSOutput is the HSTS card after a change.
type HTTPSHSTSOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         HTTPSHSTSBody
}

// registerHTTPSHSTS wires the HSTS switch. It needs settings.write like every other change on the page
// and sits behind the same CSRF check; no new permission exists.
func registerHTTPSHSTS(r *httpx.Router, d HTTPSDeps) {
	write := rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal}
	httpx.Register(r, huma.Operation{
		OperationID: "putHTTPSHSTS",
		Method:      http.MethodPut,
		Path:        "/api/https/hsts",
		Summary:     "Turn HSTS on or off",
		Description: "Stores the choice and recomputes the header at once, so the next TLS response carries the new decision. " +
			"It is off by default. HSTS is sent only over a certificate that verifies for the public host, ramping from five minutes to a year, " +
			"and is withdrawn ahead of the certificate's end. Turning it off sends max-age=0 so browsers forget it. Answers the whole HSTS card. " +
			"While TIERPLAN_HSTS is set it decides: its own value is accepted and stores nothing, and any other value is refused with 409 " + problemHSTSLocked +
			". 503 " + problemHTTPSUnavailable + " on a server without the policy. Needs settings.write.",
		Tags: []string{"https"},
	}, write, func(ctx context.Context, in *PutHTTPSHSTSInput) (*HTTPSHSTSOutput, error) {
		if d.HSTS == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemHTTPSUnavailable, "HSTS is not available on this server.")
		}
		if err := d.HSTS.SetHSTSEnabled(ctx, in.Body.Enabled); err != nil {
			var locked *ErrHSTSLocked
			if errors.As(err, &locked) {
				return nil, httpx.Problem(http.StatusConflict, problemHSTSLocked, "Set by the "+locked.Var+" environment variable.")
			}
			return nil, acmeInternal(d.Log, "save the HSTS setting", err)
		}
		v, err := d.HSTS.HSTSView(ctx)
		if err != nil {
			return nil, acmeInternal(d.Log, "read the HSTS state", err)
		}
		return &HTTPSHSTSOutput{CacheControl: "no-store", Body: *hstsBody(v)}, nil
	})
}
