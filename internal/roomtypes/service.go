package roomtypes

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/internal/gst"
	"github.com/Maynstayy/ai-chatbot-backend/internal/rooms"
)

var ErrInvalidDateFmt = errors.New("check_in/check_out must be in YYYY-MM-DD format")
var ErrInvalidGuests = errors.New("guests must be a whole number between 1 and 200")

// MaxGuestsQuery caps the guests query so a huge number can't be used to
// generate an unbounded range of cache keys.
const MaxGuestsQuery = 200

const dateLayout = "2006-01-02"

type Service interface {
	// guests is optional (0 = not given); when positive each room type also
	// reports how many rooms those guests need and whether they fit.
	GetRoomTypesByHotel(ctx context.Context, hotelID int64, checkIn, checkOut *string, guests int) ([]RoomType, error)
}

type service struct {
	repo Repository
}

func NewService(repo Repository) Service {
	return &service{repo: repo}
}

// GetRoomTypesByHotel is the public, unauthenticated listing used by the
// customer-facing hotel detail page. checkIn/checkOut are optional; when
// both are given, AvailableCount is scoped to that date range so the
// "Rooms" selector on the detail page has something real to validate
// against.
func (s *service) GetRoomTypesByHotel(ctx context.Context, hotelID int64, checkIn, checkOut *string, guests int) ([]RoomType, error) {
	if err := validateDatePair(checkIn, checkOut); err != nil {
		return nil, err
	}
	if guests < 0 || guests > MaxGuestsQuery {
		return nil, ErrInvalidGuests
	}
	types, err := s.repo.GetRoomTypesByHotelID(ctx, hotelID, checkIn, checkOut)
	if err != nil {
		return nil, err
	}
	// A plan's discount is judged against the check-in date once the guest
	// has picked dates, and against today before that.
	var stayStart *time.Time
	if checkIn != nil {
		if t, err := time.Parse(dateLayout, *checkIn); err == nil {
			stayStart = &t
		}
	}
	for i := range types {
		for j := range types[i].RatePlans {
			if stayStart != nil {
				rooms.ApplyPublicPricingOn(&types[i].RatePlans[j], *stayStart)
			} else {
				rooms.ApplyPublicPricing(&types[i].RatePlans[j])
			}
		}
	}
	// Seasonal/festival rates are not part of the standalone catalogue, so
	// prices are the plans' own (after any active discount).
	// GST is shown on the final nightly prices, so it comes after discounts.
	for i := range types {
		types[i].GSTRate = gst.Rate(types[i].PricePerNight)
		types[i].TaxPerNight = gst.TaxPerNight(types[i].PricePerNight)
		for j := range types[i].RatePlans {
			rooms.ApplyGST(&types[i].RatePlans[j])
		}
	}
	if guests > 0 {
		applyGuestFit(types, guests)
	}
	return types, nil
}

// applyGuestFit works out, per room type, how many rooms `guests` people need
// (each room sleeps Capacity), whether that many are available, and the
// nightly cost of those rooms together. The cheapest fitting option (fewest
// rooms on a tie) is marked Recommended, and the list is ordered best first:
// fitting options by total price, then the ones that don't fit.
func applyGuestFit(types []RoomType, guests int) {
	best := -1
	for i := range types {
		t := &types[i]
		capacity := t.Capacity
		if capacity < 1 {
			capacity = 1
		}
		t.RoomsNeeded = (guests + capacity - 1) / capacity
		fits := t.AvailableCount >= t.RoomsNeeded
		t.FitsGuests = &fits
		t.ComboPricePerNight = math.Round(float64(t.RoomsNeeded)*cheapestNightly(t)*100) / 100
		if !fits {
			continue
		}
		if best < 0 || t.ComboPricePerNight < types[best].ComboPricePerNight ||
			(t.ComboPricePerNight == types[best].ComboPricePerNight && t.RoomsNeeded < types[best].RoomsNeeded) {
			best = i
		}
	}
	if best >= 0 {
		types[best].Recommended = true
	}
	sort.SliceStable(types, func(a, b int) bool {
		fa, fb := *types[a].FitsGuests, *types[b].FitsGuests
		if fa != fb {
			return fa
		}
		if types[a].ComboPricePerNight != types[b].ComboPricePerNight {
			return types[a].ComboPricePerNight < types[b].ComboPricePerNight
		}
		return types[a].RoomsNeeded < types[b].RoomsNeeded
	})
}

// cheapestNightly is the lowest nightly price a guest can book this room type
// at: its cheapest active rate plan, or the room type's own price when it has
// no plan.
func cheapestNightly(t *RoomType) float64 {
	price := t.PricePerNight
	first := true
	for _, p := range t.RatePlans {
		if !p.IsActive {
			continue
		}
		if first || p.PricePerNight < price {
			price = p.PricePerNight
			first = false
		}
	}
	return price
}

func validateDatePair(checkIn, checkOut *string) error {
	if checkIn == nil && checkOut == nil {
		return nil
	}
	if checkIn == nil || checkOut == nil {
		return ErrInvalidDateFmt
	}
	if _, err := time.Parse(dateLayout, *checkIn); err != nil {
		return ErrInvalidDateFmt
	}
	if _, err := time.Parse(dateLayout, *checkOut); err != nil {
		return ErrInvalidDateFmt
	}
	return nil
}
