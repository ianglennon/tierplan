// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package auth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// --- usernames -------------------------------------------------------------

func TestNormalizeUsernameLowercasesAndAcceptsTheAllowedSet(t *testing.T) {
	for in, want := range map[string]string{
		"Admin.User":            "admin.user",
		"abc":                   "abc",
		"A_b-c.9":               "a_b-c.9",
		strings.Repeat("x", 32): strings.Repeat("x", 32),
	} {
		got, err := auth.NormalizeUsername(in)
		if err != nil || got != want {
			t.Errorf("NormalizeUsername(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestNormalizeUsernameRejectsOutOfRangeAndForeignCharacters(t *testing.T) {
	for _, in := range []string{
		"", "ab", strings.Repeat("x", 33), "bad name", "ünïcode", "a/b", "a@b", "tab\tname", "new\nline", " lead",
	} {
		if got, err := auth.NormalizeUsername(in); !errors.Is(err, auth.ErrInvalidUsername) {
			t.Errorf("NormalizeUsername(%q) = %q, %v; want ErrInvalidUsername", in, got, err)
		}
	}
}

// --- password policy -------------------------------------------------------

func TestMinPasswordLengthIsTwelve(t *testing.T) {
	if auth.MinPasswordLength != 12 {
		t.Fatalf("MinPasswordLength = %d, want 12 (D-07)", auth.MinPasswordLength)
	}
}

func TestValidatePasswordRejectsShortOnes(t *testing.T) {
	if err := auth.ValidatePassword("abcdefghijk", "admin"); !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Fatalf("an 11-character password: %v, want ErrPasswordTooShort", err)
	}
	if err := auth.ValidatePassword("", "admin"); !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Fatalf("an empty password: %v, want ErrPasswordTooShort", err)
	}
}

func TestValidatePasswordCountsCharactersNotBytes(t *testing.T) {
	// Twelve runes of three bytes each pass the length rule; eleven do not.
	if err := auth.ValidatePassword(strings.Repeat("é", 11), "admin"); !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Fatalf("11 runes: %v, want ErrPasswordTooShort", err)
	}
	if err := auth.ValidatePassword(strings.Repeat("語", 12), "admin"); err != nil {
		t.Fatalf("12 runes: %v, want accepted", err)
	}
}

func TestValidatePasswordRejectsTheEmbeddedCommonList(t *testing.T) {
	for _, pw := range []string{"passwordpassword", "PasswordPassword", "PASSWORDPASSWORD"} {
		if err := auth.ValidatePassword(pw, "admin"); !errors.Is(err, auth.ErrPasswordCommon) {
			t.Errorf("ValidatePassword(%q) = %v, want ErrPasswordCommon", pw, err)
		}
	}
}

func TestValidatePasswordRejectsAPasswordContainingTheUsername(t *testing.T) {
	if err := auth.ValidatePassword("xx-Gandalf-the-grey-xx", "gandalf"); !errors.Is(err, auth.ErrPasswordCommon) {
		t.Fatalf("a password containing the username: %v, want ErrPasswordCommon", err)
	}
}

func TestValidatePasswordHasNoCompositionRules(t *testing.T) {
	for _, pw := range []string{"correct horse battery", "aaaaaaaaaaaaaaaaaaaaaaaaaa-no-digits", "1234567890123 only digits"} {
		if err := auth.ValidatePassword(pw, "admin"); err != nil {
			t.Errorf("ValidatePassword(%q) = %v, want accepted (D-07: no composition rules)", pw, err)
		}
	}
}

// --- argon2id --------------------------------------------------------------

func TestHashPasswordProducesAnArgon2idPHCStringWithTheOWASPParameters(t *testing.T) {
	h, err := auth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash %q does not carry the expected parameters", h)
	}
	h2, _ := auth.HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("two hashes of the same password are identical: the salt is not random")
	}
	parts := strings.Split(h, "$")
	if len(parts) != 6 {
		t.Fatalf("PHC string has %d parts, want 6: %q", len(parts), h)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		t.Fatalf("salt: %v, %d bytes, want 16", err, len(salt))
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) != 32 {
		t.Fatalf("key: %v, %d bytes, want 32", err, len(key))
	}
}

