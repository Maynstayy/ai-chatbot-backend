// Package catalog provides the hotel catalogue from a JSON file: the same
// hotels / roomtypes / landmarks repository interfaces the MySQL
// repositories implement, so the chatbot and the public endpoints run
// without any database (local development, tests, demos). The bundled file
// holds clearly fake test hotels only.
//
// The matching rules follow the MySQL repositories (adapted from MaynStay):
// city/category equality and name/location search are case-insensitive like
// MySQL's default collation, the "from" price is the cheapest room type
// with rooms, and availability subtracts overlapping pending/confirmed holds.
package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/gst"
	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
	"github.com/Maynstayy/ai-chatbot-backend/internal/landmarks"
	"github.com/Maynstayy/ai-chatbot-backend/internal/rooms"
	"github.com/Maynstayy/ai-chatbot-backend/internal/roomtypes"
)

// Data is the file format (also what cmd/seedcatalog loads into MySQL).
type Data struct {
	Hotels    []hotels.Hotel       `json:"hotels"`
	RoomTypes []RoomTypeRecord     `json:"room_types"`
	Holds     []Hold               `json:"holds"`
	Landmarks []landmarks.Landmark `json:"landmarks"`
}

// RoomTypeRecord is a room type as stored: TotalRooms physical rooms, all
// bookable, with the rate plans guests can choose from.
type RoomTypeRecord struct {
	RoomTypeID    int64            `json:"room_type_id"`
	HotelID       int64            `json:"hotel_id"`
	Name          string           `json:"name"`
	Capacity      int              `json:"capacity"`
	PricePerNight float64          `json:"price_per_night"`
	Description   string           `json:"description"`
	Amenities     []string         `json:"amenities"`
	Images        []string         `json:"images"`
	BedType       string           `json:"bed_type,omitempty"`
	SizeSqm       *int             `json:"size_sqm,omitempty"`
	ViewType      string           `json:"view_type,omitempty"`
	TotalRooms    int              `json:"total_rooms"`
	RatePlans     []rooms.RatePlan `json:"rate_plans"`
}

// Hold is rooms of one type taken for a stay (the stand-in for bookings).
type Hold struct {
	RoomTypeID int64  `json:"room_type_id"`
	CheckIn    string `json:"check_in"`
	CheckOut   string `json:"check_out"`
	RoomCount  int    `json:"room_count"`
	Status     string `json:"status"` // pending | confirmed | cancelled
}

// Store is an immutable in-memory catalogue.
type Store struct {
	data Data
}

// LoadFile reads and validates a catalogue file.
func LoadFile(path string) (*Store, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("catalog file: %w", err)
	}
	return Parse(raw)
}

