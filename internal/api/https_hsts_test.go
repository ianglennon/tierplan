// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The HSTS switch and the HSTS block of the HTTPS status (phase 16, TLS-06, D-10, D-11, D-12, D-14, D-17, D-22, D-23, D-26).

// stubHSTS is the policy as the routes see it: a view it hands out, a record of what was set, and an
// error to answer with.
type stubHSTS struct {
	mu      sync.Mutex
	view    api.HSTSView
	setErr  error
	viewErr error
	sets    []bool
}

func (s *stubHSTS) HSTSView(context.Context) (api.HSTSView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view, s.viewErr
}

func (s *stubHSTS) SetHSTSEnabled(_ context.Context, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.sets = append(s.sets, on)
	s.view.Enabled, s.view.StoredEnabled = on, on
	return nil
}

func (s *stubHSTS) setCalls() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.sets...)
}

// hstsFix is the Let's Encrypt fixture with an HSTS policy in place.
func newHSTSFix(t *testing.T, stub *stubHSTS) *acmeFix {
	t.Helper()
	f := newACMEFix(t, acmeOpts{})
	if stub != nil {
		f.https.HSTS = stub
	} else {
		f.https.HSTS = nil
	}
	f.h = f.build(t)
	return f
}

// hstsStatus reads the HTTPS status and decodes it; the hsts block may be nil.
func (f *acmeFix) hstsStatus(t *testing.T) api.HTTPSStatusBody {
	t.Helper()
	rec := f.get(t, "/api/https/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/https/status: %d %s", rec.Code, rec.Body)
	}
	var got api.HTTPSStatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode the status: %v", err)
	}
	return got
}

// The HSTS route needs settings.write at global scope, which httpx.VerifyAllGuarded and the route table both see (T-16-40).
func TestHSTSRouteDeclaresItsPermission(t *testing.T) {
	t.Parallel()
	r := httpx.New(httpx.Options{})
	api.RegisterHTTPS(r, api.HTTPSDeps{})
	var found bool
	for _, rt := range r.Routes() {
		if rt.Method == http.MethodPut && rt.Path == "/api/https/hsts" {
			found = true
			if rt.Req.Perm != rbac.PermSettingsWrite || rt.Req.Scope != rbac.ScopeGlobal {
				t.Errorf("PUT /api/https/hsts needs %q at scope %v, want settings.write at global scope", rt.Req.Perm, rt.Req.Scope)
			}
		}
	}
	if !found {
		t.Error("PUT /api/https/hsts is not registered")
	}
	if err := httpx.VerifyAllGuarded(r); err != nil {
		t.Errorf("VerifyAllGuarded: %v", err)
	}
}

// PUT stores the choice through the policy and answers the whole card.
func TestHSTSPutStoresTheChoiceAndAnswersTheCard(t *testing.T) {
	t.Parallel()
	stub := &stubHSTS{view: api.HSTSView{State: api.HSTSStateOffClearing, Clearing: true}}
	f := newHSTSFix(t, stub)

	rec := f.send(t, http.MethodPut, "/api/https/hsts", `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT hsts: %d %s, want 200", rec.Code, rec.Body)
	}
	if got := stub.setCalls(); len(got) != 1 || !got[0] {
		t.Errorf("the policy was told %v, want [true]", got)
	}
	var body api.HTTPSHSTSBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Enabled || !body.StoredEnabled {
		t.Errorf("the answer = %+v, want enabled and stored", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// A change without the CSRF proof, or without a body that says enabled, changes nothing (T-16-43).
func TestHSTSPutNeedsCSRFAndABody(t *testing.T) {
	t.Parallel()
	stub := &stubHSTS{}
	f := newHSTSFix(t, stub)

	rec := f.call(t, authReq{method: http.MethodPut, path: "/api/https/hsts", body: `{"enabled":true}`, cookie: f.cookie})
	if rec.Code != http.StatusForbidden {
		t.Errorf("PUT without the CSRF token: %d %s, want 403", rec.Code, rec.Body)
	}
	rec = f.send(t, http.MethodPut, "/api/https/hsts", `{}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("PUT with no enabled: %d %s, want 422", rec.Code, rec.Body)
	}
	if got := stub.setCalls(); len(got) != 0 {
		t.Errorf("the policy was told %v, want nothing", got)
	}
}

// While TIERPLAN_HSTS fixes the toggle the policy refuses and the route answers 409 hsts_locked, naming the variable.
func TestHSTSPutLockedAnswers409(t *testing.T) {
	t.Parallel()
	stub := &stubHSTS{setErr: &api.ErrHSTSLocked{Var: "TIERPLAN_HSTS"}}
	f := newHSTSFix(t, stub)

	rec := f.send(t, http.MethodPut, "/api/https/hsts", `{"enabled":false}`)
	acmeProblemOf(t, rec, http.StatusConflict, "hsts_locked")
	if !strings.Contains(rec.Body.String(), "Set by the TIERPLAN_HSTS environment variable.") {
		t.Errorf("body = %s, want it to name TIERPLAN_HSTS", rec.Body)
	}
	if got := stub.setCalls(); len(got) != 0 {
		t.Errorf("the policy stored %v, want nothing", got)
	}
}

