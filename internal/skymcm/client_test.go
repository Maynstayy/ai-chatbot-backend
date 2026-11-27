package skymcm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
	"github.com/Maynstayy/ai-chatbot-backend/internal/roomtypes"
)

// Compile-time: the client is a drop-in for the services the app uses.
var (
	_ interface {
		GetHotels(context.Context, hotels.HotelFilter) ([]hotels.Hotel, error)
		GetHotelByID(context.Context, int64) (*hotels.Hotel, error)
	} = (*Client)(nil)
	_ roomtypes.Service = (*Client)(nil)
)

// --- a stand-in for the SKYMCM API, shaped like MaynStay's real responses ---

const hotelsJSON = `[
 {"hotel_id":11,"name":"Ganga View Residency","location":"Assi Ghat","description":"Riverside stay","city":"Varanasi","state":"Uttar Pradesh","country":"India","address":"1 Ghat Rd",
  "category":"Heritage","price_per_night":3800,"rating":4.6,"total_reviews":120,"amenities":["Free WiFi","Rooftop"],
  "thumbnail_url":"/uploads/hotels/ab12.jpg","images":["/uploads/hotels/ab12.jpg","https://cdn.example.com/x.jpg","javascript:alert(1)","//evil.example/y.jpg"],
  "owner_id":7,"is_active":true,"is_deleted":false,"free_cancellation":true,"pay_at_property":true,"instant_booking":false,
  "gst_rate":0.05,"tax_per_night":190,"tax_rate":0,"check_in_time":"","check_out_time":""},
 {"hotel_id":12,"name":"Closed Lodge","city":"Varanasi","price_per_night":900,"is_active":false,"is_deleted":false,"amenities":null},
 {"hotel_id":13,"name":"Palace Stay","city":"Varanasi","price_per_night":8200,"rating":4.8,"total_reviews":210,"is_active":true,"is_deleted":false,
  "thumbnail_url":"","images":[],"amenities":[],"gst_rate":0.18,"tax_per_night":1476}
]`

// Final prices from MaynStay's /room-types: the 9000 plan is already
// discounted to 8100 (10% off) with GST on 8100, and a seasonal stay_total.
const roomTypesJSON = `[
 {"room_type_id":131,"hotel_id":13,"hotel_name":"Palace Stay","name":"Palace Room","capacity":2,"price_per_night":8200,"description":"","amenities":[],
  "images":["/uploads/rooms/r1.jpg"],"total_rooms":5,"available_count":3,"gst_rate":0.18,"tax_per_night":1476,"rooms_needed":2,"fits_guests":true,
  "combo_price_per_night":16200,"recommended":true,
  "rate_plans":[{"rate_plan_id":901,"room_id":55,"name":"Winter offer","price_per_night":8100,"max_occupancy":2,"free_cancellation":true,"pay_at_property":false,
    "breakfast_included":true,"inclusions":["Breakfast"],"is_active":true,"discount_type":"percentage","discount_value":10,
    "discount_valid_from":"2026-11-01","discount_valid_to":"2027-02-28","original_price_per_night":9000,"discount_percent":10,
    "stay_total":16500,"gst_rate":0.18,"tax_per_night":1458}]},
 {"room_type_id":999,"hotel_id":99,"name":"Someone else's room","capacity":2,"price_per_night":1,"available_count":9,"rate_plans":[]}
]`

type stand struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	reqs     []*http.Request
	calls    atomic.Int64
	inflight atomic.Int64
	maxIn    atomic.Int64
	handler  func(w http.ResponseWriter, r *http.Request) bool // true = handled
	delay    time.Duration
}

