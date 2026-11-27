// Package app wires the standalone chatbot backend: the hotel catalogue
// (file or MySQL), the AI chatbot from MaynStay's feature/ai-chatbot branch,
// and the HTTP stack.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/catalog"
	"github.com/Maynstayy/ai-chatbot-backend/internal/chatbot"
	"github.com/Maynstayy/ai-chatbot-backend/internal/config"
	"github.com/Maynstayy/ai-chatbot-backend/internal/database"
	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
	"github.com/Maynstayy/ai-chatbot-backend/internal/httpx"
	"github.com/Maynstayy/ai-chatbot-backend/internal/landmarks"
	"github.com/Maynstayy/ai-chatbot-backend/internal/roomtypes"
	"github.com/Maynstayy/ai-chatbot-backend/internal/skymcm"
	"github.com/Maynstayy/ai-chatbot-backend/pkg/cache"
	"github.com/Maynstayy/ai-chatbot-backend/pkg/utils"
)

// App is the wired API.
type App struct {
	cfg       *config.Config
	db        *sql.DB // nil with the file catalogue
	cache     *cache.Cache
	mux       *http.ServeMux
	aiEnabled bool
}

// Sources is where hotel data comes from: repositories for the local test
// catalogues (file, mysql), or ready services for the SKYMCM API, whose
// prices are final and must not be repriced.
type Sources struct {
	Hotels    hotels.Repository
	RoomTypes roomtypes.Repository
	Landmarks landmarks.Repository
	DB        *sql.DB // set for MySQL, for the readiness check

	SKYMCM *skymcm.Client // set for CATALOG_SOURCE=skymcm
}

// New validates the configuration, opens the catalogue and wires everything.
func New(cfg *config.Config) (*App, error) {
	useJSONLogs()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if bad := httpx.SetTrustedProxies(cfg.TrustedProxies); len(bad) > 0 {
		return nil, fmt.Errorf("configuration: TRUSTED_PROXIES has invalid entries: %v", bad)
	}
	src, err := openSources(cfg)
	if err != nil {
		return nil, err
	}
	// Redis is optional: without REDIS_URL, conversations and rate limits
	// live in process memory (fine for one instance).
	return Build(cfg, src, cache.New(cfg.RedisURL)), nil
}

func openSources(cfg *config.Config) (Sources, error) {
	switch cfg.CatalogSource {
	case config.CatalogSKYMCM:
		client, err := skymcm.New(skymcm.Config{
			BaseURL:          cfg.SKYMCMAPIURL,
			AssetURL:         cfg.SKYMCMAssetURL,
			Timeout:          time.Duration(cfg.SKYMCMTimeoutSeconds) * time.Second,
			CacheTTL:         time.Duration(cfg.SKYMCMCacheSeconds) * time.Second,
			RoomCacheTTL:     time.Duration(cfg.SKYMCMRoomCacheSecs) * time.Second,
			LandmarkCacheTTL: time.Duration(cfg.SKYMCMLandmarkSecs) * time.Second,
			MaxConcurrent:    cfg.SKYMCMMaxConcurrency,
		})
		if err != nil {
			return Sources{}, err
		}
		// Only the host is logged; the catalogue is fetched on first use.
		slog.Info("hotel catalogue: SKYMCM public API", "host", hostOf(cfg.SKYMCMAPIURL))
		return Sources{SKYMCM: client}, nil
	case config.CatalogMySQL:
		db, err := database.Open(context.Background(), database.Config{
			User: cfg.DBUser, Password: cfg.DBPassword, Host: cfg.DBHost, Port: cfg.DBPort, Name: cfg.DBName,
		})
		if err != nil {
			return Sources{}, fmt.Errorf("database: %w", err)
		}
		return Sources{
			Hotels: hotels.NewRepository(db), RoomTypes: roomtypes.NewRepository(db),
			Landmarks: landmarks.NewRepository(db), DB: db,
		}, nil
	default:
		store, err := catalog.LoadFile(cfg.CatalogFile)
		if err != nil {
			return Sources{}, err
		}
		slog.Info("hotel catalogue loaded from file (test data)", "file", cfg.CatalogFile,
			"hotels", len(store.Data().Hotels))
		return Sources{Hotels: store.Hotels(), RoomTypes: store.RoomTypes(), Landmarks: store.Landmarks()}, nil
	}
}

