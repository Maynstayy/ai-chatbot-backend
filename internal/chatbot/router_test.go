package chatbot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func lastFilterPrices(f *fixture) (float64, float64) {
	got := f.hotels.filters[len(f.hotels.filters)-1]
	return got.MinPrice, got.MaxPrice
}

func TestInventedPriceFiltersAreDropped(t *testing.T) {
	cases := []struct {
		msg, args        string
		wantMin, wantMax float64
	}{
		// "Agra" with an invented 300-500 range: both dropped.
		{"hotels in Haridwar", `{"city":"Haridwar","min_price":300,"max_price":500}`, 0, 0},
		// "under 2000" sent as min=max=2000: a ceiling only.
		{"show me hotels in Haridwar under 2000", `{"city":"Haridwar","min_price":2000,"max_price":2000}`, 0, 2000},
		// "under 2000" sent as min_price only: turned into the ceiling.
		{"show me hotels in Haridwar under 2000", `{"city":"Haridwar","min_price":2000}`, 0, 2000},
		// "above 2000" stays a floor.
		{"Haridwar hotels above 2000", `{"city":"Haridwar","min_price":2000}`, 2000, 0},
		// min 0 is not a filter.
		{"show me hotels in Haridwar under 2000", `{"city":"Haridwar","min_price":0,"max_price":2000}`, 0, 2000},
		// "2k" counts as 2000.
		{"Haridwar stays under 2k", `{"city":"Haridwar","max_price":2000}`, 0, 2000},
		// A range the guest gave is kept.
		{"Haridwar hotels between 1000 and 3000", `{"city":"Haridwar","min_price":1000,"max_price":3000}`, 1000, 3000},
		// Exactly is exactly.
		{"Haridwar hotels for exactly 2000", `{"city":"Haridwar","min_price":2000,"max_price":2000}`, 2000, 2000},
	}
	for _, c := range cases {
		f := newFixture()
		f.ai.calls = []ToolCall{{Name: toolSearchHotels, Input: json.RawMessage(c.args)}}
		if _, err := f.svc.HandleMessage(context.Background(), Input{Message: c.msg + " - which one is best?"}); err != nil {
			t.Fatal(err)
		}
		if minP, maxP := lastFilterPrices(f); minP != c.wantMin || maxP != c.wantMax {
			t.Errorf("%q %s: got min %v max %v, want %v %v", c.msg, c.args, minP, maxP, c.wantMin, c.wantMax)
		}
	}
}

func TestInventedCategoryAndQueryAreDropped(t *testing.T) {
	f := newFixture()
	f.ai.calls = []ToolCall{{Name: toolSearchHotels, Input: json.RawMessage(`{"city":"Haridwar","category":"Luxury","query":"riverside"}`)}}
	if _, err := f.svc.HandleMessage(context.Background(), Input{Message: "Which hotels in Haridwar are quiet?"}); err != nil {
		t.Fatal(err)
	}
	got := f.hotels.filters[len(f.hotels.filters)-1]
	if got.Category != "" || got.Search != "" || got.City != "Haridwar" {
		t.Fatalf("invented filters kept: %+v", got)
	}
}

