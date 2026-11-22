package sandbox

import (
	"errors"
	"fmt"
	"net/http"
)

var (
	ErrNonZeroExit       = errors.New("sandbox: command exited with a nonzero code")
	ErrTimeout           = errors.New("sandbox: execution timed out")
	ErrCanceled          = errors.New("sandbox: execution canceled")
	ErrUnknown           = errors.New("sandbox: execution termination cause is unknown")
	ErrOOM               = errors.New("sandbox: killed by the kernel OOM killer")
	ErrEvicted           = errors.New("sandbox: container was stopped externally")
	ErrCancelUnconfirmed = errors.New("sandbox: could not confirm the server stopped the work")
	ErrNotFound          = errors.New("sandbox: not found")
	ErrUnauthorized      = errors.New("sandbox: unauthorized")
	ErrBusy              = errors.New("sandbox: another execution is running")
	ErrInvalidState      = errors.New("sandbox: operation invalid in the current state")
)

type Error struct {
	StatusCode     int
	Code           string
	Message        string
	Execution      *Execution
	IdempotencyKey string
	Cause          error
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "sandbox: request failed"
	}
	if e.Cause != nil {
		return msg + ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound || e.Code == "not_found"
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized || e.Code == "unauthorized"
	case ErrBusy:
		return e.Code == "busy"
	case ErrInvalidState:
		return e.Code == "invalid_state"
	case ErrCancelUnconfirmed:
		return e.Code == "cancel_unconfirmed"
	case ErrCanceled:
		return e.Code == string(ReasonCanceled) || e.Code == "canceled"
	case ErrTimeout:
		return e.Code == string(ReasonTimeout)
	case ErrUnknown:
		return e.Code == string(ReasonUnknown)
	case ErrOOM:
		return e.Code == string(ReasonOOM)
	case ErrEvicted:
		return e.Code == string(ReasonEvicted)
	case ErrNonZeroExit:
		if e.Code == "nonzero_exit" {
			return true
		}
		return e.Execution != nil &&
			e.Execution.TerminationReason != nil &&
			*e.Execution.TerminationReason == ReasonExited &&
			e.Execution.ExitCode != nil &&
			*e.Execution.ExitCode != 0
	default:
		return false
	}
}

func (e *Error) Retryable() bool {
	switch e.StatusCode {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return e.StatusCode == 0 && e.Code == "transport"
}

func outcomeError(ex *Execution) error {
	if ex == nil {
		return nil
	}
	if ex.TerminationReason == nil {
		return &Error{Code: "incomplete", Message: "sandbox: execution did not complete", Execution: ex}
	}
	switch *ex.TerminationReason {
	case ReasonCanceled:
		return &Error{Code: string(ReasonCanceled), Message: "sandbox: execution canceled", Execution: ex}
	case ReasonTimeout:
		return &Error{Code: string(ReasonTimeout), Message: "sandbox: execution timed out", Execution: ex}
	case ReasonUnknown:
		return &Error{Code: string(ReasonUnknown), Message: "sandbox: execution termination cause is unknown", Execution: ex}
	case ReasonOOM:
		return &Error{Code: string(ReasonOOM), Message: "sandbox: killed by the kernel OOM killer", Execution: ex}
	case ReasonEvicted:
		return &Error{Code: string(ReasonEvicted), Message: "sandbox: container was stopped externally", Execution: ex}
	}
	if *ex.TerminationReason != ReasonExited {
		return &Error{Code: string(*ex.TerminationReason), Message: "sandbox: unrecognized termination reason", Execution: ex}
	}
	if ex.ExitCode == nil {
		return &Error{Code: "nonzero_exit", Message: "sandbox: the runtime reported no exit code", Execution: ex}
	}
	if *ex.ExitCode != 0 {
		return &Error{
			Code:      "nonzero_exit",
			Message:   fmt.Sprintf("sandbox: command exited %d", *ex.ExitCode),
			Execution: ex,
		}
	}
	return nil
}
