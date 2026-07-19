package client

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://cricket.sportmonks.com/api/v2.0"

type Mode string

const (
	ModeOff    Mode = "off"
	ModeShadow Mode = "shadow"
	ModeLive   Mode = "live"
)

// Config contains both HTTP-client settings and the provider timing defaults
// shared by feedworker components. Durations are deliberately parsed here so
// command entrypoints do not each grow their own subtly different env parser.
type Config struct {
	Mode        Mode
	APIToken    string
	BaseURL     string
	HTTPTimeout time.Duration

	QuotaReservePercent        int
	HourlyRequestLimit         int
	FastPollingEnabled         bool
	AllowLiveCorrections       bool
	AllowMidMatchLiveAdmission bool
	MinPollInterval            time.Duration
	MaxPollInterval            time.Duration
	DiscoveryInterval          time.Duration
	FixtureSyncInterval        time.Duration
	MetadataSyncInterval       time.Duration
	PreMatchInterval           time.Duration
	BreakInterval              time.Duration
	FinalizingInterval         time.Duration
	MaxConcurrency             int
	LeaseTTL                   time.Duration
	StaleMinimum               time.Duration
	ActiveFreezeTimeout        time.Duration
	InningsFinalizationHold    time.Duration
	MatchFinalizationHold      time.Duration
	RawPayloadTTL              time.Duration
}

