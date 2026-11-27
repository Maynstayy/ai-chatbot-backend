package chatbot

import "net/http"

// RegisterRoutes adds the public chat endpoint. MaynStay wrapped it in
// OptionalAuth; the standalone backend has no login, so it is plain public.
func RegisterRoutes(mux *http.ServeMux, handler *Handler) {
	mux.HandleFunc("POST /api/v1/chatbot/message", handler.Message)
}
