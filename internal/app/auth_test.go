package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/catalog"
	"github.com/Maynstayy/ai-chatbot-backend/internal/config"
	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
	"github.com/Maynstayy/ai-chatbot-backend/internal/roomtypes"
	"github.com/Maynstayy/ai-chatbot-backend/internal/session"
	"github.com/Maynstayy/ai-chatbot-backend/pkg/cache"
)

// Mock tokens only: shaped like the website's JWTs, accepted or refused by
// the local mock profile endpoint below. No real secret is involved.
func mockToken(id int64, exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(map[string]any{"signup_id": id, "exp": exp.Unix()})
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(payload) + ".bW9jay1zaWduYXR1cmU"
}

type mockAuth struct {
	*httptest.Server
	mu      sync.Mutex
	valid   map[string]int64
	revoked map[string]bool
	calls   atomic.Int32
}

func newMockAuth(t *testing.T) *mockAuth {
	m := &mockAuth{valid: map[string]int64{}, revoked: map[string]bool{}}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		if r.URL.Path != session.ProfilePath {
			http.NotFound(w, r)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		id, ok := m.valid[tok]
		if !ok || m.revoked[tok] {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"Unauthorized"}`)
			return
		}
		fmt.Fprintf(w, `{"success":true,"data":{"signup_id":%d}}`, id)
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *mockAuth) issue(id int64) string {
	tok := mockToken(id, time.Now().Add(time.Hour))
	m.mu.Lock()
	m.valid[tok] = id
	m.mu.Unlock()
	return tok
}

// countingHotels/countingRooms record every catalogue read, to show that a
// refused request never reaches a hotel search.
type countingHotels struct {
	hotels.Repository
	n *atomic.Int32
}

func (c countingHotels) GetHotels(ctx context.Context, f hotels.HotelFilter) ([]hotels.Hotel, error) {
	c.n.Add(1)
	return c.Repository.GetHotels(ctx, f)
}

func (c countingHotels) GetHotelByID(ctx context.Context, id int64) (*hotels.Hotel, error) {
	c.n.Add(1)
	return c.Repository.GetHotelByID(ctx, id)
}

type countingRooms struct {
	roomtypes.Repository
	n *atomic.Int32
}

func (c countingRooms) GetRoomTypesByHotelID(ctx context.Context, id int64, in, out *string) ([]roomtypes.RoomType, error) {
	c.n.Add(1)
	return c.Repository.GetRoomTypesByHotelID(ctx, id, in, out)
}

type authServer struct {
	*testServer
	auth    *mockAuth
	lookups *atomic.Int32
}

func newAuthServer(t *testing.T, cacheSeconds int) *authServer {
	t.Helper()
	auth := newMockAuth(t)
	ai := &fakeOllama{reply: "Happy to help with your stay.\n<meta intent=\"general\" needs_info=\"false\" needs_login=\"false\"/>"}
	aiServer := httptest.NewServer(ai)
	t.Cleanup(aiServer.Close)
	cfg := &config.Config{
		AppEnv: config.EnvTest, AppPort: "0", CatalogSource: config.CatalogFile, CatalogFile: "../../testdata/catalog.json",
		CORSAllowedOrigins: []string{frontendOrigin},
		AIProvider:         "ollama", AIModel: "test-model", AIBaseURL: aiServer.URL,
		AITimeoutSeconds: 10, AIMaxTokens: 256, ChatbotRatePerMin: 20,
		SiteURL: "https://example.test", SupportPhone: "+91 0000000000", SupportEmail: "support@example.test",
		UploadsDir:   t.TempDir(),
		AuthRequired: true, AuthURL: auth.URL, AuthCookieName: "maynstay_token",
		AuthCacheSeconds: cacheSeconds, AuthTimeoutSeconds: 2,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store, err := catalog.LoadFile(cfg.CatalogFile)
	if err != nil {
		t.Fatal(err)
	}
	var lookups atomic.Int32
	a := Build(cfg, Sources{
		Hotels:    countingHotels{store.Hotels(), &lookups},
		RoomTypes: countingRooms{store.RoomTypes(), &lookups},
		Landmarks: store.Landmarks(),
	}, cache.New(""))
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return &authServer{testServer: &testServer{Server: srv, ai: ai}, auth: auth, lookups: &lookups}
}

func cookie(tok string) map[string]string {
	h := map[string]string{"Origin": frontendOrigin}
	if tok != "" {
		h["Cookie"] = "maynstay_token=" + tok
	}
	return h
}

// Every protected endpoint, as the chat frontend calls it.
var protectedCalls = []struct{ method, path, body string }{
	{http.MethodPost, "/api/v1/chatbot/message", `{"message":"Show me hotels in Varanasi"}`},
	{http.MethodPost, "/api/v1/chatbot/message", `{"message":"tell me something about hotel rules"}`},
	{http.MethodGet, "/api/v1/chatbot/session", ""},
	{http.MethodGet, "/api/v1/hotels", ""},
	{http.MethodGet, "/api/v1/hotels/101", ""},
	{http.MethodGet, "/api/v1/room-types?hotel_id=101&check_in=2026-12-10&check_out=2026-12-12&guests=2", ""},
}

func errorText(t *testing.T, res *http.Response) string {
	t.Helper()
	var body struct{ Error string }
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Error
}

func TestSignedOutRequestsAre401AndRunNothing(t *testing.T) {
	s := newAuthServer(t, 30)
	expired := mockToken(9004, time.Now().Add(-time.Minute))
	revoked := s.auth.issue(9003)
	s.auth.mu.Lock()
	s.auth.revoked[revoked] = true
	s.auth.mu.Unlock()

	for name, tok := range map[string]string{
		"missing": "",
		"invalid": mockToken(4242, time.Now().Add(time.Hour)), // never issued
		"garbage": "%%%",
		"expired": expired,
		"revoked": revoked,
	} {
		for _, c := range protectedCalls {
			res := s.do(t, c.method, c.path, c.body, cookie(tok))
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s %s: status %d", name, c.method, c.path, res.StatusCode)
			}
			if msg := errorText(t, res); msg != "Please login to your SKYMCM account to use the AI Chatbot." {
				t.Fatalf("%s %s: message %q", name, c.path, msg)
			}
			// The browser can read the 401 (CORS with credentials).
			if res.Header.Get("Access-Control-Allow-Origin") != frontendOrigin || res.Header.Get("Access-Control-Allow-Credentials") != "true" {
				t.Fatalf("%s %s: CORS headers missing on 401", name, c.path)
			}
		}
	}
	if s.ai.last() != nil {
		t.Fatal("the AI model was called for a signed-out request")
	}
	if n := s.lookups.Load(); n != 0 {
		t.Fatalf("%d hotel lookups ran for signed-out requests", n)
	}
}

func TestSignedInChatWorks(t *testing.T) {
	s := newAuthServer(t, 30)
	tok := s.auth.issue(9001)

	res := s.do(t, http.MethodGet, "/api/v1/chatbot/session", "", cookie(tok))
	var sess struct {
		Data struct {
			Authenticated bool   `json:"authenticated"`
			AccountKey    string `json:"account_key"`
		} `json:"data"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&sess) != nil || !sess.Data.Authenticated || sess.Data.AccountKey == "" {
		t.Fatalf("session: %d %+v", res.StatusCode, sess)
	}
	res = s.do(t, http.MethodPost, "/api/v1/chatbot/message", `{"message":"Show me hotels in Varanasi"}`, cookie(tok))
	env := decode[messageEnvelope](t, res)
	if res.StatusCode != http.StatusOK || len(env.Data.Hotels) == 0 {
		t.Fatalf("hotel search: %d %q", res.StatusCode, env.Message)
	}
	res = s.do(t, http.MethodPost, "/api/v1/chatbot/message", `{"message":"tell me something about hotel rules"}`, cookie(tok))
	if res.StatusCode != http.StatusOK || s.ai.last() == nil {
		t.Fatalf("model reply: %d, model called %v", res.StatusCode, s.ai.last() != nil)
	}
	for _, p := range []string{"/api/v1/hotels", "/api/v1/hotels/101"} {
		if res := s.do(t, http.MethodGet, p, "", cookie(tok)); res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", p, res.StatusCode)
		}
	}
	// One check per token, reused for 30 seconds.
	if n := s.auth.calls.Load(); n != 1 {
		t.Fatalf("auth server asked %d times", n)
	}
	// Open: health checks and CORS preflights.
	if res := s.do(t, http.MethodGet, "/healthz", "", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", res.StatusCode)
	}
	pre := s.do(t, http.MethodOptions, "/api/v1/chatbot/message", "", map[string]string{"Origin": frontendOrigin, "Access-Control-Request-Method": "POST"})
	if pre.StatusCode != http.StatusNoContent || pre.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("preflight: %d", pre.StatusCode)
	}
}

