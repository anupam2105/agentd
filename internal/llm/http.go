package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const maxResponseBytes = 8 << 20

// postJSON sends body and decodes a 2xx response into out, classifying failures.
func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return &Error{Msg: "encode request: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return &Error{Msg: "build request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return &Error{Retryable: true, Msg: "request failed: " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return &Error{Retryable: true, Msg: "read response: " + err.Error()}
	}
	if resp.StatusCode/100 != 2 {
		return &Error{
			Status:    resp.StatusCode,
			Retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			Msg:       errorMessage(data),
		}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Msg: "decode response: " + err.Error()}
	}
	return nil
}

func errorMessage(data []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	if len(data) > 300 {
		data = data[:300]
	}
	return string(data)
}

// IsRetryable reports whether err is a retryable provider error.
func IsRetryable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable
	}
	return true
}
