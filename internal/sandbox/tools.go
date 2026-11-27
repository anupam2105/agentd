package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Spec is a fully resolved container execution.
type Spec struct {
	Name           string
	Image          string
	Command        []string
	Env            []string
	User           string
	Network        bool
	MemoryBytes    int64
	CPUs           float64
	PidsLimit      int64
	Timeout        time.Duration
	MaxOutputBytes int
	Labels         map[string]string
}

// Result is the outcome of a tool execution, journaled as the step result.
type Result struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// Runner executes a Spec. *Docker implements it; tests use fakes.
type Runner interface {
	Run(ctx context.Context, spec *Spec) (*Result, error)
}

// ToolConfig is a tool as declared in the config file.
type ToolConfig struct {
	Description    string            `yaml:"description" json:"description"`
	Image          string            `yaml:"image" json:"-"`
	Command        []string          `yaml:"command" json:"-"`
	InputSchema    map[string]any    `yaml:"input_schema" json:"input_schema"`
	Timeout        string            `yaml:"timeout" json:"-"`
	Memory         string            `yaml:"memory" json:"-"`
	CPUs           float64           `yaml:"cpus" json:"-"`
	Pids           int64             `yaml:"pids" json:"-"`
	Network        bool              `yaml:"network" json:"network"`
	User           string            `yaml:"user" json:"-"`
	Env            map[string]string `yaml:"env" json:"-"`
	MaxOutputBytes int               `yaml:"max_output_bytes" json:"-"`
}

// Tool is a validated, ready-to-run tool.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Network     bool            `json:"network"`

	cfg      ToolConfig
	argv     []*template.Template
	timeout  time.Duration
	memory   int64
	required []string
	props    map[string]string // property -> JSON type
}

// MaxTimeout bounds any tool's timeout.
const MaxTimeout = 10 * time.Minute

// NewTool validates a tool config.
func NewTool(name string, c *ToolConfig) (*Tool, error) {
	if c.Image == "" {
		return nil, errors.New("image is required")
	}
	if len(c.Command) == 0 {
		return nil, errors.New("command is required")
	}
	t := &Tool{Name: name, Description: c.Description, Network: c.Network, cfg: *c, timeout: 30 * time.Second, memory: 256 << 20}
	if t.cfg.CPUs <= 0 {
		t.cfg.CPUs = 0.5
	}
	if t.cfg.Pids <= 0 {
		t.cfg.Pids = 128
	}
	if t.cfg.User == "" {
		t.cfg.User = "65534:65534"
	}
	if t.cfg.MaxOutputBytes <= 0 {
		t.cfg.MaxOutputBytes = 64 << 10
	}
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil || d <= 0 || d > MaxTimeout {
			return nil, fmt.Errorf("timeout %q must be a duration in (0, %s]", c.Timeout, MaxTimeout)
		}
		t.timeout = d
	}
	if c.Memory != "" {
		m, err := ParseBytes(c.Memory)
		if err != nil {
			return nil, err
		}
		t.memory = m
	}
	for i, arg := range c.Command {
		tpl, err := template.New(fmt.Sprint(i)).Option("missingkey=error").Parse(arg)
		if err != nil {
			return nil, fmt.Errorf("command[%d]: %w", i, err)
		}
		t.argv = append(t.argv, tpl)
	}
	schema := c.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("input_schema: %w", err)
	}
	t.InputSchema = raw
	t.props = map[string]string{}
	if props, ok := schema["properties"].(map[string]any); ok {
		for k, v := range props {
			if m, ok := v.(map[string]any); ok {
				typ, _ := m["type"].(string)
				t.props[k] = typ
			}
		}
	}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				t.required = append(t.required, s)
			}
		}
	}
	return t, nil
}

// ValidateInput checks required properties and top-level property types.
// It is deliberately a small subset of JSON Schema.
func (t *Tool) ValidateInput(input map[string]any) error {
	for _, r := range t.required {
		if _, ok := input[r]; !ok {
			return fmt.Errorf("missing required property %q", r)
		}
	}
	for k, v := range input {
		want, ok := t.props[k]
		if !ok || want == "" {
			continue
		}
		if !jsonTypeIs(v, want) {
			return fmt.Errorf("property %q must be %s", k, want)
		}
	}
	return nil
}

func jsonTypeIs(v any, want string) bool {
	switch want {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(json.Number)
		if !ok {
			_, ok = v.(float64)
		}
		return ok
	case "integer":
		switch n := v.(type) {
		case json.Number:
			_, err := n.Int64()
			return err == nil
		case float64:
			return n == float64(int64(n))
		}
		return false
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "null":
		return v == nil
	}
	return true
}

// Spec renders the container spec for one invocation.
func (t *Tool) Spec(runID, stepKey string, attempt int, input map[string]any, rawInput []byte) (*Spec, error) {
	argv := make([]string, 0, len(t.argv))
	for i, tpl := range t.argv {
		var sb strings.Builder
		if err := tpl.Execute(&sb, input); err != nil {
			return nil, fmt.Errorf("render command[%d]: %w", i, err)
		}
		argv = append(argv, sb.String())
	}
	env := []string{"AGENTD_TOOL_INPUT=" + string(rawInput), "AGENTD_RUN_ID=" + runID, "AGENTD_STEP_KEY=" + stepKey}
	keys := make([]string, 0, len(t.cfg.Env))
	for k := range t.cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+t.cfg.Env[k])
	}
	sum := sha256.Sum256([]byte(runID + "\x00" + stepKey))
	return &Spec{
		Name:           fmt.Sprintf("agentd-%s-%d", hex.EncodeToString(sum[:8]), attempt),
		Image:          t.cfg.Image,
		Command:        argv,
		Env:            env,
		User:           t.cfg.User,
		Network:        t.cfg.Network,
		MemoryBytes:    t.memory,
		CPUs:           t.cfg.CPUs,
		PidsLimit:      t.cfg.Pids,
		Timeout:        t.timeout,
		MaxOutputBytes: t.cfg.MaxOutputBytes,
		Labels:         map[string]string{"io.agentd.managed": "true", "io.agentd.run": runID, "io.agentd.step": stepKey, "io.agentd.tool": t.Name},
	}, nil
}

// Timeout is the tool's execution timeout.
func (t *Tool) Timeout() time.Duration { return t.timeout }

// Registry holds the configured tools.
type Registry struct {
	tools  map[string]*Tool
	Runner Runner
}

// NewRegistry validates all tool configs.
func NewRegistry(cfgs map[string]ToolConfig, runner Runner) (*Registry, error) {
	r := &Registry{tools: map[string]*Tool{}, Runner: runner}
	for name := range cfgs {
		c := cfgs[name]
		t, err := NewTool(name, &c)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", name, err)
		}
		r.tools[name] = t
	}
	return r, nil
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (*Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// List returns all tools sorted by name.
func (r *Registry) List() []*Tool {
	out := make([]*Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ParseBytes parses sizes like "256m", "1g", "512k" or plain bytes.
func ParseBytes(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n*mult < 6<<20 {
		return 0, fmt.Errorf("memory must be at least 6m")
	}
	return n * mult, nil
}
