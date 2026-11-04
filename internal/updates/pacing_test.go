// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/regclient/regclient/types/errs"
	"golang.org/x/time/rate"
)

// The pace of asking a registry (plan 12-05, D-01, T-12-11). The clock is injected and the wait for a turn
// at the rate is immediate, so none of this sleeps.

type pacerClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *pacerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *pacerClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testPacer() (*hostPacer, *pacerClock) {
	clk := &pacerClock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	p := newHostPacer(clk.Now)
	p.wait = func(context.Context, *rate.Limiter) error { return nil }
	return p, clk
}

var errTooMany = fmt.Errorf("toomany: %w", errs.ErrHTTPRateLimit)

// try makes one request to host that answers err, and says whether the request was made.
func try(p *hostPacer, host string, answer error) (made bool, err error) {
	err = p.Do(context.Background(), host, func(context.Context) error {
		made = true
		return answer
	})
	return made, err
}

func TestBreakerBackoff(t *testing.T) {
	t.Parallel()
	p, clk := testPacer()
	const host = "registry.example"

	// The first refusal opens the breaker for 15 minutes: nothing is sent while it is open.
	if made, err := try(p, host, errTooMany); !made || !errors.Is(err, errs.ErrHTTPRateLimit) {
		t.Fatalf("the first request: made=%v err=%v", made, err)
	}
	clk.advance(15*time.Minute - time.Second)
	if made, err := try(p, host, nil); made || !errors.Is(err, errBreakerOpen) {
		t.Errorf("just before 15 minutes: made=%v err=%v, want the breaker open and nothing sent", made, err)
	}
	clk.advance(time.Second)
	// Each refusal after it reopens doubles the wait, up to two hours.
	for _, want := range []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour} {
		if made, _ := try(p, host, errTooMany); !made {
			t.Fatalf("the breaker should have closed for the retry before the %v wait", want)
		}
		clk.advance(want - time.Second)
		if made, err := try(p, host, nil); made || !errors.Is(err, errBreakerOpen) {
			t.Errorf("just before %v: made=%v err=%v, want the breaker still open", want, made, err)
		}
		clk.advance(time.Second)
	}
	// A success closes it and forgets the trips: the next refusal is 15 minutes again.
	if made, err := try(p, host, nil); !made || err != nil {
		t.Fatalf("after the wait: made=%v err=%v, want the request to go and succeed", made, err)
	}
	if made, _ := try(p, host, errTooMany); !made {
		t.Fatal("the breaker should be closed after a success")
	}
	clk.advance(15*time.Minute - time.Second)
	if made, _ := try(p, host, nil); made {
		t.Error("the first refusal after a success should hold for 15 minutes")
	}
	clk.advance(time.Second)
	if made, err := try(p, host, nil); !made || err != nil {
		t.Errorf("15 minutes after: made=%v err=%v, want the request to go", made, err)
	}
}

func TestBreakerIsPerHostAndOnlyForRateLimits(t *testing.T) {
	t.Parallel()
	p, _ := testPacer()
	if made, _ := try(p, "a.example", errTooMany); !made {
		t.Fatal("the first request was not made")
	}
	if made, err := try(p, "a.example", nil); made || !errors.Is(err, errBreakerOpen) {
		t.Errorf("host a after its refusal: made=%v err=%v, want the breaker open", made, err)
	}
	if made, err := try(p, "b.example", nil); !made || err != nil {
		t.Errorf("host b: made=%v err=%v, want it unaffected", made, err)
	}
	// Other failures say nothing about the limit.
	for _, err := range []error{errors.New("connection refused"), fmt.Errorf("denied: %w", errs.ErrHTTPUnauthorized), fmt.Errorf("gone: %w", errs.ErrNotFound)} {
		if made, _ := try(p, "c.example", err); !made {
			t.Fatal("request not made")
		}
		if made, got := try(p, "c.example", nil); !made || got != nil {
			t.Errorf("after %v: made=%v err=%v, want the breaker closed", err, made, got)
		}
	}
}

func TestRefusalsInFlightTogetherTripOnce(t *testing.T) {
	t.Parallel()
	p, clk := testPacer()
	const host = "registry.example"
	// Four requests are in flight when the registry starts refusing: all four are refused, and the
	// breaker is open for 15 minutes, not for 2 hours.
	var in sync.WaitGroup
	in.Add(hostConcurrency)
	release := make(chan struct{})
	var done sync.WaitGroup
	for range hostConcurrency {
		done.Add(1)
		go func() {
			defer done.Done()
			_ = p.Do(context.Background(), host, func(context.Context) error {
				in.Done()
				<-release
				return errTooMany
			})
		}()
	}
	in.Wait()
	close(release)
	done.Wait()
	clk.advance(15*time.Minute - time.Second)
	if made, _ := try(p, host, nil); made {
		t.Error("the breaker should still be open just before 15 minutes")
	}
	clk.advance(time.Second)
	if made, err := try(p, host, nil); !made || err != nil {
		t.Errorf("at 15 minutes: made=%v err=%v, want the breaker closed: four refusals together are one trip", made, err)
	}
}

func TestHostCapAndRates(t *testing.T) {
	t.Parallel()
	p, _ := testPacer()
	const host = "registry.example"
	var mu sync.Mutex
	inFlight, most := 0, 0
	var wg sync.WaitGroup
	release := make(chan struct{})
	for range 3 * hostConcurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Do(context.Background(), host, func(context.Context) error {
				mu.Lock()
				inFlight++
				most = max(most, inFlight)
				mu.Unlock()
				<-release
				mu.Lock()
				inFlight--
				mu.Unlock()
				return nil
			})
		}()
	}
	for {
		mu.Lock()
		n := inFlight
		mu.Unlock()
		if n == hostConcurrency {
			break
		}
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	if most != hostConcurrency {
		t.Errorf("at most %d requests in flight to one host, want %d", most, hostConcurrency)
	}

	for host, want := range map[string]rate.Limit{
		"docker.io": 5, "registry-1.docker.io": 5, "index.docker.io": 5, "DOCKER.IO": 5, "ghcr.io": 20, "registry.example:5000": 20,
	} {
		if got := p.host(host).lim.Limit(); got != want {
			t.Errorf("rate for %s = %v, want %v", host, got, want)
		}
	}
}

func TestCancelledWaitIsNotAnAnswer(t *testing.T) {
	t.Parallel()
	p, _ := testPacer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := p.Do(ctx, "registry.example", func(context.Context) error { called = true; return nil })
	if called || !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled call: called=%v err=%v, want no request and context.Canceled", called, err)
	}
}
