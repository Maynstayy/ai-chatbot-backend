package chatbot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/bookings"
	"github.com/Maynstayy/ai-chatbot-backend/internal/hotels"
	"github.com/Maynstayy/ai-chatbot-backend/internal/rooms"
	"github.com/Maynstayy/ai-chatbot-backend/internal/roomtypes"
)

// The chatbot reads data only through these slices of existing services - the
// same code the public hotel pages and the authenticated booking endpoints
// use. It never runs SQL of its own and never writes anything.

// HotelSource is satisfied by hotels.Service.
type HotelSource interface {
	GetHotels(ctx context.Context, filter hotels.HotelFilter) ([]hotels.Hotel, error)
	GetHotelByID(ctx context.Context, id int64) (*hotels.Hotel, error)
}

// RoomSource is satisfied by roomtypes.Service.
type RoomSource interface {
	GetRoomTypesByHotel(ctx context.Context, hotelID int64, checkIn, checkOut *string, guests int) ([]roomtypes.RoomType, error)
}

// BookingSource is satisfied by bookings.Service.
type BookingSource interface {
	GetBookingByID(ctx context.Context, id int64) (*bookings.Booking, error)
	GetMyBookings(ctx context.Context, userID int64, status string) ([]bookings.Booking, error)
}

// PaymentSource is satisfied by payments.Service.
type PaymentSource interface {
	HasPaidPayment(ctx context.Context, bookingID int64) (bool, error)
}

const (
	toolSearchHotels      = "search_hotels"
	toolHotelDetails      = "get_hotel_details"
	toolRoomAvailability  = "check_room_availability"
	toolListMyBookings    = "list_my_bookings"
	toolGetBookingStatus  = "get_booking_status"
	maxHotelResults       = 8
	maxBookingResults     = 10
	maxNights             = 30 // same cap the booking flow enforces
	maxGuests             = 50
	maxFilterTextLen      = 80
	maxRecordedRoomTypes  = 12
	dateLayout            = "2006-01-02"
	bookingNotFoundResult = `{"found":false,"note":"No booking with this id exists on the signed-in account."}`
)

// indiaTime decides what "today" is for date checks, as bookings does.
var indiaTime = time.FixedZone("IST", 5*3600+30*60)

var safeText = regexp.MustCompile(`^[\p{L}\p{N} .,'&()-]*$`)

// publicTools are offered to every caller; bookingTools only to a signed-in
// caller, and they always act on that caller's own bookings.
func publicTools() []ToolSpec {
	return []ToolSpec{
		{
			Name: toolSearchHotels,
			Description: "Search SKYMCM's listed hotels by city/destination, name, category or nightly price range. " +
				"Returns up to 8 hotels with their starting nightly price (INR, before GST), rating, amenities and policies. " +
				"It does NOT check dates - use check_room_availability for availability on specific dates.",
			Properties: map[string]any{
				"city":      map[string]any{"type": "string", "description": "City or destination, e.g. \"Haridwar\""},
				"query":     map[string]any{"type": "string", "description": "Free text matched against hotel name and location"},
				"category":  map[string]any{"type": "string", "description": "Hotel category, only if the guest named one"},
				"min_price": map[string]any{"type": "number", "description": "Minimum starting price per night in INR"},
				"max_price": map[string]any{"type": "number", "description": "Maximum starting price per night in INR (the guest's budget)"},
				"sort":      map[string]any{"type": "string", "enum": []string{"price_asc", "price_desc", "rating"}},
			},
		},
		{
			Name:        toolHotelDetails,
			Description: "Full details of one hotel by hotel_id: description, address, amenities, check-in/out times, cancellation, child, pet and ID policies, payment options.",
			Properties: map[string]any{
				"hotel_id": map[string]any{"type": "integer", "description": "hotel_id from search_hotels"},
			},
			Required: []string{"hotel_id"},
		},
		{
			Name: toolRoomAvailability,
			Description: "Room types of one hotel with live availability and prices for the given dates (and how many rooms the guests need). " +
				"Dates are YYYY-MM-DD; check_in must be today or later and the stay at most 30 nights.",
			Properties: map[string]any{
				"hotel_id":  map[string]any{"type": "integer"},
				"check_in":  map[string]any{"type": "string", "description": "YYYY-MM-DD"},
				"check_out": map[string]any{"type": "string", "description": "YYYY-MM-DD"},
				"guests":    map[string]any{"type": "integer", "description": "Total number of guests (optional)"},
			},
			Required: []string{"hotel_id", "check_in", "check_out"},
		},
	}
}

