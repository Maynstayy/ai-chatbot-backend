package catalog

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
	"github.com/Maynstayy/ai-chatbot-backend/internal/roomtypes"
)

func load(t *testing.T) *Store {
	t.Helper()
	s, err := LoadFile("../../testdata/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBundledCatalogueIsTestDataOnly(t *testing.T) {
	for _, h := range load(t).Data().Hotels {
		if len(h.Name) < 5 || h.Name[:5] != "Test " {
			t.Errorf("hotel %d %q is not clearly marked as test data", h.HotelID, h.Name)
		}
	}
}

func TestHotelFilters(t *testing.T) {
	repo := load(t).Hotels()
	ctx := context.Background()
	active := true

	all, _ := repo.GetHotels(ctx, hotels.HotelFilter{IsActive: &active})
	if len(all) != 6 {
		t.Fatalf("all: %d", len(all))
	}
	city, _ := repo.GetHotels(ctx, hotels.HotelFilter{City: "varanasi"})
	if len(city) != 3 {
		t.Fatalf("city match should be case-insensitive: %d", len(city))
	}
	search, _ := repo.GetHotels(ctx, hotels.HotelFilter{Search: "ganga"})
	if len(search) != 1 || search[0].HotelID != 101 {
		t.Fatalf("search: %+v", search)
	}
	cheap, _ := repo.GetHotels(ctx, hotels.HotelFilter{MaxPrice: 1500, Sort: "price_asc"})
	if len(cheap) != 2 || cheap[0].PricePerNight > cheap[1].PricePerNight {
		t.Fatalf("budget: %+v", cheap)
	}
}

func TestStartingPriceAndGST(t *testing.T) {
	h, err := load(t).Hotels().GetHotelByID(context.Background(), 103)
	if err != nil {
		t.Fatal(err)
	}
	// Cheapest room type with rooms, then GST at 18% above Rs 7,500.
	if h.PricePerNight != 8200 || h.GSTRate != 0.18 || h.TaxPerNight != 1476 {
		t.Fatalf("price %v gst %v tax %v", h.PricePerNight, h.GSTRate, h.TaxPerNight)
	}
	if _, err := load(t).Hotels().GetHotelByID(context.Background(), 999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing hotel: %v", err)
	}
}

func TestAvailabilitySubtractsOverlappingHolds(t *testing.T) {
	svc := roomtypes.NewService(load(t).RoomTypes())
	ctx := context.Background()
	in, out := "2027-01-10", "2027-01-12"
	types, err := svc.GetRoomTypesByHotel(ctx, 101, &in, &out, 4)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]roomtypes.RoomType{}
	for _, rt := range types {
		byID[rt.RoomTypeID] = rt
	}
	// Family Suite: both rooms held (confirmed). Deluxe: 6 minus 3 pending.
	if byID[1012].AvailableCount != 0 || byID[1011].AvailableCount != 3 {
		t.Fatalf("availability: suite %d deluxe %d", byID[1012].AvailableCount, byID[1011].AvailableCount)
	}
	if byID[1012].FitsGuests == nil || *byID[1012].FitsGuests {
		t.Fatal("a sold-out suite can't fit the party")
	}

	// A cancelled hold frees its rooms; a stay after the holds is free.
	in, out = "2027-01-12", "2027-01-14"
	types, _ = svc.GetRoomTypesByHotel(ctx, 101, &in, &out, 0)
	for _, rt := range types {
		if rt.RoomTypeID == 1011 && rt.AvailableCount != 6 {
			t.Fatalf("check-out day must not count: %d", rt.AvailableCount)
		}
	}
}

func TestParseRejectsBrokenData(t *testing.T) {
	cases := map[string]string{
		"unknown field":     `{"hotels":[],"rooms":[]}`,
		"duplicate hotel":   `{"hotels":[{"hotel_id":1,"name":"A"},{"hotel_id":1,"name":"B"}]}`,
		"orphan room type":  `{"hotels":[{"hotel_id":1,"name":"A"}],"room_types":[{"room_type_id":5,"hotel_id":2}]}`,
		"hold bad date":     `{"hotels":[{"hotel_id":1,"name":"A"}],"room_types":[{"room_type_id":5,"hotel_id":1}],"holds":[{"room_type_id":5,"check_in":"soon","check_out":"2027-01-01"}]}`,
		"orphan hold":       `{"hotels":[{"hotel_id":1,"name":"A"}],"holds":[{"room_type_id":9,"check_in":"2027-01-01","check_out":"2027-01-02"}]}`,
		"duplicate plan id": `{"hotels":[{"hotel_id":1,"name":"A"}],"room_types":[{"room_type_id":5,"hotel_id":1,"rate_plans":[{"rate_plan_id":7},{"rate_plan_id":7}]}]}`,
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
