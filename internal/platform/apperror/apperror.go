// Package apperror defines the typed errors shared by all modules.
//
// Errors are matched by Code, not by pointer identity. That is what lets an
// error rebuilt from a remote (RPC) response still satisfy
// errors.Is(err, somecontract.ErrNotFound), so callers behave the same whether
// a module runs in-process or in another service.
package apperror

import (
	"errors"
	"net/http"
)

type Kind string

const (
	KindInvalid         Kind = "invalid"
	KindUnauthorized    Kind = "unauthorized"
	KindForbidden       Kind = "forbidden"
	KindNotFound        Kind = "not_found"
	KindConflict        Kind = "conflict"
	KindUnprocessable   Kind = "unprocessable"
	KindUnavailable     Kind = "unavailable"
	KindTooManyRequests Kind = "too_many_requests"
	KindInternal        Kind = "internal"
)

type Error struct {
	Kind    Kind   `json:"kind"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Is matches on Code so local and remote errors compare equal.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// WithMessage returns a copy with a more specific message; the Code (and so
// errors.Is behaviour) is unchanged.
func (e *Error) WithMessage(msg string) *Error {
	c := *e
	c.Message = msg
	return &c
}

func New(kind Kind, code, msg string) *Error { return &Error{Kind: kind, Code: code, Message: msg} }

func Invalid(code, msg string) *Error       { return New(KindInvalid, code, msg) }
func Unauthorized(code, msg string) *Error  { return New(KindUnauthorized, code, msg) }
func Forbidden(code, msg string) *Error     { return New(KindForbidden, code, msg) }
func NotFound(code, msg string) *Error      { return New(KindNotFound, code, msg) }
func Conflict(code, msg string) *Error      { return New(KindConflict, code, msg) }
func Unprocessable(code, msg string) *Error { return New(KindUnprocessable, code, msg) }
func Unavailable(code, msg string) *Error   { return New(KindUnavailable, code, msg) }

var internalError = New(KindInternal, "internal_error", "an internal error occurred")

// From converts any error into an *Error. Unknown errors become a generic
// internal error so internal details never leak to API clients.
func From(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return internalError
}

func Status(kind Kind) int {
	switch kind {
	case KindInvalid:
		return http.StatusBadRequest
	case KindUnauthorized:
		return http.StatusUnauthorized
	case KindForbidden:
		return http.StatusForbidden
	case KindNotFound:
		return http.StatusNotFound
	case KindConflict:
		return http.StatusConflict
	case KindUnprocessable:
		return http.StatusUnprocessableEntity
	case KindUnavailable:
		return http.StatusBadGateway
	case KindTooManyRequests:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}
