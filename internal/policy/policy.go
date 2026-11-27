// Package policy decides whether a step may run: allow, deny, or pause for
// human approval. Rules are evaluated in order; the first match wins.
package policy

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
)

// Action is a policy outcome.
type Action string

// Actions.
const (
	Allow           Action = "allow"
	Deny            Action = "deny"
	RequireApproval Action = "require_approval"
)

func (a Action) valid() bool { return a == Allow || a == Deny || a == RequireApproval }

// Rule matches steps; every set field must match.
type Rule struct {
	Name       string            `yaml:"name" json:"name"`
	Kind       string            `yaml:"kind" json:"kind,omitempty"`   // llm | tool | record (empty = any)
	Tool       string            `yaml:"tool" json:"tool,omitempty"`   // glob
	Model      string            `yaml:"model" json:"model,omitempty"` // glob
	Agent      string            `yaml:"agent" json:"agent,omitempty"` // glob
	Input      map[string]string `yaml:"input" json:"input,omitempty"` // tool input field -> regex
	MinCostUSD float64           `yaml:"min_cost_usd" json:"min_cost_usd,omitempty"`
	Action     Action            `yaml:"action" json:"action"`
	Reason     string            `yaml:"reason" json:"reason,omitempty"`

	input map[string]*regexp.Regexp
}

// Defaults apply when no rule matches.
type Defaults struct {
	LLM    Action `yaml:"llm" json:"llm"`
	Tool   Action `yaml:"tool" json:"tool"`
	Record Action `yaml:"record" json:"record"`
}

// Config is the policy section of the config file.
type Config struct {
	Defaults Defaults `yaml:"defaults" json:"defaults"`
	Rules    []Rule   `yaml:"rules" json:"rules"`
}

// Input describes the step being evaluated.
type Input struct {
	Kind       string
	Agent      string
	Tool       string
	Model      string
	ToolInput  map[string]any
	EstCostUSD float64
}

// Decision is the evaluation result.
type Decision struct {
	Action Action
	Rule   string // "default:<kind>" when no rule matched
	Reason string
}

// Engine evaluates a compiled policy.
type Engine struct {
	cfg Config
}

// New validates and compiles a policy. Missing defaults are safe: LLM calls
// and records are allowed, tools are denied.
func New(cfg Config) (*Engine, error) {
	if cfg.Defaults.LLM == "" {
		cfg.Defaults.LLM = Allow
	}
	if cfg.Defaults.Tool == "" {
		cfg.Defaults.Tool = Deny
	}
	if cfg.Defaults.Record == "" {
		cfg.Defaults.Record = Allow
	}
	for _, a := range []Action{cfg.Defaults.LLM, cfg.Defaults.Tool, cfg.Defaults.Record} {
		if !a.valid() {
			return nil, fmt.Errorf("invalid default action %q", a)
		}
	}
	rules := make([]Rule, len(cfg.Rules))
	copy(rules, cfg.Rules)
	for i := range rules {
		r := &rules[i]
		if r.Name == "" {
			r.Name = fmt.Sprintf("rule-%d", i+1)
		}
		if !r.Action.valid() {
			return nil, fmt.Errorf("rule %q: invalid action %q", r.Name, r.Action)
		}
		switch r.Kind {
		case "", "llm", "tool", "record":
		default:
			return nil, fmt.Errorf("rule %q: invalid kind %q", r.Name, r.Kind)
		}
		for _, g := range []string{r.Tool, r.Model, r.Agent} {
			if _, err := path.Match(g, ""); err != nil {
				return nil, fmt.Errorf("rule %q: bad glob %q", r.Name, g)
			}
		}
		r.input = map[string]*regexp.Regexp{}
		for field, expr := range r.Input {
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, fmt.Errorf("rule %q: input.%s: %w", r.Name, field, err)
			}
			r.input[field] = re
		}
	}
	cfg.Rules = rules
	return &Engine{cfg: cfg}, nil
}

// Evaluate returns the decision for a step.
func (e *Engine) Evaluate(in *Input) Decision {
	for i := range e.cfg.Rules {
		r := &e.cfg.Rules[i]
		if r.matches(in) {
			return Decision{Action: r.Action, Rule: r.Name, Reason: r.Reason}
		}
	}
	d := Decision{Rule: "default:" + in.Kind}
	switch in.Kind {
	case "llm":
		d.Action = e.cfg.Defaults.LLM
	case "tool":
		d.Action = e.cfg.Defaults.Tool
	default:
		d.Action = e.cfg.Defaults.Record
	}
	return d
}

func (r *Rule) matches(in *Input) bool {
	if r.Kind != "" && r.Kind != in.Kind {
		return false
	}
	if !globMatch(r.Tool, in.Tool) || !globMatch(r.Model, in.Model) || !globMatch(r.Agent, in.Agent) {
		return false
	}
	if r.MinCostUSD > 0 && in.EstCostUSD < r.MinCostUSD {
		return false
	}
	for field, re := range r.input {
		v, ok := in.ToolInput[field]
		if !ok || !re.MatchString(stringify(v)) {
			return false
		}
	}
	return true
}

func globMatch(pattern, s string) bool {
	if pattern == "" {
		return true
	}
	ok, _ := path.Match(pattern, s)
	return ok
}

func stringify(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
