package chatbot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOllama answers /api/chat with the scripted responses in order (the
// last one repeats) and records every request body.
type fakeOllama struct {
	mu        sync.Mutex
	responses []string
	status    int
	requests  []ollamaChatRequest
}

func (f *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/chat" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "" {
		panic("ollama request must not carry credentials")
	}
	var req ollamaChatRequest
	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, &req); err != nil {
		panic("bad request body: " + err.Error())
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	i := min(len(f.requests), len(f.responses)) - 1
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.status != 0 {
		w.WriteHeader(f.status)
	}
	io.WriteString(w, f.responses[i])
}

func ollamaReply(content string) string {
	b, _ := json.Marshal(map[string]any{
		"model": "qwen2.5:1.5b", "done": true, "done_reason": "stop",
		"message": map[string]any{"role": "assistant", "content": content},
	})
	return string(b)
}

func ollamaToolReply(calls ...string) string {
	return `{"model":"qwen2.5:1.5b","done":true,"done_reason":"stop","message":{"role":"assistant","content":"","tool_calls":[` +
		strings.Join(calls, ",") + `]}}`
}

func newFakeOllama(t *testing.T, responses ...string) (*fakeOllama, *ollamaProvider) {
	t.Helper()
	f := &fakeOllama{responses: responses}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, newOllamaProvider(srv.URL+"/", "qwen2.5:1.5b", 512, 5*time.Second)
}

