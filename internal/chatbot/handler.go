package chatbot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Maynstayy/ai-chatbot-backend/pkg/utils"
)

const maxBodyBytes = 8 << 10

// MessageRequest is the body of POST /api/v1/chatbot/message.
type MessageRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id"`
	// conversationId is accepted too, for clients that send camelCase.
	ConversationIDCamel string `json:"conversationId"`
	Language            string `json:"language"`
	// Context is optional: what the guest already chose in the website's
	// guided search. Malformed fields are dropped, never trusted.
	Context *MessageContext `json:"context"`
}

type MessageContext struct {
	Destination string  `json:"destination"`
	CheckIn     string  `json:"check_in"`
	CheckOut    string  `json:"check_out"`
	Guests      int     `json:"guests"`
	Rooms       int     `json:"rooms"`
	BudgetMin   float64 `json:"budget_min"`
	BudgetMax   float64 `json:"budget_max"`
	HotelIDs    []int64 `json:"hotel_ids"`
}

type Handler struct {
	service *Service
	limiter *RateLimiter
}

func NewHandler(service *Service, limiter *RateLimiter) *Handler {
	return &Handler{service: service, limiter: limiter}
}

// POST /api/v1/chatbot/message - public guest chat. The standalone backend
// has no login: every caller is a guest (UserID 0), so the booking tools are
// never offered and booking questions get the sign-in reply.
func (h *Handler) Message(w http.ResponseWriter, r *http.Request) {
	var req MessageRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			utils.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		utils.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.ConversationID == "" {
		req.ConversationID = req.ConversationIDCamel
	}

	in := Input{
		Message:        req.Message,
		ConversationID: req.ConversationID,
		Language:       req.Language,
	}
	if c := req.Context; c != nil {
		in.Context = SearchContext{Destination: c.Destination, CheckIn: c.CheckIn, CheckOut: c.CheckOut,
			Guests: c.Guests, Rooms: c.Rooms, BudgetMin: c.BudgetMin, BudgetMax: c.BudgetMax, HotelIDs: c.HotelIDs}
	}
	if err := h.service.Validate(&in); err != nil {
		utils.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !h.service.Enabled() {
		utils.Error(w, "The chat assistant is not available right now.", http.StatusServiceUnavailable)
		return
	}
	if !h.limiter.Allow(r.Context(), rateKey(in.UserID, r)) {
		w.Header().Set("Retry-After", "60")
		utils.Error(w, "Too many messages. Please wait a minute and try again.", http.StatusTooManyRequests)
		return
	}

	// The server's WriteTimeout (10s) is shorter than an AI reply with tool
	// calls can take; extend it for this response only.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(h.service.Timeout() + 5*time.Second))

	out, err := h.service.HandleMessage(r.Context(), in)
	switch {
	case err == nil:
	case errors.Is(err, ErrTimeout):
		utils.Error(w, "The assistant took too long to respond. Please try again.", http.StatusGatewayTimeout)
		return
	case errors.Is(err, context.Canceled):
		return // client went away
	default:
		// ErrUnavailable and anything unexpected: details are already logged.
		utils.Error(w, "The assistant is temporarily unavailable. Please try again shortly.", http.StatusServiceUnavailable)
		return
	}
	utils.WriteSuccess(w, http.StatusOK, out.Reply, out)
}
