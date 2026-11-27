package chatbot

import (
	"context"
	"testing"
)

// lastSearch is the last hotel filter the router used for a city search
// (the catalogue load, which has no city or search term, is skipped).
func lastSearch(f *fixture) (city string, min, max float64, ok bool) {
	for i := len(f.hotels.filters) - 1; i >= 0; i-- {
		h := f.hotels.filters[i]
		if h.City != "" || h.Search != "" {
			return h.City + h.Search, h.MinPrice, h.MaxPrice, true
		}
	}
	return "", 0, 0, false
}

func TestDestinationSearchReturnsSummary(t *testing.T) {
	f := newFixture()
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "Show me hotels in Haridwar under 3000"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Search == nil || *out.Search != (SearchSummary{Destination: "Haridwar", MaxPrice: 3000, TotalMatches: 1}) {
		t.Fatalf("search summary: %+v", out.Search)
	}
}

// The website's guided search sends the chosen destination as context with a
// plain request; it gets the very same search and summary.
func TestGuidedDestinationUsesTheSameSearch(t *testing.T) {
	f := newFixture()
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "Show me hotels",
		Context: SearchContext{Destination: "Haridwar", BudgetMin: 2000, BudgetMax: 3000}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Reply != "Sure! Here are some hotels available in Haridwar under ₹3,000." || len(out.Hotels) != 1 || f.ai.lastReq.System != "" {
		t.Fatalf("guided search: %+v", out)
	}
	if city, min, max, ok := lastSearch(f); !ok || city != "Haridwar" || min != 2000 || max != 3000 {
		t.Fatalf("filter: %q %v %v", city, min, max)
	}
	if out.Search == nil || out.Search.MinPrice != 2000 || out.Search.MaxPrice != 3000 {
		t.Fatalf("summary: %+v", out.Search)
	}
}

func TestNoSummaryWithoutResults(t *testing.T) {
	f := newFixture()
	f.hotels.list = nil // nothing listed: the search runs and finds nothing
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Show me hotels", Context: SearchContext{Destination: "Haridwar"}})
	if out.Search != nil || len(out.Hotels) != 0 || out.Reply != "I couldn't find any hotels in Haridwar right now. Please try another destination or budget." {
		t.Fatalf("empty search: %+v", out)
	}
	other, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Customer support"})
	if other.Search != nil {
		t.Fatalf("summary on a non-search reply: %+v", other.Search)
	}
}

func TestBookingCardCarriesPaymentState(t *testing.T) {
	f := newFixture()
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "status of booking 100", UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Bookings) != 1 || out.Bookings[0].BookingID != 100 || out.Bookings[0].Payment != "paid" {
		t.Fatalf("booking card: %+v", out.Bookings)
	}
	// Another guest's booking: neutral not-found, and no card.
	other, _ := f.svc.HandleMessage(context.Background(), Input{Message: "status of booking 200", UserID: 7})
	if len(other.Bookings) != 0 {
		t.Fatalf("another guest's booking leaked: %+v", other.Bookings)
	}
}
