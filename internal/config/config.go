// Package config loads runtime configuration from environment variables.
// All vars are prefixed HWN_. Required vars cause a fast failure at startup so
// the service never half-starts with a missing HA token or broker address.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime settings. See AGENT.md / .env.example for the env vars.
type Config struct {
	HTTPAddr      string // HWN_HTTP_ADDR, default ":8099"
	PublicBaseURL string // HWN_PUBLIC_BASE_URL, LAN-reachable URL the Sonos fetches
	HABaseURL     string // HWN_HA_BASE_URL
	HAToken       string // HWN_HA_TOKEN (long-lived access token)
	HAMediaPlayer string // HWN_HA_MEDIA_PLAYER, e.g. media_player.bedroom
	MQTTBroker    string // HWN_MQTT_BROKER, e.g. tcp://host:1883
	MQTTUser      string // HWN_MQTT_USER (optional)
	MQTTPass      string // HWN_MQTT_PASS (optional)
	DefaultPreset string // HWN_DEFAULT_PRESET, one of white|pink|brown
	DefaultVolume int    // HWN_DEFAULT_VOLUME, 0-100
	LogLevel      string // HWN_LOG_LEVEL, one of debug|info|warn|error (default info)
}

var validPresets = map[string]bool{"white": true, "pink": true, "brown": true}

// Load reads configuration from the environment, applies defaults, and validates
// required fields. It returns an error listing every problem found so the operator
// can fix them in one pass rather than one-at-a-time.
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:      getEnv("HWN_HTTP_ADDR", ":8099"),
		PublicBaseURL: strings.TrimRight(os.Getenv("HWN_PUBLIC_BASE_URL"), "/"),
		HABaseURL:     strings.TrimRight(os.Getenv("HWN_HA_BASE_URL"), "/"),
		HAToken:       os.Getenv("HWN_HA_TOKEN"),
		HAMediaPlayer: os.Getenv("HWN_HA_MEDIA_PLAYER"),
		MQTTBroker:    os.Getenv("HWN_MQTT_BROKER"),
		MQTTUser:      os.Getenv("HWN_MQTT_USER"),
		MQTTPass:      os.Getenv("HWN_MQTT_PASS"),
		DefaultPreset: getEnv("HWN_DEFAULT_PRESET", "brown"),
		DefaultVolume: 80,
		LogLevel:      getEnv("HWN_LOG_LEVEL", "info"),
	}

	var problems []string

	// Required string vars.
	for name, val := range map[string]string{
		"HWN_PUBLIC_BASE_URL": c.PublicBaseURL,
		"HWN_HA_BASE_URL":     c.HABaseURL,
		"HWN_HA_TOKEN":        c.HAToken,
		"HWN_HA_MEDIA_PLAYER": c.HAMediaPlayer,
		"HWN_MQTT_BROKER":     c.MQTTBroker,
	} {
		if val == "" {
			problems = append(problems, name+" is required")
		}
	}

	if !validPresets[c.DefaultPreset] {
		problems = append(problems, fmt.Sprintf("HWN_DEFAULT_PRESET %q must be white|pink|brown", c.DefaultPreset))
	}

	if v := os.Getenv("HWN_DEFAULT_VOLUME"); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("HWN_DEFAULT_VOLUME %q must be an integer", v))
		case n < 0 || n > 100:
			problems = append(problems, fmt.Sprintf("HWN_DEFAULT_VOLUME %d must be 0-100", n))
		default:
			c.DefaultVolume = n
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
