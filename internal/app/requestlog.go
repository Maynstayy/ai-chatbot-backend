package app

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"time"
)

// requestIDHeader carries the id in both directions: a caller (or a proxy)
// may supply one, and every response echoes it, so a user's "something went
// wrong" can be matched to the exact server log lines.
const requestIDHeader = "X-Request-ID"

// Accept only short, plain ids from outside so a header can't inject odd
// characters into the logs.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// useJSONLogs makes slog (and the standard log package, which slog takes
// over) write one JSON object per line to stdout - systemd appends that to
// the app log file.
func useJSONLogs() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
}

// requestLog assigns each request an id and writes one access log line when
// it finishes: method, path, status, duration, client ip and request id.
func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)

		rw := &loggedResponse{ResponseWriter: w, status: http.StatusOK, id: id}
		start := time.Now()
		next.ServeHTTP(rw, r)

		level := slog.LevelInfo
		if rw.status >= 500 {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", clientIP(r),
			"request_id", id,
		)
	})
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// clientIP prefers X-Real-IP, which nginx sets from the real visitor, over
// the TCP peer (nginx itself in production).
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// loggedResponse records the status code and exposes the request id to
// error helpers (utils.RequestID) without changing their call sites.
type loggedResponse struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	id          string
}

func (l *loggedResponse) WriteHeader(status int) {
	if !l.wroteHeader {
		l.status, l.wroteHeader = status, true
	}
	l.ResponseWriter.WriteHeader(status)
}

func (l *loggedResponse) Write(b []byte) (int, error) {
	l.wroteHeader = true
	return l.ResponseWriter.Write(b)
}

func (l *loggedResponse) RequestID() string { return l.id }

// Unwrap lets http.ResponseController reach the underlying writer.
func (l *loggedResponse) Unwrap() http.ResponseWriter { return l.ResponseWriter }
