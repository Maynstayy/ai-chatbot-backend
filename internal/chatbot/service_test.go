package chatbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/bookings"
	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
	"github.com/Maynstayy/ai-chatbot-backend/internal/rooms"
	"github.com/Maynstayy/ai-chatbot-backend/internal/roomtypes"
	"github.com/Maynstayy/ai-chatbot-backend/pkg/cache"
)

// --- fakes for the AI provider and the existing services -----------------

// scriptProvider plays the model: each step may call tools, then it replies.
type scriptProvider struct {
	calls   []ToolCall
	reply   string
	err     error
	block   bool // wait for the context to end (timeout)
	lastReq ProviderRequest
	results []string
	isErr   []bool
}

func (p *scriptProvider) Reply(ctx context.Context, req ProviderRequest) (string, error) {
	p.lastReq = req
	if p.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	for _, c := range p.calls {
		out, isErr := req.Execute(ctx, c)
		p.results = append(p.results, out)
		p.isErr = append(p.isErr, isErr)
	}
	return p.reply, p.err
}

type fakeHotels struct {
	list    []hotels.Hotel
	byID    map[int64]*hotels.Hotel
	err     error
	filters []hotels.HotelFilter
	// emptyForCity returns nothing when the City filter is set (forces the
	// search-term retry).
	emptyForCity bool
	// noResults returns nothing for every search (the catalogue still loads).
	noResults bool
}

func (f *fakeHotels) GetHotels(_ context.Context, filter hotels.HotelFilter) ([]hotels.Hotel, error) {
	// The router's catalogue load (active hotels, no filter) isn't a search.
	catalogue := filter.City == "" && filter.Search == "" && filter.Category == "" && filter.MinPrice == 0 && filter.MaxPrice == 0 && filter.Sort == ""
	if !catalogue {
		f.filters = append(f.filters, filter)
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.noResults && !catalogue {
		return []hotels.Hotel{}, nil
	}
	if f.emptyForCity && filter.City != "" {
		return []hotels.Hotel{}, nil
	}
	return f.list, nil
}

func (f *fakeHotels) GetHotelByID(_ context.Context, id int64) (*hotels.Hotel, error) {
	if h, ok := f.byID[id]; ok {
		return h, nil
	}
	return nil, sql.ErrNoRows
}

type fakeRooms struct {
	types []roomtypes.RoomType
	got   []string
}

func (f *fakeRooms) GetRoomTypesByHotel(_ context.Context, hotelID int64, in, out *string, guests int) ([]roomtypes.RoomType, error) {
	f.got = append(f.got, *in+".."+*out)
	return f.types, nil
}

type fakeBookings struct {
	byID     map[int64]*bookings.Booking
	mine     map[int64][]bookings.Booking
	err      error
	statuses []string
	lookups  []int64
}

func (f *fakeBookings) GetBookingByID(_ context.Context, id int64) (*bookings.Booking, error) {
	f.lookups = append(f.lookups, id)
	if f.err != nil {
		return nil, f.err
	}
	if b, ok := f.byID[id]; ok {
		return b, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeBookings) GetMyBookings(_ context.Context, userID int64, status string) ([]bookings.Booking, error) {
	f.statuses = append(f.statuses, status)
	if f.err != nil {
		return nil, f.err
	}
	return f.mine[userID], nil
}

type fakePayments struct{ paid map[int64]bool }

func (f *fakePayments) HasPaidPayment(_ context.Context, id int64) (bool, error) {
	return f.paid[id], nil
}

var haridwarHotel = hotels.Hotel{
	HotelID: 11, Name: "Ganga View Residency", City: "Haridwar", PricePerNight: 2499, TaxPerNight: 125,
	Rating: 4.4, TotalReviews: 120, Amenities: []string{"WiFi", "Parking"}, IsActive: true,
	FreeCancellation: true, CancellationHours: intPtr(24), CheckInTime: "12:00",
}

func intPtr(n int) *int { return &n }

type fixture struct {
	svc      *Service
	ai       *scriptProvider
	hotels   *fakeHotels
	rooms    *fakeRooms
	bookings *fakeBookings
}

func newFixture() *fixture {
	h := haridwarHotel
	inactive := hotels.Hotel{HotelID: 12, Name: "Hidden", IsActive: false}
	f := &fixture{
		ai:     &scriptProvider{reply: "ok"},
		hotels: &fakeHotels{list: []hotels.Hotel{h}, byID: map[int64]*hotels.Hotel{11: &h, 12: &inactive}},
		rooms: &fakeRooms{types: []roomtypes.RoomType{{RoomTypeID: 5, HotelID: 11, Name: "Deluxe", Capacity: 2,
			PricePerNight: 2499, AvailableCount: 3}}},
		bookings: &fakeBookings{
			byID: map[int64]*bookings.Booking{
				100: {BookingID: 100, UserID: 7, ItemType: "hotel", ItemID: 11, Status: "confirmed",
					CheckIn: "2026-12-01", CheckOut: "2026-12-03", TotalAmount: 5248},
				200: {BookingID: 200, UserID: 8, ItemType: "hotel", ItemID: 11, Status: "confirmed"},
			},
			mine: map[int64][]bookings.Booking{7: {{BookingID: 100, UserID: 7, ItemType: "hotel", ItemID: 11, Status: "confirmed"}}},
		},
	}
	now := func() time.Time { return time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC) }
	mem := newMemoryStore()
	mem.now = now
	f.svc = NewService(f.ai, &hybridStore{redis: cache.New(""), mem: mem}, f.hotels, f.rooms, f.bookings,
		&fakePayments{paid: map[int64]bool{100: true}}, time.Second)
	f.svc.now = now
	return f
}

func toolNames(specs []ToolSpec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// --- tests ---------------------------------------------------------------

func TestBasicMessage(t *testing.T) {
	f := newFixture()
	f.ai.reply = "Namaste! How can I help?\n<meta intent=\"greeting\" needs_info=\"false\" needs_login=\"false\"/>"
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "  hotel tips please  "})
	if err != nil {
		t.Fatal(err)
	}
	if out.Reply != "Namaste! How can I help?" || out.Intent != IntentGreeting || out.RequiresMoreInformation {
		t.Fatalf("unexpected output: %+v", out)
	}
	if !conversationIDPattern.MatchString(out.ConversationID) {
		t.Fatalf("bad conversation id %q", out.ConversationID)
	}
	if out.Hotels == nil || out.RoomTypes == nil {
		t.Fatal("hotels/room_types must be empty arrays, not null")
	}
	last := f.ai.lastReq.History[len(f.ai.lastReq.History)-1]
	if last.Role != "user" || last.Text != "hotel tips please" {
		t.Fatalf("message not trimmed/passed: %+v", last)
	}
}

