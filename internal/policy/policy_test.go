package policy

import "testing"

func TestEvaluate(t *testing.T) {
	e, err := New(Config{
		Defaults: Defaults{LLM: Allow},
		Rules: []Rule{
			{Name: "no-prod", Agent: "prod-*", Kind: "tool", Action: Deny, Reason: "read-only agent"},
			{Name: "destructive", Kind: "tool", Tool: "shell", Input: map[string]string{"command": `\b(rm|dd|mkfs)\b`}, Action: RequireApproval},
			{Name: "shell", Kind: "tool", Tool: "shell", Action: Allow},
			{Name: "pricey", Kind: "llm", MinCostUSD: 0.5, Action: RequireApproval},
			{Name: "opus", Kind: "llm", Model: "claude-opus-*", Action: Deny},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		in   Input
		want Action
		rule string
	}{
		{"default tool deny", Input{Kind: "tool", Tool: "http_get"}, Deny, "default:tool"},
		{"default llm allow", Input{Kind: "llm", Model: "gpt-x"}, Allow, "default:llm"},
		{"default record allow", Input{Kind: "record"}, Allow, "default:record"},
		{"agent glob first", Input{Kind: "tool", Agent: "prod-bot", Tool: "shell", ToolInput: map[string]any{"command": "ls"}}, Deny, "no-prod"},
		{"input regex", Input{Kind: "tool", Tool: "shell", ToolInput: map[string]any{"command": "rm -rf /"}}, RequireApproval, "destructive"},
		{"regex no match", Input{Kind: "tool", Tool: "shell", ToolInput: map[string]any{"command": "format"}}, Allow, "shell"},
		{"missing input field", Input{Kind: "tool", Tool: "shell"}, Allow, "shell"},
		{"non-string input", Input{Kind: "tool", Tool: "shell", ToolInput: map[string]any{"command": []any{"rm"}}}, RequireApproval, "destructive"},
		{"cost threshold", Input{Kind: "llm", Model: "gpt", EstCostUSD: 0.6}, RequireApproval, "pricey"},
		{"model glob", Input{Kind: "llm", Model: "claude-opus-5"}, Deny, "opus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := e.Evaluate(&c.in)
			if d.Action != c.want || d.Rule != c.rule {
				t.Fatalf("got %s/%s, want %s/%s", d.Action, d.Rule, c.want, c.rule)
			}
		})
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	bad := []Config{
		{Defaults: Defaults{Tool: "maybe"}},
		{Rules: []Rule{{Action: "yes"}}},
		{Rules: []Rule{{Kind: "shell", Action: Allow}}},
		{Rules: []Rule{{Tool: "[", Action: Allow}}},
		{Rules: []Rule{{Input: map[string]string{"x": "("}, Action: Allow}}},
	}
	for i, c := range bad {
		if _, err := New(c); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}