func bookingTools() []ToolSpec {
	return []ToolSpec{
		{
			Name:        toolListMyBookings,
			Description: "List the signed-in guest's own most recent bookings (up to 10), optionally filtered by status.",
			Properties: map[string]any{
				"status": map[string]any{"type": "string", "enum": []string{"pending", "confirmed", "cancelled", "failed"}},
			},
		},
		{
			Name:        toolGetBookingStatus,
			Description: "Status, dates, amount, payment record and the hotel's cancellation terms for one of the signed-in guest's own bookings.",
			Properties: map[string]any{
				"booking_id": map[string]any{"type": "integer"},
			},
			Required: []string{"booking_id"},
		},
	}
}

// toolRun is one request's tool executor: it knows who is asking and records
// what the tools returned so the API response can carry the real data.
type toolRun struct {
	svc    *Service
	userID int64
	now    time.Time

	// evidence is what the guest actually said in this conversation (their
	// own messages plus the search details they chose on the website),
	// lower-cased. Tool filters the model adds that the guest never stated
	// are dropped against it. bookingEvidence is the guest's messages only:
	// a booking id must have been typed by the guest.
	evidence        string
	bookingEvidence string
	debug           bool
	source          string // "model" or "router", for the debug log

	used            []string
	uses            []ToolUse
	hotels          []HotelCard
	roomTypes       []RoomTypeCard
	bookings        []BookingCard
	bookingsChecked bool
	count           int // results of the current call, for the debug log
}

// ToolUse is one tool call as reported in development (CHATBOT_DEBUG).
// Arguments are search filters and ids only, never credentials or tokens.
type ToolUse struct {
	Name    string          `json:"name"`
	Args    json.RawMessage `json:"args"`
	Results int             `json:"results"`
	Error   string          `json:"error,omitempty"`
	Source  string          `json:"source"`
}

func (t *toolRun) execute(ctx context.Context, call ToolCall) (string, bool) {
	t.used = append(t.used, call.Name)
	t.count = 0
	args, sanErr := t.sanitize(call.Name, call.Input)
	result, isErr := t.dispatch(ctx, call.Name, args, sanErr)

	use := ToolUse{Name: call.Name, Args: args, Results: t.count, Source: t.source}
	if use.Source == "" {
		use.Source = "model"
	}
	if !json.Valid(use.Args) {
		use.Args = json.RawMessage("{}")
	}
	if isErr {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(result), &e)
		use.Error = e.Error
	}
	t.uses = append(t.uses, use)
	if t.debug {
		slog.Info("chatbot tool", "tool", use.Name, "source", use.Source, "args", string(use.Args),
			"results", use.Results, "error", use.Error)
	}
	return result, isErr
}

func (t *toolRun) dispatch(ctx context.Context, name string, args json.RawMessage, sanErr error) (string, bool) {
	var (
		out any
		err error
	)
	switch {
	case name != toolSearchHotels && name != toolHotelDetails && name != toolRoomAvailability &&
		name != toolListMyBookings && name != toolGetBookingStatus:
		return errorResult("unknown tool"), true
	case sanErr != nil:
		err = sanErr
	case name == toolSearchHotels:
		out, err = t.searchHotels(ctx, args)
	case name == toolHotelDetails:
		out, err = t.hotelDetails(ctx, args)
	case name == toolRoomAvailability:
		out, err = t.roomAvailability(ctx, args)
	case name == toolListMyBookings:
		out, err = t.listMyBookings(ctx, args)
	default:
		out, err = t.bookingStatus(ctx, args)
	}
	if err != nil {
		var ie inputError
		if errors.As(err, &ie) {
			return errorResult(ie.msg), true
		}
		// Internal failures are logged here and never shown to the model
		// (or, through it, to the user).
		slog.Warn("chatbot tool failed", "tool", name, "err", err.Error())
		return errorResult("this lookup is temporarily unavailable"), true
	}
	if s, ok := out.(string); ok {
		return s, false
	}
	b, err := json.Marshal(out)
	if err != nil {
		return errorResult("could not encode result"), true
	}
	return string(b), false
}

// --- dropping filters the guest never asked for --------------------------

var (
	numberInText = regexp.MustCompile(`(?i)(\d[\d,]*(?:\.\d+)?)\s*(k\b)?`)
	exactPrice   = regexp.MustCompile(`(?i)\b(exactly|exact)\b`)
)

// mentionsNumber reports whether n appears in text as a number ("2000",
// "2,000" or "2k").
func mentionsNumber(text string, n float64) bool {
	for _, m := range numberInText.FindAllStringSubmatch(text, -1) {
		v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
		if err != nil {
			continue
		}
		if m[2] != "" {
			v *= 1000
		}
		if v-n < 0.5 && n-v < 0.5 {
			return true
		}
	}
	return false
}

