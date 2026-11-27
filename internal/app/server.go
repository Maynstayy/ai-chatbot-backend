package app

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/pkg/utils"
)

// Run serves the API until SIGINT/SIGTERM, then shuts down gracefully,
// letting in-flight AI replies finish for up to the AI timeout.
func (a *App) Run() error {
	defer a.cache.Close()
	if a.db != nil {
		defer a.db.Close()
	}

	server := &http.Server{
		Addr:              ":" + a.cfg.AppPort,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// The chatbot handler extends its own write deadline to the AI
		// timeout (handler.go); everything else answers well within this.
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
		// A chat request is small; headers have no reason to be large.
		MaxHeaderBytes: 32 << 10,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", server.Addr, "env", a.cfg.AppEnv, "catalog", a.cfg.CatalogSource)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serveErr:
		return err
	case <-stop:
	}

	slog.Info("shutting down")
	grace := time.Duration(a.cfg.AITimeoutSeconds)*time.Second + 5*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		return err
	}
	slog.Info("server stopped")
	return nil
}

// corsHandler allows the configured browser origins (from MaynStay's
// internal/app/server.go, narrowed to the methods this API serves).
// Requests without an Origin header (server-to-server, curl) are unaffected:
// CORS only restricts what other websites' pages can read.
func corsHandler(origins []string, h http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[strings.TrimRight(strings.TrimSpace(o), "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Request-ID")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// securityHeaders are safe defaults for a JSON API.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/v1/chatbot/") {
			// Replies are per conversation: never cached.
			hd.Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}

// recoverer turns a handler panic into a logged 500 with the API's JSON
// error, instead of a dropped connection. The stack goes to the log only.
func recoverer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				slog.Error("panic", "path", r.URL.Path, "request_id", utils.RequestID(w), "stack", string(debug.Stack()))
				utils.Error(w, "Something went wrong on our end. Please try again in a moment.", http.StatusInternalServerError)
			}
		}()
		h.ServeHTTP(w, r)
	})
}
