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

const DefaultBaseURL = "https://cricketliveapi.com/api/v1"

// DefaultCricketLineBaseURL is the CricketLineApi host. Its paths start at /api.
const DefaultCricketLineBaseURL = "https://apis.cricketlineapi.com"

// Feed names the upstream a feed pipeline reads. The pipeline (worker, reducer,
// store) is shared; only the provider adapter differs.
type Feed string

const (
	FeedCricketLine Feed = "cricketline"
	FeedCricLive    Feed = "criclive"
)

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
	Mode Mode
	// Feed selects the provider adapter. APIToken/BaseURL belong to CricLive,
	// CricketLineAPIKey/CricketLineBaseURL to CricketLineApi; every other
	// setting tunes the shared pipeline whichever feed is selected.
	Feed               Feed
	APIToken           string
	BaseURL            string
	CricketLineAPIKey  string
	CricketLineBaseURL string
	HTTPTimeout        time.Duration

	QuotaReservePercent        int
	HourlyRequestLimit         int
	DailyRequestLimit          int
	FastPollingEnabled         bool
	AllowLiveCorrections       bool
	AllowMidMatchLiveAdmission bool
	MinPollInterval            time.Duration
	MaxPollInterval            time.Duration
	DiscoveryInterval          time.Duration
	FixtureSyncInterval        time.Duration
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

