package roomtypes

import "github.com/Maynstayy/ai-chatbot-backend/internal/rooms"

// RoomType is the guest-facing grouping of physical hotel_rooms sharing the
// same room_type_id - one card per type on the hotel detail page, instead of
// one per physical room. AvailableCount is the number of physical rooms of
// this type free for the requested date range (or the raw total when no
// dates were given), computed server-side so it can't be spoofed and so a
// booking can be validated against it.
type RoomType struct {
	RoomTypeID     int64            `json:"room_type_id"`
	HotelID        int64            `json:"hotel_id"`
	HotelName      string           `json:"hotel_name"`
	Name           string           `json:"name"`
	Capacity       int              `json:"capacity"`
	PricePerNight  float64          `json:"price_per_night"`
	Description    string           `json:"description"`
	Amenities      []string         `json:"amenities"`
	Images         []string         `json:"images"`
	BedType        string           `json:"bed_type,omitempty"`
	SizeSqm        *int             `json:"size_sqm,omitempty"`
	ViewType       string           `json:"view_type,omitempty"`
	TotalRooms     int              `json:"total_rooms"`
	AvailableCount int              `json:"available_count"`
	RatePlans      []rooms.RatePlan `json:"rate_plans,omitempty"`
	// SeasonalNote names the festival/seasonal rates applied for the requested
	// dates (empty when none), so the page can explain a changed price.
	SeasonalNote string `json:"seasonal_note,omitempty"`
	// GST on one night at PricePerNight (the type's own price, for types without a plan).
	GSTRate     float64 `json:"gst_rate"`
	TaxPerNight float64 `json:"tax_per_night"`

	// Set only when the request carried ?guests=N: how many rooms of this type
	// N guests need (each sleeps Capacity), whether that many are available for
	// the requested dates, what those rooms cost per night together (cheapest
	// active plan), and which fitting option is the best pick.
	RoomsNeeded        int     `json:"rooms_needed,omitempty"`
	FitsGuests         *bool   `json:"fits_guests,omitempty"`
	ComboPricePerNight float64 `json:"combo_price_per_night,omitempty"`
	Recommended        bool    `json:"recommended,omitempty"`
}
