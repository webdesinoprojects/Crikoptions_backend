package client

import (
	"strings"
	"testing"
	"time"
)

var configEnvironment = []string{
	"CRICLIVE_MODE",
	"CRICLIVE_API_TOKEN",
	"CRICLIVE_BASE_URL",
	"CRICLIVE_HTTP_TIMEOUT",
	"CRICLIVE_QUOTA_RESERVE_PERCENT",
	"CRICLIVE_HOURLY_REQUEST_LIMIT",
	"CRICLIVE_FAST_POLLING_ENABLED",
	"CRICLIVE_ALLOW_LIVE_CORRECTIONS",
	"CRICLIVE_ALLOW_MID_MATCH_LIVE_ADMISSION",
	"CRICLIVE_MIN_POLL_INTERVAL",
	"CRICLIVE_MAX_POLL_INTERVAL",
	"CRICLIVE_DISCOVERY_INTERVAL",
	"CRICLIVE_FIXTURE_SYNC_INTERVAL",
	"CRICLIVE_PREMATCH_INTERVAL",
	"CRICLIVE_BREAK_INTERVAL",
	"CRICLIVE_FINALIZING_INTERVAL",
	"CRICLIVE_MAX_CONCURRENCY",
	"CRICLIVE_LEASE_TTL",
	"CRICLIVE_STALE_MINIMUM",
	"CRICLIVE_ACTIVE_FREEZE_TIMEOUT",
	"CRICLIVE_INNINGS_FINALIZATION_HOLD",
	"CRICLIVE_MATCH_FINALIZATION_HOLD",
	"CRICLIVE_RAW_PAYLOAD_TTL",
}

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range configEnvironment {
		t.Setenv(name, "")
	}
}

func TestLoadConfigFromEnvDefaultsToOff(t *testing.T) {
	clearConfigEnvironment(t)

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if cfg.Mode != ModeOff {
		t.Fatalf("Mode = %q, want %q", cfg.Mode, ModeOff)
	}
	if cfg.BaseURL != DefaultBaseURL {
		t.Fatalf("BaseURL = %q, want %q", cfg.BaseURL, DefaultBaseURL)
	}
	if cfg.QuotaReservePercent != 20 || cfg.MinPollInterval != 2*time.Second || cfg.MaxPollInterval != 6*time.Second {
		t.Fatalf("unexpected quota/poll defaults: %+v", cfg)
	}
	// The hourly guard is deliberately small: CricLive meters a daily
	// allowance (5,000/day on the base plan), so an hourly ceiling sized for a
	// per-hour API would exhaust a whole day's budget in under two hours.
	if cfg.HourlyRequestLimit != 200 || cfg.FastPollingEnabled || cfg.AllowLiveCorrections || cfg.AllowMidMatchLiveAdmission {
		t.Fatalf("unexpected live-safety defaults: %+v", cfg)
	}
	if cfg.RawPayloadTTL != 2*time.Hour {
		t.Fatalf("RawPayloadTTL = %s", cfg.RawPayloadTTL)
	}
}

func TestLoadConfigFromEnvOverrides(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("CRICLIVE_MODE", " SHADOW ")
	t.Setenv("CRICLIVE_API_TOKEN", " token-value ")
	t.Setenv("CRICLIVE_BASE_URL", "https://example.test/cricket/v2")
	t.Setenv("CRICLIVE_HTTP_TIMEOUT", "9s")
	t.Setenv("CRICLIVE_QUOTA_RESERVE_PERCENT", "25")
	t.Setenv("CRICLIVE_MIN_POLL_INTERVAL", "6s")
	t.Setenv("CRICLIVE_MAX_POLL_INTERVAL", "18s")
	t.Setenv("CRICLIVE_MAX_CONCURRENCY", "7")
	t.Setenv("CRICLIVE_MATCH_FINALIZATION_HOLD", "3m")
	t.Setenv("CRICLIVE_HOURLY_REQUEST_LIMIT", "1800")
	t.Setenv("CRICLIVE_FAST_POLLING_ENABLED", "true")
	t.Setenv("CRICLIVE_ALLOW_LIVE_CORRECTIONS", "true")
	t.Setenv("CRICLIVE_ALLOW_MID_MATCH_LIVE_ADMISSION", "true")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if cfg.Mode != ModeShadow || cfg.APIToken != "token-value" {
		t.Fatalf("unexpected mode/token: %q / %q", cfg.Mode, cfg.APIToken)
	}
	if cfg.BaseURL != "https://example.test/cricket/v2" || cfg.HTTPTimeout != 9*time.Second {
		t.Fatalf("unexpected transport config: %+v", cfg)
	}
	if cfg.QuotaReservePercent != 25 || cfg.MinPollInterval != 6*time.Second || cfg.MaxPollInterval != 18*time.Second {
		t.Fatalf("unexpected quota/poll config: %+v", cfg)
	}
	if cfg.MaxConcurrency != 7 || cfg.MatchFinalizationHold != 3*time.Minute {
		t.Fatalf("unexpected concurrency/finalization config: %+v", cfg)
	}
	if cfg.HourlyRequestLimit != 1800 || !cfg.FastPollingEnabled || !cfg.AllowLiveCorrections || !cfg.AllowMidMatchLiveAdmission {
		t.Fatalf("unexpected live-safety overrides: %+v", cfg)
	}
}

func TestLoadConfigFromEnvRequiresTokenWhenEnabled(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("CRICLIVE_MODE", "live")

	_, err := LoadConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "CRICLIVE_API_TOKEN") {
		t.Fatalf("error = %v, want missing token error", err)
	}
}

func TestLoadConfigFromEnvRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		value string
		want  string
	}{
		{name: "mode", env: "CRICLIVE_MODE", value: "write-through", want: "CRICLIVE_MODE"},
		{name: "duration", env: "CRICLIVE_HTTP_TIMEOUT", value: "soon", want: "CRICLIVE_HTTP_TIMEOUT"},
		{name: "quota", env: "CRICLIVE_QUOTA_RESERVE_PERCENT", value: "100", want: "CRICLIVE_QUOTA_RESERVE_PERCENT"},
		{name: "concurrency", env: "CRICLIVE_MAX_CONCURRENCY", value: "0", want: "CRICLIVE_MAX_CONCURRENCY"},
		{name: "base url query", env: "CRICLIVE_BASE_URL", value: "https://example.test/v2?api_token=bad", want: "CRICLIVE_BASE_URL"},
		{name: "insecure remote URL", env: "CRICLIVE_BASE_URL", value: "http://example.test/v2", want: "https"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv(test.env, test.value)
			_, err := LoadConfigFromEnv()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want mention of %s", err, test.want)
			}
		})
	}
}

func TestLoadConfigFromEnvRejectsReversedPollRange(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("CRICLIVE_MIN_POLL_INTERVAL", "20s")
	t.Setenv("CRICLIVE_MAX_POLL_INTERVAL", "10s")

	_, err := LoadConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "poll intervals") {
		t.Fatalf("error = %v, want poll interval error", err)
	}
}
