package config

import (
	"strings"
	"testing"
)

func valid() *Config {
	return &Config{AppEnv: EnvDevelopment, CatalogSource: CatalogFile, CatalogFile: "testdata/catalog.json"}
}

func TestDatabaseNameGuard(t *testing.T) {
	for _, name := range []string{"maynstay", "hotel_booking", "skymcm_prod", "ai_chatbot", "AI_CHATBOT_TEST", "ai_chatbot_test;drop"} {
		c := valid()
		c.CatalogSource, c.DBHost, c.DBPort, c.DBUser, c.DBName = CatalogMySQL, "127.0.0.1", "3306", "chatbot", name
		if err := c.Validate(); err == nil {
			t.Errorf("DB_NAME %q was accepted", name)
		} else if strings.Contains(err.Error(), name) && name != "ai_chatbot" {
			t.Errorf("error echoes the database name: %v", err)
		}
	}
	c := valid()
	c.CatalogSource, c.DBHost, c.DBPort, c.DBUser, c.DBName = CatalogMySQL, "127.0.0.1", "3306", "chatbot", "ai_chatbot_test"
	if err := c.Validate(); err != nil {
		t.Fatalf("ai_chatbot_test refused: %v", err)
	}
}

func TestProductionRules(t *testing.T) {
	c := valid()
	c.AppEnv = EnvProduction
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "CORS_ALLOWED_ORIGINS") {
		t.Fatalf("production without CORS: %v", err)
	}
	c.CORSAllowedOrigins = []string{"https://chat.example.com"}
	c.ChatbotDebug = true
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "CHATBOT_DEBUG") {
		t.Fatalf("production with debug: %v", err)
	}
	c.ChatbotDebug = false
	// The fake catalogues never serve production.
	for _, src := range []string{CatalogFile, CatalogMySQL} {
		c.CatalogSource = src
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "CATALOG_SOURCE must be skymcm in production") {
			t.Fatalf("production with %s catalogue: %v", src, err)
		}
	}
	c.CatalogSource, c.SKYMCMAPIURL = CatalogSKYMCM, "https://skymcm.com"
	// Sign-in can't be switched off in production.
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "AUTH_REQUIRED can't be false in production") {
		t.Fatalf("production without sign-in: %v", err)
	}
	// (A configuration value only: validating it makes no request.)
	c.AuthRequired, c.AuthURL, c.AuthCookieName = true, "https://skymcm.com", "maynstay_token"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.CORSOrigins()) != 1 {
		t.Fatal("production must not add the localhost default")
	}
}

func TestSKYMCMSettings(t *testing.T) {
	c := valid()
	c.CatalogSource = CatalogSKYMCM
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "SKYMCM_API_URL is required") {
		t.Fatalf("missing URL: %v", err)
	}
	for _, bad := range []string{"skymcm.com", "ftp://skymcm.com", "http://skymcm.com", "https://user:pw@skymcm.com", "https://skymcm.com/?a=1"} {
		c.SKYMCMAPIURL = bad
		if err := c.Validate(); err == nil {
			t.Errorf("SKYMCM_API_URL %q accepted", bad)
		}
	}
	for _, ok := range []string{"https://skymcm.com", "https://api.skymcm.com/backend", "http://localhost:8099", "http://127.0.0.1:8099"} {
		c.SKYMCMAPIURL = ok
		if err := c.Validate(); err != nil {
			t.Errorf("SKYMCM_API_URL %q refused: %v", ok, err)
		}
	}
	// A local stand-in over http is never allowed in production.
	c.AppEnv, c.CORSAllowedOrigins, c.SKYMCMAPIURL = EnvProduction, []string{"https://chat.example.com"}, "http://localhost:8099"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("http in production: %v", err)
	}
	c.SKYMCMAPIURL, c.SKYMCMAssetURL = "https://skymcm.com", "http://cdn.skymcm.com"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "SKYMCM_ASSET_URL") {
		t.Fatalf("http asset URL in production: %v", err)
	}
}

