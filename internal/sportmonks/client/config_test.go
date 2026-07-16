package client

import (
	"strings"
	"testing"
	"time"
)

var configEnvironment = []string{
	"SPORTMONKS_MODE",
	"SPORTMONKS_API_TOKEN",
	"SPORTMONKS_BASE_URL",
	"SPORTMONKS_HTTP_TIMEOUT",
	"SPORTMONKS_QUOTA_RESERVE_PERCENT",
	"SPORTMONKS_HOURLY_REQUEST_LIMIT",
	"SPORTMONKS_FAST_POLLING_ENABLED",
	"SPORTMONKS_ALLOW_LIVE_CORRECTIONS",
	"SPORTMONKS_ALLOW_MID_MATCH_LIVE_ADMISSION",
	"SPORTMONKS_MIN_POLL_INTERVAL",
	"SPORTMONKS_MAX_POLL_INTERVAL",
	"SPORTMONKS_DISCOVERY_INTERVAL",
	"SPORTMONKS_FIXTURE_SYNC_INTERVAL",
	"SPORTMONKS_METADATA_SYNC_INTERVAL",
	"SPORTMONKS_PREMATCH_INTERVAL",
	"SPORTMONKS_BREAK_INTERVAL",
	"SPORTMONKS_FINALIZING_INTERVAL",
	"SPORTMONKS_MAX_CONCURRENCY",
	"SPORTMONKS_LEASE_TTL",
	"SPORTMONKS_STALE_MINIMUM",
	"SPORTMONKS_ACTIVE_FREEZE_TIMEOUT",
	"SPORTMONKS_INNINGS_FINALIZATION_HOLD",
	"SPORTMONKS_MATCH_FINALIZATION_HOLD",
	"SPORTMONKS_RAW_PAYLOAD_TTL",
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
	if cfg.QuotaReservePercent != 20 || cfg.MinPollInterval != 5*time.Second || cfg.MaxPollInterval != 15*time.Second {
		t.Fatalf("unexpected quota/poll defaults: %+v", cfg)
	}
	if cfg.HourlyRequestLimit != 2000 || cfg.FastPollingEnabled || cfg.AllowLiveCorrections || cfg.AllowMidMatchLiveAdmission {
		t.Fatalf("unexpected live-safety defaults: %+v", cfg)
	}
	if cfg.RawPayloadTTL != 30*24*time.Hour {
		t.Fatalf("RawPayloadTTL = %s", cfg.RawPayloadTTL)
	}
}

func TestLoadConfigFromEnvOverrides(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("SPORTMONKS_MODE", " SHADOW ")
	t.Setenv("SPORTMONKS_API_TOKEN", " token-value ")
	t.Setenv("SPORTMONKS_BASE_URL", "https://example.test/cricket/v2")
	t.Setenv("SPORTMONKS_HTTP_TIMEOUT", "9s")
	t.Setenv("SPORTMONKS_QUOTA_RESERVE_PERCENT", "25")
	t.Setenv("SPORTMONKS_MIN_POLL_INTERVAL", "6s")
	t.Setenv("SPORTMONKS_MAX_POLL_INTERVAL", "18s")
	t.Setenv("SPORTMONKS_MAX_CONCURRENCY", "7")
	t.Setenv("SPORTMONKS_MATCH_FINALIZATION_HOLD", "3m")
	t.Setenv("SPORTMONKS_HOURLY_REQUEST_LIMIT", "1800")
	t.Setenv("SPORTMONKS_FAST_POLLING_ENABLED", "true")
	t.Setenv("SPORTMONKS_ALLOW_LIVE_CORRECTIONS", "true")
	t.Setenv("SPORTMONKS_ALLOW_MID_MATCH_LIVE_ADMISSION", "true")

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
	t.Setenv("SPORTMONKS_MODE", "live")

	_, err := LoadConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "SPORTMONKS_API_TOKEN") {
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
		{name: "mode", env: "SPORTMONKS_MODE", value: "write-through", want: "SPORTMONKS_MODE"},
		{name: "duration", env: "SPORTMONKS_HTTP_TIMEOUT", value: "soon", want: "SPORTMONKS_HTTP_TIMEOUT"},
		{name: "quota", env: "SPORTMONKS_QUOTA_RESERVE_PERCENT", value: "100", want: "SPORTMONKS_QUOTA_RESERVE_PERCENT"},
		{name: "concurrency", env: "SPORTMONKS_MAX_CONCURRENCY", value: "0", want: "SPORTMONKS_MAX_CONCURRENCY"},
		{name: "base url query", env: "SPORTMONKS_BASE_URL", value: "https://example.test/v2?api_token=bad", want: "SPORTMONKS_BASE_URL"},
		{name: "insecure remote URL", env: "SPORTMONKS_BASE_URL", value: "http://example.test/v2", want: "https"},
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
	t.Setenv("SPORTMONKS_MIN_POLL_INTERVAL", "20s")
	t.Setenv("SPORTMONKS_MAX_POLL_INTERVAL", "10s")

	_, err := LoadConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "poll intervals") {
		t.Fatalf("error = %v, want poll interval error", err)
	}
}
