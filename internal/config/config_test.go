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
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.CORSOrigins()) != 1 {
		t.Fatal("production must not add the localhost default")
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
	for _, k := range []string{"APP_ENV", "CATALOG_SOURCE", "AI_PROVIDER", "AI_MODEL", "CORS_ALLOWED_ORIGINS", "AI_TIMEOUT_SECONDS"} {
		t.Setenv(k, "")
	}
	c := Load()
	if c.AppEnv != EnvDevelopment || c.CatalogSource != CatalogFile || c.AIProvider != "ollama" || c.AIModel != "qwen2.5:1.5b" || c.AITimeoutSeconds != 90 {
		t.Fatalf("defaults: %+v", c)
	}
}
