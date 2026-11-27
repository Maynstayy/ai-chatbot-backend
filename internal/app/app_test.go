package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Maynstayy/ai-chatbot-backend/internal/catalog"
	"github.com/Maynstayy/ai-chatbot-backend/internal/config"
	"github.com/Maynstayy/ai-chatbot-backend/pkg/cache"
)

const frontendOrigin = "http://localhost:3000"

// fakeOllama answers /api/chat like a local Ollama server, recording what
// the backend sent so tests can check the conversation context.
type fakeOllama struct {
	mu       sync.Mutex
	requests []map[string]any
	reply    string
}

func (f *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/chat" {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	reply := f.reply
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{
		"message": map[string]any{"role": "assistant", "content": reply},
		"done":    true, "done_reason": "stop",
	})
}

func (f *fakeOllama) last() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

type testServer struct {
	*httptest.Server
	ai *fakeOllama
}

func newTestServer(t *testing.T, mutate func(*config.Config)) *testServer {
	t.Helper()
	ai := &fakeOllama{reply: "Our test stays in Varanasi include riverside rooms near the ghats.\n<meta intent=\"travel\" needs_info=\"false\" needs_login=\"false\"/>"}
	aiServer := httptest.NewServer(ai)
	t.Cleanup(aiServer.Close)

	cfg := &config.Config{
		AppEnv: config.EnvTest, AppPort: "0", CatalogSource: config.CatalogFile, CatalogFile: "../../testdata/catalog.json",
		CORSAllowedOrigins: []string{frontendOrigin},
		AIProvider:         "ollama", AIModel: "test-model", AIBaseURL: aiServer.URL,
		AITimeoutSeconds: 10, AIMaxTokens: 256, ChatbotRatePerMin: 20,
		SiteURL: "https://example.test", SupportPhone: "+91 0000000000", SupportEmail: "support@example.test",
		UploadsDir: t.TempDir(),
	}
	if mutate != nil {
		mutate(cfg)
	}
	store, err := catalog.LoadFile(cfg.CatalogFile)
	if err != nil {
		t.Fatal(err)
	}
	a := Build(cfg, Sources{Hotels: store.Hotels(), RoomTypes: store.RoomTypes(), Landmarks: store.Landmarks()}, cache.New(""))
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return &testServer{Server: srv, ai: ai}
}

func (s *testServer) do(t *testing.T, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, s.URL+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func decode[T any](t *testing.T, res *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

// messageEnvelope is exactly what the frontend reads (features/chat/api/assistant.ts).
type messageEnvelope struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		ConversationID          string            `json:"conversation_id"`
		Intent                  string            `json:"intent"`
		RequiresMoreInformation *bool             `json:"requires_more_information"`
		RequiresLogin           *bool             `json:"requires_login"`
		Hotels                  []map[string]any  `json:"hotels"`
		RoomTypes               []map[string]any  `json:"room_types"`
		Bookings                []json.RawMessage `json:"bookings"`
		Support                 []map[string]any  `json:"support"`
		Search                  *struct {
			Destination  string `json:"destination"`
			TotalMatches int    `json:"total_matches"`
		} `json:"search"`
	} `json:"data"`
}

func TestMessageContractAndConversationContext(t *testing.T) {
	s := newTestServer(t, nil)

	res := s.do(t, "POST", "/api/v1/chatbot/message", `{"message":"What is the best time to visit the ghats near my hotel?","conversation_id":""}`, map[string]string{"Origin": frontendOrigin})
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != frontendOrigin {
		t.Errorf("CORS origin = %q", got)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("chat replies must not be cached")
	}
	first := decode[messageEnvelope](t, res)
	if !first.Success || first.Message == "" || strings.Contains(first.Message, "<meta") {
		t.Fatalf("bad envelope: %+v", first)
	}
	if len(first.Data.ConversationID) != 32 {
		t.Fatalf("conversation id %q", first.Data.ConversationID)
	}
	if first.Data.RequiresLogin == nil || first.Data.RequiresMoreInformation == nil || first.Data.Hotels == nil || first.Data.RoomTypes == nil {
		t.Fatalf("frontend fields missing: %+v", first.Data)
	}

	// The second message continues the same conversation: the model gets
	// the earlier turns.
	s.ai.reply = "Mornings are calm and cool.\n<meta intent=\"travel\" needs_info=\"false\" needs_login=\"false\"/>"
	res = s.do(t, "POST", "/api/v1/chatbot/message", `{"message":"And in the evening?","conversation_id":"`+first.Data.ConversationID+`"}`, nil)
	second := decode[messageEnvelope](t, res)
	if second.Data.ConversationID != first.Data.ConversationID {
		t.Fatalf("conversation not continued: %q vs %q", second.Data.ConversationID, first.Data.ConversationID)
	}
	msgs, _ := s.ai.last()["messages"].([]any)
	var sawFirst bool
	for _, m := range msgs {
		if mm, _ := m.(map[string]any); strings.Contains(mm["content"].(string), "best time to visit the ghats") {
			sawFirst = true
		}
	}
	if !sawFirst {
		t.Fatalf("model did not receive the earlier turn: %v", msgs)
	}
}

