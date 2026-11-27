package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anupam2105/agentd/internal/domain"
	"github.com/anupam2105/agentd/internal/llm"
	"github.com/anupam2105/agentd/internal/policy"
	"github.com/anupam2105/agentd/internal/sandbox"
	"github.com/anupam2105/agentd/internal/store"
)

type fakeRunner struct {
	calls atomic.Int32
	fn    func(n int32, spec *sandbox.Spec) (*sandbox.Result, error)
}

func (f *fakeRunner) Run(_ context.Context, spec *sandbox.Spec) (*sandbox.Result, error) {
	n := f.calls.Add(1)
	if f.fn != nil {
		return f.fn(n, spec)
	}
	return &sandbox.Result{ExitCode: 0, Stdout: spec.Command[len(spec.Command)-1]}, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	e      *Engine
	runner *fakeRunner
	clock  *clock
}

func newFixture(t *testing.T, pol policy.Config) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runner := &fakeRunner{}
	reg, err := sandbox.NewRegistry(map[string]sandbox.ToolConfig{
		"shell": {
			Image: "alpine:3", Command: []string{"sh", "-c", "{{.command}}"},
			InputSchema: map[string]any{"type": "object", "required": []any{"command"},
				"properties": map[string]any{"command": map[string]any{"type": "string"}}},
		},
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	pe, err := policy.New(pol)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	e := New(&Options{
		Store: st, LLM: llm.NewGateway(llm.Pricing{"priced-*": {Input: 1, Output: 2}}), Tools: reg, Policy: pe,
		Now: c.Now, Lease: time.Minute,
	})
	return &fixture{e: e, runner: runner, clock: c}
}

var allowShell = policy.Config{Rules: []policy.Rule{
	{Name: "dangerous", Kind: "tool", Tool: "shell", Input: map[string]string{"command": `\brm\b`}, Action: policy.RequireApproval},
	{Name: "shell", Kind: "tool", Tool: "shell", Action: policy.Allow},
}}

func (f *fixture) newRun(t *testing.T, b domain.Budget) *domain.Run {
	t.Helper()
	r, created, err := f.e.CreateRun(context.Background(), &CreateRunRequest{Agent: "test", Budget: b})
	if err != nil || !created {
		t.Fatalf("create run: %v %v", created, err)
	}
	return r
}

func (f *fixture) step(t *testing.T, runID, key string, kind domain.StepKind, req string) *StepOutcome {
	t.Helper()
	out, err := f.e.ExecuteStep(context.Background(), runID, &StepRequest{Key: key, Kind: kind, Request: json.RawMessage(req)})
	if err != nil {
		t.Fatalf("step %s: %v", key, err)
	}
	return out
}

func (f *fixture) stepErr(t *testing.T, runID, key string, kind domain.StepKind, req string) *Error {
	t.Helper()
	_, err := f.e.ExecuteStep(context.Background(), runID, &StepRequest{Key: key, Kind: kind, Request: json.RawMessage(req)})
	if err == nil {
		t.Fatalf("step %s: expected error", key)
	}
	return AsError(err)
}

const llmReq = `{"model":"mock-1","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`

func TestCreateRunIdempotent(t *testing.T) {
	f := newFixture(t, policy.Config{})
	ctx := context.Background()
	r1, created, err := f.e.CreateRun(ctx, &CreateRunRequest{ID: "r1", Agent: "a"})
	if err != nil || !created || r1.TraceID == "" {
		t.Fatalf("first create: %v %v %+v", created, err, r1)
	}
	r2, created, err := f.e.CreateRun(ctx, &CreateRunRequest{ID: "r1", Agent: "a"})
	if err != nil || created || r2.ID != "r1" {
		t.Fatalf("second create: %v %v", created, err)
	}
	if _, _, err := f.e.CreateRun(ctx, &CreateRunRequest{ID: "r1", Agent: "b"}); AsError(err).Status != http.StatusConflict {
		t.Fatalf("want conflict, got %v", err)
	}
	if _, _, err := f.e.CreateRun(ctx, &CreateRunRequest{Agent: "bad agent!"}); AsError(err).Status != http.StatusBadRequest {
		t.Fatalf("want 400, got %v", err)
	}
}

func TestLLMStepReplayAndNondeterminism(t *testing.T) {
	f := newFixture(t, policy.Config{})
	run := f.newRun(t, domain.Budget{})
	first := f.step(t, run.ID, "1:llm", domain.KindLLM, llmReq)
	if first.Step.Status != domain.StepCompleted || first.Replayed || first.Step.Usage.InputTokens == 0 {
		t.Fatalf("unexpected first step: %+v", first.Step)
	}
	// Key order and whitespace must not matter.
	again := f.step(t, run.ID, "1:llm", domain.KindLLM, `{"max_tokens":100, "messages":[{"content":"hi","role":"user"}],"model":"mock-1"}`)
	if !again.Replayed || string(again.Step.Result) != string(first.Step.Result) {
		t.Fatalf("expected replay of identical result")
	}
	got, _ := f.e.Store().GetRun(context.Background(), run.ID)
	if got.Usage.Steps != 1 || got.Usage.InputTokens != first.Step.Usage.InputTokens || got.ReservedTokens != 0 {
		t.Fatalf("usage not accounted once: %+v reserved=%d", got.Usage, got.ReservedTokens)
	}
	e := f.stepErr(t, run.ID, "1:llm", domain.KindLLM, `{"model":"mock-1","messages":[{"role":"user","content":"different"}]}`)
	if e.Code != ErrCodeNondeterminism {
		t.Fatalf("want nondeterminism, got %+v", e)
	}
}

func TestRecordStep(t *testing.T) {
	f := newFixture(t, policy.Config{})
	run := f.newRun(t, domain.Budget{MaxSteps: 1})
	out := f.step(t, run.ID, "1:record", domain.KindRecord, `{"value":{"now":"2026-01-01"}}`)
	if out.Step.Status != domain.StepCompleted || string(out.Step.Result) != `{"now":"2026-01-01"}` {
		t.Fatalf("record: %+v %s", out.Step, out.Step.Result)
	}
	// Records do not consume max_steps.
	if s := f.step(t, run.ID, "2:llm", domain.KindLLM, llmReq).Step; s.Status != domain.StepCompleted {
		t.Fatalf("llm after record should fit budget: %+v", s.Error)
	}
}

func TestToolPolicyAndExecution(t *testing.T) {
	f := newFixture(t, allowShell)
	run := f.newRun(t, domain.Budget{})

	if s := f.step(t, run.ID, "1:tool", domain.KindTool, `{"tool":"nope","input":{}}`).Step; s.Status != domain.StepDenied || s.Error.Code != domain.CodeUnknownTool {
		t.Fatalf("unknown tool: %+v", s)
	}
	if s := f.step(t, run.ID, "2:tool", domain.KindTool, `{"tool":"shell","input":{}}`).Step; s.Status != domain.StepFailed || s.Error.Code != domain.CodeInvalidInput {
		t.Fatalf("invalid input: %+v", s)
	}
	s := f.step(t, run.ID, "3:tool", domain.KindTool, `{"tool":"shell","input":{"command":"uptime"}}`).Step
	if s.Status != domain.StepCompleted || s.PolicyRule != "shell" {
		t.Fatalf("shell: %+v", s)
	}
	var res sandbox.Result
	if err := json.Unmarshal(s.Result, &res); err != nil || res.Stdout != "uptime" {
		t.Fatalf("result: %s", s.Result)
	}
	f.step(t, run.ID, "3:tool", domain.KindTool, `{"tool":"shell","input":{"command":"uptime"}}`)
	if n := f.runner.calls.Load(); n != 1 {
		t.Fatalf("tool executed %d times, want 1", n)
	}
}

func TestToolsDeniedByDefault(t *testing.T) {
	f := newFixture(t, policy.Config{})
	run := f.newRun(t, domain.Budget{})
	s := f.step(t, run.ID, "1:tool", domain.KindTool, `{"tool":"shell","input":{"command":"id"}}`).Step
	if s.Status != domain.StepDenied || s.Error.Code != domain.CodePolicyDenied || s.PolicyRule != "default:tool" {
		t.Fatalf("want default deny: %+v", s)
	}
}

func TestApprovalFlow(t *testing.T) {
	f := newFixture(t, allowShell)
	ctx := context.Background()
	run := f.newRun(t, domain.Budget{})
	req := `{"tool":"shell","input":{"command":"rm -rf /tmp/x"}}`

	s := f.step(t, run.ID, "1:tool", domain.KindTool, req).Step
	if s.Status != domain.StepAwaitingApproval || s.PolicyRule != "dangerous" {
		t.Fatalf("want awaiting approval: %+v", s)
	}
	if s := f.step(t, run.ID, "1:tool", domain.KindTool, req).Step; s.Status != domain.StepAwaitingApproval {
		t.Fatalf("re-post should still wait: %+v", s)
	}
	if f.runner.calls.Load() != 0 {
		t.Fatal("tool ran before approval")
	}
	if _, err := f.e.Decide(ctx, run.ID, "1:tool", &DecisionRequest{Approve: true, By: "alice"}); err != nil {
		t.Fatal(err)
	}
	// Repeating the same decision is idempotent.
	if _, err := f.e.Decide(ctx, run.ID, "1:tool", &DecisionRequest{Approve: true, By: "alice"}); err != nil {
		t.Fatal(err)
	}
	s = f.step(t, run.ID, "1:tool", domain.KindTool, req).Step
	if s.Status != domain.StepCompleted || s.Decision == nil || s.Decision.By != "alice" || f.runner.calls.Load() != 1 {
		t.Fatalf("approved step should execute: %+v", s)
	}

	// Deny path.
	f.step(t, run.ID, "2:tool", domain.KindTool, req)
	if _, err := f.e.Decide(ctx, run.ID, "2:tool", &DecisionRequest{Approve: false, By: "bob", Reason: "no"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Decide(ctx, run.ID, "2:tool", &DecisionRequest{Approve: true, By: "bob"}); AsError(err).Status != http.StatusConflict {
		t.Fatalf("flip-flopping decision should conflict: %v", err)
	}
	s = f.step(t, run.ID, "2:tool", domain.KindTool, req).Step
	if s.Status != domain.StepDenied || s.Error.Code != domain.CodeApprovalDenied {
		t.Fatalf("denied: %+v", s)
	}
}

func TestCancelDeniesPendingApprovals(t *testing.T) {
	f := newFixture(t, allowShell)
	ctx := context.Background()
	run := f.newRun(t, domain.Budget{})
	f.step(t, run.ID, "1:tool", domain.KindTool, `{"tool":"shell","input":{"command":"rm x"}}`)
	if _, err := f.e.CancelRun(ctx, run.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	st, _ := f.e.Store().GetStep(ctx, run.ID, "1:tool")
	if st.Status != domain.StepDenied || st.Error.Code != domain.CodeRunCancelled {
		t.Fatalf("pending step should be denied: %+v", st)
	}
	if e := f.stepErr(t, run.ID, "2:llm", domain.KindLLM, llmReq); e.Code != ErrCodeRunNotRunning {
		t.Fatalf("new step on cancelled run: %+v", e)
	}
	if _, err := f.e.FinishRun(ctx, run.ID, &FinishRunRequest{Status: domain.RunCompleted}); AsError(err).Code != ErrCodeRunNotRunning {
		t.Fatalf("finish after cancel: %v", err)
	}
	// Replay of journaled steps still works after the run ended.
	if out := f.step(t, run.ID, "1:tool", domain.KindTool, `{"tool":"shell","input":{"command":"rm x"}}`); !out.Replayed {
		t.Fatal("expected replay")
	}
}

func TestFinishRun(t *testing.T) {
	f := newFixture(t, policy.Config{})
	ctx := context.Background()
	run := f.newRun(t, domain.Budget{})
	req := &FinishRunRequest{Status: domain.RunCompleted, Output: json.RawMessage(`{ "answer": 42 }`)}
	got, err := f.e.FinishRun(ctx, run.ID, req)
	if err != nil || got.Status != domain.RunCompleted || string(got.Output) != `{"answer":42}` || got.FinishedAt == nil {
		t.Fatalf("finish: %v %+v", err, got)
	}
	if _, err := f.e.FinishRun(ctx, run.ID, req); err != nil {
		t.Fatalf("repeat finish should be idempotent: %v", err)
	}
	if _, err := f.e.FinishRun(ctx, "missing", req); AsError(err).Status != http.StatusNotFound {
		t.Fatalf("want 404: %v", err)
	}
}

func TestBudgets(t *testing.T) {
	f := newFixture(t, policy.Config{})
	ctx := context.Background()

	run := f.newRun(t, domain.Budget{MaxSteps: 1})
	f.step(t, run.ID, "1:llm", domain.KindLLM, llmReq)
	s := f.step(t, run.ID, "2:llm", domain.KindLLM, llmReq).Step
	if s.Status != domain.StepDenied || s.Error.Code != domain.CodeBudgetSteps {
		t.Fatalf("steps budget: %+v", s)
	}

	run = f.newRun(t, domain.Budget{MaxTokens: 50})
	s = f.step(t, run.ID, "1:llm", domain.KindLLM, llmReq).Step
	if s.Status != domain.StepDenied || s.Error.Code != domain.CodeBudgetTokens {
		t.Fatalf("tokens budget: %+v", s)
	}
	got, _ := f.e.Store().GetRun(ctx, run.ID)
	if got.Usage.Steps != 0 {
		t.Fatalf("denied step must not consume steps: %+v", got.Usage)
	}

	run = f.newRun(t, domain.Budget{MaxCostUSD: 0.0001})
	priced := `{"model":"priced-x","provider":"mock","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`
	s = f.step(t, run.ID, "1:llm", domain.KindLLM, priced).Step
	if s.Status != domain.StepDenied || s.Error.Code != domain.CodeBudgetCost {
		t.Fatalf("cost budget: %+v", s)
	}
	unpriced := `{"model":"unpriced","provider":"mock","messages":[{"role":"user","content":"hi"}]}`
	if e := f.stepErr(t, run.ID, "2:llm", domain.KindLLM, unpriced); e.Status != http.StatusBadRequest {
		t.Fatalf("unknown pricing with cost budget must fail closed: %+v", e)
	}

	run = f.newRun(t, domain.Budget{MaxCostUSD: 1})
	s = f.step(t, run.ID, "1:llm", domain.KindLLM, priced).Step
	got, _ = f.e.Store().GetRun(ctx, run.ID)
	want := float64(s.Usage.InputTokens*1+s.Usage.OutputTokens*2) / 1e6
	if s.Status != domain.StepCompleted || got.Usage.CostUSD != want || got.ReservedCost != 0 {
		t.Fatalf("cost accounting: %+v cost=%v want=%v reserved=%v", s.Status, got.Usage.CostUSD, want, got.ReservedCost)
	}
}

func TestRetryableToolFailure(t *testing.T) {
	f := newFixture(t, allowShell)
	f.runner.fn = func(n int32, _ *sandbox.Spec) (*sandbox.Result, error) {
		if n == 1 {
			return nil, errors.New("docker daemon unavailable")
		}
		return &sandbox.Result{ExitCode: 3, Stderr: "boom"}, nil
	}
	run := f.newRun(t, domain.Budget{MaxSteps: 1})
	req := `{"tool":"shell","input":{"command":"false"}}`
	s := f.step(t, run.ID, "1:tool", domain.KindTool, req).Step
	if s.Status != domain.StepFailed || !s.Error.Retryable || s.Error.Code != domain.CodeSandboxError {
		t.Fatalf("want retryable failure: %+v", s)
	}
	s = f.step(t, run.ID, "1:tool", domain.KindTool, req).Step
	if s.Status != domain.StepCompleted || s.Attempts != 2 {
		t.Fatalf("retry should complete (non-zero exit is a result): %+v", s)
	}
	got, _ := f.e.Store().GetRun(context.Background(), run.ID)
	if got.Usage.Steps != 1 {
		t.Fatalf("retry must not double-count steps: %d", got.Usage.Steps)
	}
}

func TestLeaseReclaim(t *testing.T) {
	f := newFixture(t, allowShell)
	release := make(chan struct{})
	started := make(chan struct{})
	f.runner.fn = func(n int32, spec *sandbox.Spec) (*sandbox.Result, error) {
		if n == 1 {
			close(started)
			<-release
			return &sandbox.Result{Stdout: "stale"}, nil
		}
		return &sandbox.Result{Stdout: "fresh:" + spec.Name}, nil
	}
	run := f.newRun(t, domain.Budget{})
	req := `{"tool":"shell","input":{"command":"sleep"}}`

	done := make(chan *StepOutcome)
	go func() {
		out, _ := f.e.ExecuteStep(context.Background(), run.ID, &StepRequest{Key: "1:tool", Kind: domain.KindTool, Request: json.RawMessage(req)})
		done <- out
	}()
	<-started
	if e := f.stepErr(t, run.ID, "1:tool", domain.KindTool, req); e.Code != ErrCodeInProgress || !e.Retryable {
		t.Fatalf("want in_progress: %+v", e)
	}
	f.clock.Add(2 * time.Minute)
	s := f.step(t, run.ID, "1:tool", domain.KindTool, req).Step
	if s.Status != domain.StepCompleted || s.Attempts != 2 {
		t.Fatalf("reclaimed step: %+v", s)
	}
	close(release)
	stale := <-done
	if string(stale.Step.Result) != string(s.Result) {
		t.Fatalf("stale executor must not overwrite: %s vs %s", stale.Step.Result, s.Result)
	}
}

func TestValidation(t *testing.T) {
	f := newFixture(t, policy.Config{})
	run := f.newRun(t, domain.Budget{})
	cases := map[string]StepRequest{
		"bad key":      {Key: "bad key", Kind: domain.KindLLM, Request: json.RawMessage(llmReq)},
		"bad kind":     {Key: "1", Kind: "shell", Request: json.RawMessage(`{}`)},
		"bad json":     {Key: "1", Kind: domain.KindLLM, Request: json.RawMessage(`{`)},
		"no messages":  {Key: "1", Kind: domain.KindLLM, Request: json.RawMessage(`{"model":"mock"}`)},
		"no provider":  {Key: "1", Kind: domain.KindLLM, Request: json.RawMessage(`{"model":"claude-x","messages":[{"role":"user","content":"x"}]}`)},
		"record value": {Key: "1", Kind: domain.KindRecord, Request: json.RawMessage(`{}`)},
		"tool name":    {Key: "1", Kind: domain.KindTool, Request: json.RawMessage(`{"input":{}}`)},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.e.ExecuteStep(context.Background(), run.ID, &req)
			if AsError(err).Status != http.StatusBadRequest {
				t.Fatalf("want 400, got %v", err)
			}
		})
	}
	if _, err := f.e.ExecuteStep(context.Background(), "missing", &StepRequest{Key: "1", Kind: domain.KindLLM, Request: json.RawMessage(llmReq)}); AsError(err).Status != http.StatusNotFound {
		t.Fatalf("want 404: %v", err)
	}
}