func TestLogoutBlocksTheNextMessage(t *testing.T) {
	// Cache 0: a revocation (logout elsewhere) applies to the very next call.
	s := newAuthServer(t, 0)
	tok := s.auth.issue(9001)
	if res := s.do(t, http.MethodPost, "/api/v1/chatbot/message", `{"message":"Show me hotels in Varanasi"}`, cookie(tok)); res.StatusCode != http.StatusOK {
		t.Fatalf("before logout: %d", res.StatusCode)
	}
	s.auth.mu.Lock()
	s.auth.revoked[tok] = true
	s.auth.mu.Unlock()
	before := s.lookups.Load()
	if res := s.do(t, http.MethodPost, "/api/v1/chatbot/message", `{"message":"Show me hotels in Varanasi"}`, cookie(tok)); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", res.StatusCode)
	}
	// Logging out on the website also clears the cookie: no token at all.
	if res := s.do(t, http.MethodPost, "/api/v1/chatbot/message", `{"message":"hi"}`, cookie("")); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie cleared: %d", res.StatusCode)
	}
	if s.lookups.Load() != before {
		t.Fatal("a hotel search ran after logout")
	}
}

func TestAuthServerDownBlocksChat(t *testing.T) {
	s := newAuthServer(t, 30)
	tok := s.auth.issue(9001)
	s.auth.Close()
	for _, c := range protectedCalls {
		res := s.do(t, c.method, c.path, c.body, cookie(tok))
		if res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s %s with the auth server down: %d", c.method, c.path, res.StatusCode)
		}
	}
	if s.ai.last() != nil || s.lookups.Load() != 0 {
		t.Fatal("chat or hotel lookups ran while the login couldn't be checked")
	}
}

func TestBrokenAuthSettingFailsClosed(t *testing.T) {
	// A config that skipped validation (an invalid URL) must still refuse.
	cfg := &config.Config{AppEnv: config.EnvTest, CatalogSource: config.CatalogFile, CatalogFile: "../../testdata/catalog.json",
		AuthRequired: true, AuthURL: "not-a-url", AIProvider: "off", UploadsDir: t.TempDir()}
	store, err := catalog.LoadFile(cfg.CatalogFile)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Build(cfg, Sources{Hotels: store.Hotels(), RoomTypes: store.RoomTypes(), Landmarks: store.Landmarks()}, cache.New("")).Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/v1/hotels")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", res.StatusCode)
	}
}

func TestCrossSitePostIsRefused(t *testing.T) {
	s := newAuthServer(t, 30)
	tok := s.auth.issue(9001)
	h := cookie(tok)
	h["Origin"] = "https://evil.example"
	if res := s.do(t, http.MethodPost, "/api/v1/chatbot/message", `{"message":"hi"}`, h); res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", res.StatusCode)
	}
	if s.ai.last() != nil {
		t.Fatal("model called for a cross-site request")
	}
}