func TestVerifyPasswordAcceptsTheRightPasswordAndRejectsAWrongOne(t *testing.T) {
	h, err := auth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := auth.VerifyPassword("correct horse battery", h); err != nil || !ok {
		t.Fatalf("right password: %v, %v", ok, err)
	}
	for _, wrong := range []string{"correct horse batter", "Correct horse battery", "", "correct horse battery "} {
		if ok, err := auth.VerifyPassword(wrong, h); err != nil || ok {
			t.Errorf("wrong password %q: %v, %v; want false, nil", wrong, ok, err)
		}
	}
}

// phc builds a hash with explicit parameters, as an older or newer release
// would have stored it.
func phc(pw string, m, t uint32, p uint8) string {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(pw), salt, t, m, p, 32)
	return "$argon2id$v=19$m=" + strconv.FormatUint(uint64(m), 10) +
		",t=" + strconv.FormatUint(uint64(t), 10) +
		",p=" + strconv.FormatUint(uint64(p), 10) + "$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key)
}

func TestVerifyPasswordUsesTheParametersEncodedInTheHash(t *testing.T) {
	h := phc("another long passphrase", 8*1024, 1, 2)
	if ok, err := auth.VerifyPassword("another long passphrase", h); err != nil || !ok {
		t.Fatalf("hash with different parameters: %v, %v", ok, err)
	}
	if ok, err := auth.VerifyPassword("another long passphrasf", h); err != nil || ok {
		t.Fatalf("wrong password against different parameters: %v, %v", ok, err)
	}
}

func TestVerifyPasswordRejectsMalformedHashesWithoutPanicking(t *testing.T) {
	for _, h := range []string{
		"", "x", "$argon2id$", "$bcrypt$v=19$m=1,t=1,p=1$AAAA$AAAA",
		"$argon2id$v=18$m=19456,t=2,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=abc,t=2,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=19456,t=2,p=1$!!!!$AAAA",
		"$argon2id$v=19$m=19456,t=2,p=1$AAAA$",
		// A tampered row must not make the server allocate gigabytes or spin for minutes.
		"$argon2id$v=19$m=4294967295,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=19456,t=4294967295,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	} {
		ok, err := auth.VerifyPassword("whatever password", h)
		if ok || err == nil {
			t.Errorf("VerifyPassword with %q = %v, %v; want false and an error", h, ok, err)
		}
	}
}

func TestDummyVerifyDoesARealArgon2idComputation(t *testing.T) {
	start := time.Now()
	auth.DummyVerify("anything at all")
	// wall-clock: lower bound; the dummy verify cannot finish faster under load
	if d := time.Since(start); d < time.Millisecond {
		t.Fatalf("DummyVerify took %v; it must hash for real so unknown users cost the same as known ones", d)
	}
}