// A failure nobody can act on is a plain 500, and a server without the policy answers 503.
func TestHSTSPutFailuresAndAServerWithoutThePolicy(t *testing.T) {
	t.Parallel()
	t.Run("an unexpected error", func(t *testing.T) {
		t.Parallel()
		f := newHSTSFix(t, &stubHSTS{setErr: errors.New("disk is full")})
		rec := f.send(t, http.MethodPut, "/api/https/hsts", `{"enabled":true}`)
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "disk is full") {
			t.Errorf("PUT hsts: %d %s, want a plain 500", rec.Code, rec.Body)
		}
	})
	t.Run("no policy", func(t *testing.T) {
		t.Parallel()
		f := newHSTSFix(t, nil)
		acmeProblemOf(t, f.send(t, http.MethodPut, "/api/https/hsts", `{"enabled":true}`), http.StatusServiceUnavailable, "https_unavailable")
		if got := f.hstsStatus(t); got.HSTS != nil {
			t.Errorf("a status without a policy carries hsts %+v, want none", got.HSTS)
		}
	})
}

// Every field of the view reaches the status: the effective and stored toggle, the state, the step and the next one,
// the withdrawal, the forget-by time and the lock (D-11, D-12, D-14, D-22, D-23, D-26).
func TestHSTSStatusCarriesEveryField(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name string
		view api.HSTSView
		want api.HTTPSHSTSBody
	}{
		{
			name: "sending at step 2 names the next step",
			view: api.HSTSView{Enabled: true, StoredEnabled: true, State: api.HSTSStateSending, MaxAge: 604800, Step: 2, NextStepAt: t0.Add(7 * 24 * time.Hour)},
			want: api.HTTPSHSTSBody{Enabled: true, StoredEnabled: true, State: "sending", MaxAge: 604800, Step: 2, NextStepAt: ptr(t0.Add(7 * 24 * time.Hour))},
		},
		{
			name: "the last step has no next step",
			view: api.HSTSView{Enabled: true, StoredEnabled: true, State: api.HSTSStateSending, MaxAge: 31536000, Step: 4},
			want: api.HTTPSHSTSBody{Enabled: true, StoredEnabled: true, State: "sending", MaxAge: 31536000, Step: 4},
		},
		{
			name: "a capped certificate stops at a week",
			view: api.HSTSView{Enabled: true, StoredEnabled: true, State: api.HSTSStateSending, MaxAge: 604800, Step: 2, Capped: true},
			want: api.HTTPSHSTSBody{Enabled: true, StoredEnabled: true, State: "sending", MaxAge: 604800, Step: 2, Capped: true},
		},
		{
			name: "off over a verified certificate clears and says when browsers forget",
			view: api.HSTSView{State: api.HSTSStateOffClearing, Clearing: true, ForgetBy: t0.Add(24 * time.Hour)},
			want: api.HTTPSHSTSBody{State: "off_clearing", Clearing: true, ForgetBy: ptr(t0.Add(24 * time.Hour))},
		},
		{
			name: "off over the self-signed certificate is silent",
			view: api.HSTSView{State: api.HSTSStateOffSilent},
			want: api.HTTPSHSTSBody{State: "off_silent"},
		},
		{
			name: "on over the self-signed certificate is not sent",
			view: api.HSTSView{Enabled: true, StoredEnabled: true, State: api.HSTSStateNotSent, NotSentReason: "self_signed"},
			want: api.HTTPSHSTSBody{Enabled: true, StoredEnabled: true, State: "not_sent", NotSentReason: "self_signed"},
		},
		{
			name: "an untrusted certificate file clears and is not sent",
			view: api.HSTSView{Enabled: true, StoredEnabled: true, State: api.HSTSStateNotSent, Clearing: true, NotSentReason: "unknown_authority"},
			want: api.HTTPSHSTSBody{Enabled: true, StoredEnabled: true, State: "not_sent", Clearing: true, NotSentReason: "unknown_authority"},
		},
		{
			name: "inside the withdrawal window",
			view: api.HSTSView{
				Enabled: true, StoredEnabled: true, State: api.HSTSStateWithdrawn, Clearing: true,
				Withdrawal: &api.HSTSWithdrawal{NotAfter: t0.Add(5 * 24 * time.Hour), Source: api.SourceLetsEncrypt},
			},
			want: api.HTTPSHSTSBody{
				Enabled: true, StoredEnabled: true, State: "withdrawn", Clearing: true,
				Withdrawal: &api.HTTPSHSTSWithdrawalBody{NotAfter: t0.Add(5 * 24 * time.Hour), Source: "lets_encrypt"},
			},
		},
		{
			name: "a public URL on a port other than 443 carries the port (WR-04)",
			view: api.HSTSView{State: api.HSTSStateOffSilent, PublicPort: 8443},
			want: api.HTTPSHSTSBody{State: "off_silent", PublicPort: 8443},
		},
		{
			name: "the variable fixes the toggle over the stored one",
			view: api.HSTSView{
				Enabled: true, State: api.HSTSStateNotSent, NotSentReason: "self_signed",
				Lock: &api.HTTPSLockBody{Var: "TIERPLAN_HSTS", Value: "on", Stored: "off"},
			},
			want: api.HTTPSHSTSBody{
				Enabled: true, State: "not_sent", NotSentReason: "self_signed",
				Lock: &api.HTTPSLockBody{Var: "TIERPLAN_HSTS", Value: "on", Stored: "off"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newHSTSFix(t, &stubHSTS{view: tc.view})
			got := f.hstsStatus(t).HSTS
			if got == nil {
				t.Fatal("the status has no hsts block")
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("hsts = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// ptr is a pointer to v, in UTC.
func ptr(v time.Time) *time.Time { u := v.UTC(); return &u }
