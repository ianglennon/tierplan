// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package connect is the agent's one connection to TierPlan: dial out over
// WebSocket, authenticate inside it, then serve the server's Docker calls over
// one multiplexed session. No port on the host is opened (D-18).
//
// The agent enrols once, with the join token, and keeps the key it generated and
// the certificate it was issued in its state directory (package state). Every
// later connection, after a restart or a dropped link, says hello with that
// certificate and signs the server's challenge with that key; the token is not
// needed again (D-01, D-03). After the welcome the agent opens a control stream
// and heartbeats on it (D-41). When the connection ends it reconnects with
// jittered exponential backoff and never gives up. The server's certificate is
// verified by the system roots or by a pin (tls.go).
//
// The agent keeps its certificate fresh by asking for a new one over the control
// stream (renew.go), and keeps the address it dials and the server's pins current
// from what the server tells it (D-19): the newest address is dialed first and the
// previous one is kept in the list for 30 days. The server accepts only its current
// host and port, so the previous address helps only if an administrator changes it
// back (or if the change kept the host and port).
package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ianglennon/tierplan/agent/internal/facts"
	"github.com/ianglennon/tierplan/agent/internal/hostnet"
	"github.com/ianglennon/tierplan/agent/internal/proxy"
	"github.com/ianglennon/tierplan/agent/internal/selfmaint"
	"github.com/ianglennon/tierplan/agent/internal/state"
	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/wire"
)

// Config is what Run needs.
type Config struct {
	// URL is the server's agent endpoint, for example wss://tierplan.example/agent/connect.
	URL string
	// JoinToken is presented once, in the handshake. It is never logged.
	JoinToken string
	// SocketPath is the Docker socket. Empty means /var/run/docker.sock.
	SocketPath string
	// Version is the agent's version, reported in the handshake.
	Version string
	// Log receives one line per event. Nil means slog.Default().
	Log *slog.Logger
	// HTTPClient dials the server. Nil means the agent's own client: system
	// roots (honouring SSL_CERT_FILE), the pins below and those stored from
	// earlier welcomes, and the proxy from the environment (tls.go). A caller
	// that supplies one takes over verification; the agent binary never does.
	HTTPClient *http.Client
	// InstallPins are the server certificate pins of TIERPLAN_AGENT_PIN: the
	// pin of TierPlan's own certificate, or of a certificate authority in the
	// chain of a private-CA proxy.
	InstallPins []string
	// StateDir holds the agent's identity (key, certificate, IDs). Empty keeps
	// the identity in memory only, so a restart enrols again.
	StateDir string
	// BackoffMin and BackoffMax bound the reconnect delay. Zero means 1 s and 60 s.
	BackoffMin, BackoffMax time.Duration
	// ReadOnly makes the agent refuse every request that is not GET or HEAD, and
	// SelfUpdate says whether the host lets the agent update itself. Both are
	// reported in the handshake so the UI can show them (D-28).
	ReadOnly, SelfUpdate bool
	// AllowPrivilegedServices is TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES.
	AllowPrivilegedServices bool
	// Maint answers the server's update and uninstall messages (D-24, D-26). Nil
	// answers every such message self_update_disabled.
	Maint selfmaint.Service
	// HostRouteFile is the read-only mount of the host's /proc/1/net/route, which
	// the agent reads when the server asks for the host's routes. Empty means
	// hostnet.DefaultRouteFile.
	HostRouteFile string
}

// runEnv is what a test replaces: the wait between attempts, the jitter and the
// clock the heartbeat reports.
type runEnv struct {
	sleep func(context.Context, time.Duration) error
	rnd   func() float64
	now   func() time.Time
	// roots replaces the system roots in a test; nil in production.
	roots *x509.CertPool
}

// RefusedError is returned when the server answers the handshake with a refusal.
type RefusedError struct {
	Code, Detail string
	// RetryAfter is how long the server asked the agent to wait, when it did.
	RetryAfter time.Duration
}

func (e *RefusedError) Error() string {
	if e.Detail == "" {
		return "the server refused this agent: " + e.Code
	}
	return "the server refused this agent: " + e.Code + ": " + e.Detail
}

const (
	defaultSocket = "/var/run/docker.sock"
	// kindTimeout bounds the wait for the kind byte of a new stream.
	kindTimeout = 10 * time.Second
	// hostNetWriteTimeout bounds writing the host-net document to the server.
	hostNetWriteTimeout = 5 * time.Second
	// maxRetryAfter bounds how long the agent will wait because a server said so.
	maxRetryAfter = 5 * time.Minute
)

// muxConfig is the yamux configuration of RESEARCH Pattern 1: detection of a
// silent black hole in at most about 20 s, inside the D-41 window of 30 s.
func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = 10 * time.Second
	c.ConnectionWriteTimeout = 10 * time.Second
	c.StreamCloseTimeout = 30 * time.Second
	c.StreamOpenTimeout = 10 * time.Second
	c.AcceptBacklog = 64
	c.LogOutput = io.Discard
	return c
}

// Run connects and serves until ctx ends. It returns nil only when ctx ended: a
// dropped connection or a refusal is retried with backoff. An agent that is
// refused revoked and has a join token drops its identity and enrols again as a
// new agent (D-10). One refused auth_rejected that was started with a join token
// it has no record of being accepted with, as after TierPlan was reinstalled and
// the agent with it, sets its identity aside and enrols with the token; it drops
// the old identity only once that enrolment is accepted, and takes it back if it
// is not. A blocked host waits at the maximum backoff until an administrator
// unblocks it. It returns an error for a configuration the agent cannot work with,
// and for a refusal of the very first enrolment (a *RefusedError) other than
// blocked or host_already_connected, because a wrong token does not fix itself. An
// enrolment made with an identity set aside is not a first one: its refusal gives the
// identity back. A host whose other agent is connected waits at the maximum backoff
// too, at its first enrolment as at any other.
func Run(ctx context.Context, cfg Config) error {
	return runWith(ctx, cfg, runEnv{})
}

// errNoCredentials means the agent has neither an identity nor a join token.
var errNoCredentials = errors.New("the agent has no identity yet and no join token: set TIERPLAN_AGENT_JOIN_TOKEN_FILE or TIERPLAN_AGENT_JOIN_TOKEN")

