// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

const longSecret = "ghp_0123456789abcdefghijklmnopqrstuvwxyz"

func newStore(t *testing.T) (*registry.Store, *store.Store, *vault.Vault) {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{9}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	return registry.NewStore(st, v, func() time.Time { return now }), st, v
}

func create(t *testing.T, s *registry.Store, mutate func(*registry.CreateInput)) registry.Credential {
	t.Helper()
	in := valid()
	if mutate != nil {
		mutate(&in)
	}
	c, err := s.Create(context.Background(), in, "admin-1", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return c
}

func sealedOf(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	var sealed string
	if err := st.Read.QueryRow(`SELECT secret_sealed FROM registry_credentials WHERE id = ?`, id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestCreateSealsTheSecretUnderTheCredentialsOwnID(t *testing.T) {
	s, st, v := newStore(t)
	c := create(t, s, nil)

	if c.ID == "" || c.Name != "ghcr-acme" || c.Host != "ghcr.io" || c.Prefix != "acme" || c.Username != "bot" || c.Layer() != registry.LayerInstance {
		t.Errorf("credential = %+v", c)
	}
	if c.Last4 != "wxyz" {
		t.Errorf("Last4 = %q, want wxyz", c.Last4)
	}
	if !c.CreatedAt.Equal(now) || !c.UpdatedAt.Equal(now) {
		t.Errorf("created/updated = %v/%v, want %v", c.CreatedAt, c.UpdatedAt, now)
	}

	sealed := sealedOf(t, st, c.ID)
	if strings.Contains(sealed, longSecret) || sealed == "" {
		t.Fatalf("the stored value is not sealed: %q", sealed)
	}
	plain, err := v.Open(vault.ContextRegistryCred+c.ID, sealed)
	if err != nil || string(plain) != longSecret {
		t.Fatalf("Open under its own ID = %q, %v", plain, err)
	}
	if _, err := v.Open(vault.ContextRegistryCred+"another-credential", sealed); !errors.Is(err, vault.ErrDecrypt) {
		t.Errorf("Open under another credential's ID: %v, want ErrDecrypt", err)
	}
}

func TestCreateKeepsNoLastCharactersOfAShortSecret(t *testing.T) {
	s, st, _ := newStore(t)
	short := create(t, s, func(in *registry.CreateInput) { in.Secret = "hunter2hunt" }) // 11 characters
	long := create(t, s, func(in *registry.CreateInput) { in.Name = "second"; in.Prefix = "other"; in.Secret = "hunter2hunt2" })
	if short.Last4 != "" || long.Last4 != "unt2" {
		t.Errorf("Last4 = %q and %q, want an empty one and unt2", short.Last4, long.Last4)
	}
	var stored string
	if err := st.Read.QueryRow(`SELECT secret_last4 FROM registry_credentials WHERE id = ?`, short.ID).Scan(&stored); err != nil || stored != "" {
		t.Errorf("secret_last4 of a short secret = %q, %v", stored, err)
	}
}

func TestGetAndListReturnTheCredentialWithoutAnySecret(t *testing.T) {
	s, _, _ := newStore(t)
	a := create(t, s, nil)
	b := create(t, s, func(in *registry.CreateInput) {
		in.Name, in.Type, in.Prefix, in.HubFallback, in.Expires = "hub", registry.TypeDockerHub, "", true, "2027-01-31"
	})

	got, err := s.Get(context.Background(), a.ID)
	if err != nil || got.ID != a.ID || got.Last4 != "wxyz" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := s.Get(context.Background(), "no-such-id"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("Get of an unknown ID: %v, want ErrNotFound", err)
	}
	list, err := s.List(context.Background())
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	// By host: docker.io before ghcr.io.
	if list[0].ID != b.ID || list[1].ID != a.ID {
		t.Errorf("order = %s, %s", list[0].Name, list[1].Name)
	}
	if !list[0].HubFallback || list[0].ExpiresAt == nil || !list[0].ExpiresAt.Equal(time.Date(2027, 1, 31, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("hub credential = %+v", list[0])
	}
}

func TestCreateRefusesADuplicateInTheSameLayer(t *testing.T) {
	s, _, _ := newStore(t)
	create(t, s, func(in *registry.CreateInput) {
		in.Type, in.Prefix, in.HubFallback = registry.TypeDockerHub, "acme", true
	})

	for _, c := range []struct {
		name   string
		mutate func(*registry.CreateInput)
		want   error
	}{
		{"same name", func(in *registry.CreateInput) { in.Type, in.Prefix = registry.TypeGHCR, "elsewhere" }, registry.ErrNameTaken},
		{"same host and prefix", func(in *registry.CreateInput) { in.Name, in.Type, in.Prefix = "other", registry.TypeDockerHub, "acme" }, registry.ErrHostPrefixTaken},
		{"a second Hub fallback", func(in *registry.CreateInput) {
			in.Name, in.Type, in.Prefix, in.HubFallback = "other", registry.TypeDockerHub, "", true
		}, registry.ErrHubFallbackTaken},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := valid()
			c.mutate(&in)
			if _, err := s.Create(context.Background(), in, "admin-1", nil); !errors.Is(err, c.want) {
				t.Errorf("Create: %v, want %v", err, c.want)
			}
		})
	}
}

func TestCreateRefusesInvalidInputWithEveryFieldAtFault(t *testing.T) {
	s, st, _ := newStore(t)
	_, err := s.Create(context.Background(), registry.CreateInput{Type: registry.TypeGeneric}, "admin-1", nil)
	var ve *registry.ValidationError
	if !errors.As(err, &ve) || len(ve.Fields) != 4 {
		t.Fatalf("Create: %v, want a ValidationError of 4 fields", err)
	}
	var n int
	if err := st.Read.QueryRow(`SELECT count(*) FROM registry_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows = %d, %v; a refused credential stores nothing", n, err)
	}
}

func TestCreateRollsBackWhenTheHookFails(t *testing.T) {
	s, st, _ := newStore(t)
	boom := errors.New("audit write failed")
	_, err := s.Create(context.Background(), valid(), "admin-1", func(context.Context, *sql.Tx, registry.Credential) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Create: %v, want the hook's error", err)
	}
	var n int
	if err := st.Read.QueryRow(`SELECT count(*) FROM registry_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows = %d, %v; a credential must not exist without its audit row", n, err)
	}
}

// updateOf is the unchanged form of the credential create() makes, for a test to change one
// field of.
func updateOf(c registry.Credential) registry.UpdateInput {
	return registry.UpdateInput{Name: c.Name, Type: c.Type, Host: c.Host, Prefix: c.Prefix, Username: c.Username, HubFallback: c.HubFallback}
}

func TestUpdateWithNoSecretKeepsTheSealedSecretByteForByte(t *testing.T) {
	s, st, _ := newStore(t)
	c := create(t, s, nil)
	before := sealedOf(t, st, c.ID)

	in := updateOf(c)
	in.Username = "bot2"
	got, changed, err := s.Update(context.Background(), c.ID, in, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Username != "bot2" || got.Last4 != "wxyz" || !slices.Equal(changed, []string{"username"}) {
		t.Errorf("credential = %+v, changed = %v", got, changed)
	}
	if after := sealedOf(t, st, c.ID); after != before {
		t.Errorf("the stored envelope changed without a new secret: %q -> %q", before, after)
	}
}

func TestUpdateWithANewSecretReSealsItUnderTheSameContextAndMovesLast4(t *testing.T) {
	s, st, v := newStore(t)
	c := create(t, s, nil)
	before := sealedOf(t, st, c.ID)

	in := updateOf(c)
	in.Secret = "  glpat-a-brand-new-token-9876  "
	got, changed, err := s.Update(context.Background(), c.ID, in, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Last4 != "9876" || !slices.Equal(changed, []string{"secret"}) {
		t.Errorf("Last4 = %q, changed = %v", got.Last4, changed)
	}
	after := sealedOf(t, st, c.ID)
	if after == before || strings.Contains(after, "brand-new") {
		t.Fatalf("the stored value was not re-sealed: %q", after)
	}
	plain, err := v.Open(vault.ContextRegistryCred+c.ID, after)
	if err != nil || string(plain) != "glpat-a-brand-new-token-9876" {
		t.Fatalf("Open under its own ID = %q, %v", plain, err)
	}
}

func TestUpdateListsTheChangedFieldsSortedAndMovesUpdatedAt(t *testing.T) {
	s, _, _ := newStore(t)
	c := create(t, s, nil)

	in := updateOf(c)
	in.Name, in.Prefix, in.Secret, in.Expires = "renamed", "acme/team", "another-long-token-0000", "2199-01-02"
	got, changed, err := s.Update(context.Background(), c.ID, in, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if want := []string{"expires", "name", "prefix", "secret"}; !slices.Equal(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if got.Name != "renamed" || got.Prefix != "acme/team" || got.ExpiresAt == nil {
		t.Errorf("credential = %+v", got)
	}
}

func TestUpdateChangesUpdatedAtWhenSomethingChanges(t *testing.T) {
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{9}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	clock := now
	s := registry.NewStore(st, v, func() time.Time { return clock })
	c, err := s.Create(context.Background(), valid(), "admin-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Hour)
	in := updateOf(c)
	in.Username = "bot2"
	got, _, err := s.Update(context.Background(), c.ID, in, nil)
	if err != nil || !got.UpdatedAt.Equal(clock) || !got.CreatedAt.Equal(now) {
		t.Errorf("created/updated = %v/%v, %v; want %v/%v", got.CreatedAt, got.UpdatedAt, err, now, clock)
	}
}

func TestUpdateOfAnUnchangedFormWritesNothingAndRunsNoHook(t *testing.T) {
	s, _, _ := newStore(t)
	c := create(t, s, nil)
	called := false
	got, changed, err := s.Update(context.Background(), c.ID, updateOf(c), func(context.Context, *sql.Tx, registry.Credential, []string) error {
		called = true
		return nil
	})
	if err != nil || len(changed) != 0 || called || !got.UpdatedAt.Equal(c.UpdatedAt) {
		t.Errorf("changed = %v, hook called = %v, %v", changed, called, err)
	}
}

func TestUpdateOfAnUnknownCredentialIsNotFound(t *testing.T) {
	s, _, _ := newStore(t)
	if _, _, err := s.Update(context.Background(), "no-such-id", registry.UpdateInput{Name: "x", Type: registry.TypeGHCR, Username: "u"}, nil); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("Update: %v, want ErrNotFound", err)
	}
}

func TestUpdateRefusesAClashWithAnotherCredentialAndNamesIt(t *testing.T) {
	s, _, _ := newStore(t)
	a := create(t, s, nil)
	b := create(t, s, func(in *registry.CreateInput) { in.Name, in.Prefix = "second", "other" })
	hub := create(t, s, func(in *registry.CreateInput) {
		in.Name, in.Type, in.Prefix, in.HubFallback = "hub-all", registry.TypeDockerHub, "", true
	})
	hub2 := create(t, s, func(in *registry.CreateInput) {
		in.Name, in.Type, in.Prefix = "hub-ns", registry.TypeDockerHub, "ns"
	})

	for _, c := range []struct {
		name   string
		target registry.Credential
		mutate func(*registry.UpdateInput)
		want   error
		with   string
	}{
		{"rename onto another name", b, func(in *registry.UpdateInput) { in.Name = a.Name }, registry.ErrNameTaken, a.Name},
		{"move onto another host and prefix", b, func(in *registry.UpdateInput) { in.Prefix = a.Prefix }, registry.ErrHostPrefixTaken, a.Name},
		{"take the Hub fallback a second time", hub2, func(in *registry.UpdateInput) { in.HubFallback = true }, registry.ErrHubFallbackTaken, hub.Name},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := updateOf(c.target)
			c.mutate(&in)
			_, _, err := s.Update(context.Background(), c.target.ID, in, nil)
			var ce *registry.ConflictError
			if !errors.Is(err, c.want) || !errors.As(err, &ce) || ce.With != c.with {
				t.Errorf("Update: %v, want %v with %q", err, c.want, c.with)
			}
		})
	}
	// Nothing was stored by a refused update.
	got, err := s.Get(context.Background(), b.ID)
	if err != nil || got.Name != "second" || got.Prefix != "other" {
		t.Errorf("after the refusals = %+v, %v", got, err)
	}
}

func TestUpdateRefusesInvalidInputWithEveryFieldAtFaultAndAnEmptySecretIsNotOne(t *testing.T) {
	s, _, _ := newStore(t)
	c := create(t, s, nil)
	in := updateOf(c)
	in.Name, in.Username, in.Prefix = "", "", "Bad Prefix"
	_, _, err := s.Update(context.Background(), c.ID, in, nil)
	var ve *registry.ValidationError
	if !errors.As(err, &ve) || len(ve.Fields) != 3 {
		t.Fatalf("Update: %v, want a ValidationError of 3 fields (name, prefix, username; no secret)", err)
	}
	for _, f := range ve.Fields {
		if f.Field == "secret" {
			t.Errorf("an empty secret was refused: %+v", f)
		}
	}
}

func TestUpdateKeepsTheSameExpiryOfAnExpiredCredentialValid(t *testing.T) {
	st := storetest.New(t)
	v, _ := vault.New(bytes.Repeat([]byte{9}, 32), "test")
	clock := now
	s := registry.NewStore(st, v, func() time.Time { return clock })
	in := valid()
	in.Expires = "2026-10-20"
	c, err := s.Create(context.Background(), in, "admin-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	clock = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) // the day has passed
	up := updateOf(c)
	up.Expires = "2026-10-20"
	up.Username = "bot2"
	if _, _, err := s.Update(context.Background(), c.ID, up, nil); err != nil {
		t.Errorf("Update of an expired credential that keeps its day: %v", err)
	}
	up.Expires = "2026-10-25" // a new, past day is refused
	var ve *registry.ValidationError
	if _, _, err := s.Update(context.Background(), c.ID, up, nil); !errors.As(err, &ve) {
		t.Errorf("Update to another past day: %v, want a ValidationError", err)
	}
}

func TestUpdateRollsBackWhenTheHookFails(t *testing.T) {
	s, _, _ := newStore(t)
	c := create(t, s, nil)
	boom := errors.New("audit write failed")
	in := updateOf(c)
	in.Username = "bot2"
	if _, _, err := s.Update(context.Background(), c.ID, in, func(context.Context, *sql.Tx, registry.Credential, []string) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Update: %v, want the hook's error", err)
	}
	got, err := s.Get(context.Background(), c.ID)
	if err != nil || got.Username != "bot" {
		t.Errorf("after the rollback = %+v, %v; a change must not exist without its audit row", got, err)
	}
}
func TestDeleteRemovesTheCredentialAndASecondDeleteIsNotFound(t *testing.T) {
	s, st, _ := newStore(t)
	c := create(t, s, nil)
	var seen registry.Credential
	got, err := s.Delete(context.Background(), c.ID, func(_ context.Context, _ *sql.Tx, d registry.Credential) error {
		seen = d
		return nil
	})
	if err != nil || got.ID != c.ID || got.Name != c.Name || seen.ID != c.ID {
		t.Fatalf("Delete = %+v, %v (hook saw %+v)", got, err, seen)
	}
	var n int
	if err := st.Read.QueryRow(`SELECT count(*) FROM registry_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows = %d, %v", n, err)
	}
	if _, err := s.Delete(context.Background(), c.ID, nil); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("second Delete: %v, want ErrNotFound", err)
	}
}

func TestDeleteRollsBackWhenTheHookFails(t *testing.T) {
	s, _, _ := newStore(t)
	c := create(t, s, nil)
	boom := errors.New("audit write failed")
	if _, err := s.Delete(context.Background(), c.ID, func(context.Context, *sql.Tx, registry.Credential) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Delete: %v, want the hook's error", err)
	}
	if _, err := s.Get(context.Background(), c.ID); err != nil {
		t.Errorf("the credential is gone though its audit row failed: %v", err)
	}
}

func addEnvironment(t *testing.T, st *store.Store, id, name string) {
	t.Helper()
	if _, err := st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
		VALUES (?, 'agent', ?, '', 1, 1, 1)`, id, name); err != nil {
		t.Fatal(err)
	}
}

func TestCandidatesAreTheInstanceLayerAndOneEnvironmentsWithNoSecretAndNoOtherEnvironment(t *testing.T) {
	s, st, _ := newStore(t)
	addEnvironment(t, st, "env-a", "Alpha")
	addEnvironment(t, st, "env-b", "Beta")
	create(t, s, func(in *registry.CreateInput) { in.Name = "inst"; in.Prefix = "acme" })
	create(t, s, func(in *registry.CreateInput) { in.Name = "in-a"; in.Prefix = "acme/team"; in.EnvID = "env-a" })
	create(t, s, func(in *registry.CreateInput) { in.Name = "in-b"; in.Prefix = "acme/team/app"; in.EnvID = "env-b" })

	got, err := s.Candidates(context.Background(), "env-a")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(got))
	for i, c := range got {
		names[i] = c.Name
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"in-a", "inst"}) {
		t.Fatalf("candidates = %v, want the instance one and Alpha's only", names)
	}
	for _, c := range got {
		switch c.Name {
		case "inst":
			if c.Layer != registry.LayerInstance || c.EnvID != "" || c.EnvName != "" || c.Host != "ghcr.io" || c.Prefix != "acme" {
				t.Errorf("instance candidate = %+v", c)
			}
		case "in-a":
			if c.Layer != registry.LayerEnvironment || c.EnvID != "env-a" || c.EnvName != "Alpha" || c.Prefix != "acme/team" {
				t.Errorf("environment candidate = %+v", c)
			}
		}
		if c.ID == "" {
			t.Errorf("candidate %q has no ID", c.Name)
		}
	}

	none, err := s.Candidates(context.Background(), "env-without-credentials")
	if err != nil || len(none) != 1 || none[0].Name != "inst" {
		t.Errorf("an environment with none of its own: %+v, %v, want only the instance one", none, err)
	}
}

func TestAnEnvironmentMayHoldTheHostAndPrefixOfAnInstanceCredentialButNotTwice(t *testing.T) {
	s, st, _ := newStore(t)
	addEnvironment(t, st, "env-a", "Alpha")
	addEnvironment(t, st, "env-b", "Beta")
	create(t, s, nil) // the instance credential for ghcr.io/acme

	// D-02: the same host and prefix in an environment is allowed, and so is the same name.
	own := create(t, s, func(in *registry.CreateInput) { in.EnvID = "env-a" })
	if own.EnvID != "env-a" || own.Layer() != registry.LayerEnvironment {
		t.Fatalf("credential = %+v, want the environment layer of env-a", own)
	}
	// Another environment holds the same too: the layers are per environment.
	create(t, s, func(in *registry.CreateInput) { in.EnvID = "env-b" })

	in := valid()
	in.EnvID, in.Name = "env-a", "another"
	if _, err := s.Create(context.Background(), in, "admin-1", nil); !errors.Is(err, registry.ErrHostPrefixTaken) {
		t.Errorf("a second credential for the same host and prefix in one environment: %v, want ErrHostPrefixTaken", err)
	}
}

func TestGetInEnvIsOnlyTheCredentialsOwnEnvironment(t *testing.T) {
	s, st, _ := newStore(t)
	addEnvironment(t, st, "env-a", "Alpha")
	addEnvironment(t, st, "env-b", "Beta")
	instance := create(t, s, nil)
	inA := create(t, s, func(in *registry.CreateInput) { in.EnvID = "env-a" })

	ctx := context.Background()
	got, err := s.GetInEnv(ctx, "env-a", inA.ID)
	if err != nil || got.ID != inA.ID || got.EnvID != "env-a" {
		t.Fatalf("GetInEnv(env-a, own) = %+v, %v", got, err)
	}
	for name, c := range map[string]struct{ env, id string }{
		"another environment's credential": {"env-b", inA.ID},
		"an instance credential":           {"env-a", instance.ID},
		"an unknown ID":                    {"env-a", "no-such"},
		"an empty environment":             {"", inA.ID},
	} {
		if _, err := s.GetInEnv(ctx, c.env, c.id); !errors.Is(err, registry.ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}
}
