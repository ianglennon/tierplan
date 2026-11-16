// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"testing/fstest"

	"github.com/ianglennon/tierplan/internal/store"
)

// migratedFresh is a new database at the schema this binary embeds.
func migratedFresh(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := store.Migrate(context.Background(), s, store.MigrateOptions{AppVersion: "v0.1.0", DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAFreshDatabaseHasTheRowsTheAppExpects(t *testing.T) {
	s := migratedFresh(t)
	for table, want := range map[string]int{"roles": 3, "janitor_defaults": 1, "image_update_defaults": 1} {
		var n int
		if err := s.Read.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != want {
			t.Errorf("%s has %d rows (%v), want %d", table, n, err, want)
		}
	}
}

func TestSchemaConstraints(t *testing.T) {
	s := migratedFresh(t)
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u-1', 'ada', 'x', 1, 1, 1)`,
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type) VALUES ('env-a', 'agent', 'A', '', 1, 1, 1, 'standalone')`,
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type) VALUES ('env-b', 'agent', 'B', '', 1, 1, 1, 'standalone')`,
	} {
		if _, err := s.Write.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	assertRestartLoopChecks(t, s)
	assertRestartLoopMutesTableWorks(t, s)
	assertShellPrefsTableWorks(t, s, "u-1")
	assertIngressTablesWork(t, s)
}

func assertRestartLoopChecks(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	const failures = `UPDATE environments SET restart_loop_failures = ? WHERE id = 'env-a'`
	const window = `UPDATE environments SET restart_loop_window_minutes = ? WHERE id = 'env-a'`
	for _, c := range []struct {
		name, query string
		val         int
		want        bool
	}{
		{"restart_loop_failures", failures, 1, false}, {"restart_loop_failures", failures, 2, true},
		{"restart_loop_failures", failures, 50, true}, {"restart_loop_failures", failures, 51, false},
		{"restart_loop_window_minutes", window, 0, false}, {"restart_loop_window_minutes", window, 1, true},
		{"restart_loop_window_minutes", window, 120, true}, {"restart_loop_window_minutes", window, 121, false},
	} {
		_, err := s.Write.ExecContext(ctx, c.query, c.val)
		if (err == nil) != c.want {
			t.Errorf("%s = %d: accepted = %v, want %v (%v)", c.name, c.val, err == nil, c.want, err)
		}
	}
	if _, err := s.Write.ExecContext(ctx, `UPDATE environments SET restart_loop_failures = 3, restart_loop_window_minutes = 10 WHERE id = 'env-a'`); err != nil {
		t.Fatal(err)
	}
}

// assertRestartLoopMutesTableWorks checks the mutes' uniqueness per environment, kind and
// target, the kinds the table accepts, and that a removed environment takes its mutes.
func assertRestartLoopMutesTableWorks(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	const ins = `INSERT INTO restart_loop_mutes (env_id, kind, target_id, name, muted_by, muted_at) VALUES (?, ?, ?, 'web', 'u-1', 5)`
	if _, err := s.Write.ExecContext(ctx, ins, "env-b", "service", "svc-1"); err != nil {
		t.Fatalf("insert a mute: %v", err)
	}
	if _, err := s.Write.ExecContext(ctx, ins, "env-b", "service", "svc-1"); err == nil {
		t.Errorf("a second mute of the same target was accepted")
	}
	if _, err := s.Write.ExecContext(ctx, ins, "env-b", "container", "svc-1"); err != nil {
		t.Errorf("the same id as a container is its own target: %v", err)
	}
	if _, err := s.Write.ExecContext(ctx, ins, "env-b", "stack", "svc-2"); err == nil {
		t.Errorf("a kind outside service and container was accepted")
	}
	if _, err := s.Write.ExecContext(ctx, `DELETE FROM environments WHERE id = 'env-b'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM restart_loop_mutes WHERE env_id = 'env-b'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d mutes outlived their environment (%v)", n, err)
	}
}

// assertShellPrefsTableWorks writes and reads a remembered choice for the user, and checks
// that the table refuses a shell it does not know and goes with its user.
func assertShellPrefsTableWorks(t *testing.T, s *store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO user_shell_prefs (user_id, image, shell, command, run_as, working_dir, updated_at) VALUES (?, 'nginx:1.27', 'bash', '', 'root', '', 5)`, userID); err != nil {
		t.Fatalf("insert a shell preference: %v", err)
	}
	var shell, runAs string
	if err := s.Read.QueryRowContext(ctx, `SELECT shell, run_as FROM user_shell_prefs WHERE user_id = ? AND image = 'nginx:1.27'`, userID).Scan(&shell, &runAs); err != nil || shell != "bash" || runAs != "root" {
		t.Errorf("preference = %q as %q (%v), want bash as root", shell, runAs, err)
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO user_shell_prefs (user_id, image, shell, updated_at) VALUES (?, 'other', 'zsh', 6)`, userID); err == nil {
		t.Errorf("a shell outside auto, bash, sh, ash and custom was accepted")
	}
}

// A database as v0.1.0 left it (migrated to 00036 only) gains tls_certs when the next binary
// migrates it, and the rows it already held are untouched.
func TestTLSCertsTableArrivesOnA00036Database(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.1.0", DataDir: dir, Migrations: plus(t, 0)}); err != nil {
		t.Fatal(err)
	}
	if hasTable(t, s, "tls_certs") {
		t.Fatal("a database at 00036 already has tls_certs")
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u-1', 'ada', 'x', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}

	res, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.2.0", DataDir: dir})
	if err != nil {
		t.Fatalf("migrate from 00036: %v", err)
	}
	if res.From != 36 || res.To < 37 {
		t.Errorf("From, To = %d, %d; want 36 and at least 37", res.From, res.To)
	}
	if !hasTable(t, s, "tls_certs") {
		t.Error("tls_certs is missing after migrating a 00036 database")
	}
	var name string
	if err := s.Read.QueryRowContext(ctx, `SELECT username FROM users WHERE id = 'u-1'`).Scan(&name); err != nil || name != "ada" {
		t.Errorf("the existing user row = %q, %v; want ada", name, err)
	}
	assertTLSCertsTableWorks(t, s)
}

func TestTLSCertsTableExistsInAFreshDatabase(t *testing.T) {
	s := migratedFresh(t)
	if !hasTable(t, s, "tls_certs") {
		t.Fatal("tls_certs is missing from a fresh database")
	}
	assertTLSCertsTableWorks(t, s)
}

func hasTable(t *testing.T, s *store.Store, name string) bool {
	t.Helper()
	var n int
	if err := s.Read.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// assertTLSCertsTableWorks checks the id is free (the default certificate and later sources each
// use their own) and unique, and that every column is required.
func assertTLSCertsTableWorks(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	const ins = `INSERT INTO tls_certs (id, cert_pem, key_sealed, created_at, updated_at) VALUES (?, 'c', 'k', 1, 1)`
	for _, id := range []string{"selfsigned", "acme"} {
		if _, err := s.Write.ExecContext(ctx, ins, id); err != nil {
			t.Errorf("insert %q: %v", id, err)
		}
	}
	if _, err := s.Write.ExecContext(ctx, ins, "selfsigned"); err == nil {
		t.Error("a second row with the same id was accepted")
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO tls_certs (id, cert_pem, created_at, updated_at) VALUES ('nokey', 'c', 1, 1)`); err == nil {
		t.Error("a row without a sealed key was accepted")
	}
}

// upTo37 is the embedded migrations as far as 00037, which is what a build before Phase 14 knew.
func upTo37(t *testing.T) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	for _, f := range []string{"00036_baseline.sql", "00037_tls_certs.sql"} {
		data, err := os.ReadFile("migrations/" + f) //nolint:gosec // a test reads the embedded migration files by their fixed names
		if err != nil {
			t.Fatal(err)
		}
		m[f] = &fstest.MapFile{Data: data}
	}
	return m
}

func TestACMETablesExistInAFreshDatabase(t *testing.T) {
	s := migratedFresh(t)
	assertACMETablesWork(t, s)
}

// A database as the build before Let's Encrypt left it (migrated to 00037) gains the ACME tables, and
// the self-signed certificate row it already held is untouched.
func TestACMETablesArriveOnA00037Database(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.1.0", DataDir: dir, Migrations: upTo37(t)}); err != nil {
		t.Fatal(err)
	}
	if hasTable(t, s, "acme_state") {
		t.Fatal("a database at 00037 already has acme_state")
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO tls_certs (id, cert_pem, key_sealed, created_at, updated_at) VALUES ('selfsigned', 'cert', 'v1.k1-x.sealed', 11, 12)`); err != nil {
		t.Fatal(err)
	}

	res, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.2.0", DataDir: dir})
	if err != nil {
		t.Fatalf("migrate from 00037: %v", err)
	}
	if res.From != 37 || res.To < 38 {
		t.Errorf("From, To = %d, %d; want 37 and at least 38", res.From, res.To)
	}
	var cert, sealed string
	var created, updated int64
	if err := s.Read.QueryRowContext(ctx, `SELECT cert_pem, key_sealed, created_at, updated_at FROM tls_certs WHERE id = 'selfsigned'`).Scan(&cert, &sealed, &created, &updated); err != nil {
		t.Fatalf("the selfsigned row after migrating: %v", err)
	}
	if cert != "cert" || sealed != "v1.k1-x.sealed" || created != 11 || updated != 12 {
		t.Errorf("the selfsigned row changed: %q %q %d %d", cert, sealed, created, updated)
	}
	assertACMETablesWork(t, s)
}

// assertACMETablesWork checks the four tables exist, the state singleton is seeded empty and the
// CHECK constraints refuse what the code never writes.
func assertACMETablesWork(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"acme_account", "acme_state", "acme_orders", "acme_runs"} {
		if !hasTable(t, s, table) {
			t.Errorf("%s is missing", table)
		}
	}
	var n int
	var mode, paused string
	var failures, lastAttempt int64
	if err := s.Read.QueryRowContext(ctx, `SELECT COUNT(*), MIN(mode), MIN(paused_reason), MIN(consecutive_failures), MIN(last_attempt_at) FROM acme_state`).Scan(&n, &mode, &paused, &failures, &lastAttempt); err != nil {
		t.Fatal(err)
	}
	if n != 1 || mode != "off" || paused != "" || failures != 0 || lastAttempt != 0 {
		t.Errorf("acme_state = %d rows, mode %q, pause %q, %d failures, last attempt %d; want one empty row in mode off", n, mode, paused, failures, lastAttempt)
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO acme_state (id) VALUES (2)`); err == nil {
		t.Error("a second acme_state row was accepted")
	}
	if _, err := s.Write.ExecContext(ctx, `UPDATE acme_state SET mode = 'sometimes'`); err == nil {
		t.Error("a mode outside off and active was accepted")
	}
	const order = `INSERT INTO acme_orders (id, directory_url, order_url, kind, names, cert_key_sealed, stage, status, created_at, updated_at) VALUES (?, 'd', 'o', ?, '[]', 'k', 'created', ?, 1, 1)`
	if _, err := s.Write.ExecContext(ctx, order, "o-1", "issue", "open"); err != nil {
		t.Errorf("a valid order was refused: %v", err)
	}
	if _, err := s.Write.ExecContext(ctx, order, "o-2", "other", "open"); err == nil {
		t.Error("an order of a kind other than issue and renew was accepted")
	}
	if _, err := s.Write.ExecContext(ctx, order, "o-3", "renew", "pending"); err == nil {
		t.Error("an order status other than open and closed was accepted")
	}
}

