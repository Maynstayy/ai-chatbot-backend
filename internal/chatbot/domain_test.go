package chatbot

import (
	"context"
	"testing"
)

func TestOffTopicMessagesGetTheRedirectWithoutAnyLookup(t *testing.T) {
	for _, msg := range []string{
		"What is the capital of India?", "Chutiya ho kya", "write a poem about the sea",
		"who won the cricket match yesterday", "what is 25 * 4", "tell me a joke", "explain quantum physics to me",
	} {
		for _, userID := range []int64{0, 7} {
			f := newFixture()
			out, err := f.svc.HandleMessage(context.Background(), Input{Message: msg, UserID: userID})
			if err != nil {
				t.Fatal(err)
			}
			// Hinglish messages get the same redirect in Hinglish.
			if out.Reply != offTopicReply || out.Intent != IntentGeneral {
				t.Errorf("%q: %q", msg, out.Reply)
			}
			if f.ai.lastReq.System != "" || len(f.hotels.filters) != 0 || len(f.bookings.lookups) != 0 ||
				len(f.bookings.statuses) != 0 || len(out.Hotels) != 0 || out.BookingsChecked || out.RequiresLogin {
				t.Errorf("%q triggered a lookup or the model: %+v", msg, out)
			}
		}
	}
}

func TestHotelAndTravelMessagesAreNotRedirected(t *testing.T) {
	for _, msg := range []string{
		"Do you have hotels in Haridwar?", "Does this hotel have free parking?", "What is the cancellation policy?",
		"What are the payment options?", "Is it safe to walk near the ghats at night?", "How far is the railway station?",
		"hotels near the capital of India", "Mujhe Haridwar mein kamra chahiye", "मुझे होटल चाहिए", "Haridwar",
		"Ganga View Residency", "1 to 3 December", "2 adults", "status of booking 100",
	} {
		if isOffTopic(msg, []string{"Haridwar", "Ganga View Residency"}, false) {
			t.Errorf("%q treated as off-topic", msg)
		}
	}
}

func TestFollowUpsInAConversationAreNotRedirected(t *testing.T) {
	for _, msg := range []string{"is it close to the ghats?", "what about the second one?", "and the other?"} {
		if isOffTopic(msg, nil, true) {
			t.Errorf("%q redirected mid-conversation", msg)
		}
	}
	// A clearly unrelated question is still redirected mid-conversation.
	if !isOffTopic("What is the capital of India?", nil, true) {
		t.Fatal("off-topic question allowed because of context")
	}
}

func TestGreetingsAndThanksGetNaturalReplies(t *testing.T) {
	for msg, want := range map[string]string{
		"Hello": greetingReply, "hi there": greetingReply, "Namaste": greetingReply, "Good morning!": greetingReply,
		"thank you": "You're welcome! Is there anything else I can help you with for your stay?",
		"bye":       "Goodbye, and have a wonderful trip! Come back any time you need help with hotels or bookings.",
	} {
		f := newFixture()
		out, err := f.svc.HandleMessage(context.Background(), Input{Message: msg})
		if err != nil {
			t.Fatal(err)
		}
		if out.Reply != want || f.ai.lastReq.System != "" || len(f.hotels.filters) != 0 {
			t.Errorf("%q: %q", msg, out.Reply)
		}
	}
	f := newFixture()
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Hello"})
	if out.Intent != IntentGreeting {
		t.Fatalf("intent %q", out.Intent)
	}
}

func TestWhichOfTheseUsesTheShownHotels(t *testing.T) {
	f := newFixture()
	in := Input{Message: "Which of these has free wifi?", Context: SearchContext{Destination: "Haridwar", HotelIDs: []int64{11, 11, -3, 12}}}
	out, err := f.svc.HandleMessage(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the shown, listed hotels (12 is inactive, -3 invalid); no new search.
	if len(f.hotels.filters) != 0 || len(out.Hotels) != 1 || out.Hotels[0].HotelID != 11 {
		t.Fatalf("hotels: %+v filters %+v", out.Hotels, f.hotels.filters)
	}
	if out.Reply != "Ganga View Residency lists Wi-Fi among its amenities." {
		t.Fatalf("reply: %q", out.Reply)
	}
	if c := normalizeContext(SearchContext{HotelIDs: []int64{1, 2, 3, 4, 5, 6, 7}}); len(c.HotelIDs) != maxContextHotels {
		t.Fatalf("hotel ids not capped: %v", c.HotelIDs)
	}
}

func TestOffTopicDoesNotReusePreviousBookingIntent(t *testing.T) {
	f := newFixture()
	first, _ := f.svc.HandleMessage(context.Background(), Input{Message: "View my bookings", UserID: 7})
	f.bookings.statuses = nil
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "What is the capital of India?", UserID: 7, ConversationID: first.ConversationID})
	if out.Reply != offTopicReply || out.BookingsChecked || len(out.Bookings) != 0 || len(f.bookings.statuses) != 0 || out.Intent != IntentGeneral {
		t.Fatalf("previous booking intent reused: %+v", out)
	}
	// And the next hotel question still works in the same conversation.
	f.ai.reply = "It has parking."
	next, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Does Ganga View Residency have a pool?", UserID: 7, ConversationID: first.ConversationID})
	if next.ConversationID != first.ConversationID || len(next.Hotels) != 1 {
		t.Fatalf("hotel question after a redirect: %+v", next)
	}
}
