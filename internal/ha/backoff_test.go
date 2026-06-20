package ha

import (
	"testing"
	"time"
)

func TestBackoffDelay_ExponentialNoJitter(t *testing.T) {
	base := 500 * time.Millisecond
	max := 30 * time.Second
	// jitter=1 makes the factor (0.5 + 0.5*~1) ≈ 1.0, so delay ≈ the full step.
	const j = 0.999999
	wantSteps := []time.Duration{
		500 * time.Millisecond, // attempt 0
		1 * time.Second,        // attempt 1
		2 * time.Second,        // attempt 2
		4 * time.Second,        // attempt 3
		8 * time.Second,        // attempt 4
		16 * time.Second,       // attempt 5
		30 * time.Second,       // attempt 6 → capped (would be 32s)
		30 * time.Second,       // attempt 7 → still capped
	}
	for attempt, full := range wantSteps {
		got := backoffDelay(attempt, base, max, j)
		// With j≈1 the delay is ~full; allow a tiny epsilon below it.
		if got > full || got < time.Duration(float64(full)*0.999) {
			t.Errorf("attempt %d: delay = %v, want ≈ %v", attempt, got, full)
		}
	}
}

func TestBackoffDelay_JitterBounds(t *testing.T) {
	base := time.Second
	max := time.Minute
	for attempt := 0; attempt < 8; attempt++ {
		// Uncapped step for this attempt (doubling), capped at max.
		step := base
		for i := 0; i < attempt; i++ {
			step *= 2
			if step > max {
				step = max
			}
		}
		lo := backoffDelay(attempt, base, max, 0.0)    // factor 0.5
		hi := backoffDelay(attempt, base, max, 0.9999) // factor ≈ 1.0
		if lo < time.Duration(float64(step)*0.5)-time.Millisecond || lo > step {
			t.Errorf("attempt %d: lo bound %v out of [0.5*step, step] (step=%v)", attempt, lo, step)
		}
		if hi > step {
			t.Errorf("attempt %d: hi bound %v exceeds step %v (cap violated)", attempt, hi, step)
		}
		if lo > hi {
			t.Errorf("attempt %d: lo %v > hi %v", attempt, lo, hi)
		}
	}
}

func TestBackoffDelay_JitterClamped(t *testing.T) {
	// Out-of-range jitter must not panic or exceed the cap.
	d := backoffDelay(2, time.Second, time.Minute, 5.0)
	if d <= 0 || d > 4*time.Second {
		t.Errorf("clamped-jitter delay = %v, want within (0, 4s]", d)
	}
	if got := backoffDelay(2, time.Second, time.Minute, -1.0); got <= 0 {
		t.Errorf("negative-jitter delay = %v, want > 0", got)
	}
}
