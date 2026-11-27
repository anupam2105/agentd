package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// OpenAI calls an OpenAI-compatible Chat Completions API (OpenAI, Azure
// OpenAI-compatible gateways, Ollama, vLLM, LiteLLM...).
type OpenAI struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

// NewOpenAI builds the provider; baseURL defaults to https://api.openai.com/v1.
func NewOpenAI(apiKey, baseURL string, timeout time.Duration) *OpenAI {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAI{APIKey: apiKey, BaseURL: strings.TrimRight(baseURL, "/"), Client: &http.Client{Timeout: timeout}}
}

// Name implements Provider.
func (*OpenAI) Name() string { return "openai" }

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    *string       `json:"content"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

// toOpenAIMessages translates canonical messages to Chat Completions messages.
func toOpenAIMessages(system string, msgs []Message) ([]oaiMessage, error) {
	var out []oaiMessage
	if system != "" {
		out = append(out, oaiMessage{Role: "system", Content: &system})
	}
	for _, m := range msgs {
		blocks, err := ParseContent(m.Content)
		if err != nil {
			return nil, err
		}
		var text strings.Builder
		var calls []oaiToolCall
		for i := range blocks {
			b := &blocks[i]
			switch b.Type {
			case "text":
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(b.Text)
			case "tool_use":
				c := oaiToolCall{ID: b.ID, Type: "function"}
				c.Function.Name = b.Name
				c.Function.Arguments = string(b.Input)
				if c.Function.Arguments == "" {
					c.Function.Arguments = "{}"
				}
				calls = append(calls, c)
			case "tool_result":
				content := BlockText(b.Content)
				if b.IsError {
					content = "ERROR: " + content
				}
				out = append(out, oaiMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: &content})
			}
		}
		if text.Len() > 0 || len(calls) > 0 {
			msg := oaiMessage{Role: m.Role, ToolCalls: calls}
			if text.Len() > 0 {
				s := text.String()
				msg.Content = &s
			}
			out = append(out, msg)
		}
	}
	return out, nil
}

// Complete implements Provider.
func (o *OpenAI) Complete(ctx context.Context, req *Request) (*Response, error) {
	msgs, err := toOpenAIMessages(req.System, req.Messages)
	if err != nil {
		return nil, &Error{Msg: err.Error()}
	}
	body := map[string]any{"model": req.Model, "messages": msgs}
	// OpenAI's current models require max_completion_tokens; most
	// OpenAI-compatible servers (Ollama, vLLM, LiteLLM) still expect max_tokens.
	if strings.Contains(o.BaseURL, "api.openai.com") {
		body["max_completion_tokens"] = req.MaxTokens
	} else {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.InputSchema,
			}})
		}
		body["tools"] = tools
	}
	headers := map[string]string{}
	if o.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.APIKey
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      oaiMessage `json:"message"`
			FinishReason string     `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := postJSON(ctx, o.Client, o.BaseURL+"/chat/completions", headers, body, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, &Error{Retryable: true, Msg: "response has no choices"}
	}
	ch := out.Choices[0]
	var content []Block
	if ch.Message.Content != nil && *ch.Message.Content != "" {
		content = append(content, Block{Type: "text", Text: *ch.Message.Content})
	}
	for _, c := range ch.Message.ToolCalls {
		input := json.RawMessage(c.Function.Arguments)
		if !json.Valid(input) {
			b, _ := json.Marshal(map[string]string{"_raw": c.Function.Arguments})
			input = b
		}
		content = append(content, Block{Type: "tool_use", ID: c.ID, Name: c.Function.Name, Input: input})
	}
	stop := map[string]string{"stop": "end_turn", "tool_calls": "tool_use", "length": "max_tokens"}[ch.FinishReason]
	if stop == "" {
		stop = ch.FinishReason
	}
	return &Response{
		Model:      out.Model,
		Content:    content,
		StopReason: stop,
		Usage:      Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens},
	}, nil
}
