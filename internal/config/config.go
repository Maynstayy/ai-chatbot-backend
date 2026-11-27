// Package config loads the backend's settings from the environment (and a
// local .env file when present). Only the chatbot's own settings exist here:
// no payment, email, OAuth or JWT settings, since the standalone backend
// has no login, bookings or payments.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

const (
	EnvDevelopment = "development"
	EnvTest        = "test"
	EnvProduction  = "production"

	CatalogFile  = "file"
	CatalogMySQL = "mysql"
)

type Config struct {
	AppEnv  string
	AppPort string

	// Where hotel data comes from: "file" (CATALOG_FILE, fake test data, no
	// database) or "mysql" (DB_* below).
	CatalogSource string
	CatalogFile   string

	DBUser     string
	DBPassword string
	DBHost     string
	DBPort     string
	DBName     string

	// RedisURL (redis://:password@host:port/db) is optional; empty keeps
	// conversations and rate limits in process memory.
	RedisURL string

	// Browser origins allowed to call the API (comma-separated).
	CORSAllowedOrigins []string
	// Proxies whose forwarded client-IP headers are trusted (CIDRs or IPs),
	// besides this machine itself.
	TrustedProxies []string

	// Chatbot AI provider (from MaynStay): "ollama" (default, local, no key),
	// "anthropic" (needs AIAPIKey) or "off". Keys stay server-side.
	AIProvider        string
	AIAPIKey          string
	AIModel           string
	AIBaseURL         string
	AITimeoutSeconds  int
	AIMaxTokens       int
	ChatbotRatePerMin int
	// ChatbotDebug adds tools_used to responses. Development only; refused
	// in production.
	ChatbotDebug bool

	// SiteURL is the public booking website the assistant refers guests to.
	SiteURL         string
	SupportPhone    string
	SupportEmail    string
	SupportWhatsApp string

	// UploadsDir is served at /uploads/ when it exists (hotel photos).
	UploadsDir string
}

// Load reads .env (if present) and the environment.
func Load() *Config {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("could not read .env; using the process environment only", "err", err.Error())
	}

	aiProvider := strings.ToLower(strings.TrimSpace(getenvDefault("AI_PROVIDER", "ollama")))
	appEnv := strings.ToLower(strings.TrimSpace(getenvDefault("APP_ENV", EnvDevelopment)))

	return &Config{
		AppEnv:  appEnv,
		AppPort: getenvDefault("APP_PORT", "8080"),

		CatalogSource: strings.ToLower(strings.TrimSpace(getenvDefault("CATALOG_SOURCE", CatalogFile))),
		CatalogFile:   getenvDefault("CATALOG_FILE", "testdata/catalog.json"),

		DBUser:     os.Getenv("DB_USER"),
		DBPassword: os.Getenv("DB_PASSWORD"),
		DBHost:     os.Getenv("DB_HOST"),
		DBPort:     getenvDefault("DB_PORT", "3306"),
		DBName:     os.Getenv("DB_NAME"),

		RedisURL: os.Getenv("REDIS_URL"),

		CORSAllowedOrigins: splitList(os.Getenv("CORS_ALLOWED_ORIGINS")),
		TrustedProxies:     splitList(os.Getenv("TRUSTED_PROXIES")),

		AIProvider:        aiProvider,
		AIAPIKey:          os.Getenv("AI_API_KEY"),
		AIModel:           getenvDefault("AI_MODEL", DefaultAIModel(aiProvider)),
		AIBaseURL:         getenvDefault("AI_BASE_URL", "http://localhost:11434"),
		AITimeoutSeconds:  getenvInt("AI_TIMEOUT_SECONDS", 90),
		AIMaxTokens:       getenvInt("AI_MAX_TOKENS", 1024),
		ChatbotRatePerMin: getenvInt("CHATBOT_RATE_LIMIT_PER_MINUTE", 20),
		ChatbotDebug:      strings.EqualFold(strings.TrimSpace(os.Getenv("CHATBOT_DEBUG")), "true"),

		SiteURL:         getenvDefault("SITE_URL", "https://skymcm.com"),
		SupportPhone:    getenvDefault("SUPPORT_PHONE", "+91 7607956595"),
		SupportEmail:    getenvDefault("SUPPORT_EMAIL", "skymcm17@gmail.com"),
		SupportWhatsApp: os.Getenv("SUPPORT_WHATSAPP"),

		UploadsDir: getenvDefault("UPLOADS_DIR", "./uploads"),
	}
}

