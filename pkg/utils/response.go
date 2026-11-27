package utils

import (
	"encoding/json"
	"net/http"
)

type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}

func WriteJSON(w http.ResponseWriter, status int, success bool, message string, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Response{Success: success, Message: message, Data: data})
}

func WriteSuccess(w http.ResponseWriter, status int, message string, data interface{}) {
	WriteJSON(w, status, true, message, data)
}

// Error writes the API's single error shape, {"error": "<message>"}, with
// the given status. Same argument order as http.Error so call sites read the
// same; every handler error, the auth middleware and unmatched routes all
// answer this way, so clients parse one format.
func Error(w http.ResponseWriter, message string, status int) {
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	w.Write([]byte(`{"error":` + jsonString(message) + "}\n"))
}

// WriteError is Error with WriteJSON's argument order.
func WriteError(w http.ResponseWriter, status int, message string) {
	Error(w, message, status)
}

// WriteJSONBody writes v as a bare JSON body (health checks).
func WriteJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