// runner is the agent's state across connection attempts.
type runner struct {
	cfg Config
	env runEnv
	log *slog.Logger

	// cur is the identity, once the agent has one. It is also kept in memory when
	// there is no state directory, so a reconnect in the same process never
	// enrols a second agent. mu guards it, because the control reader of a session
	// that is ending may still be saving a renewed certificate while the next
	// connection begins.
	mu   sync.Mutex
	cur  state.State
	have bool
	// enrolHash is the state.TokenHash of the join token cur was enrolled with, or
	// that the agent held when TierPlan last accepted cur's hello; empty when that is
	// not known (an identity enrolled before the agent kept the record). mu guards it
	// with cur.
	enrolHash string
	// triedHash is the state.TokenHash of a join token the agent tried with cur set
	// aside and that TierPlan refused auth_rejected, or empty. newToken does not try
	// that token again for cur, so an agent whose hello keeps being refused for a cause
	// a fresh enrolment cannot fix does not set its identity aside every minute,
	// forever (review 17-2 WR-04). It is kept next to the identity (tried-token.json)
	// and goes with it. mu guards it with cur.
	triedHash string
	// triedSaid keeps the agent from saying more than once that it will not try its
	// join token again.
	triedSaid bool
	// forceEnrol makes the next attempt enrol although an identity exists: the
	// certificate expired and a join token is at hand.
	forceEnrol bool
	// rejoinedAt is when the agent last dropped an identity, or set one aside, to
	// enrol again. It keeps an agent that is refused again at once from enrolling a
	// new agent every few seconds.
	rejoinedAt time.Time
	// stashed is the identity the agent set aside to try its join token after a hello
	// was refused auth_rejected (see setAside), nil at every other time. It is taken
	// back when that enrolment does not end in a welcome, and dropped when it does.
	// Only the run loop touches it.
	stashed *stashedIdentity
	// quietStash keeps a set-aside, refused and taken-back cycle that repeats every
	// BackoffMax from logging each round. A welcome resets it.
	quietStash bool
	// lastCode is the refusal code last logged, so a refusal that repeats every
	// minute is said once.
	lastCode string
	// pins are the server pins kept from welcomes and pushes, with their overlap
	// deadlines. urls are the addresses to dial, newest first, with the overlap
	// deadline of every one but the newest. Both are guarded by mu, because a push
	// arrives on the control reader while the next connection is being made.
	pins []state.PinEntry
	urls []state.URLEntry
}

// target is one address to dial: the URL, the audience the handshake signs (the
// host[:port] dialed) and the host name the server's certificate must be valid for.
type target struct{ url, aud, host string }

// newTarget reads an address the agent is going to dial. It refuses what
// state.DialableURL refuses, so nothing reaches the dialer around the filters at
// load: only a wss:// address is ever dialed (D-09, T-13.1-39).
func newTarget(raw string) (target, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "wss" {
		return target{}, fmt.Errorf("%q is not a wss:// address", raw)
	}
	if !state.DialableURL(raw) {
		return target{}, fmt.Errorf("%s is not an address the agent may dial", u.Host)
	}
	aud, err := authn.Audience(raw)
	if err != nil {
		return target{}, err
	}
	return target{url: raw, aud: aud, host: u.Hostname()}, nil
}

func runWith(ctx context.Context, cfg Config, env runEnv) error {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = defaultSocket
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = defaultBackoffMin
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = defaultBackoffMax
	}
	if env.sleep == nil {
		env.sleep = sleepCtx
	}
	if env.now == nil {
		env.now = time.Now
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "wss" || !state.DialableURL(cfg.URL) {
		return fmt.Errorf("TIERPLAN_AGENT_URL must be a wss:// address: the agent connects to TierPlan only over TLS: %q", cfg.URL)
	}
	if _, err := authn.Audience(cfg.URL); err != nil {
		return fmt.Errorf("TIERPLAN_AGENT_URL: %w", err)
	}

	// Until the server says otherwise the address from the environment is the only
	// one. Once it has pushed one, the stored list rules and the environment's
	// address is just one more entry in it, kept for 30 days as an undo window in
	// case the change is reverted. The exception is an operator who starts the agent
	// with a TIERPLAN_AGENT_URL that differs from the one it last started with: they
	// mean it, most often to bring home an agent that was offline while TierPlan's
	// agent address changed, so it is dialed first (G-02-4b).
	r := &runner{cfg: cfg, env: env, log: log, urls: []state.URLEntry{{URL: cfg.URL}}}
	if cfg.StateDir != "" {
		// An identity set aside by a run that stopped before its enrolment was
		// answered is put back first, so the agent never starts as if it had none.
		if restored, err := state.ResolveStash(cfg.StateDir); err != nil {
			log.Error("the agent could not settle the identity it had set aside in its state directory; fix the permissions of the directory", "dir", cfg.StateDir, "error", err)
		} else if restored {
			log.Info("the agent stopped while it was trying its join token with its identity set aside; it took the identity back")
		}
		cur, ok, err := state.Load(cfg.StateDir)
		if err != nil {
			return err
		}
		r.cur, r.have = cur, ok
		if ok {
			// A record that cannot be read reads as unknown: on an auth_rejected hello
			// the agent then enrols again with the token it has, which is what it does
			// for an identity enrolled before the record existed.
			hash, known, err := state.LoadEnrolToken(cfg.StateDir)
			if err != nil {
				log.Warn("the agent could not read which join token it enrolled with; it treats it as unknown", "error", err)
			} else if known {
				r.enrolHash = hash
			}
			// A record that cannot be read reads as none: the agent then tries its token
			// once more, which costs one refused enrolment.
			tried, known, err := state.LoadTriedToken(cfg.StateDir, cur.AgentID)
			if err != nil {
				log.Warn("the agent could not read which join token TierPlan refused for its identity; it treats it as not tried", "error", err)
			} else if known {
				r.triedHash = tried
			}
		}
		// A pins file that cannot be read leaves the agent with fewer pins, which
		// can only make it refuse more, never trust more.
		if r.pins, err = state.LoadPins(cfg.StateDir); err != nil {
			log.Warn("the agent could not read its stored server pins; it will trust only the install pin and the system roots", "error", err)
			r.pins = nil
		}
		// env-url.json is only compared with TIERPLAN_AGENT_URL, to see whether the
		// operator changed it. It is never dialed, so a plain address recorded there
		// under other settings cannot make the agent dial anything.
		last, known, err := state.LoadEnvURL(cfg.StateDir)
		if err != nil {
			log.Warn("the agent could not read the TIERPLAN_AGENT_URL it last started with; it treats TIERPLAN_AGENT_URL as unchanged", "error", err)
			known = false
		}
		stored, err := state.LoadURLs(cfg.StateDir)
		if err != nil {
			log.Warn("the agent could not read the addresses the server gave it; it will dial TIERPLAN_AGENT_URL", "error", err)
		} else {
			// A stored plain ws:// address was learned or written by an earlier
			// version. It is never dialed (D-09, T-13.1-39).
			if kept := slices.DeleteFunc(slices.Clone(stored), func(e state.URLEntry) bool {
				return !state.DialableURL(e.URL)
			}); len(kept) != len(stored) {
				log.Warn("the agent dropped stored addresses that are not wss://; it dials only over TLS",
					"count", len(stored)-len(kept))
				stored = kept
			}
			urls, changed := state.StartURLs(stored, cfg.URL, last, known, env.now())
			r.urls = urls
			if changed {
				log.Info("TIERPLAN_AGENT_URL changed since the agent last started; it dials the new address first", "address", cfg.URL)
				if err := state.SaveURLs(cfg.StateDir, urls); err != nil {
					log.Warn("the agent could not save its new address list", "error", err)
				}
			}
		}
		if !known || last != cfg.URL {
			if err := state.SaveEnvURL(cfg.StateDir, cfg.URL); err != nil {
				log.Warn("the agent could not record the TIERPLAN_AGENT_URL it started with; a later change of it may not be noticed", "error", err)
			}
		}
	}
	if !r.have && cfg.JoinToken == "" {
		return errNoCredentials
	}

	failures := 0
	for {
		welcomed, err := r.once(ctx)
		if ctx.Err() != nil {
			// Stopping in the middle of an enrolment with the identity set aside puts
			// it back, so the next start finds it where it belongs.
			if r.stashed != nil {
				r.takeBack(err, true)
			}
			return nil
		}
		if errors.Is(err, errNoCredentials) {
			return err
		}
		if welcomed {
			// The connection worked: whatever ended it is a first failure, and a
			// refusal after it is news again.
			failures = 0
			r.lastCode = ""
			r.quietStash = false
		}
		var wait time.Duration
		var refused *RefusedError
		if r.stashed != nil {
			// The enrolment the agent tried with its identity set aside did not end in
			// a welcome (accept drops the stash when it does): refused for any reason,
			// or not answered at all. The agent takes its identity back and keeps it,
			// and waits the maximum before it tries again. It never stops for this:
			// the refusal is not of a first enrolment, the agent has an identity.
			wait = r.takeBack(err, false)
		} else if errors.As(err, &refused) {
			if !r.have && !transient(refused.Code) && !waitsAtMax(refused.Code) {
				// The very first enrolment was refused for a reason that will not
				// pass by itself, most often a wrong or disabled join token. Say so
				// and stop, so the operator sees the failure where they installed
				// the agent. An enrolled agent never stops for a refusal. Nor does
				// a blocked host, which is also what a revoked agent meets when it
				// enrols again: an administrator can lift the block, and exiting
				// would only make the restart policy retry faster than the backoff.
				// Nor does a host whose other agent is connected now: that one may
				// be a container that is stopping, or a duplicate the operator
				// removes, and either way this one enrols once it is gone.
				return refused
			}
			wait = r.refused(refused, &failures)
		} else if untrusted := asUntrusted(err); untrusted != nil {
			// The server could not be verified, so nothing was sent to it. Said
			// once, with the way out; a server that stays untrusted is retried
			// quietly with the usual backoff until its certificate changes.
			failures++
			wait = backoff(failures, cfg.BackoffMin, cfg.BackoffMax, env.rnd)
			if r.lastCode != untrustedCode {
				r.lastCode = untrustedCode
				log.Warn("the server's certificate could not be verified, so the agent did not connect",
					"code", untrustedCode, "detail", untrusted.Detail, "hint", untrustedHint, "retry_in", wait.Round(time.Millisecond).String())
			}
		} else {
			failures++
			wait = backoff(failures, cfg.BackoffMin, cfg.BackoffMax, env.rnd)
			log.Warn("the connection to TierPlan ended; will reconnect", "error", err, "retry_in", wait.Round(time.Millisecond).String())
		}
		if err := env.sleep(ctx, wait); err != nil {
			return nil
		}
	}
}

