// Package skymcm reads the SKYMCM website's public hotel catalogue over its
// existing read-only API, server-to-server:
//
//	GET /api/v1/hotels          listed hotels (filters as query parameters)
//	GET /api/v1/hotels/{id}     one hotel, with check-in/check-out times
//	GET /api/v1/room-types      live availability and final prices for dates
//	GET /api/v1/landmarks       a city's landmarks
//
// These are the same endpoints (and the same services) the website itself
// uses; the previous in-process chatbot called those services directly. The
// Client implements the hotels, roomtypes and landmarks Service interfaces,
// so the chatbot and the public endpoints use it unchanged.
//
// Prices are used exactly as the API returns them: it has already applied
// active discounts, seasonal rates, GST and the guest-fit room count, so
// nothing here reprices anything.
//
// Safety: only the four GET paths above can be requested; no cookies,
// Authorization header or guest data are ever sent; redirects to another
// host are refused; every response is size-capped, time-limited and strictly
// decoded; upstream error bodies are never logged or passed on.
package skymcm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
	"github.com/Maynstayy/ai-chatbot-backend/internal/landmarks"
	"github.com/Maynstayy/ai-chatbot-backend/internal/roomtypes"
)

var (
	// ErrNotFound: the hotel doesn't exist or isn't listed (inactive).
	ErrNotFound = errors.New("skymcm: not found")
	// ErrRateLimited: the API answered 429; calls fail fast until Retry-After.
	ErrRateLimited = errors.New("skymcm: rate limited")
	// ErrUnavailable: network failure, timeout, 5xx, or an unusable response.
	ErrUnavailable = errors.New("skymcm: catalogue temporarily unavailable")
)

const (
	dateLayout      = "2006-01-02"
	defaultTimeout  = 8 * time.Second
	defaultCacheTTL = 60 * time.Second
	// Landmarks change rarely (admins add them by hand); MaynStay caches them
	// for 30 minutes too.
	defaultLandmarkTTL = 30 * time.Minute
	defaultRoomTTL     = 30 * time.Second
	defaultMaxStale    = 10 * time.Minute
	defaultMaxBody     = 4 << 20
	defaultParallel    = 4
	maxCooldown        = 60 * time.Second
	defaultCooldown    = 30 * time.Second
	retryDelay         = 300 * time.Millisecond
	defaultUserAgent   = "skymcm-ai-chatbot/1.0 (+server-to-server catalogue reader)"
)

// The only requests this client can make.
var allowedPath = regexp.MustCompile(`^/api/v1/(hotels(/[1-9][0-9]{0,18})?|room-types|landmarks)$`)

// Config configures the client. BaseURL is required.
type Config struct {
	BaseURL  string // e.g. https://skymcm.com (path prefix allowed)
	AssetURL string // where /uploads/... photos are served; defaults to BaseURL
	Timeout  time.Duration
	// CacheTTL applies to hotels, LandmarkCacheTTL to landmarks and
	// RoomCacheTTL to availability.
	CacheTTL         time.Duration
	LandmarkCacheTTL time.Duration
	RoomCacheTTL     time.Duration
	// MaxStale: how long past its cache time a hotel or landmark response may
	// still be served during an outage. Availability is never served stale.
	MaxStale      time.Duration
	MaxConcurrent int
	MaxBodyBytes  int64
	UserAgent     string
	// HTTPClient is for tests; production builds its own.
	HTTPClient *http.Client
}

// Client is safe for concurrent use.
type Client struct {
	base      *url.URL
	asset     string
	http      *http.Client
	cfg       Config
	sem       chan struct{}
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error
	mu        sync.Mutex
	cache     map[string]cacheEntry
	flights   map[string]*flight
	coolUntil time.Time
	lastOK    time.Time
	lastErr   time.Time
}

type cacheEntry struct {
	body    []byte
	fetched time.Time
	ttl     time.Duration
}

type flight struct {
	done chan struct{}
	body []byte
	err  error
}

