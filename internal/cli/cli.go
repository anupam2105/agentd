// Package cli implements the operator subcommands (runs, approvals, ...).
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"strings"
	"text/tabwriter"
	"time"
)

// Usage is printed for -h and unknown commands.
const Usage = `agentd — durable, sandboxed, policy-governed runtime for AI agents

Usage:
  agentd [serve]                      run the server (configured via AGENTD_* env vars)
  agentd runs [--status S] [--agent A] [--limit N]
  agentd run <run-id>                 show a run
  agentd steps <run-id>               show a run's step journal
  agentd approvals                    list steps awaiting a human decision
  agentd approve <run-id> <step-key> [--reason R]
  agentd deny <run-id> <step-key> [--reason R]
  agentd cancel <run-id> [--reason R]
  agentd tools                        list sandboxed tools
  agentd version

Client commands use AGENTD_URL (default http://localhost:7700) and AGENTD_API_TOKEN.
Add --json to any client command for raw JSON output.
`

// Client talks to an agentd server.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Out     io.Writer
}

// FromEnv builds a client from AGENTD_URL / AGENTD_API_TOKEN.
func FromEnv(out io.Writer) *Client {
	base := os.Getenv("AGENTD_URL")
	if base == "" {
		base = "http://localhost:7700"
	}
	return &Client{BaseURL: strings.TrimRight(base, "/"), Token: os.Getenv("AGENTD_API_TOKEN"),
		HTTP: &http.Client{Timeout: 30 * time.Second}, Out: out}
}

// IsCommand reports whether name is a client subcommand.
func IsCommand(name string) bool {
	switch name {
	case "runs", "run", "steps", "approvals", "approve", "deny", "cancel", "tools":
		return true
	}
	return false
}

// Run executes a client subcommand.
func (c *Client) Run(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "raw JSON output")
	status := fs.String("status", "", "filter by status")
	agent := fs.String("agent", "", "filter by agent")
	limit := fs.Int("limit", 50, "max rows")
	reason := fs.String("reason", "", "reason for the decision")
	by := fs.String("by", defaultUser(), "who is deciding")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, Usage)
	}
	need := func(n int) error {
		if len(pos) != n {
			return fmt.Errorf("%s expects %d argument(s)\n\n%s", cmd, n, Usage)
		}
		return nil
	}

	var (
		raw    json.RawMessage
		render func(json.RawMessage) error
	)
	switch cmd {
	case "runs":
		q := url.Values{}
		q.Set("limit", fmt.Sprint(*limit))
		if *status != "" {
			q.Set("status", *status)
		}
		if *agent != "" {
			q.Set("agent", *agent)
		}
		raw, err = c.do(ctx, http.MethodGet, "/v1/runs?"+q.Encode(), nil)
		render = c.renderRuns
	case "run":
		if err := need(1); err != nil {
			return err
		}
		raw, err = c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(pos[0]), nil)
		render = c.renderRun
	case "steps":
		if err := need(1); err != nil {
			return err
		}
		raw, err = c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(pos[0])+"/steps", nil)
		render = func(b json.RawMessage) error { return c.renderSteps(b, "steps") }
	case "approvals":
		raw, err = c.do(ctx, http.MethodGet, "/v1/approvals", nil)
		render = func(b json.RawMessage) error { return c.renderSteps(b, "approvals") }
	case "approve", "deny":
		if err := need(2); err != nil {
			return err
		}
		body := map[string]any{"approve": cmd == "approve", "by": *by, "reason": *reason}
		raw, err = c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(pos[0])+"/steps/"+url.PathEscape(pos[1])+"/decision", body)
		render = func(json.RawMessage) error {
			_, err := fmt.Fprintf(c.Out, "%s %s/%s\n", map[bool]string{true: "approved", false: "denied"}[cmd == "approve"], pos[0], pos[1])
			return err
		}
	case "cancel":
		if err := need(1); err != nil {
			return err
		}
		raw, err = c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(pos[0])+"/cancel", map[string]string{"reason": *reason})
		render = c.renderRun
	case "tools":
		raw, err = c.do(ctx, http.MethodGet, "/v1/tools", nil)
		render = c.renderTools
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, Usage)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err != nil {
			return err
		}
		_, err := fmt.Fprintln(c.Out, buf.String())
		return err
	}
	return render(raw)
}

