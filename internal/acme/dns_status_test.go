// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/dns"
)

// holdsString reports where in v a string equal to needle (or containing it) is, by walking every
// field, element and pointer; "" when there is none. Unexported fields are read by kind, never by
// Interface, so nothing is skipped.
func holdsString(v reflect.Value, needle, path string) string {
	switch v.Kind() {
	case reflect.String:
		if strings.Contains(v.String(), needle) {
			return path
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return holdsString(v.Elem(), needle, path)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if at := holdsString(v.Index(i), needle, fmt.Sprintf("%s[%d]", path, i)); at != "" {
				return at
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if at := holdsString(v.MapIndex(k), needle, path+"[map]"); at != "" {
				return at
			}
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return ""
		}
		for i := range v.NumField() {
			if at := holdsString(v.Field(i), needle, path+"."+v.Type().Field(i).Name); at != "" {
				return at
			}
		}
	default:
	}
	return ""
}

func TestStatusShowsTheDNSViewWithoutTheToken(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	e.save(ChallengeDNS01, []string{"1.1.1.1:53", "9.9.9.9:53"})
	newer := e.leftOver("_acme-challenge.newer.example.com", "n", testNow.Add(-time.Hour))
	older := e.leftOver("_acme-challenge.older.example.com", "o", testNow.Add(-3*time.Hour))
	// Inserted last, but the oldest: the list is ordered by when the record was made.
	failedAt, nextAt := testNow.Add(-time.Minute), testNow.Add(9*time.Minute)
	if err := e.store.MarkDNSRecordFailed(ctx, older.ID, 2, failedAt, nextAt, dns.ClassTokenScope); err != nil {
		t.Fatal(err)
	}

	st, err := e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	d := st.DNS
	if d.Provider != dnsProviderID || !d.TokenSaved {
		t.Errorf("DNS = provider %q tokenSaved %v, want %q and true", d.Provider, d.TokenSaved, dnsProviderID)
	}
	if want := []string{"1.1.1.1:53", "9.9.9.9:53"}; !slices.Equal(d.Resolvers, want) {
		t.Errorf("resolvers = %v, want %v", d.Resolvers, want)
	}
	if len(d.LeftOver) != 2 {
		t.Fatalf("left-over = %+v, want two rows", d.LeftOver)
	}
	first, second := d.LeftOver[0], d.LeftOver[1]
	if first.Name != older.Name || first.RecordID != older.RecordID || first.Class != dns.ClassTokenScope ||
		!first.LastTryAt.Equal(failedAt) || !first.NextTryAt.Equal(nextAt) || first.RetryPaused || first.PausedReason != "" {
		t.Errorf("first left-over = %+v, want the older record, class dns_token_scope, next try %v", first, nextAt)
	}
	if second.Name != newer.Name || second.Class != "" {
		t.Errorf("second left-over = %+v, want the newer record, no error class", second)
	}

	if at := holdsString(reflect.ValueOf(st), dnsToken, "Status"); at != "" {
		t.Errorf("the token is held at %s", at)
	}
	if shown := fmt.Sprintf("%+v", st); strings.Contains(shown, dnsToken) {
		t.Errorf("%%+v of the status contains the token")
	}
	for _, f := range reflect.VisibleFields(reflect.TypeOf(DNSView{})) {
		if strings.Contains(f.Name, "Token") && f.Type.Kind() != reflect.Bool {
			t.Errorf("DNSView.%s is a %s; a token field may only be a bool", f.Name, f.Type)
		}
	}
}

func TestStatusDNSViewDefaults(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)

	st, err := e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if st.DNS.LeftOver == nil || len(st.DNS.LeftOver) != 0 {
		t.Errorf("left-over = %#v, want an empty, non-nil list", st.DNS.LeftOver)
	}
	if st.DNS.Resolvers == nil || len(st.DNS.Resolvers) != 0 {
		t.Errorf("resolvers = %#v, want an empty, non-nil list", st.DNS.Resolvers)
	}
	if e.built.Load() != 0 || len(e.prov.Calls()) != 0 {
		t.Error("the status built a provider or called the DNS provider")
	}
}

func TestStatusOrdersLeftOversWithTheSameInstantByID(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	a := e.leftOver("_acme-challenge.a.example.com", "a", testNow)
	b := e.leftOver("_acme-challenge.b.example.com", "b", testNow)
	want := []string{a.RecordID, b.RecordID}
	if b.ID < a.ID {
		want = []string{b.RecordID, a.RecordID}
	}

	for range 3 {
		st, err := e.mgr.Status(context.Background())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		var got []string
		for _, l := range st.DNS.LeftOver {
			got = append(got, l.RecordID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("left-over order = %v, want %v on every read", got, want)
		}
	}
}

func TestStatusValidationUntestedIsFalseWhileARunIsActive(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	if err := e.store.SetValidationUntested(ctx, true, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	st, err := e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.ValidationUntested {
		t.Error("ValidationUntested = false, want true for an untested saved setting")
	}

	e.mgr.mu.Lock()
	e.mgr.active = &activeRun{jobID: "job-1", kind: KindIssue, trigger: TriggerAdmin, challenge: ChallengeDNS01, startedAt: e.clock.Now()}
	e.mgr.mu.Unlock()
	st, err = e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ValidationUntested {
		t.Error("ValidationUntested = true while a run is active, want false")
	}
	if st.ActiveRun == nil || st.ActiveRun.Challenge != ChallengeDNS01 {
		t.Errorf("ActiveRun = %+v, want the run with challenge dns-01", st.ActiveRun)
	}
}

func TestStatusCarriesTheDNSFactsOfTheErrorAndTheChallengeOfTheRun(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	f := Failure{
		Class: dns.ClassNotVisible, Zone: "example.com", Target: "validation.example.net",
		Resolvers: []string{"1.1.1.1:53"}, Detail: "the record did not show",
	}
	e.issue()

	got, err := e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.LastRun == nil || got.LastRun.Challenge != ChallengeDNS01 {
		t.Errorf("LastRun = %+v, want the challenge dns-01", got.LastRun)
	}

	// A failure document is what a DNS failure leaves behind.
	st := e.state()
	st.LastErrorClass, st.LastErrorDetail, st.LastErrorAt = f.Class, encodeError(f), e.clock.Now()
	if err := e.store.SaveState(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err = e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	le := got.LastError
	if le == nil || le.Zone != "example.com" || le.Target != "validation.example.net" || !slices.Equal(le.Resolvers, []string{"1.1.1.1:53"}) {
		t.Errorf("LastError = %+v, want the zone, the target and the resolvers", le)
	}
}
