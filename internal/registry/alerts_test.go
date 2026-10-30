// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package registry_test

import (
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/registry"
)

// D-07: the bell warns about a credential that is about to expire, has expired or was refused by
// its registry. An expiry day counts as the end of that day in UTC (UI-SPEC default 17), and the
// seven-day warning and Expired use that same instant.

// endOf is the instant the store keeps for an expiry day.
func endOf(t *testing.T, day string) *time.Time {
	t.Helper()
	end, ok := registry.ExpiryEnd(day)
	if !ok {
		t.Fatalf("bad day %q", day)
	}
	return &end
}

func cred(id string) registry.Credential {
	return registry.Credential{ID: id, Name: "name-" + id, Host: "ghcr.io", Prefix: "acme"}
}

func TestAlertsSaysACredentialExpiringInThreeDaysExpiresInThree(t *testing.T) {
	c := cred("a")
	c.ExpiresAt = endOf(t, "2026-10-14") // now is the 12th, 15:30 UTC
	got := registry.Alerts(now, []registry.Credential{c})
	if len(got) != 1 {
		t.Fatalf("alerts = %+v, want one", got)
	}
	a := got[0]
	if a.Kind != registry.AlertExpiring || a.Days != 3 || a.CredentialID != "a" || a.Name != "name-a" ||
		a.Host != "ghcr.io" || a.Prefix != "acme" || a.Layer != registry.LayerInstance || !a.At.Equal(*c.ExpiresAt) {
		t.Errorf("alert = %+v, want an expiring alert with 3 days for the credential", a)
	}
}

func TestAlertsSaysAnExpiredCredentialHasExpired(t *testing.T) {
	c := cred("a")
	c.ExpiresAt = endOf(t, "2026-10-11")
	got := registry.Alerts(now, []registry.Credential{c})
	if len(got) != 1 || got[0].Kind != registry.AlertExpired {
		t.Errorf("alerts = %+v, want one expired alert", got)
	}
}

func TestAlertsCountsAnExpiryDayAsTheEndOfThatDayUTC(t *testing.T) {
	c := cred("a")
	c.ExpiresAt = endOf(t, "2026-10-12") // today: still valid until 23:59:59 UTC

	for _, tc := range []struct {
		name string
		at   time.Time
		want registry.AlertKind
		days int
	}{
		{"the morning of the day", time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), registry.AlertExpiring, 1},
		{"the last second of the day", time.Date(2026, 10, 12, 23, 59, 59, 0, time.UTC), registry.AlertExpiring, 1},
		{"the first second after", time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), registry.AlertExpired, 0},
	} {
		got := registry.Alerts(tc.at, []registry.Credential{c})
		if len(got) != 1 || got[0].Kind != tc.want || got[0].Days != tc.days {
			t.Errorf("%s: alerts = %+v, want one %s with %d days", tc.name, got, tc.want, tc.days)
		}
	}
}

func TestAlertsWarnsFromSevenDaysBeforeThatSameInstant(t *testing.T) {
	c := cred("a")
	c.ExpiresAt = endOf(t, "2026-10-19")
	end := *c.ExpiresAt
	at := end.Add(-7 * 24 * time.Hour)
	if got := registry.Alerts(at, []registry.Credential{c}); len(got) != 1 || got[0].Kind != registry.AlertExpiring || got[0].Days != 7 {
		t.Errorf("exactly 7 days before: %+v, want expiring with 7 days", got)
	}
	if got := registry.Alerts(at.Add(-time.Second), []registry.Credential{c}); len(got) != 0 {
		t.Errorf("a second earlier: %+v, want nothing", got)
	}
}

func TestAlertsSaysAFailureWithNoLaterSuccessWasRefused(t *testing.T) {
	failedAt := now.Add(-time.Hour)
	c := cred("a")
	c.LastFailure = &registry.LastFailure{At: failedAt, Code: "registry_unauthorized"}
	got := registry.Alerts(now, []registry.Credential{c})
	if len(got) != 1 || got[0].Kind != registry.AlertAuthFailed || !got[0].At.Equal(failedAt) {
		t.Fatalf("alerts = %+v, want one auth_failed alert at the failure", got)
	}

	// A test that passed after the failure clears it, even if the stored failure is still there.
	c.LastTest = &registry.LastTest{At: now.Add(-time.Minute), OK: true}
	if got := registry.Alerts(now, []registry.Credential{c}); len(got) != 0 {
		t.Errorf("after a passing test: %+v, want nothing", got)
	}
	// A passing test from before the failure does not.
	c.LastTest = &registry.LastTest{At: failedAt.Add(-time.Minute), OK: true}
	if got := registry.Alerts(now, []registry.Credential{c}); len(got) != 1 {
		t.Errorf("a passing test from before the failure: %+v, want the alert to stay", got)
	}
}

func TestAlertsKeepsOnlyTheMostSevereForOneCredential(t *testing.T) {
	c := cred("a")
	c.ExpiresAt = endOf(t, "2026-10-01")
	c.LastFailure = &registry.LastFailure{At: now.Add(-time.Hour), Code: "registry_unauthorized"}
	got := registry.Alerts(now, []registry.Credential{c})
	if len(got) != 1 || got[0].Kind != registry.AlertExpired {
		t.Errorf("expired and refused: %+v, want only expired", got)
	}

	c.ExpiresAt = endOf(t, "2026-10-14")
	got = registry.Alerts(now, []registry.Credential{c})
	if len(got) != 1 || got[0].Kind != registry.AlertAuthFailed {
		t.Errorf("expiring and refused: %+v, want only auth_failed", got)
	}
}

func TestAlertsSaysNothingOfACredentialThatIsFine(t *testing.T) {
	far := cred("far")
	far.ExpiresAt = endOf(t, "2027-10-12")
	if got := registry.Alerts(now, []registry.Credential{cred("none"), far}); len(got) != 0 {
		t.Errorf("alerts = %+v, want none", got)
	}
}

func TestAlertsCarryTheScopeOfAnEnvironmentCredentialAndComeMostSevereFirst(t *testing.T) {
	soon := cred("soon")
	soon.ExpiresAt = endOf(t, "2026-10-14")
	gone := cred("gone")
	gone.EnvID = "env-a"
	gone.ExpiresAt = endOf(t, "2026-10-01")
	refused := cred("refused")
	refused.LastFailure = &registry.LastFailure{At: now.Add(-time.Hour), Code: "registry_unauthorized"}

	got := registry.Alerts(now, []registry.Credential{soon, refused, gone})
	var order []registry.AlertKind
	for _, a := range got {
		order = append(order, a.Kind)
	}
	want := []registry.AlertKind{registry.AlertExpired, registry.AlertAuthFailed, registry.AlertExpiring}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if got[0].Layer != registry.LayerEnvironment || got[0].EnvID != "env-a" {
		t.Errorf("expired alert = %+v, want the environment layer with env-a", got[0])
	}
}