// LoadConfigFromEnv loads CricLive settings without reading a dotenv file.
// The application's top-level config loader remains responsible for dotenv
// handling. A token is mandatory only when provider operation is enabled.
func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		Mode:                ModeOff,
		Feed:                FeedCricketLine,
		BaseURL:             DefaultBaseURL,
		CricketLineBaseURL:  DefaultCricketLineBaseURL,
		HTTPTimeout:         15 * time.Second,
		QuotaReservePercent: 20,
		// CricLive meters a fixed number of calls per calendar day, so the
		// daily figure is the real ceiling and the hourly one is only a burst
		// limiter. The hourly value must stay high enough to poll a match in
		// progress; the daily value is what keeps the plan from being spent.
		HourlyRequestLimit: 900,
		DailyRequestLimit:  5000,
		// A live poll is one /cricket/overs request. At 6s a T20 costs ~2,100
		// requests, which a 5,000/day plan can carry alongside discovery; the
		// previous 2s cadence needed ~6,300 for the same match.
		MinPollInterval: 6 * time.Second,
		MaxPollInterval: 10 * time.Second,
		// One global call covers every match, and it is what notices a
		// fixture going live, so it is the last thing to economise on.
		DiscoveryInterval:   60 * time.Second,
		FixtureSyncInterval: 6 * time.Hour,
		// A fixture that has not started tells us nothing new minute to
		// minute; /cricket/live discovery is what notices it going live.
		PreMatchInterval:        15 * time.Minute,
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

	if value := strings.ToLower(strings.TrimSpace(os.Getenv("CRICLIVE_MODE"))); value != "" {
		cfg.Mode = Mode(value)
	}
	cfg.APIToken = strings.TrimSpace(os.Getenv("CRICLIVE_API_TOKEN"))
	if value := strings.TrimSpace(os.Getenv("CRICLIVE_BASE_URL")); value != "" {
		cfg.BaseURL = value
	}
	cfg.CricketLineAPIKey = strings.TrimSpace(os.Getenv("CRICKETLINE_API_KEY"))
	if value := strings.TrimSpace(os.Getenv("CRICKETLINE_BASE_URL")); value != "" {
		cfg.CricketLineBaseURL = value
	}
	// An explicit LIVE_FEED_PROVIDER wins. Without one the feed follows the
	// credential that is configured, so a deployment that only ever had a
	// CricLive token keeps booting on CricLive until a CricketLine key is added.
	if value := strings.ToLower(strings.TrimSpace(os.Getenv("LIVE_FEED_PROVIDER"))); value != "" {
		cfg.Feed = Feed(value)
	} else if cfg.CricketLineAPIKey == "" && cfg.APIToken != "" {
		cfg.Feed = FeedCricLive
	}

	durations := []struct {
		name   string
		target *time.Duration
	}{
		{"CRICLIVE_HTTP_TIMEOUT", &cfg.HTTPTimeout},
		{"CRICLIVE_MIN_POLL_INTERVAL", &cfg.MinPollInterval},
		{"CRICLIVE_MAX_POLL_INTERVAL", &cfg.MaxPollInterval},
		{"CRICLIVE_DISCOVERY_INTERVAL", &cfg.DiscoveryInterval},
		{"CRICLIVE_FIXTURE_SYNC_INTERVAL", &cfg.FixtureSyncInterval},
		{"CRICLIVE_PREMATCH_INTERVAL", &cfg.PreMatchInterval},
		{"CRICLIVE_BREAK_INTERVAL", &cfg.BreakInterval},
		{"CRICLIVE_FINALIZING_INTERVAL", &cfg.FinalizingInterval},
		{"CRICLIVE_LEASE_TTL", &cfg.LeaseTTL},
		{"CRICLIVE_STALE_MINIMUM", &cfg.StaleMinimum},
		{"CRICLIVE_ACTIVE_FREEZE_TIMEOUT", &cfg.ActiveFreezeTimeout},
		{"CRICLIVE_INNINGS_FINALIZATION_HOLD", &cfg.InningsFinalizationHold},
		{"CRICLIVE_MATCH_FINALIZATION_HOLD", &cfg.MatchFinalizationHold},
		{"CRICLIVE_RAW_PAYLOAD_TTL", &cfg.RawPayloadTTL},
	}
	for _, setting := range durations {
		if err := parsePositiveDurationEnv(setting.name, setting.target); err != nil {
			return Config{}, err
		}
	}

	if err := parseIntEnv("CRICLIVE_QUOTA_RESERVE_PERCENT", &cfg.QuotaReservePercent); err != nil {
		return Config{}, err
	}
	if err := parseIntEnv("CRICLIVE_MAX_CONCURRENCY", &cfg.MaxConcurrency); err != nil {
		return Config{}, err
	}
	if err := parseIntEnv("CRICLIVE_HOURLY_REQUEST_LIMIT", &cfg.HourlyRequestLimit); err != nil {
		return Config{}, err
	}
	if err := parseIntEnv("CRICLIVE_DAILY_REQUEST_LIMIT", &cfg.DailyRequestLimit); err != nil {
		return Config{}, err
	}
	cfg.AllowLiveCorrections = parseBoolEnv("CRICLIVE_ALLOW_LIVE_CORRECTIONS")
	cfg.AllowMidMatchLiveAdmission = parseBoolEnv("CRICLIVE_ALLOW_MID_MATCH_LIVE_ADMISSION")
	// Live mode always uses fast polling for ball-by-ball trading UX. The env flag
	// only applies in shadow mode (quota-sensitive dry runs).
	if cfg.Mode == ModeLive {
		cfg.FastPollingEnabled = true
	} else if raw, ok := os.LookupEnv("CRICLIVE_FAST_POLLING_ENABLED"); ok {
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
		return fmt.Errorf("CRICLIVE_MODE must be one of off, shadow, or live")
	}
	switch c.Feed {
	case FeedCricketLine:
		if c.Mode != ModeOff && strings.TrimSpace(c.CricketLineAPIKey) == "" {
			return errors.New("CRICKETLINE_API_KEY is required when LIVE_FEED_PROVIDER is cricketline and CRICLIVE_MODE is shadow or live")
		}
		if err := validateProviderBaseURL("CRICKETLINE_BASE_URL", c.CricketLineBaseURL); err != nil {
			return err
		}
	case FeedCricLive:
		if c.Mode != ModeOff && strings.TrimSpace(c.APIToken) == "" {
			return errors.New("CRICLIVE_API_TOKEN is required when CRICLIVE_MODE is shadow or live")
		}
	default:
		return fmt.Errorf("LIVE_FEED_PROVIDER must be one of %s or %s", FeedCricketLine, FeedCricLive)
	}
	if err := validateProviderBaseURL("CRICLIVE_BASE_URL", c.BaseURL); err != nil {
		return err
	}
	if c.HTTPTimeout <= 0 {
		return errors.New("CRICLIVE_HTTP_TIMEOUT must be positive")
	}
	if c.QuotaReservePercent < 0 || c.QuotaReservePercent >= 100 {
		return errors.New("CRICLIVE_QUOTA_RESERVE_PERCENT must be between 0 and 99")
	}
	if c.HourlyRequestLimit <= 0 {
		return errors.New("CRICLIVE_HOURLY_REQUEST_LIMIT must be positive")
	}
	if c.DailyRequestLimit <= 0 {
		return errors.New("CRICLIVE_DAILY_REQUEST_LIMIT must be positive")
	}
	if c.MinPollInterval <= 0 || c.MaxPollInterval <= 0 || c.MinPollInterval > c.MaxPollInterval {
		return errors.New("CricLive poll intervals must be positive and minimum must not exceed maximum")
	}
	positiveDurations := []struct {
		name  string
		value time.Duration
	}{
		{"CRICLIVE_DISCOVERY_INTERVAL", c.DiscoveryInterval},
		{"CRICLIVE_FIXTURE_SYNC_INTERVAL", c.FixtureSyncInterval},
		{"CRICLIVE_PREMATCH_INTERVAL", c.PreMatchInterval},
		{"CRICLIVE_BREAK_INTERVAL", c.BreakInterval},
		{"CRICLIVE_FINALIZING_INTERVAL", c.FinalizingInterval},
		{"CRICLIVE_LEASE_TTL", c.LeaseTTL},
		{"CRICLIVE_STALE_MINIMUM", c.StaleMinimum},
		{"CRICLIVE_ACTIVE_FREEZE_TIMEOUT", c.ActiveFreezeTimeout},
		{"CRICLIVE_INNINGS_FINALIZATION_HOLD", c.InningsFinalizationHold},
		{"CRICLIVE_MATCH_FINALIZATION_HOLD", c.MatchFinalizationHold},
		{"CRICLIVE_RAW_PAYLOAD_TTL", c.RawPayloadTTL},
	}
	for _, setting := range positiveDurations {
		if setting.value <= 0 {
			return fmt.Errorf("%s must be positive", setting.name)
		}
	}
	if c.MaxConcurrency <= 0 {
		return errors.New("CRICLIVE_MAX_CONCURRENCY must be positive")
	}
	// The last innings must finish its settlement hold before the match does:
	// once the match is terminal its fixture is no longer polled, and an
	// innings still inside its hold would never settle.
	if c.InningsFinalizationHold > c.MatchFinalizationHold {
		return errors.New("CRICLIVE_INNINGS_FINALIZATION_HOLD must not exceed CRICLIVE_MATCH_FINALIZATION_HOLD")
	}
	return nil
}

func validateProviderBaseURL(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute http or https URL", name)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must not contain credentials, query parameters, or a fragment", name)
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("%s must use https outside loopback development", name)
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
