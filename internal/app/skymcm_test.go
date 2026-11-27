package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/config"
	"github.com/Maynstayy/ai-chatbot-backend/internal/skymcm"
	"github.com/Maynstayy/ai-chatbot-backend/pkg/cache"
)

// A stand-in for the SKYMCM website API, answering like MaynStay's handlers.
func skymcmStandIn(t *testing.T, down *atomic.Bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("unexpected request to the website API: %s %s", r.Method, r.URL.Path)
		}
		if down != nil && down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "dial tcp 10.9.9.9:3306: connection refused")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/hotels":
			fmt.Fprint(w, `[
 {"hotel_id":21,"name":"Shanti Ghat Residency","location":"Assi Ghat","description":"Rooms by the river","city":"Varanasi","state":"Uttar Pradesh","country":"India",
  "address":"4 Assi Road","category":"Heritage","price_per_night":2600,"rating":4.5,"total_reviews":88,"amenities":["Free WiFi","Breakfast"],
  "thumbnail_url":"/uploads/hotels/3f9a.jpg","images":["/uploads/hotels/3f9a.jpg"],"owner_id":5,"is_active":true,"is_deleted":false,
  "free_cancellation":true,"pay_at_property":true,"instant_booking":true,"gst_rate":0.05,"tax_per_night":130,"tax_rate":0,"check_in_time":"","check_out_time":""},
 {"hotel_id":22,"name":"Hidden Draft Hotel","city":"Varanasi","price_per_night":1000,"is_active":false,"is_deleted":false}
]`)
		case "/api/v1/hotels/21":
			fmt.Fprint(w, `{"hotel_id":21,"name":"Shanti Ghat Residency","city":"Varanasi","is_active":true,"price_per_night":2600,"tax_per_night":130,"thumbnail_url":"/uploads/hotels/3f9a.jpg","pet_policy":"No pets","check_in_time":"13:00","check_out_time":"11:00"}`)
		case "/api/v1/hotels/22":
			fmt.Fprint(w, `{"hotel_id":22,"name":"Hidden Draft Hotel","city":"Varanasi","is_active":false}`)
		case "/api/v1/room-types":
			// Already discounted 3000 -> 2700 with GST on 2700.
			fmt.Fprint(w, `[{"room_type_id":211,"hotel_id":21,"hotel_name":"Shanti Ghat Residency","name":"River Room","capacity":2,"price_per_night":2600,
 "description":"","amenities":[],"images":[],"total_rooms":6,"available_count":2,"gst_rate":0.05,"tax_per_night":130,"rooms_needed":2,"fits_guests":true,
 "rate_plans":[{"rate_plan_id":71,"room_id":3,"name":"Festive deal","price_per_night":2700,"max_occupancy":2,"free_cancellation":true,"pay_at_property":true,
  "breakfast_included":false,"inclusions":[],"is_active":true,"discount_type":"percentage","discount_value":10,"discount_valid_from":"2026-01-01",
  "discount_valid_to":"2027-12-31","original_price_per_night":3000,"discount_percent":10,"gst_rate":0.05,"tax_per_night":135}]}]`)
		case "/api/v1/landmarks":
			fmt.Fprint(w, `[{"landmark_id":1,"city":"Varanasi","name":"Assi Ghat","latitude":25.2861,"longitude":83.0063,"is_active":true}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newSKYMCMServer(t *testing.T, down *atomic.Bool) *testServer {
	t.Helper()
	ai := &fakeOllama{reply: "Here are stays in Varanasi.\n<meta intent=\"hotel_search\" needs_info=\"false\" needs_login=\"false\"/>"}
	aiServer := httptest.NewServer(ai)
	t.Cleanup(aiServer.Close)
	site := skymcmStandIn(t, down)

	cfg := &config.Config{
		AppEnv: config.EnvTest, CatalogSource: config.CatalogSKYMCM, SKYMCMAPIURL: site.URL, SKYMCMAssetURL: "https://skymcm.example",
		CORSAllowedOrigins: []string{frontendOrigin}, AIProvider: "ollama", AIModel: "test-model", AIBaseURL: aiServer.URL,
		AITimeoutSeconds: 10, AIMaxTokens: 256, ChatbotRatePerMin: 50, SiteURL: "https://skymcm.example", UploadsDir: t.TempDir(),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	client, err := skymcm.New(skymcm.Config{BaseURL: cfg.SKYMCMAPIURL, AssetURL: cfg.SKYMCMAssetURL, Timeout: 2 * time.Second, CacheTTL: time.Millisecond, RoomCacheTTL: time.Millisecond, MaxStale: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	a := Build(cfg, Sources{SKYMCM: client}, cache.New(""))
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return &testServer{Server: srv, ai: ai}
}

func TestSKYMCMCatalogueKeepsTheFrontendContract(t *testing.T) {
	s := newSKYMCMServer(t, nil)

	list := decode[[]map[string]any](t, s.do(t, "GET", "/api/v1/hotels?city=Varanasi", "", nil))
	if len(list) != 1 || list[0]["name"] != "Shanti Ghat Residency" {
		t.Fatalf("want only the listed hotel, got %v", list)
	}
	h := list[0]
	if h["thumbnail_url"] != "https://skymcm.example/uploads/hotels/3f9a.jpg" {
		t.Errorf("photo URL %v", h["thumbnail_url"])
	}
	if h["price_per_night"] != 2600.0 || h["tax_per_night"] != 130.0 {
		t.Errorf("price/GST changed: %v / %v", h["price_per_night"], h["tax_per_night"])
	}
	for _, k := range []string{"hotel_id", "name", "location", "description", "city", "state", "country", "address", "category", "price_per_night",
		"rating", "total_reviews", "amenities", "thumbnail_url", "images", "owner_id", "is_active", "free_cancellation", "pay_at_property",
		"instant_booking", "tax_rate", "check_in_time", "check_out_time"} {
		if _, ok := h[k]; !ok {
			t.Errorf("hotel missing %q", k)
		}
	}

	if r := s.do(t, "GET", "/api/v1/hotels/22", "", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("inactive hotel by id: %d", r.StatusCode)
	}
	one := decode[map[string]any](t, s.do(t, "GET", "/api/v1/hotels/21", "", nil))
	if one["check_in_time"] != "13:00" {
		t.Errorf("hotel 21: %v", one)
	}

	types := decode[[]map[string]any](t, s.do(t, "GET", "/api/v1/room-types?hotel_id=21&check_in=2027-01-10&check_out=2027-01-12&guests=4", "", nil))
	if len(types) != 1 {
		t.Fatalf("room types: %v", types)
	}
	plan := types[0]["rate_plans"].([]any)[0].(map[string]any)
	// 2430 would mean the 10% discount was applied a second time.
	if plan["price_per_night"] != 2700.0 || plan["original_price_per_night"] != 3000.0 || plan["tax_per_night"] != 135.0 {
		t.Fatalf("plan repriced: %v", plan)
	}
	if types[0]["available_count"] != 2.0 || types[0]["rooms_needed"] != 2.0 {
		t.Errorf("availability changed: %v", types[0])
	}
	if r := s.do(t, "GET", "/api/v1/room-types?hotel_id=21&check_in=bad&check_out=2027-01-12", "", nil); r.StatusCode != http.StatusBadRequest {
		t.Errorf("bad date: %d", r.StatusCode)
	}

	ready := decode[map[string]any](t, s.do(t, "GET", "/readyz", "", nil))
	st, _ := ready["skymcm"].(map[string]any)
	if ready["catalog"] != "skymcm" || st["state"] != "ok" {
		t.Errorf("readyz: %v", ready)
	}
}

func TestSKYMCMHotelsReachTheChatbot(t *testing.T) {
	s := newSKYMCMServer(t, nil)
	env := decode[messageEnvelope](t, s.do(t, "POST", "/api/v1/chatbot/message", `{"message":"Show me hotels in Varanasi"}`, nil))
	if len(env.Data.Hotels) != 1 || env.Data.Hotels[0]["name"] != "Shanti Ghat Residency" {
		t.Fatalf("cards: %+v", env.Data.Hotels)
	}
	if env.Data.Hotels[0]["thumbnail_url"] != "https://skymcm.example/uploads/hotels/3f9a.jpg" || env.Data.Hotels[0]["tax_per_night"] != 130.0 {
		t.Errorf("card: %v", env.Data.Hotels[0])
	}
}

func TestSKYMCMOutageIsReportedSafely(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	s := newSKYMCMServer(t, &down)
	r := s.do(t, "GET", "/api/v1/hotels", "", nil)
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusInternalServerError || strings.Contains(string(body), "10.9.9.9") || !strings.Contains(string(body), `"error"`) {
		t.Fatalf("outage: %d %s", r.StatusCode, body)
	}
	ready := decode[map[string]any](t, s.do(t, "GET", "/readyz", "", nil))
	if st, _ := ready["skymcm"].(map[string]any); st["state"] != "degraded" {
		t.Errorf("readyz during outage: %v", ready)
	}
}
