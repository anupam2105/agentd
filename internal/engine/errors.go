package engine

import (
	"errors"
	"fmt"
	"net/http"
)

// API error codes.
const (
	ErrCodeNotFound       = "not_found"
	ErrCodeInvalid        = "invalid_request"
	ErrCodeConflict       = "conflict"
	ErrCodeNondeterminism = "nondeterminism"
	ErrCodeRunNotRunning  = "run_not_running"
	ErrCodeInProgress     = "in_progress"
	ErrCodeInternal       = "internal"
)

// Error is an API-facing error with an HTTP status.
type Error struct {
	Status    int    `json:"-"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *Error {
	return errf(http.StatusBadRequest, ErrCodeInvalid, format, args...)
}

func notFound(format string, args ...any) *Error {
	return errf(http.StatusNotFound, ErrCodeNotFound, format, args...)
}

// AsError converts any error to an *Error (internal errors become 500).
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Status: http.StatusInternalServerError, Code: ErrCodeInternal, Message: err.Error()}
}
