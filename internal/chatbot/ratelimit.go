package chatbot

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/httpx"
	"github.com/Maynstayy/ai-chatbot-backend/pkg/cache"
)

const rateWindow = time.Minute

// RateLimiter is a fixed-window message counter per caller: Redis when
// configured (shared across instances), process memory otherwise.
type RateLimiter struct {
	redis  *cache.Cache
	limit  int
	mu     sync.Mutex
	counts map[string]*window
	now    func() time.Time
}

type window struct {
	start time.Time
	n     int
}

func NewRateLimiter(redis *cache.Cache, perMinute int) *RateLimiter {
	if perMinute <= 0 {
		perMinute = 20
	}
	return &RateLimiter{redis: redis, limit: perMinute, counts: map[string]*window{}, now: time.Now}
}

// Allow counts one message for key and reports whether it is within the limit.
func (l *RateLimiter) Allow(ctx context.Context, key string) bool {
	limit := l.limit
	if key == "ip:unknown" {
		// Callers whose address is unknown share one bucket; give it room.
		limit *= 5
	}
	if l.redis.Enabled() {
		if n, err := l.redis.IncrWindow(ctx, "chatbot:rate:"+key, rateWindow); err == nil {
			return n <= int64(limit)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.counts) > 10000 {
		for k, w := range l.counts {
			if now.Sub(w.start) > rateWindow {
				delete(l.counts, k)
			}
		}
	}
	w := l.counts[key]
	if w == nil || now.Sub(w.start) > rateWindow {
		w = &window{start: now}
		l.counts[key] = w
	}
	w.n++
	return w.n <= limit
}

// rateKey identifies the caller: the signed-in user, else the client IP.
func rateKey(userID int64, r *http.Request) string {
	if userID > 0 {
		return "user:" + strconv.FormatInt(userID, 10)
	}
	if ip := httpx.ClientIP(r); ip != "" {
		return "ip:" + ip
	}
	return "ip:unknown"
}
