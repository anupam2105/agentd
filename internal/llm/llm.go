// Package llm is the provider-neutral LLM gateway. The canonical message
// format is Anthropic's content-block format; other providers translate to and
// from it.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Message is one conversation turn. Content is a JSON string or an array of Blocks.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Tool describes a function the model may call.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Request is an LLM step request.
type Request struct {
	Provider    string    `json:"provider,omitempty"`
	Model       string    `json:"model"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	MaxTokens   int64     `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

// Block is a content block (text, tool_use or tool_result).
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Usage is the token usage reported by a provider.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Response is the normalised completion.
type Response struct {
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	Content    []Block `json:"content"`
	StopReason string  `json:"stop_reason"` // end_turn | tool_use | max_tokens | stop_sequence
	Usage      Usage   `json:"usage"`
}

// Provider completes requests against one LLM API.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req *Request) (*Response, error)
}

// Error is a provider failure. Retryable errors (network, 429, 5xx) are not
// journaled as final so the step can be retried.
type Error struct {
	Status    int
	Retryable bool
	Msg       string
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("provider returned HTTP %d: %s", e.Status, e.Msg)
	}
	return e.Msg
}

// DefaultMaxTokens is used when a request omits max_tokens.
const DefaultMaxTokens = 1024

// Validate checks a request and fills defaults.
func (r *Request) Validate() error {
	if r.Model == "" {
		return errors.New("model is required")
	}
	if len(r.Messages) == 0 {
		return errors.New("messages must not be empty")
	}
	for i, m := range r.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return fmt.Errorf("messages[%d].role must be user or assistant", i)
		}
		if _, err := ParseContent(m.Content); err != nil {
			return fmt.Errorf("messages[%d].content: %w", i, err)
		}
	}
	for i, t := range r.Tools {
		if t.Name == "" {
			return fmt.Errorf("tools[%d].name is required", i)
		}
	}
	if r.MaxTokens == 0 {
		r.MaxTokens = DefaultMaxTokens
	}
	if r.MaxTokens < 0 || r.MaxTokens > 1_000_000 {
		return fmt.Errorf("max_tokens %d out of range", r.MaxTokens)
	}
	return nil
}

// ParseContent decodes message content given as a string or a block array.
func ParseContent(raw json.RawMessage) ([]Block, error) {
	if len(raw) == 0 {
		return nil, errors.New("content is required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []Block{{Type: "text", Text: s}}, nil
	}
	var blocks []Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("content must be a string or an array of blocks")
	}
	for i := range blocks {
		if blocks[i].Type == "" {
			return nil, fmt.Errorf("block %d has no type", i)
		}
	}
	return blocks, nil
}

// BlockText flattens a tool_result content value (string or blocks) to text.
func BlockText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	blocks, err := ParseContent(raw)
	if err != nil {
		return string(raw)
	}
	var sb strings.Builder
	for i := range blocks {
		if blocks[i].Type == "text" {
			sb.WriteString(blocks[i].Text)
		}
	}
	return sb.String()
}

// EstimateInputTokens approximates prompt size at ~4 bytes per token. It
// errs high, which is the safe direction for budget checks.
func EstimateInputTokens(r *Request) int64 {
	n := len(r.System)
	for _, m := range r.Messages {
		n += len(m.Content) + 8
	}
	for _, t := range r.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return int64(n/4) + 16
}

// Price is USD per million tokens.
type Price struct {
	Input  float64 `yaml:"input" json:"input"`
	Output float64 `yaml:"output" json:"output"`
}

// Pricing maps model names or glob patterns (e.g. "claude-sonnet-4-5*") to prices.
type Pricing map[string]Price

// Lookup returns the price for model: exact match first, then the longest matching glob.
func (p Pricing) Lookup(model string) (Price, bool) {
	if pr, ok := p[model]; ok {
		return pr, true
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		if ok, _ := path.Match(k, model); ok {
			return p[k], true
		}
	}
	return Price{}, false
}

// Cost returns the USD cost of the given token counts.
func (pr Price) Cost(inputTokens, outputTokens int64) float64 {
	return (float64(inputTokens)*pr.Input + float64(outputTokens)*pr.Output) / 1_000_000
}
