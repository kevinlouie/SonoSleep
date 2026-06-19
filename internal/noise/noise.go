// Package noise synthesizes continuous white / pink / brown noise as raw PCM
// (s16le, stereo, 48 kHz) suitable for piping into ffmpeg's stdin.
//
// The DSP is ported verbatim from ../reSpeakerSleep (ESP32 firmware,
// components/noise_gen). Those filters and gains were tuned by ear to sound
// right and avoid clipping/stutter — do not re-derive them. The fixed gains
// (brown *6, pink *0.11, white *0.433) preserve the reference's relative levels.
package noise

import (
	"fmt"
	"strings"
)

// PCM output format. Matches specs/brown-noise-dsp.md and what ffmpeg is told
// to expect (-f s16le -ar 48000 -ac 2).
const (
	SampleRate     = 48000
	Channels       = 2
	BytesPerSample = 2 // int16
	BytesPerFrame  = Channels * BytesPerSample
)

// fullScale leaves ~5% headroom below int16 max so the MP3 encoder / amp never
// clips on inter-sample peaks (reference used the same 0.95 factor).
const fullScale = 0.95 * 32767.0

// Preset selects the noise colour.
type Preset uint8

const (
	White Preset = iota
	Pink
	Brown
)

func (p Preset) String() string {
	switch p {
	case White:
		return "white"
	case Pink:
		return "pink"
	case Brown:
		return "brown"
	default:
		return "unknown"
	}
}

// ParsePreset maps a preset name (case-insensitive: white/pink/brown) to a
// Preset. Unknown names are an error.
func ParsePreset(s string) (Preset, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "white":
		return White, nil
	case "pink":
		return Pink, nil
	case "brown":
		return Brown, nil
	default:
		return 0, fmt.Errorf("unknown preset %q (want white, pink or brown)", s)
	}
}

// Per-preset post-filter gains, ported from reSpeakerSleep. These set the
// absolute level; the relative balance between presets is the by-ear tuning.
//
//	white: uniform white RMS = 1/sqrt(3) ≈ 0.577; ×0.433 → RMS ≈ 0.25
//	       (spec-mandated headroom so the encoder has room).
//	brown: one-pole-lowpass output std ≈ 0.0502; ×6 → RMS ≈ 0.30.
//	pink:  Kellet sum ×0.11 (reference value, kept faithfully).
const (
	whiteGain = 0.4330 // 0.25 / (1/sqrt(3))
	brownGain = 6.0
	pinkScale = 0.11
	brownCoef = 0.015 // one-pole a → fc = a*fs/2π ≈ 115 Hz
)

// Generator produces an endless stream of one preset. It is NOT safe for
// concurrent use; create one per stream connection. To change preset mid-stream
// the caller would tear down and recreate (the HTTP design uses preset per URL).
type Generator struct {
	preset Preset
	rng    uint32     // xorshift32 state
	pinkB  [7]float64 // Paul Kellet filter state
	brown  float64    // one-pole lowpass state
}

// New returns a Generator for the given preset, seeded deterministically.
func New(p Preset) *Generator {
	return &Generator{preset: p, rng: 0x9E3779B9}
}

// white returns the next raw white sample, uniform in [-1, 1). xorshift32 RNG —
// fast and more than adequate for audio noise (no crypto needed).
func (g *Generator) white() float64 {
	x := g.rng
	x ^= x << 13
	x ^= x >> 17
	x ^= x << 5
	g.rng = x
	return float64(int32(x)) * (1.0 / 2147483648.0)
}

// sample returns the next mono sample for the configured preset, post-gain,
// clamped to [-1, 1]. This is the value before int16 scaling.
func (g *Generator) sample() float64 {
	w := g.white()
	var s float64
	switch g.preset {
	case Pink:
		// Paul Kellet's refined pink-noise filter (7 coefficients).
		b := &g.pinkB
		b[0] = 0.99886*b[0] + w*0.0555179
		b[1] = 0.99332*b[1] + w*0.0750759
		b[2] = 0.96900*b[2] + w*0.1538520
		b[3] = 0.86650*b[3] + w*0.3104856
		b[4] = 0.55000*b[4] + w*0.5329522
		b[5] = -0.7616*b[5] - w*0.0168980
		s = (b[0] + b[1] + b[2] + b[3] + b[4] + b[5] + b[6] + w*0.5362) * pinkScale
		b[6] = w * 0.115926
	case Brown:
		// One-pole lowpass on white (-6 dB/oct, fc ≈ 115 Hz). Keeps RMS ≈ 0.30
		// so peaks rarely clip — the old leaky integrator railed and stuttered.
		g.brown += brownCoef * (w - g.brown)
		s = g.brown * brownGain
	default: // White
		s = w * whiteGain
	}
	if s > 1.0 {
		s = 1.0
	} else if s < -1.0 {
		s = -1.0
	}
	return s
}

// Fill writes `frames` stereo s16le frames into buf. buf must be at least
// frames*BytesPerFrame bytes. The same mono sample is written to L and R
// (mono-duplicated; acceptable for v1 per spec). Returns bytes written.
func (g *Generator) Fill(buf []byte, frames int) int {
	n := frames * BytesPerFrame
	for i := 0; i < frames; i++ {
		v := int16(g.sample() * fullScale)
		lo := byte(v)
		hi := byte(uint16(v) >> 8)
		o := i * BytesPerFrame
		buf[o] = lo   // L low
		buf[o+1] = hi // L high
		buf[o+2] = lo // R low
		buf[o+3] = hi // R high
	}
	return n
}

// VolumeGain maps a 0–100 volume level to a linear 0.0–1.0 gain. This is the
// value passed to Home Assistant's media_player.volume_set — volume is applied
// Sonos-side, NOT in the PCM (the generated RMS stays constant per preset).
func VolumeGain(level int) float64 {
	if level < 0 {
		level = 0
	} else if level > 100 {
		level = 100
	}
	return float64(level) / 100.0
}
