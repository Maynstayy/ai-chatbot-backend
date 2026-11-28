package chatbot

import (
	"context"
	"strings"
	"testing"

	"github.com/Maynstayy/ai-chatbot-backend/internal/landmarks"
)

func TestSupportRequestsGetContactsInstantly(t *testing.T) {
	for _, msg := range []string{"Customer support", "Talk to support", "Contact support", "I need help", "help",
		"Customer care", "I want to speak with your support team", "Support team se baat karni hai",
		"Mujhe customer support chahiye", "Contact number", "How can I contact SKYMCM?"} {
		for _, userID := range []int64{0, 7} {
			f := newFixture()
			f.svc.SetSiteURL("https://skymcm.com/")
			out, err := f.svc.HandleMessage(context.Background(), Input{Message: msg, UserID: userID})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Sure! Our customer support team is here to help you.", "+91 7607956595", "skymcm17@gmail.com", "https://skymcm.com/contact"} {
				if !strings.Contains(out.Reply, want) {
					t.Errorf("%q: missing %q in %q", msg, want, out.Reply)
				}
			}
			if strings.Contains(strings.ToLower(out.Reply), "whatsapp") || out.Intent != IntentSupport {
				t.Errorf("%q: unverified WhatsApp or wrong intent: %+v", msg, out)
			}
			if f.ai.lastReq.System != "" || len(f.hotels.filters) != 0 || len(f.bookings.statuses) != 0 || len(out.Hotels) != 0 {
				t.Errorf("%q triggered a search, booking lookup or the model", msg)
			}
		}
	}
	// "Show hotels in Haridwar" never triggers support; a booking question isn't support.
	if isSupportRequest("Show hotels in Haridwar") {
		t.Fatal("hotel search treated as support")
	}
	f := newFixture()
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "I need help with my booking 100", UserID: 7})
	if !strings.Contains(out.Reply, "Booking #100") {
		t.Fatalf("booking question routed to support: %q", out.Reply)
	}
}

func TestDestinationShowsHotelsInstantly(t *testing.T) {
	for _, msg := range []string{"Show me hotels in Haridwar.", "Haridwar hotels", "Haridwar", "Mujhe Haridwar mein hotel dikhao",
		"Haridwar ke liye booking karni hai", "I want to book a hotel in Haridwar", "I need accommodation in Haridwar",
		"Haridwar mein hotel chahiye", "हरिद्वार में होटल चाहिए"} {
		f := newFixture()
		out, err := f.svc.HandleMessage(context.Background(), Input{Message: msg})
		if err != nil {
			t.Fatal(err)
		}
		if out.Reply != "Sure! Here are some hotels available in Haridwar." || len(out.Hotels) != 1 || out.Hotels[0].HotelID != 11 ||
			out.RequiresMoreInformation || f.ai.lastReq.System != "" || out.Intent != IntentHotelSearch {
			t.Errorf("%q: %+v", msg, out)
		}
	}
	// A budget the guest gives is used as a filter.
	f := newFixture()
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "hotels in Haridwar under 3000"})
	if out.Reply != "Sure! Here are some hotels available in Haridwar under ₹3,000." || f.hotels.filters[0].MaxPrice != 3000 {
		t.Fatalf("budget: %q %+v", out.Reply, f.hotels.filters)
	}
	// No results: said plainly.
	f = newFixture()
	f.hotels.noResults = true
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "hotels in Haridwar"})
	if out.Reply != "I couldn't find any hotels in Haridwar right now. Please try another destination or budget." || len(out.Hotels) != 0 {
		t.Fatalf("no results: %q", out.Reply)
	}
	// A real question about the hotels still goes to the model, with the data.
	f = newFixture()
	f.ai.reply = "Ganga View Residency is close to the river."
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "Which hotel in Haridwar is closest to the river?"})
	if f.ai.lastReq.System == "" || out.Reply != "Ganga View Residency is close to the river." {
		t.Fatalf("question: %+v", out)
	}
}

func TestLandmarksAndUnlistedPlaces(t *testing.T) {
	f := newFixture()
	f.svc.SetLandmarks(&fakeLandmarks{byCity: map[string][]landmarks.Landmark{"Haridwar": {
		{Name: "Har Ki Pauri", Latitude: 29.95, Longitude: 78.17, IsActive: true},
	}}})
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Hotels near Har Ki Pauri"})
	if out.Reply != "Har Ki Pauri is in Haridwar. Here are some hotels available in Haridwar." || len(out.Hotels) != 1 {
		t.Fatalf("landmark: %q", out.Reply)
	}
	for msg, place := range map[string]string{"Show hotels in Jaipur": "Jaipur", "I want to book a hotel in Atlantis": "Atlantis"} {
		f = newFixture()
		out, _ = f.svc.HandleMessage(context.Background(), Input{Message: msg})
		if out.Reply != "No hotels are currently listed in "+place+" on SKYMCM. We currently have hotels in Haridwar." ||
			f.ai.lastReq.System != "" || len(f.hotels.filters) != 0 {
			t.Errorf("%q: %q", msg, out.Reply)
		}
	}
	f = newFixture()
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "मुझे होटल चाहिए"})
	if out.Reply != "Sure! Which city would you like to stay in? We currently have hotels in Haridwar." {
		t.Fatalf("devanagari without a city: %q", out.Reply)
	}
}

func TestHinglishModelTextIsReplaced(t *testing.T) {
	if !looksHinglish("Haridwar mein ek hotel mila.") || looksHinglish("Shree Hare Rama Hare Krishna Guest House has parking.") {
		t.Fatal("Hinglish detection")
	}
	f := newFixture()
	f.ai.reply = "Aapke liye Haridwar mein ek hotel mila hai."
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Which hotel in Haridwar is quiet?"})
	if out.Reply != "I found 1 hotel: Ganga View Residency (from ₹2,499 a night + GST). Open one and pick your dates to see live availability and prices." {
		t.Fatalf("hinglish model text: %q", out.Reply)
	}
}
