package chatbot

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func channelKinds(list []SupportChannel) string {
	var kinds []string
	for _, c := range list {
		kinds = append(kinds, c.Kind)
	}
	return strings.Join(kinds, ",")
}

func TestHindiAndHinglishSupportRequestsGetEnglishContacts(t *testing.T) {
	for _, msg := range []string{"mujhe madad chahiye", "madad karo", "help chahiye", "help karo", "sahayata chahiye",
		"customer care se baat karni hai", "complaint karni hai", "मुझे मदद चाहिए", "कस्टमर केयर से बात करनी है",
		"संपर्क नंबर दीजिए", "I need help", "help"} {
		f := newFixture()
		f.svc.SetSiteURL("https://skymcm.com")
		out, err := f.svc.HandleMessage(context.Background(), Input{Message: msg})
		if err != nil {
			t.Fatal(err)
		}
		if out.Intent != IntentSupport || !strings.HasPrefix(out.Reply, "Sure! Our customer support team is here to help you.") {
			t.Errorf("%q: not a support reply: %+v", msg, out)
		}
		if devanagari.MatchString(out.Reply) || looksHinglish(out.Reply) {
			t.Errorf("%q: reply is not English: %q", msg, out.Reply)
		}
		if got := channelKinds(out.Support); got != "phone,email,contact" {
			t.Errorf("%q: channels %q", msg, got)
		}
		if f.ai.lastReq.System != "" || len(f.hotels.filters) != 0 {
			t.Errorf("%q triggered the model or a hotel search", msg)
		}
	}
}

func TestSupportChannelsAreClickableAndVerified(t *testing.T) {
	f := newFixture()
	f.svc.SetSiteURL("https://skymcm.com/")
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Customer support"})
	want := []SupportChannel{
		{Kind: "phone", Label: "Call Us", Href: "tel:+917607956595", Value: "+91 7607956595", Note: supportPhoneHours},
		{Kind: "email", Label: "Email Support", Href: "mailto:skymcm17@gmail.com", Value: "skymcm17@gmail.com", Note: supportEmailReply},
		{Kind: "contact", Label: "Contact Us", Href: "https://skymcm.com/contact"},
	}
	if len(out.Support) != len(want) {
		t.Fatalf("channels: %+v", out.Support)
	}
	for i := range want {
		if out.Support[i] != want[i] {
			t.Errorf("channel %d = %+v, want %+v", i, out.Support[i], want[i])
		}
	}

	// Other replies carry no support channels.
	other, _ := f.svc.HandleMessage(context.Background(), Input{Message: "Show hotels in Haridwar"})
	if other.Support == nil || len(other.Support) != 0 {
		t.Errorf("non-support reply has channels: %+v", other.Support)
	}
}

func TestSupportConfiguration(t *testing.T) {
	f := newFixture()
	// A configured WhatsApp number is shown first, as a wa.me link.
	f.svc.SetSupport(SupportInfo{WhatsApp: "+91 98765 43210"})
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "whatsapp number"})
	if channelKinds(out.Support) != "whatsapp,phone,email,contact" || out.Support[0].Href != "https://wa.me/919876543210" {
		t.Fatalf("whatsapp channel: %+v", out.Support)
	}
	if !strings.Contains(out.Reply, "WhatsApp: +91 98765 43210") {
		t.Errorf("reply missing WhatsApp: %q", out.Reply)
	}

	// Invalid values never become links: WhatsApp is hidden, phone and email
	// fall back to the published details.
	f.svc.SetSupport(SupportInfo{Phone: "call me", Email: "not-an-email", WhatsApp: "12345"})
	out, _ = f.svc.HandleMessage(context.Background(), Input{Message: "Customer support"})
	if channelKinds(out.Support) != "phone,email,contact" || out.Support[0].Href != "tel:+917607956595" || out.Support[1].Href != "mailto:skymcm17@gmail.com" {
		t.Fatalf("invalid config leaked: %+v", out.Support)
	}
}

func TestSupportDoesNotCollideWithHotelSearch(t *testing.T) {
	for _, msg := range []string{"Show hotels in Haridwar", "I need help finding a hotel in Haridwar", "madad chahiye Haridwar mein hotel ke liye",
		"हरिद्वार में होटल के लिए मदद चाहिए"} {
		if isSupportRequest(msg) {
			t.Errorf("%q treated as support", msg)
		}
	}
	f := newFixture()
	out, _ := f.svc.HandleMessage(context.Background(), Input{Message: "I need help finding a hotel in Haridwar"})
	if out.Intent != IntentHotelSearch || len(out.Hotels) == 0 {
		t.Fatalf("search with 'need help' not searched: %+v", out)
	}
}

// The JSON the website reads, through the real handler: support channels on
// a Hindi support request, and the same conversation then answering a hotel
// search (no stale support reply) with an empty support list.
func TestHandlerSupportThenSearchJSON(t *testing.T) {
	f := newFixture()
	f.svc.SetSiteURL("https://skymcm.com")
	mux := newTestMux(f.svc, 20)

	type body struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			ConversationID string           `json:"conversation_id"`
			Intent         string           `json:"intent"`
			Hotels         []HotelCard      `json:"hotels"`
			Bookings       []BookingCard    `json:"bookings"`
			Support        []SupportChannel `json:"support"`
		} `json:"data"`
	}
	decode := func(rec *httptest.ResponseRecorder) body {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var b body
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		return b
	}

	first := decode(post(mux, `{"message":"मुझे मदद चाहिए"}`))
	if first.Data.Intent != IntentSupport || channelKinds(first.Data.Support) != "phone,email,contact" || devanagari.MatchString(first.Message) {
		t.Fatalf("support: %+v", first)
	}

	second := decode(post(mux, `{"message":"Show me hotels in Haridwar","conversation_id":"`+first.Data.ConversationID+`"}`))
	if second.Data.Intent != IntentHotelSearch || len(second.Data.Hotels) == 0 || second.Data.Support == nil || len(second.Data.Support) != 0 ||
		strings.Contains(second.Message, "customer support") || second.Data.ConversationID != first.Data.ConversationID {
		t.Fatalf("search after support: %+v", second)
	}
	if second.Data.Hotels[0].Name == "" || second.Data.Bookings == nil {
		t.Fatalf("card fields: %+v", second.Data)
	}
}

func TestShortDescription(t *testing.T) {
	if got := shortDescription("  A calm   stay\nby the Ganga.  "); got != "A calm stay by the Ganga." {
		t.Errorf("collapsed: %q", got)
	}
	long := strings.Repeat("riverside rooms with views ", 20)
	got := shortDescription(long)
	if len([]rune(got)) > cardDescriptionRunes+1 || !strings.HasSuffix(got, "…") || strings.HasSuffix(got, " …") {
		t.Errorf("truncated: %q", got)
	}
}

func TestMessageLogHasNoContent(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	f := newFixture()
	secret := "Show hotels in Haridwar my password is hunter2-secret"
	out, err := f.svc.HandleMessage(context.Background(), Input{Message: secret, UserID: 7, FullName: "Asha Verma"})
	if err != nil {
		t.Fatal(err)
	}
	log := buf.String()
	for _, want := range []string{"chatbot message", "route=router", "intent=hotel_search", "outcome=ok", "duration_ms=", "hotels=", "signed_in=true"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q: %s", want, log)
		}
	}
	for _, leaked := range []string{"hunter2", "password", "Haridwar", "Asha", out.ConversationID} {
		if strings.Contains(log, leaked) {
			t.Errorf("log leaked %q: %s", leaked, log)
		}
	}
}
