// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// The shell choice remembered per user per image (plan 04-24, D-26, UI-SPEC Defaults 19).
// The server writes it when an open succeeds and the launcher reads it back through a
// self-service route, so it follows the person to another browser.

type shellPref struct {
	Shell      string `json:"shell"`
	Command    string `json:"command"`
	User       string `json:"user"`
	WorkingDir string `json:"workingDir"`
}

func prefPath(image string) string {
	return "/api/me/shell-preferences?image=" + url.QueryEscape(image)
}

func (f *shellFix) prefRows(t *testing.T, userID string) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM user_shell_prefs WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestShellAnOpenRemembersTheChoiceForTheImageAndTheRouteGivesItBack(t *testing.T) {
	t.Parallel()
	f := newShellFix(t, nil)
	rec := f.open(t, "web", `{"shell":"bash","user":"root","workingDir":"/var/www","cols":80,"rows":24}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("open: %d %s", rec.code, rec.body)
	}
	got := f.req(t, http.MethodGet, prefPath("nginx:1.27"), "")
	if got.code != http.StatusOK {
		t.Fatalf("GET: %d %s", got.code, got.body)
	}
	if p := decode[shellPref](t, got); p != (shellPref{Shell: "bash", Command: "", User: "root", WorkingDir: "/var/www"}) {
		t.Errorf("preference = %+v, want bash as root in /var/www", p)
	}

	// The next open of the image replaces it, and a custom command is kept with it.
	rec = f.open(t, "web", `{"shell":"custom","command":"/bin/zsh -l","cols":80,"rows":24}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("second open: %d %s", rec.code, rec.body)
	}
	if p := decode[shellPref](t, f.req(t, http.MethodGet, prefPath("nginx:1.27"), "")); p != (shellPref{Shell: "custom", Command: "/bin/zsh -l"}) {
		t.Errorf("preference after a custom open = %+v", p)
	}

	// Choosing a built-in shell again forgets the command.
	if rec = f.open(t, "web", `{"shell":"sh","command":"ignored","cols":80,"rows":24}`); rec.code != http.StatusCreated {
		t.Fatalf("third open: %d %s", rec.code, rec.body)
	}
	if p := decode[shellPref](t, f.req(t, http.MethodGet, prefPath("nginx:1.27"), "")); p != (shellPref{Shell: "sh"}) {
		t.Errorf("preference after sh = %+v, want sh with no command", p)
	}
}

func TestShellAPreferenceIsTheCallersOwnAndAnotherImageHasNone(t *testing.T) {
	t.Parallel()
	f := newShellFix(t, nil)
	f.addEnvUser(t, "olive", f.env.ID, "viewer")
	if rec := f.open(t, "web", `{"shell":"bash","user":"root","cols":80,"rows":24}`); rec.code != http.StatusCreated {
		t.Fatalf("open: %d %s", rec.code, rec.body)
	}
	// Another user, even one who cannot open a shell at all, finds nothing of the admin's.
	rec := f.asUser(t, "olive", http.MethodGet, prefPath("nginx:1.27"), "")
	if rec.code != http.StatusNotFound || rec.ptyp != "shell_preference_not_found" {
		t.Errorf("another user: %d %s (%s), want 404 shell_preference_not_found", rec.code, rec.body, rec.ptyp)
	}
	rec = f.req(t, http.MethodGet, prefPath("postgres:17"), "")
	if rec.code != http.StatusNotFound || rec.ptyp != "shell_preference_not_found" {
		t.Errorf("another image: %d %s (%s), want 404 shell_preference_not_found", rec.code, rec.body, rec.ptyp)
	}
	if rec = f.req(t, http.MethodGet, "/api/me/shell-preferences", ""); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("no image: %d, want 422", rec.code)
	}
	long := make([]byte, 513)
	for i := range long {
		long[i] = 'a'
	}
	if rec = f.req(t, http.MethodGet, prefPath(string(long)), ""); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("image over 512 characters: %d, want 422", rec.code)
	}
}

func TestShellAnAnonymousCallerCannotAskForAPreference(t *testing.T) {
	t.Parallel()
	f := newShellFix(t, nil)
	rec := f.call(t, authReq{method: http.MethodGet, path: prefPath("nginx:1.27")})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d, want 401", rec.Code)
	}
}

func TestShellARefusedOpenRemembersNothing(t *testing.T) {
	t.Parallel()
	f := newShellFix(t, nil)
	// db is stopped, so the open is refused before any exec.
	if rec := f.open(t, "db", `{"shell":"bash","user":"root","cols":80,"rows":24}`); rec.code != http.StatusConflict {
		t.Fatalf("open of a stopped container: %d %s, want 409", rec.code, rec.body)
	}
	if n := f.prefRows(t, "id-admin"); n != 0 {
		t.Errorf("%d preference rows after a refused open, want none", n)
	}
	if rec := f.req(t, http.MethodGet, prefPath("postgres:17"), ""); rec.code != http.StatusNotFound {
		t.Errorf("GET after a refused open: %d, want 404", rec.code)
	}
}

func TestShellOnlyTheNewest200ChoicesOfAUserAreKept(t *testing.T) {
	t.Parallel()
	f := newShellFix(t, nil)
	var adminID string
	if err := f.st.Read.QueryRow(`SELECT id FROM users WHERE username = 'admin'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 205; i++ {
		// All older than the fixture's clock, so the open below is the newest.
		if _, err := f.st.Write.Exec(`INSERT INTO user_shell_prefs (user_id, image, shell, command, run_as, working_dir, updated_at) VALUES (?, ?, 'sh', '', '', '', ?)`,
			adminID, fmt.Sprintf("img-%03d", i), int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if rec := f.open(t, "web", `{"shell":"bash","cols":80,"rows":24}`); rec.code != http.StatusCreated {
		t.Fatalf("open: %d %s", rec.code, rec.body)
	}
	if n := f.prefRows(t, adminID); n != 200 {
		t.Fatalf("%d rows, want 200", n)
	}
	// The 6 oldest went, and the choice just made is among those kept.
	for _, gone := range []string{"img-001", "img-006"} {
		if rec := f.req(t, http.MethodGet, prefPath(gone), ""); rec.code != http.StatusNotFound {
			t.Errorf("%s: %d, want it trimmed", gone, rec.code)
		}
	}
	for _, kept := range []string{"img-007", "img-205", "nginx:1.27"} {
		if rec := f.req(t, http.MethodGet, prefPath(kept), ""); rec.code != http.StatusOK {
			t.Errorf("%s: %d, want it kept", kept, rec.code)
		}
	}
}

func TestShellRemovingAUserRemovesTheirPreferences(t *testing.T) {
	t.Parallel()
	f := newShellFix(t, nil)
	cookie, csrf := f.operatorWithShell(t, "olive")
	rec := f.call(t, authReq{method: http.MethodPost, path: f.openPath("web"), body: openBody, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusCreated {
		t.Fatalf("open: %d %s", rec.Code, rec.Body)
	}
	if n := f.prefRows(t, "id-olive"); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
	for _, q := range []string{
		`DELETE FROM assignment_extras WHERE assignment_id IN (SELECT id FROM role_assignments WHERE principal_id = 'id-olive')`,
		`DELETE FROM role_assignments WHERE principal_id = 'id-olive'`,
		`DELETE FROM users WHERE id = 'id-olive'`,
	} {
		if _, err := f.st.Write.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if n := f.prefRows(t, "id-olive"); n != 0 {
		t.Errorf("%d rows after the user went, want none", n)
	}
}
