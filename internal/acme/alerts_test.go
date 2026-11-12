// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

var alertsNow = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func kindsOf(as []Alert) []AlertKind {
	var out []AlertKind
	for _, a := range as {
		out = append(out, a.Kind)
	}
	return out
}

func TestDeriveAlerts(t *testing.T) {
	t.Parallel()
	next := alertsNow.Add(10 * time.Minute)
	failing := &ErrorView{
		Class: "rate_limited", Host: "tierplan.example.com", Profile: "shortlived", TermsURL: "https://ca.example/tos",
		Zone: "example.com", Target: "acme.dns.example", Resolvers: []string{"1.1.1.1"}, Detail: "ca-private-detail-1717",
	}
	cert := &CertView{NotAfter: alertsNow.Add(30 * 24 * time.Hour)}
	scheduleIssue := &RunView{Trigger: TriggerSchedule, Kind: KindIssue}
	adminIssue := &RunView{Trigger: TriggerAdmin, Kind: KindIssue}

	t.Run("a failed renewal on an active install is one renewal_failed alert with its facts", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{
			Mode: ModeActive, Certificate: cert, LastError: failing, Failures: 2, NextAttemptAt: next,
		}, Served{})
		want := []Alert{{
			Kind: AlertRenewalFailed, Class: "rate_limited", Host: "tierplan.example.com", Profile: "shortlived",
			TermsURL: "https://ca.example/tos", Zone: "example.com", Target: "acme.dns.example",
			Resolvers: []string{"1.1.1.1"}, NextAttemptAt: next,
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("a manual first Issue that failed gives no alert", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{Mode: ModeOff, LastError: failing, Failures: 1, LastRun: adminIssue}, Served{})
		if len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})

	t.Run("a first issue the environment keeps retrying gives renewal_failed with noCertificate", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{Mode: ModeOff, LastError: failing, Failures: 1, LastRun: scheduleIssue}, Served{})
		if len(got) != 1 || got[0].Kind != AlertRenewalFailed || !got[0].NoCertificate {
			t.Fatalf("got %+v, want one renewal_failed with NoCertificate", got)
		}
	})

	t.Run("a first issue with a certificate already stored is not the first issue", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{
			Mode: ModeOff, Certificate: cert, LastError: failing, Failures: 1, LastRun: scheduleIssue,
		}, Served{})
		if len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})

	for name, st := range map[string]Status{
		"no failures":       {Mode: ModeActive, Certificate: cert, LastError: failing},
		"certificate files": {Mode: ModeActive, Certificate: cert, LastError: failing, Failures: 2, CertFiles: true},
		"no last error":     {Mode: ModeActive, Certificate: cert, Failures: 2},
	} {
		t.Run("no renewal_failed with "+name, func(t *testing.T) {
			t.Parallel()
			if got := DeriveAlerts(alertsNow, st, Served{}); len(got) != 0 {
				t.Fatalf("got %+v, want none", got)
			}
		})
	}

	t.Run("the expiry fallback is a fallback alert with when it expired", func(t *testing.T) {
		t.Parallel()
		expired := alertsNow.Add(-time.Hour)
		got := DeriveAlerts(alertsNow, Status{Mode: ModeActive, Fallback: true, ExpiredAt: expired}, Served{})
		want := []Alert{{Kind: AlertFallback, NotAfter: expired}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("a restore pause is one paused alert whatever the mode", func(t *testing.T) {
		t.Parallel()
		for _, mode := range []string{ModeActive, ModeOff} {
			got := DeriveAlerts(alertsNow, Status{Mode: mode, PausedReason: PausedRestore}, Served{})
			want := []Alert{{Kind: AlertPaused, Reason: PausedRestore}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("mode %q: got %+v, want %+v", mode, got, want)
			}
		}
	})

	until := alertsNow.Add(45 * time.Minute)
	for _, reason := range []string{BlockToldToWait, BlockFailureGuard} {
		t.Run(reason+" on an active install is one paused alert with the time", func(t *testing.T) {
			t.Parallel()
			got := DeriveAlerts(alertsNow, Status{Mode: ModeActive, Certificate: cert, Block: &BlockView{Until: until, Reason: reason}}, Served{})
			want := []Alert{{Kind: AlertPaused, Reason: reason, Until: until}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
		t.Run(reason+" with the mode off and no certificate is listed too, and is no renewal_failed", func(t *testing.T) {
			t.Parallel()
			// Three failed manual Issues: the failures are on the page, the pause is in the bell.
			got := DeriveAlerts(alertsNow, Status{
				Mode: ModeOff, LastError: failing, Failures: 3, LastRun: adminIssue, Block: &BlockView{Until: until, Reason: reason},
			}, Served{})
			want := []Alert{{Kind: AlertPaused, Reason: reason, Until: until}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}

	t.Run("the certificate-files pause gives no alert", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{Mode: ModeActive, CertFiles: true, PausedReason: PausedCertFiles}, Served{})
		if len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})

	t.Run("certificate files with a block set give no alert", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{Mode: ModeActive, CertFiles: true, Block: &BlockView{Until: until, Reason: BlockToldToWait}}, Served{})
		if len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})

	t.Run("fallback, renewal failed and paused come out in that order", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{
			Mode: ModeActive, Certificate: cert, Fallback: true, ExpiredAt: alertsNow, LastError: failing, Failures: 1,
			Block: &BlockView{Until: until, Reason: BlockFailureGuard},
		}, Served{})
		want := []AlertKind{AlertFallback, AlertRenewalFailed, AlertPaused}
		if !slices.Equal(kindsOf(got), want) {
			t.Fatalf("kinds = %v, want %v", kindsOf(got), want)
		}
	})

	t.Run("the alert has nowhere to hold the CA's problem text", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{Mode: ModeActive, Certificate: cert, LastError: failing, Failures: 1}, Served{})
		if len(got) != 1 {
			t.Fatalf("got %+v, want one", got)
		}
		if slices.Contains(kindsOf(got), AlertKind("ca-private-detail-1717")) || containsDetail(got[0], failing.Detail) {
			t.Fatalf("the alert carries the CA's detail: %+v", got[0])
		}
	})
}

// containsDetail reports whether any string field of a holds s.
func containsDetail(a Alert, s string) bool {
	v := reflect.ValueOf(a)
	for i := range v.NumField() {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			if f.String() == s {
				return true
			}
		case reflect.Slice:
			if r, ok := f.Interface().([]string); ok && slices.Contains(r, s) {
				return true
			}
		}
	}
	return false
}
