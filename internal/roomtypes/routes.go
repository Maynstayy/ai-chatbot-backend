package roomtypes

import "net/http"

func RegisterRoutes(mux *http.ServeMux, handler *Handler) {
	mux.HandleFunc("GET /api/v1/room-types", handler.GetRoomTypesByHotel)
}