// Parse validates a catalogue document.
func Parse(raw []byte) (*Store, error) {
	var d Data
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("catalog file: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &Store{data: d}, nil
}

// Validate checks ids are unique and references resolve.
func (d *Data) Validate() error {
	hotelIDs := map[int64]bool{}
	for _, h := range d.Hotels {
		if h.HotelID <= 0 || hotelIDs[h.HotelID] || strings.TrimSpace(h.Name) == "" {
			return fmt.Errorf("catalog: hotel %d: missing name or duplicate/invalid id", h.HotelID)
		}
		hotelIDs[h.HotelID] = true
	}
	typeIDs := map[int64]bool{}
	planIDs := map[int64]bool{}
	for _, rt := range d.RoomTypes {
		if rt.RoomTypeID <= 0 || typeIDs[rt.RoomTypeID] || !hotelIDs[rt.HotelID] {
			return fmt.Errorf("catalog: room type %d: duplicate id or unknown hotel %d", rt.RoomTypeID, rt.HotelID)
		}
		typeIDs[rt.RoomTypeID] = true
		for _, p := range rt.RatePlans {
			if p.RatePlanID <= 0 || planIDs[p.RatePlanID] {
				return fmt.Errorf("catalog: rate plan %d: duplicate or invalid id", p.RatePlanID)
			}
			planIDs[p.RatePlanID] = true
		}
	}
	for _, h := range d.Holds {
		if !typeIDs[h.RoomTypeID] {
			return fmt.Errorf("catalog: hold for unknown room type %d", h.RoomTypeID)
		}
		if _, err := time.Parse("2006-01-02", h.CheckIn); err != nil {
			return fmt.Errorf("catalog: hold check_in %q: %w", h.CheckIn, err)
		}
		if _, err := time.Parse("2006-01-02", h.CheckOut); err != nil {
			return fmt.Errorf("catalog: hold check_out %q: %w", h.CheckOut, err)
		}
	}
	return nil
}

// Data returns the loaded document (for seeding a database).
func (s *Store) Data() Data { return s.data }

// Hotels returns the hotels repository view.
func (s *Store) Hotels() hotels.Repository { return hotelRepo{s} }

// RoomTypes returns the room types repository view.
func (s *Store) RoomTypes() roomtypes.Repository { return roomTypeRepo{s} }

// Landmarks returns the landmarks repository view.
func (s *Store) Landmarks() landmarks.Repository { return landmarkRepo{s} }

// --- hotels ------------------------------------------------------------------

type hotelRepo struct{ s *Store }

// startingPrice mirrors the SQL: the cheapest room type that has rooms,
// else the hotel's own stored price.
func (s *Store) startingPrice(h hotels.Hotel) float64 {
	best := 0.0
	for _, rt := range s.data.RoomTypes {
		if rt.HotelID != h.HotelID || rt.PricePerNight <= 0 || rt.TotalRooms <= 0 {
			continue
		}
		if best == 0 || rt.PricePerNight < best {
			best = rt.PricePerNight
		}
	}
	if best == 0 {
		return h.PricePerNight
	}
	return best
}

func (s *Store) public(h hotels.Hotel) hotels.Hotel {
	h.PricePerNight = s.startingPrice(h)
	h.GSTRate = gst.Rate(h.PricePerNight)
	h.TaxPerNight = gst.TaxPerNight(h.PricePerNight)
	if h.Amenities == nil {
		h.Amenities = []string{}
	}
	if h.Images == nil {
		h.Images = []string{}
	}
	h.Amenities = append([]string(nil), h.Amenities...)
	h.Images = append([]string(nil), h.Images...)
	return h
}

func (r hotelRepo) GetHotels(_ context.Context, f hotels.HotelFilter) ([]hotels.Hotel, error) {
	out := []hotels.Hotel{}
	search := strings.ToLower(f.Search)
	for _, raw := range r.s.data.Hotels {
		h := r.s.public(raw)
		switch {
		case h.IsDeleted:
			continue
		case f.Category != "" && !strings.EqualFold(h.Category, f.Category):
			continue
		case f.City != "" && !strings.EqualFold(h.City, f.City):
			continue
		case search != "" && !strings.Contains(strings.ToLower(h.Name), search) && !strings.Contains(strings.ToLower(h.Location), search):
			continue
		case f.MinPrice > 0 && h.PricePerNight < f.MinPrice:
			continue
		case f.MaxPrice > 0 && h.PricePerNight > f.MaxPrice:
			continue
		case f.OwnerID > 0 && h.OwnerID != f.OwnerID:
			continue
		case f.IsActive != nil && h.IsActive != *f.IsActive:
			continue
		}
		out = append(out, h)
	}
	switch f.Sort {
	case "price_asc":
		sort.SliceStable(out, func(i, j int) bool { return out[i].PricePerNight < out[j].PricePerNight })
	case "price_desc":
		sort.SliceStable(out, func(i, j int) bool { return out[i].PricePerNight > out[j].PricePerNight })
	case "rating":
		sort.SliceStable(out, func(i, j int) bool { return out[i].Rating > out[j].Rating })
	default: // newest first, like ORDER BY created_at DESC
		sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	}
	return out, nil
}

func (r hotelRepo) GetHotelByID(_ context.Context, id int64) (*hotels.Hotel, error) {
	for _, raw := range r.s.data.Hotels {
		if raw.HotelID == id && !raw.IsDeleted {
			h := r.s.public(raw)
			if h.CheckInTime == "" {
				h.CheckInTime = "14:00"
			}
			if h.CheckOutTime == "" {
				h.CheckOutTime = "12:00"
			}
			return &h, nil
		}
	}
	// Same error the MySQL repository returns for a missing row.
	return nil, sql.ErrNoRows
}

// --- room types ----------------------------------------------------------------

type roomTypeRepo struct{ s *Store }

func (r roomTypeRepo) GetRoomTypesByHotelID(_ context.Context, hotelID int64, checkIn, checkOut *string) ([]roomtypes.RoomType, error) {
	var hotelName string
	for _, h := range r.s.data.Hotels {
		if h.HotelID == hotelID {
			hotelName = h.Name
		}
	}
	out := []roomtypes.RoomType{}
	for _, rec := range r.s.data.RoomTypes {
		if rec.HotelID != hotelID {
			continue
		}
		rt := roomtypes.RoomType{
			RoomTypeID: rec.RoomTypeID, HotelID: rec.HotelID, HotelName: hotelName, Name: rec.Name,
			Capacity: rec.Capacity, PricePerNight: rec.PricePerNight, Description: rec.Description,
			Amenities: nonNil(rec.Amenities), Images: nonNil(rec.Images), BedType: rec.BedType,
			SizeSqm: rec.SizeSqm, ViewType: rec.ViewType, TotalRooms: rec.TotalRooms, AvailableCount: rec.TotalRooms,
		}
		if checkIn != nil && checkOut != nil {
			rt.AvailableCount -= r.s.heldRooms(rec.RoomTypeID, *checkIn, *checkOut)
			if rt.AvailableCount < 0 {
				rt.AvailableCount = 0
			}
		}
		if rec.TotalRooms > 0 {
			for _, p := range rec.RatePlans {
				if p.IsActive {
					p.RoomID = rec.RoomTypeID
					p.Inclusions = nonNil(p.Inclusions)
					rt.RatePlans = append(rt.RatePlans, p)
				}
			}
			sort.SliceStable(rt.RatePlans, func(i, j int) bool { return rt.RatePlans[i].PricePerNight < rt.RatePlans[j].PricePerNight })
		}
		out = append(out, rt)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PricePerNight < out[j].PricePerNight })
	return out, nil
}

// heldRooms counts rooms of a type taken on any night of [checkIn, checkOut).
func (s *Store) heldRooms(roomTypeID int64, checkIn, checkOut string) int {
	n := 0
	for _, h := range s.data.Holds {
		if h.RoomTypeID != roomTypeID || (h.Status != "pending" && h.Status != "confirmed") {
			continue
		}
		// Same overlap test as the SQL: check_in < ? AND check_out > ?
		if h.CheckIn < checkOut && h.CheckOut > checkIn {
			n += h.RoomCount
		}
	}
	return n
}

// --- landmarks -------------------------------------------------------------------

type landmarkRepo struct{ s *Store }

func (r landmarkRepo) ListByCity(_ context.Context, city string) ([]landmarks.Landmark, error) {
	out := []landmarks.Landmark{}
	for _, l := range r.s.data.Landmarks {
		if l.IsActive && strings.EqualFold(l.City, city) {
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return append([]string(nil), v...)
}