// New validates the configuration and builds a client.
func New(cfg Config) (*Client, error) {
	base, err := parseBase(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("SKYMCM_API_URL: %w", err)
	}
	asset := strings.TrimRight(cfg.AssetURL, "/")
	if asset == "" {
		asset = strings.TrimRight(base.String(), "/")
	} else if _, err := parseBase(asset); err != nil {
		return nil, fmt.Errorf("SKYMCM_ASSET_URL: %w", err)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = defaultCacheTTL
	}
	if cfg.RoomCacheTTL <= 0 {
		cfg.RoomCacheTTL = defaultRoomTTL
	}
	if cfg.LandmarkCacheTTL <= 0 {
		cfg.LandmarkCacheTTL = defaultLandmarkTTL
	}
	if cfg.MaxStale <= 0 {
		cfg.MaxStale = defaultMaxStale
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = defaultParallel
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBody
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	} else {
		cp := *hc
		hc = &cp
		if hc.Timeout <= 0 {
			hc.Timeout = cfg.Timeout
		}
	}
	// No cookie jar; redirects only within the same scheme and host.
	hc.Jar = nil
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != base.Scheme || !strings.EqualFold(req.URL.Host, base.Host) {
			return fmt.Errorf("redirect to another host refused")
		}
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		return nil
	}
	return &Client{
		base: base, asset: asset, http: hc, cfg: cfg,
		sem: make(chan struct{}, cfg.MaxConcurrent), now: time.Now, sleep: sleepCtx,
		cache: map[string]cacheEntry{}, flights: map[string]*flight{},
	}, nil
}

func parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("must be an absolute http(s) URL such as https://skymcm.com")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must not contain credentials, a query or a fragment")
	}
	return u, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// --- the four endpoints -------------------------------------------------------

// GetHotels lists listed hotels matching the filter. Inactive or deleted
// hotels are never returned, whatever the filter says.
func (c *Client) GetHotels(ctx context.Context, f hotels.HotelFilter) ([]hotels.Hotel, error) {
	q := url.Values{}
	for k, v := range map[string]string{"city": f.City, "search": f.Search, "category": f.Category, "sort": f.Sort} {
		if v = strings.TrimSpace(v); v != "" {
			q.Set(k, v)
		}
	}
	if f.MinPrice > 0 {
		q.Set("min_price", strconv.FormatFloat(f.MinPrice, 'f', -1, 64))
	}
	if f.MaxPrice > 0 {
		q.Set("max_price", strconv.FormatFloat(f.MaxPrice, 'f', -1, 64))
	}
	var list []hotels.Hotel
	if err := c.getJSON(ctx, "/api/v1/hotels", q, c.cfg.CacheTTL, true, &list); err != nil {
		return nil, err
	}
	out := make([]hotels.Hotel, 0, len(list))
	for _, h := range list {
		if h.HotelID > 0 && h.IsActive && !h.IsDeleted {
			out = append(out, c.resolveHotel(h))
		}
	}
	return out, nil
}

// GetHotelByID returns one listed hotel; ErrNotFound when it doesn't exist
// or isn't listed.
func (c *Client) GetHotelByID(ctx context.Context, id int64) (*hotels.Hotel, error) {
	if id <= 0 {
		return nil, ErrNotFound
	}
	var h hotels.Hotel
	if err := c.getJSON(ctx, "/api/v1/hotels/"+strconv.FormatInt(id, 10), nil, c.cfg.CacheTTL, true, &h); err != nil {
		return nil, err
	}
	if h.HotelID != id || !h.IsActive || h.IsDeleted {
		return nil, ErrNotFound
	}
	out := c.resolveHotel(h)
	return &out, nil
}

// GetRoomTypesByHotel returns room types with live availability and final
// prices for the stay, exactly as the API computed them. A hotel that isn't
// listed has no rooms to offer.
func (c *Client) GetRoomTypesByHotel(ctx context.Context, hotelID int64, checkIn, checkOut *string, guests int) ([]roomtypes.RoomType, error) {
	if err := validateStay(checkIn, checkOut); err != nil {
		return nil, err
	}
	if guests < 0 || guests > roomtypes.MaxGuestsQuery {
		return nil, roomtypes.ErrInvalidGuests
	}
	if hotelID <= 0 {
		return []roomtypes.RoomType{}, nil
	}
	listed, err := c.isListed(ctx, hotelID)
	if err != nil {
		return nil, err
	}
	if !listed {
		return []roomtypes.RoomType{}, nil
	}
	q := url.Values{"hotel_id": {strconv.FormatInt(hotelID, 10)}}
	if checkIn != nil && checkOut != nil {
		q.Set("check_in", *checkIn)
		q.Set("check_out", *checkOut)
	}
	if guests > 0 {
		q.Set("guests", strconv.Itoa(guests))
	}
	var list []roomtypes.RoomType
	// Availability is never served stale.
	if err := c.getJSON(ctx, "/api/v1/room-types", q, c.cfg.RoomCacheTTL, false, &list); err != nil {
		return nil, err
	}
	out := make([]roomtypes.RoomType, 0, len(list))
	for _, rt := range list {
		if rt.HotelID != 0 && rt.HotelID != hotelID {
			continue // never mix another hotel's rooms in
		}
		rt.Images = c.resolveImages(rt.Images)
		out = append(out, rt)
	}
	return out, nil
}