// budgetFloor is "above 2000", "from ₹2,000", "at least 2k".
var budgetFloor = regexp.MustCompile(`(?i)\b(?:above|over|more than|from|at least|min(?:imum)?|starting(?: at| from)?)\s*(?:₹|rs\.?|inr)?\s*(\d[\d,]*)\s*(k\b)?`)

// priceIn reports whether re (with the amount in group 1 and an optional
// "k" in group 2) matches n somewhere in text.
func priceIn(re *regexp.Regexp, text string, n float64) bool {
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
		if err != nil {
			continue
		}
		if m[2] != "" {
			v *= 1000
		}
		if v-n < 0.5 && n-v < 0.5 {
			return true
		}
	}
	return false
}

// statusWords maps a booking status to the word stems that name it.
var statusWords = map[string][]string{
	"pending":   {"pending", "unpaid", "awaiting"},
	"confirmed": {"confirm"},
	"cancelled": {"cancel"},
	"failed":    {"fail"},
}

func statusMentioned(text, status string) bool {
	for _, w := range statusWords[status] {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// sanitize removes arguments a small model tends to invent: price limits,
// categories and search text the guest never stated, and booking status
// filters they never asked for. An invented booking id is rejected outright.
// What survives still goes through each tool's strict validation, and the
// booking tools still check sign-in and ownership.
func (t *toolRun) sanitize(name string, raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}"), nil
	}
	var args map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&args) != nil || args == nil {
		return raw, nil // not an object: the tool's own decoding rejects it
	}
	num := func(k string) (float64, bool) {
		n, ok := args[k].(json.Number)
		if !ok {
			return 0, false
		}
		f, err := n.Float64()
		return f, err == nil
	}
	text := func(k string) (string, bool) {
		s, ok := args[k].(string)
		return strings.ToLower(strings.TrimSpace(s)), ok
	}
	changed := false
	drop := func(k string) { delete(args, k); changed = true }

	switch name {
	case toolSearchHotels:
		minP, hasMin := num("min_price")
		maxP, hasMax := num("max_price")
		if hasMin && (minP == 0 || !mentionsNumber(t.evidence, minP)) {
			drop("min_price")
			hasMin = false
		}
		if hasMax && (maxP == 0 || !mentionsNumber(t.evidence, maxP)) {
			drop("max_price")
			hasMax = false
		}
		// "under 2000" sent as min=max=2000: keep it as a ceiling.
		if hasMin && hasMax && minP == maxP && !exactPrice.MatchString(t.evidence) {
			drop("min_price")
			hasMin = false
		}
		// "under 2000" sent as min_price=2000: the guest meant a ceiling.
		if hasMin && priceIn(budgetCeiling, t.evidence, minP) && !priceIn(budgetFloor, t.evidence, minP) {
			if !hasMax {
				args["max_price"] = args["min_price"]
			}
			drop("min_price")
		}
		for _, k := range []string{"category", "query"} {
			if v, ok := text(k); ok && (v == "" || !strings.Contains(t.evidence, v)) {
				drop(k)
			}
		}
	case toolListMyBookings:
		if v, ok := text("status"); ok && !statusMentioned(t.evidence, v) {
			drop("status")
		}
	case toolGetBookingStatus:
		if id, ok := num("booking_id"); ok && id > 0 && !mentionsNumber(t.bookingEvidence, id) {
			return raw, inputError{"the guest has not given this booking id; ask them for the booking id shown in My Bookings instead of guessing"}
		}
	}
	if !changed {
		return raw, nil
	}
	out, err := json.Marshal(args)
	if err != nil {
		return raw, nil
	}
	return out, nil
}

// inputError is a rejected tool argument; its message goes back to the model.
type inputError struct{ msg string }

func (e inputError) Error() string { return e.msg }

func errorResult(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

// decodeInput parses tool arguments strictly: unknown fields are rejected.
func decodeInput(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return inputError{"invalid arguments: " + strings.TrimPrefix(err.Error(), "json: ")}
	}
	return nil
}

func cleanFilterText(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if len([]rune(v)) > maxFilterTextLen || !safeText.MatchString(v) {
		return "", inputError{field + " must be plain text of at most 80 characters"}
	}
	return v, nil
}

