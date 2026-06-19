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
