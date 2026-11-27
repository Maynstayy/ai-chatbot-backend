package chatbot

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/Maynstayy/ai-chatbot-backend/internal/landmarks"
)

type fakeLandmarks struct {
	byCity map[string][]landmarks.Landmark
	calls  int
}

func (f *fakeLandmarks) ListByCity(_ context.Context, city string) ([]landmarks.Landmark, error) {
	f.calls++
	return f.byCity[city], nil
}

func floatPtr(v float64) *float64 { return &v }

func TestUnsupportedServicesAreTurnedDownWithoutQuestions(t *testing.T) {
	for _, msg := range []string{"I want to book a car", "book a cab from the airport", "Book a flight to Haridwar",
		"train tickets to Haridwar", "car rental in Haridwar", "airport transfer please", "Haridwar tour package"} {
		f := newFixture()
		out, _ := f.svc.HandleMessage(context.Background(), Input{Message: msg})
		if out.Reply != unsupportedReply || f.ai.lastReq.System != "" || len(f.hotels.filters) != 0 || out.RequiresMoreInformation {
			t.Errorf("%q: %q", msg, out.Reply)
		}
	}
	// A hotel facility question isn't a request for the service.
	if isUnsupportedService("Does the hotel provide airport pickup?") {
		t.Fatal("hotel facility question treated as an unsupported service")
	}
}

func TestSiteServicesAreAnsweredFromTheSite(t *testing.T) {
	for msg, want := range map[string]string{
		"Do you work with Agoda?":                 "not connected to other booking sites",
		"How can I list my property on SKYMCM?":   "Become a Vendor",
		"Do you have resorts?":                    "Resorts page",
		"good restaurants near my stay":           "Shop & Dine",
		"What is free cancellation?":              "cancel within the specified window to get a full refund",
		"What is the weather?":                    offTopicReply,
		"Best time to visit Haridwar?":            offTopicReply,
		"Plan a 3 day trip for me":                offTopicReply,
		"What is Python?":                         offTopicReply,
		"How far is the railway station?":         "Which hotel do you mean?",
		"Does this hotel have free parking?":      "Which hotel do you mean? Tell me its name, or search a city",
		"What are your room prices?":              "Starting prices per room per night, before GST: Haridwar from ₹2,499",
		"What are room prices in Haridwar?":       "Hotels in Haridwar start from ₹2,499 per room per night, before GST",
		"How much is Ganga View Residency?":       "Ganga View Residency from ₹2,499",
		"Is Ganga View Residency pet friendly?":   "", // goes to the model with the hotel's details
		"Where is Ganga View Residency located?":  "Ganga View Residency is at",
		"Does Ganga View Residency have parking?": "lists parking among its amenities",
	} {
		f := newFixture()
		f.hotels.byID[11].Address = "Har Ki Pauri Road, Haridwar"
		f.ai.reply = "From the model."
		out, err := f.svc.HandleMessage(context.Background(), Input{Message: msg})
		if err != nil {
			t.Fatal(err)
		}
		if want == "" {
			if f.ai.lastReq.System == "" {
				t.Errorf("%q should reach the model", msg)
			}
			continue
		}
		if !strings.Contains(out.Reply, want) || f.ai.lastReq.System != "" {
			t.Errorf("%q: got %q", msg, out.Reply)
		}
	}
}

func TestResortQuestionsNeverSearch(t *testing.T) {
	f := newFixture()
	f.svc.HandleMessage(context.Background(), Input{Message: "show me villas in Goa"})
	if len(f.hotels.filters) != 0 || f.ai.lastReq.System != "" {
		t.Fatal("resort question triggered a search or the model")
	}
}

func TestNearbyUsesVerifiedLandmarksOnly(t *testing.T) {
	f := newFixture()
	h := f.hotels.byID[11]
	h.Address = "Har Ki Pauri Road, Haridwar"
	h.Latitude, h.Longitude = floatPtr(29.9560), floatPtr(78.1700)
	lm := &fakeLandmarks{byCity: map[string][]landmarks.Landmark{"Haridwar": {
		{Name: "Har Ki Pauri", Category: "ghat", Latitude: 29.9563, Longitude: 78.1712, IsActive: true},
		{Name: "Haridwar Junction", Category: "railway station", Latitude: 29.9476, Longitude: 78.1577, IsActive: true},
		{Name: "Closed Museum", Latitude: 29.9500, Longitude: 78.1600, IsActive: false},
		{Name: "Far Away Fort", Latitude: 30.9000, Longitude: 78.1700, IsActive: true},
	}}}
	f.svc.SetLandmarks(lm)

	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "What is near Ganga View Residency?"})
	if !strings.Contains(out.Reply, "Near Ganga View Residency (straight-line distance): Har Ki Pauri about 100 m, Haridwar Junction about 1.5 km") ||
		strings.Contains(out.Reply, "Closed Museum") || strings.Contains(out.Reply, "Far Away Fort") || f.ai.lastReq.System != "" {
		t.Fatalf("nearby: %q", out.Reply)
	}
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "How far is Ganga View Residency from the railway station?"})
	if !strings.Contains(out.Reply, "Haridwar Junction about 1.5 km") || strings.Contains(out.Reply, "Har Ki Pauri about") {
		t.Fatalf("station: %q", out.Reply)
	}
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "How far is Ganga View Residency from the airport?"})
	if !strings.Contains(out.Reply, "No airport is listed near Ganga View Residency, so I can't verify that distance") {
		t.Fatalf("airport: %q", out.Reply)
	}

	// No coordinates: say so, give the address, never a made-up distance.
	h.Latitude = nil
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "How far is Ganga View Residency from the railway station?"})
	if !strings.Contains(out.Reply, "I can't verify distances for Ganga View Residency yet. Its address is Har Ki Pauri Road, Haridwar") {
		t.Fatalf("no coordinates: %q", out.Reply)
	}
}