// refused decides how long to wait after a refusal, and says why once.
//
// A revoked agent with a join token drops its identity and enrols again at once,
// as a new agent (see rejoin). An agent the server does not know (unknown_agent, as
// after a restore from a backup older than the agent) is handled the same way: its
// identity is one the server will never accept, and only a new enrolment helps.
// A hello refused auth_rejected makes the agent try its join token only when the
// token is not the one it has a record of (see newToken), and then without giving
// up the identity: it is set aside, and taken back if the enrolment is not
// accepted (see setAside). The server does not say why it rejected the identity
// (T-02-21): it may no longer have the environment (TierPlan was reinstalled), or
// the fault may be one the identity has nothing to do with (an audience after an
// address change, a channel binding behind a proxy, a clock), so a rejection is no
// proof that the identity is dead. With the token it has a record of, the agent
// keeps the identity and backs off as before: that token cannot fix it. Without a
// token, and for a blocked host, nothing the agent can do changes the answer, so it
// keeps its identity and waits the maximum; only an administrator, or an operator
// who adds a token, can help. An expired certificate with a join token at hand
// enrols again at once; without one it waits the maximum too. Anything else backs
// off as an ordinary failure.
func (r *runner) refused(re *RefusedError, failures *int) time.Duration {
	if re.Code != r.lastCode {
		r.lastCode = re.Code
		r.log.Warn("the server refused this agent", "code", re.Code, "detail", re.Detail, "hint", r.hint(re.Code))
	}
	switch re.Code {
	case wire.ReasonRevoked, wire.ReasonUnknownAgent:
		return r.rejoin(re.Code)
	case wire.ReasonAuthRejected:
		if r.newToken() {
			return r.rejoin(re.Code)
		}
		r.sayTried()
	case wire.ReasonBlocked, wire.RefuseHostAlreadyConnected:
		return r.cfg.BackoffMax
	case wire.ReasonCertExpired:
		if r.cfg.JoinToken == "" {
			return r.cfg.BackoffMax
		}
		r.forceEnrol = true
	}
	*failures++
	wait := backoff(*failures, r.cfg.BackoffMin, r.cfg.BackoffMax, r.env.rnd)
	// A server that is busy says when to come back; never sooner than that.
	return max(wait, min(re.RetryAfter, maxRetryAfter))
}

// newToken reports whether a hello the server refused auth_rejected should make the
// agent try its join token: it holds an identity, the attempt was a hello and not an
// enrolment, and it has a join token that is not the one on record for the identity,
// or it has no record (an identity enrolled before the agent kept one). The record is
// the token the identity was enrolled with, or the one the agent held when TierPlan
// last accepted the identity's hello (accept).
//
// A missing record has to count as a new token: the agents in the field have none,
// and an agent reinstalled after TierPlan itself was reinstalled with an empty data
// directory (whose environment no longer exists, refused auth_rejected like any other
// bad certificate, T-02-21) must come back by itself. A different record is not proof
// that the identity is dead either: after a token rotation (SwarmRotateCommand) every
// agent holds the new token, and until one of its hellos is accepted with it the
// record still names the old one. A rejection then for a cause that has nothing to do
// with the identity (an audience after an address change, a channel binding behind a
// proxy, a clock) also arrives here. That is why the attempt is non-destructive
// (setAside): it costs one refused enrolment and the identity is taken back, at most
// once per BackoffMax.
//
// A token the agent already tried that way, and that TierPlan refused auth_rejected,
// is not tried again for the same identity (triedHash): the refusal says the token,
// or the way it reaches TierPlan, is no good, and trying it every minute would only
// set the identity aside and back every minute, forever (review 17-2 WR-04). A
// different token, or a new identity, is tried as before.
func (r *runner) newToken() bool {
	if r.cfg.JoinToken == "" || r.forceEnrol {
		return false
	}
	tok := state.TokenHash(r.cfg.JoinToken)
	r.mu.Lock()
	have, hash, tried := r.have, r.enrolHash, r.triedHash
	r.mu.Unlock()
	return have && hash != tok && tried != tok
}

// sayTried says, once, that the agent does not try its join token because TierPlan
// refused it for this identity before.
func (r *runner) sayTried() {
	if r.triedSaid || r.cfg.JoinToken == "" {
		return
	}
	r.mu.Lock()
	tried := r.triedHash != "" && r.triedHash == state.TokenHash(r.cfg.JoinToken)
	r.mu.Unlock()
	if tried {
		r.triedSaid = true
		r.log.Warn(triedLog)
	}
}

// triedLog is what the agent says when it will not try its join token again.
const triedLog = "TierPlan refused an enrolment with this join token while the agent's identity was set aside, so the agent does not try the token again with this identity; it keeps its identity and retries hello. Start it with another join token to try that one"

