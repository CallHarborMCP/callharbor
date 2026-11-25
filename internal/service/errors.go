// Package service is the typed application layer shared by the REST v1 API
// and the MCP transport. Every operation takes an Actor whose identity comes
// only from a validated session or OAuth principal, never from input.
package service

import (
	"errors"
	"fmt"
	"log/slog"
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

// Envelope is the error body shared by REST and MCP tool errors. Its JSON form
// is {"error":{"code":...,"message":...}}. Cause is never part of it.
type Envelope struct {
	Error EnvelopeDetail `json:"error"`
}

// EnvelopeDetail carries the stable code and the caller-safe message.
type EnvelopeDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// EnvelopeOf converts err to the shared envelope. Unknown errors and internal
// failures always carry the generic message, never the cause text.
func EnvelopeOf(err error) Envelope {
	se := AsError(err)
	return Envelope{Error: EnvelopeDetail{Code: string(se.Code), Message: se.Message}}
}

// LogCause records the server-side cause of an internal failure. Only the
// code, the operation label and the cause text are logged. Callers pass no
// tokens, secrets or request bodies, because the operation layer never holds
// them.
func LogCause(logger *slog.Logger, transport string, err error) {
	if logger == nil {
		return
	}
	se := AsError(err)
	if se.Code != CodeInternal {
		return
	}
	cause := se.Cause
	if cause == nil {
		cause = se
	}
	logger.Error("request failed", "transport", transport, "code", string(se.Code), "cause", cause.Error())
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