// isListed reports whether a hotel is active and not deleted, from the hotel
// list the listings already use (one cached /api/v1/hotels response shared
// by every check), so a guided search over many hotels doesn't add a
// /hotels/{id} request per hotel. Only when that list can't be loaded at all
// does it fall back to the hotel's own lookup.
func (c *Client) isListed(ctx context.Context, hotelID int64) (bool, error) {
	list, err := c.GetHotels(ctx, hotels.HotelFilter{})
	if err == nil {
		for _, h := range list { // GetHotels returns listed hotels only
			if h.HotelID == hotelID {
				return true, nil
			}
		}
		return false, nil
	}
	if errors.Is(err, ErrRateLimited) {
		return false, err
	}
	if _, err := c.GetHotelByID(ctx, hotelID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ListByCity returns a city's active landmarks.
func (c *Client) ListByCity(ctx context.Context, city string) ([]landmarks.Landmark, error) {
	city = strings.TrimSpace(city)
	if city == "" {
		return []landmarks.Landmark{}, nil
	}
	var list []landmarks.Landmark
	if err := c.getJSON(ctx, "/api/v1/landmarks", url.Values{"city": {city}}, c.cfg.LandmarkCacheTTL, true, &list); err != nil {
		return nil, err
	}
	out := make([]landmarks.Landmark, 0, len(list))
	for _, l := range list {
		if l.IsActive {
			out = append(out, l)
		}
	}
	return out, nil
}

func validateStay(checkIn, checkOut *string) error {
	if checkIn == nil && checkOut == nil {
		return nil
	}
	if checkIn == nil || checkOut == nil {
		return roomtypes.ErrInvalidDateFmt
	}
	if _, err := time.Parse(dateLayout, *checkIn); err != nil {
		return roomtypes.ErrInvalidDateFmt
	}
	if _, err := time.Parse(dateLayout, *checkOut); err != nil {
		return roomtypes.ErrInvalidDateFmt
	}
	return nil
}

// --- images -----------------------------------------------------------------------

// resolveImage turns a stored photo path ("/uploads/hotels/<hash>.jpg") into
// a full URL on the asset host. Absolute http(s) URLs are kept; anything else
// (javascript:, data:, "//host", blanks) is dropped.
func (c *Client) resolveImage(u string) string {
	u = strings.TrimSpace(u)
	switch {
	case u == "" || strings.ContainsAny(u, " \"'<>\\"):
		return ""
	case strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//"):
		return c.asset + u
	case strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://"):
		return u
	}
	return ""
}

func (c *Client) resolveImages(list []string) []string {
	out := make([]string, 0, len(list))
	for _, u := range list {
		if r := c.resolveImage(u); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func (c *Client) resolveHotel(h hotels.Hotel) hotels.Hotel {
	h.ThumbnailURL = c.resolveImage(h.ThumbnailURL)
	h.Images = c.resolveImages(h.Images)
	if h.Amenities == nil {
		h.Amenities = []string{}
	}
	return h
}

// --- transport --------------------------------------------------------------------

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, ttl time.Duration, allowStale bool, v any) error {
	body, err := c.fetch(ctx, path, q, ttl, allowStale)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(body)) == "null" {
		return nil // an empty list
	}
	if err := json.Unmarshal(body, v); err != nil {
		slog.Warn("skymcm: response is not the expected JSON", "path", path)
		return ErrUnavailable
	}
	return nil
}

// fetch returns a fresh cached body, or one request's body shared by every
// identical concurrent caller. During an outage or a 429 cooldown it serves
// a stale body (if allowed and not too old) instead of failing.
func (c *Client) fetch(ctx context.Context, path string, q url.Values, ttl time.Duration, allowStale bool) ([]byte, error) {
	if !allowedPath.MatchString(path) {
		return nil, fmt.Errorf("skymcm: path %q is not allowed", path)
	}
	key := path + "?" + q.Encode()
	now := c.now()

	c.mu.Lock()
	if e, ok := c.cache[key]; ok && now.Sub(e.fetched) < e.ttl {
		c.mu.Unlock()
		return e.body, nil
	}
	if now.Before(c.coolUntil) {
		stale, ok := c.staleLocked(key, now, allowStale)
		c.mu.Unlock()
		if ok {
			return stale, nil
		}
		return nil, ErrRateLimited
	}
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.body, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()

	body, err := c.request(ctx, path, q)

	c.mu.Lock()
	delete(c.flights, key)
	if err == nil {
		c.cache[key] = cacheEntry{body: body, fetched: c.now(), ttl: ttl}
		c.lastOK = c.now()
		c.pruneLocked()
	} else if !errors.Is(err, ErrNotFound) && !errors.Is(err, context.Canceled) {
		c.lastErr = c.now()
		if stale, ok := c.staleLocked(key, c.now(), allowStale); ok {
			slog.Warn("skymcm: serving cached data while the API is unavailable", "path", path)
			body, err = stale, nil
		}
	}
	c.mu.Unlock()

	f.body, f.err = body, err
	close(f.done)
	return body, err
}

func (c *Client) staleLocked(key string, now time.Time, allowStale bool) ([]byte, bool) {
	if !allowStale {
		return nil, false
	}
	if e, ok := c.cache[key]; ok && now.Sub(e.fetched) < e.ttl+c.cfg.MaxStale {
		return e.body, true
	}
	return nil, false
}

// pruneLocked drops entries too old to serve even as stale (past their own
// cache time plus MaxStale).
func (c *Client) pruneLocked() {
	if len(c.cache) < 500 {
		return
	}
	now := c.now()
	for k, e := range c.cache {
		if now.Sub(e.fetched) > e.ttl+c.cfg.MaxStale {
			delete(c.cache, k)
		}
	}
}

// request performs one GET (retrying once after a network error or a 502,
// 503 or 504; never after a 429), within the concurrency limit.
func (c *Client) request(ctx context.Context, path string, q url.Values) ([]byte, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()

	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + path
	u.RawQuery = q.Encode()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, retryDelay); err != nil {
				return nil, err
			}
		}
		body, retry, err := c.once(ctx, u.String(), path)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return nil, lastErr
}

