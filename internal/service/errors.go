// Package service is the typed application layer shared by the REST v1 API
// and the MCP transport. Every operation takes an Actor whose identity comes
// only from a validated session or OAuth principal, never from input.
package service

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is the stable, machine-readable error taxonomy shared by REST and MCP.
type Code string

const (
	CodeUnauthenticated   Code = "unauthenticated"
	CodeForbidden         Code = "forbidden"
	CodeInsufficientScope Code = "insufficient_scope"
	CodeNotFound          Code = "not_found"
	CodeInvalidInput      Code = "invalid_input"
	CodeConflict          Code = "conflict"
	CodeInternal          Code = "internal"
)

// HTTPStatus maps a code to the status used by the REST API.
func (c Code) HTTPStatus() int {
	switch c {
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeForbidden, CodeInsufficientScope:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeInvalidInput:
		return http.StatusBadRequest
	case CodeConflict:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// Error is a service failure. Message is safe to show to callers. Cause, when
// set, is for server-side logging only and is never rendered in a response.
type Error struct {
	Code    Code
	Message string
	Cause   error
}

func (e *Error) Error() string {
	return string(e.Code) + ": " + e.Message
}

// Unwrap exposes the internal cause to errors.Is and errors.As.
func (e *Error) Unwrap() error {
	return e.Cause
}

func newError(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func invalidInput(format string, args ...any) *Error {
	return newError(CodeInvalidInput, fmt.Sprintf(format, args...))
}

// internal wraps an unexpected failure. The message stays generic; the cause
// is kept for logs only.
func internal(cause error) *Error {
	return &Error{Code: CodeInternal, Message: "internal error", Cause: cause}
}

// CodeOf returns the taxonomy code for err. Anything that is not a service
// error is reported as internal, so no raw error text reaches a caller.
func CodeOf(err error) Code {
	var se *Error
	if errors.As(err, &se) {
		return se.Code
	}
	return CodeInternal
}

// AsErrorWith builds a caller-facing service error with a fixed code and
// message. Transports use it for input they parse themselves.
func AsErrorWith(code Code, message string) *Error {
	return newError(code, message)
}

// AsError returns the service error for err, converting unknown failures to
// the generic internal error.
func AsError(err error) *Error {
	var se *Error
	if errors.As(err, &se) {
		return se
	}
	return internal(err)
}