// LoadConfigFromEnv loads Sportmonks settings without reading a dotenv file.
// The application's top-level config loader remains responsible for dotenv
// handling. A token is mandatory only when provider operation is enabled.
func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		Mode:                    ModeOff,
		BaseURL:                 DefaultBaseURL,
		HTTPTimeout:             15 * time.Second,
		QuotaReservePercent:     20,
		HourlyRequestLimit:      2000,
		MinPollInterval:         2 * time.Second,
		MaxPollInterval:         6 * time.Second,
		DiscoveryInterval:       30 * time.Second,
		FixtureSyncInterval:     6 * time.Hour,
		MetadataSyncInterval:    24 * time.Hour,
		PreMatchInterval:        2 * time.Minute,
		BreakInterval:           time.Minute,
		FinalizingInterval:      15 * time.Second,
		MaxConcurrency:          4,
		LeaseTTL:                30 * time.Second,
		StaleMinimum:            60 * time.Second,
		ActiveFreezeTimeout:     5 * time.Minute,
		InningsFinalizationHold: time.Minute,
		MatchFinalizationHold:   2 * time.Minute,
		RawPayloadTTL:           2 * time.Hour,
	}

	if value := strings.ToLower(strings.TrimSpace(os.Getenv("SPORTMONKS_MODE"))); value != "" {
		cfg.Mode = Mode(value)
	}
	cfg.APIToken = strings.TrimSpace(os.Getenv("SPORTMONKS_API_TOKEN"))
	if value := strings.TrimSpace(os.Getenv("SPORTMONKS_BASE_URL")); value != "" {
		cfg.BaseURL = value
	}

	durations := []struct {
		name   string
		target *time.Duration
	}{
		{"SPORTMONKS_HTTP_TIMEOUT", &cfg.HTTPTimeout},
		{"SPORTMONKS_MIN_POLL_INTERVAL", &cfg.MinPollInterval},
		{"SPORTMONKS_MAX_POLL_INTERVAL", &cfg.MaxPollInterval},
		{"SPORTMONKS_DISCOVERY_INTERVAL", &cfg.DiscoveryInterval},
		{"SPORTMONKS_FIXTURE_SYNC_INTERVAL", &cfg.FixtureSyncInterval},
		{"SPORTMONKS_METADATA_SYNC_INTERVAL", &cfg.MetadataSyncInterval},
		{"SPORTMONKS_PREMATCH_INTERVAL", &cfg.PreMatchInterval},
		{"SPORTMONKS_BREAK_INTERVAL", &cfg.BreakInterval},
		{"SPORTMONKS_FINALIZING_INTERVAL", &cfg.FinalizingInterval},
		{"SPORTMONKS_LEASE_TTL", &cfg.LeaseTTL},
		{"SPORTMONKS_STALE_MINIMUM", &cfg.StaleMinimum},
		{"SPORTMONKS_ACTIVE_FREEZE_TIMEOUT", &cfg.ActiveFreezeTimeout},
		{"SPORTMONKS_INNINGS_FINALIZATION_HOLD", &cfg.InningsFinalizationHold},
		{"SPORTMONKS_MATCH_FINALIZATION_HOLD", &cfg.MatchFinalizationHold},
		{"SPORTMONKS_RAW_PAYLOAD_TTL", &cfg.RawPayloadTTL},
	}
	for _, setting := range durations {
		if err := parsePositiveDurationEnv(setting.name, setting.target); err != nil {
			return Config{}, err
		}
	}

	if err := parseIntEnv("SPORTMONKS_QUOTA_RESERVE_PERCENT", &cfg.QuotaReservePercent); err != nil {
		return Config{}, err
	}
	if err := parseIntEnv("SPORTMONKS_MAX_CONCURRENCY", &cfg.MaxConcurrency); err != nil {
		return Config{}, err
	}
	if err := parseIntEnv("SPORTMONKS_HOURLY_REQUEST_LIMIT", &cfg.HourlyRequestLimit); err != nil {
		return Config{}, err
	}
	cfg.AllowLiveCorrections = parseBoolEnv("SPORTMONKS_ALLOW_LIVE_CORRECTIONS")
	cfg.AllowMidMatchLiveAdmission = parseBoolEnv("SPORTMONKS_ALLOW_MID_MATCH_LIVE_ADMISSION")
	// Live mode always uses fast polling for ball-by-ball trading UX. The env flag
	// only applies in shadow mode (quota-sensitive dry runs).
	if cfg.Mode == ModeLive {
		cfg.FastPollingEnabled = true
	} else if raw, ok := os.LookupEnv("SPORTMONKS_FAST_POLLING_ENABLED"); ok {
		cfg.FastPollingEnabled = parseBoolValue(raw)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	switch c.Mode {
	case ModeOff, ModeShadow, ModeLive:
	default:
		return fmt.Errorf("SPORTMONKS_MODE must be one of off, shadow, or live")
	}
	if c.Mode != ModeOff && strings.TrimSpace(c.APIToken) == "" {
		return errors.New("SPORTMONKS_API_TOKEN is required when SPORTMONKS_MODE is shadow or live")
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("SPORTMONKS_BASE_URL must not be empty")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("SPORTMONKS_BASE_URL must be an absolute http or https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("SPORTMONKS_BASE_URL must not contain credentials, query parameters, or a fragment")
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return errors.New("SPORTMONKS_BASE_URL must use https outside loopback development")
	}
	if c.HTTPTimeout <= 0 {
		return errors.New("SPORTMONKS_HTTP_TIMEOUT must be positive")
	}
	if c.QuotaReservePercent < 0 || c.QuotaReservePercent >= 100 {
		return errors.New("SPORTMONKS_QUOTA_RESERVE_PERCENT must be between 0 and 99")
	}
	if c.HourlyRequestLimit <= 0 {
		return errors.New("SPORTMONKS_HOURLY_REQUEST_LIMIT must be positive")
	}
	if c.MinPollInterval <= 0 || c.MaxPollInterval <= 0 || c.MinPollInterval > c.MaxPollInterval {
		return errors.New("Sportmonks poll intervals must be positive and minimum must not exceed maximum")
	}
	positiveDurations := []struct {
		name  string
		value time.Duration
	}{
		{"SPORTMONKS_DISCOVERY_INTERVAL", c.DiscoveryInterval},
		{"SPORTMONKS_FIXTURE_SYNC_INTERVAL", c.FixtureSyncInterval},
		{"SPORTMONKS_METADATA_SYNC_INTERVAL", c.MetadataSyncInterval},
		{"SPORTMONKS_PREMATCH_INTERVAL", c.PreMatchInterval},
		{"SPORTMONKS_BREAK_INTERVAL", c.BreakInterval},
		{"SPORTMONKS_FINALIZING_INTERVAL", c.FinalizingInterval},
		{"SPORTMONKS_LEASE_TTL", c.LeaseTTL},
		{"SPORTMONKS_STALE_MINIMUM", c.StaleMinimum},
		{"SPORTMONKS_ACTIVE_FREEZE_TIMEOUT", c.ActiveFreezeTimeout},
		{"SPORTMONKS_INNINGS_FINALIZATION_HOLD", c.InningsFinalizationHold},
		{"SPORTMONKS_MATCH_FINALIZATION_HOLD", c.MatchFinalizationHold},
		{"SPORTMONKS_RAW_PAYLOAD_TTL", c.RawPayloadTTL},
	}
	for _, setting := range positiveDurations {
		if setting.value <= 0 {
			return fmt.Errorf("%s must be positive", setting.name)
		}
	}
	if c.MaxConcurrency <= 0 {
		return errors.New("SPORTMONKS_MAX_CONCURRENCY must be positive")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

func parsePositiveDurationEnv(name string, target *time.Duration) error {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fmt.Errorf("%s must be a positive duration", name)
	}
	*target = duration
	return nil
}

func parseIntEnv(name string, target *int) error {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%s must be an integer", name)
	}
	*target = n
	return nil
}

func parseBoolEnv(name string) bool {
	return parseBoolValue(os.Getenv(name))
}

func parseBoolValue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
