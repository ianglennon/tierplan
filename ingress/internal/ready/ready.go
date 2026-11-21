// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ready is the supervisor's readiness endpoint and the probe behind the image's HEALTHCHECK
// (D-17, D-18). The supervisor owns readiness; Traefik's ping is liveness only. A 200 from /readyz means
// "a bundle is on disk and Traefik answers its ping", not "Traefik has loaded that bundle": the image runs
// Traefik without an API, so the supervisor cannot ask it which configuration it holds. Readiness never
// depends on reaching TierPlan (design correction 8), so a node with a cached bundle stays ready while
// TierPlan is down and a node with none stays unready however the network looks.
package ready

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// PingTimeout is how long the readiness check waits for Traefik's ping (D-17). It is below ProbeTimeout,
// which is below Docker's own health-check timeout, so a hung Traefik makes the endpoint answer 503
// instead of making its caller time out.
const PingTimeout = 2 * time.Second

// State records whether the node has applied a bundle. The supervisor sets it once a bundle is on disk
// and Traefik has been given it, and clears it if that stops being true.
type State struct {
	applied atomic.Bool
}

// SetApplied records whether a bundle is applied.
func (s *State) SetApplied(v bool) { s.applied.Store(v) }

// Applied reports whether a bundle is applied.
func (s *State) Applied() bool { return s.applied.Load() }

// Pinger reports whether Traefik's ping answered 200. It is also what the supervisor reports as traefik_ok in
// its periodic health message.
type Pinger func(ctx context.Context) bool

// DefaultPingURL is Traefik's ping URL on this node.
func DefaultPingURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(ingressbundle.PingPort) + ingressbundle.PingPath
}

// NewPinger returns a Pinger that GETs url with client, bounded by PingTimeout. It is true only for a 200:
// Traefik answers 503 once it is terminating, and a refused connection or a timeout is not an answer.
func NewPinger(client *http.Client, url string) Pinger {
	return func(ctx context.Context) bool {
		ctx, cancel := context.WithTimeout(ctx, PingTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
}

// Handler answers the readiness path with a bare status code and nothing else (D-17): 200 when a bundle is
// applied and ping answers, 503 otherwise, 404 for any other path and 405 for any method but GET and HEAD.
// The decision is made per request. It is a plain handler rather than a request multiplexer, which would redirect
// a path that only cleans to /readyz and write a text body on a 404.
func Handler(st *State, ping Pinger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ingressbundle.ReadyPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if st.Applied() && ping(r.Context()) {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
}
