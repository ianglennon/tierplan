// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"crypto/x509"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The sources a served certificate can come from. SourceLetsEncrypt is produced
// once TierPlan obtains a certificate itself; it is declared now so the enum the
// contract publishes does not change.
const (
	SourceSelfSigned  = "self_signed"
	SourceCertFiles   = "cert_files"
	SourceLetsEncrypt = "lets_encrypt"
)

// problemHTTPSUnavailable answers a status request when the listener has no
// readable certificate to describe.
const problemHTTPSUnavailable = "https_unavailable"

// ServedCertificate is the certificate the live listener serves: where it came
// from and its parsed public leaf. A nil Leaf means none could be read.
type ServedCertificate struct {
	Source string
	Leaf   *x509.Certificate
	// Trust is whether the certificate is trusted for the public host. It comes from the same read of
	// the listener as Leaf, so the two never describe different certificates.
	Trust CertTrust
}

// CertTrust is the verdict on a served certificate: whether a client that knows only the system roots
// trusts it for the public URL's host. Checked is false when nothing was judged. Reason is one of
// self_signed, unknown_authority, expired, name_mismatch or no_public_url when Verified is false.
type CertTrust struct {
	Checked, Verified bool
	Reason, Host      string
}

// HTTPSDeps are what the HTTPS routes need. Served reads the listener's live
// certificate on every call, so the answer is what a browser receives now.
type HTTPSDeps struct {
	Served func() ServedCertificate
	// CertFiles is true when TIERPLAN_TLS_CERT_FILE and its key fix the certificate.
	CertFiles bool
	// CertFile and KeyFile are the paths of the certificate files (TIERPLAN_TLS_CERT_FILE and
	// TIERPLAN_TLS_KEY_FILE). Only the paths are ever shown, never what the files hold.
	CertFile, KeyFile string
	// AgentTrust is how an agent installed now trusts the server: "pin", "public" or "unverified".
	// Nil means unverified.
	AgentTrust func() string
	Log        *slog.Logger
	// ACME is the Let's Encrypt manager. Nil leaves the status without its acme block and answers the
	// Let's Encrypt routes 503.
	ACME HTTPSACME
	// HSTS is the Strict-Transport-Security policy. Nil leaves the status without its hsts block and
	// answers the HSTS route 503.
	HSTS HTTPSHSTS
	// Jobs is the hub the Let's Encrypt run jobs live in; the run stream follows them.
	Jobs *jobs.Hub
	// ListenPort is the port TierPlan listens on, which the page names when it explains the port-80
	// challenge listener.
	ListenPort int
	// MaxStreamsPerUser, Heartbeat and MaxLifetime shape the run stream like the pull stream's; zero
	// takes the same defaults.
	MaxStreamsPerUser int
	Heartbeat         time.Duration
	MaxLifetime       time.Duration
}

// HTTPSCertificateBody describes the certificate in use. It carries public
// certificate fields only.
type HTTPSCertificateBody struct {
	Source    string         `json:"source" enum:"self_signed,cert_files,lets_encrypt" doc:"Where the certificate comes from: the one TierPlan generated, the files you supplied, or Let's Encrypt"`
	Issuer    string         `json:"issuer" doc:"Who issued it: the issuer's common name, else its organisation, else empty"`
	Names     []string       `json:"names" doc:"Every name the certificate covers, as it carries them: DNS names first, then IP addresses"`
	NotBefore time.Time      `json:"notBefore" doc:"When it becomes valid"`
	NotAfter  time.Time      `json:"notAfter" doc:"When it expires"`
	TestCA    bool           `json:"testCA" doc:"True when the certificate comes from Let's Encrypt but the directory was overridden for testing, so browsers will not trust it"`
	Trust     HTTPSTrustBody `json:"trust" doc:"Whether a client that knows only the system's root certificates trusts this certificate for the public URL's host, which is how an agent without a pin judges it"`
}

// HTTPSTrustBody is the verdict on a certificate.
type HTTPSTrustBody struct {
	Checked  bool   `json:"checked" doc:"False when nothing was judged, because no certificate could be read"`
	Verified bool   `json:"verified" doc:"True when the certificate chain verifies against the system roots for the public URL's host now"`
	Reason   string `json:"reason,omitempty" enum:",self_signed,unknown_authority,expired,name_mismatch,no_public_url" doc:"Why it does not verify: self_signed, unknown_authority (the chain does not reach a trusted root, or the intermediates are missing), expired, name_mismatch, or no_public_url (no public URL is set to check the name against). Empty when it verifies"`
	Host     string `json:"host,omitempty" doc:"The public URL's host the certificate was checked for; empty when there is no public URL"`
}

// HTTPSCertFilesBody is what the page shows about certificate files fixed by the environment. It holds
// the two paths and the verdict, never the contents.
type HTTPSCertFilesBody struct {
	CertPath string         `json:"certPath" doc:"The certificate file, from TIERPLAN_TLS_CERT_FILE"`
	KeyPath  string         `json:"keyPath" doc:"The private key file, from TIERPLAN_TLS_KEY_FILE. Only the path is shown"`
	Trust    HTTPSTrustBody `json:"trust" doc:"Whether the certificate in those files is trusted for the public URL's host"`
}

