package chatbot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Maynstayy/ai-chatbot-backend/internal/rooms"
	"github.com/Maynstayy/ai-chatbot-backend/internal/roomtypes"
)

// The stay the guest already chose on the website (fixture "today" is
// 3 Nov 2026), for the hotel on the last result card.
var mealStay = SearchContext{HotelIDs: []int64{11}, CheckIn: "2026-12-10", CheckOut: "2026-12-12", Guests: 2}

func plan(id int64, name string, price float64, breakfast bool, inclusions ...string) rooms.RatePlan {
	return rooms.RatePlan{RatePlanID: id, Name: name, PricePerNight: price, IsActive: true, BreakfastIncluded: breakfast, Inclusions: inclusions}
}

func roomType(id int64, name string, available int, plans ...rooms.RatePlan) roomtypes.RoomType {
	return roomtypes.RoomType{RoomTypeID: id, HotelID: 11, Name: name, Capacity: 2, PricePerNight: 3000, AvailableCount: available, RatePlans: plans}
}

func askMeals(t *testing.T, f *fixture, msg string, c SearchContext) *Output {
	t.Helper()
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: msg, Context: c})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// modelCalled reports whether the AI was asked at all.
func modelCalled(f *fixture) bool { return f.ai.lastReq.System != "" }

func TestBreakfastIncludedIsAnsweredFromThePlans(t *testing.T) {
	f := newFixture()
	f.ai.reply = "Breakfast is not included." // what a guessing model might say
	f.rooms.types = []roomtypes.RoomType{
		roomType(1, "Standard", 2, plan(10, "Room Only", 1000, false), plan(11, "Advance Purchase", 850, false)),
		roomType(2, "Super Deluxe", 1, plan(20, "Breakfast Included", 2300, true), plan(21, "Room Only", 2000, false)),
	}
	out := askMeals(t, f, "Is breakfast included there?", mealStay)

	if modelCalled(f) {
		t.Fatal("the model was asked; the answer must come from the plans")
	}
	want := "At Ganga View Residency for 10 Dec 2026 – 12 Dec 2026, 2 guests, breakfast is included with Super Deluxe – Breakfast Included (₹2,300 a night)."
	if !strings.HasPrefix(out.Reply, want) {
		t.Fatalf("reply:\n%s\nwant prefix:\n%s", out.Reply, want)
	}
	for _, p := range []string{"Standard – Advance Purchase (₹850 a night)", "Standard – Room Only (₹1,000 a night)", "Super Deluxe – Room Only (₹2,000 a night)"} {
		if !strings.Contains(out.Reply, "Not included with") || !strings.Contains(out.Reply, p) {
			t.Errorf("reply should list %q as not including breakfast:\n%s", p, out.Reply)
		}
	}
	if out.Intent != IntentHotelInfo || len(f.rooms.got) != 1 || f.rooms.got[0] != "2026-12-10..2026-12-12" {
		t.Errorf("intent %q, lookups %v", out.Intent, f.rooms.got)
	}
}

func TestBreakfastInTheInclusionTextCounts(t *testing.T) {
	f := newFixture()
	f.rooms.types = []roomtypes.RoomType{roomType(1, "Deluxe", 3, plan(10, "Bed & Breakfast", 2600, false, "Complimentary breakfast", "Wi-Fi"))}
	out := askMeals(t, f, "does the room come with breakfast?", mealStay)
	if !strings.Contains(out.Reply, "breakfast is included with Deluxe – Bed & Breakfast") || strings.Contains(out.Reply, "Not included") {
		t.Fatalf("reply: %s", out.Reply)
	}
}

func TestBreakfastNotIncluded(t *testing.T) {
	f := newFixture()
	f.ai.reply = "Yes, breakfast is included."
	f.rooms.types = []roomtypes.RoomType{roomType(1, "Deluxe", 3, plan(10, "Room Only", 2499, false), plan(11, "Free Cancellation", 2699, false, "Free Wi-Fi"))}
	out := askMeals(t, f, "Is breakfast included?", mealStay)
	if modelCalled(f) {
		t.Fatal("the model was asked")
	}
	want := "None of Ganga View Residency's available plans for 10 Dec 2026 – 12 Dec 2026, 2 guests include breakfast: Deluxe – Room Only (₹2,499 a night); Deluxe – Free Cancellation (₹2,699 a night)."
	if !strings.HasPrefix(out.Reply, want) {
		t.Fatalf("reply:\n%s\nwant prefix:\n%s", out.Reply, want)
	}
	if strings.Contains(strings.ToLower(out.Reply), "is included") {
		t.Fatalf("claims an inclusion: %s", out.Reply)
	}
}

