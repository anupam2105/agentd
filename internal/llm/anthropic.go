package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Anthropic calls the Anthropic Messages API.
type Anthropic struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

// NewAnthropic builds the provider; baseURL defaults to https://api.anthropic.com.
func NewAnthropic(apiKey, baseURL string, timeout time.Duration) *Anthropic {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	return &Anthropic{APIKey: apiKey, BaseURL: strings.TrimRight(baseURL, "/"), Client: &http.Client{Timeout: timeout}}
}

// Name implements Provider.
func (*Anthropic) Name() string { return "anthropic" }

// Complete implements Provider.
func (a *Anthropic) Complete(ctx context.Context, req *Request) (*Response, error) {
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"messages":   req.Messages,
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	var out struct {
		Model      string  `json:"model"`
		Content    []Block `json:"content"`
		StopReason string  `json:"stop_reason"`
		Usage      Usage   `json:"usage"`
	}
	err := postJSON(ctx, a.Client, a.BaseURL+"/v1/messages", map[string]string{
		"x-api-key":         a.APIKey,
		"anthropic-version": "2023-06-01",
	}, body, &out)
	if err != nil {
		return nil, err
	}
	content := make([]Block, 0, len(out.Content))
	for i := range out.Content {
		b := &out.Content[i]
		if b.Type == "text" || b.Type == "tool_use" {
			if b.Type == "tool_use" && len(b.Input) == 0 {
				b.Input = json.RawMessage(`{}`)
			}
			content = append(content, *b)
		}
	}
	return &Response{Model: out.Model, Content: content, StopReason: out.StopReason, Usage: out.Usage}, nil
}
