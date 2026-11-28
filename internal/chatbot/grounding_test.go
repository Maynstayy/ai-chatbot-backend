package chatbot

import (
	"context"
	"strings"
	"testing"

	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
)

// Regression tests for four reported chatbot bugs:
//  1. hotels, prices and ratings made up for a city SKYMCM doesn't list;
//  2. the previous city's hotel cards shown for a new search;
//  3. "under ₹1,000" showing a hotel at ₹1,000 or more, and the budget
//     follow-up losing the city;
//  4. "hotels that give a full refund on cancellation" answered with the
//     refund processing time instead of a search.
// Everything runs on local fixtures: no network, no production API.

// groundingCatalogue is a small local listing in the shape SKYMCM returns:
// two cities (one stored with a trailing space, as on the live site), a
// hotel priced exactly at ₹1,000, and a mix of free-cancellation flags and
// cancellation windows.
func groundingCatalogue() []hotels.Hotel {
	h := func(id int64, name, city string, price, rating float64, reviews int, free bool, hours *int) hotels.Hotel {
		return hotels.Hotel{HotelID: id, Name: name, City: city, State: "Uttar Pradesh", Country: "India",
			PricePerNight: price, Rating: rating, TotalReviews: reviews, IsActive: true,
			FreeCancellation: free, CancellationHours: hours}
	}
	return []hotels.Hotel{
		h(21, "Shashi Guest House", "Varanasi", 400, 4.1, 12, true, intPtr(24)),
		h(22, "Shivshakti Homestay", "Varanasi", 990, 3.9, 5, false, nil),
		h(23, "Ghat Edge Inn", "Varanasi", 1000, 4.3, 30, true, nil),
		h(24, "Kashi Heritage Hotel", "Varanasi", 2500, 4.6, 80, true, intPtr(48)),
		h(31, "Awadh Courtyard", "Lucknow ", 1800, 4.0, 9, false, nil),
	}
}

func newGroundingFixture() *fixture {
	f := newFixture()
	list := groundingCatalogue()
	f.hotels.list = list
	f.hotels.byID = map[int64]*hotels.Hotel{}
	for i := range list {
		f.hotels.byID[list[i].HotelID] = &list[i]
	}
	return f
}

func ask(t *testing.T, f *fixture, in Input) *Output {
	t.Helper()
	out, err := f.svc.HandleMessage(context.Background(), in)
	if err != nil {
		t.Fatalf("%q: %v", in.Message, err)
	}
	return out
}

func cardIDs(cards []HotelCard) []int64 {
	ids := make([]int64, len(cards))
	for i, c := range cards {
		ids[i] = c.HotelID
	}
	return ids
}

// --- Bug 1 and 2: unlisted cities, stale cards ------------------------------

func TestUnlistedCityAfterAnotherSearchShowsNoOldCards(t *testing.T) {
	f := newGroundingFixture()
	f.ai.reply = "Here are hotels in Mumbai: Sea Breeze Grand at ₹1,299, rated 4.5."
	// The website still sends the Varanasi search as context.
	out := ask(t, f, Input{Message: "Show me hotels in Mumbai", Context: SearchContext{
		Destination: "Varanasi", HotelIDs: []int64{21, 22, 23}}})
	want := "No hotels are currently listed in Mumbai on SKYMCM. We currently have hotels in Varanasi and Lucknow."
	if out.Reply != want {
		t.Fatalf("reply %q, want %q", out.Reply, want)
	}
	if len(out.Hotels) != 0 || modelCalled(f) || out.Search != nil {
		t.Fatalf("stale cards %v / model called %v / search %+v", cardIDs(out.Hotels), modelCalled(f), out.Search)
	}
}

