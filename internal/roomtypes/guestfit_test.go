package roomtypes

import (
	"testing"

	"github.com/Maynstayy/ai-chatbot-backend/internal/rooms"
)

func plan(price float64, active bool) rooms.RatePlan {
	return rooms.RatePlan{PricePerNight: price, IsActive: active}
}

// The hotel from the spec: Standard sleeps 3, Double 5, Deluxe 3.
func sampleTypes() []RoomType {
	return []RoomType{
		{RoomTypeID: 1, Name: "Standard", Capacity: 3, PricePerNight: 1000, AvailableCount: 5, RatePlans: []rooms.RatePlan{plan(1000, true)}},
		{RoomTypeID: 2, Name: "Double", Capacity: 5, PricePerNight: 1800, AvailableCount: 4, RatePlans: []rooms.RatePlan{plan(1800, true)}},
		{RoomTypeID: 3, Name: "Deluxe", Capacity: 3, PricePerNight: 1350, AvailableCount: 2, RatePlans: []rooms.RatePlan{plan(1350, true)}},
	}
}

func byName(types []RoomType, name string) RoomType {
	for _, t := range types {
		if t.Name == name {
			return t
		}
	}
	return RoomType{}
}

func TestApplyGuestFitRoomsNeeded(t *testing.T) {
	cases := []struct {
		guests                   int
		standard, double, deluxe int
	}{
		{1, 1, 1, 1},
		{3, 1, 1, 1},
		{4, 2, 1, 2},
		{5, 2, 1, 2},
		{7, 3, 2, 3},
		{10, 4, 2, 4},
	}
	for _, c := range cases {
		types := sampleTypes()
		applyGuestFit(types, c.guests)
		if got := byName(types, "Standard").RoomsNeeded; got != c.standard {
			t.Errorf("%d guests: Standard needs %d, want %d", c.guests, got, c.standard)
		}
		if got := byName(types, "Double").RoomsNeeded; got != c.double {
			t.Errorf("%d guests: Double needs %d, want %d", c.guests, got, c.double)
		}
		if got := byName(types, "Deluxe").RoomsNeeded; got != c.deluxe {
			t.Errorf("%d guests: Deluxe needs %d, want %d", c.guests, got, c.deluxe)
		}
	}
}

func TestApplyGuestFitRecommendsCheapestFittingOption(t *testing.T) {
	types := sampleTypes()
	applyGuestFit(types, 7) // Standard x3 = 3000, Double x2 = 3600, Deluxe x3 = 4050 but only 2 free

	if !*byName(types, "Standard").FitsGuests || !*byName(types, "Double").FitsGuests {
		t.Fatal("Standard x3 and Double x2 should fit")
	}
	if *byName(types, "Deluxe").FitsGuests {
		t.Fatal("Deluxe x3 needs 3 rooms but only 2 are free: must not fit")
	}
	if got := byName(types, "Standard").ComboPricePerNight; got != 3000 {
		t.Errorf("Standard combo price = %v, want 3000", got)
	}
	if !byName(types, "Standard").Recommended {
		t.Error("Standard x3 (3000) is the cheapest fitting option and should be recommended")
	}
	for _, n := range []string{"Double", "Deluxe"} {
		if byName(types, n).Recommended {
			t.Errorf("%s must not be recommended", n)
		}
	}
	// best first: fitting options by price, then the one that does not fit
	if types[0].Name != "Standard" || types[1].Name != "Double" || types[2].Name != "Deluxe" {
		t.Errorf("order = %s, %s, %s", types[0].Name, types[1].Name, types[2].Name)
	}
}

func TestApplyGuestFitTieGoesToFewerRooms(t *testing.T) {
	types := []RoomType{
		{RoomTypeID: 1, Name: "TwoRooms", Capacity: 2, PricePerNight: 1500, AvailableCount: 5},
		{RoomTypeID: 2, Name: "OneRoom", Capacity: 4, PricePerNight: 3000, AvailableCount: 5},
	}
	applyGuestFit(types, 4) // both cost 3000; one room beats two
	if !byName(types, "OneRoom").Recommended || byName(types, "TwoRooms").Recommended {
		t.Fatal("equal price should recommend the option with fewer rooms")
	}
}

func TestApplyGuestFitNothingFits(t *testing.T) {
	types := sampleTypes()
	applyGuestFit(types, 40) // far more than the hotel can hold
	for _, rt := range types {
		if *rt.FitsGuests || rt.Recommended {
			t.Errorf("%s: nothing should fit or be recommended", rt.Name)
		}
	}
}

func TestApplyGuestFitUsesCheapestActivePlanAndIgnoresInactive(t *testing.T) {
	types := []RoomType{{
		RoomTypeID: 1, Name: "Room", Capacity: 2, PricePerNight: 2000, AvailableCount: 3,
		RatePlans: []rooms.RatePlan{plan(500, false), plan(1800, true), plan(1600, true)},
	}}
	applyGuestFit(types, 4)
	if got := types[0].ComboPricePerNight; got != 3200 { // 2 rooms x 1600
		t.Fatalf("combo price = %v, want 3200 (inactive 500 plan ignored)", got)
	}
}

func TestApplyGuestFitZeroCapacityDoesNotDivideByZero(t *testing.T) {
	types := []RoomType{{RoomTypeID: 1, Name: "Odd", Capacity: 0, PricePerNight: 100, AvailableCount: 10}}
	applyGuestFit(types, 3)
	if types[0].RoomsNeeded != 3 {
		t.Fatalf("rooms needed = %d, want 3", types[0].RoomsNeeded)
	}
}