// HTTPSStatusBody is the HTTPS status.
type HTTPSStatusBody struct {
	Certificate   HTTPSCertificateBody `json:"certificate" doc:"The certificate the listener serves now"`
	CertFiles     bool                 `json:"certFiles" doc:"Whether certificate files from the environment fix the certificate"`
	AgentTrust    string               `json:"agentTrust" enum:"pin,public,unverified" doc:"How an agent installed now trusts this server: pin (it pins the self-signed certificate), public (the certificate verifies against the public CAs, so the agent needs no pin) or unverified (the certificate does not verify for the public URL's host and no pin can stand in for it; an agent installed without a pin keeps the self-signed pin it receives on connecting). The page words it"`
	CertFilesInfo *HTTPSCertFilesBody  `json:"certFilesInfo,omitempty" doc:"The paths of the certificate files and whether the certificate is trusted; present only when certificate files fix the certificate"`
	ACME          *HTTPSACMEBody       `json:"acme,omitempty" doc:"The Let's Encrypt state; left out when this build has no Let's Encrypt support"`
	HSTS          *HTTPSHSTSBody       `json:"hsts,omitempty" doc:"The HSTS state: the switch, the ramp step and when the next one comes, why it is on but not sent, the withdrawal date, when browsers will have forgotten it, and the environment lock. Left out when this build has no HSTS policy"`
}

// HTTPSStatusOutput wraps HTTPSStatusBody for huma.
type HTTPSStatusOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         HTTPSStatusBody
}

// certificateBody builds the public description of a served certificate.
func trustBody(t CertTrust) HTTPSTrustBody {
	return HTTPSTrustBody{Checked: t.Checked, Verified: t.Verified, Reason: t.Reason, Host: t.Host}
}

func certificateBody(s ServedCertificate) HTTPSCertificateBody {
	leaf := s.Leaf
	names := make([]string, 0, len(leaf.DNSNames)+len(leaf.IPAddresses))
	names = append(names, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	issuer := leaf.Issuer.CommonName
	if issuer == "" && len(leaf.Issuer.Organization) > 0 {
		issuer = leaf.Issuer.Organization[0]
	}
	return HTTPSCertificateBody{
		Source: s.Source, Issuer: issuer, Names: names,
		NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(),
		Trust: trustBody(s.Trust),
	}
}

// RegisterHTTPS wires the HTTPS routes.
func RegisterHTTPS(r *httpx.Router, d HTTPSDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getHTTPSStatus",
		Method:      http.MethodGet,
		Path:        "/api/https/status",
		Summary:     "Get the certificate the server serves",
		Description: "The certificate the listener serves right now, read from the live listener and never from a stored copy: " +
			"its source (self_signed, cert_files or lets_encrypt), issuer, every name it covers and its validity, " +
			"and a trust verdict: whether a client that knows only the system roots trusts it for the public URL's host, or why not. " +
			"When certificate files fix the certificate the answer also carries their two paths and the verdict, never their contents. " +
			"Only public certificate fields are returned. The acme block holds the whole Let's Encrypt state the Settings page shows: " +
			"the directory, account, names and the refusals that stop an order, the stored certificate, the next renewal, the last failure and the runs. " +
			"It is read from local state, makes no request to the CA and never carries a key. 503 " + problemHTTPSUnavailable + " when no certificate can be read. Needs settings.read.",
		Tags: []string{"https"},
	}, rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal}, func(ctx context.Context, _ *struct{}) (*HTTPSStatusOutput, error) {
		var served ServedCertificate
		if d.Served != nil {
			served = d.Served()
		}
		if served.Leaf == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemHTTPSUnavailable, "The server has no readable certificate.")
		}
		body := HTTPSStatusBody{Certificate: certificateBody(served), CertFiles: d.CertFiles}
		body.AgentTrust = "unverified"
		if d.AgentTrust != nil {
			body.AgentTrust = d.AgentTrust()
		}
		if d.CertFiles {
			body.CertFilesInfo = &HTTPSCertFilesBody{CertPath: d.CertFile, KeyPath: d.KeyFile, Trust: trustBody(served.Trust)}
		}
		if d.ACME != nil {
			st, err := d.ACME.Status(ctx)
			if err != nil {
				return nil, acmeInternal(d.Log, "read the Let's Encrypt status", err)
			}
			body.ACME = acmeStatusBody(st, d.ListenPort)
			body.Certificate.TestCA = served.Source == SourceLetsEncrypt && st.DirectoryOverridden
		}
		if d.HSTS != nil {
			v, err := d.HSTS.HSTSView(ctx)
			if err != nil {
				return nil, acmeInternal(d.Log, "read the HSTS state", err)
			}
			body.HSTS = hstsBody(v)
		}
		return &HTTPSStatusOutput{CacheControl: "no-store", Body: body}, nil
	})
	// The routes are always registered so the published contract does not depend on the build; a server
	// without a manager answers them 503.
	registerHTTPSACME(r, d)
	registerHTTPSHSTS(r, d)
	registerHTTPSACMERuns(r, d)
	registerHTTPSACMEDNS(r, d)
	registerHTTPSACMEStream(r, d)
}
