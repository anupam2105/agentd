package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func capture(t *testing.T, status int, resp string, got *map[string]any, hdr *http.Header) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			_ = json.NewDecoder(r.Body).Decode(got)
		}
		if hdr != nil {
			*hdr = r.Header.Clone()
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func toolConversation() *Request {
	return &Request{
		Model: "m", System: "be brief", MaxTokens: 50,
		Tools: []Tool{{Name: "shell", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []Message{
			{Role: "user", Content: json.RawMessage(`"check disk"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"ok"},{"type":"tool_use","id":"c1","name":"shell","input":{"command":"df"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"c1","content":"90% used","is_error":true}]`)},
		},
	}
}

func TestOpenAITranslation(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	ts := capture(t, 200, `{"model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hi","tool_calls":[{"id":"c2","type":"function","function":{"name":"shell","arguments":"{\"command\":\"ls\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`, &got, &hdr)
	p := NewOpenAI("k", ts.URL, time.Second)
	resp, err := p.Complete(context.Background(), toolConversation())
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Get("Authorization") != "Bearer k" || got["max_tokens"] != float64(50) {
		t.Fatalf("request: %v %v", hdr, got)
	}
	msgs := got["messages"].([]any) //nolint:errcheck // test
	if len(msgs) != 4 {
		t.Fatalf("want system+user+assistant+tool messages, got %d: %v", len(msgs), msgs)
	}
	toolMsg := msgs[3].(map[string]any) //nolint:errcheck // test
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "c1" || toolMsg["content"] != "ERROR: 90% used" {
		t.Fatalf("tool message: %v", toolMsg)
	}
	if resp.StopReason != "tool_use" || len(resp.Content) != 2 || resp.Content[1].Name != "shell" || resp.Usage.InputTokens != 11 {
		t.Fatalf("response: %+v", resp)
	}
}

func TestAnthropicRequest(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	ts := capture(t, 200, `{"model":"claude-x","content":[{"type":"text","text":"done"},{"type":"thinking","thinking":"..."}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`, &got, &hdr)
	p := NewAnthropic("k", ts.URL, time.Second)
	resp, err := p.Complete(context.Background(), toolConversation())
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Get("x-api-key") != "k" || hdr.Get("anthropic-version") == "" || got["system"] != "be brief" {
		t.Fatalf("request: %v %v", hdr, got)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "done" || resp.Usage.OutputTokens != 2 {
		t.Fatalf("response: %+v", resp)
	}
}

func TestErrorClassification(t *testing.T) {
	for status, retryable := range map[int]bool{429: true, 500: true, 529: true, 400: false, 401: false} {
		ts := capture(t, status, `{"error":{"message":"nope"}}`, nil, nil)
		_, err := NewAnthropic("k", ts.URL, time.Second).Complete(context.Background(), toolConversation())
		e, ok := err.(*Error) //nolint:errorlint // direct type in test
		if !ok || e.Retryable != retryable || e.Msg != "nope" || e.Status != status {
			t.Errorf("status %d: %#v", status, err)
		}
	}
	_, err := NewOpenAI("k", "http://127.0.0.1:1", time.Second).Complete(context.Background(), toolConversation())
	if !IsRetryable(err) {
		t.Errorf("connection errors must be retryable: %v", err)
	}
}

func TestGatewayRoutingAndMock(t *testing.T) {
	g := NewGateway(Pricing{"claude-sonnet-*": {Input: 3, Output: 15}, "claude-sonnet-4-5*": {Input: 1, Output: 1}})
	if _, err := g.Resolve(&Request{Model: "claude-sonnet-4"}); err == nil {
		t.Fatal("anthropic is not configured")
	}
	if pr, ok := g.Pricing.Lookup("claude-sonnet-4-5-2026"); !ok || pr.Input != 1 {
		t.Fatalf("longest glob should win: %+v", pr)
	}
	req := toolConversation()
	req.Model = "mock"
	req.Messages = req.Messages[:1]
	req.Tools[0].InputSchema = json.RawMessage(`{"type":"object","examples":[{"command":"uptime"}]}`)
	resp, err := g.Complete(context.Background(), req)
	if err != nil || resp.StopReason != "tool_use" || string(resp.Content[1].Input) != `{"command":"uptime"}` || resp.Provider != "mock" {
		t.Fatalf("mock tool call: %+v %v", resp, err)
	}
	req = toolConversation()
	req.Model = "mock"
	resp, _ = g.Complete(context.Background(), req)
	if resp.StopReason != "end_turn" {
		t.Fatalf("mock should summarise after a tool result: %+v", resp)
	}
}

func TestValidate(t *testing.T) {
	r := &Request{Model: "m", Messages: []Message{{Role: "user", Content: json.RawMessage(`"x"`)}}}
	if err := r.Validate(); err != nil || r.MaxTokens != DefaultMaxTokens {
		t.Fatalf("defaults: %v %d", err, r.MaxTokens)
	}
	bad := []*Request{
		{Messages: r.Messages},
		{Model: "m"},
		{Model: "m", Messages: []Message{{Role: "system", Content: json.RawMessage(`"x"`)}}},
		{Model: "m", Messages: []Message{{Role: "user", Content: json.RawMessage(`5`)}}},
		{Model: "m", Messages: r.Messages, MaxTokens: -1},
	}
	for i, b := range bad {
		if b.Validate() == nil {
			t.Errorf("case %d should fail", i)
		}
	}
}
