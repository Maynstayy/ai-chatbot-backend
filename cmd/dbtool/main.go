// Command dbtool applies the chatbot's migrations and loads the fake test
// catalogue into the chatbot's OWN MySQL database.
//
//	go run ./cmd/dbtool -confirm=ai_chatbot_test migrate
//	go run ./cmd/dbtool -confirm=ai_chatbot_test seed
//
// Guards, in order: DB_NAME must be ai_chatbot_<env>; -confirm must repeat
// DB_NAME exactly; the database must not contain any MaynStay booking-system
// table; seeding is refused when APP_ENV=production or when the catalogue
// already has hotels. Settings come from .env / the environment (DB_*).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/catalog"
	"github.com/Maynstayy/ai-chatbot-backend/internal/config"
	"github.com/Maynstayy/ai-chatbot-backend/internal/database"
)

func main() {
	confirm := flag.String("confirm", "", "the DB_NAME you intend to change (required)")
	dir := flag.String("migrations", "migrations", "migrations directory")
	file := flag.String("catalog", "", "catalogue JSON to seed (default CATALOG_FILE)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: dbtool -confirm=<DB_NAME> migrate|seed")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *confirm, *dir, *file); err != nil {
		fmt.Fprintln(os.Stderr, "dbtool:", err)
		os.Exit(1)
	}
}

func run(cmd, confirm, dir, file string) error {
	cfg := config.Load()
	if err := cfg.ValidateDatabase(); err != nil {
		return err
	}
	if confirm != cfg.DBName {
		return errors.New("-confirm must repeat DB_NAME exactly; nothing was changed")
	}
	if cmd != "migrate" && cmd != "seed" {
		return fmt.Errorf("unknown command %q (migrate or seed)", cmd)
	}
	if cmd == "seed" && cfg.AppEnv == config.EnvProduction {
		return errors.New("seed loads fake test hotels and is refused when APP_ENV=production")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := database.Open(ctx, database.Config{User: cfg.DBUser, Password: cfg.DBPassword, Host: cfg.DBHost, Port: cfg.DBPort, Name: cfg.DBName})
	if err != nil {
		return err
	}
	defer db.Close()

	if cmd == "migrate" {
		return migrate(ctx, db, dir)
	}
	if file == "" {
		file = cfg.CatalogFile
	}
	store, err := catalog.LoadFile(file)
	if err != nil {
		return err
	}
	return seed(ctx, db, store.Data())
}

// migrate applies migrations/NNNN_*.sql once each, in order, recording them
// in schema_migrations.
func migrate(ctx context.Context, db *sql.DB, dir string) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(100) NOT NULL PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "[0-9][0-9][0-9][0-9]_*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		version := strings.TrimSuffix(filepath.Base(f), ".sql")
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?", version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			fmt.Println("already applied:", version)
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		for _, stmt := range splitStatements(string(raw)) {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", version, err)
			}
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", version); err != nil {
			return err
		}
		fmt.Println("applied:", version)
	}
	return nil
}

