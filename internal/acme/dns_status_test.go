// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
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

// lookupRecorder is a host lookup that answers addrs and remembers what it was asked.
type lookupRecorder struct {
	addrs []netip.Addr
	err   error

	mu        sync.Mutex
	hosts     []string
	deadlines []time.Duration
}

func (l *lookupRecorder) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hosts = append(l.hosts, host)
	if dl, ok := ctx.Deadline(); ok {
		l.deadlines = append(l.deadlines, time.Until(dl))
	} else {
		l.deadlines = append(l.deadlines, -1)
	}
	return l.addrs, l.err
}

func (l *lookupRecorder) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hosts)
}

func addrs(t *testing.T, texts ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, len(texts))
	for i, s := range texts {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", s, err)
		}
		out[i] = a
	}
	return out
}

// suggestEnv is an HTTP-01 environment whose manager looks hosts up with rec.
func suggestEnv(t *testing.T, rec *lookupRecorder) *dnsEnv {
	t.Helper()
	e := newDNSEnv(t, ChallengeHTTP01)
	e.lookupIP = rec.lookup
	e.mgr = e.build(false)
	return e
}

func TestSuggestDNS01ForAPrivateAddress(t *testing.T) {
	rec := &lookupRecorder{addrs: addrs(t, "192.168.1.10")}
	e := suggestEnv(t, rec)

	got := e.mgr.suggestDNS01(context.Background(), e.state(), testHost, false)

	if got == nil || got.Host != testHost || got.Address != "192.168.1.10" {
		t.Errorf("suggestion = %+v, want %s at 192.168.1.10", got, testHost)
	}
}

func TestSuggestDNS01WhichAddressesSuggest(t *testing.T) {
	for _, tc := range []struct {
		addr    string
		suggest bool
	}{
		{"10.1.2.3", true}, {"172.16.0.5", true}, {"192.168.1.10", true}, {"127.0.0.1", true},
		{"fe80::1", true}, {"fd00::1", true}, {"100.64.0.1", true}, {"100.127.255.254", true},
		{"::ffff:192.168.1.10", true},
		{"1.1.1.1", false}, {"2606:4700::1", false}, {"100.63.255.255", false}, {"100.128.0.1", false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			rec := &lookupRecorder{addrs: addrs(t, tc.addr)}
			e := suggestEnv(t, rec)

			got := e.mgr.suggestDNS01(context.Background(), e.state(), testHost, false)

			if (got != nil) != tc.suggest {
				t.Errorf("suggestion for %s = %+v, want a suggestion: %v", tc.addr, got, tc.suggest)
			}
		})
	}

	t.Run("the first private address is the one named", func(t *testing.T) {
		rec := &lookupRecorder{addrs: addrs(t, "1.1.1.1", "10.0.0.7", "192.168.1.10")}
		e := suggestEnv(t, rec)

		got := e.mgr.suggestDNS01(context.Background(), e.state(), testHost, false)

		if got == nil || got.Address != "10.0.0.7" {
			t.Errorf("suggestion = %+v, want the first private address 10.0.0.7", got)
		}
	})
}

func TestSuggestDNS01OnlyForAnHTTP01NameOnTheRealDirectory(t *testing.T) {
	private := addrs(t, "192.168.1.10")
	for _, tc := range []struct {
		name       string
		challenge  string
		host       string
		overridden bool
		err        error
	}{
		{"DNS-01 saved", ChallengeDNS01, testHost, false, nil},
		{"an IPv4 literal host", ChallengeHTTP01, "192.168.1.10", false, nil},
		{"an IPv6 literal host", ChallengeHTTP01, "fd00::1", false, nil},
		{"no host", ChallengeHTTP01, "", false, nil},
		{"an overridden directory", ChallengeHTTP01, testHost, true, nil},
		{"a lookup error", ChallengeHTTP01, testHost, false, errors.New("no such host")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &lookupRecorder{addrs: private, err: tc.err}
			e := suggestEnv(t, rec)
			st := e.state()
			st.ChallengeType = tc.challenge

			got := e.mgr.suggestDNS01(context.Background(), st, tc.host, tc.overridden)

			if got != nil {
				t.Errorf("suggestion = %+v, want none", got)
			}
		})
	}
}

