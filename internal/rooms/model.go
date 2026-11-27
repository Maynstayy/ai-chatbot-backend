package rooms

import (
	"math"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/gst"
)

type Room struct {
	RoomID    int64  `json:"room_id"`
	HotelID   int64  `json:"hotel_id"`
	HotelName string `json:"hotel_name"`
	// RoomTypeID groups this physical room under a shared room_types record
	// (name/price/description/etc). 0 means the room predates room types and
	// hasn't been backfilled/linked yet.
	RoomTypeID    int64      `json:"room_type_id,omitempty"`
	RoomNumber    string     `json:"room_number"`
	RoomType      string     `json:"room_type"`
	Floor         int        `json:"floor"`
	Capacity      int        `json:"capacity"`
	PricePerNight float64    `json:"price_per_night"`
	Description   string     `json:"description"`
	Amenities     []string   `json:"amenities"`
	Images        []string   `json:"images"`
	Status        string     `json:"status"`
	RatePlans     []RatePlan `json:"rate_plans,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`

	// Real per-room-type facts shown as chips on the property page (e.g.
	// "1 King Bed", "22 m2", "Courtyard view"). All optional/nullable -
	// omitted from display rather than guessed when not set by the vendor.
	BedType  string `json:"bed_type,omitempty"`
	SizeSqm  *int   `json:"size_sqm,omitempty"`
	ViewType string `json:"view_type,omitempty"`
}

// RatePlan is a bookable pricing variant of a Room (e.g. "Room Only" vs
// "Breakfast Included"), each with its own price, occupancy, and
// cancellation/payment terms - modeled after Agoda's per-room-type rate rows.
//
// DiscountType/DiscountValue/DiscountValidFrom/DiscountValidTo let a
// vendor/admin configure an automatic promotional price with no coupon
// code: it turns on and expires by itself based on the date window, rather
// than needing someone to toggle it manually. PricePerNight always holds
// the vendor-set base rate; the public listing (see ApplyPublicPricing)
// overwrites it with the discounted price and moves the base rate into
// OriginalPricePerNight so admin/vendor editing screens keep seeing (and
// editing) the undiscounted rate they set.
type RatePlan struct {
	RatePlanID        int64     `json:"rate_plan_id"`
	RoomID            int64     `json:"room_id"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	PricePerNight     float64   `json:"price_per_night"`
	MaxOccupancy      int       `json:"max_occupancy"`
	FreeCancellation  bool      `json:"free_cancellation"`
	PayAtProperty     bool      `json:"pay_at_property"`
	BreakfastIncluded bool      `json:"breakfast_included"`
	Inclusions        []string  `json:"inclusions"`
	IsActive          bool      `json:"is_active"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`

	DiscountType      string  `json:"discount_type"`
	DiscountValue     float64 `json:"discount_value"`
	DiscountValidFrom *string `json:"discount_valid_from,omitempty"`
	DiscountValidTo   *string `json:"discount_valid_to,omitempty"`
	RoomsLeft         *int    `json:"rooms_left,omitempty"`

	// Populated only by ApplyPublicPricing, for guest-facing responses.
	OriginalPricePerNight *float64 `json:"original_price_per_night,omitempty"`
	DiscountPercent       *int     `json:"discount_percent,omitempty"`

	// StayTotal is the exact price of one room on this plan for the dates the
	// guest asked about, set only when a seasonal rate changes some night's
	// price (PricePerNight is then the average night).
	StayTotal *float64 `json:"stay_total,omitempty"`

	// GST on one night at PricePerNight (0 / 5% / 18% by the tariff), so cards can show
	// "+ Rs X taxes". Set by ApplyGST once the guest-facing price is final.
	GSTRate     float64 `json:"gst_rate"`
	TaxPerNight float64 `json:"tax_per_night"`
}

const dateLayout = "2006-01-02"

// parseDay reads the date part of a stored validity bound. The database
// driver hands DATE/DATETIME columns back as "2026-12-31T00:00:00Z" while
// API callers send "2026-12-31"; both must parse, or the bound is silently
// skipped and the discount never expires.
func parseDay(s string) (time.Time, error) {
	if len(s) > len(dateLayout) {
		s = s[:len(dateLayout)]
	}
	return time.Parse(dateLayout, s)
}

// discountActive reports whether rp's discount window contains now (an unset
// bound is treated as open-ended). Callers pass the stay's check-in date when
// the guest has picked dates, so the window describes which stays are
// discounted - not the day the booking happens to be made.
func (rp RatePlan) discountActive(now time.Time) bool {
	if rp.DiscountType == "" || rp.DiscountType == "none" || rp.DiscountValue <= 0 {
		return false
	}
	if rp.DiscountValidFrom != nil {
		if from, err := parseDay(*rp.DiscountValidFrom); err == nil && now.Before(from) {
			return false
		}
	}
	if rp.DiscountValidTo != nil {
		// valid_to is inclusive: the window closes at the start of the next day
		if to, err := parseDay(*rp.DiscountValidTo); err == nil && !now.Before(to.AddDate(0, 0, 1)) {
			return false
		}
	}
	return true
}

// effectivePrice returns the guest-facing price after applying an active
// discount, or the base price unchanged when no discount currently applies.
func (rp RatePlan) effectivePrice(now time.Time) float64 {
	if !rp.discountActive(now) {
		return rp.PricePerNight
	}
	switch rp.DiscountType {
	case "percentage":
		pct := rp.DiscountValue
		if pct > 100 {
			pct = 100
		}
		return round2(rp.PricePerNight * (1 - pct/100))
	case "fixed":
		final := rp.PricePerNight - rp.DiscountValue
		if final < 0 {
			final = 0
		}
		return round2(final)
	default:
		return rp.PricePerNight
	}
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// ApplyGST fills the plan's GST fields from its current (guest-facing) nightly price.
func ApplyGST(rp *RatePlan) {
	rp.GSTRate = gst.Rate(rp.PricePerNight)
	rp.TaxPerNight = gst.TaxPerNight(rp.PricePerNight)
}

// ApplyPublicPricing mutates rp in place so the guest-facing JSON carries
// the discounted price as price_per_night, with the vendor-set base rate
// moved to original_price_per_night and the equivalent percentage surfaced
// for display - a no-op when no discount is currently active.
func ApplyPublicPricing(rp *RatePlan) {
	ApplyPublicPricingOn(rp, todayInIndia())
}

// ApplyPublicPricingOn is ApplyPublicPricing for a stay that starts on
// checkIn: the discount applies only if checkIn falls inside its window.
func ApplyPublicPricingOn(rp *RatePlan, checkIn time.Time) {
	now := checkIn
	if !rp.discountActive(now) {
		return
	}
	base := rp.PricePerNight
	effective := rp.effectivePrice(now)
	if effective >= base {
		return
	}
	pct := int(math.Round((1 - effective/base) * 100))
	rp.OriginalPricePerNight = &base
	rp.DiscountPercent = &pct
	rp.PricePerNight = effective
}

// EffectivePriceForBooking returns the nightly price to charge for rp on a
// stay starting on checkIn, applying the discount if checkIn falls inside its
// window - the same rule ApplyPublicPricingOn uses for display, so the amount
// charged matches what the guest was shown for those dates.
func EffectivePriceForBooking(rp RatePlan, checkIn time.Time) float64 {
	return rp.effectivePrice(checkIn)
}

// todayInIndia is the current calendar date in India as a UTC-midnight
// time, comparable with the parsed valid_from/valid_to dates. Used when no
// stay dates are known yet.
func todayInIndia() time.Time {
	n := time.Now().In(time.FixedZone("IST", 5*3600+30*60))
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}
