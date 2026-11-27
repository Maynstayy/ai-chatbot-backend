package hotels

import "time"

type Hotel struct {
	HotelID          int64     `json:"hotel_id"`
	Name             string    `json:"name"`
	Location         string    `json:"location"`
	Description      string    `json:"description"`
	City             string    `json:"city"`
	State            string    `json:"state"`
	Country          string    `json:"country"`
	Address          string    `json:"address"`
	Latitude         *float64  `json:"latitude,omitempty"`
	Longitude        *float64  `json:"longitude,omitempty"`
	Category         string    `json:"category"`
	PricePerNight    float64   `json:"price_per_night"`
	Rating           float64   `json:"rating"`
	TotalReviews     int       `json:"total_reviews"`
	Amenities        []string  `json:"amenities"`
	ThumbnailURL     string    `json:"thumbnail_url"`
	Images           []string  `json:"images"`
	OwnerID          int64     `json:"owner_id"`
	IsActive         bool      `json:"is_active"`
	IsDeleted        bool      `json:"is_deleted"`
	FreeCancellation bool      `json:"free_cancellation"`
	PayAtProperty    bool      `json:"pay_at_property"`
	InstantBooking   bool      `json:"instant_booking"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`

	// Guest-facing policy fields, editable by the owning vendor or an admin.
	CancellationHours *int   `json:"cancellation_hours,omitempty"`
	ChildPolicy       string `json:"child_policy,omitempty"`
	PetPolicy         string `json:"pet_policy,omitempty"`
	IDRules           string `json:"id_rules,omitempty"`
	// The hotel's GST registration number (printed on receipts).
	GSTIN string `json:"gstin,omitempty"`

	// GST on one night at the listed ("from") price: gst_rate is 0, 0.05 or 0.18 and
	// tax_per_night the rupee amount, so cards can show "+ Rs X taxes". The booking itself
	// slabs each room-night on the price actually charged.
	GSTRate     float64 `json:"gst_rate"`
	TaxPerNight float64 `json:"tax_per_night"`

	// Populated by a join against vendor_settings (keyed by owner_id) - a
	// vendor with multiple properties currently shares one check-in/out time
	// and tax rate across all of them, since vendor_settings is per-account.
	TaxRate      float64 `json:"tax_rate"`
	CheckInTime  string  `json:"check_in_time"`
	CheckOutTime string  `json:"check_out_time"`

	// BookedLast24h is a real count from the bookings table, only populated
	// on the single-hotel lookup (GetHotelByID), not the list endpoint.
	BookedLast24h int `json:"booked_last_24h,omitempty"`
}
