// Package database opens the chatbot's own MySQL database.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Config is what Open needs; it never logs the password.
type Config struct {
	User, Password, Host, Port, Name string
}

// DSN uses loc=UTC so DATETIME values round-trip the same way regardless of
// the server's timezone (from MaynStay's app.dsn).
func (c Config) DSN() string {
	m := mysql.NewConfig()
	m.User, m.Passwd, m.Net, m.Addr, m.DBName = c.User, c.Password, "tcp", c.Host+":"+c.Port, c.Name
	m.ParseTime = true
	m.Loc = time.UTC
	m.Params = map[string]string{"charset": "utf8mb4"}
	return m.FormatDSN()
}

// Open connects, pings, and refuses a database that looks like the MaynStay
// booking database (it has signup, bookings or payments tables): the
// chatbot must only ever run against its own disposable schema.
func Open(ctx context.Context, c Config) (*sql.DB, error) {
	db, err := sql.Open("mysql", c.DSN())
	if err != nil {
		return nil, err
	}
	// A chatbot reads a few small tables; a modest pool is plenty.
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("database ping: %w", err)
	}
	if err := RefuseForeignSchema(pingCtx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// foreignTables are MaynStay booking-system tables that never exist in the
// chatbot's schema.
var foreignTables = []string{"signup", "bookings", "payments", "vendor_settings", "user_tokens"}

// RefuseForeignSchema errors when the connected database contains any
// MaynStay booking-system table.
func RefuseForeignSchema(ctx context.Context, db *sql.DB) error {
	q := "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name IN (?" +
		strings.Repeat(",?", len(foreignTables)-1) + ")"
	args := make([]any, len(foreignTables))
	for i, t := range foreignTables {
		args[i] = t
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("database schema check: %w", err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		found = append(found, name)
	}
	if len(found) > 0 {
		return fmt.Errorf("refusing to use this database: it contains booking-system tables (%s); point DB_NAME at the chatbot's own ai_chatbot_* database", strings.Join(found, ", "))
	}
	return rows.Err()
}

// Ping is the readiness check.
func Ping(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}