func TestHotelSearchAsksForMissingInformation(t *testing.T) {
	// A destination is enough: the hotels come back straight away, in
	// English, without asking for dates, guests or rooms and without the model.
	f := newFixture()
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "Mujhe Haridwar mein hotel chahiye", Language: "hinglish"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Reply != "Sure! Here are some hotels available in Haridwar." || out.Intent != IntentHotelSearch ||
		out.RequiresMoreInformation || len(out.Hotels) != 1 || out.Hotels[0].HotelID != 11 || f.ai.lastReq.System != "" {
		t.Fatalf("unexpected output: %+v", out)
	}
}

func TestHotelSearchUsesExistingService(t *testing.T) {
	f := newFixture()
	f.ai.calls = []ToolCall{{Name: toolSearchHotels, Input: json.RawMessage(`{"city":"Haridwar","max_price":3000,"sort":"price_asc"}`)}}
	f.ai.reply = "Ganga View Residency is from Rs 2499 a night." // no meta line: intent comes from the tools
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "Which hotels in Haridwar under 3000 are best?"})
	if err != nil {
		t.Fatal(err)
	}
	got := f.hotels.filters[len(f.hotels.filters)-1]
	if got.City != "Haridwar" || got.MaxPrice != 3000 || got.Sort != "price_asc" || got.IsActive == nil || !*got.IsActive {
		t.Fatalf("filter not passed through: %+v", got)
	}
	if f.ai.isErr[0] || !strings.Contains(f.ai.results[0], "Ganga View Residency") {
		t.Fatalf("tool result: %s", f.ai.results[0])
	}
	if out.Intent != IntentHotelSearch || len(out.Hotels) != 1 || out.Hotels[0].HotelID != 11 || out.Hotels[0].PricePerNight != 2499 {
		t.Fatalf("hotels not returned from the real data: %+v", out)
	}
}

