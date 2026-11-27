package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Maynstayy/ai-chatbot-backend/pkg/utils"
)

// captureLogs routes slog to a buffer for the test and returns the parsed
// JSON lines.
func captureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil {
				out = append(out, m)
			}
		}
		return out
	}
}

func TestRequestIDIsAssignedAndEchoed(t *testing.T) {
	h := requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if id := rec.Header().Get(requestIDHeader); len(id) != 16 {
		t.Fatalf("generated id %q, want 16 hex chars", id)
	}

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(requestIDHeader, "edge-abc.123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get(requestIDHeader); got != "edge-abc.123" {
		t.Fatalf("incoming id not reused: %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(requestIDHeader, "bad id\nwith newline")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get(requestIDHeader); strings.ContainsAny(got, " \n") || len(got) != 16 {
		t.Fatalf("unsafe incoming id should be replaced, got %q", got)
	}
}

func TestAccessLogAndErrorShareRequestID(t *testing.T) {
	logs := captureLogs(t)
	h := requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		utils.ServerError(w, "bookings.create", errors.New("dial tcp 127.0.0.1:3306: connection refused"))
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/bookings", nil)
	req.Header.Set("X-Real-IP", "203.0.113.9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	id := rec.Header().Get(requestIDHeader)
	var internal, access map[string]any
	for _, l := range logs() {
		switch l["msg"] {
		case "internal error":
			internal = l
		case "request":
			access = l
		}
	}
	if internal == nil || access == nil {
		t.Fatalf("missing log lines: %v", logs())
	}
	if internal["request_id"] != id || access["request_id"] != id {
		t.Fatalf("request ids differ: header %q, error %v, access %v", id, internal["request_id"], access["request_id"])
	}
	if internal["where"] != "bookings.create" || !strings.Contains(internal["err"].(string), "connection refused") {
		t.Fatalf("internal error line = %v", internal)
	}
	if access["status"] != float64(500) || access["method"] != "POST" || access["path"] != "/api/v1/bookings" || access["ip"] != "203.0.113.9" || access["level"] != "ERROR" {
		t.Fatalf("access line = %v", access)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatal("internal error leaked to the client")
	}
}
