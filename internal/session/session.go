// Package session requires a signed-in SKYMCM account for the chatbot API.
//
// The SKYMCM website keeps its login in the HttpOnly "maynstay_token" cookie.
// The browser sends that cookie with the chatbot's API requests; this package
// asks the website's API whether it is still a valid session (signature,
// expiry and logout revocation are all checked there, by
// GET /api/v1/users/profile) and caches the answer briefly. The token itself
// is never logged or stored, only a SHA-256 hash of it as the cache key.
//
// Anything other than a clear "valid" fails closed: a missing, malformed,
// expired, invalid or revoked token is 401, and an auth server that can't be
// reached is 503. Nothing behind the middleware runs in either case.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/pkg/utils"
)

// LoginRequiredText is the only thing a signed-out caller is told.
const LoginRequiredText = "Please login to your SKYMCM account to use the AI Chatbot."

// UnavailableText is returned when the login can't be checked at all.
const UnavailableText = "We couldn't verify your SKYMCM login right now. Please try again shortly."

// ProfilePath is the website endpoint that answers 200 only for a valid,
// unrevoked session.
const ProfilePath = "/api/v1/users/profile"

var (
	// ErrUnauthorized: no session, or one the website rejects.
	ErrUnauthorized = errors.New("session: not signed in")
	// ErrUnavailable: the website's answer couldn't be obtained.
	ErrUnavailable = errors.New("session: login check unavailable")
)

// Account is the signed-in SKYMCM account. Only the id is kept; the
// profile's name, email and phone are discarded as soon as they are read.
type Account struct {
	ID int64
}

// Key is an opaque, stable identifier for the account, for the browser to
// keep its saved chats apart. It isn't the account id itself.
func (a Account) Key() string {
	sum := sha256.Sum256([]byte("skymcm-chatbot-account:" + strconv.FormatInt(a.ID, 10)))
	return hex.EncodeToString(sum[:])[:24]
}

type ctxKey struct{}

// FromContext returns the account the middleware verified for this request.
func FromContext(ctx context.Context) (Account, bool) {
	a, ok := ctx.Value(ctxKey{}).(Account)
	return a, ok && a.ID > 0
}

// WithAccount marks a context as signed in (for the middleware and tests).
func WithAccount(ctx context.Context, a Account) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// Config configures the validator.
type Config struct {
	// AuthURL is the website API's base URL (the profile path is appended).
	AuthURL string
	// CookieName is the website's login cookie.
	CookieName string
	// CacheTTL is how long an answer is reused; 0 asks on every request.
	CacheTTL time.Duration
	// Timeout bounds one check.
	Timeout time.Duration
	// Client overrides the HTTP client (tests).
	Client *http.Client
}

type entry struct {
	account Account
	err     error // nil or ErrUnauthorized; ErrUnavailable is never cached
	expires time.Time
}

type call struct {
	done    chan struct{}
	account Account
	err     error
}

// Validator checks sessions against the website.
type Validator struct {
	profileURL string
	cookie     string
	ttl        time.Duration
	client     *http.Client
	now        func() time.Time

	mu       sync.Mutex
	cache    map[string]entry
	inflight map[string]*call
}

const maxCacheEntries = 10000

// New builds a validator. AuthURL must be an absolute http(s) URL; the
// configuration package decides which hosts are allowed.
func New(cfg Config) (*Validator, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.AuthURL), "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, errors.New("session: auth URL must be an absolute http(s) URL")
	}
	cookie := strings.TrimSpace(cfg.CookieName)
	if cookie == "" {
		cookie = "maynstay_token"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{
			Timeout: timeout,
			// A redirect is never a valid answer from the profile endpoint.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	ttl := cfg.CacheTTL
	if ttl < 0 {
		ttl = 0
	}
	return &Validator{
		profileURL: base + ProfilePath, cookie: cookie, ttl: ttl, client: client, now: time.Now,
		cache: map[string]entry{}, inflight: map[string]*call{},
	}, nil
}

// tokenShape is what a website token can look like (a JWT is three base64url
// parts); anything else is rejected without asking the website.
var tokenShape = regexp.MustCompile(`^[A-Za-z0-9_\-]+(\.[A-Za-z0-9_\-]+){0,2}$`)

// Token reads the login token from the website's cookie, or from an
// "Authorization: Bearer" header (for local testing tools). Empty when there
// is none or it isn't token-shaped.
func (v *Validator) Token(r *http.Request) string {
	tok := ""
	if c, err := r.Cookie(v.cookie); err == nil {
		tok = c.Value
	}
	if tok == "" {
		if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			tok = strings.TrimSpace(h[7:])
		}
	}
	if len(tok) < 16 || len(tok) > 4096 || !tokenShape.MatchString(tok) {
		return ""
	}
	return tok
}

// expiry reads a JWT's exp claim without verifying it: only to refuse a
// token that has plainly expired and to never cache past its expiry. The
// website's answer is what decides validity.
func expiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == "" {
		return time.Time{}, false
	}
	n, err := claims.Exp.Int64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// Check validates a token: the account, ErrUnauthorized or ErrUnavailable.
