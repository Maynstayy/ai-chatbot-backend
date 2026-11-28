package session

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
)

// jwtLike builds a token shaped like the website's (header.payload.sig);
// only the mock auth server below decides whether it is valid.
func jwtLike(id int64, exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(map[string]any{"signup_id": id, "exp": exp.Unix()})
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(payload) + ".c2lnbmF0dXJlLW9mLWEtdGVzdA"
}

// authServer is a local stand-in for the website's profile endpoint.
type authServer struct {
	*httptest.Server
	calls   atomic.Int32
	mu      sync.Mutex
	valid   map[string]int64
	revoked map[string]bool
	status  int // forced status when non-zero
}

func newAuthServer(t *testing.T) *authServer {
	a := &authServer{valid: map[string]int64{}, revoked: map[string]bool{}}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.calls.Add(1)
		if r.URL.Path != ProfilePath || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.status != 0 {
			w.WriteHeader(a.status)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		id, ok := a.valid[tok]
		if !ok || a.revoked[tok] {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"Unauthorized"}`)
			return
		}
		fmt.Fprintf(w, `{"success":true,"message":"ok","data":{"signup_id":%d,"full_name":"Test","email":"t@example.test","phone":"0"}}`, id)
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *authServer) add(tok string, id int64) {
	a.mu.Lock()
	a.valid[tok] = id
	a.mu.Unlock()
}

func (a *authServer) revoke(tok string) {
	a.mu.Lock()
	a.revoked[tok] = true
	a.mu.Unlock()
}

func newValidator(t *testing.T, url string, ttl time.Duration) *Validator {
	v, err := New(Config{AuthURL: url, CacheTTL: ttl, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// protected is the middleware around a handler that records whether it ran.
func protected(v *Validator) (http.Handler, *atomic.Int32) {
	var ran atomic.Int32
	h := v.Middleware([]string{"http://localhost:3000"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran.Add(1)
		a, _ := FromContext(r.Context())
		fmt.Fprintf(w, `{"account":%d}`, a.ID)
	}))
	return h, &ran
}

func request(h http.Handler, method, cookie, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1/chatbot/message", strings.NewReader(`{"message":"hi"}`))
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "maynstay_token", Value: cookie})
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct{ Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %q", rec.Body.String())
	}
	return body.Error
}

func TestValidSessionReachesTheHandler(t *testing.T) {
	auth := newAuthServer(t)
	tok := jwtLike(9001, time.Now().Add(time.Hour))
	auth.add(tok, 9001)
	h, ran := protected(newValidator(t, auth.URL, 30*time.Second))

	rec := request(h, http.MethodPost, tok, "http://localhost:3000")
	if rec.Code != http.StatusOK || ran.Load() != 1 || !strings.Contains(rec.Body.String(), `"account":9001`) {
		t.Fatalf("status %d ran %d body %s", rec.Code, ran.Load(), rec.Body.String())
	}
	// Bearer works too (local tools), and the cache answers the repeat.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hotels", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || auth.calls.Load() != 1 {
		t.Fatalf("bearer: status %d, auth calls %d", rec.Code, auth.calls.Load())
	}
}

func TestRejectedSessionsAre401AndNothingRuns(t *testing.T) {
	auth := newAuthServer(t)
	revoked := jwtLike(9003, time.Now().Add(time.Hour))
	auth.add(revoked, 9003)
	auth.revoke(revoked)
	expired := jwtLike(9004, time.Now().Add(-time.Minute))
	auth.add(expired, 9004)
	h, ran := protected(newValidator(t, auth.URL, 30*time.Second))

	for name, tok := range map[string]string{
		"missing":   "",
		"invalid":   jwtLike(1234, time.Now().Add(time.Hour)), // unknown to the auth server
		"malformed": "not a token!",
		"expired":   expired,
		"revoked":   revoked,
	} {
		rec := request(h, http.MethodPost, tok, "")
		if rec.Code != http.StatusUnauthorized || errorOf(t, rec) != LoginRequiredText {
			t.Errorf("%s: status %d body %s", name, rec.Code, rec.Body.String())
		}
	}
	if ran.Load() != 0 {
		t.Fatalf("handler ran %d times for rejected sessions", ran.Load())
	}
	// Expired and malformed are refused without asking the auth server.
	if n := auth.calls.Load(); n != 2 {
		t.Fatalf("auth server asked %d times, want 2 (invalid, revoked)", n)
	}
}

func TestAuthServerDownFailsClosed(t *testing.T) {
	auth := newAuthServer(t)
	tok := jwtLike(9001, time.Now().Add(time.Hour))
	auth.add(tok, 9001)
	v := newValidator(t, auth.URL, 30*time.Second)
	h, ran := protected(v)

	auth.mu.Lock()
	auth.status = http.StatusInternalServerError
	auth.mu.Unlock()
	if rec := request(h, http.MethodPost, tok, ""); rec.Code != http.StatusServiceUnavailable || errorOf(t, rec) != UnavailableText {
		t.Fatalf("5xx: status %d", rec.Code)
	}
	// Not cached: once the server answers again, the same token works.
	auth.mu.Lock()
	auth.status = 0
	auth.mu.Unlock()
	if rec := request(h, http.MethodPost, tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("recovered: status %d", rec.Code)
	}
	// Unreachable server.
	other := jwtLike(9002, time.Now().Add(time.Hour))
	auth.Close()
	if rec := request(h, http.MethodPost, other, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable: status %d", rec.Code)
	}
	if ran.Load() != 1 {
		t.Fatalf("handler ran %d times, want only the recovered request", ran.Load())
	}
}

func TestCacheTTLAndRevocation(t *testing.T) {
	auth := newAuthServer(t)
	tok := jwtLike(9001, time.Now().Add(time.Hour))
	auth.add(tok, 9001)
	v := newValidator(t, auth.URL, 30*time.Second)
	now := time.Now()
	v.now = func() time.Time { return now }
	h, _ := protected(v)

	request(h, http.MethodGet, tok, "")
	auth.revoke(tok) // logout elsewhere
	if rec := request(h, http.MethodGet, tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("within the cache window: %d", rec.Code)
	}
	now = now.Add(31 * time.Second)
	if rec := request(h, http.MethodGet, tok, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after the cache window a revoked token must be refused: %d", rec.Code)
	}

	// TTL 0: every request asks, so revocation applies at once.
	tok2 := jwtLike(9002, time.Now().Add(time.Hour))
	auth.add(tok2, 9002)
	v0 := newValidator(t, auth.URL, 0)
	h0, _ := protected(v0)
	if rec := request(h0, http.MethodGet, tok2, ""); rec.Code != http.StatusOK {
		t.Fatalf("ttl 0 valid: %d", rec.Code)
	}
	auth.revoke(tok2)
	if rec := request(h0, http.MethodGet, tok2, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ttl 0 revoked: %d", rec.Code)
	}
}

func TestCacheNeverOutlivesTheTokenExpiry(t *testing.T) {
	auth := newAuthServer(t)
	exp := time.Now().Add(10 * time.Second)
	tok := jwtLike(9001, exp)
	auth.add(tok, 9001)
	v := newValidator(t, auth.URL, 30*time.Second)
	now := time.Now()
	v.now = func() time.Time { return now }
	if _, err := v.Check(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	now = exp.Add(time.Second)
	if _, err := v.Check(context.Background(), tok); err != ErrUnauthorized {
		t.Fatalf("expired token still accepted: %v", err)
	}
}

func TestCrossSiteWritesAreRefused(t *testing.T) {
	auth := newAuthServer(t)
	tok := jwtLike(9001, time.Now().Add(time.Hour))
	auth.add(tok, 9001)
	h, ran := protected(newValidator(t, auth.URL, 30*time.Second))
	if rec := request(h, http.MethodPost, tok, "https://evil.example"); rec.Code != http.StatusForbidden || ran.Load() != 0 {
		t.Fatalf("cross-site POST: %d", rec.Code)
	}
	// Preflights pass through untouched (the CORS layer answers them).
	if rec := request(h, http.MethodOptions, "", "https://evil.example"); rec.Code != http.StatusOK || ran.Load() != 1 {
		t.Fatalf("OPTIONS: %d", rec.Code)
	}
}

func TestConcurrentChecksShareOneRequest(t *testing.T) {
	auth := newAuthServer(t)
	tok := jwtLike(9001, time.Now().Add(time.Hour))
	auth.add(tok, 9001)
	v := newValidator(t, auth.URL, 30*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Check(context.Background(), tok); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := auth.calls.Load(); n != 1 {
		t.Fatalf("auth server asked %d times", n)
	}
}

func TestSessionHandlerAndAccountKey(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/chatbot/session", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no account: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chatbot/session", nil)
	Handler(rec, req.WithContext(WithAccount(req.Context(), Account{ID: 9001})))
	var body struct {
		Data struct {
			Authenticated bool   `json:"authenticated"`
			AccountKey    string `json:"account_key"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.Data.Authenticated || len(body.Data.AccountKey) != 24 {
		t.Fatalf("signed in: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "9001") || (Account{ID: 9001}).Key() == (Account{ID: 9002}).Key() {
		t.Fatal("account key must be opaque and distinct per account")
	}
}