func TestBadOriginsAndDefaults(t *testing.T) {
	c := valid()
	c.CORSAllowedOrigins = []string{"https://ok.example", "*"}
	if err := c.Validate(); err == nil {
		t.Fatal("wildcard origin accepted")
	}
	if got := valid().CORSOrigins(); len(got) != 1 || got[0] != "http://localhost:3000" {
		t.Fatalf("development default: %v", got)
	}
	if DefaultAIModel("ollama") != "qwen2.5:1.5b" || DefaultAIModel("anthropic") != "claude-opus-5-5" {
		t.Fatal("default models changed")
	}
}

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{"APP_ENV", "CATALOG_SOURCE", "AI_PROVIDER", "AI_MODEL", "CORS_ALLOWED_ORIGINS", "AI_TIMEOUT_SECONDS", "SKYMCM_API_URL", "SKYMCM_MAX_CONCURRENCY", "SKYMCM_CACHE_SECONDS", "SKYMCM_LANDMARK_CACHE_SECONDS"} {
		t.Setenv(k, "")
	}
	c := Load()
	if c.AppEnv != EnvDevelopment || c.CatalogSource != CatalogFile || c.AIProvider != "ollama" || c.AIModel != "qwen2.5:1.5b" || c.AITimeoutSeconds != 90 ||
		c.SKYMCMAPIURL != "" || c.SKYMCMMaxConcurrency != 4 || c.SKYMCMTimeoutSeconds != 8 || c.SKYMCMRoomCacheSecs != 30 ||
		c.SKYMCMCacheSeconds != 60 || c.SKYMCMLandmarkSecs != 1800 {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestLandmarkCacheSetting(t *testing.T) {
	t.Setenv("SKYMCM_LANDMARK_CACHE_SECONDS", "")
	if c := Load(); c.SKYMCMLandmarkSecs != 1800 {
		t.Fatalf("default landmark cache: %d", c.SKYMCMLandmarkSecs)
	}
	t.Setenv("SKYMCM_LANDMARK_CACHE_SECONDS", "600")
	if c := Load(); c.SKYMCMLandmarkSecs != 600 {
		t.Fatalf("override: %d", c.SKYMCMLandmarkSecs)
	}
	t.Setenv("SKYMCM_LANDMARK_CACHE_SECONDS", "nonsense")
	if c := Load(); c.SKYMCMLandmarkSecs != 1800 {
		t.Fatalf("invalid value must fall back to the default: %d", c.SKYMCMLandmarkSecs)
	}
}

func TestAuthSettings(t *testing.T) {
	c := valid()
	c.AuthRequired, c.AuthCookieName = true, "maynstay_token"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "SKYMCM_AUTH_URL is required") {
		t.Fatalf("missing auth URL: %v", err)
	}
	// Outside production only this machine (the mock auth server) is allowed,
	// so local testing can never send a login check to the live site.
	for _, bad := range []string{"https://skymcm.com", "http://skymcm.com", "http://192.168.1.5:8090", "localhost:8090",
		"http://127.0.0.1:8090/api", "http://user:pw@127.0.0.1:8090", "ftp://127.0.0.1"} {
		c.AuthURL = bad
		if err := c.Validate(); err == nil {
			t.Errorf("SKYMCM_AUTH_URL %q accepted outside production", bad)
		}
	}
	for _, ok := range []string{"http://127.0.0.1:8090", "http://localhost:8090", "http://[::1]:8090"} {
		c.AuthURL = ok
		if err := c.Validate(); err != nil {
			t.Errorf("SKYMCM_AUTH_URL %q refused: %v", ok, err)
		}
	}
	c.AuthURL, c.AuthCookieName = "http://127.0.0.1:8090", "bad cookie;"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "AUTH_COOKIE_NAME") {
		t.Fatalf("bad cookie name: %v", err)
	}
	// Production: https only.
	p := valid()
	p.AppEnv, p.CORSAllowedOrigins, p.CatalogSource, p.SKYMCMAPIURL = EnvProduction, []string{"https://skymcm.com"}, CatalogSKYMCM, "https://skymcm.com"
	p.AuthRequired, p.AuthURL, p.AuthCookieName = true, "http://localhost:8090", "maynstay_token"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "https in production") {
		t.Fatalf("http auth in production: %v", err)
	}
}

func TestAuthDefaults(t *testing.T) {
	for _, k := range []string{"AUTH_REQUIRED", "SKYMCM_AUTH_URL", "AUTH_COOKIE_NAME", "AUTH_CACHE_SECONDS", "AUTH_TIMEOUT_SECONDS"} {
		t.Setenv(k, "")
	}
	c := Load()
	if !c.AuthRequired || c.AuthCookieName != "maynstay_token" || c.AuthCacheSeconds != 30 || c.AuthTimeoutSeconds != 5 {
		t.Fatalf("defaults: %+v", c)
	}
	t.Setenv("AUTH_CACHE_SECONDS", "0")
	t.Setenv("AUTH_REQUIRED", "false")
	if c := Load(); c.AuthCacheSeconds != 0 || c.AuthRequired {
		t.Fatalf("cache 0 / auth off: %d %v", c.AuthCacheSeconds, c.AuthRequired)
	}
	t.Setenv("AUTH_CACHE_SECONDS", "-5")
	if c := Load(); c.AuthCacheSeconds != 30 {
		t.Fatalf("negative cache: %d", c.AuthCacheSeconds)
	}
}