func TestUnlistedCityPhrasingsInAFreshChat(t *testing.T) {
	for msg, place := range map[string]string{
		"Mumbai hotels":                       "Mumbai",
		"Find me Goa stays":                   "Goa",
		"Any hotels at Jaipur under 2000?":    "Jaipur",
		"best hotels in new delhi for family": "New Delhi",
	} {
		f := newGroundingFixture()
		out := ask(t, f, Input{Message: msg})
		if !strings.HasPrefix(out.Reply, "No hotels are currently listed in "+place+" on SKYMCM.") ||
			len(out.Hotels) != 0 || modelCalled(f) {
			t.Errorf("%q: %q (cards %v)", msg, out.Reply, cardIDs(out.Hotels))
		}
	}
}

func TestPlaceDetectionIgnoresNonPlacesAndListedCities(t *testing.T) {
	catalog := groundingCatalogue()
	for _, msg := range []string{
		"Show hotels in Varanasi", "hotels in varanasi under 1000", "Hotels in Lucknow", // listed
		"Shashi Guest House hotels", // a hotel's own name
		"Show rooms in this hotel", "hotels in December", "hotels in my budget",
		"Tell me about hotels", "Does this hotel have parking?", "I want to book a hotel",
		"hotels near the ghat", "hotels with free breakfast", "hotels for 2 guests",
	} {
		if got := unlistedPlaceIn(msg, catalog); got != "" {
			t.Errorf("%q: treated %q as an unlisted place", msg, got)
		}
	}
}

// --- Bug 1: the model's reply is checked against the listings --------------

func TestInventedHotelFactsAreReplaced(t *testing.T) {
	f := newGroundingFixture()
	run := &toolRun{}
	in := Input{Message: "Which area is good for a family?"}
	for _, reply := range []string{
		"Try Royal Ganges Palace Hotel, a lovely stay.",
		"Rooms start at ₹1,299 a night.",
		"It is rated 4.8 out of 5.",
		"Guests love it: 1,234 reviews so far.",
	} {
		if !f.svc.unsupportedClaim(context.Background(), reply, run, in) {
			t.Errorf("not caught: %q", reply)
		}
	}
	for _, reply := range []string{
		"Shashi Guest House starts at ₹400 a night.",
		"Kashi Heritage Hotel is rated 4.6 with 80 reviews.",
		"Godowlia and Assi Ghat are popular areas with families.",
	} {
		if f.svc.unsupportedClaim(context.Background(), reply, run, in) {
			t.Errorf("real listing data flagged: %q", reply)
		}
	}

	// Policy statements that contradict the listing.
	for reply, wrong := range map[string]bool{
		"Shashi Guest House is a good choice. It is not free cancellation.":    true,  // it is
		"Shivshakti Homestay offers free cancellation.":                        true,  // it doesn't
		"Kashi Heritage Hotel costs ₹2,500 per night, including GST.":          true,  // listed before GST
		"Kashi Heritage Hotel offers free cancellation up to 48 hours before.": false, // as listed
		"Shivshakti Homestay is not marked free cancellation.":                 false, // as listed
		"Free cancellation depends on the hotel; check the booking page.":      false, // no hotel named
	} {
		if got := f.svc.unsupportedClaim(context.Background(), reply, run, in); got != wrong {
			t.Errorf("%q: flagged %v, want %v", reply, got, wrong)
		}
	}

	// End to end: the invented reply never reaches the guest.
	f = newGroundingFixture()
	f.ai.reply = "I recommend the Royal Ganges Palace Hotel at ₹1,299, rated 4.8."
	out := ask(t, f, Input{Message: "tell me something about hotel rules"})
	if strings.Contains(out.Reply, "1,299") || strings.Contains(out.Reply, "Royal Ganges") || len(out.Hotels) != 0 {
		t.Fatalf("invented facts shown: %q", out.Reply)
	}
}

// --- Bug 3: strict budgets, kept across follow-ups --------------------------