// Build wires every module onto a fresh ServeMux. Tests pass a file-backed
// catalogue and an empty cache.
func Build(cfg *config.Config, src Sources, redisCache *cache.Cache) *App {
	mux := http.NewServeMux()

	var (
		hotelService    hotels.Service
		roomTypeService roomtypes.Service
		landmarkService landmarks.Service
	)
	if src.SKYMCM != nil {
		// The website's own prices, availability and listings, as returned.
		hotelService, roomTypeService, landmarkService = src.SKYMCM, src.SKYMCM, src.SKYMCM
	} else {
		hotelService = hotels.NewService(src.Hotels)
		roomTypeService = roomtypes.NewService(src.RoomTypes)
		landmarkService = landmarks.NewService(src.Landmarks)
	}

	// PUBLIC CATALOGUE - the read-only endpoints the chat frontend's guided
	// search uses (same paths and JSON as MaynStay).
	hotels.RegisterRoutes(mux, hotels.NewHandler(hotelService))
	roomtypes.RegisterRoutes(mux, roomtypes.NewHandler(roomTypeService))

	// CHATBOT - the AI assistant from MaynStay. Reads hotels, room
	// availability and landmarks through the services above; never writes.
	// Local Ollama by default (AI_PROVIDER=ollama, no key) or Claude
	// (AI_PROVIDER=anthropic + AI_API_KEY); AI_PROVIDER=off answers 503.
	aiTimeout := time.Duration(cfg.AITimeoutSeconds) * time.Second
	aiProvider, err := chatbot.NewProvider(chatbot.ProviderConfig{
		Provider:  cfg.AIProvider,
		APIKey:    cfg.AIAPIKey,
		Model:     cfg.AIModel,
		BaseURL:   cfg.AIBaseURL,
		Timeout:   aiTimeout,
		MaxTokens: cfg.AIMaxTokens,
	})
	if err != nil {
		slog.Error("chatbot disabled", "err", err.Error())
	}
	if aiProvider == nil {
		slog.Warn("AI provider not configured: POST /api/v1/chatbot/message will answer 503", "provider", cfg.AIProvider)
	} else {
		slog.Info("AI provider configured", "provider", cfg.AIProvider, "model", cfg.AIModel)
	}

	// Guests only: conversations live in the short-lived store (Redis or
	// memory, 30 minutes idle). No booking or payment source is wired: the
	// standalone backend has no login, so booking tools are never offered.
	chatService := chatbot.NewService(
		aiProvider,
		chatbot.NewConversationStore(redisCache),
		hotelService,
		roomTypeService,
		nil, // bookings
		nil, // payments
		aiTimeout,
	)
	chatService.SetDebug(cfg.ChatbotDebug)
	chatService.SetLandmarks(landmarkService)
	chatService.SetSiteURL(cfg.SiteURL)
	chatService.SetSupport(chatbot.SupportInfo{Phone: cfg.SupportPhone, Email: cfg.SupportEmail, WhatsApp: cfg.SupportWhatsApp})
	chatbot.RegisterRoutes(mux, chatbot.NewHandler(chatService, chatbot.NewRateLimiter(redisCache, cfg.ChatbotRatePerMin)))

	// HEALTH - liveness and readiness (no secrets in either).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		utils.WriteJSONBody(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"status": "ok", "catalog": cfg.CatalogSource, "ai": aiStatus(aiProvider != nil)}
		if src.SKYMCM != nil {
			// Last known state only: a readiness probe never calls the website.
			body["skymcm"] = src.SKYMCM.Status()
		}
		if src.DB != nil {
			if err := database.Ping(r.Context(), src.DB); err != nil {
				slog.Error("readiness: database ping failed", "err", err.Error())
				body["status"], body["database"] = "unavailable", "down"
				utils.WriteJSONBody(w, http.StatusServiceUnavailable, body)
				return
			}
			body["database"] = "up"
		}
		utils.WriteJSONBody(w, http.StatusOK, body)
	})

	// UPLOADS - hotel photos stored as /uploads/... paths, when a directory
	// is configured and present.
	if st, err := os.Stat(cfg.UploadsDir); err == nil && st.IsDir() {
		mux.Handle("GET /uploads/", uploadedFiles(cfg.UploadsDir))
	}

	return &App{cfg: cfg, db: src.DB, cache: redisCache, mux: mux, aiEnabled: aiProvider != nil}
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

func aiStatus(on bool) string {
	if on {
		return "configured"
	}
	return "disabled"
}

// Handler is the full HTTP stack: request id + access log, panic recovery,
// security headers, CORS, then the routes (with JSON 404/405).
func (a *App) Handler() http.Handler {
	return requestLog(recoverer(securityHeaders(corsHandler(a.cfg.CORSOrigins(), jsonFallback(a.mux)))))
}

// jsonFallback serves requests normally, except that when no route matches
// it replaces the mux's plain-text 404/405 page with the API's JSON error.
// (From MaynStay's internal/app.)
func jsonFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		rec := &captureWriter{header: http.Header{}, status: http.StatusOK}
		mux.ServeHTTP(rec, r)
		switch rec.status {
		case http.StatusNotFound:
			utils.Error(w, "not found", http.StatusNotFound)
		case http.StatusMethodNotAllowed:
			w.Header().Set("Allow", rec.header.Get("Allow"))
			utils.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		default: // e.g. the mux's trailing-slash redirect
			for k, v := range rec.header {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.status)
			w.Write(rec.body)
		}
	})
}

// captureWriter buffers a response so jsonFallback can rewrite it.
type captureWriter struct {
	header http.Header
	status int
	body   []byte
}

func (c *captureWriter) Header() http.Header    { return c.header }
func (c *captureWriter) WriteHeader(status int) { c.status = status }
func (c *captureWriter) Write(b []byte) (int, error) {
	c.body = append(c.body, b...)
	return len(b), nil
}

// uploadedFiles serves /uploads/. Files are content-hash-named, so they never
// change under a given URL - browsers may cache them for a year.
func uploadedFiles(dir string) http.Handler {
	fs := http.StripPrefix("/uploads/", http.FileServer(http.Dir(dir)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fs.ServeHTTP(w, r)
	})
}