func TestBreakfastUnknownIsNeverGuessed(t *testing.T) {
	cases := []struct {
		name  string
		types []roomtypes.RoomType
		rooms RoomSource
		want  string
	}{
		{"no rate plans listed", []roomtypes.RoomType{roomType(1, "Deluxe", 3)},
			nil, "Ganga View Residency hasn't listed rate plans for 10 Dec 2026 – 12 Dec 2026, 2 guests, so I can't confirm whether breakfast is included."},
		{"no rooms free", []roomtypes.RoomType{roomType(1, "Deluxe", 0, plan(10, "Breakfast Included", 2600, true))},
			nil, "Ganga View Residency has no rooms available for 10 Dec 2026 – 12 Dec 2026, 2 guests, so there are no rate plans to check for breakfast."},
		{"lookup failed", nil, failingRooms{},
			"I couldn't check Ganga View Residency's rate plans just now, so I can't confirm whether breakfast is included."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture()
			f.ai.reply = "Breakfast is included."
			f.rooms.types = c.types
			if c.rooms != nil {
				f.svc.rooms = c.rooms
			}
			out := askMeals(t, f, "Is breakfast included there?", mealStay)
			if modelCalled(f) {
				t.Fatal("the model was asked")
			}
			if !strings.HasPrefix(out.Reply, c.want) {
				t.Fatalf("reply:\n%s\nwant prefix:\n%s", out.Reply, c.want)
			}
			if strings.Contains(out.Reply, "is included with") {
				t.Fatalf("claims an inclusion: %s", out.Reply)
			}
		})
	}
}

type failingRooms struct{}

func (failingRooms) GetRoomTypesByHotel(context.Context, int64, *string, *string, int) ([]roomtypes.RoomType, error) {
	return nil, errors.New("dial tcp 10.0.0.9:443: connection refused")
}

func TestSpecificMealsNeedTheirOwnListing(t *testing.T) {
	f := newFixture()
	f.rooms.types = []roomtypes.RoomType{roomType(1, "Garden Room", 2, plan(10, "With meals", 1800, true, "Vegetarian breakfast and dinner"))}
	if out := askMeals(t, f, "Is dinner included?", mealStay); !strings.Contains(out.Reply, "dinner is included with Garden Room – With meals") {
		t.Fatalf("dinner: %s", out.Reply)
	}
	// Breakfast and dinner are listed, lunch is not: never implied.
	if out := askMeals(t, newFixtureWith(f.rooms.types), "Is lunch included?", mealStay); !strings.Contains(out.Reply, "None of Ganga View Residency's available plans") || !strings.Contains(out.Reply, "include lunch") {
		t.Fatalf("lunch: %s", out.Reply)
	}
	if out := askMeals(t, newFixtureWith(f.rooms.types), "What meals are included?", mealStay); !strings.Contains(out.Reply, "these plans include meals: Garden Room – With meals (₹1,800 a night): Vegetarian breakfast and dinner") {
		t.Fatalf("meals: %s", out.Reply)
	}
}

func newFixtureWith(types []roomtypes.RoomType) *fixture {
	f := newFixture()
	f.rooms.types = types
	return f
}

func TestNamedHotelAndSeveralHotelsOnScreen(t *testing.T) {
	types := []roomtypes.RoomType{roomType(1, "Deluxe", 2, plan(10, "Breakfast Included", 2600, true))}

	// A hotel named in the message, with the stay chosen but no cards in view.
	f := newFixtureWith(types)
	named := mealStay
	named.HotelIDs = nil
	out := askMeals(t, f, "Is breakfast included at Ganga View Residency?", named)
	if modelCalled(f) || !strings.Contains(out.Reply, "At Ganga View Residency for 10 Dec 2026") {
		t.Fatalf("named hotel: %s", out.Reply)
	}

	// Two hotels on the last cards: one line each.
	f = newFixtureWith(types)
	second := haridwarHotel
	second.HotelID, second.Name = 13, "Riverside Palace"
	f.hotels.list = append(f.hotels.list, second)
	f.hotels.byID[13] = &second
	several := mealStay
	several.HotelIDs = []int64{11, 13}
	out = askMeals(t, f, "Which of these include breakfast?", several)
	if !strings.HasPrefix(out.Reply, "For 10 Dec 2026 – 12 Dec 2026, 2 guests:\n• At Ganga View Residency") || !strings.Contains(out.Reply, "\n• At Riverside Palace") {
		t.Fatalf("several hotels:\n%s", out.Reply)
	}
}

