package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Mock is a deterministic offline provider for tests and demos (models "mock*").
//
// If tools are offered and the conversation has no tool_result yet, it calls
// the first tool with that tool's input_schema "examples"[0] (or {}).
// Otherwise it answers with text summarising the latest message.
type Mock struct{}

// Name implements Provider.
func (Mock) Name() string { return "mock" }

// Complete implements Provider.
func (Mock) Complete(_ context.Context, req *Request) (*Response, error) {
	var lastText, lastToolResult string
	sawToolResult := false
	for _, m := range req.Messages {
		blocks, _ := ParseContent(m.Content)
		for i := range blocks {
			b := &blocks[i]
			switch b.Type {
			case "text":
				if m.Role == "user" {
					lastText = b.Text
				}
			case "tool_result":
				sawToolResult = true
				lastToolResult = BlockText(b.Content)
			}
		}
	}
	in := EstimateInputTokens(req)

	if len(req.Tools) > 0 && !sawToolResult {
		t := req.Tools[0]
		input := json.RawMessage(`{}`)
		var schema struct {
			Examples []json.RawMessage `json:"examples"`
		}
		if json.Unmarshal(t.InputSchema, &schema) == nil && len(schema.Examples) > 0 {
			input = schema.Examples[0]
		}
		return &Response{
			Model:      req.Model,
			Content:    []Block{{Type: "text", Text: "Let me check with " + t.Name + "."}, {Type: "tool_use", ID: "toolu_mock_1", Name: t.Name, Input: input}},
			StopReason: "tool_use",
			Usage:      Usage{InputTokens: in, OutputTokens: 24},
		}, nil
	}

	var text string
	if sawToolResult {
		text = "Mock summary. Tool output:\n" + truncate(strings.TrimSpace(lastToolResult), 500)
	} else {
		text = fmt.Sprintf("Mock response to: %s", truncate(lastText, 200))
	}
	return &Response{
		Model:      req.Model,
		Content:    []Block{{Type: "text", Text: text}},
		StopReason: "end_turn",
		Usage:      Usage{InputTokens: in, OutputTokens: int64(len(text)/4 + 1)},
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
