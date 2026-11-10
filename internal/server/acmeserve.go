// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/ianglennon/tierplan/internal/acme"
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

// acmeWiring is what Build needs back from wireACME.
type acmeWiring struct {
	mgr        *acme.Manager
	challenges *acme.Challenges
}

// wireACME builds the ACME manager over the server's parts. It does not start it: Build calls Start
// before the server listens and runs the scheduler in the background. A bad directory setting is an
// error that stops start-up.
func wireACME(cfg config.Config, log *slog.Logger, st *store.Store, vlt *vault.Vault, inst *instance.Instance,
	jobHub *jobs.Hub, tunnelHub *tunnel.Hub, rt *httpsRuntime, self *certSource, clock stackops.Clock) (acmeWiring, error) {
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
		CertFiles: cfg.TLSCertFile != "", Log: log,
	})
	return acmeWiring{mgr: mgr, challenges: challenges}, nil
}
