# Spec: Noise DSP (ported from reSpeakerSleep)

Port the algorithms proven in `../reSpeakerSleep` (ESP32 firmware). Those were tuned to
sound right and avoid clipping/stutter — do not invent new ones. Search that repo's
`esphome/` / `components/noise_gen/` for the reference C++.

## Output format
- Sample rate: **48000 Hz**, **stereo**, **16-bit signed little-endian (s16le)**.
- Generate in blocks (e.g. 4096 frames) for steady throughput into ffmpeg's stdin.
- Independent L/R noise (or decorrelated) is fine and sounds wider; mono-duplicated is
  acceptable for v1.

## White
- Per sample: uniform or Gaussian white in [-1, 1]. A fast xorshift/PCG RNG is enough;
  no crypto RNG. Scale to int16 with headroom (target RMS ~0.2–0.3 full-scale to leave
  room before the MP3 encoder, avoid inter-sample peaks clipping).

## Brown (the primary preset)
- reSpeakerSleep final design: **one-pole lowpass on white**, `fc ≈ 115 Hz`, normalized to
  **RMS ≈ 0.3**. (The earlier leaky-integrator version clipped constantly → perceived
  stutter; it was rejected. Use the lowpass form.)
- One-pole: `y[n] = y[n-1] + a * (x[n] - y[n-1])`, where
  `a = 1 - exp(-2*pi*fc/fs)` ≈ `2*pi*115/48000`. Then normalize block RMS to target.

## Pink
- **Paul Kellet** filter (the economical 7-coefficient version) on white. Standard
  coefficients; normalize RMS to a comfortable level similar to brown.

## Volume
- Default volume is applied **Sonos-side** (`volume_set`), not in the PCM. Keep generated
  RMS constant per preset. (Only add a server-side gain stage if a real need appears.)
- Default volume 80 (matches reSpeakerSleep default).

## Optional: fade-in
- reSpeakerSleep ramps gain over ~3 s on start. Optional here (Phase 5). If added, apply
  as a server-side gain envelope at stream start.

## Tests
- RMS of each preset within tolerance of its target.
- Spectral check: brown has substantially more energy below ~200 Hz than white (FFT a
  block, compare low-band/high-band energy ratio). Pink between the two.
- No int16 overflow/wraparound in generated blocks.