func TestUnknownConversationIDStartsFresh(t *testing.T) {
	s := newTestServer(t, nil)
	made := "0123456789abcdef0123456789abcdef"
	res := s.do(t, "POST", "/api/v1/chatbot/message", `{"message":"Hello there, what can you do?","conversation_id":"`+made+`"}`, nil)
	env := decode[messageEnvelope](t, res)
	// A guest can't pick or take over a conversation id: an unknown one is
	// replaced by a fresh server id.
	if env.Data.ConversationID == made || len(env.Data.ConversationID) != 32 {
		t.Fatalf("guest-chosen id was accepted: %q", env.Data.ConversationID)
	}
}

func TestHotelSearchReturnsCatalogueCards(t *testing.T) {
	s := newTestServer(t, nil)
	res := s.do(t, "POST", "/api/v1/chatbot/message", `{"message":"Show me hotels in Varanasi"}`, nil)
	env := decode[messageEnvelope](t, res)
	if len(env.Data.Hotels) == 0 {
		t.Fatalf("no hotel cards: %+v", env)
	}
	for _, h := range env.Data.Hotels {
		if h["city"] != "Varanasi" || !strings.HasPrefix(h["name"].(string), "Test ") {
			t.Errorf("unexpected card %v", h)
		}
		for _, k := range []string{"hotel_id", "price_per_night", "tax_per_night", "rating", "total_reviews", "free_cancellation", "pay_at_property"} {
			if _, ok := h[k]; !ok {
				t.Errorf("card missing %s: %v", k, h)
			}
		}
	}
}

func TestBookingQuestionNeedsSignInAndNoBookingTools(t *testing.T) {
	s := newTestServer(t, nil)
	res := s.do(t, "POST", "/api/v1/chatbot/message", `{"message":"Show my bookings"}`, map[string]string{"Authorization": "Bearer anything"})
	env := decode[messageEnvelope](t, res)
	if env.Data.RequiresLogin == nil || !*env.Data.RequiresLogin || len(env.Data.Bookings) != 0 {
		t.Fatalf("guest booking question: %+v", env.Data)
	}
	if req := s.ai.last(); req != nil {
		if b, _ := json.Marshal(req["tools"]); bytes.Contains(b, []byte("list_my_bookings")) {
			t.Fatal("booking tools offered to a guest")
		}
	}
}

func TestInvalidMessages(t *testing.T) {
	s := newTestServer(t, nil)
	cases := []struct {
		name, body string
		status     int
	}{
		{"not json", `{nope`, http.StatusBadRequest},
		{"empty", `{"message":"   "}`, http.StatusBadRequest},
		{"too long", `{"message":"` + strings.Repeat("a", 1001) + `"}`, http.StatusBadRequest},
		{"body too large", `{"message":"` + strings.Repeat("a", 9000) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		res := s.do(t, "POST", "/api/v1/chatbot/message", c.body, nil)
		if res.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", c.name, res.StatusCode, c.status)
			continue
		}
		e := decode[map[string]string](t, res)
		if e["error"] == "" {
			t.Errorf("%s: no JSON error", c.name)
		}
	}
}

func TestRateLimit(t *testing.T) {
	s := newTestServer(t, func(c *config.Config) { c.ChatbotRatePerMin = 2 })
	var last *http.Response
	// From a local proxy, the client is the forwarded address. (A caller
	// with no usable address shares one bucket with 5x the limit.)
	visitor := map[string]string{"X-Real-IP": "198.51.100.23"}
	for i := 0; i < 3; i++ {
		last = s.do(t, "POST", "/api/v1/chatbot/message", `{"message":"hello"}`, visitor)
	}
	// Another visitor still gets through.
	if r := s.do(t, "POST", "/api/v1/chatbot/message", `{"message":"hello"}`, map[string]string{"X-Real-IP": "198.51.100.24"}); r.StatusCode != http.StatusOK {
		t.Fatalf("other visitor limited: %d", r.StatusCode)
	}
	if last.StatusCode != http.StatusTooManyRequests || last.Header.Get("Retry-After") != "60" {
		t.Fatalf("third message: %d %q", last.StatusCode, last.Header.Get("Retry-After"))
	}
}