// HotelCard is the public subset of a hotel the chatbot returns.
type HotelCard struct {
	HotelID          int64    `json:"hotel_id"`
	Name             string   `json:"name"`
	City             string   `json:"city"`
	State            string   `json:"state,omitempty"`
	Location         string   `json:"location,omitempty"`
	Category         string   `json:"category,omitempty"`
	PricePerNight    float64  `json:"price_per_night"`
	TaxPerNight      float64  `json:"tax_per_night"`
	Rating           float64  `json:"rating"`
	TotalReviews     int      `json:"total_reviews"`
	Amenities        []string `json:"amenities,omitempty"`
	FreeCancellation bool     `json:"free_cancellation"`
	PayAtProperty    bool     `json:"pay_at_property"`
	// ThumbnailURL is the hotel's primary photo, exactly as stored (e.g.
	// "/uploads/hotels/<hash>.jpg"): its thumbnail, or the first gallery image
	// when it has no thumbnail. Empty when neither is a usable URL; the
	// website then shows its own fallback image.
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	// Description is the start of the hotel's own description, for the
	// card. It is listing data, shown as plain text only.
	Description string `json:"description,omitempty"`
}

// usableImageURL reports a stored image URL the website can load as-is: a
// path on the site ("/uploads/..."), or an absolute http(s) URL. Anything
// else (javascript:, data:, "//host", blank) is not passed on.
func usableImageURL(u string) bool {
	u = strings.TrimSpace(u)
	if strings.HasPrefix(u, "/") {
		return !strings.HasPrefix(u, "//") && !strings.ContainsAny(u, " \"'<>\\")
	}
	parsed, err := url.Parse(u)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" && !strings.ContainsAny(u, " \"'<>\\")
}

// primaryImage is the photo the website shows for a hotel: its thumbnail,
// else its first gallery image. URLs are passed on unchanged.
func primaryImage(h hotels.Hotel) string {
	if usableImageURL(h.ThumbnailURL) {
		return strings.TrimSpace(h.ThumbnailURL)
	}
	for _, img := range h.Images {
		if usableImageURL(img) {
			return strings.TrimSpace(img)
		}
	}
	return ""
}

const cardDescriptionRunes = 160

// shortDescription is the first cardDescriptionRunes characters of a hotel
// description, whitespace collapsed, cut at a word with an ellipsis.
func shortDescription(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= cardDescriptionRunes {
		return s
	}
	cut := string(r[:cardDescriptionRunes])
	if i := strings.LastIndex(cut, " "); i > cardDescriptionRunes/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:-") + "…"
}

func toCard(h hotels.Hotel) HotelCard {
	amenities := h.Amenities
	if len(amenities) > 10 {
		amenities = amenities[:10]
	}
	return HotelCard{
		HotelID: h.HotelID, Name: h.Name, City: h.City, State: h.State, Location: h.Location,
		Category: h.Category, PricePerNight: h.PricePerNight, TaxPerNight: h.TaxPerNight,
		Rating: h.Rating, TotalReviews: h.TotalReviews, Amenities: amenities,
		FreeCancellation: h.FreeCancellation, PayAtProperty: h.PayAtProperty, ThumbnailURL: primaryImage(h),
		Description: shortDescription(h.Description),
	}
}

func (t *toolRun) searchHotels(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		City     string  `json:"city"`
		Query    string  `json:"query"`
		Category string  `json:"category"`
		MinPrice float64 `json:"min_price"`
		MaxPrice float64 `json:"max_price"`
		Sort     string  `json:"sort"`
	}
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	var err error
	if in.City, err = cleanFilterText("city", in.City); err != nil {
		return nil, err
	}
	if in.Query, err = cleanFilterText("query", in.Query); err != nil {
		return nil, err
	}
	if in.Category, err = cleanFilterText("category", in.Category); err != nil {
		return nil, err
	}
	if in.MinPrice < 0 || in.MaxPrice < 0 || in.MinPrice > 1e7 || in.MaxPrice > 1e7 {
		return nil, inputError{"prices must be between 0 and 10000000"}
	}
	switch in.Sort {
	case "", "price_asc", "price_desc", "rating":
	default:
		return nil, inputError{"sort must be price_asc, price_desc or rating"}
	}

	active := true
	filter := hotels.HotelFilter{
		City: in.City, Search: in.Query, Category: in.Category,
		MinPrice: in.MinPrice, MaxPrice: in.MaxPrice, Sort: in.Sort, IsActive: &active,
	}
	list, err := t.svc.hotels.GetHotels(ctx, filter)
	if err != nil {
		return nil, err
	}
	// The city column is matched exactly; a destination like "Rishikesh" may
	// only appear in the location text, so retry it as a search term. When a
	// search term was given too (small models sometimes put a hotel name in
	// both), retry with that term alone.
	if len(list) == 0 && in.City != "" {
		filter.City = ""
		if in.Query == "" {
			filter.Search = in.City
		}
		if list, err = t.svc.hotels.GetHotels(ctx, filter); err != nil {
			return nil, err
		}
	}

	total := len(list)
	if len(list) > maxHotelResults {
		list = list[:maxHotelResults]
	}
	cards := make([]HotelCard, 0, len(list))
	for _, h := range list {
		cards = append(cards, toCard(h))
	}
	t.hotels = mergeCards(t.hotels, cards)
	t.count = len(cards)
	// The model gets a compact copy (a small local model slows down sharply
	// on long prompts); the website's cards above keep the full data.
	brief := make([]modelHotel, 0, maxModelHotels)
	for _, c := range cards {
		if len(brief) == maxModelHotels {
			break
		}
		brief = append(brief, briefCard(c))
	}
	return map[string]any{
		"total_matches": total,
		"hotels":        brief,
		"price_note":    "price_per_night_before_gst is the starting nightly price in INR; GST (gst_per_night) is added on top at checkout",
	}, nil
}