// rejoin handles a hello that was refused revoked (D-10) or unknown_agent (D-31),
// or auth_rejected by an agent started with a join token it has no record of
// (newToken). The server keeps a revoked agent revoked for good, also after an
// unblock, and an agent it does not know stays unknown, so in those cases the
// identity can never work again: with a join token the agent deletes it and enrols
// again with a fresh key. A host that left its swarm and rejoined then comes back
// by itself, while a host an administrator blocked is refused blocked at that
// enrolment and waits. An auth_rejected says less (see newToken), so the identity is
// only set aside for the enrolment, and taken back unless it is accepted (setAside,
// takeBack); an agent reinstalled with the install command of a TierPlan that was
// itself reinstalled comes back by itself that way. It returns how long to wait
// before the next attempt: none when it enrols again now, the maximum when it
// cannot or should not.
//
// An agent that rejoined less than the maximum backoff ago and is refused again
// waits the maximum before it does so a second time, so at most one new agent
// per minute is ever enrolled, and at most one enrolment per minute is tried with
// an identity set aside.
func (r *runner) rejoin(code string) time.Duration {
	if !r.have || r.cfg.JoinToken == "" {
		return r.cfg.BackoffMax
	}
	now := r.env.now()
	if !r.rejoinedAt.IsZero() && now.Sub(r.rejoinedAt) < r.cfg.BackoffMax {
		return r.cfg.BackoffMax
	}
	if code == wire.ReasonAuthRejected {
		if !r.setAside() {
			return r.cfg.BackoffMax
		}
		r.rejoinedAt = now
		if !r.quietStash {
			r.log.Info(setAsideLog)
		}
		return 0
	}
	if !r.discard() {
		return r.cfg.BackoffMax
	}
	r.rejoinedAt = now
	if code == wire.ReasonUnknownAgent {
		r.log.Info("the agent's identity is not known to this TierPlan; it dropped it and will enrol again with its join token")
	} else {
		r.log.Info("the agent's identity was revoked; it dropped it and will enrol again with its join token")
	}
	return 0
}

// setAsideLog is what the agent says when it sets its identity aside to try its
// join token.
const setAsideLog = "TierPlan did not accept the agent's identity and the agent has a join token it has no record of being accepted with; it set the identity aside and will enrol with the token, and takes the identity back if TierPlan does not accept that either"

// discard deletes the identity for good, on disk and in memory. It reports false,
// with the identity kept, when nothing could be removed.
func (r *runner) discard() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg.StateDir != "" {
		if err := state.Discard(r.cfg.StateDir); err != nil {
			if _, still, _ := state.Load(r.cfg.StateDir); still {
				// Nothing was removed: keep the identity and the old behaviour.
				r.log.Error("the agent could not remove its old identity, so it will not enrol again; fix the permissions of its state directory", "dir", r.cfg.StateDir, "error", err)
				return false
			}
			// The identity is gone and only leftovers remain; the next save replaces them.
			r.log.Warn("the agent removed its old identity but could not remove every file of it", "dir", r.cfg.StateDir, "error", err)
		}
	}
	r.cur, r.have, r.enrolHash, r.triedHash, r.forceEnrol = state.State{}, false, "", "", false
	return true
}

// stashedIdentity is an identity set aside, with the record of its token and of a
// token it tried and was refused.
type stashedIdentity struct {
	cur         state.State
	hash, tried string
}

// setAside moves the identity out of the way of an enrolment, on disk into the
// stash (state.Stash) and in memory into r.stashed, so the next attempt enrols with
// the join token and a fresh key. Nothing is deleted. It reports false, with the
// identity kept where it was, when the files could not be moved. It holds mu
// throughout, so a renewal the control reader of an ending session saves cannot land
// between the move and the change in memory.
func (r *runner) setAside() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg.StateDir != "" {
		if err := state.Stash(r.cfg.StateDir); err != nil {
			if _, still, _ := state.Load(r.cfg.StateDir); still {
				r.log.Error("the agent could not set its identity aside, so it will not try its join token; fix the permissions of its state directory", "dir", r.cfg.StateDir, "error", err)
				return false
			}
			// Part of it moved and could not be put back. The identity is in memory
			// and takeBack writes it again; a restart puts the stash back first.
			r.log.Warn("the agent set its identity aside but could not move every file of it", "dir", r.cfg.StateDir, "error", err)
		}
	}
	r.stashed = &stashedIdentity{cur: r.cur, hash: r.enrolHash, tried: r.triedHash}
	r.cur, r.have, r.enrolHash, r.triedHash, r.forceEnrol = state.State{}, false, "", "", false
	return true
}

// takeBack restores the identity setAside moved, after the enrolment it was moved
// for ended without a welcome (err says how), and returns the wait before the next
// attempt: the maximum, as for every rejoin. On disk the files move back
// (state.Restore); if that fails the identity is written again from memory. Either
// way the agent goes on with its identity in memory, and the next hello presents it.
//
// An enrolment refused auth_rejected was refused for the token, or for the way it
// reaches TierPlan, not for anything the host can change by itself: the token is
// recorded as tried for the identity, and newToken does not try it again (review
// 17-2 WR-04). Any other outcome (a blocked host, a busy server, a host whose other
// agent is connected, a connection that failed) leaves the token to be tried again a
// minute later.
//
// stopping says the agent is stopping (its context ended) in the middle of that
// enrolment: nothing was refused, and the server may even have accepted it, so the
// token is not recorded and the agent says only that it put the identity back.
func (r *runner) takeBack(err error, stopping bool) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stashed
	r.stashed = nil
	tried := s.tried
	var refused *RefusedError
	refusedToken := !stopping && errors.As(err, &refused) && refused.Code == wire.ReasonAuthRejected && r.cfg.JoinToken != ""
	if refusedToken {
		tried = state.TokenHash(r.cfg.JoinToken)
	}
	if dir := r.cfg.StateDir; dir != "" {
		placed, wrote := true, false
		_, rerr := state.Restore(dir)
		if got, ok, lerr := state.Load(dir); rerr != nil || lerr != nil || !ok || !got.Key.Equal(s.cur.Key) {
			if serr := state.Save(dir, s.cur); serr != nil {
				// The stash is still there for the next start (state.ResolveStash).
				placed = false
				r.log.Error("the agent could not put back the identity it had set aside; it keeps it in memory, and a restart puts it back", "dir", dir, "error", errors.Join(rerr, serr))
			} else {
				// Written from memory: so are its records.
				wrote = true
				if s.hash != "" {
					if herr := state.SaveEnrolToken(dir, s.hash); herr != nil {
						r.log.Warn("the agent could not record which join token its identity was accepted with", "dir", dir, "error", herr)
					}
				}
				// The identity is in place again; what is left in the stash is debris.
				if derr := state.DiscardStash(dir); derr != nil {
					r.log.Warn("the agent could not clear the identity it had set aside", "dir", dir, "error", derr)
				}
			}
		}
		if placed && tried != "" && (refusedToken || wrote) {
			if terr := state.SaveTriedToken(dir, s.cur.AgentID, tried); terr != nil {
				// Kept in memory: only a restart tries the token once more.
				r.log.Warn("the agent could not record that TierPlan refused its join token", "dir", dir, "error", terr)
			}
		}
	}
	r.cur, r.have, r.enrolHash, r.triedHash = s.cur, true, s.hash, tried
	if stopping {
		r.log.Info(stoppingLog)
		return r.cfg.BackoffMax
	}
	if !r.quietStash {
		r.quietStash = true
		attrs := []any{"retry_in", r.cfg.BackoffMax.String()}
		switch {
		case refused != nil:
			attrs = append(attrs, "code", refused.Code)
		case err != nil:
			attrs = append(attrs, "error", err)
		}
		r.log.Warn(takenBackLog, attrs...)
	}
	if refusedToken && !r.triedSaid {
		r.triedSaid = true
		r.log.Warn(triedLog)
	}
	return r.cfg.BackoffMax
}

