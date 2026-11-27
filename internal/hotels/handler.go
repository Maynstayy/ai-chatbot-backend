package hotels

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Maynstayy/ai-chatbot-backend/pkg/utils"
)

// Handler serves the public, read-only hotel endpoints the chatbot frontend
// calls (adapted from MaynStay's internal/hotels handler).
type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

// GET /api/v1/hotels?city=&search=&category=&min_price=&max_price=&sort=
// Returns a bare JSON array of listed (active) hotels.
func (h *Handler) GetHotels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	active := true
	filter := HotelFilter{
		Category: q.Get("category"),
		City:     q.Get("city"),
		Search:   q.Get("search"),
		Sort:     q.Get("sort"),
		// Only listed hotels are public (MaynStay let admins see the rest).
		IsActive: &active,
	}
	if v := q.Get("min_price"); v != "" {
		if p, err := strconv.ParseFloat(v, 64); err == nil {
			filter.MinPrice = p
		}
	}
	if v := q.Get("max_price"); v != "" {
		if p, err := strconv.ParseFloat(v, 64); err == nil {
			filter.MaxPrice = p
		}
	}

	hotels, err := h.service.GetHotels(r.Context(), filter)
	if err != nil {
		utils.ServerError(w, "hotels", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(hotels)
}

// GET /api/v1/hotels/{id}
func (h *Handler) GetHotelByID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		utils.Error(w, "invalid hotel id", http.StatusBadRequest)
		return
	}
	hotel, err := h.service.GetHotelByID(r.Context(), id)
	if err != nil || hotel == nil || !hotel.IsActive {
		utils.Error(w, "hotel not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(hotel)
}

func RegisterRoutes(mux *http.ServeMux, handler *Handler) {
	mux.HandleFunc("GET /api/v1/hotels", handler.GetHotels)
	mux.HandleFunc("GET /api/v1/hotels/{id}", handler.GetHotelByID)
}
