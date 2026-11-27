package chatbot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/pkg/cache"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

func newTestMux(svc *Service, perMinute int) *http.ServeMux {
	mux := http.NewServeMux()
	RegisterRoutes(mux, NewHandler(svc, NewRateLimiter(cache.New(""), perMinute)))
	return mux
}

func post(mux http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chatbot/message", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:5000"
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHandlerSuccessShape(t *testing.T) {
	f := newFixture()
	f.ai.calls = []ToolCall{{Name: toolSearchHotels, Input: json.RawMessage(`{"city":"Haridwar"}`)}}
	f.ai.reply = "Here is one option.\n<meta intent=\"hotel_search\" needs_info=\"false\" needs_login=\"false\"/>"
	rec := post(newTestMux(f.svc, 20), `{"message":"Haridwar hotels","language":"en"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			ConversationID string      `json:"conversation_id"`
			Intent         string      `json:"intent"`
			Hotels         []HotelCard `json:"hotels"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// A plain destination request is answered with the results directly.
	if !body.Success || body.Message != "Sure! Here are some hotels available in Haridwar." || body.Data.Intent != IntentHotelSearch ||
		body.Data.ConversationID == "" || len(body.Data.Hotels) != 1 {
		t.Fatalf("body: %s", rec.Body)
	}
}

func TestHandlerInvalidRequests(t *testing.T) {
	f := newFixture()
	mux := newTestMux(f.svc, 20)
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"not json":     {`hello`, http.StatusBadRequest},
		"empty body":   {``, http.StatusBadRequest},
		"no message":   {`{"language":"en"}`, http.StatusBadRequest},
		"bad language": {`{"message":"hi","language":"xx"}`, http.StatusBadRequest},
		"too large":    {`{"message":"` + strings.Repeat("a", maxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		rec := post(mux, tc.body)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestHandlerDisabledAndFailures(t *testing.T) {
	svc := NewService(nil, NewConversationStore(cache.New("")), &fakeHotels{}, &fakeRooms{}, &fakeBookings{}, nil, time.Second)
	if rec := post(newTestMux(svc, 20), `{"message":"hi"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled: %d", rec.Code)
	}

	f := newFixture()
	f.ai.block = true
	f.svc.timeout = 20 * time.Millisecond
	if rec := post(newTestMux(f.svc, 20), `{"message":"hotel tips please"}`); rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout: %d %s", rec.Code, rec.Body)
	}

	f = newFixture()
	f.ai.err = io.ErrUnexpectedEOF
	rec := post(newTestMux(f.svc, 20), `{"message":"hotel tips please"}`)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "EOF") {
		t.Fatalf("provider error: %d %s", rec.Code, rec.Body)
	}
}

func TestHandlerRateLimit(t *testing.T) {
	f := newFixture()
	mux := newTestMux(f.svc, 2)
	for i := 0; i < 2; i++ {
		if rec := post(mux, `{"message":"hi"}`); rec.Code != http.StatusOK {
			t.Fatalf("message %d: %d", i, rec.Code)
		}
	}
	if rec := post(mux, `{"message":"hi"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third message: %d", rec.Code)
	}
}

func TestHandlerIgnoresInvalidToken(t *testing.T) {
	f := newFixture()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chatbot/message", strings.NewReader(`{"message":"my bookings"}`))
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	newTestMux(f.svc, 20).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	for _, n := range toolNames(f.ai.lastReq.Tools) {
		if n == toolListMyBookings {
			t.Fatal("invalid token unlocked booking tools")
		}
	}
}

// TestAnthropicProviderToolLoop runs the real SDK-based provider against a
// fake Messages API: one tool round, then a final answer.
func TestAnthropicProviderToolLoop(t *testing.T) {
	var n int32
	var secondBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" && r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("api key not sent")
		}
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&n, 1) == 1 {
			if !strings.Contains(string(b), `"search_hotels"`) || !strings.Contains(string(b), `"effort":"low"`) {
				t.Errorf("request missing tools/effort: %s", b)
			}
			io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
				"content":[{"type":"tool_use","id":"toolu_1","name":"search_hotels","input":{"city":"Haridwar"}}],
				"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`)
			return
		}
		secondBody = string(b)
		io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-5-5",
			"content":[{"type":"text","text":"Found Ganga View Residency."}],
			"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":8}}`)
	}))
	defer srv.Close()

	p := newAnthropicProvider("test-key", "claude-opus-5-5", 1024, 5*time.Second, option.WithBaseURL(srv.URL))
	var ran []string
	text, err := p.Reply(t.Context(), ProviderRequest{
		System:  "sys",
		History: []Turn{{Role: "user", Text: "hotels in Haridwar"}},
		Tools:   publicTools(),
		Execute: func(_ context.Context, c ToolCall) (string, bool) {
			ran = append(ran, c.Name+string(c.Input))
			return `{"total_matches":1}`, false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "Found Ganga View Residency." || len(ran) != 1 || ran[0] != `search_hotels{"city":"Haridwar"}` {
		t.Fatalf("text %q ran %v", text, ran)
	}
	if !strings.Contains(secondBody, `"tool_use_id":"toolu_1"`) {
		t.Fatalf("tool result not sent back: %s", secondBody)
	}
}

func TestAnthropicProviderErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.RawQuery, "refuse") {
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
	}))
	defer srv.Close()
	p := newAnthropicProvider("k", "claude-opus-5-5", 1024, 2*time.Second, option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
	_, err := p.Reply(t.Context(), ProviderRequest{History: []Turn{{Role: "user", Text: "hi"}}})
	if err == nil {
		t.Fatal("5xx not reported")
	}

	refusal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],
			"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer refusal.Close()
	p = newAnthropicProvider("k", "claude-opus-5-5", 1024, 2*time.Second, option.WithBaseURL(refusal.URL))
	if _, err := p.Reply(t.Context(), ProviderRequest{History: []Turn{{Role: "user", Text: "hi"}}}); err != ErrProviderRefused {
		t.Fatalf("want ErrProviderRefused, got %v", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	p = newAnthropicProvider("k", "claude-opus-5-5", 1024, 100*time.Millisecond, option.WithBaseURL(slow.URL), option.WithMaxRetries(0))
	start := time.Now()
	if _, err := p.Reply(t.Context(), ProviderRequest{History: []Turn{{Role: "user", Text: "hi"}}}); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("timeout not enforced: %v after %s", err, time.Since(start))
	}
}

func TestNewProviderConfig(t *testing.T) {
	if p, err := NewProvider(ProviderConfig{}); p != nil || err != nil {
		t.Fatal("empty config must disable the chatbot")
	}
	if p, err := NewProvider(ProviderConfig{Provider: "anthropic"}); p != nil || err != nil {
		t.Fatal("missing key must disable the chatbot")
	}
	if _, err := NewProvider(ProviderConfig{Provider: "acme", APIKey: "k"}); err == nil {
		t.Fatal("unknown provider accepted")
	}
	if p, err := NewProvider(ProviderConfig{Provider: "anthropic", APIKey: "k"}); p == nil || err != nil {
		t.Fatal("anthropic provider not built")
	}
	if p, err := NewProvider(ProviderConfig{Provider: "off"}); p != nil || err != nil {
		t.Fatal("AI_PROVIDER=off must disable the chatbot")
	}
	p, err := NewProvider(ProviderConfig{Provider: "ollama"}) // no key needed
	if op, ok := p.(*ollamaProvider); !ok || err != nil || op.model != "qwen2.5:1.5b" || op.chatURL != "http://localhost:11434/api/chat" {
		t.Fatalf("ollama provider not built with defaults: %+v %v", p, err)
	}
	if _, err := NewProvider(ProviderConfig{Provider: "ollama", BaseURL: "localhost:11434"}); err == nil {
		t.Fatal("base URL without scheme accepted")
	}
}