// DefaultAIModel is the model used when AI_MODEL is unset (unchanged from MaynStay).
func DefaultAIModel(provider string) string {
	switch provider {
	case "anthropic", "claude":
		return "claude-opus-5-5"
	case "ollama":
		return "qwen2.5:1.5b"
	}
	return ""
}

// The standalone backend's databases are named ai_chatbot_<something>
// (ai_chatbot_test, ai_chatbot_dev, ai_chatbot_prod). Anything else - in
// particular the MaynStay database - is refused before connecting.
var allowedDBName = regexp.MustCompile(`^ai_chatbot_[a-z0-9_]{1,40}$`)

// Validate reports settings that must not start the server. Only names are
// reported, never values.
func (c *Config) Validate() error {
	var problems []string
	switch c.AppEnv {
	case EnvDevelopment, EnvTest, EnvProduction:
	default:
		problems = append(problems, "APP_ENV must be development, test or production")
	}
	switch c.CatalogSource {
	case CatalogFile:
		if strings.TrimSpace(c.CatalogFile) == "" {
			problems = append(problems, "CATALOG_FILE is required when CATALOG_SOURCE=file")
		}
	case CatalogMySQL:
		if err := c.ValidateDatabase(); err != nil {
			problems = append(problems, err.Error())
		}
	default:
		problems = append(problems, "CATALOG_SOURCE must be file or mysql")
	}
	for _, o := range c.CORSAllowedOrigins {
		if !validOrigin.MatchString(o) {
			problems = append(problems, fmt.Sprintf("CORS_ALLOWED_ORIGINS entry %q is not an origin like https://example.com", o))
		}
	}
	if c.AppEnv == EnvProduction {
		if len(c.CORSAllowedOrigins) == 0 {
			problems = append(problems, "CORS_ALLOWED_ORIGINS is required in production")
		}
		if c.ChatbotDebug {
			problems = append(problems, "CHATBOT_DEBUG must be off in production")
		}
	}
	if len(problems) > 0 {
		return errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return nil
}

var validOrigin = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:\d{1,5})?$`)

// ValidateDatabase checks the DB_* settings, including the database-name
// guard. DB_PASSWORD may legitimately be empty.
func (c *Config) ValidateDatabase() error {
	var missing []string
	for _, v := range []struct{ name, value string }{
		{"DB_HOST", c.DBHost}, {"DB_PORT", c.DBPort}, {"DB_USER", c.DBUser}, {"DB_NAME", c.DBName},
	} {
		if strings.TrimSpace(v.value) == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing database settings: %s", strings.Join(missing, ", "))
	}
	if !allowedDBName.MatchString(c.DBName) {
		return fmt.Errorf("DB_NAME must be a dedicated chatbot database named ai_chatbot_<env> (got a different name; refusing to connect)")
	}
	return nil
}

// CORSOrigins is the allowed-origin set; development defaults to the local
// frontend when nothing is configured.
func (c *Config) CORSOrigins() []string {
	if len(c.CORSAllowedOrigins) == 0 && c.AppEnv != EnvProduction {
		return []string{"http://localhost:3000"}
	}
	return c.CORSAllowedOrigins
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimRight(strings.TrimSpace(s), "/"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func getenvDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// getenvInt reads a positive integer, falling back on a missing or bad value.
func getenvInt(key string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && n > 0 {
		return n
	}
	return fallback
}