func TestHotelSearchRetriesWithoutUnmatchedCity(t *testing.T) {
	f := newFixture()
	f.hotels.emptyForCity = true
	f.ai.calls = []ToolCall{{Name: toolSearchHotels, Input: json.RawMessage(`{"city":"Ganga View Residency","query":"Ganga View Residency"}`)}}
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "tell me about Ganga View Residency"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.hotels.filters) != 2 || f.hotels.filters[1].City != "" || f.hotels.filters[1].Search != "Ganga View Residency" {
		t.Fatalf("no retry without the city: %+v", f.hotels.filters)
	}
	if len(out.Hotels) != 1 || out.Hotels[0].HotelID != 11 {
		t.Fatalf("hotel not found on retry: %+v", out.Hotels)
	}
}

func TestHotelDetailsMarksMissingPolicies(t *testing.T) {
	f := newFixture()
	f.hotels.byID[11].PetPolicy = "Pets allowed on request"
	f.ai.calls = []ToolCall{{Name: toolHotelDetails, Input: json.RawMessage(`{"hotel_id":11}`)}}
	if _, err := f.svc.HandleMessage(context.Background(), Input{Message: "pets?"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(f.ai.results[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got["pet_policy"] != "Pets allowed on request" || got["child_policy"] != notStated || got["id_rules"] != notStated {
		t.Fatalf("policies: %v / %v / %v", got["pet_policy"], got["child_policy"], got["id_rules"])
	}
}

func TestHotelSearchEmptyResults(t *testing.T) {
	// A place SKYMCM doesn't list: said plainly, no model, nothing invented.
	f := newFixture()
	f.hotels.list = []hotels.Hotel{}
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "hotels in Nowhere"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Reply, "Please enter a valid city or hotel name from our listings.") || len(out.Hotels) != 0 || f.ai.lastReq.System != "" {
		t.Fatalf("unlisted place: %+v", out)
	}

	// The tool itself: no city match, retried as a search term, still empty.
	f = newFixture()
	f.hotels.list = []hotels.Hotel{}
	run := &toolRun{svc: f.svc, evidence: "hotels in nowhere"}
	result, isErr := run.execute(context.Background(), ToolCall{Name: toolSearchHotels, Input: json.RawMessage(`{"city":"Nowhere"}`)})
	if isErr || !strings.Contains(result, `"total_matches":0`) {
		t.Fatalf("expected empty result, got %s", result)
	}
	if len(f.hotels.filters) != 2 || f.hotels.filters[1].Search != "Nowhere" || f.hotels.filters[1].City != "" {
		t.Fatalf("no search-term retry: %+v", f.hotels.filters)
	}
}

func TestHotelServiceFailureIsHiddenFromModel(t *testing.T) {
	f := newFixture()
	f.hotels.err = errors.New("dial tcp 10.0.0.5:3306: connection refused")
	f.ai.calls = []ToolCall{{Name: toolSearchHotels, Input: json.RawMessage(`{"city":"Haridwar"}`)}}
	f.ai.reply = "Search is unavailable right now."
	if _, err := f.svc.HandleMessage(context.Background(), Input{Message: "hotels"}); err != nil {
		t.Fatal(err)
	}
	if !f.ai.isErr[0] || strings.Contains(f.ai.results[0], "3306") || strings.Contains(f.ai.results[0], "dial") {
		t.Fatalf("internal error leaked to the model: %s", f.ai.results[0])
	}
}

func TestRoomAvailabilityAndDateValidation(t *testing.T) {
	f := newFixture()
	f.ai.calls = []ToolCall{
		{Name: toolRoomAvailability, Input: json.RawMessage(`{"hotel_id":11,"check_in":"2026-12-01","check_out":"2026-12-03","guests":2}`)},
		{Name: toolRoomAvailability, Input: json.RawMessage(`{"hotel_id":11,"check_in":"2026-10-01","check_out":"2026-10-03"}`)},
		{Name: toolRoomAvailability, Input: json.RawMessage(`{"hotel_id":11,"check_in":"2026-12-01","check_out":"2027-02-01"}`)},
		{Name: toolRoomAvailability, Input: json.RawMessage(`{"hotel_id":12,"check_in":"2026-12-01","check_out":"2026-12-03"}`)},
	}
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "rooms?"})
	if err != nil {
		t.Fatal(err)
	}
	if f.ai.isErr[0] || len(out.RoomTypes) != 1 || out.RoomTypes[0].AvailableCount != 3 {
		t.Fatalf("availability not returned: %v %+v", f.ai.results[0], out.RoomTypes)
	}
	if !f.ai.isErr[1] || !strings.Contains(f.ai.results[1], "past") {
		t.Fatalf("past date accepted: %s", f.ai.results[1])
	}
	if !f.ai.isErr[2] || !strings.Contains(f.ai.results[2], "30 nights") {
		t.Fatalf("long stay accepted: %s", f.ai.results[2])
	}
	if !f.ai.isErr[3] || len(f.rooms.got) != 1 {
		t.Fatalf("inactive hotel reachable: %s", f.ai.results[3])
	}
}

func TestRoomCardCarriesBookableRatePlan(t *testing.T) {
	f := newFixture()
	two, one := 2, 1
	f.rooms.types = []roomtypes.RoomType{
		{RoomTypeID: 5, HotelID: 11, Name: "Deluxe", Capacity: 2, PricePerNight: 2499, AvailableCount: 3, RoomsNeeded: 2,
			RatePlans: []rooms.RatePlan{
				{RatePlanID: 1, Name: "Inactive", PricePerNight: 1000, IsActive: false},
				{RatePlanID: 2, Name: "Last room", PricePerNight: 1500, IsActive: true, RoomsLeft: &one},
				{RatePlanID: 3, Name: "With breakfast", PricePerNight: 2899, TaxPerNight: 145, IsActive: true, RoomsLeft: &two},
				{RatePlanID: 4, Name: "Room only", PricePerNight: 2499, TaxPerNight: 125, IsActive: true},
			}},
		{RoomTypeID: 6, HotelID: 11, Name: "No plans", Capacity: 2, PricePerNight: 1999, AvailableCount: 1},
	}
	f.ai.calls = []ToolCall{{Name: toolRoomAvailability,
		Input: json.RawMessage(`{"hotel_id":11,"check_in":"2026-12-01","check_out":"2026-12-03","guests":4}`)}}
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "rooms?"})
	if err != nil || len(out.RoomTypes) != 2 {
		t.Fatalf("%v %+v", err, out)
	}
	got := out.RoomTypes[0]
	if got.RatePlanID != 4 || got.RatePlanName != "Room only" || got.RatePlanPricePerNight != 2499 || got.RatePlanTaxPerNight != 125 || got.Guests != 4 {
		t.Fatalf("wrong plan picked: %+v", got)
	}
	if out.RoomTypes[1].RatePlanID != 0 {
		t.Fatalf("plan invented for a type without plans: %+v", out.RoomTypes[1])
	}
	if !strings.Contains(f.ai.results[0], `"rate_plan_id":4`) {
		t.Fatalf("plan not in tool result: %s", f.ai.results[0])
	}
}