func TestGuidedBudgetCountsAsStated(t *testing.T) {
	f := newFixture()
	f.ai.calls = []ToolCall{{Name: toolSearchHotels, Input: json.RawMessage(`{"city":"Haridwar","max_price":3000}`)}}
	in := Input{Message: "which of these is closest to the ghats?", Context: SearchContext{Destination: "Haridwar", BudgetMax: 3000}}
	if _, err := f.svc.HandleMessage(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, maxP := lastFilterPrices(f); maxP != 3000 {
		t.Fatalf("budget from the guided search dropped: %v", maxP)
	}
	if !strings.Contains(f.ai.lastReq.System, "destination Haridwar") || !strings.Contains(f.ai.lastReq.System, "budget up to INR 3000") {
		t.Fatal("guided-search context not in the prompt")
	}
}

func TestBookingStatusFilterOnlyWhenAsked(t *testing.T) {
	f := newFixture()
	run := &toolRun{svc: f.svc, userID: 7, evidence: "show me what i booked"}
	if _, isErr := run.execute(context.Background(), ToolCall{Name: toolListMyBookings, Input: json.RawMessage(`{"status":"pending"}`)}); isErr {
		t.Fatal("list failed")
	}
	run = &toolRun{svc: f.svc, userID: 7, evidence: "show my cancelled ones"}
	run.execute(context.Background(), ToolCall{Name: toolListMyBookings, Input: json.RawMessage(`{"status":"cancelled"}`)})
	if strings.Join(f.bookings.statuses, ",") != ",cancelled" {
		t.Fatalf("status filters passed: %q", f.bookings.statuses)
	}
}

func TestInventedBookingIDIsRejected(t *testing.T) {
	f := newFixture()
	run := &toolRun{svc: f.svc, userID: 7, evidence: "what is the status of my booking?", bookingEvidence: "what is the status of my booking?"}
	out, isErr := run.execute(context.Background(), ToolCall{Name: toolGetBookingStatus, Input: json.RawMessage(`{"booking_id":12345}`)})
	if !isErr || !strings.Contains(out, "has not given this booking id") || len(f.bookings.lookups) != 0 {
		t.Fatalf("invented id looked up: %s %v", out, f.bookings.lookups)
	}
	// The website's hotel-id hint doesn't count as a booking id the guest typed.
	f = newFixture()
	f.ai.calls = []ToolCall{{Name: toolGetBookingStatus, Input: json.RawMessage(`{"booking_id":41}`)}}
	if _, err := f.svc.HandleMessage(context.Background(),
		Input{Message: "is it paid?\n(SKYMCM hotel ids: Hotel The First = hotel_id 41)", UserID: 7}); err != nil {
		t.Fatal(err)
	}
	if !f.ai.isErr[0] || len(f.bookings.lookups) != 0 {
		t.Fatalf("hotel id used as a booking id: %s", f.ai.results[0])
	}
}

func TestBookingListIsAnsweredFromTheTool(t *testing.T) {
	f := newFixture()
	f.bookings.mine[7][0].CheckIn, f.bookings.mine[7][0].CheckOut = "2026-12-01", "2026-12-03"
	f.bookings.mine[7][0].Guests, f.bookings.mine[7][0].TotalAmount = 2, 5248
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "View my bookings", UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if f.ai.lastReq.System != "" {
		t.Fatal("a booking list should not need the model")
	}
	if !out.BookingsChecked || len(out.Bookings) != 1 || out.Bookings[0].BookingID != 100 || out.Bookings[0].HotelName != "Ganga View Residency" {
		t.Fatalf("bookings: %+v", out)
	}
	for _, want := range []string{"Booking #100", "Ganga View Residency", "1 Dec 2026 to 3 Dec 2026", "2 guests", "confirmed", "₹5,248"} {
		if !strings.Contains(out.Reply, want) {
			t.Errorf("reply missing %q: %s", want, out.Reply)
		}
	}
	if out.Intent != IntentBookingStatus || out.RequiresLogin || f.bookings.statuses[0] != "" {
		t.Fatalf("intent %q login %v status %q", out.Intent, out.RequiresLogin, f.bookings.statuses[0])
	}
	// A conversation turn is still saved, so the next message has context.
	next, _ := f.svc.HandleMessage(context.Background(), Input{Message: "which hotel was that at?", UserID: 7, ConversationID: out.ConversationID})
	if next.ConversationID != out.ConversationID || len(f.ai.lastReq.History) != 3 {
		t.Fatalf("conversation not continued: %+v", f.ai.lastReq.History)
	}
}

func TestBookingListFiltersAndEmpty(t *testing.T) {
	f := newFixture()
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "show my confirmed bookings", UserID: 7})
	if f.bookings.statuses[0] != "confirmed" || !strings.Contains(out.Reply, "confirmed bookings") {
		t.Fatalf("status %q reply %s", f.bookings.statuses, out.Reply)
	}
	f = newFixture()
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "View my bookings", UserID: 9})
	if !out.BookingsChecked || len(out.Bookings) != 0 || !strings.Contains(out.Reply, "don't have any bookings") {
		t.Fatalf("empty list: %+v", out)
	}
}

