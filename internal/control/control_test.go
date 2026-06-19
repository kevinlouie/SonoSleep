package control

import (
	"context"
	"testing"
	"time"
)

type fakePlayer struct {
	plays   []play
	stops   int
	volumes []int
}

type play struct {
	preset string
	volume int
}

func (p *fakePlayer) PlayMedia(_ context.Context, preset string, volume int) error {
	p.plays = append(p.plays, play{preset, volume})
	return nil
}
func (p *fakePlayer) VolumeSet(_ context.Context, level int) error {
	p.volumes = append(p.volumes, level)
	return nil
}
func (p *fakePlayer) MediaStop(_ context.Context) error { p.stops++; return nil }

func TestSetOnPlaysAndSetsState(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	if err := s.SetOn(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !s.IsOn() {
		t.Error("IsOn = false after SetOn(true)")
	}
	if len(p.plays) != 1 || p.plays[0] != (play{"brown", 80}) {
		t.Errorf("plays = %v", p.plays)
	}
}

func TestSetOffStopsAndSuppresses(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	fixed := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return fixed }

	_ = s.SetOn(context.Background(), true)
	if err := s.SetOn(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if s.IsOn() {
		t.Error("IsOn = true after SetOn(false)")
	}
	if p.stops != 1 {
		t.Errorf("stops = %d, want 1", p.stops)
	}
	// Watchdog must be suppressed for suppressWindow after a deliberate stop.
	until := s.SuppressedUntil()
	if !until.Equal(fixed.Add(suppressWindow)) {
		t.Errorf("SuppressedUntil = %v, want %v", until, fixed.Add(suppressWindow))
	}
}

func TestReplayNoOpWhenOff(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	if err := s.Replay(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.plays) != 0 {
		t.Errorf("Replay played while OFF: %v", p.plays)
	}
}

func TestReplayPlaysWhenOn(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "pink", 30)
	_ = s.SetOn(context.Background(), true)
	_ = s.Replay(context.Background())
	if len(p.plays) != 2 {
		t.Fatalf("plays = %d, want 2", len(p.plays))
	}
	if p.plays[1] != (play{"pink", 30}) {
		t.Errorf("replay = %+v, want {pink 30}", p.plays[1])
	}
}

func TestSetVolumeClampsAndSnapshots(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	if err := s.SetVolume(context.Background(), 200); err != nil {
		t.Fatal(err)
	}
	if got := p.volumes[len(p.volumes)-1]; got != 100 {
		t.Errorf("volume_set = %d, want 100", got)
	}
	if snap := s.Snapshot(); snap.Volume != 100 {
		t.Errorf("snapshot volume = %d, want 100", snap.Volume)
	}
}

func TestSetPresetRejectsInvalid(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	if err := s.SetPreset(context.Background(), "purple"); err == nil {
		t.Error("expected error for invalid preset")
	}
	if snap := s.Snapshot(); snap.Preset != "brown" {
		t.Errorf("preset = %q, want unchanged brown", snap.Preset)
	}
}
