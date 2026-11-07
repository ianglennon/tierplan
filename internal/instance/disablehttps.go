// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	neturl "net/url"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// ErrTLSSetByEnv is returned by DisableHTTPS when TIERPLAN_TLS_SELF_SIGNED or
// TIERPLAN_TLS_CERT_FILE sets TLS (D-18). Such an install serves HTTPS whatever is
// stored, and an http public URL there would lock the admin out: https sign-in
// fails the Origin check and plain HTTP is refused (D-14). Nothing is written.
var ErrTLSSetByEnv = errors.New("TLS is set by an environment variable")

// targetSettings is the audit target of every instance settings change; the API's
// settings handler uses the same value.
const targetSettings = "settings"

// DisableResult is what DisableHTTPS did and what the operator should be told.
// Changes holds one line per setting it changed. Notes holds the rest, in the order
// to print them: with ErrTLSSetByEnv they are the refusal, otherwise the outcome and
// how to correct it.
type DisableResult struct {
	Changes []string
	Notes   []string
	// From and To are the public URL before and after, set only when it changed.
	From, To string
}

// DisableHTTPS resets the stored HTTPS state so an operator locked out of the UI
// can get back in: the stored public URL becomes http with an explicit port, in one
// write transaction with the settings.updated audit row by system:cli (D-06, D-09).
// The running server applies it from the settings clock (D-05).
//
// The checks run in a fixed order (D-18). First, TIERPLAN_PUBLIC_URL fixes the URL:
// ErrPublicURLLocked, before the database is touched (D-07). Second, an environment
// variable sets TLS: ErrTLSSetByEnv with the refusal in Notes, nothing written.
// Third, the flip. The port is the stored URL's explicit port, else the port of
// cfg.Listen, never an implicit 80 or 443 (D-15). A second run, or no stored URL,
// reports "nothing to change" and writes nothing. The stored URL is read inside the
// write transaction, so two runs at once change it once.
func DisableHTTPS(ctx context.Context, st *store.Store, cfg config.Config, w *audit.Writer) (DisableResult, error) {
	if cfg.PublicURL != "" {
		return DisableResult{}, ErrPublicURLLocked
	}
	if cfg.TLSSelfSigned || cfg.TLSCertFile != "" {
		return refuseEnvTLS(ctx, st, cfg)
	}

	tx, err := st.Write.BeginTx(ctx, nil)
	if err != nil {
		return DisableResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed

	qt := st.Q.WithTx(tx)
	stored, err := qt.GetSetting(ctx, SettingPublicURL)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return DisableResult{}, fmt.Errorf("read setting %s: %w", SettingPublicURL, err)
	}

	var res DisableResult
	// One step per stored HTTPS state; later phases add their resets here, in the
	// same transaction and the same Changes list (D-06).
	details := map[string]string{}
	if err := disablePublicURL(ctx, qt, cfg, stored, &res, details); err != nil {
		return DisableResult{}, err
	}
	if len(res.Changes) == 0 {
		return res, nil
	}

	details["via"] = "cli"
	if err := w.RecordTx(ctx, tx, audit.Entry{
		Actor:      audit.SystemCLI,
		Action:     audit.ActionSettingsUpdated,
		TargetType: targetSettings,
		TargetID:   targetSettings,
		Result:     audit.ResultSuccess,
		Details:    details,
	}); err != nil {
		return DisableResult{}, fmt.Errorf("audit the change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DisableResult{}, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

// disablePublicURL turns a stored https public URL into an http one with an explicit
// port, and records what it did in res and details. When there is nothing to change
// it only adds the reason to res.Notes.
func disablePublicURL(ctx context.Context, qt *sqlc.Queries, cfg config.Config, stored string, res *DisableResult, details map[string]string) error {
	if stored == "" {
		res.Notes = append(res.Notes, "nothing to change: no public URL is stored")
		return nil
	}
	u, err := neturl.Parse(stored)
	if err != nil || u.Host == "" {
		return fmt.Errorf("the stored public URL %q cannot be read: %v", stored, err)
	}
	if u.Scheme != "https" {
		res.Notes = append(res.Notes, "nothing to change: the public URL is already "+stored)
		return nil
	}

	port, fromStored, err := publicURLPort(u, cfg.Listen)
	if err != nil {
		return err
	}
	to, err := config.NormalisePublicURL("http://" + net.JoinHostPort(u.Hostname(), port) + u.EscapedPath())
	if err != nil {
		return fmt.Errorf("build the http public URL: %w", err)
	}
	if err := qt.UpsertSetting(ctx, sqlc.UpsertSettingParams{Key: SettingPublicURL, Value: to, UpdatedAt: time.Now().UTC().UnixNano()}); err != nil {
		return fmt.Errorf("store %s: %w", SettingPublicURL, err)
	}

	source := "the port of TIERPLAN_LISTEN"
	if fromStored {
		source = "the port in the stored URL"
	}
	res.From, res.To = stored, to
	details["public_url_from"], details["public_url_to"] = stored, to
	res.Changes = append(res.Changes, "changed public URL: "+stored+" -> "+to)
	res.Notes = append(res.Notes,
		fmt.Sprintf("the public URL is now %s; port %s is %s. If the container publishes TierPlan on another port, "+
			"set TIERPLAN_PUBLIC_URL=http://%s%s on the container and restart it.",
			to, port, source, net.JoinHostPort(u.Hostname(), "<port>"), u.EscapedPath()),
		"Everyone is signed out once: the session cookie follows the public URL's scheme.",
		"The running server applies this within a few seconds; no restart is needed.",
	)
	return nil
}

// publicURLPort is the port an http public URL built from u must carry (D-15): the
// explicit port u has, else the port of the listen address. fromStored says which.
func publicURLPort(u *neturl.URL, listen string) (port string, fromStored bool, err error) {
	if p := u.Port(); p != "" {
		return p, true, nil
	}
	_, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "", false, fmt.Errorf("cannot read the port from TIERPLAN_LISTEN %q: %w", listen, err)
	}
	if p == "" || p == "0" {
		return "", false, fmt.Errorf("TIERPLAN_LISTEN %q has no fixed port to put in the public URL: set TIERPLAN_LISTEN to the port TierPlan is published on", listen)
	}
	return p, false, nil
}

// refuseEnvTLS is the D-18 refusal: TLS is set by an environment variable, so the
// stored URL is left alone and the operator is told the https address that works and
// how to get plain HTTP. It reads the stored URL without a write transaction.
func refuseEnvTLS(ctx context.Context, st *store.Store, cfg config.Config) (DisableResult, error) {
	variable := "TIERPLAN_TLS_CERT_FILE"
	if cfg.TLSSelfSigned {
		variable = "TIERPLAN_TLS_SELF_SIGNED"
	}

	stored, err := st.RQ.GetSetting(ctx, SettingPublicURL)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return DisableResult{}, fmt.Errorf("read setting %s: %w", SettingPublicURL, err)
	}
	var u *neturl.URL
	if stored != "" {
		if p, perr := neturl.Parse(stored); perr == nil && p.Host != "" {
			u = p
		}
	}

	// The exact origin the sign-in Origin check accepts is the stored one, when it is
	// https. Otherwise build it from the stored host (or the request's own, which is
	// all that is known before one is stored) and the D-15 port.
	var wayIn string
	switch {
	case u != nil && u.Scheme == "https":
		wayIn = stored
	default:
		host, path, port := "<host>", "", "<port>"
		if u != nil {
			host, path = u.Hostname(), u.EscapedPath()
		}
		if u != nil && u.Port() != "" {
			port = u.Port()
		} else if _, p, err := net.SplitHostPort(cfg.Listen); err == nil && p != "" {
			port = p
		}
		wayIn = "https://" + net.JoinHostPort(host, port) + path
	}

	notes := []string{
		"TLS is set by " + variable + ", so TierPlan keeps serving HTTPS on this port; nothing was changed.",
		"The way in is " + wayIn + ". To serve plain HTTP instead, remove " + variable + " from the container and restart it.",
	}
	if u != nil && u.Scheme == "http" {
		notes = append(notes, "The stored public URL is "+stored+", so sign-in over https:// is refused until the public URL is https: "+
			"set TIERPLAN_PUBLIC_URL="+wayIn+" on the container and restart it, or remove "+variable+" and restart it.")
	}
	return DisableResult{Notes: notes}, ErrTLSSetByEnv
}