func TestWithoutStayDatesTheModelGetsThePlanData(t *testing.T) {
	f := newFixture()
	f.rooms.types = []roomtypes.RoomType{roomType(1, "Deluxe", 2, plan(10, "Breakfast Included", 2600, true, "Breakfast for two"))}
	f.ai.reply = "Breakfast is included with the Breakfast Included plan."
	// No dates chosen: not answered by the router; the model checks rooms itself.
	f.ai.calls = []ToolCall{{Name: toolRoomAvailability, Input: json.RawMessage(`{"hotel_id":11,"check_in":"2026-12-10","check_out":"2026-12-12"}`)}}
	out := askMeals(t, f, "Is breakfast included there?", SearchContext{HotelIDs: []int64{11}})
	if !modelCalled(f) {
		t.Fatal("without dates the question should go to the model")
	}
	var res struct {
		RoomTypes []struct {
			Plans []PlanCard `json:"rate_plans"`
		} `json:"room_types"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(f.ai.results[0]), &res); err != nil {
		t.Fatal(err)
	}
	p := res.RoomTypes[0].Plans[0]
	if !p.BreakfastIncluded || p.Name != "Breakfast Included" || len(p.Inclusions) != 1 || p.Inclusions[0] != "Breakfast for two" {
		t.Fatalf("the model did not get the plan inclusions: %+v", res.RoomTypes)
	}
	if !strings.Contains(res.Note, "never assume") {
		t.Errorf("tool note: %q", res.Note)
	}
	// The website's room cards are unchanged: no plan list in the response.
	b, _ := json.Marshal(out.RoomTypes)
	if len(out.RoomTypes) != 1 || strings.Contains(string(b), "rate_plans") || strings.Contains(string(b), "inclusions") {
		t.Fatalf("room cards changed: %s", b)
	}
}

func TestNotAMealQuestion(t *testing.T) {
	f := newFixture()
	f.rooms.types = []roomtypes.RoomType{roomType(1, "Deluxe", 2, plan(10, "Room Only", 2600, false))}
	_ = askMeals(t, f, "Is GST included in the price?", mealStay)
	if len(f.rooms.got) != 0 {
		t.Fatalf("a tax question ran the meal lookup: %v", f.rooms.got)
	}
	for _, m := range []string{"Is breakfast included?", "what does the room include", "any meals?", "half board available?", "is food included"} {
		if !mealAsk.MatchString(m) {
			t.Errorf("%q not recognised as a meal question", m)
		}
	}
	for _, m := range []string{"Is GST included?", "are taxes included in the price", "Is parking free?", "good food nearby?"} {
		if mealAsk.MatchString(m) {
			t.Errorf("%q wrongly treated as a meal question", m)
		}
	}
}

func TestInclusionsAreCleanedAndCapped(t *testing.T) {
	got := cleanInclusions([]string{"  Breakfast\n\tfor two ", "", strings.Repeat("x", 100), "a", "b", "c", "d", "e"})
	if got[0] != "Breakfast for two" || len([]rune(got[1])) != maxInclusionRunes || len(got) != maxInclusions {
		t.Fatalf("%q", got)
	}
}

// "Which meals?" lists only food: a plan whose breakfast is confirmed by the
// hotel's flag but whose inclusions are other extras says "Breakfast", and
// those extras are never shown as meals.
func TestMealListShowsBreakfastNotOtherExtras(t *testing.T) {
	f := newFixtureWith([]roomtypes.RoomType{
		roomType(1, "Deluxe", 3,
			plan(10, "Breakfast Saver", 2000, true, "Wi-Fi", "Parking"),
			plan(11, "Half Board", 2900, true, "Wi-Fi", "Dinner buffet"),
			plan(12, "Room Only", 1700, false, "Wi-Fi", "Late checkout"),
		),
	})
	out := askMeals(t, f, "What meals are included?", mealStay)
	if modelCalled(f) {
		t.Fatal("the model was asked")
	}
	for _, want := range []string{
		"Deluxe – Breakfast Saver (₹2,000 a night): Breakfast",
		"Deluxe – Half Board (₹2,900 a night): Breakfast, Dinner buffet",
		"Not included with Deluxe – Room Only (₹1,700 a night).",
	} {
		if !strings.Contains(out.Reply, want) {
			t.Errorf("missing %q in:\n%s", want, out.Reply)
		}
	}
	for _, extra := range []string{"Wi-Fi", "Parking", "Late checkout"} {
		if strings.Contains(out.Reply, extra) {
			t.Errorf("non-meal extra %q shown as a meal:\n%s", extra, out.Reply)
		}
	}

	// The same plans asked about breakfast specifically: unchanged.
	f = newFixtureWith(f.rooms.types)
	out = askMeals(t, f, "Is breakfast included?", mealStay)
	if !strings.Contains(out.Reply, "breakfast is included with Deluxe – Breakfast Saver (₹2,000 a night); Deluxe – Half Board (₹2,900 a night).") ||
		!strings.Contains(out.Reply, "Not included with Deluxe – Room Only") || strings.Contains(out.Reply, "Wi-Fi") {
		t.Fatalf("breakfast reply:\n%s", out.Reply)
	}
}

func TestMealsListed(t *testing.T) {
	cases := []struct {
		plan PlanCard
		want string
	}{
		{PlanCard{BreakfastIncluded: true, Inclusions: []string{"Wi-Fi"}}, "Breakfast"},
		{PlanCard{BreakfastIncluded: true}, "Breakfast"},
		{PlanCard{BreakfastIncluded: true, Inclusions: []string{"Buffet breakfast", "Wi-Fi"}}, "Buffet breakfast"},
		{PlanCard{BreakfastIncluded: true, Inclusions: []string{"Spa access", "All meals"}}, "Breakfast, All meals"},
		{PlanCard{Inclusions: []string{"Vegetarian breakfast and dinner", "Parking"}}, "Vegetarian breakfast and dinner"},
		{PlanCard{Inclusions: []string{"Wi-Fi", "Parking"}}, ""},
	}
	for _, c := range cases {
		if got := strings.Join(mealsListed(c.plan), ", "); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.plan, got, c.want)
		}
	}
}