func TestAIOffAnswers503(t *testing.T) {
	s := newTestServer(t, func(c *config.Config) { c.AIProvider = "off" })
	res := s.do(t, "POST", "/api/v1/chatbot/message", `{"message":"hello"}`, nil)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", res.StatusCode)
	}
}

func TestAIProviderFailureIsSanitized(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "dial tcp 10.0.0.9:11434: connection refused", http.StatusBadGateway)
	}))
	defer broken.Close()
	s := newTestServer(t, func(c *config.Config) { c.AIBaseURL = broken.URL })
	res := s.do(t, "POST", "/api/v1/chatbot/message", `{"message":"What is the best time to visit the ghats near my hotel?"}`, nil)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode < 500 || bytes.Contains(body, []byte("dial tcp")) || bytes.Contains(body, []byte("10.0.0.9")) {
		t.Fatalf("status %d body %s", res.StatusCode, body)
	}
}

func TestCORS(t *testing.T) {
	s := newTestServer(t, nil)
	pre := s.do(t, "OPTIONS", "/api/v1/chatbot/message", "", map[string]string{
		"Origin": frontendOrigin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type",
	})
	if pre.StatusCode != http.StatusNoContent || pre.Header.Get("Access-Control-Allow-Origin") != frontendOrigin ||
		!strings.Contains(pre.Header.Get("Access-Control-Allow-Headers"), "Content-Type") {
		t.Fatalf("allowed preflight: %d %v", pre.StatusCode, pre.Header)
	}
	evil := s.do(t, "OPTIONS", "/api/v1/chatbot/message", "", map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"})
	if evil.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unknown origin was allowed")
	}
}

// The fields the frontend's lib/catalog.ts PublicHotel and RoomType read.
func TestCatalogueContract(t *testing.T) {
	s := newTestServer(t, nil)
	res := s.do(t, "GET", "/api/v1/hotels", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("hotels %d", res.StatusCode)
	}
	list := decode[[]map[string]any](t, res)
	if len(list) != 6 {
		t.Fatalf("want 6 test hotels, got %d", len(list))
	}
	for _, k := range []string{"hotel_id", "name", "location", "description", "city", "state", "country", "address", "category",
		"price_per_night", "rating", "total_reviews", "amenities", "thumbnail_url", "images", "owner_id", "is_active",
		"free_cancellation", "pay_at_property", "instant_booking", "tax_rate", "check_in_time", "check_out_time"} {
		if _, ok := list[0][k]; !ok {
			t.Errorf("hotel missing %q", k)
		}
	}

	res = s.do(t, "GET", "/api/v1/room-types?hotel_id=101&check_in=2027-01-10&check_out=2027-01-12&guests=2", "", nil)
	types := decode[[]map[string]any](t, res)
	if len(types) != 2 {
		t.Fatalf("room types: %v", types)
	}
	for _, k := range []string{"room_type_id", "hotel_id", "hotel_name", "name", "capacity", "price_per_night", "description",
		"amenities", "images", "total_rooms", "available_count", "rate_plans", "rooms_needed", "fits_guests"} {
		if _, ok := types[0][k]; !ok {
			t.Errorf("room type missing %q", k)
		}
	}
	plan := types[0]["rate_plans"].([]any)[0].(map[string]any)
	for _, k := range []string{"rate_plan_id", "name", "price_per_night", "is_active", "free_cancellation", "pay_at_property", "inclusions"} {
		if _, ok := plan[k]; !ok {
			t.Errorf("rate plan missing %q", k)
		}
	}

	if r := s.do(t, "GET", "/api/v1/room-types?hotel_id=101&check_in=bad&check_out=2027-01-12", "", nil); r.StatusCode != http.StatusBadRequest {
		t.Errorf("bad date: %d", r.StatusCode)
	}
	if r := s.do(t, "GET", "/api/v1/hotels/999", "", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown hotel: %d", r.StatusCode)
	}
}

func TestHealthAndJSONFallback(t *testing.T) {
	s := newTestServer(t, nil)
	if r := s.do(t, "GET", "/healthz", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("healthz %d", r.StatusCode)
	}
	ready := decode[map[string]any](t, s.do(t, "GET", "/readyz", "", nil))
	if ready["status"] != "ok" || ready["ai"] != "configured" || ready["catalog"] != "file" {
		t.Fatalf("readyz %v", ready)
	}
	r := s.do(t, "GET", "/nope", "", nil)
	if r.StatusCode != http.StatusNotFound || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("404: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := s.do(t, "DELETE", "/api/v1/hotels", "", nil); r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("405: %d", r.StatusCode)
	}
	if s.do(t, "GET", "/healthz", "", nil).Header.Get("X-Request-ID") == "" {
		t.Fatal("no request id")
	}
}