func TestSuggestDNS01IsCachedForFiveMinutesOnTheClock(t *testing.T) {
	ctx := context.Background()
	rec := &lookupRecorder{addrs: addrs(t, "192.168.1.10")}
	e := suggestEnv(t, rec)
	st := e.state()

	e.mgr.suggestDNS01(ctx, st, testHost, false)
	e.mgr.suggestDNS01(ctx, st, testHost, false)
	if n := rec.calls(); n != 1 {
		t.Fatalf("two reads made %d lookups, want 1", n)
	}

	e.clock.Advance(5*time.Minute - time.Second)
	got := e.mgr.suggestDNS01(ctx, st, testHost, false)
	if n := rec.calls(); n != 1 || got == nil {
		t.Fatalf("a read just inside five minutes made %d lookups and answered %+v, want the cached answer", n, got)
	}

	e.clock.Advance(time.Second)
	e.mgr.suggestDNS01(ctx, st, testHost, false)
	if n := rec.calls(); n != 2 {
		t.Errorf("a read after five minutes made %d lookups in all, want 2", n)
	}

	e.mgr.suggestDNS01(ctx, st, "other.example.org", false)
	if n := rec.calls(); n != 3 || rec.hosts[2] != "other.example.org" {
		t.Errorf("a changed host made %d lookups in all (%v), want a lookup at once", n, rec.hosts)
	}
}

func TestSuggestDNS01CachesAFailedLookupToo(t *testing.T) {
	ctx := context.Background()
	rec := &lookupRecorder{err: errors.New("no such host")}
	e := suggestEnv(t, rec)

	e.mgr.suggestDNS01(ctx, e.state(), testHost, false)
	e.mgr.suggestDNS01(ctx, e.state(), testHost, false)

	if n := rec.calls(); n != 1 {
		t.Errorf("a failing resolver was asked %d times in a minute, want once", n)
	}
}

func TestSuggestDNS01LookupCarriesATwoSecondDeadline(t *testing.T) {
	rec := &lookupRecorder{addrs: addrs(t, "192.168.1.10")}
	e := suggestEnv(t, rec)

	e.mgr.suggestDNS01(context.Background(), e.state(), testHost, false)

	if len(rec.deadlines) != 1 || rec.deadlines[0] <= 0 || rec.deadlines[0] > 2*time.Second {
		t.Errorf("lookup deadlines = %v, want one deadline within 2 s", rec.deadlines)
	}
}

func TestStatusSuggestsDNS01ForAPrivatelyResolvingHost(t *testing.T) {
	ctx := context.Background()
	rec := &lookupRecorder{addrs: addrs(t, "10.0.0.9")}
	e := suggestEnv(t, rec)

	// The test directory is an override, which never suggests.
	st, err := e.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.SuggestDNS01 != nil || rec.calls() != 0 {
		t.Errorf("an overridden directory gave %+v after %d lookups, want none", st.SuggestDNS01, rec.calls())
	}

	// The same install on the real directory.
	real := NewManager(Deps{
		Store: e.store, Client: NewClient(Directory{URL: LetsEncryptDirectory}, e.store, e.clock, "tierplan-test/0"),
		Challenges: e.tokens, Jobs: e.hub, Installer: e.inst, Clock: e.clock,
		PublicURL: e.urls.getPublic, AgentURL: e.urls.getAgent, BasePath: e.urls.getBase,
		Log: slog.New(slog.DiscardHandler), DNS: DNSDeps{LookupIP: rec.lookup},
	})
	st, err = real.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.SuggestDNS01 == nil || st.SuggestDNS01.Host != testHost || st.SuggestDNS01.Address != "10.0.0.9" {
		t.Errorf("SuggestDNS01 = %+v, want %s at 10.0.0.9", st.SuggestDNS01, testHost)
	}

	if _, err := real.Status(ctx); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if n := rec.calls(); n != 1 {
		t.Errorf("two status reads made %d lookups, want 1", n)
	}
}
