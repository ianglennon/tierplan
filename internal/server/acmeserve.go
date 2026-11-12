// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/cloudflare"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/buildinfo"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/vault"
)

// acmeInstaller is how the ACME manager changes the certificate the listener serves (D-01). It is
// the only caller of httpsRuntime.Install besides Build. Every swap writes the mirror the
// healthcheck reads first, so a failed write keeps the certificate that is being served and
// `tierplan healthcheck` never trusts a file that is not what the listener presents.
//
// With TIERPLAN_TLS_CERT_FILE and its key set the installer refuses both swaps: the operator's
// files are served and nothing here may replace them (D-12).
type acmeInstaller struct {
	rt      *httpsRuntime
	self    *certSource
	dataDir string
	files   bool
	log     *slog.Logger

	// cert is the Let's Encrypt pair being served. A handshake loads this pointer and nothing else.
	cert atomic.Pointer[tls.Certificate]
}

var _ acme.Installer = (*acmeInstaller)(nil)

// errACMECertFiles is the refusal when certificate files fix the certificate.
var errACMECertFiles = errors.New("certificate files are configured, so the served certificate is not changed")

// ServeACME serves the Let's Encrypt chain and key (PEM) from now on.
func (i *acmeInstaller) ServeACME(chainPEM, keyPEM []byte) error {
	if i.files {
		return errACMECertFiles
	}
	pair, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("read the Let's Encrypt certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("read the Let's Encrypt certificate: %w", err)
	}
	pair.Leaf = leaf
	// The mirror first: a failed write returns here and the current certificate stays. The
	// self-signed source's lock keeps its name re-sign from writing the mirror between this write
	// and the flag that tells it the mirror is no longer its own.
	i.self.mu.Lock()
	defer i.self.mu.Unlock()
	if err := writeMirror(i.dataDir, chainPEM); err != nil {
		return fmt.Errorf("mirror the Let's Encrypt certificate for the healthcheck: %w", err)
	}
	i.self.ownsMirror.Store(false)
	i.cert.Store(&pair)
	i.rt.Install(tlsKindACME, i.get)
	sum := sha256.Sum256(pair.Certificate[0])
	i.log.Info("serving the Let's Encrypt certificate", "sha256", fmt.Sprintf("%X", sum), "not_after", leaf.NotAfter)
	return nil
}

// ServeSelfSigned serves the install's own self-signed certificate again, with the same key and so
// the same pin it always had (D-16).
func (i *acmeInstaller) ServeSelfSigned() error {
	if i.files {
		return errACMECertFiles
	}
	// Under the source's lock, so a name re-sign in progress has finished and the certificate read
	// here is the one it serves.
	i.self.mu.Lock()
	defer i.self.mu.Unlock()
	cur := i.self.cert.Load()
	if cur == nil || len(cur.Certificate) == 0 {
		return errors.New("the self-signed certificate is not available")
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cur.Certificate[0]})
	if err := writeMirror(i.dataDir, certPEM); err != nil {
		return fmt.Errorf("mirror the self-signed certificate for the healthcheck: %w", err)
	}
	i.self.ownsMirror.Store(true)
	i.rt.Install(tlsKindSelfSigned, i.self.GetCertificate)
	i.log.Info("serving the self-signed certificate again")
	return nil
}

// get answers a handshake with the Let's Encrypt pair: a pointer load, no clock, lock or disk.
func (i *acmeInstaller) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := i.cert.Load()
	if c == nil {
		return nil, errors.New("no Let's Encrypt certificate is loaded")
	}
	return c, nil
}

// errACMEOffUnderHSTS is the refusal at start when TIERPLAN_ACME=off would take a Let's Encrypt certificate
// out of service while HSTS is on.
var errACMEOffUnderHSTS = errors.New("TIERPLAN_ACME=off with Let's Encrypt in use while HSTS is on would lock browsers out: " +
	"the self-signed certificate would be served to browsers that hold the policy, which they refuse with no way to click through. " +
	"Turn HSTS off in Settings (or set TIERPLAN_HSTS=off), wait until browsers have forgotten the policy, then set TIERPLAN_ACME=off")

