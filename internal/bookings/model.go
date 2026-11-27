// Package bookings holds only the MaynStay booking record shape that the
// chatbot's booking tools are typed against (internal/chatbot/tools.go:
// BookingSource). The standalone backend has no login, no booking system
// and no payment system: no BookingSource or PaymentSource is wired, and the
// booking tools are offered only to a signed-in user, which never happens
// here, so guests always get the "sign in on the booking website" reply.
// Kept so the transferred chatbot code and its tests stay intact for a later
// integration with SKYMCM's own sign-in.
package bookings

import "time"

type Booking struct {
	BookingID      int64   `json:"booking_id"`
	UserID         int64   `json:"user_id"`
	ItemType       string  `json:"item_type"`
	ItemID         int64   `json:"item_id"`
	RoomID         int64   `json:"room_id,omitempty"`
	RatePlanID     int64   `json:"rate_plan_id,omitempty"`
	RoomTypeID     int64   `json:"room_type_id,omitempty"`
	CheckIn        string  `json:"check_in"`
	CheckOut       string  `json:"check_out"`
	Guests         int     `json:"guests"`
	RoomCount      int     `json:"room_count"`
	TotalAmount    float64 `json:"total_amount"`
	DiscountID     int64   `json:"discount_id,omitempty"`
	DiscountAmount float64 `json:"discount_amount"`
	Status         string  `json:"status"`

	TaxAmount  float64 `json:"tax_amount"`
	CGSTAmount float64 `json:"cgst_amount"`
	SGSTAmount float64 `json:"sgst_amount"`

	PayAtProperty      bool      `json:"pay_at_property"`
	CancellationReason string    `json:"cancellation_reason,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}
