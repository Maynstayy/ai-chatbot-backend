package utils

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestSafeMessage(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		status     int
		wantStatus int
		leaks      bool // true = the original message must be passed through
	}{
		{"validation message passes through", errors.New("check_out must be after check_in"), http.StatusBadRequest, http.StatusBadRequest, true},
		{"permission message passes through", errors.New("you do not have permission to modify this room"), http.StatusForbidden, http.StatusForbidden, true},
		{"mysql error sent as 400 becomes generic 500", &mysql.MySQLError{Number: 1055, Message: "Expression #1 of SELECT list is not in GROUP BY clause"}, http.StatusBadRequest, http.StatusInternalServerError, false},
		{"wrapped mysql error", fmt.Errorf("load trend: %w", &mysql.MySQLError{Number: 1146, Message: "Table 'maynstay.x' doesn't exist"}), http.StatusBadRequest, http.StatusInternalServerError, false},
		{"sql.ErrNoRows", sql.ErrNoRows, http.StatusNotFound, http.StatusInternalServerError, false},
		{"flattened driver text", errors.New("Error 1055 (42000): Expression #1 of SELECT list"), http.StatusBadRequest, http.StatusInternalServerError, false},
		{"connection failure text", errors.New("dial tcp 127.0.0.1:3306: connect: connection refused"), http.StatusConflict, http.StatusInternalServerError, false},
		{"any 500 is generic", errors.New("something specific"), http.StatusInternalServerError, http.StatusInternalServerError, false},
	}
	for _, tc := range tests {
		status, msg := SafeMessage("test", tc.err, tc.status)
		if status != tc.wantStatus {
			t.Errorf("%s: status %d, want %d", tc.name, status, tc.wantStatus)
		}
		if passed := msg == tc.err.Error(); passed != tc.leaks {
			t.Errorf("%s: message %q (passed through: %v, want %v)", tc.name, msg, passed, tc.leaks)
		}
	}
}

func TestSafeJSONErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	SafeJSONError(rec, "test", errors.New(`bad "quote"`), http.StatusBadRequest)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("got %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"bad \"quote\""}` {
		t.Errorf("body %s", got)
	}
}
