package utils

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"

	"github.com/go-sql-driver/mysql"
)

const genericErrorMessage = "Something went wrong on our end. Please try again in a moment."

// ServerError logs the real error server-side (with a short context label so
// it can be found in the logs) and returns a generic, non-technical message
// to the client. Use it for every unexpected/internal failure so that SQL
// driver strings, connection errors and stack traces never reach end users.
func ServerError(w http.ResponseWriter, context string, err error) {
	logInternal(RequestID(w), context, err)
	Error(w, genericErrorMessage, http.StatusInternalServerError)
}

// logInternal records an internal failure. The request id ties it to the
// request's access log line (and to the X-Request-ID the client got back).
func logInternal(requestID, context string, err error) {
	slog.Error("internal error", "where", context, "err", err.Error(), "request_id", requestID)
}

// internalLeak matches text that only an internal failure produces. It backs
// up the type checks in isInternal for errors that were flattened into a
// string (fmt.Errorf with %v) before reaching the handler.
var internalLeak = regexp.MustCompile(`(?i)\b(sql|mysql|panic|goroutine|runtime error|nil pointer|invalid memory address|dial tcp|connection refused|no such host|broken pipe|bad connection|invalid connection|deadline exceeded)\b|Error \d{4} \(`)

// isInternal reports whether err is a database, network or runtime failure
// rather than a message written for the caller (a validation or permission
// error). Handlers used to send err.Error() for both.
func isInternal(err error) bool {
	var mysqlErr *mysql.MySQLError
	var netErr net.Error
	switch {
	case errors.As(err, &mysqlErr), errors.As(err, &netErr):
		return true
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, sql.ErrConnDone), errors.Is(err, sql.ErrTxDone):
		return true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return true
	}
	return internalLeak.MatchString(err.Error())
}

// SafeMessage returns the status and message that are safe to send for err:
// err's own message with the given status when it was meant for the caller,
// otherwise a generic 500 (with the real error logged under context).
func SafeMessage(context string, err error, status int) (int, string) {
	return safeMessage("", context, err, status)
}

func safeMessage(requestID, context string, err error, status int) (int, string) {
	if status >= http.StatusInternalServerError || isInternal(err) {
		logInternal(requestID, context, err)
		return http.StatusInternalServerError, genericErrorMessage
	}
	return status, err.Error()
}

// SafeError is Error(w, err.Error(), status) without the leak.
func SafeError(w http.ResponseWriter, context string, err error, status int) {
	status, msg := safeMessage(RequestID(w), context, err, status)
	Error(w, msg, status)
}

// SafeJSONError is SafeError; every error is JSON now. Kept so existing call
// sites read unchanged.
func SafeJSONError(w http.ResponseWriter, context string, err error, status int) {
	SafeError(w, context, err, status)
}

// WriteSafeError is WriteError(w, status, err.Error()) through SafeMessage.
func WriteSafeError(w http.ResponseWriter, context string, err error, status int) {
	status, msg := SafeMessage(context, err, status)
	WriteError(w, status, msg)
}