// upTo40 is the embedded migrations as far as 00040, which is what a build before the review fix that
// keeps the zone name in the DNS record ledger knew.
func upTo40(t *testing.T) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	for _, f := range []string{
		"00036_baseline.sql", "00037_tls_certs.sql", "00038_acme.sql", "00039_acme_issuing_account.sql", "00040_acme_dns.sql",
	} {
		data, err := os.ReadFile("migrations/" + f) //nolint:gosec // a test reads the embedded migration files by their fixed names
		if err != nil {
			t.Fatal(err)
		}
		m[f] = &fstest.MapFile{Data: data}
	}
	return m
}

// A DNS ledger row written before 00041 keeps working: its zone name reads ”, and a row written after
// carries one.
func TestACMEDNSRecordsGainAZoneNameOnA00040Database(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.1.0", DataDir: dir, Migrations: upTo40(t)}); err != nil {
		t.Fatal(err)
	}
	const ins = `INSERT INTO acme_dns_records (id, provider, zone_id, record_id, name, value, owner_kind, owner_id, created_at)
		VALUES (?, 'cloudflare', 'zone-1', 'rec-1', '_acme-challenge.a.example.org', 'v', 'order', 'order-1', 1)`
	if _, err := s.Write.ExecContext(ctx, ins, "old-row"); err != nil {
		t.Fatal(err)
	}

	res, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.2.0", DataDir: dir})
	if err != nil {
		t.Fatalf("migrate from 00040: %v", err)
	}
	if res.From != 40 || res.To < 41 {
		t.Errorf("From, To = %d, %d; want 40 and at least 41", res.From, res.To)
	}
	var name string
	if err := s.Read.QueryRowContext(ctx, `SELECT zone_name FROM acme_dns_records WHERE id = 'old-row'`).Scan(&name); err != nil || name != "" {
		t.Errorf("zone_name of the old row = %q, %v; want ''", name, err)
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO acme_dns_records (id, provider, zone_id, zone_name, record_id, name, value, owner_kind, owner_id, created_at)
		VALUES ('new-row', 'cloudflare', 'zone-1', 'example.org', 'rec-2', '_acme-challenge.b.example.org', 'v', 'order', 'order-1', 2)`); err != nil {
		t.Fatalf("a row with a zone name: %v", err)
	}
}

// upTo41 is the embedded migrations as far as 00041.
func upTo41(t *testing.T) fstest.MapFS {
	t.Helper()
	m := upTo40(t)
	data, err := os.ReadFile("migrations/00041_acme_dns_zone_name.sql") //nolint:gosec // a test reads the embedded migration files by their fixed names
	if err != nil {
		t.Fatal(err)
	}
	m["00041_acme_dns_zone_name.sql"] = &fstest.MapFile{Data: data}
	return m
}

// A DNS ledger row written before 00042 keeps its zone name and reads an empty account ID; a row
// written after carries one.
func TestACMEDNSRecordsGainAnAccountIDOnA00041Database(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.1.0", DataDir: dir, Migrations: upTo41(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO acme_dns_records (id, provider, zone_id, zone_name, record_id, name, value, owner_kind, owner_id, created_at)
		VALUES ('old-row', 'cloudflare', 'zone-1', 'example.org', 'rec-1', '_acme-challenge.a.example.org', 'v', 'order', 'order-1', 1)`); err != nil {
		t.Fatal(err)
	}

	res, err := store.Migrate(ctx, s, store.MigrateOptions{AppVersion: "v0.2.0", DataDir: dir})
	if err != nil {
		t.Fatalf("migrate from 00041: %v", err)
	}
	if res.From != 41 || res.To < 42 {
		t.Errorf("From, To = %d, %d; want 41 and at least 42", res.From, res.To)
	}
	var zone, account string
	if err := s.Read.QueryRowContext(ctx, `SELECT zone_name, account_id FROM acme_dns_records WHERE id = 'old-row'`).Scan(&zone, &account); err != nil || zone != "example.org" || account != "" {
		t.Errorf("old row = zone %q, account %q, %v; want its zone name kept and an empty account ID", zone, account, err)
	}
	if _, err := s.Write.ExecContext(ctx, `INSERT INTO acme_dns_records (id, provider, zone_id, zone_name, account_id, record_id, name, value, owner_kind, owner_id, created_at)
		VALUES ('new-row', 'cloudflare', 'zone-1', 'example.org', 'acct-1', 'rec-2', '_acme-challenge.b.example.org', 'v', 'order', 'order-1', 2)`); err != nil {
		t.Fatalf("a row with an account ID: %v", err)
	}
}

