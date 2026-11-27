//go:build ignore

// Server-side chat history (from MaynStay feature/ai-chatbot), kept for a
// later version and excluded from the default build: V1 keeps history in
// the browser. Its owner is still the MaynStay signed-in user
// (middleware.GetSignupIDFromCtx); enabling it needs a guest identity first.

package chatbot

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Maynstayy/ai-chatbot-backend/internal/middleware"
	"github.com/Maynstayy/ai-chatbot-backend/pkg/utils"
)

// History requests carry up to maxMessagesPerSave messages of at most
// maxHistoryMessageRune characters each.
const maxHistoryBodyBytes = 256 << 10

// HistoryHandler serves the signed-in guest's chat history. Every route is
// behind RequireAuth; the owner always comes from the verified token.
type HistoryHandler struct {
	svc     *HistoryService
	limiter *RateLimiter
}

func NewHistoryHandler(svc *HistoryService, limiter *RateLimiter) *HistoryHandler {
	return &HistoryHandler{svc: svc, limiter: limiter}
}

// RegisterHistoryRoutes adds the history API. Rename is PUT (the API's CORS
// policy allows GET, POST, PUT and DELETE).
func RegisterHistoryRoutes(mux *http.ServeMux, h *HistoryHandler, requireAuth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/v1/chatbot/conversations", requireAuth(h.List))
	mux.HandleFunc("POST /api/v1/chatbot/conversations", requireAuth(h.Create))
	mux.HandleFunc("GET /api/v1/chatbot/conversations/{id}/messages", requireAuth(h.Messages))
	mux.HandleFunc("POST /api/v1/chatbot/conversations/{id}/messages", requireAuth(h.Append))
	mux.HandleFunc("PUT /api/v1/chatbot/conversations/{id}", requireAuth(h.Rename))
	mux.HandleFunc("DELETE /api/v1/chatbot/conversations/{id}", requireAuth(h.Delete))
}

// owner is the signed-in caller, after the rate limit; 0 means the response
// has already been written.
func (h *HistoryHandler) owner(w http.ResponseWriter, r *http.Request) int64 {
	owner := int64(middleware.GetSignupIDFromCtx(r.Context()))
	if owner <= 0 {
		utils.Error(w, "Unauthorized", http.StatusUnauthorized)
		return 0
	}
	if !h.limiter.Allow(r.Context(), "history:"+rateKey(owner, r)) {
		w.Header().Set("Retry-After", "60")
		utils.Error(w, "Too many requests. Please wait a minute and try again.", http.StatusTooManyRequests)
		return 0
	}
	return owner
}

func writeHistoryError(w http.ResponseWriter, err error) {
	var input HistoryInputError
	switch {
	case errors.As(err, &input):
		utils.Error(w, input.Msg, http.StatusBadRequest)
	case errors.Is(err, ErrHistoryNotFound):
		utils.Error(w, "Conversation not found.", http.StatusNotFound)
	default:
		utils.Error(w, "Chat history is temporarily unavailable. Please try again shortly.", http.StatusServiceUnavailable)
	}
}

func queryInt(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	return n
}

func decodeHistoryBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxHistoryBodyBytes))
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			utils.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			utils.Error(w, "invalid request body", http.StatusBadRequest)
		}
		return false
	}
	return true
}

// GET /api/v1/chatbot/conversations?q=&cursor=&limit=
func (h *HistoryHandler) List(w http.ResponseWriter, r *http.Request) {
	owner := h.owner(w, r)
	if owner == 0 {
		return
	}
	q := r.URL.Query()
	list, next, err := h.svc.List(r.Context(), owner, q.Get("q"), q.Get("cursor"), int(queryInt(r, "limit")))
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	utils.WriteSuccess(w, http.StatusOK, "", map[string]any{"conversations": list, "next_cursor": next})
}

// POST /api/v1/chatbot/conversations
func (h *HistoryHandler) Create(w http.ResponseWriter, r *http.Request) {
	owner := h.owner(w, r)
	if owner == 0 {
		return
	}
	c, err := h.svc.Create(r.Context(), owner)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	utils.WriteSuccess(w, http.StatusCreated, "", map[string]any{"conversation": c})
}

// GET /api/v1/chatbot/conversations/{id}/messages?before=&limit=
func (h *HistoryHandler) Messages(w http.ResponseWriter, r *http.Request) {
	owner := h.owner(w, r)
	if owner == 0 {
		return
	}
	c, msgs, more, err := h.svc.Messages(r.Context(), owner, r.PathValue("id"), queryInt(r, "before"), int(queryInt(r, "limit")))
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	data := map[string]any{"conversation": c, "messages": msgs, "has_more": more}
	if more && len(msgs) > 0 {
		data["next_before"] = msgs[0].ID
	}
	utils.WriteSuccess(w, http.StatusOK, "", data)
}

// POST /api/v1/chatbot/conversations/{id}/messages {"messages": [...]}
func (h *HistoryHandler) Append(w http.ResponseWriter, r *http.Request) {
	owner := h.owner(w, r)
	if owner == 0 {
		return
	}
	var body struct {
		Messages []NewMessage `json:"messages"`
	}
	if !decodeHistoryBody(w, r, &body) {
		return
	}
	c, saved, err := h.svc.Append(r.Context(), owner, r.PathValue("id"), body.Messages)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	utils.WriteSuccess(w, http.StatusOK, "", map[string]any{"conversation": c, "saved": saved})
}

// PUT /api/v1/chatbot/conversations/{id} {"title": "..."}
func (h *HistoryHandler) Rename(w http.ResponseWriter, r *http.Request) {
	owner := h.owner(w, r)
	if owner == 0 {
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if !decodeHistoryBody(w, r, &body) {
		return
	}
	c, err := h.svc.Rename(r.Context(), owner, r.PathValue("id"), body.Title)
	if err != nil {
		writeHistoryError(w, err)
		return
	}
	utils.WriteSuccess(w, http.StatusOK, "", map[string]any{"conversation": c})
}

// DELETE /api/v1/chatbot/conversations/{id}
func (h *HistoryHandler) Delete(w http.ResponseWriter, r *http.Request) {
	owner := h.owner(w, r)
	if owner == 0 {
		return
	}
	if err := h.svc.Delete(r.Context(), owner, r.PathValue("id")); err != nil {
		writeHistoryError(w, err)
		return
	}
	utils.WriteSuccess(w, http.StatusOK, "", map[string]any{"deleted": true})
}