// parseInterleaved allows flags after positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func defaultUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "cli"
}

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("agentd: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error.Message == "" {
			e.Error.Message = strings.TrimSpace(string(data))
		}
		return nil, &APIError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	return data, nil
}

// ---------------------------------------------------------------- rendering

type usage struct {
	Steps        int64   `json:"steps"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

type run struct {
	ID        string          `json:"id"`
	Agent     string          `json:"agent"`
	Status    string          `json:"status"`
	Error     string          `json:"error"`
	Output    json.RawMessage `json:"output"`
	Usage     usage           `json:"usage"`
	TraceID   string          `json:"trace_id"`
	CreatedAt time.Time       `json:"created_at"`
}

type step struct {
	RunID      string          `json:"run_id"`
	Key        string          `json:"key"`
	Kind       string          `json:"kind"`
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	Request    json.RawMessage `json:"request"`
	Attempts   int             `json:"attempts"`
	Usage      usage           `json:"usage"`
	PolicyRule string          `json:"policy_rule"`
	Error      *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	CreatedAt time.Time `json:"created_at"`
}

func (c *Client) table() *tabwriter.Writer { return tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0) }

func (c *Client) renderRuns(b json.RawMessage) error {
	var v struct{ Runs []run }
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	w := c.table()
	_, _ = fmt.Fprintln(w, "RUN\tAGENT\tSTATUS\tSTEPS\tTOKENS\tCOST\tAGE")
	for i := range v.Runs {
		r := &v.Runs[i]
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t$%.4f\t%s\n", r.ID, r.Agent, r.Status, r.Usage.Steps,
			r.Usage.InputTokens+r.Usage.OutputTokens, r.Usage.CostUSD, age(r.CreatedAt))
	}
	return w.Flush()
}

func (c *Client) renderRun(b json.RawMessage) error {
	var r run
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	w := c.table()
	_, _ = fmt.Fprintf(w, "Run:\t%s\nAgent:\t%s\nStatus:\t%s\nSteps:\t%d\nTokens:\t%d in / %d out\nCost:\t$%.4f\nTrace:\t%s\nCreated:\t%s\n",
		r.ID, r.Agent, r.Status, r.Usage.Steps, r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CostUSD, r.TraceID,
		r.CreatedAt.Format(time.RFC3339))
	if r.Error != "" {
		_, _ = fmt.Fprintf(w, "Error:\t%s\n", r.Error)
	}
	if len(r.Output) > 0 {
		_, _ = fmt.Fprintf(w, "Output:\t%s\n", truncate(string(r.Output), 400))
	}
	return w.Flush()
}

func (c *Client) renderSteps(b json.RawMessage, field string) error {
	var v map[string][]step
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	w := c.table()
	if field == "approvals" {
		_, _ = fmt.Fprintln(w, "RUN\tSTEP\tKIND\tNAME\tRULE\tWAITING\tREQUEST")
		for i := range v[field] {
			s := &v[field][i]
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.RunID, s.Key, s.Kind, s.Name, s.PolicyRule, age(s.CreatedAt), truncate(string(s.Request), 80))
		}
		return w.Flush()
	}
	_, _ = fmt.Fprintln(w, "STEP\tKIND\tNAME\tSTATUS\tATTEMPTS\tTOKENS\tRULE\tDETAIL")
	for i := range v[field] {
		s := &v[field][i]
		detail := ""
		if s.Error != nil {
			detail = s.Error.Code + ": " + truncate(s.Error.Message, 60)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n", s.Key, s.Kind, s.Name, s.Status, s.Attempts,
			s.Usage.InputTokens+s.Usage.OutputTokens, s.PolicyRule, detail)
	}
	return w.Flush()
}

func (c *Client) renderTools(b json.RawMessage) error {
	var v struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Network     bool   `json:"network"`
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	w := c.table()
	_, _ = fmt.Fprintln(w, "TOOL\tNETWORK\tDESCRIPTION")
	for _, t := range v.Tools {
		_, _ = fmt.Fprintf(w, "%s\t%v\t%s\n", t.Name, t.Network, truncate(t.Description, 80))
	}
	return w.Flush()
}

func age(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return d.String()
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// IsAPIError reports whether err is an API error with the given status.
func IsAPIError(err error, status int) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == status
}
