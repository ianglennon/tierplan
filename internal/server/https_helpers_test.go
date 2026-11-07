// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"crypto/tls"
	"crypto/x509"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// stepClock is a clock the test moves. After arms a timer and announces it, so a test that moves the
// clock knows the loop under test is waiting on it and never sleeps to find out. It is the janitor
// scheduler's schedClock, kept here because that one lives in another package's tests.
type stepClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []stepTimer
	armed  chan struct{}
}

type stepTimer struct {
	at time.Time
	ch chan time.Time
}

func newStepClock(now time.Time) *stepClock {
	return &stepClock{now: now, armed: make(chan struct{}, 4096)}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, stepTimer{at: c.now.Add(d), ch: ch})
	c.armed <- struct{}{}
	return ch
}

// Advance moves the clock on by d and fires every timer that is due.
func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}

// WaitArmed waits for the next timer to be armed. A hang fails with a deadline naming what was awaited.
func (c *stepClock) WaitArmed(t *testing.T) {
	t.Helper()
	testsupport.Await(t, "the loop under test to arm its next timer", c.armed)
}

// Tick waits for a timer to be armed and then moves the clock on by d.
func (c *stepClock) Tick(t *testing.T, d time.Duration) {
	t.Helper()
	c.WaitArmed(t)
	c.Advance(d)
}

// genCert makes a certificate of its own, with its own key, that a client can verify with a pool
// and ServerName "localhost". commonName is added to the names, so two certificates differ in more
// than their serial number. Plans 05-08 reuse it.
func genCert(t *testing.T, commonName string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	want := names{dns: []string{"localhost"}, ips: []string{"127.0.0.1", "::1"}}
	if commonName != "" && commonName != "localhost" {
		want.dns = sortedKeys(map[string]bool{"localhost": true, commonName: true})
	}
	der, err := signCertificate(key, want, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}