func (c *Client) once(ctx context.Context, rawURL, path string) (body []byte, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, ErrUnavailable
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)

	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		slog.Warn("skymcm: request failed", "path", path, "err", errorKind(err))
		return nil, true, ErrUnavailable
	}
	defer res.Body.Close()

	switch {
	case res.StatusCode == http.StatusOK:
		data, err := io.ReadAll(io.LimitReader(res.Body, c.cfg.MaxBodyBytes+1))
		if err != nil {
			return nil, true, ErrUnavailable
		}
		if int64(len(data)) > c.cfg.MaxBodyBytes {
			slog.Warn("skymcm: response too large", "path", path, "limit_bytes", c.cfg.MaxBodyBytes)
			return nil, false, ErrUnavailable
		}
		return data, false, nil
	case res.StatusCode == http.StatusNotFound:
		return nil, false, ErrNotFound
	case res.StatusCode == http.StatusTooManyRequests:
		wait := retryAfter(res.Header.Get("Retry-After"), c.now())
		c.mu.Lock()
		if until := c.now().Add(wait); until.After(c.coolUntil) {
			c.coolUntil = until
		}
		c.mu.Unlock()
		slog.Warn("skymcm: rate limited by the API; pausing requests", "path", path, "seconds", int(wait.Seconds()))
		return nil, false, ErrRateLimited
	case res.StatusCode == http.StatusBadRequest:
		// Inputs are validated before the call, so this is unexpected.
		slog.Warn("skymcm: API rejected the request", "path", path, "status", res.StatusCode)
		return nil, false, ErrUnavailable
	default:
		slog.Warn("skymcm: API error", "path", path, "status", res.StatusCode)
		retry := res.StatusCode == http.StatusBadGateway || res.StatusCode == http.StatusServiceUnavailable || res.StatusCode == http.StatusGatewayTimeout
		return nil, retry, ErrUnavailable
	}
}

// retryAfter reads Retry-After (seconds or an HTTP date), capped at a minute.
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	d := defaultCooldown
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	if d < time.Second {
		d = time.Second
	}
	if d > maxCooldown {
		d = maxCooldown
	}
	return d
}

// errorKind describes a transport error without its addresses.
func errorKind(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "timeout"
		}
		err = ue.Err
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "redirect"):
		return "redirect refused"
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "x509") || strings.Contains(msg, "tls"):
		return "tls error"
	case strings.Contains(msg, "refused"):
		return "connection refused"
	case strings.Contains(msg, "no such host"):
		return "dns error"
	}
	return "network error"
}

// --- status -----------------------------------------------------------------------

// Status is the API's last known state, for the readiness check. It never
// makes a request.
type Status struct {
	State     string `json:"state"` // unknown | ok | degraded | rate_limited
	LastOKAgo string `json:"last_ok_ago,omitempty"`
}

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	s := Status{State: "unknown"}
	if !c.lastOK.IsZero() {
		s.State = "ok"
		s.LastOKAgo = now.Sub(c.lastOK).Round(time.Second).String()
	}
	if c.lastErr.After(c.lastOK) {
		s.State = "degraded"
	}
	if now.Before(c.coolUntil) {
		s.State = "rate_limited"
	}
	return s
}
