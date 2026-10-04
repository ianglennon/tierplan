// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package connect

import (
	"crypto/ed25519"
	"log/slog"
	"sync"
	"time"
)

const (
	// renewRetry is how long after a failed renewal the agent asks again, give or
	// take a quarter, so a fleet that failed together does not ask together.
	renewRetry = time.Hour
	// renewAnswerTimeout is how long the agent waits for the server's answer to a
	// renew before it counts the attempt as failed and asks again later.
	renewAnswerTimeout = 2 * time.Minute
)

// nextRenewal is when the agent should next ask for a new certificate (D-04). With
// no failures it is two thirds of the way through the certificate's life, about day
// 20 of 30. After a failure it is about renewRetry after the failure at now, jittered
// by rnd (nil means the process's random source).
func nextRenewal(notBefore, notAfter, now time.Time, failures int, rnd func() float64) time.Time {
	return time.Time{}
}

// renewer is the agent's side of certificate renewal: it decides when to ask, and
// checks and keeps what the server answers.
type renewer struct {
	mu sync.Mutex
}

// newRenewer reads the certificate the agent holds and schedules its renewal. It is
// nil when the certificate cannot be read, and then the agent does not renew; it
// logs why. save keeps a renewed certificate: it must replace the stored one
// atomically and update the identity the next connection presents.
func newRenewer(cert []byte, agentID string, key ed25519.PrivateKey, now func() time.Time, rnd func() float64, log *slog.Logger, save func([]byte) error) *renewer {
	return &renewer{}
}

// poll reports whether a renew should be sent now, and if so counts one as in
// flight until the answer arrives or renewAnswerTimeout passes.
func (rn *renewer) poll() bool { return false }

// handle takes the server's answer to a renew: typ is the message type and raw the
// whole line.
func (rn *renewer) handle(typ string, raw []byte) {}
