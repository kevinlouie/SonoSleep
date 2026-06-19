package noise

import (
	"math"
	"testing"
)

// decode renders n frames and returns the L-channel samples normalized back to
// [-1, 1] (value / fullScale). Verifies no int16 overflow along the way.
func decode(t *testing.T, g *Generator, frames int) []float64 {
	t.Helper()
	buf := make([]byte, frames*BytesPerFrame)
	g.Fill(buf, frames)
	out := make([]float64, frames)
	for i := 0; i < frames; i++ {
		o := i * BytesPerFrame
		l := int16(uint16(buf[o]) | uint16(buf[o+1])<<8)
		r := int16(uint16(buf[o+2]) | uint16(buf[o+3])<<8)
		if l != r {
			t.Fatalf("frame %d: L=%d != R=%d (expected mono-duplicated)", i, l, r)
		}
		// |sample| must never reach full int16 range — we clamp pre-scale by 0.95.
		if l == math.MinInt16 || l == math.MaxInt16 {
			t.Fatalf("frame %d: sample %d hit int16 rail (clip)", i, l)
		}
		out[i] = float64(l) / fullScale
	}
	return out
}

func rms(s []float64) float64 {
	var sum float64
	for _, v := range s {
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(s)))
}

// TestRMSInSaneBand checks each preset sits in a documented, generous level
// band. Brown/white are tuned analytically (≈0.30 / ≈0.25); pink keeps the
// reference *0.11 scaling — its exact RMS is confirmed on the first human
// `go test` run, so the band is wide. See decisions log.
func TestRMSInSaneBand(t *testing.T) {
	const frames = 48000 // 1 s
	for _, p := range []Preset{White, Pink, Brown} {
		g := New(p)
		// Discard filter warm-up transient.
		decode(t, g, 4800)
		r := rms(decode(t, g, frames))
		if r < 0.10 || r > 0.45 {
			t.Errorf("%s RMS = %.3f, want within [0.10, 0.45]", p, r)
		}
		t.Logf("%s RMS = %.3f", p, r)
	}
}

// lowBandFraction returns the fraction of spectral energy below cutoffHz,
// computed with a naive real DFT over n samples.
func lowBandFraction(s []float64, cutoffHz float64) float64 {
	n := len(s)
	var low, total float64
	// Only need bins up to Nyquist; energy is symmetric so half-spectrum is fine.
	for k := 1; k < n/2; k++ {
		var re, im float64
		w := -2.0 * math.Pi * float64(k) / float64(n)
		for i, v := range s {
			re += v * math.Cos(w*float64(i))
			im += v * math.Sin(w*float64(i))
		}
		mag := re*re + im*im
		freq := float64(k) * SampleRate / float64(n)
		total += mag
		if freq < cutoffHz {
			low += mag
		}
	}
	if total == 0 {
		return 0
	}
	return low / total
}

// TestSpectralOrdering is the load-bearing, gain-independent check: brown must
// have substantially more low-frequency energy than pink, and pink more than
// white. (Below 500 Hz.)
func TestSpectralOrdering(t *testing.T) {
	const n = 2048
	frac := func(p Preset) float64 {
		g := New(p)
		decode(t, g, 4800) // warm-up
		return lowBandFraction(decode(t, g, n), 500.0)
	}
	white := frac(White)
	pink := frac(Pink)
	brown := frac(Brown)
	t.Logf("low-band (<500Hz) energy fraction: white=%.3f pink=%.3f brown=%.3f", white, pink, brown)

	if !(brown > pink) {
		t.Errorf("expected brown low-band fraction (%.3f) > pink (%.3f)", brown, pink)
	}
	if !(pink > white) {
		t.Errorf("expected pink low-band fraction (%.3f) > white (%.3f)", pink, white)
	}
	// Brown should be dominated by low frequencies.
	if brown < 0.5 {
		t.Errorf("brown low-band fraction = %.3f, expected > 0.5", brown)
	}
}

func TestParsePreset(t *testing.T) {
	cases := map[string]Preset{
		"white": White, "Pink": Pink, "BROWN": Brown, " brown ": Brown,
	}
	for in, want := range cases {
		got, err := ParsePreset(in)
		if err != nil {
			t.Errorf("ParsePreset(%q) error: %v", in, err)
		}
		if got != want {
			t.Errorf("ParsePreset(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParsePreset("grey"); err == nil {
		t.Error("ParsePreset(grey) expected error, got nil")
	}
}

func TestPresetString(t *testing.T) {
	for _, p := range []Preset{White, Pink, Brown} {
		got, err := ParsePreset(p.String())
		if err != nil || got != p {
			t.Errorf("round-trip %v -> %q failed: got %v err %v", p, p.String(), got, err)
		}
	}
}

func TestVolumeGain(t *testing.T) {
	cases := map[int]float64{-10: 0.0, 0: 0.0, 50: 0.5, 80: 0.8, 100: 1.0, 200: 1.0}
	for level, want := range cases {
		if got := VolumeGain(level); math.Abs(got-want) > 1e-9 {
			t.Errorf("VolumeGain(%d) = %v, want %v", level, got, want)
		}
	}
}