// refuseACMEOffUnderHSTS applies at start the rule D-26 applies to Stop and config.Load applies to
// TIERPLAN_HSTS=on: env must not create the lockout either. TIERPLAN_ACME=off makes the manager serve the
// self-signed certificate whatever is stored, so it is refused while Let's Encrypt is in use (the stored
// mode is active) and the effective HSTS setting is on, which is TIERPLAN_HSTS when set and the stored
// toggle otherwise. Certificate files win over Let's Encrypt and are served either way, so they do not
// count. Config checks the variables against each other; only here is the stored setting known.
func refuseACMEOffUnderHSTS(ctx context.Context, cfg config.Config, st *store.Store, acmeStore *acme.Store) error {
	if cfg.ACME.Enabled == nil || *cfg.ACME.Enabled || cfg.TLSCertFile != "" {
		return nil
	}
	hstsOn := false
	if cfg.ACME.HSTS != nil {
		hstsOn = *cfg.ACME.HSTS
	} else {
		row, err := st.RQ.GetHTTPSState(ctx)
		if err != nil {
			return fmt.Errorf("read the HSTS setting: %w", err)
		}
		hstsOn = row.HstsEnabled != 0
	}
	if !hstsOn {
		return nil
	}
	state, err := acmeStore.State(ctx)
	if err != nil {
		return err
	}
	if state.Mode != acme.ModeActive {
		return nil
	}
	return errACMEOffUnderHSTS
}

// acmeWiring is what Build needs back from wireACME.
type acmeWiring struct {
	mgr        *acme.Manager
	challenges *acme.Challenges
}

// wireACME builds the ACME manager over the server's parts. It does not start it: Build calls Start
// before the server listens and runs the scheduler in the background. A bad directory setting is an
// error that stops start-up.
func wireACME(cfg config.Config, log *slog.Logger, st *store.Store, vlt *vault.Vault, inst *instance.Instance,
	jobHub *jobs.Hub, tunnelHub *tunnel.Hub, rt *httpsRuntime, self *certSource, clock stackops.Clock, dnsDeps *acme.DNSDeps,
	hsts *hstsPolicy, listening <-chan struct{}) (acmeWiring, error) {
	dir, err := acme.NewDirectory(cfg)
	if err != nil {
		return acmeWiring{}, err
	}
	if dir.Overridden {
		log.Warn("TIERPLAN_ACME_DIRECTORY_URL is set: Let's Encrypt requests go to this directory instead of the real one. This is for testing only and unsupported",
			"directory", dir.URL)
	}
	if clock == nil {
		clock = stackops.RealClock()
	}
	acmeStore := acme.NewStore(st, vlt)
	client := acme.NewClient(dir, acmeStore, clock, "TierPlan/"+buildinfo.Get().Version)
	challenges := acme.NewChallenges()
	installer := &acmeInstaller{
		rt: rt, self: self, dataDir: cfg.DataDir, files: cfg.TLSCertFile != "", log: log.With("component", "acme"),
	}
	// The DNS provider factory carries the bearer token, so it is built here and no setting or
	// environment variable can point it elsewhere (WithACMEDNS is a Go option for tests).
	var dnsD acme.DNSDeps
	if dnsDeps != nil {
		dnsD = *dnsDeps
	}
	if dnsD.Providers == nil {
		dnsD.Providers = map[string]dns.Factory{
			"cloudflare": cloudflare.New(cloudflare.Options{UserAgent: "TierPlan/" + buildinfo.Get().Version}),
		}
	}
	mgr := acme.NewManager(acme.Deps{
		Store: acmeStore, Client: client, Challenges: challenges, Jobs: jobHub, Installer: installer, Clock: clock,
		PublicURL: inst.PublicURL, AgentURL: inst.AgentURL, BasePath: inst.BasePath,
		DialHosts: func() []string {
			var hosts []string
			for _, s := range tunnelHub.Sessions() {
				hosts = append(hosts, s.DialHost)
			}
			return hosts
		},
		CertFiles: cfg.TLSCertFile != "", Log: log, DNS: dnsD,
		// The outcome of every certificate run is a system:acme row in the audit log (Phase 17, SET-04). The writer is
		// the server's own over the same database; several writers may append to one chain.
		Audit: audit.NewWriter(st, vlt, log, nil).Record,
		// TIERPLAN_ACME* decide at the manager's decision sites and are never saved; the start-time issue
		// waits for the listener, because the CA validates through it (D-07).
		Env: cfg.ACME, Ready: listening,
		// Stop using Let's Encrypt is refused while HSTS is on, because the self-signed certificate that
		// would replace it is not one browsers holding a policy accept (D-26).
		HSTSOn: hsts.Enabled,
		// The check and the swap in Stop happen with the toggle held, so a toggle-on cannot slip in between
		// them. Nothing here is on the request path.
		HSTSHold: hsts.WithToggleLocked,
	})
	return acmeWiring{mgr: mgr, challenges: challenges}, nil
}
