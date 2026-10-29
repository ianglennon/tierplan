// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import (
	"errors"
	"net/http"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/ianglennon/bayplan/internal/registry"
)

// ErrProtocolRefused is what a call over a protocol Bayplan does not offer fails with: http (it
// would send a token in clear text), git, file (it would read the server's own filesystem) and a
// redirect to any of them. The connection is never made.
var ErrProtocolRefused = errors.New("gitx: this protocol is not allowed")

// maxRedirects is how many redirects one request may follow.
const maxRedirects = 5

var installOnce sync.Once

// InstallTransport installs, once per process, the HTTPS transport every go-git call uses: it
// dials through p (and through the policy of a call, see registry.WithEgressPolicy), verifies
// certificates, follows at most five redirects and only to https. go-git keeps its protocols in a
// package variable, so installing a protocol per request would race with every concurrent call;
// a second call to InstallTransport is a no-op. The protocols http, git and file are replaced by
// one that always refuses. The ssh configuration of the server's user is never read.
func InstallTransport(p registry.EgressPolicy) {
	installOnce.Do(func() {
		hc := &http.Client{Transport: p.GitTransport(), CheckRedirect: checkRedirect}
		client.InstallProtocol("https", githttp.NewClient(hc))
		for _, scheme := range []string{"http", "git", "file"} {
			client.InstallProtocol(scheme, refusedTransport{})
		}
		gitssh.DefaultSSHConfig = nil
		registerSSHDialer()
	})
}

// checkRedirect lets a request follow up to maxRedirects redirects, and only to https.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("gitx: too many redirects")
	}
	if req.URL.Scheme != "https" {
		return ErrProtocolRefused
	}
	return nil
}

// refusedTransport is the transport of every protocol Bayplan does not offer.
type refusedTransport struct{}

func (refusedTransport) NewUploadPackSession(*transport.Endpoint, transport.AuthMethod) (transport.UploadPackSession, error) {
	return nil, ErrProtocolRefused
}

func (refusedTransport) NewReceivePackSession(*transport.Endpoint, transport.AuthMethod) (transport.ReceivePackSession, error) {
	return nil, ErrProtocolRefused
}
