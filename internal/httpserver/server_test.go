package httpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anupam2105/agentd/internal/engine"
	"github.com/anupam2105/agentd/internal/httpserver"
	"github.com/anupam2105/agentd/internal/llm"
	"github.com/anupam2105/agentd/internal/observability"
	"github.com/anupam2105/agentd/internal/policy"
	"github.com/anupam2105/agentd/internal/sandbox"
	"github.com/anupam2105/agentd/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	return newTestServerWithToken(t, "")
}

func newTestServerWithToken(t *testing.T, token string) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pol, err := policy.New(policy.Config{Rules: []policy.Rule{{Name: "gate", Kind: "tool", Tool: "echo", Action: policy.RequireApproval}}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := sandbox.NewRegistry(map[string]sandbox.ToolConfig{"echo": {Image: "alpine", Command: []string{"echo", "{{.msg}}"}}}, echoRunner{})
	if err != nil {
		t.Fatal(err)
	}
	metrics := observability.NewMetrics()
	eng := engine.New(&engine.Options{Store: st, LLM: llm.NewGateway(nil), Tools: reg, Policy: pol, Metrics: metrics, Logger: logger})
	s := httpserver.New(httpserver.Options{Addr: ":0", Version: "test", Logger: logger, Metrics: metrics, Engine: eng, APIToken: token})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

type echoRunner struct{}

func (echoRunner) Run(_ context.Context, spec *sandbox.Spec) (*sandbox.Result, error) {
	return &sandbox.Result{Stdout: strings.Join(spec.Command[1:], " ") + "\n"}, nil
}

func doGet(t *testing.T, url string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestEndpoints(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	cases := []struct {
		path, want string
		status     int
	}{
		{"/healthz", `"status":"ok"`, http.StatusOK},
		{"/readyz", `"status":"ready"`, http.StatusOK},
		{"/v1/version", `"version":"test"`, http.StatusOK},
		{"/does-not-exist", "", http.StatusNotFound},
	}
	for _, c := range cases {
		status, body := doGet(t, ts.URL+c.path)
		if status != c.status || !strings.Contains(body, c.want) {
			t.Errorf("GET %s = %d %q, want %d containing %q", c.path, status, body, c.status, c.want)
		}
	}
}

func TestMetricsUseBoundedRouteLabels(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	doGet(t, ts.URL+"/healthz")
	doGet(t, ts.URL+"/random-scanner-path")

	_, body := doGet(t, ts.URL+"/metrics")
	if !strings.Contains(body, `agentd_http_requests_total{method="GET",route="/healthz",status="200"} 1`) {
		t.Errorf("missing /healthz series in:\n%s", body)
	}
	if !strings.Contains(body, `route="unknown"`) || strings.Contains(body, "random-scanner-path") {
		t.Errorf("unmatched paths must be labelled unknown")
	}
}

func call(t *testing.T, method, url, token, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAuth(t *testing.T) {
	t.Parallel()
	ts := newTestServerWithToken(t, "s3cret")
	if code, out := call(t, "GET", ts.URL+"/v1/runs", "", ""); code != http.StatusUnauthorized || out["error"] == nil {
		t.Fatalf("no token: %d %v", code, out)
	}
	if code, _ := call(t, "GET", ts.URL+"/v1/runs", "wrong", ""); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _ := call(t, "GET", ts.URL+"/v1/runs", "s3cret", ""); code != http.StatusOK {
		t.Fatalf("good token: %d", code)
	}
	if code, _ := call(t, "GET", ts.URL+"/healthz", "", ""); code != http.StatusOK {
		t.Fatalf("healthz must stay open: %d", code)
	}
}

func TestRunLifecycleOverHTTP(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	code, run := call(t, "POST", ts.URL+"/v1/runs", "", `{"id":"r1","agent":"demo","input":{"q":1},"budget":{"max_steps":5}}`)
	if code != http.StatusCreated || run["trace_id"] == "" {
		t.Fatalf("create: %d %v", code, run)
	}
	if code, _ := call(t, "POST", ts.URL+"/v1/runs", "", `{"id":"r1","agent":"demo"}`); code != http.StatusOK {
		t.Fatalf("idempotent create: %d", code)
	}

	llmStep := `{"key":"1:llm","kind":"llm","request":{"model":"mock","messages":[{"role":"user","content":"hello"}]}}`
	code, step := call(t, "POST", ts.URL+"/v1/runs/r1/steps", "", llmStep)
	if code != http.StatusOK || step["status"] != "completed" || step["replayed"] != false {
		t.Fatalf("llm step: %d %v", code, step)
	}
	if _, step = call(t, "POST", ts.URL+"/v1/runs/r1/steps", "", llmStep); step["replayed"] != true {
		t.Fatalf("replay: %v", step)
	}

	toolStep := `{"key":"2:tool:echo","kind":"tool","request":{"tool":"echo","input":{"msg":"hi"}}}`
	if code, step = call(t, "POST", ts.URL+"/v1/runs/r1/steps", "", toolStep); code != http.StatusAccepted || step["status"] != "awaiting_approval" {
		t.Fatalf("gated tool: %d %v", code, step)
	}
	_, approvals := call(t, "GET", ts.URL+"/v1/approvals", "", "")
	if n := len(list(approvals["approvals"])); n != 1 {
		t.Fatalf("approvals: %v", approvals)
	}
	if code, _ := call(t, "POST", ts.URL+"/v1/runs/r1/steps/2:tool:echo/decision", "", `{"approve":true,"by":"tester"}`); code != http.StatusOK {
		t.Fatalf("decision: %d", code)
	}
	code, step = call(t, "POST", ts.URL+"/v1/runs/r1/steps", "", toolStep)
	result, _ := step["result"].(map[string]any)
	if code != http.StatusOK || result["stdout"] != "hi\n" {
		t.Fatalf("approved tool: %d %v", code, step)
	}

	if code, out := call(t, "POST", ts.URL+"/v1/runs/r1/steps", "", `{"key":"1:llm","kind":"llm","request":{"model":"mock","messages":[{"role":"user","content":"changed"}]}}`); code != http.StatusConflict || errCode(out) != "nondeterminism" {
		t.Fatalf("nondeterminism: %d %v", code, out)
	}
	if code, _ := call(t, "POST", ts.URL+"/v1/runs/r1/steps", "", `{"key":"x","kind":"llm","bogus":1}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", code)
	}
	if code, _ := call(t, "POST", ts.URL+"/v1/runs/r1/steps", "", `{"key":"x","kind":"record","request":{"value":"`+strings.Repeat("a", httpserver.MaxBodyBytes)+`"}}`); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d", code)
	}

	_, steps := call(t, "GET", ts.URL+"/v1/runs/r1/steps", "", "")
	if n := len(list(steps["steps"])); n != 2 {
		t.Fatalf("steps: %v", steps)
	}
	if code, _ := call(t, "GET", ts.URL+"/v1/runs/r1/steps/nope", "", ""); code != http.StatusNotFound {
		t.Fatalf("missing step: %d", code)
	}
	code, run = call(t, "POST", ts.URL+"/v1/runs/r1/finish", "", `{"status":"completed","output":{"ok":true}}`)
	if code != http.StatusOK || run["status"] != "completed" {
		t.Fatalf("finish: %d %v", code, run)
	}
	_, runs := call(t, "GET", ts.URL+"/v1/runs?status=completed", "", "")
	if n := len(list(runs["runs"])); n != 1 {
		t.Fatalf("list: %v", runs)
	}
	_, tools := call(t, "GET", ts.URL+"/v1/tools", "", "")
	if n := len(list(tools["tools"])); n != 1 {
		t.Fatalf("tools: %v", tools)
	}
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func errCode(out map[string]any) any {
	e, _ := out["error"].(map[string]any)
	return e["code"]
}
