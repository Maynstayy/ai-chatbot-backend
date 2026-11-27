package utils

import "net/http"

// requestIDer is implemented by the ResponseWriter the request logger wraps
// every response in (internal/app/requestlog.go).
type requestIDer interface{ RequestID() string }

// RequestID returns the id of the request w answers, or "" outside the
// request logger (e.g. in unit tests). Middleware that wraps the writer
// (the HTTP cache, for one) is looked through via Unwrap.
func RequestID(w http.ResponseWriter) string {
	for i := 0; w != nil && i < 8; i++ {
		if r, ok := w.(requestIDer); ok {
			return r.RequestID()
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = u.Unwrap()
	}
	return ""
}