func TestModelTextIsSanitised(t *testing.T) {
	cases := map[string]string{
		"<suggestion>\nPlease tell me your destination and dates.\n</suggestion>":                 "Please tell me your destination and dates.",
		"<think>call search_hotels</think>Here is what I found.":                                  "Here is what I found.",
		"<meta intent=\"general\"/>\nHappy to help with your stay.":                               "Happy to help with your stay.",
		"Sure! {\"hotel_id\": 11, \"name\": \"Ganga View Residency\"}":                            "Sure!",
		"I checked the database using search_hotels.":                                             safeFallback(),
		"I am Qwen running on Ollama.":                                                            safeFallback(),
		"आपका स्वागत है":                                                                          safeFallback(),
		"<tool_call>{\"name\":\"get_hotel_details\",\"arguments\":{\"hotel_id\":11}}</tool_call>": safeFallback(),
	}
	for reply, want := range cases {
		f := newFixture()
		f.ai.reply = reply
		out, err := f.svc.HandleMessage(context.Background(), Input{Message: "tell me something about hotel rules"})
		if err != nil {
			t.Fatalf("%q: %v", reply, err)
		}
		if out.Reply != want {
			t.Errorf("%q: got %q, want %q", reply, out.Reply, want)
		}
	}
	// After a lookup, an unsafe reply is replaced by one written from the data.
	f := newFixture()
	f.ai.reply = "According to the API, Ganga View Residency is available."
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Which hotels in Haridwar are quiet?"})
	if !strings.Contains(out.Reply, "Ganga View Residency (from ₹2,499 a night + GST)") {
		t.Fatalf("data fallback: %q", out.Reply)
	}
}

func TestRepliesAreAlwaysEnglish(t *testing.T) {
	msgs := []string{
		"Hello", "Namaste", "Chutiya ho kya", "Abe pagal ho kya", "mujhe car book karni hai", "What is Python?",
		"मुझे होटल चाहिए", "आज मौसम कैसा है", "Haridwar mein hotel chahiye", "Mujhe Haridwar mein hotel dikhao",
		"Haridwar ke liye booking karni hai", "refund kab milega", "kitne ka room hai", "Customer support se baat karni hai",
		"Mujhe customer support chahiye", "meri bookings dikhao", "booking 100 ka status kya hai", "View my bookings",
		"What is free cancellation?", "Do you work with Agoda?", "thanks", "bye", "Ganga View Residency mein parking hai?",
		"What are your room prices?",
	}
	for _, modelReply := range []string{"आपका स्वागत है", "<suggestion>Kaunsa sheher?</suggestion>", "Haridwar mein ek hotel mila."} {
		for _, lang := range []string{"auto", "english", "hinglish", "hindi"} {
			for _, userID := range []int64{0, 7} {
				for _, msg := range msgs {
					f := newFixture()
					f.ai.reply = modelReply
					out, err := f.svc.HandleMessage(context.Background(), Input{Message: msg, Language: lang, UserID: userID})
					if err != nil {
						continue // validation or provider errors carry no reply
					}
					if devanagari.MatchString(out.Reply) || strings.Contains(out.Reply, "<") || anyHinglishWord(out.Reply) {
						t.Fatalf("lang %s, msg %q, model %q: reply %q", lang, msg, modelReply, out.Reply)
					}
				}
			}
		}
	}
	// Hindi or Hinglish in, English out.
	for msg, want := range map[string]string{
		"Namaste":                            greetingReply,
		"mujhe car book karni hai":           unsupportedReply,
		"Mujhe Haridwar mein hotel dikhao":   "Sure! Here are some hotels available in Haridwar.",
		"Haridwar ke liye booking karni hai": "Sure! Here are some hotels available in Haridwar.",
		"Customer support se baat karni hai": "Sure! Our customer support team is here to help you.",
	} {
		f := newFixture()
		out, _ := f.svc.HandleMessage(context.Background(), Input{Message: msg, Language: "hinglish"})
		if !strings.HasPrefix(out.Reply, want) {
			t.Errorf("%q: %q", msg, out.Reply)
		}
	}
	f := newFixture()
	f.svc.HandleMessage(context.Background(), Input{Message: "Haridwar ke hotels mein kaunsa sabse accha hai?"})
	if !strings.Contains(f.ai.lastReq.System, "Always reply in clear, simple English") || !strings.Contains(f.ai.lastReq.System, "Reply language: English only.") {
		t.Fatal("English-only rule not in the prompt")
	}
}

// anyHinglishWord flags even one common Hindi word written in Roman script.
func anyHinglishWord(s string) bool {
	return regexp.MustCompile(`(?i)\b(aap|aapka|aapki|aapke|hai|hain|nahi|kijiye|kripya|mein|ke liye|chahiye|abhi|sakta|karun|dhoondh\w*|swagat)\b`).MatchString(s)
}