func TestToolArgumentsAreValidated(t *testing.T) {
	f := newFixture()
	f.ai.calls = []ToolCall{
		{Name: toolSearchHotels, Input: json.RawMessage(`{"city":"x","sql":"DROP TABLE hotels"}`)},
		{Name: toolSearchHotels, Input: json.RawMessage(`{"city":"Goa'; DELETE FROM bookings; --"}`)},
		{Name: "run_sql", Input: json.RawMessage(`{"q":"SELECT 1"}`)},
		{Name: toolHotelDetails, Input: json.RawMessage(`{"hotel_id":-1}`)},
	}
	if _, err := f.svc.HandleMessage(context.Background(), Input{Message: "x"}); err != nil {
		t.Fatal(err)
	}
	for i, isErr := range f.ai.isErr {
		if !isErr {
			t.Errorf("call %d accepted: %s", i, f.ai.results[i])
		}
	}
	if len(f.hotels.filters) != 0 {
		t.Fatal("invalid arguments reached the hotel service")
	}
}

func TestAnonymousCallerHasNoBookingTools(t *testing.T) {
	f := newFixture()
	// Even if the model calls a booking tool it wasn't offered, it's refused.
	f.ai.calls = []ToolCall{{Name: toolGetBookingStatus, Input: json.RawMessage(`{"booking_id":100}`)}}
	f.ai.reply = "Please log in.\n<meta intent=\"booking_status\" needs_info=\"false\" needs_login=\"true\"/>"
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "hotel tips please"})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range toolNames(f.ai.lastReq.Tools) {
		if n == toolGetBookingStatus || n == toolListMyBookings {
			t.Fatalf("booking tool %s offered to an anonymous caller", n)
		}
	}
	if !f.ai.isErr[0] || strings.Contains(f.ai.results[0], "confirmed") {
		t.Fatalf("anonymous booking lookup allowed: %s", f.ai.results[0])
	}
	if !out.RequiresLogin {
		t.Fatal("requires_login not set")
	}
}

