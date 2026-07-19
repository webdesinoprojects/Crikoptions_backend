package config

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Port           string
	MongoURI       string
	MongoDB        string
	JWTSecret      string
	TokenHours     int
	ChatEnabled    bool
	AllowedOrigins []string
}

type MongoConfig struct {
	URI      string
	Database string
}

// LoadMongo loads only the settings required by background services. It keeps
// feedworker independent from API-only PORT and JWT configuration.
func LoadMongo() (MongoConfig, error) {
	_ = loadDotEnv(".env")
	_ = loadDotEnv(filepath.Join("backend", ".env"))
	uri := strings.TrimSpace(os.Getenv("MONGO_DB"))
	if uri == "" {
		return MongoConfig{}, errors.New("MONGO_DB is required")
	}
	database := strings.TrimSpace(os.Getenv("MONGO_DB_NAME"))
	if database == "" {
		database = "crikoptions"
	}
	return MongoConfig{URI: uri, Database: database}, nil
}

func Load() (Config, error) {
	_ = loadDotEnv(".env")
	_ = loadDotEnv(filepath.Join("backend", ".env"))

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		return Config{}, errors.New("PORT is required")
	}

	mongoConfig, err := LoadMongo()
	if err != nil {
		return Config{}, err
	}

	jwtSecret := strings.TrimSpace(os.Getenv("JWT_SECRET"))
	if jwtSecret == "" {
		return Config{}, errors.New("JWT_SECRET is required")
	}

	tokenHours := 24
	if v := strings.TrimSpace(os.Getenv("TOKEN_HOURS")); v != "" {
		if n, ok := toPositiveInt(v); ok {
			tokenHours = n
		} else {
			return Config{}, errors.New("TOKEN_HOURS must be a positive number")
		}
	}

	return Config{
		Port: port, MongoURI: mongoConfig.URI, MongoDB: mongoConfig.Database, JWTSecret: jwtSecret, TokenHours: tokenHours,
		ChatEnabled:    parseBool(os.Getenv("CHAT_ENABLED")),
		AllowedOrigins: allowedOrigins(os.Getenv("ALLOWED_ORIGINS")),
	}, nil
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseCSVWithDefault(value string, fallback []string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimRight(strings.TrimSpace(item), "/")
		if item != "" {
			out = append(out, item)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func allowedOrigins(value string) []string {
	defaults := []string{
		"http://localhost:3000",
		"https://cricoptions.vercel.app",
	}
	origins := parseCSVWithDefault(value, defaults)
	seen := make(map[string]struct{}, len(origins)+len(defaults))
	for _, origin := range origins {
		seen[origin] = struct{}{}
	}
	for _, origin := range defaults {
		if _, ok := seen[origin]; !ok {
			origins = append(origins, origin)
		}
	}
	return origins
}

func toPositiveInt(s string) (int, bool) {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int(ch-'0')
	}
	return n, n > 0
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}

		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		_ = os.Setenv(key, value)
	}

	return scanner.Err()
}
