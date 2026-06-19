package config

import (
	"strings"
	"testing"
)

// setRequired sets the minimum env for a valid Load, then runs fn which may
// override or clear individual vars. t.Setenv auto-restores after the test.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("HWN_PUBLIC_BASE_URL", "http://192.168.1.50:8099")
	t.Setenv("HWN_HA_BASE_URL", "http://homeassistant.local:8123")
	t.Setenv("HWN_HA_TOKEN", "tok")
	t.Setenv("HWN_HA_MEDIA_PLAYER", "media_player.bedroom")
	t.Setenv("HWN_MQTT_BROKER", "tcp://192.168.1.50:1883")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.HTTPAddr != ":8099" {
		t.Errorf("HTTPAddr = %q, want :8099", c.HTTPAddr)
	}
	if c.DefaultPreset != "brown" {
		t.Errorf("DefaultPreset = %q, want brown", c.DefaultPreset)
	}
	if c.DefaultVolume != 80 {
		t.Errorf("DefaultVolume = %d, want 80", c.DefaultVolume)
	}
}

func TestLoadTrimsTrailingSlash(t *testing.T) {
	setRequired(t)
	t.Setenv("HWN_PUBLIC_BASE_URL", "http://host:8099/")
	t.Setenv("HWN_HA_BASE_URL", "http://ha:8123/")
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.PublicBaseURL != "http://host:8099" {
		t.Errorf("PublicBaseURL = %q, want no trailing slash", c.PublicBaseURL)
	}
	if c.HABaseURL != "http://ha:8123" {
		t.Errorf("HABaseURL = %q, want no trailing slash", c.HABaseURL)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	// Clear everything; expect each required var named in the error.
	for _, k := range []string{"HWN_PUBLIC_BASE_URL", "HWN_HA_BASE_URL", "HWN_HA_TOKEN", "HWN_HA_MEDIA_PLAYER", "HWN_MQTT_BROKER"} {
		t.Setenv(k, "")
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing required vars, got nil")
	}
	for _, want := range []string{"HWN_HA_TOKEN", "HWN_MQTT_BROKER"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadInvalidPresetAndVolume(t *testing.T) {
	setRequired(t)
	t.Setenv("HWN_DEFAULT_PRESET", "purple")
	t.Setenv("HWN_DEFAULT_VOLUME", "150")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid preset/volume")
	}
	if !strings.Contains(err.Error(), "HWN_DEFAULT_PRESET") {
		t.Errorf("error %q missing preset complaint", err)
	}
	if !strings.Contains(err.Error(), "HWN_DEFAULT_VOLUME") {
		t.Errorf("error %q missing volume complaint", err)
	}
}