func TestBookingOwnership(t *testing.T) {
	// Booking ids the guest names are answered straight from the lookup:
	// their own booking in full, someone else's exactly like a missing one.
	f := newFixture()
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "check booking 100, booking 200 and booking 999", UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if f.ai.lastReq.System != "" {
		t.Fatal("booking status should not need the model")
	}
	if !strings.Contains(out.Reply, "Booking #100 at Ganga View Residency") || !strings.Contains(out.Reply, "is confirmed") ||
		!strings.Contains(out.Reply, "The online payment is recorded as paid.") {
		t.Fatalf("own booking: %s", out.Reply)
	}
	if !strings.Contains(out.Reply, "I couldn't find booking #200 on your account") || !strings.Contains(out.Reply, "I couldn't find booking #999 on your account") {
		t.Fatalf("another user's booking must look like a missing one: %s", out.Reply)
	}
	if len(out.Bookings) != 1 || out.Bookings[0].BookingID != 100 || out.Intent != IntentBookingStatus {
		t.Fatalf("bookings %+v intent %q", out.Bookings, out.Intent)
	}

	// The model is offered the booking tools for a signed-in guest, and they
	// are still scoped to that guest.
	f = newFixture()
	f.ai.calls = []ToolCall{{Name: toolListMyBookings, Input: json.RawMessage(`{}`)}}
	if _, err := f.svc.HandleMessage(context.Background(), Input{Message: "tell me about hotel rules", UserID: 7}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(toolNames(f.ai.lastReq.Tools), ","), toolGetBookingStatus) {
		t.Fatal("booking tools not offered to a signed-in caller")
	}
	if strings.Contains(f.ai.results[0], `"booking_id":200`) || !strings.Contains(f.ai.results[0], `"booking_id":100`) {
		t.Fatalf("list not scoped to the caller: %s", f.ai.results[0])
	}
}

func TestBookingLookupFailureIsGeneric(t *testing.T) {
	f := newFixture()
	f.bookings.err = errors.New("Error 1045 (28000): Access denied for user 'root'")
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "booking 100", UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Reply, "root") || strings.Contains(out.Reply, "1045") || !strings.Contains(out.Reply, "I couldn't look up booking #100 just now") {
		t.Fatalf("db error leaked or not reported: %s", out.Reply)
	}
	if out.BookingsChecked {
		t.Fatal("a failed lookup must not count as checked")
	}
}