func TestBookingListFailureIsNotReportedAsEmpty(t *testing.T) {
	f := newFixture()
	f.bookings.err = errorString("Error 1045 (28000): Access denied")
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "View my bookings", UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if out.BookingsChecked || strings.Contains(out.Reply, "don't have") || strings.Contains(out.Reply, "1045") || !strings.Contains(out.Reply, "couldn't load") {
		t.Fatalf("failure reported as empty or leaked: %+v", out)
	}
}

func TestLoggedOutBookingRequestsGetTheLoginPrompt(t *testing.T) {
	for _, msg := range []string{"View my bookings", "status of booking 100", "Is booking #100 paid?"} {
		f := newFixture()
		out, err := f.svc.HandleMessage(context.Background(), Input{Message: msg})
		if err != nil {
			t.Fatal(err)
		}
		if !out.RequiresLogin || !strings.Contains(out.Reply, "log in") || f.ai.lastReq.System != "" ||
			len(f.bookings.lookups) != 0 || len(f.bookings.statuses) != 0 || len(out.Bookings) != 0 {
			t.Fatalf("%q: %+v", msg, out)
		}
	}
}

func TestBookingIDTypedByTheGuestIsLookedUpFirst(t *testing.T) {
	f := newFixture()
	f.ai.reply = "Here are your most recent bookings: ..." // never used
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "I need to cancel my booking 100", UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !out.BookingsChecked || len(out.Bookings) != 1 || f.ai.lastReq.System != "" {
		t.Fatalf("booking not looked up directly: %+v", out)
	}
	if !strings.Contains(out.Reply, "Booking #100 at Ganga View Residency (1 Dec 2026 to 3 Dec 2026") ||
		!strings.Contains(out.Reply, "To cancel, open My Bookings") {
		t.Fatalf("reply: %s", out.Reply)
	}
	// Hinglish in, English out.
	f = newFixture()
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "booking 100 ka status kya hai", UserID: 7})
	if !strings.Contains(out.Reply, "Booking #100 at Ganga View Residency") || !strings.Contains(out.Reply, "is confirmed") {
		t.Fatalf("hinglish: %s", out.Reply)
	}
}

func TestHinglishBookingListAndNoStaleRepeats(t *testing.T) {
	f := newFixture()
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "meri bookings dikhao", UserID: 7})
	if !out.BookingsChecked || !strings.Contains(out.Reply, "Here are your most recent bookings") || f.ai.lastReq.System != "" {
		t.Fatalf("hinglish list: %+v", out)
	}
	// The model copying the previous reply is replaced.
	f.ai.reply = out.Reply
	next, _ := f.svc.HandleMessage(context.Background(), Input{Message: "tell me about hotel rules", UserID: 7, ConversationID: out.ConversationID})
	if next.Reply == out.Reply || next.Reply != safeFallback() {
		t.Fatalf("stale repeat not replaced: %q", next.Reply)
	}
}

func TestHowToBookingQuestionsAreNotBookingLookups(t *testing.T) {
	f := newFixture()
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "how do I cancel my booking", UserID: 7})
	if out.BookingsChecked || len(f.bookings.statuses) != 0 || len(f.bookings.lookups) != 0 {
		t.Fatalf("how-to question routed as a lookup: %+v", out)
	}
	if !strings.Contains(out.Reply, "open My Bookings") || out.Intent != IntentCancellation {
		t.Fatalf("how-to answer: %+v", out)
	}
}

