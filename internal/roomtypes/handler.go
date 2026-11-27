package roomtypes

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Maynstayy/ai-chatbot-backend/pkg/utils"
)

type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

// GET /api/v1/room-types?hotel_id={id}&check_in=YYYY-MM-DD&check_out=YYYY-MM-DD&guests=N
// check_in/check_out are optional; when omitted, available_count is the raw
// count of physical rooms currently marked Available (no date filtering).
// guests is optional; when given, each type carries rooms_needed, fits_guests,
// combo_price_per_night and recommended, and the list is ordered best first.
func (h *Handler) GetRoomTypesByHotel(w http.ResponseWriter, r *http.Request) {
	hotelID, err := strconv.ParseInt(r.URL.Query().Get("hotel_id"), 10, 64)
	if err != nil {
		utils.Error(w, "hotel_id is required", http.StatusBadRequest)
		return
	}

	var checkIn, checkOut *string
	if v := r.URL.Query().Get("check_in"); v != "" {
		checkIn = &v
	}
	if v := r.URL.Query().Get("check_out"); v != "" {
		checkOut = &v
	}

	// Optional ?guests=N: also report how many rooms of each type N guests need.
	guests := 0
	if v := r.URL.Query().Get("guests"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			utils.SafeError(w, "roomtypes", ErrInvalidGuests, http.StatusBadRequest)
			return
		}
		guests = n
	}

	types, err := h.service.GetRoomTypesByHotel(r.Context(), hotelID, checkIn, checkOut, guests)
	if err != nil {
		if errors.Is(err, ErrInvalidDateFmt) || errors.Is(err, ErrInvalidGuests) {
			utils.SafeError(w, "roomtypes", err, http.StatusBadRequest)
			return
		}
		utils.SafeError(w, "roomtypes", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(types)
}
