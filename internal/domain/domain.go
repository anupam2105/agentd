// Package domain defines the run and step types shared across agentd.
package domain

import (
	"encoding/json"
	"regexp"
	"time"
)

// RunStatus is the lifecycle state of a run.
type RunStatus string

// Run statuses.
const (
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// StepKind identifies what a step does.
type StepKind string

// Step kinds.
const (
	KindLLM    StepKind = "llm"
	KindTool   StepKind = "tool"
	KindRecord StepKind = "record" // journals a client-computed value (non-deterministic side effect)
)

// StepStatus is the lifecycle state of a step.
type StepStatus string

// Step statuses. completed, failed (non-retryable) and denied are terminal and
// replay verbatim.
const (
	StepAwaitingApproval StepStatus = "awaiting_approval"
	StepApproved         StepStatus = "approved"
	StepRunning          StepStatus = "running"
	StepCompleted        StepStatus = "completed"
	StepFailed           StepStatus = "failed"
	StepDenied           StepStatus = "denied"
)

// Error codes returned on steps.
const (
	CodePolicyDenied    = "policy_denied"
	CodeApprovalDenied  = "approval_denied"
	CodeRunCancelled    = "run_cancelled"
	CodeUnknownTool     = "unknown_tool"
	CodeInvalidInput    = "invalid_input"
	CodeBudgetSteps     = "budget_exceeded_steps"
	CodeBudgetTokens    = "budget_exceeded_tokens"
	CodeBudgetCost      = "budget_exceeded_cost"
	CodeProviderError   = "provider_error"
	CodeSandboxError    = "sandbox_error"
	CodeProviderRequest = "provider_rejected_request"
)

// Budget caps what a run may consume. Zero means unlimited.
type Budget struct {
	MaxSteps   int64   `json:"max_steps,omitempty"`
	MaxTokens  int64   `json:"max_tokens,omitempty"`
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
}

// Usage is what a run or step has consumed.
type Usage struct {
	Steps        int64   `json:"steps,omitempty"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// Run is one execution of an agent.
type Run struct {
	ID         string          `json:"id"`
	Agent      string          `json:"agent"`
	Status     RunStatus       `json:"status"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Error      string          `json:"error,omitempty"`
	Budget     Budget          `json:"budget"`
	Usage      Usage           `json:"usage"`
	TraceID    string          `json:"trace_id,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`

	// Reservations held by in-flight LLM steps so parallel steps cannot
	// jointly overshoot the budget.
	ReservedTokens int64   `json:"-"`
	ReservedCost   float64 `json:"-"`
}

// Terminal reports whether the run has finished.
func (r *Run) Terminal() bool { return r.Status != RunRunning }

// StepError describes why a step failed or was denied.
type StepError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

// Decision records a human approval decision.
type Decision struct {
	Approved bool      `json:"approved"`
	By       string    `json:"by"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}

// Step is one journaled side effect of a run.
type Step struct {
	RunID       string          `json:"run_id"`
	Key         string          `json:"key"`
	Seq         int64           `json:"seq"`
	Kind        StepKind        `json:"kind"`
	Name        string          `json:"name,omitempty"` // model or tool name
	Status      StepStatus      `json:"status"`
	Request     json.RawMessage `json:"request"`
	RequestHash string          `json:"-"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *StepError      `json:"error,omitempty"`
	Attempts    int             `json:"attempts"`
	Usage       Usage           `json:"usage"`
	PolicyRule  string          `json:"policy_rule,omitempty"`
	Decision    *Decision       `json:"decision,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`

	EstTokens int64   `json:"-"`
	EstCost   float64 `json:"-"`
}

// Terminal reports whether the step's outcome is final and replays verbatim.
func (s *Step) Terminal() bool {
	switch s.Status {
	case StepCompleted, StepDenied:
		return true
	case StepFailed:
		return s.Error == nil || !s.Error.Retryable
	default:
		return false
	}
}

var (
	idPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// ValidID reports whether s is an acceptable run ID or agent name.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// ValidStepKey reports whether s is an acceptable step key.
func ValidStepKey(s string) bool { return keyPattern.MatchString(s) }