// maxModelHotels is how many search results the model is shown.
const maxModelHotels = 5

// modelHotel is the subset of a hotel card the model needs to answer.
type modelHotel struct {
	HotelID          int64    `json:"hotel_id"`
	Name             string   `json:"name"`
	City             string   `json:"city"`
	Location         string   `json:"location,omitempty"`
	Category         string   `json:"category,omitempty"`
	PriceBeforeGST   float64  `json:"price_per_night_before_gst"`
	GSTPerNight      float64  `json:"gst_per_night"`
	Rating           float64  `json:"rating"`
	TotalReviews     int      `json:"total_reviews"`
	Amenities        []string `json:"amenities,omitempty"`
	FreeCancellation bool     `json:"free_cancellation"`
	PayAtProperty    bool     `json:"pay_at_property"`
}

func briefCard(c HotelCard) modelHotel {
	amenities := c.Amenities
	if len(amenities) > 5 {
		amenities = amenities[:5]
	}
	return modelHotel{HotelID: c.HotelID, Name: c.Name, City: c.City, Location: c.Location, Category: c.Category,
		PriceBeforeGST: c.PricePerNight, GSTPerNight: c.TaxPerNight, Rating: c.Rating, TotalReviews: c.TotalReviews,
		Amenities: amenities, FreeCancellation: c.FreeCancellation, PayAtProperty: c.PayAtProperty}
}

func mergeCards(have, add []HotelCard) []HotelCard {
	seen := map[int64]bool{}
	for _, c := range have {
		seen[c.HotelID] = true
	}
	for _, c := range add {
		if !seen[c.HotelID] && len(have) < maxHotelResults {
			seen[c.HotelID] = true
			have = append(have, c)
		}
	}
	return have
}

// activeHotel loads a hotel guests can see (not deleted, not deactivated).
func (t *toolRun) activeHotel(ctx context.Context, id int64) (*hotels.Hotel, error) {
	if id <= 0 {
		return nil, inputError{"hotel_id must be a positive integer"}
	}
	h, err := t.svc.hotels.GetHotelByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (h == nil || !h.IsActive || h.IsDeleted)) {
		return nil, inputError{"no listed hotel has this hotel_id"}
	}
	return h, err
}

func (t *toolRun) hotelDetails(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		HotelID int64 `json:"hotel_id"`
	}
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	h, err := t.activeHotel(ctx, in.HotelID)
	if err != nil {
		return nil, err
	}
	desc := []rune(h.Description)
	if len(desc) > 600 {
		desc = append(desc[:600], '…')
	}
	t.hotels = mergeCards(t.hotels, []HotelCard{toCard(*h)})
	t.count = 1
	return map[string]any{
		"hotel_id":           h.HotelID,
		"name":               h.Name,
		"description":        string(desc),
		"address":            h.Address,
		"location":           h.Location,
		"city":               h.City,
		"state":              h.State,
		"country":            h.Country,
		"category":           h.Category,
		"price_per_night":    h.PricePerNight,
		"tax_per_night":      h.TaxPerNight,
		"rating":             h.Rating,
		"total_reviews":      h.TotalReviews,
		"amenities":          h.Amenities,
		"check_in_time":      h.CheckInTime,
		"check_out_time":     h.CheckOutTime,
		"free_cancellation":  h.FreeCancellation,
		"cancellation_hours": h.CancellationHours,
		"pay_at_property":    h.PayAtProperty,
		"instant_booking":    h.InstantBooking,
		"child_policy":       orNotStated(h.ChildPolicy),
		"pet_policy":         orNotStated(h.PetPolicy),
		"id_rules":           orNotStated(h.IDRules),
	}, nil
}