// stoppingLog is what the agent says when it stops while its identity is set aside.
const stoppingLog = "the agent is stopping while it was trying its join token; it took back the identity it had set aside"

// takenBackLog is what the agent says when it takes back an identity it set aside.
const takenBackLog = "TierPlan did not accept the enrolment with the join token either; the agent took back the identity it had set aside and keeps it"

// transient reports whether a refusal is the server's trouble rather than the
// agent's: it will pass, so the agent retries instead of ending. A node the server
// has not seen in a node list yet (node_not_in_swarm) is such a lag: the server reads
// the list again within about a minute, and a node that just joined the swarm is
// accepted at the retry after that.
func transient(code string) bool {
	return code == wire.RefuseRateLimited || code == wire.RefuseInternal || code == wire.RefuseNodeNotInSwarm
}

// waitsAtMax reports whether a refusal is one the agent can do nothing about but
// that can pass without a change on the host: an administrator unblocks a blocked
// host, and a host whose other agent is connected now (host_already_connected,
// review 17-2 WR-02) can enrol once that agent is gone. The agent waits the maximum
// backoff between tries and never ends for one, not even at its first enrolment.
func waitsAtMax(code string) bool {
	return code == wire.ReasonBlocked || code == wire.RefuseHostAlreadyConnected
}

// routeFile is the host route file the agent reads.
func (r *runner) routeFile() string {
	if r.cfg.HostRouteFile != "" {
		return r.cfg.HostRouteFile
	}
	return hostnet.DefaultRouteFile
}

// hint is what an operator should do about a refusal code.
func (r *runner) hint(code string) string {
	switch code {
	case wire.ReasonRevoked:
		if r.cfg.JoinToken == "" {
			return "this agent was revoked in TierPlan, or its node left the swarm, and it has no join token to enrol again with: start it with TIERPLAN_AGENT_JOIN_TOKEN_FILE or TIERPLAN_AGENT_JOIN_TOKEN set"
		}
		return "this agent was revoked in TierPlan, or its node left the swarm; it drops the revoked identity and enrols again as a new agent, unless an administrator blocked this host"
	case wire.ReasonUnknownAgent:
		return "this agent was enrolled after the backup this TierPlan was restored from; reinstall it with the environment's install command"
	case wire.ReasonBlocked:
		return "this host is on TierPlan's block list; an administrator can unblock it in the environment's settings"
	case wire.RefuseHostAlreadyConnected:
		return "another agent of this host is connected to this TierPlan environment now, such as a second agent container or service on the host; remove the duplicate. " +
			"A VM cloned from this host shares its Docker engine ID and is refused this way. On the clone, in this order: first remove the copied agent and its " +
			"tierplan-agent volume, which hold the original's identity (a swarm node leaves the swarm first, then removes them); then stop Docker, delete the " +
			"engine-id file in Docker's data root (by default /var/lib/docker/engine-id) and start Docker; then install the agent fresh, a standalone clone " +
			"as its own environment (a standalone environment manages one host), a swarm node by joining the swarm again. " +
			"This agent tries again once a minute and enrols by itself once the other one is gone"
	case wire.RefuseNodeNotInSwarm:
		return "this node is not in the swarm's node list as TierPlan last read it; a node that just joined enrols by itself within about a minute; " +
			"a node that was removed from the swarm must join it again, or remove the agent from the host"
	case wire.ReasonCertExpired:
		return "the agent's certificate expired; start the agent with a join token (TIERPLAN_AGENT_JOIN_TOKEN_FILE or TIERPLAN_AGENT_JOIN_TOKEN) to enrol again"
	case wire.ReasonAuthRejected:
		return "TierPlan did not accept the agent's credentials; check the join token and TIERPLAN_AGENT_URL. If TierPlan's agent address changed while this agent was offline, start it again with the new address in TIERPLAN_AGENT_URL on the same volume. An enrolled agent that has a join token it has no record of being accepted with (a new install command, for example after TierPlan was reinstalled, or an identity enrolled by an older agent) sets its identity aside and enrols with the token, at most once a minute; if TierPlan refuses that too it takes the identity back, so this refusal never costs it the identity, and a token refused auth_rejected that way is not tried again with the same identity. With the token on record, or one already tried, it keeps its identity and retries"
	case wire.ReasonVersionIncompatible:
		return "upgrade the agent to the version TierPlan runs"
	case wire.ReasonClockSkew:
		return "set this host's clock, for example with NTP"
	case wire.ReasonClusterChanged:
		return "the swarm behind this environment was re-initialised; an administrator adopts the new cluster in the environment's settings in TierPlan, and the agent connects on its next try"
	case wire.ReasonEngineUnreachable:
		return "the agent could not read Docker's description of this host; check that /var/run/docker.sock is mounted into the agent and that Docker is running"
	}
	return ""
}

// once makes one connection: dial, handshake, serve. welcomed reports whether
// the server accepted the agent, so the caller can reset its failure count.
//
// The addresses are tried in order, the newest first (D-19). An address that cannot
// be reached, or whose certificate cannot be verified, hands over to the next, and
// the error the caller sees is the newest address's. Once a WebSocket is open to an
// address the answer is final, whatever it is: a refusal from the server is not a
// reason to ask another address.
func (r *runner) once(ctx context.Context) (welcomed bool, err error) {
	// The engine is described before the dial, so the handshake itself never
	// waits on it. An engine that does not answer is reported, not fatal: the
	// server shows engine_unreachable (D-40).
	reported := facts.Collect(ctx, r.cfg.SocketPath)
	if !reported.EngineReachable {
		r.log.Warn("the Docker engine did not answer on its socket", "socket", r.cfg.SocketPath)
	}
	// What the host owner set is part of the facts, so the UI can say "read-only
	// (set on host)" and show the manual update commands (D-28).
	reported.Policy = wire.PolicyFacts{ReadOnly: r.cfg.ReadOnly, SelfUpdate: r.cfg.SelfUpdate, PrivilegedServices: r.cfg.AllowPrivilegedServices}

	// Whether the host's route file can be read tells the server whether to ask
	// for the host's routes (stream kind 0x03). An agent older than this field
	// sends nothing and is never asked.
	reported.HostNet = hostnet.Probe(r.routeFile())
	if reported.HostNet == wire.HostNetUnmounted {
		r.log.Info("the host's route file is not mounted; the subnet picker skips this host's route check", "path", r.routeFile())
	}

	cr, err := r.credentials()
	if err != nil {
		return false, err
	}

	var first error
	urls := r.dialURLs()
	for i, raw := range urls {
		tg, err := newTarget(raw)
		if err != nil {
			// Only addresses that passed state.ValidURL are kept, so this is a bug
			// in what was stored, not something to stop on.
			r.log.Warn("the agent skipped an address it cannot dial", "error", err)
			continue
		}
		welcomed, reached, err := r.attempt(ctx, tg, reported, cr)
		if reached || ctx.Err() != nil {
			return welcomed, err
		}
		if first == nil {
			first = err
		}
		if i+1 < len(urls) {
			r.log.Info("the agent could not connect to that address and tries the previous one", "address", tg.aud, "error", err)
		}
	}
	if first == nil {
		first = errors.New("the agent has no address to dial")
	}
	return false, first
}