// assertIngressTablesWork checks the ingress tables' constraints on raw SQL: one owner per hostname
// install-wide, lower-case names, one route per hostname and path, the restrict on a domain in use,
// the cascade from a stack and the guarded config_version trigger, and that an environment holding
// all of it can still be removed.
func assertIngressTablesWork(t *testing.T, s *store.Store) {
	t.Helper()
	exec := func(q string, args ...any) error {
		_, err := s.Write.Exec(q, args...)
		return err
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustFail := func(what, q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err == nil {
			t.Errorf("%s was accepted", what)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.Write.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	const claim = `INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at) VALUES (?, ?, ?, 'stack', ?, 'u-1', 1)`
	const route = `INSERT INTO ingress_routes (id, env_id, hostname, name, path_prefix, service, port, created_at, updated_at) VALUES (?, ?, ?, '', ?, 'web', 80, 1, 1)`

	// Its own environments: earlier assertions remove env-b. env-i3 is the one removed at the end.
	for _, e := range []string{"env-i1", "env-i2", "env-i3"} {
		mustExec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type) VALUES (?, 'agent', ?, '', 1, 1, 1, 'standalone')`, e, e)
	}
	for _, d := range [][2]string{{"dom-a", "env-i1"}, {"dom-b", "env-i2"}, {"dom-c", "env-i3"}} {
		mustExec(`INSERT INTO ingress_domains (id, env_id, suffix, created_by, created_at, updated_at) VALUES (?, ?, 'example.org', 'u-1', 1, 1)`, d[0], d[1])
	}
	for _, st := range [][2]string{{"stack-a", "env-i1"}, {"stack-b", "env-i2"}, {"stack-c", "env-i3"}} {
		mustExec(`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES (?, ?, 'web', 'standalone', 'u-1', 1)`, st[0], st[1])
	}

	// One owner per hostname, whichever environment asks (D-01).
	mustExec(claim, "app.example.org", "env-i1", "dom-a", "stack-a")
	mustFail("a second claim of one hostname from another environment", claim, "app.example.org", "env-i2", "dom-b", "stack-b")
	mustFail("an upper-case hostname", claim, "App.example.org", "env-i2", "dom-b", "stack-b")
	// A route needs a claimed hostname and is unique per hostname and path prefix.
	mustFail("a route for an unclaimed hostname", route, "r-x", "env-i2", "other.example.org", "/")
	mustExec(route, "r-1", "env-i1", "app.example.org", "/")
	mustFail("a second route on one hostname and path prefix", route, "r-2", "env-i1", "app.example.org", "/")
	mustExec(route, "r-3", "env-i1", "app.example.org", "/api")

	// A domain in use cannot be deleted; its claim and routes survive (D-10).
	mustFail("the deletion of a domain that holds a claim", `DELETE FROM ingress_domains WHERE id = 'dom-a'`)
	if n := count(`SELECT COUNT(*) FROM ingress_hostnames WHERE hostname = 'app.example.org'`); n != 1 {
		t.Errorf("the claim is gone after a refused domain deletion: %d rows", n)
	}
	if n := count(`SELECT COUNT(*) FROM ingress_routes WHERE hostname = 'app.example.org'`); n != 2 {
		t.Errorf("the routes are gone after a refused domain deletion: %d rows", n)
	}

	// A domain with only reserved names deletes, and its reserved rows go with it.
	mustExec(`INSERT INTO ingress_reserved (domain_id, hostname, created_by, created_at) VALUES ('dom-b', 'admin.example.org', 'u-1', 1)`)
	mustFail("an upper-case reserved name", `INSERT INTO ingress_reserved (domain_id, hostname, created_by, created_at) VALUES ('dom-b', 'Admin.example.org', 'u-1', 1)`)
	mustExec(`DELETE FROM ingress_domains WHERE id = 'dom-b'`)
	if n := count(`SELECT COUNT(*) FROM ingress_reserved WHERE domain_id = 'dom-b'`); n != 0 {
		t.Errorf("%d reserved rows outlived their domain", n)
	}

	// Deleting a stack removes its claims and routes and moves config_version (D-23). Not by exactly
	// one: it rises per cascaded claim row.
	version := func(env string) int {
		t.Helper()
		var n int
		err := s.Write.QueryRow(`SELECT config_version FROM ingress_env WHERE env_id = ?`, env).Scan(&n)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return n
	}
	before := version("env-i1")
	mustExec(`DELETE FROM stacks WHERE id = 'stack-a'`)
	if n := count(`SELECT COUNT(*) FROM ingress_hostnames WHERE env_id = 'env-i1'`); n != 0 {
		t.Errorf("%d claims outlived their stack", n)
	}
	if n := count(`SELECT COUNT(*) FROM ingress_routes WHERE env_id = 'env-i1'`); n != 0 {
		t.Errorf("%d routes outlived their stack", n)
	}
	if after := version("env-i1"); after <= before {
		t.Errorf("config_version = %d after a stack deletion, want more than %d", after, before)
	}

	// An environment holding a domain, a claim and a stack can be removed (RESEARCH Pitfall 1), and the
	// guarded trigger does not fail on the way.
	mustExec(claim, "c.example.org", "env-i3", "dom-c", "stack-c")
	mustExec(route, "r-c", "env-i3", "c.example.org", "/")
	mustExec(`DELETE FROM environments WHERE id = 'env-i3'`)
	for _, table := range []string{"ingress_domains", "ingress_hostnames", "ingress_routes", "ingress_env", "stacks"} {
		if n := count(`SELECT COUNT(*) FROM ` + table + ` WHERE env_id = 'env-i3'`); n != 0 {
			t.Errorf("%s keeps %d rows of a removed environment", table, n)
		}
	}
}