func TestSitePolicyQuestionsGetThePublishedAnswer(t *testing.T) {
	for msg, want := range map[string]string{
		"What is the cancellation policy?": "Cancellation terms vary by property",
		"How long do refunds take?":        "5–7 business days",
		"What are the payment options?":    "You pay online at the last step of booking",
	} {
		f := newFixture()
		out, _ := f.svc.HandleMessage(context.Background(), Input{Message: msg})
		if !strings.Contains(out.Reply, want) || f.ai.lastReq.System != "" || len(f.hotels.filters) != 0 {
			t.Errorf("%q: %q", msg, out.Reply)
		}
	}
	// About a hotel in view: that hotel's own data, not the site-wide text.
	f := newFixture()
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Does Ganga View Residency have free cancellation?"})
	if !strings.Contains(out.Reply, "With free cancellation: Ganga View Residency.") {
		t.Fatalf("hotel cancellation: %q", out.Reply)
	}
}

func TestNamedCityAndHotelAreLookedUpBeforeTheModel(t *testing.T) {
	f := newFixture()
	f.ai.reply = "Sure, what is your budget?" // the model skips its tools
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "I want to book hotels in Haridwar"})
	if len(out.Hotels) != 1 || out.Hotels[0].HotelID != 11 || out.Intent != IntentHotelSearch {
		t.Fatalf("city not looked up: %+v", out)
	}
	if got := f.hotels.filters[0]; got.City != "Haridwar" || got.MinPrice != 0 || got.MaxPrice != 0 {
		t.Fatalf("routed search filter: %+v", got)
	}

	f = newFixture()
	f.ai.reply = "Check-in is at 12:00."
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "What are the check-in times at Ganga View Residency?"})
	if !strings.Contains(f.ai.lastReq.System, `get_hotel_details {"hotel_id":11}`) || len(out.Hotels) != 1 {
		t.Fatalf("hotel details not prefetched: %s", f.ai.lastReq.System)
	}

	// A travel question that only mentions the city doesn't pull in hotel cards.
	f = newFixture()
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "what is the best time to visit Haridwar?"})
	if len(out.Hotels) != 0 {
		t.Fatalf("hotel cards on a travel question: %+v", out.Hotels)
	}
}

func TestRoutedSearchIsNotOfferedAgainAndPromptIsCompact(t *testing.T) {
	f := newFixture()
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Which hotels in Haridwar are quiet?"})
	for _, n := range toolNames(f.ai.lastReq.Tools) {
		if n == toolSearchHotels {
			t.Fatal("search_hotels offered again after the router answered it")
		}
	}
	if !strings.Contains(f.ai.lastReq.System, "price_per_night_before_gst") || strings.Contains(f.ai.lastReq.System, "thumbnail_url") {
		t.Fatal("model prompt should carry the compact hotel data")
	}
	if len(out.Hotels) != 1 || out.Hotels[0].Amenities == nil {
		t.Fatalf("website cards must keep full data: %+v", out.Hotels)
	}
	// Nothing found: the model keeps search_hotels to try another way.
	f = newFixture()
	f.hotels.noResults = true
	f.svc.HandleMessage(context.Background(), Input{Message: "Which hotels in Haridwar are quiet?"})
	found := false
	for _, n := range toolNames(f.ai.lastReq.Tools) {
		found = found || n == toolSearchHotels
	}
	if !found {
		t.Fatal("search_hotels withdrawn although the routed search found nothing")
	}
}

