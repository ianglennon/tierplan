// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package registry_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/vault"
)

// Which credential a pull is sent with, and the header that carries it (plan 05-09, IMG-02,
// D-14, D-25).

type authJSON struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ServerAddress string `json:"serveraddress"`
}

func decodeAuth(t *testing.T, header string) authJSON {
	t.Helper()
	raw, err := base64.URLEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("the header is not base64url: %v", err)
	}
	var a authJSON
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("the header is not JSON: %v", err)
	}
	return a
}

func TestEncodeAuthIsTheBase64URLJSONTheEngineReads(t *testing.T) {
	got := decodeAuth(t, registry.EncodeAuth("bot", "s3cret", "ghcr.io"))
	if got != (authJSON{"bot", "s3cret", "ghcr.io"}) {
		t.Errorf("decoded = %+v", got)
	}
	hub := decodeAuth(t, registry.EncodeAuth("me", "tok", "docker.io"))
	if hub.ServerAddress != "https://index.docker.io/v1/" {
		t.Errorf("a docker.io credential names %q, want the address the Docker CLI uses", hub.ServerAddress)
	}
	// A password with characters that standard base64 would write as + or / still decodes.
	odd := decodeAuth(t, registry.EncodeAuth("u", "??>>~~", "r.example:5000"))
	if odd.Password != "??>>~~" {
		t.Errorf("password = %q", odd.Password)
	}
}

func TestAuthForSendsNothingWhenNoCredentialMatches(t *testing.T) {
	s, _, _ := newStore(t)
	create(t, s, nil) // ghcr.io/acme
	ref, _ := registry.ParseRef("localhost:5000/private/busybox:1")
	header, used, err := registry.NewResolver(s).AuthFor(context.Background(), "env-1", ref)
	if header != "" || used != nil || err != nil {
		t.Errorf("AuthFor = %q, %v, %v; want nothing", header, used, err)
	}
}

func TestAuthForOpensTheMatchingSecretIntoTheHeader(t *testing.T) {
	s, _, _ := newStore(t)
	c := create(t, s, nil)
	ref, _ := registry.ParseRef("ghcr.io/acme/app:1")
	header, used, err := registry.NewResolver(s).AuthFor(context.Background(), "env-1", ref)
	if err != nil || used == nil || used.ID != c.ID || used.Name != "ghcr-acme" {
		t.Fatalf("AuthFor = %q, %+v, %v", header, used, err)
	}
	if got := decodeAuth(t, header); got != (authJSON{"bot", longSecret, "ghcr.io"}) {
		t.Errorf("decoded = %+v", got)
	}
}