// notStated replaces an empty policy, which a model tends to read as "no".
const notStated = "not stated by the hotel - the guest should ask the hotel"

func orNotStated(s string) string {
	if strings.TrimSpace(s) == "" {
		return notStated
	}
	return s
}

// RoomTypeCard is the public subset of a room type with live availability.
type RoomTypeCard struct {
	HotelID            int64    `json:"hotel_id"`
	RoomTypeID         int64    `json:"room_type_id"`
	Name               string   `json:"name"`
	Capacity           int      `json:"capacity"`
	PricePerNight      float64  `json:"price_per_night"`
	TaxPerNight        float64  `json:"tax_per_night"`
	AvailableCount     int      `json:"available_count"`
	BedType            string   `json:"bed_type,omitempty"`
	Amenities          []string `json:"amenities,omitempty"`
	RoomsNeeded        int      `json:"rooms_needed,omitempty"`
	FitsGuests         *bool    `json:"fits_guests,omitempty"`
	ComboPricePerNight float64  `json:"combo_price_per_night,omitempty"`
	Recommended        bool     `json:"recommended,omitempty"`
	SeasonalNote       string   `json:"seasonal_note,omitempty"`
	CheckIn            string   `json:"check_in"`
	CheckOut           string   `json:"check_out"`
	Guests             int      `json:"guests,omitempty"`

	// The cheapest active rate plan with enough rooms left for this stay -
	// what the booking page would charge. Empty when no plan can be booked.
	// The website uses the id to build a "Book now" link; the booking flow
	// still validates the plan, price and availability itself.
	RatePlanID            int64    `json:"rate_plan_id,omitempty"`
	RatePlanName          string   `json:"rate_plan_name,omitempty"`
	RatePlanPricePerNight float64  `json:"rate_plan_price_per_night,omitempty"`
	RatePlanTaxPerNight   float64  `json:"rate_plan_tax_per_night,omitempty"`
	RatePlanStayTotal     *float64 `json:"rate_plan_stay_total,omitempty"`
}

// bestRatePlan picks the plan the website's chat would offer: active, enough
// rooms left for the rooms this stay needs, lowest price per night.
func bestRatePlan(rt roomtypes.RoomType) *rooms.RatePlan {
	needed := max(rt.RoomsNeeded, 1)
	var best *rooms.RatePlan
	for i := range rt.RatePlans {
		p := &rt.RatePlans[i]
		if !p.IsActive || (p.RoomsLeft != nil && *p.RoomsLeft < needed) {
			continue
		}
		if best == nil || p.PricePerNight < best.PricePerNight {
			best = p
		}
	}
	return best
}

func (t *toolRun) roomAvailability(ctx context.Context, raw json.RawMessage) (any, error) {
	var in struct {
		HotelID  int64  `json:"hotel_id"`
		CheckIn  string `json:"check_in"`
		CheckOut string `json:"check_out"`
		Guests   int    `json:"guests"`
	}
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	if err := validateStay(in.CheckIn, in.CheckOut, t.now); err != nil {
		return nil, err
	}
	if in.Guests < 0 || in.Guests > maxGuests {
		return nil, inputError{"guests must be between 1 and 50"}
	}
	h, err := t.activeHotel(ctx, in.HotelID)
	if err != nil {
		return nil, err
	}
	types, err := t.svc.rooms.GetRoomTypesByHotel(ctx, h.HotelID, &in.CheckIn, &in.CheckOut, in.Guests)
	if errors.Is(err, roomtypes.ErrInvalidDateFmt) || errors.Is(err, roomtypes.ErrInvalidGuests) {
		return nil, inputError{err.Error()}
	}
	if err != nil {
		return nil, err
	}
	cards := make([]RoomTypeCard, 0, len(types))
	for _, rt := range types {
		cards = append(cards, RoomTypeCard{
			HotelID: h.HotelID, RoomTypeID: rt.RoomTypeID, Name: rt.Name, Capacity: rt.Capacity,
			PricePerNight: rt.PricePerNight, TaxPerNight: rt.TaxPerNight, AvailableCount: rt.AvailableCount,
			BedType: rt.BedType, Amenities: rt.Amenities, RoomsNeeded: rt.RoomsNeeded, FitsGuests: rt.FitsGuests,
			ComboPricePerNight: rt.ComboPricePerNight, Recommended: rt.Recommended, SeasonalNote: rt.SeasonalNote,
			CheckIn: in.CheckIn, CheckOut: in.CheckOut, Guests: in.Guests,
		})
		if p := bestRatePlan(rt); p != nil {
			c := &cards[len(cards)-1]
			c.RatePlanID, c.RatePlanName = p.RatePlanID, p.Name
			c.RatePlanPricePerNight, c.RatePlanTaxPerNight, c.RatePlanStayTotal = p.PricePerNight, p.TaxPerNight, p.StayTotal
		}
	}
	for _, c := range cards {
		if len(t.roomTypes) < maxRecordedRoomTypes {
			t.roomTypes = append(t.roomTypes, c)
		}
	}
	t.count = len(cards)
	return map[string]any{
		"hotel_id":   h.HotelID,
		"hotel_name": h.Name,
		"check_in":   in.CheckIn,
		"check_out":  in.CheckOut,
		"room_types": cards,
		"note":       "available_count is the number of rooms of that type free for these dates; prices are INR per room per night before GST; rate_plan_price_per_night is the cheapest bookable plan for this stay",
	}, nil
}