func TestUnderBudgetIsStrict(t *testing.T) {
	f := newGroundingFixture()
	out := ask(t, f, Input{Message: "Show hotels in Varanasi under 1000"})
	if out.Reply != "Sure! Here are some hotels available in Varanasi under ₹1,000." {
		t.Fatalf("reply %q", out.Reply)
	}
	if ids := cardIDs(out.Hotels); len(ids) != 2 || ids[0] != 21 || ids[1] != 22 {
		t.Fatalf("cards %v, want [21 22] (₹1,000 itself excluded, cheapest first)", ids)
	}
	for _, h := range out.Hotels {
		if h.PricePerNight >= 1000 {
			t.Fatalf("%s at ₹%v is not under ₹1,000", h.Name, h.PricePerNight)
		}
	}
	if out.Search == nil || out.Search.Destination != "Varanasi" || out.Search.MaxPrice != 1000 {
		t.Fatalf("search summary %+v", out.Search)
	}
	if len(f.hotels.filters) != 1 || f.hotels.filters[0].City != "Varanasi" || f.hotels.filters[0].MaxPrice != 1000 {
		t.Fatalf("hotel search filters %+v", f.hotels.filters)
	}
}

func TestBudgetFollowUpKeepsTheCity(t *testing.T) {
	for _, msg := range []string{"under 1000", "below ₹1,000", "less than 1k?", "and under 1000"} {
		f := newGroundingFixture()
		out := ask(t, f, Input{Message: msg, Context: SearchContext{Destination: "Varanasi", HotelIDs: []int64{24, 23}}})
		ids := cardIDs(out.Hotels)
		if !strings.Contains(out.Reply, "Varanasi under ₹1,000") || len(ids) != 2 || ids[0] != 21 || ids[1] != 22 || modelCalled(f) {
			t.Errorf("%q: %q cards %v", msg, out.Reply, ids)
		}
	}

	// "up to" includes the amount.
	f := newGroundingFixture()
	out := ask(t, f, Input{Message: "up to 1000", Context: SearchContext{Destination: "Varanasi"}})
	if ids := cardIDs(out.Hotels); len(ids) != 3 || ids[2] != 23 || !strings.Contains(out.Reply, "up to ₹1,000") {
		t.Fatalf("up to: %q cards %v", out.Reply, ids)
	}

	// Nothing that cheap: say so, with the real lowest price, and no cards.
	f = newGroundingFixture()
	out = ask(t, f, Input{Message: "under 300", Context: SearchContext{Destination: "Varanasi"}})
	if out.Reply != "No hotels in Varanasi are listed under ₹300 right now. The lowest starting price there is ₹400 a night, before GST." || len(out.Hotels) != 0 {
		t.Fatalf("none: %q cards %v", out.Reply, cardIDs(out.Hotels))
	}

	// No city chosen yet (only guests): ask for one from the listed cities.
	f = newGroundingFixture()
	out = ask(t, f, Input{Message: "under 1000", Context: SearchContext{Guests: 2}})
	if out.Reply != "Which city should I look in? We currently have hotels in Varanasi and Lucknow." || len(out.Hotels) != 0 {
		t.Fatalf("no city: %q", out.Reply)
	}
}

func TestModelSearchCardsRespectAStrictBudget(t *testing.T) {
	f := newGroundingFixture()
	f.ai.calls = []ToolCall{{Name: "search_hotels", Input: []byte(`{"city":"Varanasi","max_price":1000}`)}}
	f.ai.reply = "Here are some options."
	out := ask(t, f, Input{Message: "which quiet hotels in Varanasi are under 1000 rupees and near the river?"})
	for _, h := range out.Hotels {
		if h.PricePerNight >= 1000 {
			t.Fatalf("%s at ₹%v shown for under ₹1,000 (reply %q)", h.Name, h.PricePerNight, out.Reply)
		}
	}
}

// --- Bug 4: refundable-hotel searches vs refund timing ----------------------

var refundSearches = []string{
	"Show me hotels that provide fully refund on booking cancel",
	"Show me hotels that provide full refund on cancellation",
}

