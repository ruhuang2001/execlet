package daemon

import (
	"errors"
	"fmt"
)

type Code string

const (
	CodeInvalidRequest      Code = "invalid_request"
	CodeUnauthorized        Code = "unauthorized"
	CodeNotFound            Code = "not_found"
	CodeInvalidState        Code = "invalid_state"
	CodeBusy                Code = "busy"
	CodeIdempotencyConflict Code = "idempotency_conflict"
	CodePayloadTooLarge     Code = "payload_too_large"
	CodeRateLimited         Code = "rate_limited"
	CodeInternal            Code = "internal"
	CodeBackendError        Code = "backend_error"
	CodeUnavailable         Code = "unavailable"
)

type Error struct {
	Code    Code
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

var ErrNotFound = errors.New("daemon: not found")

var ErrStartUnconfirmed = errors.New("daemon: execution start unconfirmed")

func backendError(err error) error {
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	if errors.Is(err, ErrNotFound) {
		return &Error{Code: CodeNotFound, Message: err.Error(), Err: err}
	}
	return &Error{Code: CodeBackendError, Message: "container runtime failed", Err: err}
}
