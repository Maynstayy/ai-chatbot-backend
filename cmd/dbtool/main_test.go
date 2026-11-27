package main

import (
	"os"
	"strings"
	"testing"
)

func TestMigrationSplitsIntoTableStatements(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0001_create_catalog.sql")
	if err != nil {
		t.Fatal(err)
	}
	stmts := splitStatements(string(raw))
	if len(stmts) != 6 {
		t.Fatalf("want 6 CREATE TABLE statements, got %d", len(stmts))
	}
	for _, s := range stmts {
		if !strings.HasPrefix(s, "CREATE TABLE IF NOT EXISTS") {
			t.Errorf("unexpected statement: %.60s", s)
		}
		for _, forbidden := range []string{"DROP ", "DELETE FROM", "TRUNCATE ", "signup", "bookings", "payments"} {
			if strings.Contains(s, forbidden) {
				t.Errorf("migration mentions %q", forbidden)
			}
		}
	}
}

func TestRunRefusesWithoutMatchingConfirm(t *testing.T) {
	t.Setenv("DB_HOST", "127.0.0.1")
	t.Setenv("DB_PORT", "1") // nothing is contacted: the guard fails first
	t.Setenv("DB_USER", "chatbot")
	t.Setenv("DB_NAME", "ai_chatbot_test")
	if err := run("migrate", "ai_chatbot_dev", "../../migrations", ""); err == nil || !strings.Contains(err.Error(), "-confirm") {
		t.Fatalf("mismatched confirm: %v", err)
	}
	t.Setenv("DB_NAME", "maynstay")
	if err := run("migrate", "maynstay", "../../migrations", ""); err == nil || !strings.Contains(err.Error(), "ai_chatbot_") {
		t.Fatalf("foreign database name: %v", err)
	}
	t.Setenv("DB_NAME", "ai_chatbot_prod")
	t.Setenv("APP_ENV", "production")
	if err := run("seed", "ai_chatbot_prod", "../../migrations", ""); err == nil || !strings.Contains(err.Error(), "production") {
		t.Fatalf("production seed: %v", err)
	}
}