func TestProviderFailureAndTimeout(t *testing.T) {
	f := newFixture()
	f.ai.err = errors.New("upstream 529 overloaded")
	if _, err := f.svc.HandleMessage(context.Background(), Input{Message: "hotel tips please"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}

	f = newFixture()
	f.ai.block = true
	f.svc.timeout = 30 * time.Millisecond
	start := time.Now()
	if _, err := f.svc.HandleMessage(context.Background(), Input{Message: "hotel tips please"}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestProviderRefusalGetsPoliteReply(t *testing.T) {
	f := newFixture()
	f.ai.err = ErrProviderRefused
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "x", Language: "hindi"})
	if err != nil || out.Reply == "" {
		t.Fatalf("refusal: %v %+v", err, out)
	}
}

func TestInvalidInput(t *testing.T) {
	f := newFixture()
	cases := map[string]Input{
		"empty":    {Message: "   \x00 "},
		"too long": {Message: strings.Repeat("a", maxMessageRunes+1)},
		"bad lang": {Message: "hi", Language: "klingon"},
	}
	for name, in := range cases {
		if _, err := f.svc.HandleMessage(context.Background(), in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if f.ai.lastReq.System != "" {
		t.Fatal("invalid input reached the provider")
	}
}

func TestNotConfigured(t *testing.T) {
	svc := NewService(nil, NewConversationStore(cache.New("")), &fakeHotels{}, &fakeRooms{}, &fakeBookings{}, nil, time.Second)
	if _, err := svc.HandleMessage(context.Background(), Input{Message: "hi"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestConversationContinuesOnlyForItsOwner(t *testing.T) {
	f := newFixture()
	f.ai.reply = "Which dates?"
	first, err := f.svc.HandleMessage(context.Background(), Input{Message: "Hotel in Haridwar", UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.HandleMessage(context.Background(), Input{Message: "1 to 3 December", UserID: 7, ConversationID: first.ConversationID})
	if err != nil {
		t.Fatal(err)
	}
	if second.ConversationID != first.ConversationID || len(f.ai.lastReq.History) != 3 ||
		f.ai.lastReq.History[0].Text != "Hotel in Haridwar" || f.ai.lastReq.History[1].Text != "Sure! Here are some hotels available in Haridwar." {
		t.Fatalf("context not continued: %+v", f.ai.lastReq.History)
	}

	// Another caller presenting the same id gets a fresh conversation.
	third, err := f.svc.HandleMessage(context.Background(), Input{Message: "hotel tips please", UserID: 8, ConversationID: first.ConversationID})
	if err != nil {
		t.Fatal(err)
	}
	if third.ConversationID == first.ConversationID || len(f.ai.lastReq.History) != 1 {
		t.Fatalf("conversation leaked across users: %+v", f.ai.lastReq.History)
	}
}

func TestTrimTurns(t *testing.T) {
	var turns []Turn
	for i := 0; i < 20; i++ {
		turns = append(turns, Turn{Role: "user", Text: "q"}, Turn{Role: "assistant", Text: strings.Repeat("a", 3000)})
	}
	got := trimTurns(turns)
	if len(got) > maxStoredTurns || got[0].Role != "user" || len([]rune(got[1].Text)) != maxStoredTurnRunes {
		t.Fatalf("trim: %d turns, first %s", len(got), got[0].Role)
	}
}

func TestParseMeta(t *testing.T) {
	reply, intent, info, login := parseMeta("Hi there\n<meta intent=\"payment\" needs_info=\"true\" needs_login=\"true\"/>")
	if reply != "Hi there" || intent != IntentPayment || !info || !login {
		t.Fatalf("parse: %q %q %v %v", reply, intent, info, login)
	}
	reply, intent, _, _ = parseMeta("Hi\n<meta intent=\"hack\"/>")
	if reply != "Hi" || intent != "" {
		t.Fatalf("unknown intent accepted: %q", intent)
	}
	reply, _, _, _ = parseMeta("No meta here")
	if reply != "No meta here" {
		t.Fatal(reply)
	}
	// Small models sometimes write the line first, or twice.
	reply, intent, _, _ = parseMeta("<meta intent=\"hotel_search\" needs_info=\"false\" needs_login=\"false\"/>\n\nHere are hotels in Agra.")
	if reply != "Here are hotels in Agra." || intent != IntentHotelSearch {
		t.Fatalf("meta first: %q %q", reply, intent)
	}
	reply, intent, _, _ = parseMeta("<meta intent=\"general\"/>Hi<meta intent=\"travel\"/>")
	if reply != "Hi" || intent != IntentTravel {
		t.Fatalf("two metas: %q %q", reply, intent)
	}
}
