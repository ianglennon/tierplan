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

	// served is a certificate with the given lifetime that ends left from alertsNow.
	served := func(source string, lifetime, left time.Duration) Served {
		end := alertsNow.Add(left)
		return Served{Source: source, NotBefore: end.Add(-lifetime), NotAfter: end}
	}
	const lifetime90 = 90 * 24 * time.Hour

	t.Run("a 90-day certificate is expiring only when strictly under 14 days are left", func(t *testing.T) {
		t.Parallel()
		if got := DeriveAlerts(alertsNow, Status{Mode: ModeActive}, served("lets_encrypt", lifetime90, ExpiringWarnWithin)); len(got) != 0 {
			t.Fatalf("with exactly 14 days left: %+v, want none", got)
		}
		sv := served("lets_encrypt", lifetime90, ExpiringWarnWithin-time.Second)
		got := DeriveAlerts(alertsNow, Status{Mode: ModeActive}, sv)
		want := []Alert{{Kind: AlertExpiring, Source: "lets_encrypt", NotAfter: sv.NotAfter}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("with 14 days less a second left: %+v, want %+v", got, want)
		}
	})

	t.Run("a 160-hour certificate is expiring under a third of its lifetime", func(t *testing.T) {
		t.Parallel()
		const life = 160 * time.Hour
		third := life / 3
		if got := DeriveAlerts(alertsNow, Status{Mode: ModeActive}, served("lets_encrypt", life, third)); len(got) != 0 {
			t.Fatalf("with exactly a third left: %+v, want none", got)
		}
		got := DeriveAlerts(alertsNow, Status{Mode: ModeActive}, served("lets_encrypt", life, third-time.Second))
		if len(got) != 1 || got[0].Kind != AlertExpiring {
			t.Fatalf("with a third less a second left: %+v, want one expiring", got)
		}
		// Half way through, where such a certificate is renewed, is far from the threshold.
		if got := DeriveAlerts(alertsNow, Status{Mode: ModeActive}, served("lets_encrypt", life, life/2)); len(got) != 0 {
			t.Fatalf("at the renewal point: %+v, want none", got)
		}
	})

	t.Run("the 10-year self-signed certificate never raises it", func(t *testing.T) {
		t.Parallel()
		tenYears := 10 * 365 * 24 * time.Hour
		sv := Served{Source: "self_signed", NotBefore: alertsNow.Add(-24 * time.Hour), NotAfter: alertsNow.Add(-24*time.Hour + tenYears)}
		if got := DeriveAlerts(alertsNow, Status{Mode: ModeOff}, sv); len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})

	t.Run("certificate files that have already ended are expiring", func(t *testing.T) {
		t.Parallel()
		sv := served("cert_files", lifetime90, -48*time.Hour)
		got := DeriveAlerts(alertsNow, Status{Mode: ModeActive, CertFiles: true}, sv)
		want := []Alert{{Kind: AlertExpiring, Source: "cert_files", NotAfter: sv.NotAfter}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("no served certificate gives no expiring alert", func(t *testing.T) {
		t.Parallel()
		if got := DeriveAlerts(alertsNow, Status{Mode: ModeActive}, Served{}); len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})

	t.Run("the fallback hides the expiring item for the expired Let's Encrypt leaf", func(t *testing.T) {
		t.Parallel()
		expired := alertsNow.Add(-time.Hour)
		got := DeriveAlerts(alertsNow, Status{Mode: ModeActive, Fallback: true, ExpiredAt: expired}, served("lets_encrypt", lifetime90, -time.Hour))
		if !slices.Equal(kindsOf(got), []AlertKind{AlertFallback}) {
			t.Fatalf("kinds = %v, want only the fallback", kindsOf(got))
		}
	})

	t.Run("a failed renewal and an expiring certificate are both listed, the failure first", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{
			Mode: ModeActive, Certificate: cert, LastError: failing, Failures: 1,
		}, served("lets_encrypt", lifetime90, 5*24*time.Hour))
		want := []AlertKind{AlertRenewalFailed, AlertExpiring}
		if !slices.Equal(kindsOf(got), want) {
			t.Fatalf("kinds = %v, want %v", kindsOf(got), want)
		}
	})

	left := func(name, class string, paused bool) LeftOverView {
		return LeftOverView{Name: name, RecordID: "provider-id-" + name, Class: class, RetryPaused: paused}
	}

	t.Run("left-over records give one txt_left alert naming the oldest failed one and counting the rest", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{DNS: DNSView{LeftOver: []LeftOverView{
			left("_acme-challenge.a.example.com", "unavailable", false),
			left("_acme-challenge.b.example.com", "token_invalid", false),
			left("_acme-challenge.c.example.com", "", false),
		}}}, Served{})
		want := []Alert{{Kind: AlertTXTLeft, Record: "_acme-challenge.a.example.com", More: 1}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("a record of a run still in flight gives no txt_left alert", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{DNS: DNSView{LeftOver: []LeftOverView{
			left("_acme-challenge.a.example.com", "", false),
			left("_acme-challenge.b.example.com", "", false),
		}}}, Served{})
		if len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})

	t.Run("an in-flight record older than the failed one is not the one named", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{DNS: DNSView{LeftOver: []LeftOverView{
			left("_acme-challenge.running.example.com", "", false),
			left("_acme-challenge.stuck.example.com", "unavailable", false),
		}}}, Served{})
		if len(got) != 1 || got[0].Record != "_acme-challenge.stuck.example.com" || got[0].More != 0 {
			t.Fatalf("got %+v, want the failed record and nothing more", got)
		}
	})

	t.Run("a retry-paused record carries retryPaused", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{DNS: DNSView{LeftOver: []LeftOverView{
			left("_acme-challenge.a.example.com", "unavailable", true),
		}}}, Served{})
		want := []Alert{{Kind: AlertTXTLeft, Record: "_acme-challenge.a.example.com", RetryPaused: true}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("txt_left holds neither the provider's record ID nor any text of the record but its name", func(t *testing.T) {
		t.Parallel()
		got := DeriveAlerts(alertsNow, Status{DNS: DNSView{LeftOver: []LeftOverView{
			left("_acme-challenge.a.example.com", "unavailable", false),
		}}}, Served{})
		if len(got) != 1 || containsDetail(got[0], "provider-id-_acme-challenge.a.example.com") {
			t.Fatalf("got %+v, want one alert without the record ID", got)
		}
	})

	t.Run("the causes of the five kinds derived so far come out in the bell's order", func(t *testing.T) {
		t.Parallel()
		// Everything but the clock, in a state where each rule holds at once: the fallback hides the
		// expiring item, so the expiring cause is a separate row below.
		all := Status{
			Mode: ModeActive, Certificate: cert, Fallback: true, ExpiredAt: alertsNow, LastError: failing, Failures: 1,
			Block: &BlockView{Until: until, Reason: BlockFailureGuard},
			DNS:   DNSView{LeftOver: []LeftOverView{left("_acme-challenge.a.example.com", "unavailable", false)}},
		}
		got := DeriveAlerts(alertsNow, all, Served{})
		want := []AlertKind{AlertFallback, AlertRenewalFailed, AlertPaused, AlertTXTLeft}
		if !slices.Equal(kindsOf(got), want) {
			t.Fatalf("with the fallback: kinds = %v, want %v", kindsOf(got), want)
		}
		all.Fallback, all.ExpiredAt = false, time.Time{}
		got = DeriveAlerts(alertsNow, all, served("lets_encrypt", lifetime90, 3*24*time.Hour))
		want = []AlertKind{AlertRenewalFailed, AlertPaused, AlertExpiring, AlertTXTLeft}
		if !slices.Equal(kindsOf(got), want) {
			t.Fatalf("with an expiring certificate: kinds = %v, want %v", kindsOf(got), want)
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