func TestConcurrentHashingIsBoundedAndStillCompletes(t *testing.T) {
	if auth.MaxConcurrentHashes != 4 {
		t.Fatalf("MaxConcurrentHashes = %d, want 4 (Pitfall 17)", auth.MaxConcurrentHashes)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := auth.HashPassword("a perfectly fine passphrase")
			if err != nil {
				errs <- err
				return
			}
			if ok, err := auth.VerifyPassword("a perfectly fine passphrase", h); err != nil || !ok {
				errs <- errors.New("verify failed")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// --- sessions --------------------------------------------------------------

type sessionFixture struct {
	st  *store.Store
	m   *auth.Sessions
	v   *vault.Vault
	now time.Time
}

const (
	testIdle     = 12 * time.Hour
	testRemember = 30 * 24 * time.Hour
)

func newVault(t *testing.T, fill byte) *vault.Vault {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
	v, err := vault.LoadOrCreate(key, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	f := &sessionFixture{
		st:  storetest.New(t),
		v:   newVault(t, 7),
		now: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
	}
	f.m = auth.NewSessions(f.st, f.v, testIdle, testRemember, func() time.Time { return f.now })
	n := f.now.UnixNano()
	if err := f.st.Q.InsertUser(context.Background(), sqlc.InsertUserParams{
		ID: "u1", Username: "admin", PasswordHash: "x", CreatedAt: n, UpdatedAt: n, PasswordChangedAt: n,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *sessionFixture) create(t *testing.T, remember bool) (string, []byte) {
	t.Helper()
	tx, err := f.st.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, hash, err := f.m.CreateTx(context.Background(), tx, "u1", remember, "203.0.113.9", "test-agent/1.0")
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return raw, hash
}

func TestCreateTxStoresOnlyTheSHA256OfTheSessionID(t *testing.T) {
	f := newSessionFixture(t)
	raw, hash := f.create(t, false)

	sum := sha256.Sum256([]byte(raw))
	if !bytes.Equal(hash, sum[:]) {
		t.Fatalf("returned hash is not sha256(raw ID)")
	}
	row, err := f.st.RQ.GetSessionByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("session not stored under its hash: %v", err)
	}
	if bytes.Equal(row.IDHash, []byte(raw)) {
		t.Fatal("the raw session ID is the stored key")
	}
	// Scan the whole table for the raw ID, in text and as bytes.
	var hits int
	if err := f.st.Read.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE instr(CAST(id_hash AS TEXT), ?1) > 0 OR instr(user_id, ?1) > 0 OR instr(ip, ?1) > 0 OR instr(user_agent, ?1) > 0`,
		raw).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatal("the raw session ID appears in the sessions table")
	}
	if row.UserID != "u1" || row.Ip != "203.0.113.9" || row.UserAgent != "test-agent/1.0" {
		t.Fatalf("row = %+v", row)
	}
	if row.CreatedAt != f.now.UnixNano() || row.LastSeenAt != f.now.UnixNano() {
		t.Fatalf("timestamps = %d, %d; want %d", row.CreatedAt, row.LastSeenAt, f.now.UnixNano())
	}
}

func TestSessionIDsAre256BitsOfRandomnessAndNeverRepeat(t *testing.T) {
	f := newSessionFixture(t)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		raw, _ := f.create(t, false)
		dec, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || len(dec) != 32 {
			t.Fatalf("session ID %q: %v, %d bytes, want 32 bytes of unpadded base64url", raw, err, len(dec))
		}
		if seen[raw] {
			t.Fatal("a session ID repeated")
		}
		seen[raw] = true
	}
}

func TestCreateTxSetsAnAbsoluteExpiryOnlyForRememberedSessions(t *testing.T) {
	f := newSessionFixture(t)
	_, plain := f.create(t, false)
	_, kept := f.create(t, true)

	p, err := f.st.RQ.GetSessionByHash(context.Background(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if p.AbsoluteExpiresAt.Valid || p.Remember != 0 {
		t.Fatalf("browser-session row: %+v, want no absolute expiry and remember 0", p)
	}
	k, err := f.st.RQ.GetSessionByHash(context.Background(), kept)
	if err != nil {
		t.Fatal(err)
	}
	if !k.AbsoluteExpiresAt.Valid || k.AbsoluteExpiresAt.Int64 != f.now.Add(testRemember).UnixNano() || k.Remember != 1 {
		t.Fatalf("remembered row: %+v, want expiry %d", k, f.now.Add(testRemember).UnixNano())
	}
}

func TestCreateTxRollsBackWithTheCallersTransaction(t *testing.T) {
	f := newSessionFixture(t)
	tx, err := f.st.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := f.m.CreateTx(context.Background(), tx, "u1", false, "203.0.113.9", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.RQ.GetSessionByHash(context.Background(), hash); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("session survived a rollback: %v", err)
	}
}

func TestCreateTxRefusesAnUnknownUser(t *testing.T) {
	f := newSessionFixture(t)
	tx, err := f.st.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, _, err := f.m.CreateTx(context.Background(), tx, "no-such-user", false, "203.0.113.9", "ua"); err == nil {
		t.Fatal("a session was created for a user that does not exist")
	}
}

func TestDeletingAUserDeletesTheirSessions(t *testing.T) {
	f := newSessionFixture(t)
	_, hash := f.create(t, false)
	if _, err := f.st.Write.Exec(`DELETE FROM users WHERE id = 'u1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.RQ.GetSessionByHash(context.Background(), hash); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("session outlived its user: %v", err)
	}
}

func TestCookieOverHTTPSIsHostPrefixedSecureAndHTTPOnly(t *testing.T) {
	f := newSessionFixture(t)
	c := f.m.Cookie("rawid", false, auth.CookieScope{})
	if c.Name != "__Host-tierplan_session" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("https cookie = %+v", c)
	}
	if c.Value != "rawid" {
		t.Fatalf("value = %q", c.Value)
	}
	if c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Fatalf("a browser-session cookie must carry no lifetime: MaxAge %d, Expires %v", c.MaxAge, c.Expires)
	}
	if auth.CookieName(auth.CookieScope{}) != "__Host-tierplan_session" {
		t.Fatalf("CookieName(secure) = %q", auth.CookieName(auth.CookieScope{}))
	}
}

func TestCookieWithRememberCarriesTheRememberLifetime(t *testing.T) {
	f := newSessionFixture(t)
	c := f.m.Cookie("rawid", true, auth.CookieScope{})
	if want := int(testRemember / time.Second); c.MaxAge != want {
		t.Fatalf("MaxAge = %d, want %d", c.MaxAge, want)
	}
}

func TestCookieUnderABasePathIsSecurePrefixedAndScopedToIt(t *testing.T) {
	f := newSessionFixture(t)
	c := f.m.Cookie("rawid", false, auth.CookieScope{BasePath: "/tierplan"})
	if c.Name != "__Secure-tierplan_session" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/tierplan" || c.Domain != "" {
		t.Fatalf("base-path cookie = %+v", c)
	}
	clear := auth.ClearCookie(auth.CookieScope{BasePath: "/tierplan"})
	if clear.Name != "__Secure-tierplan_session" || !clear.Secure || clear.Path != "/tierplan" || clear.MaxAge != -1 {
		t.Fatalf("base-path clear cookie = %+v", clear)
	}
}

func TestClearCookieExpiresTheSameNameImmediately(t *testing.T) {
	c := auth.ClearCookie(auth.CookieScope{})
	if c.Name != "__Host-tierplan_session" || c.MaxAge != -1 || c.Value != "" || c.Path != "/" || !c.HttpOnly || !c.Secure {
		t.Errorf("ClearCookie = %+v", c)
	}
}

func TestCookieHeaderCarriesTheHardeningAttributesAndNoDomain(t *testing.T) {
	f := newSessionFixture(t)
	c := f.m.Cookie("rawid", true, auth.CookieScope{})
	s := c.String()
	for _, want := range []string{"__Host-tierplan_session=rawid", "Path=/", "HttpOnly", "Secure", "SameSite=Lax", "Max-Age=2592000"} {
		if !strings.Contains(s, want) {
			t.Errorf("Set-Cookie %q lacks %q", s, want)
		}
	}
	if strings.Contains(s, "Domain") {
		t.Errorf("Set-Cookie %q carries a Domain (the __Host- prefix forbids it)", s)
	}
}

func TestCSRFTokenIsStablePerSessionAndDiffersAcrossSessions(t *testing.T) {
	f := newSessionFixture(t)
	rawA, hashA := f.create(t, false)
	rawB, hashB := f.create(t, false)

	a1, a2, b := f.m.CSRFToken(hashA), f.m.CSRFToken(hashA), f.m.CSRFToken(hashB)
	if a1 == "" || a1 != a2 {
		t.Fatalf("token is not stable: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatal("two sessions share a CSRF token")
	}
	if a1 == rawA || b == rawB {
		t.Fatal("the CSRF token equals the session ID")
	}
	if strings.Contains(a1, rawA) || strings.Contains(a1, base64.RawURLEncoding.EncodeToString(hashA)) {
		t.Fatal("the CSRF token embeds the session ID or its hash")
	}
	dec, err := base64.RawURLEncoding.DecodeString(a1)
	if err != nil || len(dec) != sha256.Size {
		t.Fatalf("token %q: %v, %d bytes, want an HMAC-SHA256 in unpadded base64url", a1, err, len(dec))
	}
}

func TestCSRFTokenDependsOnTheVaultKey(t *testing.T) {
	f := newSessionFixture(t)
	_, hash := f.create(t, false)
	other := auth.NewSessions(f.st, newVault(t, 9), testIdle, testRemember, func() time.Time { return f.now })
	if f.m.CSRFToken(hash) == other.CSRFToken(hash) {
		t.Fatal("two different master keys produce the same CSRF token: the token is not keyed")
	}
}
