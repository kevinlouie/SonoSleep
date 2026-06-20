package ha

import (
	"context"
	"math/rand"
	"time"
)

// defaultJitter returns a pseudo-random fraction in [0,1) for backoff jitter.
func defaultJitter() float64 { return rand.Float64() }

// sleepCtx blocks for d or until ctx is cancelled, whichever comes first. It
// returns ctx.Err() if the context is done, else nil.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoffDelay computes the capped exponential backoff delay for a given
// attempt (0-based), with full lower-half jitter applied. base is the delay for
// attempt 0; it doubles each attempt up to maxDelay. jitter is a value in [0,1)
// (typically rand.Float64) that shrinks the delay into [d*0.5, d*1.0), which
// spreads reconnect storms while never exceeding the cap. It is a pure function
// so the backoff schedule is unit-testable without sleeping.
func backoffDelay(attempt int, base, maxDelay time.Duration, jitter float64) time.Duration {
	d := base
	for i := 0; i < attempt; i++ {
		d *= 2
		if d > maxDelay {
			d = maxDelay
			break
		}
	}
	if d > maxDelay {
		d = maxDelay
	}
	if jitter < 0 {
		jitter = 0
	} else if jitter >= 1 {
		jitter = 0.999999
	}
	return time.Duration(float64(d) * (0.5 + 0.5*jitter))
}