func TestRefundableSearchWithoutACity(t *testing.T) {
	for _, msg := range refundSearches {
		f := newGroundingFixture()
		out := ask(t, f, Input{Message: msg})
		if strings.Contains(out.Reply, "business days") || modelCalled(f) {
			t.Fatalf("%q answered with the refund timeline: %q", msg, out.Reply)
		}
		if !strings.HasPrefix(out.Reply, "3 hotels listed on SKYMCM are marked free cancellation: Varanasi (3).") ||
			!strings.Contains(out.Reply, "a full refund isn't guaranteed") ||
			!strings.Contains(out.Reply, "Tell me a city") {
			t.Fatalf("%q: %q", msg, out.Reply)
		}
		if len(out.Hotels) != 3 {
			t.Fatalf("%q: cards %v", msg, cardIDs(out.Hotels))
		}
		for _, h := range out.Hotels {
			if !h.FreeCancellation {
				t.Fatalf("%q: %s isn't free cancellation", msg, h.Name)
			}
		}
	}
}

func TestRefundableSearchInACity(t *testing.T) {
	for _, msg := range refundSearches {
		for _, in := range []Input{
			{Message: msg + " in Varanasi"},
			{Message: msg, Context: SearchContext{Destination: "Varanasi", HotelIDs: []int64{22}}},
		} {
			f := newGroundingFixture()
			out := ask(t, f, in)
			if !strings.HasPrefix(out.Reply, "3 of the 4 hotels listed in Varanasi are marked free cancellation.") || modelCalled(f) {
				t.Fatalf("%q (context %q): %q", in.Message, in.Context.Destination, out.Reply)
			}
			ids := cardIDs(out.Hotels)
			if len(ids) != 3 || ids[0] != 24 || ids[1] != 23 || ids[2] != 21 {
				t.Fatalf("%q: cards %v, want [24 23 21]", in.Message, ids)
			}
			// Windows are only stated where the listing gives one.
			if !strings.Contains(out.Reply, "Kashi Heritage Hotel (up to 48 hours before check-in); Shashi Guest House (up to 24 hours before check-in).") ||
				strings.Contains(out.Reply, "Ghat Edge Inn (up to") {
				t.Fatalf("%q: windows wrong: %q", in.Message, out.Reply)
			}
			if out.Search == nil || out.Search.Destination != "Varanasi" {
				t.Fatalf("%q: search %+v", in.Message, out.Search)
			}
		}
	}
}

func TestRefundableSearchWhereNoneQualify(t *testing.T) {
	for _, msg := range refundSearches {
		f := newGroundingFixture()
		out := ask(t, f, Input{Message: msg + " in Lucknow"})
		if out.Reply != "The only hotel listed in Lucknow isn't marked free cancellation right now. Each hotel's cancellation terms are shown on its booking page before you pay." ||
			len(out.Hotels) != 0 {
			t.Fatalf("%q: %q cards %v", msg, out.Reply, cardIDs(out.Hotels))
		}
		// An unlisted city: no invented refundable hotels.
		f = newGroundingFixture()
		out = ask(t, f, Input{Message: msg + " in Mumbai"})
		if !strings.HasPrefix(out.Reply, "No hotels are currently listed in Mumbai on SKYMCM.") || len(out.Hotels) != 0 {
			t.Fatalf("%q in Mumbai: %q", msg, out.Reply)
		}
	}
}

func TestRefundTimingQuestionsKeepTheTimeline(t *testing.T) {
	const timeline = "Refunds generally take 5–7 business days for card payments and 2–5 business days for UPI or wallets. Bank timelines can vary."
	for _, msg := range []string{
		"How long does a refund take?",
		"When will I get my refund?",
		"How many days for refund after cancellation?",
	} {
		f := newGroundingFixture()
		out := ask(t, f, Input{Message: msg})
		if out.Reply != timeline || len(out.Hotels) != 0 {
			t.Errorf("%q: %q cards %v", msg, out.Reply, cardIDs(out.Hotels))
		}
		if isRefundableSearch(msg) {
			t.Errorf("%q classed as a refundable-hotel search", msg)
		}
	}
	for _, msg := range refundSearches {
		if !isRefundableSearch(msg) {
			t.Errorf("%q not classed as a refundable-hotel search", msg)
		}
	}
}