func newStand(t *testing.T) *stand {
	s := &stand{t: t}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		n := s.inflight.Add(1)
		defer s.inflight.Add(-1)
		for {
			m := s.maxIn.Load()
			if n <= m || s.maxIn.CompareAndSwap(m, n) {
				break
			}
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Clone(context.Background()))
		h := s.handler
		s.mu.Unlock()
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		if h != nil && h(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/hotels":
			fmt.Fprint(w, hotelsJSON)
		case r.URL.Path == "/api/v1/hotels/11":
			fmt.Fprint(w, `{"hotel_id":11,"name":"Ganga View Residency","city":"Varanasi","is_active":true,"thumbnail_url":"/uploads/hotels/ab12.jpg","check_in_time":"14:00","check_out_time":"11:00","price_per_night":3800,"tax_per_night":190}`)
		case r.URL.Path == "/api/v1/hotels/12":
			fmt.Fprint(w, `{"hotel_id":12,"name":"Closed Lodge","city":"Varanasi","is_active":false}`)
		case r.URL.Path == "/api/v1/hotels/13":
			fmt.Fprint(w, `{"hotel_id":13,"name":"Palace Stay","city":"Varanasi","is_active":true}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/hotels/"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"hotel not found"}`)
		case r.URL.Path == "/api/v1/room-types":
			fmt.Fprint(w, roomTypesJSON)
		case r.URL.Path == "/api/v1/landmarks":
			fmt.Fprint(w, `[{"landmark_id":1,"city":"Varanasi","name":"Dashashwamedh Ghat","latitude":25.306,"longitude":83.0104,"is_active":true},{"landmark_id":2,"city":"Varanasi","name":"Hidden","latitude":1,"longitude":1,"is_active":false}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stand) setHandler(h func(w http.ResponseWriter, r *http.Request) bool) {
	s.mu.Lock()
	s.handler = h
	s.mu.Unlock()
}

func (s *stand) requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.reqs...)
}

func client(t *testing.T, s *stand, mutate ...func(*Config)) *Client {
	t.Helper()
	cfg := Config{BaseURL: s.srv.URL, AssetURL: "https://skymcm.example", Timeout: 2 * time.Second}
	for _, m := range mutate {
		m(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

var ctx = context.Background()

func str(s string) *string { return &s }

// --- listings ------------------------------------------------------------------

func TestGetHotelsKeepsOnlyListedHotelsAndResolvesPhotos(t *testing.T) {
	s := newStand(t)
	c := client(t, s)
	list, err := c.GetHotels(ctx, hotels.HotelFilter{City: "Varanasi", Search: "ganga", MinPrice: 1000, MaxPrice: 5000, Sort: "rating"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].HotelID != 11 || list[1].HotelID != 13 {
		t.Fatalf("want hotels 11 and 13 only (12 is inactive), got %+v", list)
	}
	h := list[0]
	if h.ThumbnailURL != "https://skymcm.example/uploads/hotels/ab12.jpg" {
		t.Errorf("thumbnail = %q", h.ThumbnailURL)
	}
	if want := []string{"https://skymcm.example/uploads/hotels/ab12.jpg", "https://cdn.example.com/x.jpg"}; fmt.Sprint(h.Images) != fmt.Sprint(want) {
		t.Errorf("images = %v (unsafe ones must be dropped)", h.Images)
	}
	// Prices and GST exactly as the API computed them.
	if h.PricePerNight != 3800 || h.GSTRate != 0.05 || h.TaxPerNight != 190 || list[1].TaxPerNight != 1476 {
		t.Errorf("prices changed: %+v / %+v", h, list[1])
	}
	q := s.requests()[0].URL.Query()
	if q.Get("city") != "Varanasi" || q.Get("search") != "ganga" || q.Get("min_price") != "1000" || q.Get("max_price") != "5000" || q.Get("sort") != "rating" || q.Has("owner_id") {
		t.Errorf("query sent: %v", q)
	}
}

func TestGetHotelByID(t *testing.T) {
	s := newStand(t)
	c := client(t, s)
	h, err := c.GetHotelByID(ctx, 11)
	if err != nil || h.CheckInTime != "14:00" || h.ThumbnailURL != "https://skymcm.example/uploads/hotels/ab12.jpg" {
		t.Fatalf("hotel 11: %+v %v", h, err)
	}
	for _, id := range []int64{12, 404, 0, -5} {
		if _, err := c.GetHotelByID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("hotel %d: want ErrNotFound, got %v", id, err)
		}
	}
	for _, r := range s.requests() {
		if r.URL.Path == "/api/v1/hotels/0" || r.URL.Path == "/api/v1/hotels/-5" {
			t.Errorf("invalid id was requested: %s", r.URL.Path)
		}
	}
}

// --- availability and prices -----------------------------------------------------

func TestRoomTypesUseAPIPricesAsReturned(t *testing.T) {
	s := newStand(t)
	c := client(t, s)
	types, err := c.GetRoomTypesByHotel(ctx, 13, str("2027-01-10"), str("2027-01-12"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 1 {
		t.Fatalf("want only hotel 13's room type, got %d", len(types))
	}
	rt := types[0]
	p := rt.RatePlans[0]
	// Already discounted (9000 -> 8100) with GST on 8100: unchanged here.
	if p.PricePerNight != 8100 || *p.OriginalPricePerNight != 9000 || *p.DiscountPercent != 10 || p.TaxPerNight != 1458 || *p.StayTotal != 16500 {
		t.Fatalf("plan repriced: %+v", p)
	}
	if rt.PricePerNight != 8200 || rt.TaxPerNight != 1476 || rt.AvailableCount != 3 || rt.RoomsNeeded != 2 || !*rt.FitsGuests || rt.ComboPricePerNight != 16200 {
		t.Fatalf("room type changed: %+v", rt)
	}
	if rt.Images[0] != "https://skymcm.example/uploads/rooms/r1.jpg" {
		t.Errorf("room image = %q", rt.Images[0])
	}
	var roomReq *http.Request
	for _, r := range s.requests() {
		if r.URL.Path == "/api/v1/room-types" {
			roomReq = r
		}
	}
	q := roomReq.URL.Query()
	if q.Get("hotel_id") != "13" || q.Get("check_in") != "2027-01-10" || q.Get("check_out") != "2027-01-12" || q.Get("guests") != "4" {
		t.Errorf("room-types query: %v", q)
	}
}

func TestRoomTypesValidateBeforeCallingAndSkipUnlistedHotels(t *testing.T) {
	s := newStand(t)
	c := client(t, s)
	if _, err := c.GetRoomTypesByHotel(ctx, 13, str("10-01-2027"), str("2027-01-12"), 0); !errors.Is(err, roomtypes.ErrInvalidDateFmt) {
		t.Errorf("bad date: %v", err)
	}
	if _, err := c.GetRoomTypesByHotel(ctx, 13, str("2027-01-10"), nil, 0); !errors.Is(err, roomtypes.ErrInvalidDateFmt) {
		t.Errorf("lone check-in: %v", err)
	}
	if _, err := c.GetRoomTypesByHotel(ctx, 13, nil, nil, 500); !errors.Is(err, roomtypes.ErrInvalidGuests) {
		t.Errorf("guests: %v", err)
	}
	if s.calls.Load() != 0 {
		t.Fatalf("invalid input reached the API (%d calls)", s.calls.Load())
	}
	types, err := c.GetRoomTypesByHotel(ctx, 12, nil, nil, 0)
	if err != nil || len(types) != 0 {
		t.Fatalf("inactive hotel: %v %v", types, err)
	}
	for _, r := range s.requests() {
		if r.URL.Path == "/api/v1/room-types" {
			t.Fatal("room types requested for an inactive hotel")
		}
	}
}

func TestLandmarks(t *testing.T) {
	s := newStand(t)
	c := client(t, s)
	list, err := c.ListByCity(ctx, "Varanasi")
	if err != nil || len(list) != 1 || list[0].Name != "Dashashwamedh Ghat" {
		t.Fatalf("landmarks: %+v %v", list, err)
	}
	if got, _ := c.ListByCity(ctx, "  "); len(got) != 0 || s.calls.Load() != 1 {
		t.Fatalf("empty city must not call the API")
	}
}

// --- what is sent, and what is refused ---------------------------------------------

func TestOnlyPlainGETsToAllowedPathsWithNoCredentials(t *testing.T) {
	s := newStand(t)
	c := client(t, s)
	_, _ = c.GetHotels(ctx, hotels.HotelFilter{})
	_, _ = c.GetHotelByID(ctx, 11)
	_, _ = c.GetRoomTypesByHotel(ctx, 13, nil, nil, 2)
	_, _ = c.ListByCity(ctx, "Varanasi")
	for _, r := range s.requests() {
		if r.Method != http.MethodGet || !allowedPath.MatchString(r.URL.Path) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		for _, h := range []string{"Authorization", "Cookie", "X-Forwarded-For"} {
			if r.Header.Get(h) != "" {
				t.Errorf("%s sent on %s", h, r.URL.Path)
			}
		}
		if !strings.HasPrefix(r.UserAgent(), "skymcm-ai-chatbot/") {
			t.Errorf("user agent %q", r.UserAgent())
		}
	}
	if _, err := c.fetch(ctx, "/api/v1/bookings", nil, time.Second, false); err == nil {
		t.Fatal("a path outside the allowlist was requested")
	}
}

func TestRefusesRedirectToAnotherHost(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, hotelsJSON) }))
	defer evil.Close()
	s := newStand(t)
	s.setHandler(func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusFound)
		return true
	})
	c := client(t, s)
	if _, err := c.GetHotels(ctx, hotels.HotelFilter{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("redirect off-host: %v", err)
	}
}

func TestBadResponsesAreRefusedWithoutLeakingDetails(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter){
		"oversized": func(w http.ResponseWriter) {
			fmt.Fprint(w, "["+strings.Repeat(`{"hotel_id":1},`, 2000)+`{"hotel_id":1}]`)
		},
		"not json": func(w http.ResponseWriter) { fmt.Fprint(w, "<html>gateway</html>") },
		"500": func(w http.ResponseWriter) {
			w.WriteHeader(500)
			fmt.Fprint(w, "dial tcp 10.0.0.5:3306: connection refused")
		},
	}
	for name, respond := range cases {
		s := newStand(t)
		s.setHandler(func(w http.ResponseWriter, r *http.Request) bool { respond(w); return true })
		c := client(t, s, func(c *Config) { c.MaxBodyBytes = 10_000 })
		_, err := c.GetHotels(ctx, hotels.HotelFilter{})
		if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "10.0.0.5") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTimeout(t *testing.T) {
	s := newStand(t)
	s.delay = 300 * time.Millisecond
	c := client(t, s, func(c *Config) { c.Timeout = 50 * time.Millisecond })
	start := time.Now()
	if _, err := c.GetHotels(ctx, hotels.HotelFilter{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestRetriesOnceAfterA503ButNeverAfterA429(t *testing.T) {
	s := newStand(t)
	var n atomic.Int64
	s.setHandler(func(w http.ResponseWriter, r *http.Request) bool {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	c := client(t, s)
	if _, err := c.GetHotels(ctx, hotels.HotelFilter{}); err != nil || s.calls.Load() != 2 {
		t.Fatalf("503 then 200: err=%v calls=%d", err, s.calls.Load())
	}
}

// --- 429 handling ------------------------------------------------------------------------

func TestRateLimitPausesRequestsAndServesCachedListings(t *testing.T) {
	s := newStand(t)
	c := client(t, s, func(c *Config) { c.CacheTTL = time.Second })
	now := time.Date(2027, 1, 1, 10, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	if _, err := c.GetHotels(ctx, hotels.HotelFilter{}); err != nil {
		t.Fatal(err)
	}
	s.setHandler(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Retry-After", "20")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})
	now = now.Add(2 * time.Second) // cache expired
	list, err := c.GetHotels(ctx, hotels.HotelFilter{})
	if err != nil || len(list) != 2 {
		t.Fatalf("on 429 the last listing should be served: %v %v", list, err)
	}
	before := s.calls.Load()
	if _, err := c.GetRoomTypesByHotel(ctx, 13, str("2027-01-10"), str("2027-01-12"), 2); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("availability during cooldown: %v", err)
	}
	if s.calls.Load() != before {
		t.Fatal("requests were sent during the Retry-After pause")
	}
	if st := c.Status(); st.State != "rate_limited" {
		t.Fatalf("status %+v", st)
	}
	now = now.Add(21 * time.Second)
	s.setHandler(nil)
	if _, err := c.GetRoomTypesByHotel(ctx, 13, nil, nil, 0); err != nil {
		t.Fatalf("after the pause: %v", err)
	}
}

func TestRetryAfterIsBounded(t *testing.T) {
	now := time.Now()
	if retryAfter("3600", now) != maxCooldown || retryAfter("0", now) != time.Second || retryAfter("junk", now) != defaultCooldown {
		t.Fatal("Retry-After bounds")
	}
}

// --- caching, staleness and load ---------------------------------------------------------

func TestCachesAndNeverServesStaleAvailability(t *testing.T) {
	s := newStand(t)
	c := client(t, s, func(c *Config) { c.CacheTTL = time.Minute; c.RoomCacheTTL = 10 * time.Second })
	now := time.Date(2027, 1, 1, 10, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		_, _ = c.GetHotels(ctx, hotels.HotelFilter{City: "Varanasi"})
	}
	if s.calls.Load() != 1 {
		t.Fatalf("listing not cached: %d calls", s.calls.Load())
	}
	if _, err := c.GetRoomTypesByHotel(ctx, 13, str("2027-01-10"), str("2027-01-12"), 2); err != nil {
		t.Fatal(err)
	}
	s.setHandler(func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusBadGateway)
		return true
	})
	now = now.Add(11 * time.Second) // availability expired, listing still fresh
	if _, err := c.GetRoomTypesByHotel(ctx, 13, str("2027-01-10"), str("2027-01-12"), 2); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale availability must not be served: %v", err)
	}
	now = now.Add(2 * time.Minute) // listing expired but within MaxStale
	if list, err := c.GetHotels(ctx, hotels.HotelFilter{City: "Varanasi"}); err != nil || len(list) != 2 {
		t.Fatalf("stale listing during an outage: %v %v", list, err)
	}
	if st := c.Status(); st.State != "degraded" {
		t.Fatalf("status %+v", st)
	}
	now = now.Add(time.Hour) // too old even to serve stale
	if _, err := c.GetHotels(ctx, hotels.HotelFilter{City: "Varanasi"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("very stale listing: %v", err)
	}
}

func TestConcurrencyIsLimitedAndIdenticalRequestsAreShared(t *testing.T) {
	s := newStand(t)
	s.delay = 40 * time.Millisecond
	c := client(t, s, func(c *Config) { c.MaxConcurrent = 3 })
	// Prime the cached hotel list so only room-type requests are measured.
	_, _ = c.GetHotels(ctx, hotels.HotelFilter{})

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := fmt.Sprintf("2027-02-%02d", i+1)
			out := fmt.Sprintf("2027-02-%02d", i+2)
			_, _ = c.GetRoomTypesByHotel(ctx, 13, &in, &out, 2)
		}(i)
	}
	wg.Wait()
	if m := s.maxIn.Load(); m > 3 {
		t.Fatalf("max concurrent requests %d > 3", m)
	}

	before := s.calls.Load()
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.GetHotels(ctx, hotels.HotelFilter{City: "Agra"}) }()
	}
	wg.Wait()
	if got := s.calls.Load() - before; got != 1 {
		t.Fatalf("10 identical concurrent requests made %d calls", got)
	}
}

// --- configuration ---------------------------------------------------------------------

func TestNewRejectsUnsafeBaseURLs(t *testing.T) {
	for _, raw := range []string{"", "skymcm.com", "ftp://skymcm.com", "https://user:pw@skymcm.com", "https://skymcm.com/?x=1", "https://skymcm.com/#frag"} {
		if _, err := New(Config{BaseURL: raw}); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	c, err := New(Config{BaseURL: "https://skymcm.com/"})
	if err != nil || c.asset != "https://skymcm.com" {
		t.Fatalf("asset base should default to the API host: %v %q", err, c.asset)
	}
	if c.resolveImage("/uploads/hotels/a.jpg") != "https://skymcm.com/uploads/hotels/a.jpg" {
		t.Fatal("default photo base")
	}
}

func TestStatusNeverCallsTheAPI(t *testing.T) {
	s := newStand(t)
	c := client(t, s)
	if st := c.Status(); st.State != "unknown" || s.calls.Load() != 0 {
		t.Fatalf("%+v calls=%d", st, s.calls.Load())
	}
	_, _ = c.GetHotels(ctx, hotels.HotelFilter{})
	if st := c.Status(); st.State != "ok" {
		t.Fatalf("%+v", st)
	}
	b, _ := json.Marshal(c.Status())
	if strings.Contains(string(b), s.srv.URL) {
		t.Fatal("status exposes the API address")
	}
}

// --- listing checks for availability use the cached hotel list ---------------------

func countPaths(reqs []*http.Request) map[string]int {
	m := map[string]int{}
	for _, r := range reqs {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/v1/hotels/") {
			p = "/api/v1/hotels/{id}"
		}
		m[p]++
	}
	return m
}

func TestAvailabilityChecksTheCachedListNotEachHotel(t *testing.T) {
	s := newStand(t)
	c := client(t, s)
	// Two hotels, several stays: one list request covers every listing check.
	for _, id := range []int64{13, 11, 13} {
		for _, in := range []string{"2027-01-10", "2027-01-20"} {
			out := in[:8] + fmt.Sprint(int(in[8]-'0')*10+int(in[9]-'0')+2)
			if _, err := c.GetRoomTypesByHotel(ctx, id, &in, &out, 2); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := countPaths(s.requests())
	if got["/api/v1/hotels/{id}"] != 0 || got["/api/v1/hotels"] != 1 {
		t.Fatalf("want 1 list request and no per-hotel lookups, got %v", got)
	}
	if got["/api/v1/room-types"] != 4 { // 2 hotels x 2 stays (repeats are cached)
		t.Fatalf("room-types requests: %v", got)
	}
}

func TestAvailabilityStillSkipsUnlistedHotels(t *testing.T) {
	s := newStand(t)
	c := client(t, s)
	for _, id := range []int64{12, 404} { // inactive in the list; not in the list at all
		types, err := c.GetRoomTypesByHotel(ctx, id, str("2027-01-10"), str("2027-01-12"), 2)
		if err != nil || len(types) != 0 {
			t.Fatalf("hotel %d: %v %v", id, types, err)
		}
	}
	got := countPaths(s.requests())
	if got["/api/v1/room-types"] != 0 || got["/api/v1/hotels/{id}"] != 0 {
		t.Fatalf("unlisted hotels reached room-types or per-hotel lookups: %v", got)
	}
}

func TestAvailabilityFallsBackToTheHotelLookupOnlyWhenTheListFails(t *testing.T) {
	s := newStand(t)
	s.setHandler(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v1/hotels" {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})
	c := client(t, s)
	types, err := c.GetRoomTypesByHotel(ctx, 13, str("2027-01-10"), str("2027-01-12"), 2)
	if err != nil || len(types) != 1 || types[0].RatePlans[0].PricePerNight != 8100 {
		t.Fatalf("listed hotel via fallback: %v %v", types, err)
	}
	if types, err := c.GetRoomTypesByHotel(ctx, 12, nil, nil, 0); err != nil || len(types) != 0 {
		t.Fatalf("inactive hotel via fallback: %v %v", types, err)
	}
	got := countPaths(s.requests())
	if got["/api/v1/hotels/{id}"] != 2 {
		t.Fatalf("fallback lookups: %v", got)
	}
}

func TestAvailabilityDuringA429DoesNotFallBack(t *testing.T) {
	s := newStand(t)
	s.setHandler(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})
	c := client(t, s)
	if _, err := c.GetRoomTypesByHotel(ctx, 13, nil, nil, 0); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if n := s.calls.Load(); n != 1 {
		t.Fatalf("a rate-limited list must not trigger more requests (got %d)", n)
	}
}

// --- landmarks are cached for 30 minutes ----------------------------------------------

func TestLandmarksAreCachedForThirtyMinutes(t *testing.T) {
	s := newStand(t)
	c := client(t, s) // no LandmarkCacheTTL set: the default applies
	if c.cfg.LandmarkCacheTTL != 30*time.Minute || c.cfg.CacheTTL != time.Minute || c.cfg.RoomCacheTTL != 30*time.Second {
		t.Fatalf("defaults: landmarks %v, hotels %v, rooms %v", c.cfg.LandmarkCacheTTL, c.cfg.CacheTTL, c.cfg.RoomCacheTTL)
	}
	now := time.Date(2027, 1, 1, 10, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	landmarkCalls := func() int { return countPaths(s.requests())["/api/v1/landmarks"] }
	hotelCalls := func() int { return countPaths(s.requests())["/api/v1/hotels"] }

	for _, city := range []string{"Varanasi", "Agra"} {
		if _, err := c.ListByCity(ctx, city); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = c.GetHotels(ctx, hotels.HotelFilter{})
	if landmarkCalls() != 2 || hotelCalls() != 1 {
		t.Fatalf("first round: landmarks %d, hotels %d", landmarkCalls(), hotelCalls())
	}

	now = now.Add(29 * time.Minute) // hotels expired long ago, landmarks still fresh
	for _, city := range []string{"Varanasi", "Agra", "Varanasi"} {
		if list, err := c.ListByCity(ctx, city); err != nil || len(list) != 1 {
			t.Fatalf("cached landmarks for %s: %v %v", city, list, err)
		}
	}
	_, _ = c.GetHotels(ctx, hotels.HotelFilter{})
	if landmarkCalls() != 2 {
		t.Fatalf("landmarks refetched within 30 minutes: %d calls", landmarkCalls())
	}
	if hotelCalls() != 2 {
		t.Fatalf("hotels must keep their 60-second cache: %d calls", hotelCalls())
	}

	now = now.Add(2 * time.Minute) // 31 minutes: refetched
	_, _ = c.ListByCity(ctx, "Varanasi")
	if landmarkCalls() != 3 {
		t.Fatalf("landmarks not refetched after 30 minutes: %d calls", landmarkCalls())
	}
}

func TestLandmarkCacheDurationIsConfigurable(t *testing.T) {
	s := newStand(t)
	c := client(t, s, func(c *Config) { c.LandmarkCacheTTL = 5 * time.Minute })
	now := time.Date(2027, 1, 1, 10, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	_, _ = c.ListByCity(ctx, "Varanasi")
	now = now.Add(6 * time.Minute)
	_, _ = c.ListByCity(ctx, "Varanasi")
	if n := countPaths(s.requests())["/api/v1/landmarks"]; n != 2 {
		t.Fatalf("custom 5-minute cache: %d calls", n)
	}
}

func TestCachedLandmarksCoverAnOutageWithinTheirWindow(t *testing.T) {
	s := newStand(t)
	c := client(t, s) // landmarks 30 min + 10 min stale window
	now := time.Date(2027, 1, 1, 10, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	if _, err := c.ListByCity(ctx, "Varanasi"); err != nil {
		t.Fatal(err)
	}
	s.setHandler(func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusBadGateway)
		return true
	})
	now = now.Add(35 * time.Minute) // expired, within cache time + MaxStale
	if list, err := c.ListByCity(ctx, "Varanasi"); err != nil || len(list) != 1 {
		t.Fatalf("landmarks during an outage: %v %v", list, err)
	}
	now = now.Add(10 * time.Minute) // 45 minutes: too old to serve
	if _, err := c.ListByCity(ctx, "Varanasi"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("very old landmarks must not be served: %v", err)
	}
}