// validateStay applies the booking flow's date rules.
func validateStay(checkIn, checkOut string, now time.Time) error {
	in, err1 := time.Parse(dateLayout, checkIn)
	out, err2 := time.Parse(dateLayout, checkOut)
	if err1 != nil || err2 != nil {
		return inputError{"check_in and check_out must be dates in YYYY-MM-DD format"}
	}
	today, _ := time.Parse(dateLayout, now.In(indiaTime).Format(dateLayout))
	if in.Before(today) {
		return inputError{"check_in is in the past"}
	}
	if !out.After(in) {
		return inputError{"check_out must be after check_in"}
	}
	if out.Sub(in) > maxNights*24*time.Hour {
		return inputError{fmt.Sprintf("a stay can be at most %d nights", maxNights)}
	}
	return nil
}

// requireUser guards the booking tools. They are only offered to signed-in
// callers; this re-check means a model can't reach them any other way.
func (t *toolRun) requireUser() error {
	if t.userID <= 0 {
		return inputError{"the guest is not signed in; ask them to log in to see bookings"}
	}
	return nil
}

func (t *toolRun) bookingSummary(ctx context.Context, b bookings.Booking) map[string]any {
	m := map[string]any{
		"booking_id":      b.BookingID,
		"item_type":       b.ItemType,
		"status":          b.Status,
		"check_in":        b.CheckIn,
		"check_out":       b.CheckOut,
		"guests":          b.Guests,
		"room_count":      b.RoomCount,
		"total_amount":    b.TotalAmount,
		"pay_at_property": b.PayAtProperty,
	}
	if b.ItemType == "hotel" {
		if h, err := t.svc.hotels.GetHotelByID(ctx, b.ItemID); err == nil && h != nil {
			m["hotel_name"] = h.Name
			m["hotel_id"] = h.HotelID
		}
	}
	return m
}