func (v *Validator) Check(ctx context.Context, token string) (Account, error) {
	if token == "" {
		return Account{}, ErrUnauthorized
	}
	now := v.now()
	exp, hasExp := expiry(token)
	if hasExp && !exp.After(now) {
		return Account{}, ErrUnauthorized
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])

	v.mu.Lock()
	if e, ok := v.cache[key]; ok {
		if now.Before(e.expires) {
			v.mu.Unlock()
			return e.account, e.err
		}
		delete(v.cache, key)
	}
	// One website request per token at a time; concurrent checks share it.
	if c, ok := v.inflight[key]; ok {
		v.mu.Unlock()
		select {
		case <-c.done:
			return c.account, c.err
		case <-ctx.Done():
			return Account{}, ErrUnavailable
		}
	}
	c := &call{done: make(chan struct{})}
	v.inflight[key] = c
	v.mu.Unlock()

	c.account, c.err = v.ask(ctx, token)

	v.mu.Lock()
	delete(v.inflight, key)
	if v.ttl > 0 && !errors.Is(c.err, ErrUnavailable) {
		until := v.now().Add(v.ttl)
		if hasExp && exp.Before(until) {
			until = exp
		}
		if len(v.cache) >= maxCacheEntries {
			v.sweep()
		}
		v.cache[key] = entry{account: c.account, err: c.err, expires: until}
	}
	v.mu.Unlock()
	close(c.done)
	return c.account, c.err
}

// sweep drops expired entries, or everything if none had expired. Called
// with the lock held.
func (v *Validator) sweep() {
	now := v.now()
	for k, e := range v.cache {
		if !now.Before(e.expires) {
			delete(v.cache, k)
		}
	}
	if len(v.cache) >= maxCacheEntries {
		v.cache = map[string]entry{}
	}
}

// ask is one request to the website's profile endpoint.
func (v *Validator) ask(ctx context.Context, token string) (Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.profileURL, nil)
	if err != nil {
		return Account{}, ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	res, err := v.client.Do(req)
	if err != nil {
		slog.Warn("session: login check failed", "err", errorKind(err))
		return Account{}, ErrUnavailable
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return Account{}, ErrUnauthorized
	case res.StatusCode != http.StatusOK || err != nil:
		slog.Warn("session: login check answered unexpectedly", "status", res.StatusCode)
		return Account{}, ErrUnavailable
	}
	id := signupID(body)
	if id <= 0 {
		slog.Warn("session: login check returned no account")
		return Account{}, ErrUnavailable
	}
	return Account{ID: id}, nil
}

// signupID reads the account id from the profile, enveloped
// ({"data":{"signup_id":..}}) or not. Nothing else is kept.
func signupID(body []byte) int64 {
	var p struct {
		SignupID int64 `json:"signup_id"`
		Data     *struct {
			SignupID int64 `json:"signup_id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &p) != nil {
		return 0
	}
	if p.Data != nil && p.Data.SignupID > 0 {
		return p.Data.SignupID
	}
	return p.SignupID
}

// errorKind names a transport failure without its URL (which would show the
// auth host's internals in logs).
func errorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "unreachable"
}

// Middleware requires a valid session for every request it wraps. CORS
// preflights pass (they carry no cookies and run nothing). A state-changing
// request from a browser page must come from an allowed origin, so another
// site can't use the visitor's cookie.
func (v *Validator) Middleware(allowedOrigins []string, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range allowedOrigins {
		allowed[strings.TrimRight(strings.TrimSpace(o), "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && r.Method != http.MethodGet && r.Method != http.MethodHead && !allowed[origin] {
			Refuse(w, r, "This request isn't allowed from that site.", http.StatusForbidden)
			return
		}
		account, err := v.Check(r.Context(), v.Token(r))
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case err == nil:
			next.ServeHTTP(w, r.WithContext(WithAccount(r.Context(), account)))
		case errors.Is(err, ErrUnauthorized):
			Refuse(w, r, LoginRequiredText, http.StatusUnauthorized)
		default:
			w.Header().Set("Retry-After", "10")
			Refuse(w, r, UnavailableText, http.StatusServiceUnavailable)
		}
	})
}

// Refuse answers a refused request. A small unread body (a chat message) is
// read first: closing the connection with it unread can reset the
// connection, so the browser would see a network error instead of the 401.
// The body itself is discarded unseen.
func Refuse(w http.ResponseWriter, r *http.Request, message string, status int) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	}
	utils.Error(w, message, status)
}

// Handler answers GET /api/v1/chatbot/session for a signed-in caller (the
// middleware has already refused everyone else).
func Handler(w http.ResponseWriter, r *http.Request) {
	a, ok := FromContext(r.Context())
	if !ok {
		utils.Error(w, LoginRequiredText, http.StatusUnauthorized)
		return
	}
	utils.WriteSuccess(w, http.StatusOK, "signed in", map[string]any{"authenticated": true, "account_key": a.Key()})
}

// String keeps accounts out of logs by accident.
func (a Account) String() string { return fmt.Sprintf("account(%s)", a.Key()) }