func TestOllamaProviderToolLoop(t *testing.T) {
	f, p := newFakeOllama(t,
		ollamaToolReply(`{"function":{"name":"search_hotels","arguments":{"city":"Haridwar","max_price":"3000","category":null}}}`),
		ollamaReply("Found Ganga View Residency.\n<meta intent=\"hotel_search\" needs_info=\"false\" needs_login=\"false\"/>"),
	)
	var ran []string
	text, err := p.Reply(t.Context(), ProviderRequest{
		System:  "sys",
		History: []Turn{{Role: "user", Text: "hi"}, {Role: "assistant", Text: "Hello!"}, {Role: "user", Text: "hotels in Haridwar"}},
		Tools:   publicTools(),
		Execute: func(_ context.Context, c ToolCall) (string, bool) {
			ran = append(ran, c.Name+string(c.Input))
			return `{"total_matches":1}`, false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "Found Ganga View Residency.") {
		t.Fatalf("text %q", text)
	}
	// null dropped, numeric string turned into a number
	if len(ran) != 1 || ran[0] != `search_hotels{"city":"Haridwar","max_price":3000}` {
		t.Fatalf("ran %v", ran)
	}

	first := f.requests[0]
	if first.Model != "qwen2.5:1.5b" || first.Stream || len(first.Tools) != 3 || first.Tools[0].Type != "function" {
		t.Fatalf("bad first request: %+v", first)
	}
	if first.Tools[0].Function.Parameters["type"] != "object" || first.Options["num_predict"] != float64(512) {
		t.Fatalf("bad tool schema/options: %+v", first)
	}
	roles := []string{}
	for _, m := range first.Messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,user" || !strings.HasPrefix(first.Messages[0].Content, "sys") {
		t.Fatalf("history not mapped: %v", roles)
	}

	second := f.requests[1].Messages
	last, prev := second[len(second)-1], second[len(second)-2]
	if last.Role != "tool" || last.ToolName != "search_hotels" || last.Content != `{"total_matches":1}` {
		t.Fatalf("tool result not sent back: %+v", last)
	}
	if prev.Role != "assistant" || len(prev.ToolCalls) != 1 || prev.ToolCalls[0].Function.Name != "search_hotels" {
		t.Fatalf("assistant tool call not echoed: %+v", prev)
	}
}

func TestOllamaProviderRejectsToolsNotOffered(t *testing.T) {
	f, p := newFakeOllama(t,
		ollamaToolReply(`{"function":{"name":"list_my_bookings","arguments":{}}}`, `{"function":{"name":"drop_tables","arguments":{}}}`),
		ollamaReply("Please log in to see bookings."),
	)
	executed := 0
	_, err := p.Reply(t.Context(), ProviderRequest{
		History: []Turn{{Role: "user", Text: "my bookings"}},
		Tools:   publicTools(), // anonymous caller: no booking tools
		Execute: func(context.Context, ToolCall) (string, bool) { executed++; return "{}", false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if executed != 0 {
		t.Fatal("a tool that was not offered was executed")
	}
	msgs := f.requests[1].Messages
	for _, m := range msgs[len(msgs)-2:] {
		if m.Role != "tool" || !strings.Contains(m.Content, "unknown tool") {
			t.Fatalf("want unknown-tool error, got %+v", m)
		}
	}
}

func TestOllamaProviderToolCallWrittenAsText(t *testing.T) {
	_, p := newFakeOllama(t,
		ollamaReply("<tool_call>\n{\"name\": \"get_hotel_details\", \"arguments\": {\"hotel_id\": 7}}\n</tool_call>"),
		ollamaReply("Hotel 7 allows pets."),
	)
	var ran []string
	text, err := p.Reply(t.Context(), ProviderRequest{
		History: []Turn{{Role: "user", Text: "pets at hotel 7?"}},
		Tools:   publicTools(),
		Execute: func(_ context.Context, c ToolCall) (string, bool) {
			ran = append(ran, c.Name+string(c.Input))
			return `{}`, false
		},
	})
	if err != nil || text != "Hotel 7 allows pets." || len(ran) != 1 || ran[0] != `get_hotel_details{"hotel_id":7}` {
		t.Fatalf("text %q err %v ran %v", text, err, ran)
	}

	// JSON naming an unknown tool is just text, never executed.
	_, p = newFakeOllama(t, ollamaReply(`{"name":"delete_booking","arguments":{"booking_id":1}}`))
	text, err = p.Reply(t.Context(), ProviderRequest{
		History: []Turn{{Role: "user", Text: "x"}},
		Tools:   publicTools(),
		Execute: func(context.Context, ToolCall) (string, bool) { t.Fatal("executed"); return "", false },
	})
	if err != nil || !strings.Contains(text, "delete_booking") {
		t.Fatalf("text %q err %v", text, err)
	}
}

func TestOllamaProviderLimits(t *testing.T) {
	call := `{"function":{"name":"search_hotels","arguments":{"city":"Goa"}}}`
	seven := ollamaToolReply(call, call, call, call, call, call, call)
	f, p := newFakeOllama(t, seven, seven, seven, seven, ollamaReply("Here is what I found."))
	executed := 0
	text, err := p.Reply(t.Context(), ProviderRequest{
		History: []Turn{{Role: "user", Text: "goa"}},
		Tools:   publicTools(),
		Execute: func(context.Context, ToolCall) (string, bool) { executed++; return "{}", false },
	})
	if err != nil || text != "Here is what I found." {
		t.Fatalf("text %q err %v", text, err)
	}
	if executed != maxToolRounds*maxCallsPerRound {
		t.Fatalf("executed %d tool calls, want %d", executed, maxToolRounds*maxCallsPerRound)
	}
	if len(f.requests) != maxToolRounds+1 || len(f.requests[maxToolRounds].Tools) != 0 {
		t.Fatalf("final round must withdraw tools: %d requests", len(f.requests))
	}

	// A model that keeps calling tools even without them is stopped.
	_, p = newFakeOllama(t, seven)
	_, err = p.Reply(t.Context(), ProviderRequest{
		History: []Turn{{Role: "user", Text: "goa"}},
		Tools:   publicTools(),
		Execute: func(context.Context, ToolCall) (string, bool) { return "{}", false },
	})
	if err == nil {
		t.Fatal("endless tool loop not stopped")
	}
}

func TestOllamaProviderErrors(t *testing.T) {
	req := ProviderRequest{History: []Turn{{Role: "user", Text: "hi"}}}

	f, p := newFakeOllama(t, `{"error":"model \"qwen9\" not found, try pulling it first"}`)
	f.status = http.StatusNotFound
	if _, err := p.Reply(t.Context(), req); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("404 not reported: %v", err)
	}

	f, p = newFakeOllama(t, `<html>bad gateway</html>`)
	f.status = http.StatusBadGateway
	if _, err := p.Reply(t.Context(), req); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("502 not reported: %v", err)
	}

	_, p = newFakeOllama(t, `{"message":`)
	if _, err := p.Reply(t.Context(), req); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed JSON not reported: %v", err)
	}

	_, p = newFakeOllama(t, `{"error":"out of memory"}`)
	if _, err := p.Reply(t.Context(), req); err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("ollama error not reported: %v", err)
	}

	_, p = newFakeOllama(t, ollamaReply("  "))
	if _, err := p.Reply(t.Context(), req); !errors.Is(err, ErrProviderEmpty) {
		t.Fatalf("want ErrProviderEmpty, got %v", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	p = newOllamaProvider(slow.URL, "m", 64, 100*time.Millisecond)
	start := time.Now()
	if _, err := p.Reply(t.Context(), req); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("timeout not enforced: %v after %s", err, time.Since(start))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	p = newOllamaProvider(slow.URL, "m", 64, 10*time.Second)
	if _, err := p.Reply(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request context deadline not reported: %v", err)
	}
}

func TestNormalizeToolArgs(t *testing.T) {
	spec := publicTools()[2] // check_room_availability
	for _, tc := range []struct{ in, want string }{
		{``, `{}`},
		{`null`, `{}`},
		{`"{\"hotel_id\":\"4\",\"check_in\":\"2026-12-01\"}"`, `{"check_in":"2026-12-01","hotel_id":4}`},
		{`{"hotel_id":4,"guests":"","check_out":" "}`, `{"hotel_id":4}`},
		{`{"hotel_id":"four"}`, `{"hotel_id":"four"}`}, // left for the executor to reject
	} {
		got, err := normalizeToolArgs(json.RawMessage(tc.in), spec)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s: got %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	if _, err := normalizeToolArgs(json.RawMessage(`[1,2]`), spec); err == nil {
		t.Error("non-object arguments accepted")
	}
}
