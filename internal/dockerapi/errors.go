package dockerapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/ruhuang2001/tiny-sandbox/internal/wirejson"
)

type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("dockerapi: unexpected status %d", e.Status)
	}
	return fmt.Sprintf("dockerapi: status %d: %s", e.Status, e.Message)
}

func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var payload struct {
		Message string `json:"message"`
	}
	msg := string(body)
	if wirejson.Unmarshal(body, &payload) == nil && payload.Message != "" {
		msg = payload.Message
	}
	return &StatusError{Status: resp.StatusCode, Message: msg}
}

func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

func IsConflict(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusConflict
}