func (t *toolRun) listMyBookings(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := t.requireUser(); err != nil {
		return nil, err
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	switch in.Status {
	case "", "pending", "confirmed", "cancelled", "failed":
	default:
		return nil, inputError{"status must be pending, confirmed, cancelled or failed"}
	}
	list, err := t.svc.bookings.GetMyBookings(ctx, t.userID, in.Status)
	if err != nil {
		return nil, err
	}
	total := len(list)
	if len(list) > maxBookingResults {
		list = list[:maxBookingResults]
	}
	out := make([]map[string]any, 0, len(list))
	for _, b := range list {
		if b.UserID != t.userID { // defence in depth; the query is already scoped
			continue
		}
		m := t.bookingSummary(ctx, b)
		out = append(out, m)
		t.addBookingCard(b, m)
	}
	t.bookingsChecked = true
	t.count = len(out)
	result := map[string]any{"total": total, "bookings": out}
	if in.Status != "" {
		result["status_filter"] = in.Status
	}
	return result, nil
}

// BookingCard is one of the signed-in guest's own bookings, as returned in
// the API response. It only ever holds bookings a booking tool returned for
// that guest.
type BookingCard struct {
	BookingID     int64   `json:"booking_id"`
	HotelID       int64   `json:"hotel_id,omitempty"`
	HotelName     string  `json:"hotel_name,omitempty"`
	Status        string  `json:"status"`
	CheckIn       string  `json:"check_in"`
	CheckOut      string  `json:"check_out"`
	Guests        int     `json:"guests"`
	RoomCount     int     `json:"room_count"`
	TotalAmount   float64 `json:"total_amount"`
	PayAtProperty bool    `json:"pay_at_property"`
	// Payment is set only by a single-booking lookup, from the payment
	// records: pay_at_hotel, paid, unpaid (a pending booking with no paid
	// payment) or none (no paid payment on record, e.g. cancelled: never
	// paid or refunded, which the record doesn't tell apart).
	Payment string `json:"payment,omitempty"`
}

// setBookingPayment records the looked-up payment state on a booking card.
func (t *toolRun) setBookingPayment(id int64, payment string) {
	for i := range t.bookings {
		if t.bookings[i].BookingID == id {
			t.bookings[i].Payment = payment
		}
	}
}

func (t *toolRun) addBookingCard(b bookings.Booking, summary map[string]any) {
	for _, c := range t.bookings {
		if c.BookingID == b.BookingID {
			return
		}
	}
	c := BookingCard{
		BookingID: b.BookingID, Status: b.Status, CheckIn: b.CheckIn, CheckOut: b.CheckOut,
		Guests: b.Guests, RoomCount: b.RoomCount, TotalAmount: b.TotalAmount, PayAtProperty: b.PayAtProperty,
	}
	if name, ok := summary["hotel_name"].(string); ok {
		c.HotelName = name
	}
	if id, ok := summary["hotel_id"].(int64); ok {
		c.HotelID = id
	}
	if len(t.bookings) < maxBookingResults {
		t.bookings = append(t.bookings, c)
	}
}

func (t *toolRun) bookingStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := t.requireUser(); err != nil {
		return nil, err
	}
	var in struct {
		BookingID int64 `json:"booking_id"`
	}
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	if in.BookingID <= 0 {
		return nil, inputError{"booking_id must be a positive integer"}
	}
	b, err := t.svc.bookings.GetBookingByID(ctx, in.BookingID)
	if errors.Is(err, sql.ErrNoRows) {
		t.bookingsChecked = true
		return bookingNotFoundResult, nil
	}
	if err != nil {
		return nil, err
	}
	// Ownership: a booking that isn't the caller's looks exactly like one
	// that doesn't exist, so ids can't be probed. Admins get no exception
	// here - they have the admin dashboard.
	if b == nil || b.UserID != t.userID {
		t.bookingsChecked = true
		return bookingNotFoundResult, nil
	}

	m := t.bookingSummary(ctx, *b)
	t.addBookingCard(*b, m)
	t.bookingsChecked = true
	t.count = 1
	m["found"] = true
	m["tax_amount"] = b.TaxAmount
	m["discount_amount"] = b.DiscountAmount
	if b.CancellationReason != "" {
		m["cancellation_reason"] = b.CancellationReason
	}

	switch {
	case b.PayAtProperty:
		m["payment"] = "pay_at_property: the guest pays at the hotel; no online payment is expected"
		t.setBookingPayment(b.BookingID, "pay_at_hotel")
	case t.svc.payments == nil:
		m["payment"] = "unknown: payment records are not available to the assistant"
	default:
		paid, err := t.svc.payments.HasPaidPayment(ctx, b.BookingID)
		if err != nil {
			return nil, err
		}
		if paid {
			m["payment"] = "paid: an online payment for this booking is recorded as paid (not refunded)"
			t.setBookingPayment(b.BookingID, "paid")
		} else {
			m["payment"] = "no_paid_payment_on_record: there is no online payment currently recorded as paid. " +
				"For a pending booking this means it is unpaid. For a cancelled booking it can mean it was never paid or that it was refunded - do not say which."
			if b.Status == "pending" {
				t.setBookingPayment(b.BookingID, "unpaid")
			} else {
				t.setBookingPayment(b.BookingID, "none")
			}
		}
	}

	if b.ItemType == "hotel" {
		if h, err := t.svc.hotels.GetHotelByID(ctx, b.ItemID); err == nil && h != nil {
			policy := map[string]any{
				"free_cancellation":  h.FreeCancellation,
				"cancellation_hours": h.CancellationHours,
				"check_in_time":      h.CheckInTime,
				"rule": "With free cancellation, a paid, confirmed booking cancelled at least cancellation_hours before check-in (hotel time, IST) is refunded in full automatically; " +
					"later cancellations, or bookings without free cancellation, are cancelled without a refund. The exact outcome is decided by the system at the moment of cancelling.",
			}
			if b.RatePlanID > 0 {
				policy["caveat"] = "This booking used a rate plan, which can have its own free-cancellation setting; the hotel default above may not apply. Do not promise a refund."
			}
			m["cancellation_policy"] = policy
		}
	}
	return m, nil
}