// dialURLs is the addresses to dial now, newest first. It is never empty: the
// address from the environment is the last resort.
func (r *runner) dialURLs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if urls := state.ActiveURLs(r.urls, r.env.now()); len(urls) > 0 {
		return urls
	}
	return []string{r.cfg.URL}
}

// attempt dials one address and, if the server answers, runs the handshake and
// serves the session. reached is whether a WebSocket was opened to it: false means
// the address could not be used and another may be tried.
func (r *runner) attempt(ctx context.Context, tg target, reported wire.Facts, cr credentials) (welcomed, reached bool, err error) {
	client := r.cfg.HTTPClient
	if client == nil {
		r.mu.Lock()
		pins := state.ActivePins(r.pins, r.env.now())
		r.mu.Unlock()
		client = newHTTPClient(pins, r.cfg.InstallPins, r.env.roots, tg.host)
		defer client.CloseIdleConnections()
	}
	c, resp, err := websocket.Dial(ctx, tg.url, &websocket.DialOptions{ //nolint:bodyclose // coder/websocket reads and closes the body of a refused upgrade itself
		HTTPClient:   client,
		Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
		if u := asUntrusted(err); u != nil {
			return false, false, u
		}
		return false, false, fmt.Errorf("connect to %s: %w", tg.aud, err)
	}
	// CloseNow, never a graceful close: a peer that stopped answering would make
	// a close handshake wait (Pitfall 6).
	defer func() { _ = c.CloseNow() }()
	if c.Subprotocol() != wire.Subprotocol {
		return false, true, fmt.Errorf("%s does not speak %s", tg.aud, wire.Subprotocol)
	}
	// Until the handshake is over the raw connection keeps its read limit; the
	// net.Conn wrapper removes it (Pitfall 4), so it is made only afterwards.
	c.SetReadLimit(wire.MaxHandshakeBytes)

	hctx, cancel := context.WithTimeout(ctx, wire.HandshakeTimeout)
	welcome, err := handshake(hctx, c, resp, r.cfg.Version, tg.aud, cr, reported, r.env.now)
	cancel()
	if err != nil {
		return false, true, err
	}
	verified := resp != nil && resp.TLS != nil
	if err := r.accept(cr, welcome, verified, tg); err != nil {
		return false, true, err
	}
	r.forceEnrol = false

	every := time.Duration(welcome.HeartbeatMs) * time.Millisecond
	if welcome.HeartbeatMs <= 0 {
		every = wire.DefaultHeartbeatMs * time.Millisecond
	}
	// A server cannot make the agent spin: the floor is far below any real setting.
	every = max(every, minHeartbeat)
	return true, true, serve(ctx, c, r.cfg.SocketPath, r.routeFile(), every, r.env.now, r.log, proxy.Policy{ReadOnly: r.cfg.ReadOnly, AllowPrivilegedServices: r.cfg.AllowPrivilegedServices}, r.hooks(verified))
}

// hooks is what the control stream of the next session hands the server's messages
// to: the renewal of the certificate the agent holds now, and the addresses and pins
// the server pushes. verified says the session ran over TLS that was verified: what a
// server pushes over any other session is not taken, so a spoofed server cannot send
// the agent, and its join token, somewhere else (T-02-84). For the same reason an
// update or an uninstall over any other session is refused with insecure_session
// and runs nothing.
func (r *runner) hooks(verified bool) controlHooks {
	r.mu.Lock()
	cur := r.cur
	r.mu.Unlock()
	warned := false
	maint := r.cfg.Maint
	if !verified {
		maint = &unverifiedMaint{log: r.log}
	}
	return controlHooks{
		maint: maint,
		renew: newRenewer(cur.Cert, cur.AgentID, cur.Key, r.env.now, r.env.rnd, r.log, r.saveCert),
		urls: func(u wire.URLUpdate) {
			if !verified {
				if !warned {
					warned = true
					r.log.Warn("the server sent new addresses over a connection that was not verified; the agent ignored them")
				}
				return
			}
			r.mergeURLs(u.URLs)
			r.mergePins(u.Pins)
		},
	}
}

// pushable reports whether the agent will take an address from the server: one it
// could dial, over wss. A verified session is TLS, so a plain ws:// address could
// only be a downgrade.
func pushable(raw string) bool {
	if !state.ValidURL(raw) {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "wss"
}

// mergeURLs keeps the addresses a welcome or a push lists, the newest first, and the
// ones it no longer lists for 30 days in case the change is reverted
// (state.MergeURLs), saving them when they changed. A list with nothing usable in it changes nothing. It is only called for a
// verified session.
func (r *runner) mergeURLs(fromServer []string) {
	var usable []string
	for _, u := range fromServer[:min(len(fromServer), wire.MaxPushedURLs)] {
		if pushable(u) {
			usable = append(usable, u)
		}
	}
	if len(usable) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	merged := state.MergeURLs(r.urls, usable, r.env.now())
	if state.EqualURLs(merged, r.urls) {
		return
	}
	r.urls = merged
	r.log.Info("the server changed the address the agent dials; the previous one is kept for 30 days in case the change is reverted", "address", usable[0])
	if r.cfg.StateDir == "" {
		return
	}
	if err := state.SaveURLs(r.cfg.StateDir, merged); err != nil {
		r.log.Warn("the agent could not save the new address; after a restart it will dial the one it dials now", "dir", r.cfg.StateDir, "error", err)
	}
}

// saveCert keeps a renewed certificate: the identity on the volume is replaced
// atomically (state.Save writes every file under a temporary name first), and the
// next connection says hello with it. A crash before this returns leaves the old
// certificate, which the server still accepts until it expires (authentication is by
// key). The certificate must be for the key the agent holds now, so a renewal that
// arrives after the identity was dropped and replaced cannot overwrite the new one.
func (r *runner) saveCert(cert []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.have {
		return errors.New("the agent has no identity to renew")
	}
	leaf, err := x509.ParseCertificate(cert)
	if err != nil {
		return fmt.Errorf("read the renewed certificate: %w", err)
	}
	if pub, ok := leaf.PublicKey.(ed25519.PublicKey); !ok || !pub.Equal(publicOf(r.cur.Key)) {
		return errors.New("the renewed certificate is not for the key this agent holds now")
	}
	next := r.cur
	next.Cert = cert
	if r.cfg.StateDir != "" {
		if err := state.Save(r.cfg.StateDir, next); err != nil {
			return err
		}
	}
	r.cur = next
	return nil
}

// minHeartbeat is the shortest interval the agent accepts from a server.
const minHeartbeat = 10 * time.Millisecond

// credentials is what this attempt presents: the certificate and key when the
// agent has an identity, else a fresh key and the join token.
func (r *runner) credentials() (credentials, error) {
	r.mu.Lock()
	cur, have := r.cur, r.have
	r.mu.Unlock()
	if have && !r.forceEnrol {
		return credentials{
			mode: wire.ModeHello, agentID: cur.AgentID, cert: cur.Cert,
			pub: publicOf(cur.Key), priv: cur.Key,
		}, nil
	}
	if r.cfg.JoinToken == "" {
		return credentials{}, errNoCredentials
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return credentials{}, fmt.Errorf("generate the agent key: %w", err)
	}
	return credentials{mode: wire.ModeEnrol, token: r.cfg.JoinToken, pub: pub, priv: priv}, nil
}

// accept records the outcome of a welcome: an enrolment's identity is kept in
// memory and saved, the server's pins are merged in, and every welcome marks the
// agent as connected. verified says the session ran over TLS that was verified;
// pins from any other session are not taken, so a spoofed server cannot plant one
// (T-02-33).
func (r *runner) accept(cr credentials, w wire.Welcome, verified bool, tg target) error {
	switch cr.mode {
	case wire.ModeEnrol:
		if len(w.Cert) == 0 {
			return errors.New("the server welcomed the agent without a certificate")
		}
		cur := state.State{AgentID: w.AgentID, EnvID: w.EnvID, Key: cr.priv, Cert: w.Cert}
		// Only the hash of the token is kept, never the token. credentials never
		// enrols without one; an empty one is recorded as unknown all the same.
		var hash string
		if cr.token != "" {
			hash = state.TokenHash(cr.token)
		}
		r.mu.Lock()
		// A new identity has tried no token.
		r.cur, r.have, r.enrolHash, r.triedHash = cur, true, hash, ""
		r.mu.Unlock()
		r.triedSaid = false
		r.log.Info("enrolled with TierPlan", "agent_id", w.AgentID, "environment_id", w.EnvID, "server", tg.aud)
		if r.cfg.StateDir != "" {
			if err := state.Save(r.cfg.StateDir, cur); err != nil {
				// Staying connected beats stopping: the identity is in memory, and
				// the agent says so loudly because a restart will enrol again.
				r.log.Error("the agent could not save its identity; a restart will enrol it again as a new agent", "dir", r.cfg.StateDir, "error", err)
			} else if hash != "" {
				if err := state.SaveEnrolToken(r.cfg.StateDir, hash); err != nil {
					// The identity is good. Without the record a hello refused
					// auth_rejected after a restart tries the token at hand with
					// the identity set aside, as for an identity enrolled before
					// the record existed.
					r.log.Warn("the agent could not record which join token it enrolled with", "dir", r.cfg.StateDir, "error", err)
				}
			}
		}
		if r.stashed != nil {
			// The enrolment the identity was set aside for was accepted: the new
			// identity replaces it, and TierPlan retired the old one's record when it
			// enrolled this host again. Only now is the old identity deleted.
			r.stashed = nil
			if r.cfg.StateDir != "" {
				if err := state.DiscardStash(r.cfg.StateDir); err != nil {
					r.log.Warn("the agent could not remove the identity it had set aside; the next start settles it", "dir", r.cfg.StateDir, "error", err)
				}
			}
			r.log.Info("TierPlan accepted the enrolment with the join token; the agent removed the identity it had set aside")
		}
	default:
		r.mu.Lock()
		mine, known := r.cur.AgentID, r.enrolHash
		r.mu.Unlock()
		if w.AgentID != mine {
			return fmt.Errorf("the server welcomed agent %q but this agent is %q", w.AgentID, mine)
		}
		r.log.Info("connected to TierPlan", "agent_id", w.AgentID, "environment_id", w.EnvID, "server", tg.aud)
		if verified {
			r.recordAcceptedToken(known)
		}
	}
	if verified {
		r.mergePins(w.Pins)
		r.mergeURLs(w.URLs)
	}
	if r.cfg.StateDir != "" {
		if err := state.MarkConnected(r.cfg.StateDir); err != nil {
			r.log.Warn("the agent could not write its connected marker", "error", err)
		}
	}
	return nil
}

// recordAcceptedToken updates the record of the join token after TierPlan accepted
// the identity's hello while the agent held a token other than the one on record
// (known): the identity is alive with that token, as after a token rotation, so a
// later auth_rejected with it is no reason to try it (newToken). Only a token of the
// identity's own environment is such a rotation (RotateToken keeps the environment
// in the token): a token of another environment, as an operator who ran another
// environment's install command on this host holds, says nothing about the identity,
// and recording it would stop the agent from ever trying it once its own environment
// is gone (review 17-2 WR-01). An identity with no record keeps none: the agent
// cannot tell which token enrolled it, and an agent whose operator reinstalled
// TierPlan must still come back by itself, so nothing is written for it. Nothing is
// written without a token either.
func (r *runner) recordAcceptedToken(known string) {
	if r.cfg.JoinToken == "" || known == "" {
		return
	}
	hash := state.TokenHash(r.cfg.JoinToken)
	if hash == known {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if tokenEnv(r.cfg.JoinToken) != r.cur.EnvID {
		return
	}
	r.enrolHash = hash
	if r.cfg.StateDir == "" {
		return
	}
	if err := state.SaveEnrolToken(r.cfg.StateDir, hash); err != nil {
		r.log.Warn("the agent could not record the join token TierPlan accepted it with", "dir", r.cfg.StateDir, "error", err)
	}
}

// joinTokenPrefix starts every join token: tpjt_<environment ID>_<secret>.
const joinTokenPrefix = "tpjt_"

// tokenEnv is the environment ID a join token names, or "" for a string that is not
// shaped like a join token. The server's agents.ParseToken is the authority on what
// a valid token is; the agent only needs the environment to tell a rotation of its
// own environment's token from a token of another environment.
func tokenEnv(token string) string {
	rest, ok := strings.CutPrefix(token, joinTokenPrefix)
	if !ok {
		return ""
	}
	env, secret, ok := strings.Cut(rest, "_")
	if !ok || secret == "" {
		return ""
	}
	return env
}

// mergePins keeps the pins a welcome lists, and for 30 days the ones it no longer
// does (state.MergePins), saving them when they changed.
func (r *runner) mergePins(fromServer []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	merged := state.MergePins(r.pins, fromServer, r.env.now())
	if state.EqualPins(merged, r.pins) {
		return
	}
	r.pins = merged
	if r.cfg.StateDir == "" {
		return
	}
	if err := state.SavePins(r.cfg.StateDir, merged); err != nil {
		r.log.Warn("the agent could not save the server's pins; it will trust the server by the install pin or the system roots after a restart", "dir", r.cfg.StateDir, "error", err)
	}
}

// publicOf is the public half of an Ed25519 private key: the last 32 bytes of it.
func publicOf(k ed25519.PrivateKey) ed25519.PublicKey { return ed25519.PublicKey(k[ed25519.SeedSize:]) }

// credentials is the identity an attempt presents.
type credentials struct {
	mode    string
	agentID string // hello
	cert    []byte // hello
	token   string // enrol
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handshake runs the challenge, auth and welcome exchange on the raw connection.
// In enrol mode the signature proves possession of the new key; in hello mode it
// is made with the key the certificate was issued for, over a transcript that
// carries the certificate.
func handshake(ctx context.Context, c *websocket.Conn, resp *http.Response, version, aud string,
	cr credentials, facts wire.Facts, now func() time.Time) (wire.Welcome, error) {
	var ch wire.Challenge
	var first json.RawMessage
	if err := readJSON(ctx, c, &first); err != nil {
		return wire.Welcome{}, fmt.Errorf("read the server challenge: %w", err)
	}
	if err := json.Unmarshal(first, &ch); err != nil {
		return wire.Welcome{}, fmt.Errorf("the server challenge is not JSON: %w", err)
	}
	if ch.T == wire.TypeRefuse {
		// A server that is busy turns the agent away before the challenge.
		return wire.Welcome{}, refusal(first)
	}
	if ch.T != wire.TypeChallenge {
		return wire.Welcome{}, fmt.Errorf("expected a challenge from the server, got %q", ch.T)
	}
	if ch.Proto != wire.ProtoVersion {
		return wire.Welcome{}, &RefusedError{Code: wire.ReasonVersionIncompatible,
			Detail: fmt.Sprintf("the server speaks protocol %d, this agent speaks %d", ch.Proto, wire.ProtoVersion)}
	}

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return wire.Welcome{}, fmt.Errorf("generate a nonce: %w", err)
	}
	// The channel binding: ExportKeyingMaterial on the TLS state of the dial
	// response. A server that terminates TLS itself computes the same value; behind
	// a TLS-terminating proxy the two differ and the server does not compare them.
	var ekm []byte
	if resp != nil {
		ekm = authn.EKM(resp.TLS)
	}
	auth := wire.Auth{
		T: wire.TypeAuth, Mode: cr.mode, Proto: wire.ProtoVersion, AgentVersion: version,
		Aud: aud, Nonce: nonce, TS: now().UnixMilli(), EKM: ekm, Facts: facts,
	}
	var signed []byte
	if cr.mode == wire.ModeHello {
		auth.AgentID, auth.Cert = cr.agentID, cr.cert
		signed = cr.cert
	} else {
		auth.Pub, auth.Token = cr.pub, cr.token
		signed = cr.pub
	}
	auth.Sig = authn.Sign(cr.priv, authn.Transcript(cr.mode, wire.ProtoVersion, ch.Nonce, nonce, aud, signed, ekm))
	if err := writeJSON(ctx, c, auth); err != nil {
		return wire.Welcome{}, fmt.Errorf("send the handshake: %w", err)
	}

	var raw json.RawMessage
	if err := readJSON(ctx, c, &raw); err != nil {
		// A refusal is a message followed by a close; if the message did not
		// arrive, the close frame's reason is the code.
		if code := websocket.CloseStatus(err); code == websocket.StatusPolicyViolation {
			var ce websocket.CloseError
			if errors.As(err, &ce) && ce.Reason != "" {
				return wire.Welcome{}, &RefusedError{Code: strings.TrimSpace(ce.Reason)}
			}
		}
		return wire.Welcome{}, fmt.Errorf("read the server answer: %w", err)
	}
	var head struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return wire.Welcome{}, fmt.Errorf("the server answer is not JSON: %w", err)
	}
	switch head.T {
	case wire.TypeWelcome:
		var w wire.Welcome
		if err := json.Unmarshal(raw, &w); err != nil || w.AgentID == "" || w.EnvID == "" {
			return wire.Welcome{}, errors.New("the server welcome is incomplete")
		}
		return w, nil
	case wire.TypeRefuse:
		return wire.Welcome{}, refusal(raw)
	default:
		return wire.Welcome{}, fmt.Errorf("unexpected message %q from the server", head.T)
	}
}

// refusal is the error for a refuse message.
func refusal(raw []byte) error {
	var r wire.Refuse
	if err := json.Unmarshal(raw, &r); err != nil {
		return errors.New("the server refusal is malformed")
	}
	return &RefusedError{Code: r.Code, Detail: r.Detail, RetryAfter: time.Duration(r.RetryAfterMs) * time.Millisecond}
}

// serve multiplexes the connection, opens the control stream and answers the
// server's Docker streams until the session ends. It ends when the multiplexer
// does, and when the control stream breaks, so a server that stopped reading
// heartbeats is not served as if all were well.
func serve(ctx context.Context, c *websocket.Conn, socketPath, routeFile string, every time.Duration, now func() time.Time, log *slog.Logger, policy proxy.Policy, hooks controlHooks) error {
	conn := websocket.NetConn(ctx, c, websocket.MessageBinary)
	sess, err := yamux.Server(conn, muxConfig())
	if err != nil {
		return fmt.Errorf("start the multiplexer: %w", err)
	}
	defer func() { _ = sess.Close() }()

	// Every request the proxy refuses is reported to the server on the control
	// stream, which has one writer: the proxy hands it over without blocking.
	den := newDenials()
	handler := proxy.Handler(socketPath, policy, den.report)
	go func() {
		<-ctx.Done()
		_ = sess.Close()
	}()

	ctl, err := sess.OpenStream()
	if err != nil {
		return fmt.Errorf("open the control stream: %w", err)
	}
	controlErr := make(chan error, 1)
	go func() {
		err := runControl(ctx, ctl, socketPath, every, now, log, den, hooks)
		controlErr <- err
		_ = sess.Close() // ends Accept below
	}()

	for {
		st, err := sess.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			select {
			case cerr := <-controlErr:
				if cerr != nil {
					return fmt.Errorf("the control stream to TierPlan ended: %w", cerr)
				}
			default:
			}
			return fmt.Errorf("the connection to TierPlan ended: %w", err)
		}
		go serveStream(st, handler, routeFile, log)
	}
}

// serveStream reads the kind byte and answers the stream by its kind: Docker
// HTTP goes to the allowlisting proxy, a host-net request gets one document read
// from the route file. Any other kind is closed.
func serveStream(st net.Conn, h http.Handler, routeFile string, log *slog.Logger) {
	defer func() { _ = st.Close() }()
	_ = st.SetReadDeadline(time.Now().Add(kindTimeout))
	var kind [1]byte
	if _, err := io.ReadFull(st, kind[:]); err != nil {
		return
	}
	switch kind[0] {
	case wire.StreamKindDockerHTTP:
		_ = st.SetReadDeadline(time.Time{})
		proxy.ServeConn(st, h)
	case wire.StreamKindHostNet:
		// One file read, one document, no Docker call and no command. A server
		// that does not read the answer cannot hold the stream open.
		_ = st.SetWriteDeadline(time.Now().Add(hostNetWriteTimeout))
		_ = json.NewEncoder(st).Encode(hostnet.Read(routeFile))
	default:
		log.Warn("closed a stream of an unknown kind", "kind", kind[0])
	}
}

func readJSON(ctx context.Context, c *websocket.Conn, v any) error {
	typ, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errors.New("a handshake message must be a text frame")
	}
	return json.Unmarshal(data, v)
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}
