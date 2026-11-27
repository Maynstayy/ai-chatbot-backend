package chatbot

import (
	"context"
	"strings"
	"testing"
)

func TestThirdPartyPlatformReplies(t *testing.T) {
	manageAgoda := "I can only help with bookings made on SKYMCM, so I can't view or manage reservations made through Agoda."
	cases := map[string]string{
		// How to search or book there: the standard SKYMCM answer.
		"How can I book through Agoda?":                thirdPartyBookingReply,
		"How can I book through Booking.com?":          thirdPartyBookingReply,
		"How does Booking.com work?":                   thirdPartyBookingReply,
		"Can you help me with MakeMyTrip?":             thirdPartyBookingReply,
		"How can I reserve a room through MakeMyTrip?": thirdPartyBookingReply,
		"Can you help me book through Expedia?":        thirdPartyBookingReply,
		"How can I book a hotel on Agoda?":             thirdPartyBookingReply,
		"Airbnb pe booking kaise kare":                 thirdPartyBookingReply,
		"Can I book this on another booking website?":  thirdPartyBookingReply,
		// Managing a reservation made there: contact that platform.
		"How do I cancel my Agoda booking?":                manageAgoda,
		"Agoda booking cancel karni hai":                   manageAgoda,
		"I booked through Expedia.":                        "reservations made through Expedia.",
		"When will I get my Goibibo refund?":               "please contact Goibibo directly",
		"What payment methods does Booking.com accept?":    "reservations made through Booking.com.",
		"I booked through another app, can you cancel it?": "please contact the platform where it was booked directly",
		// Connection or comparison: not connected.
		"Do you work with Agoda?":          "not connected to other booking sites, including Agoda",
		"Is SKYMCM partnered with OYO?":    "not connected to other booking sites, including OYO",
		"Are you cheaper than MakeMyTrip?": "independent hotel booking platform",
	}
	for msg, want := range cases {
		for _, userID := range []int64{0, 7} {
			f := newFixture()
			out, err := f.svc.HandleMessage(context.Background(), Input{Message: msg, UserID: userID})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.Reply, want) || out.Intent != IntentThirdParty {
				t.Errorf("%q (user %d): got %q / %s, want %q", msg, userID, out.Reply, out.Intent, want)
			}
			// Answered without the model, a hotel search or a booking lookup,
			// and never asks the guest to sign in for someone else's booking.
			if f.ai.lastReq.System != "" || len(f.hotels.filters) != 0 || len(f.bookings.statuses) != 0 || out.RequiresLogin || len(out.Bookings) != 0 {
				t.Errorf("%q (user %d) triggered the model, a search or a booking lookup: %+v", msg, userID, out)
			}
			if devanagari.MatchString(out.Reply) || looksHinglish(out.Reply) {
				t.Errorf("%q: reply not English: %q", msg, out.Reply)
			}
		}
	}
}

func TestThirdPartyRepliesNeverClaimAConnection(t *testing.T) {
	for _, msg := range []string{"How can I book through Agoda?", "How do I cancel my Agoda booking?", "Do you work with Agoda?", "I booked through Expedia."} {
		r := strings.ToLower(thirdPartyReply(msg))
		for _, claim := range []string{"we are partnered", "integrated with", "i can cancel", "i have cancelled", "i can see your", "refund has been", "log in to agoda", "go to agoda.com", "open the app"} {
			if strings.Contains(r, claim) {
				t.Errorf("%q: reply claims %q: %s", msg, claim, r)
			}
		}
	}
}

// SKYMCM's own requests are untouched: real hotel cards, booking help, and
// ordinary words that only look like a platform name.
func TestSKYMCMRequestsAreNotThirdParty(t *testing.T) {
	for _, msg := range []string{"I want to book a hotel on SKYMCM", "Show me hotels in Haridwar", "Kashi yatra ke liye hotel chahiye",
		"How do I cancel my booking?", "book a trip to Haridwar", "status of booking 100"} {
		if thirdPartyReply(msg) != "" {
			t.Errorf("%q treated as another platform", msg)
		}
	}
	f := newFixture()
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Show me hotels in Haridwar"})
	if out.Intent != IntentHotelSearch || len(out.Hotels) == 0 {
		t.Fatalf("hotel search broken: %+v", out)
	}
	booking, _ := f.svc.HandleMessage(context.Background(), Input{Message: "status of booking 100", UserID: 7})
	if !strings.Contains(booking.Reply, "Booking #100") {
		t.Fatalf("SKYMCM booking lookup broken: %q", booking.Reply)
	}
}

// "I want to book a hotel on SKYMCM" asks for a city from the real listing,
// without the model (which once suggested a city SKYMCM doesn't list).
func TestBookOnSKYMCMAsksForAListedCity(t *testing.T) {
	f := newFixture()
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: "I want to book a hotel on SKYMCM"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Reply != "Sure! Which city would you like to stay in? We currently have hotels in Haridwar." || f.ai.lastReq.System != "" {
		t.Fatalf("got %q (model called: %v)", out.Reply, f.ai.lastReq.System != "")
	}
}

// Consecutive different questions get different, fitting answers.
func TestThirdPartyThenNewQuestion(t *testing.T) {
	f := newFixture()
	first, _ := f.svc.HandleMessage(context.Background(), Input{Message: "How can I book through Agoda?"})
	second, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Show me hotels in Haridwar", ConversationID: first.ConversationID})
	third, _ := f.svc.HandleMessage(context.Background(), Input{Message: "How do I cancel my Agoda booking?", ConversationID: first.ConversationID})
	if second.Intent != IntentHotelSearch || len(second.Hotels) == 0 || second.Reply == first.Reply {
		t.Fatalf("search after third-party question: %+v", second)
	}
	if third.Reply == first.Reply || !strings.Contains(third.Reply, "contact Agoda directly") {
		t.Fatalf("cancel question got the generic answer: %q", third.Reply)
	}
}