func TestAuthForNamesTheCredentialAndNeverTheValueWhenTheVaultCannotOpenIt(t *testing.T) {
	s, st, _ := newStore(t)
	c := create(t, s, nil)
	// A secret sealed for another credential does not open under this one's ID.
	other, _ := vault.New(make([]byte, 32), "other")
	bad, err := other.Seal(vault.ContextRegistryCred+c.ID, []byte(longSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write.Exec(`UPDATE registry_credentials SET secret_sealed = ? WHERE id = ?`, bad, c.ID); err != nil {
		t.Fatal(err)
	}
	ref, _ := registry.ParseRef("ghcr.io/acme/app:1")
	header, used, err := registry.NewResolver(s).AuthFor(context.Background(), "env-1", ref)
	if err == nil || header != "" {
		t.Fatalf("AuthFor = %q, %v, %v; want an error", header, used, err)
	}
	if !strings.Contains(err.Error(), c.ID) || strings.Contains(err.Error(), longSecret) || strings.Contains(err.Error(), bad) {
		t.Errorf("error = %q: must name the ID and never a value", err)
	}
}

func TestAFailureIsRecordedWithItsCodeAndASuccessClearsIt(t *testing.T) {
	s, _, _ := newStore(t)
	c := create(t, s, nil)
	ctx := context.Background()
	if err := s.RecordFailure(ctx, c.ID, "pull_unauthorized"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, c.ID)
	if err != nil || got.LastFailure == nil || got.LastFailure.Code != "pull_unauthorized" || !got.LastFailure.At.Equal(now) {
		t.Fatalf("credential = %+v, %v", got, err)
	}
	if err := s.ClearFailure(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, c.ID)
	if got.LastFailure != nil {
		t.Errorf("failure = %+v, want cleared", got.LastFailure)
	}
	// Clearing when there is none, or for a credential deleted meanwhile, is not an error.
	if err := s.ClearFailure(ctx, c.ID); err != nil {
		t.Errorf("a second clear: %v", err)
	}
	if err := s.RecordFailure(ctx, "gone", "pull_denied"); err != nil {
		t.Errorf("a failure for a deleted credential: %v", err)
	}
}

// Which credential a tag listing is made with (plan 05-18, IMG-03, D-24a, T-05-60).

func TestForBrowseUsesTheInstanceLayerAloneWhenNoEnvironmentIsChosen(t *testing.T) {
	s, st, _ := newStore(t)
	addEnvironment(t, st, "env-a", "Alpha")
	inst := create(t, s, func(in *registry.CreateInput) { in.Name = "inst"; in.Prefix = "acme" })
	create(t, s, func(in *registry.CreateInput) { in.Name = "in-a"; in.Prefix = "acme/team"; in.EnvID = "env-a" })
	ref, _ := registry.ParseRef("ghcr.io/acme/team/app")

	got, err := registry.NewResolver(s).ForBrowse(context.Background(), "", func(registry.Candidate) bool { return true }, ref)
	if err != nil || got == nil {
		t.Fatalf("ForBrowse = %+v, %v", got, err)
	}
	if got.ID != inst.ID || got.Layer != registry.LayerInstance {
		t.Errorf("credential = %+v, want the instance one: another layer's is not a candidate without its environment", got.Candidate)
	}
	if got.HostOptions.Host != "ghcr.io" || got.Username != "bot" || got.Password != longSecret {
		t.Errorf("sign-in = %+v, want the opened secret for the registry", got.HostOptions)
	}
}

func TestForBrowseAddsTheChosenEnvironmentsLayerAndTheMoreSpecificPrefixWins(t *testing.T) {
	s, st, _ := newStore(t)
	addEnvironment(t, st, "env-a", "Alpha")
	addEnvironment(t, st, "env-b", "Beta")
	create(t, s, func(in *registry.CreateInput) { in.Name = "inst"; in.Prefix = "acme" })
	inA := create(t, s, func(in *registry.CreateInput) { in.Name = "in-a"; in.Prefix = "acme/team"; in.EnvID = "env-a" })
	create(t, s, func(in *registry.CreateInput) { in.Name = "in-b"; in.Prefix = "acme/team/app"; in.EnvID = "env-b" })
	ref, _ := registry.ParseRef("ghcr.io/acme/team/app")

	got, err := registry.NewResolver(s).ForBrowse(context.Background(), "env-a", func(registry.Candidate) bool { return true }, ref)
	if err != nil || got == nil || got.ID != inA.ID {
		t.Fatalf("ForBrowse = %+v, %v, want environment A's credential, never environment B's", got, err)
	}
}

func TestForBrowseNeverPicksACredentialTheCallerCannotSee(t *testing.T) {
	s, st, _ := newStore(t)
	addEnvironment(t, st, "env-a", "Alpha")
	inst := create(t, s, func(in *registry.CreateInput) { in.Name = "inst"; in.Prefix = "acme" })
	create(t, s, func(in *registry.CreateInput) { in.Name = "in-a"; in.Prefix = "acme/team"; in.EnvID = "env-a" })
	ref, _ := registry.ParseRef("ghcr.io/acme/team/app")
	onlyInstance := func(c registry.Candidate) bool { return c.Layer == registry.LayerInstance }

	got, err := registry.NewResolver(s).ForBrowse(context.Background(), "env-a", onlyInstance, ref)
	if err != nil || got == nil || got.ID != inst.ID {
		t.Fatalf("ForBrowse = %+v, %v, want the instance credential: the environment's is hidden from this caller", got, err)
	}
	none, err := registry.NewResolver(s).ForBrowse(context.Background(), "env-a", func(registry.Candidate) bool { return false }, ref)
	if err != nil || none != nil {
		t.Errorf("ForBrowse = %+v, %v, want anonymous (nil) when nothing is visible", none, err)
	}
}

func TestForBrowseIsAnonymousWhenNothingMatches(t *testing.T) {
	s, _, _ := newStore(t)
	create(t, s, nil) // ghcr.io/acme
	ref, _ := registry.ParseRef("ghcr.io/other/app")
	got, err := registry.NewResolver(s).ForBrowse(context.Background(), "", nil, ref)
	if got != nil || err != nil {
		t.Errorf("ForBrowse = %+v, %v, want nil, nil", got, err)
	}
}

func TestAResolvedCredentialNeverPrintsItsSecret(t *testing.T) {
	c := registry.ResolvedCredential{
		Candidate:   registry.Candidate{ID: "c1", Name: "n"},
		HostOptions: registry.HostOptions{Host: "ghcr.io", Username: "bot", Password: longSecret},
	}
	for name, got := range map[string]string{
		"%v": fmt.Sprintf("%v", c), "%+v": fmt.Sprintf("%+v", &c), "%#v": fmt.Sprintf("%#v", c),
	} {
		if strings.Contains(got, longSecret) || !strings.Contains(got, "c1") {
			t.Errorf("%s = %q, want the ID and never the secret", name, got)
		}
	}
}

// Which credential a check is keyed by (plan 12-02, D-19, T-12-03).

func TestCredentialForMatchesForBrowse(t *testing.T) {
	s, st, _ := newStore(t)
	addEnvironment(t, st, "env-a", "Alpha")
	inst := create(t, s, func(in *registry.CreateInput) { in.Name = "inst"; in.Prefix = "acme" })
	inA := create(t, s, func(in *registry.CreateInput) { in.Name = "in-a"; in.Prefix = "acme/team"; in.EnvID = "env-a" })
	r := registry.NewResolver(s)
	ctx := context.Background()

	for _, c := range []struct {
		name, env, ref, want string
	}{
		{"an instance credential", "", "ghcr.io/acme/other/app:1", inst.ID},
		{"an instance credential seen from an environment", "env-a", "ghcr.io/acme/other/app:1", inst.ID},
		{"an environment credential, the more specific prefix", "env-a", "ghcr.io/acme/team/app:1", inA.ID},
		{"an environment's credential is not another's", "", "ghcr.io/acme/team/app:1", inst.ID},
		{"no match is anonymous", "env-a", "ghcr.io/other/app:1", ""},
	} {
		ref, err := registry.ParseRef(c.ref)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.CredentialFor(ctx, c.env, ref)
		if err != nil {
			t.Fatalf("%s: CredentialFor: %v", c.name, err)
		}
		browse, err := r.ForBrowse(ctx, c.env, nil, ref)
		if err != nil {
			t.Fatalf("%s: ForBrowse: %v", c.name, err)
		}
		bid := ""
		if browse != nil {
			bid = browse.ID
		}
		if got != c.want || got != bid {
			t.Errorf("%s: CredentialFor = %q, ForBrowse = %q, want %q", c.name, got, bid, c.want)
		}
	}
}

func TestCredentialForNeverOpensSecret(t *testing.T) {
	s, st, _ := newStore(t)
	c := create(t, s, nil)
	// A secret sealed for another credential does not open under this one's ID.
	other, _ := vault.New(make([]byte, 32), "other")
	bad, err := other.Seal(vault.ContextRegistryCred+c.ID, []byte(longSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write.Exec(`UPDATE registry_credentials SET secret_sealed = ? WHERE id = ?`, bad, c.ID); err != nil {
		t.Fatal(err)
	}
	ref, _ := registry.ParseRef("ghcr.io/acme/app:1")
	r := registry.NewResolver(s)
	if _, err := r.ForBrowse(context.Background(), "env-1", nil, ref); err == nil {
		t.Fatal("ForBrowse opened a secret that cannot be opened: the test would prove nothing")
	}
	got, err := r.CredentialFor(context.Background(), "env-1", ref)
	if err != nil || got != c.ID {
		t.Errorf("CredentialFor = %q, %v, want %q: it names the credential without opening its secret", got, err, c.ID)
	}
}