func TestModelFailureAfterLookupAnswersFromTheData(t *testing.T) {
	f := newFixture()
	f.ai.err = ErrProviderEmpty
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "Which hotels in Haridwar are quiet?"})
	if err != nil {
		t.Fatalf("should answer from the routed data: %v", err)
	}
	if !strings.Contains(out.Reply, "Ganga View Residency (from ₹2,499 a night + GST)") || len(out.Hotels) != 1 || out.Intent != IntentHotelSearch {
		t.Fatalf("fallback: %+v", out)
	}
	// Empty text after the meta line is stripped, too.
	f = newFixture()
	f.ai.reply = `<meta intent="hotel_search" needs_info="false" needs_login="false"/>`
	if out, err = f.svc.HandleMessage(context.Background(), Input{Message: "Which hotels in Haridwar are quiet?"}); err != nil || out.Reply == "" {
		t.Fatalf("meta-only reply: %v %+v", err, out)
	}
	// Without verified data the failure is still reported.
	f = newFixture()
	f.ai.err = ErrProviderEmpty
	if _, err = f.svc.HandleMessage(context.Background(), Input{Message: "hotel tips please"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestPolicyQuestionsAreAnsweredFromTheData(t *testing.T) {
	f := newFixture() // Ganga View Residency: free cancellation yes, pay at hotel no
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "which of these have free cancellation?",
		Context: SearchContext{Destination: "Haridwar"}})
	if f.ai.lastReq.System != "" || !strings.Contains(out.Reply, "With free cancellation: Ganga View Residency.") {
		t.Fatalf("free cancellation: %q", out.Reply)
	}
	f = newFixture()
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "Can I pay at the hotel in Ganga View Residency?"})
	if !strings.Contains(out.Reply, "None of these offer pay at hotel: Ganga View Residency.") || len(out.Hotels) != 1 {
		t.Fatalf("pay at hotel: %q", out.Reply)
	}
}

func TestAmenityQuestionsAreAnsweredFromTheData(t *testing.T) {
	f := newFixture() // Ganga View Residency amenities: WiFi, Parking
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Does Ganga View Residency have parking?"})
	if f.ai.lastReq.System != "" || out.Reply != "Ganga View Residency lists parking among its amenities." {
		t.Fatalf("parking: %q", out.Reply)
	}
	f = newFixture()
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "Is there a pool at Ganga View Residency?"})
	if !strings.Contains(out.Reply, "A pool isn't listed for Ganga View Residency") {
		t.Fatalf("pool: %q", out.Reply)
	}
	// Other questions about a named hotel still go to the model.
	f = newFixture()
	f.svc.HandleMessage(context.Background(), Input{Message: "Tell me about Ganga View Residency"})
	if f.ai.lastReq.System == "" {
		t.Fatal("general hotel question should reach the model")
	}
}

func TestToolsUsedOnlyInDebug(t *testing.T) {
	f := newFixture()
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Which hotels in Haridwar are quiet?"})
	if out.ToolsUsed != nil {
		t.Fatal("tools_used in a non-debug response")
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "tools_used") {
		t.Fatal("tools_used serialised outside debug")
	}

	f = newFixture()
	f.svc.SetDebug(true)
	f.ai.calls = []ToolCall{{Name: toolSearchHotels, Input: json.RawMessage(`{"city":"Haridwar","min_price":300}`)}}
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "Which hotels in Haridwar are quiet?"})
	if len(out.ToolsUsed) != 2 {
		t.Fatalf("tools_used: %+v", out.ToolsUsed)
	}
	routed, model := out.ToolsUsed[0], out.ToolsUsed[1]
	if routed.Source != "router" || routed.Name != toolSearchHotels || routed.Results != 1 {
		t.Fatalf("routed use: %+v", routed)
	}
	if model.Source != "model" || strings.Contains(string(model.Args), "min_price") || model.Results != 1 {
		t.Fatalf("model use should show the sanitised args: %+v %s", model, model.Args)
	}
}

func TestContextIsValidated(t *testing.T) {
	c := normalizeContext(SearchContext{Destination: "Agra'; DROP TABLE x", CheckIn: "tomorrow", CheckOut: "2026-12-03", Guests: 900, Rooms: 2, BudgetMax: -5})
	if c.Destination != "" || c.CheckIn != "" || c.CheckOut != "2026-12-03" || c.Guests != 0 || c.Rooms != 2 || c.BudgetMax != 0 {
		t.Fatalf("context not cleaned: %+v", c)
	}
}

func TestRupeeFormatting(t *testing.T) {
	for in, want := range map[float64]string{0: "0", 999: "999", 5248: "5,248", 123456: "1,23,456", 12345678.4: "1,23,45,678"} {
		if got := inr(in); got != want {
			t.Errorf("inr(%v) = %q, want %q", in, got, want)
		}
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