// splitStatements splits on semicolons that end a line, dropping -- comments.
// The migrations keep semicolons out of string literals.
func splitStatements(sqlText string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(sqlText, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
		if strings.HasSuffix(t, ";") {
			if s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(cur.String()), ";")); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func jsonArray(v []string) any {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// seed loads the fake catalogue in one transaction.
func seed(ctx context.Context, db *sql.DB, d catalog.Data) error {
	var existing int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM hotels").Scan(&existing); err != nil {
		return fmt.Errorf("hotels table missing - run migrate first: %w", err)
	}
	if existing > 0 {
		return fmt.Errorf("the catalogue already has %d hotels; seed only loads an empty database", existing)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, h := range d.Hotels {
		if _, err := tx.ExecContext(ctx, `INSERT INTO hotels
			(hotel_id, name, location, description, city, state, country, address, latitude, longitude, category,
			 price_per_night, rating, total_reviews, amenities, thumbnail_url, images, owner_id, is_active, is_deleted,
			 free_cancellation, pay_at_property, instant_booking, cancellation_hours, child_policy, pet_policy, id_rules,
			 check_in_time, check_out_time, tax_rate, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			h.HotelID, h.Name, h.Location, h.Description, h.City, h.State, h.Country, h.Address, h.Latitude, h.Longitude,
			h.Category, h.PricePerNight, h.Rating, h.TotalReviews, jsonArray(h.Amenities), h.ThumbnailURL, jsonArray(h.Images),
			h.IsActive, h.FreeCancellation, h.PayAtProperty, h.InstantBooking, h.CancellationHours, h.ChildPolicy, h.PetPolicy,
			h.IDRules, h.CheckInTime, h.CheckOutTime, h.TaxRate, h.CreatedAt.UTC()); err != nil {
			return fmt.Errorf("hotel %d: %w", h.HotelID, err)
		}
	}
	for _, rt := range d.RoomTypes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO room_types
			(room_type_id, hotel_id, name, capacity, price_per_night, description, amenities, images, bed_type, size_sqm, view_type)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rt.RoomTypeID, rt.HotelID, rt.Name, rt.Capacity, rt.PricePerNight, rt.Description,
			jsonArray(rt.Amenities), jsonArray(rt.Images), rt.BedType, rt.SizeSqm, rt.ViewType); err != nil {
			return fmt.Errorf("room type %d: %w", rt.RoomTypeID, err)
		}
		var firstRoom int64
		for i := 1; i <= rt.TotalRooms; i++ {
			res, err := tx.ExecContext(ctx, "INSERT INTO hotel_rooms (hotel_id, room_type_id, room_number, status) VALUES (?, ?, ?, 'Available')",
				rt.HotelID, rt.RoomTypeID, fmt.Sprintf("%d-%02d", rt.RoomTypeID, i))
			if err != nil {
				return fmt.Errorf("room type %d rooms: %w", rt.RoomTypeID, err)
			}
			if i == 1 {
				firstRoom, _ = res.LastInsertId()
			}
		}
		if firstRoom == 0 {
			continue
		}
		// Plans hang off the type's first (lowest id) room: the one the
		// catalogue reads them from.
		for _, p := range rt.RatePlans {
			if _, err := tx.ExecContext(ctx, `INSERT INTO room_rate_plans
				(rate_plan_id, room_id, name, description, price_per_night, max_occupancy, free_cancellation, pay_at_property,
				 breakfast_included, inclusions, is_active, discount_type, discount_value, discount_valid_from, discount_valid_to, rooms_left)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				p.RatePlanID, firstRoom, p.Name, p.Description, p.PricePerNight, p.MaxOccupancy, p.FreeCancellation, p.PayAtProperty,
				p.BreakfastIncluded, jsonArray(p.Inclusions), p.IsActive, orNone(p.DiscountType), p.DiscountValue,
				p.DiscountValidFrom, p.DiscountValidTo, p.RoomsLeft); err != nil {
				return fmt.Errorf("rate plan %d: %w", p.RatePlanID, err)
			}
		}
	}
	for _, h := range d.Holds {
		if _, err := tx.ExecContext(ctx, "INSERT INTO room_holds (room_type_id, check_in, check_out, room_count, status) VALUES (?, ?, ?, ?, ?)",
			h.RoomTypeID, h.CheckIn, h.CheckOut, h.RoomCount, h.Status); err != nil {
			return err
		}
	}
	for _, l := range d.Landmarks {
		if _, err := tx.ExecContext(ctx, "INSERT INTO landmarks (landmark_id, city, name, category, latitude, longitude, is_active) VALUES (?, ?, ?, ?, ?, ?, ?)",
			l.LandmarkID, l.City, l.Name, l.Category, l.Latitude, l.Longitude, l.IsActive); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("seeded %d hotels, %d room types, %d holds, %d landmarks\n", len(d.Hotels), len(d.RoomTypes), len(d.Holds), len(d.Landmarks))
	return nil
}

func orNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}
