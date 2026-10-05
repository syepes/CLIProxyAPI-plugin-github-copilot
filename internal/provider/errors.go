package provider

import (
	"fmt"
	"net/http"
	"strings"
)

type StatusError struct {
	Code       string
	Message    string
	HTTPStatus int
	Retryable  bool
}

func (e *StatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func statusError(code, message string, status int) error {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return &StatusError{Code: code, Message: message, HTTPStatus: status}
}

func upstreamStatusError(status int, detail string) error {
	message := fmt.Sprintf("Copilot upstream returned HTTP %d", status)
	if detail = strings.TrimSpace(detail); detail != "" {
		message = fmt.Sprintf("Copilot upstream returned HTTP %d: %s", status, detail)
	}
	if status < 400 || status > 599 {
		status = 502
	}
	return &StatusError{
		Code:       "upstream_error",
		Message:    message,
		HTTPStatus: status,
		Retryable:  false, // Do not ask the host to replay potentially billed inference.
	}
}
