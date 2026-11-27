// Package engine implements the durable step journal: every LLM call and tool
// execution of a run is a step identified by (run_id, step_key). A step runs at
// most once to completion; replays return the journaled result.
package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/anupam2105/agentd/internal/domain"
	"github.com/anupam2105/agentd/internal/llm"
	"github.com/anupam2105/agentd/internal/notify"
	"github.com/anupam2105/agentd/internal/observability"
	"github.com/anupam2105/agentd/internal/policy"
	"github.com/anupam2105/agentd/internal/sandbox"
	"github.com/anupam2105/agentd/internal/store"
	"github.com/anupam2105/agentd/internal/tracing"
)

// Options configures an Engine.
type Options struct {
	Store    *store.Store
	LLM      *llm.Gateway
	Tools    *sandbox.Registry
	Policy   *policy.Engine
	Metrics  *observability.Metrics
	Tracer   *tracing.Tracer // optional
	Notifier notify.Notifier // optional
	Logger   *slog.Logger

	// LLMTimeout bounds one provider call.
	LLMTimeout time.Duration
	// Lease is how long a running step is owned before another request may
	// reclaim it (e.g. after agentd crashed mid-execution).
	Lease time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Engine executes and journals steps.
type Engine struct {
	o Options
}

// New builds an engine.
func New(opts *Options) *Engine {
	o := *opts
	if o.LLMTimeout <= 0 {
		o.LLMTimeout = 2 * time.Minute
	}
	if o.Lease <= 0 {
		o.Lease = 10 * time.Minute
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Tools == nil {
		o.Tools, _ = sandbox.NewRegistry(nil, nil)
	}
	if o.Policy == nil {
		o.Policy, _ = policy.New(policy.Config{})
	}
	if o.Metrics == nil {
		o.Metrics = observability.NewMetrics()
	}
	e := &Engine{o: o}
	o.Metrics.RegisterPendingApprovals(func() float64 {
		n, err := o.Store.CountStepsByStatus(context.Background(), domain.StepAwaitingApproval)
		if err != nil {
			return 0
		}
		return float64(n)
	})
	return e
}

// Store exposes the underlying store for read endpoints.
func (e *Engine) Store() *store.Store { return e.o.Store }

// Tools returns the tool registry.
func (e *Engine) Tools() *sandbox.Registry { return e.o.Tools }

func (e *Engine) now() time.Time { return e.o.Now().Truncate(time.Millisecond) }

// ---------------------------------------------------------------- runs

// CreateRunRequest is the body of POST /v1/runs.
type CreateRunRequest struct {
	ID     string          `json:"id,omitempty"`
	Agent  string          `json:"agent"`
	Input  json.RawMessage `json:"input,omitempty"`
	Budget domain.Budget   `json:"budget"`
}

// CreateRun starts a run. Supplying an ID makes the call idempotent: if the
// run exists for the same agent it is returned with created=false.
func (e *Engine) CreateRun(ctx context.Context, req *CreateRunRequest) (*domain.Run, bool, error) {
	if !domain.ValidID(req.Agent) {
		return nil, false, invalid("agent must match %s", "[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
	}
	if req.ID == "" {
		req.ID = newRunID()
	} else if !domain.ValidID(req.ID) {
		return nil, false, invalid("id must match %s", "[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
	}
	if req.Budget.MaxSteps < 0 || req.Budget.MaxTokens < 0 || req.Budget.MaxCostUSD < 0 {
		return nil, false, invalid("budget limits must be >= 0")
	}
	if len(req.Input) > 0 && !json.Valid(req.Input) {
		return nil, false, invalid("input must be valid JSON")
	}
	var (
		run     *domain.Run
		created bool
	)
	err := e.o.Store.InTx(ctx, func(tx *store.Tx) error {
		existing, err := tx.GetRun(req.ID)
		switch {
		case err == nil:
			if existing.Agent != req.Agent {
				return errf(http.StatusConflict, ErrCodeConflict, "run %q already exists for agent %q", req.ID, existing.Agent)
			}
			run = existing
			return nil
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		now := e.now()
		run = &domain.Run{
			ID: req.ID, Agent: req.Agent, Status: domain.RunRunning, Input: compact(req.Input),
			Budget: req.Budget, TraceID: tracing.TraceID(req.ID), CreatedAt: now, UpdatedAt: now,
		}
		created = true
		return tx.InsertRun(run)
	})
	if err != nil {
		return nil, false, err
	}
	if created {
		e.o.Metrics.RunsStarted.Inc()
		e.o.Logger.Info("run started", "run_id", run.ID, "agent", run.Agent)
	}
	return run, created, nil
}

// FinishRunRequest is the body of POST /v1/runs/{id}/finish.
type FinishRunRequest struct {
	Status domain.RunStatus `json:"status"`
	Output json.RawMessage  `json:"output,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// FinishRun marks a run completed or failed. Finishing again with the same
// status is a no-op.
func (e *Engine) FinishRun(ctx context.Context, id string, req *FinishRunRequest) (*domain.Run, error) {
	if req.Status != domain.RunCompleted && req.Status != domain.RunFailed {
		return nil, invalid("status must be completed or failed")
	}
	if len(req.Output) > 0 && !json.Valid(req.Output) {
		return nil, invalid("output must be valid JSON")
	}
	return e.endRun(ctx, id, req.Status, compact(req.Output), req.Error)
}

// CancelRun cancels a run; steps awaiting approval are denied.
func (e *Engine) CancelRun(ctx context.Context, id, reason string) (*domain.Run, error) {
	if reason == "" {
		reason = "cancelled"
	}
	return e.endRun(ctx, id, domain.RunCancelled, nil, reason)
}

func (e *Engine) endRun(ctx context.Context, id string, status domain.RunStatus, output json.RawMessage, msg string) (*domain.Run, error) {
	var (
		run     *domain.Run
		changed bool
	)
	err := e.o.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		run, err = tx.GetRun(id)
		if errors.Is(err, store.ErrNotFound) {
			return notFound("run %q not found", id)
		}
		if err != nil {
			return err
		}
		if run.Terminal() {
			if run.Status == status {
				return nil
			}
			return errf(http.StatusConflict, ErrCodeRunNotRunning, "run is already %s", run.Status)
		}
		now := e.now()
		pending, err := tx.PendingSteps(id)
		if err != nil {
			return err
		}
		for _, st := range pending {
			st.Status = domain.StepDenied
			st.Error = &domain.StepError{Code: domain.CodeRunCancelled, Message: "run ended with status " + string(status)}
			st.UpdatedAt, st.FinishedAt = now, &now
			if err := tx.UpdateStep(st); err != nil {
				return err
			}
		}
		run.Status, run.Output, run.Error = status, output, msg
		run.UpdatedAt, run.FinishedAt = now, &now
		changed = true
		return tx.UpdateRun(run)
	})
	if err != nil {
		return nil, err
	}
	if changed {
		e.o.Metrics.RunsFinished.WithLabelValues(string(status)).Inc()
		e.o.Tracer.RecordRun(run)
		e.o.Logger.Info("run finished", "run_id", run.ID, "status", status, "steps", run.Usage.Steps,
			"tokens", run.Usage.InputTokens+run.Usage.OutputTokens, "cost_usd", run.Usage.CostUSD)
	}
	return run, nil
}

// ---------------------------------------------------------------- steps

// StepRequest is the body of POST /v1/runs/{id}/steps.
type StepRequest struct {
	Key     string          `json:"key"`
	Kind    domain.StepKind `json:"kind"`
	Request json.RawMessage `json:"request"`
}

type toolRequest struct {
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input"`
}

// prepared is a validated step request.
type prepared struct {
	kind      domain.StepKind
	name      string
	canonical []byte
	hash      string

	llmReq    *llm.Request
	price     llm.Price
	priceOK   bool
	estTokens int64
	estCost   float64

	toolInput map[string]any
	toolRaw   []byte
	value     json.RawMessage
}

func (e *Engine) prepare(req *StepRequest) (*prepared, error) {
	if !domain.ValidStepKey(req.Key) {
		return nil, invalid("key must match %s", "[A-Za-z0-9][A-Za-z0-9._:-]{0,127}")
	}
	if len(req.Request) == 0 {
		return nil, invalid("request is required")
	}
	canonical, err := canonicalJSON(req.Request)
	if err != nil {
		return nil, invalid("%s", err)
	}
	p := &prepared{kind: req.Kind, canonical: canonical, hash: requestHash(string(req.Kind), canonical)}
	switch req.Kind {
	case domain.KindLLM:
		var r llm.Request
		if err := json.Unmarshal(req.Request, &r); err != nil {
			return nil, invalid("llm request: %s", err)
		}
		if err := r.Validate(); err != nil {
			return nil, invalid("llm request: %s", err)
		}
		if _, err := e.o.LLM.Resolve(&r); err != nil {
			return nil, invalid("%s", err)
		}
		p.llmReq, p.name = &r, r.Model
		in := llm.EstimateInputTokens(&r)
		p.estTokens = in + r.MaxTokens
		p.price, p.priceOK = e.o.LLM.Pricing.Lookup(r.Model)
		p.estCost = p.price.Cost(in, r.MaxTokens)
	case domain.KindTool:
		var r toolRequest
		if err := json.Unmarshal(req.Request, &r); err != nil {
			return nil, invalid("tool request: %s", err)
		}
		if r.Tool == "" {
			return nil, invalid("tool request: tool is required")
		}
		if r.Input == nil {
			r.Input = map[string]any{}
		}
		raw, err := json.Marshal(r.Input)
		if err != nil {
			return nil, invalid("tool input: %s", err)
		}
		p.name, p.toolInput, p.toolRaw = r.Tool, r.Input, raw
	case domain.KindRecord:
		var r struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(req.Request, &r); err != nil || len(r.Value) == 0 {
			return nil, invalid("record request must be {\"value\": ...}")
		}
		p.value = r.Value
	default:
		return nil, invalid("kind must be llm, tool or record")
	}
	return p, nil
}

// StepOutcome is what ExecuteStep returns.
type StepOutcome struct {
	Step *domain.Step
	// Replayed is true when the result came from the journal.
	Replayed bool
}

// ExecuteStep runs or replays a step. The returned step's status tells the
// caller what happened: completed/failed/denied are final (failed with
// error.retryable=true may be retried by re-posting), awaiting_approval means
// poll until approved and re-post.
func (e *Engine) ExecuteStep(ctx context.Context, runID string, req *StepRequest) (*StepOutcome, error) {
	p, err := e.prepare(req)
	if err != nil {
		return nil, err
	}

	var (
		run      *domain.Run
		st       *domain.Step
		execute  bool
		replayed bool
		notifyMe bool
		events   []func()
	)
	err = e.o.Store.InTx(ctx, func(tx *store.Tx) error {
		events = events[:0]
		var err error
		run, err = tx.GetRun(runID)
		if errors.Is(err, store.ErrNotFound) {
			return notFound("run %q not found", runID)
		}
		if err != nil {
			return err
		}
		if p.kind == domain.KindLLM && run.Budget.MaxCostUSD > 0 && !p.priceOK {
			return invalid("no pricing configured for model %q; required because the run has max_cost_usd", p.name)
		}
		now := e.now()

		st, err = tx.GetStep(runID, req.Key)
		switch {
		case errors.Is(err, store.ErrNotFound):
			st = nil
		case err != nil:
			return err
		}

		if st != nil {
			if st.Kind != p.kind || st.RequestHash != p.hash {
				return &Error{Status: http.StatusConflict, Code: ErrCodeNondeterminism, Message: "step " + req.Key +
					" was journaled with a different request; agent code must issue the same steps in the same order on replay"}
			}
			switch {
			case st.Terminal():
				replayed = true
				return nil
			case st.Status == domain.StepAwaitingApproval:
				return nil
			case st.Status == domain.StepRunning:
				if st.StartedAt != nil && now.Sub(*st.StartedAt) < e.o.Lease {
					return &Error{Status: http.StatusConflict, Code: ErrCodeInProgress, Retryable: true,
						Message: "step is executing; retry shortly"}
				}
				// Lease expired: the previous executor died. Reservation is still held.
				st.Attempts++
				st.StartedAt, st.UpdatedAt = &now, now
				execute = true
				return tx.UpdateStep(st)
			default: // approved, or failed+retryable
				if run.Terminal() {
					st.Status = domain.StepDenied
					st.Error = &domain.StepError{Code: domain.CodeRunCancelled, Message: "run is " + string(run.Status)}
					st.UpdatedAt, st.FinishedAt = now, &now
					return tx.UpdateStep(st)
				}
				return e.admit(tx, run, st, p, now, &execute, &events)
			}
		}

		if run.Terminal() {
			return errf(http.StatusConflict, ErrCodeRunNotRunning, "run is %s", run.Status)
		}
		seq, err := tx.NextSeq(runID)
		if err != nil {
			return err
		}
		st = &domain.Step{
			RunID: runID, Key: req.Key, Seq: seq, Kind: p.kind, Name: p.name, Request: p.canonical,
			RequestHash: p.hash, CreatedAt: now, UpdatedAt: now, EstTokens: p.estTokens, EstCost: p.estCost,
		}
		finish := func(status domain.StepStatus, se *domain.StepError) error {
			st.Status, st.Error, st.FinishedAt = status, se, &now
			return tx.InsertStep(st)
		}

		if p.kind == domain.KindRecord {
			st.Result = compact(p.value)
			return finish(domain.StepCompleted, nil)
		}
		if p.kind == domain.KindTool {
			tool, ok := e.o.Tools.Get(p.name)
			if !ok {
				return finish(domain.StepDenied, &domain.StepError{Code: domain.CodeUnknownTool, Message: "tool " + p.name + " is not registered"})
			}
			if err := tool.ValidateInput(p.toolInput); err != nil {
				return finish(domain.StepFailed, &domain.StepError{Code: domain.CodeInvalidInput, Message: err.Error()})
			}
		}

		d := e.o.Policy.Evaluate(&policy.Input{
			Kind: string(p.kind), Agent: run.Agent, Tool: toolName(p), Model: modelName(p),
			ToolInput: p.toolInput, EstCostUSD: p.estCost,
		})
		st.PolicyRule = d.Rule
		events = append(events, func() { e.o.Metrics.PolicyDecisions.WithLabelValues(string(p.kind), string(d.Action)).Inc() })
		switch d.Action {
		case policy.Deny:
			msg := "denied by policy rule " + d.Rule
			if d.Reason != "" {
				msg += ": " + d.Reason
			}
			return finish(domain.StepDenied, &domain.StepError{Code: domain.CodePolicyDenied, Message: msg})
		case policy.RequireApproval:
			st.Status = domain.StepAwaitingApproval
			notifyMe = true
			return tx.InsertStep(st)
		}
		st.Status = domain.StepApproved // placeholder; admit sets running or denied
		if err := tx.InsertStep(st); err != nil {
			return err
		}
		return e.admit(tx, run, st, p, now, &execute, &events)
	})
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		ev()
	}
	if replayed {
		e.o.Metrics.StepReplays.WithLabelValues(string(st.Kind)).Inc()
		return &StepOutcome{Step: st, Replayed: true}, nil
	}
	if !execute {
		if st.Status != domain.StepAwaitingApproval || notifyMe {
			e.o.Metrics.Steps.WithLabelValues(string(st.Kind), string(st.Status)).Inc()
		}
		if notifyMe {
			e.notifyApproval(ctx, run, st)
		}
		return &StepOutcome{Step: st}, nil
	}
	return e.run(ctx, run, st, p)
}

// admit checks budgets and moves the step to running with a reservation, or
// denies it.
func (e *Engine) admit(tx *store.Tx, run *domain.Run, st *domain.Step, p *prepared, now time.Time, execute *bool, events *[]func()) error {
	b := run.Budget
	firstAttempt := st.Attempts == 0
	var se *domain.StepError
	switch {
	case firstAttempt && b.MaxSteps > 0 && run.Usage.Steps+1 > b.MaxSteps:
		se = &domain.StepError{Code: domain.CodeBudgetSteps, Message: "max_steps budget exhausted"}
	case p.kind == domain.KindLLM && b.MaxTokens > 0 &&
		run.Usage.InputTokens+run.Usage.OutputTokens+run.ReservedTokens+p.estTokens > b.MaxTokens:
		se = &domain.StepError{Code: domain.CodeBudgetTokens, Message: "max_tokens budget would be exceeded (reduce max_tokens or raise the budget)"}
	case p.kind == domain.KindLLM && b.MaxCostUSD > 0 && run.Usage.CostUSD+run.ReservedCost+p.estCost > b.MaxCostUSD:
		se = &domain.StepError{Code: domain.CodeBudgetCost, Message: "max_cost_usd budget would be exceeded"}
	}
	if se != nil {
		limit := se.Code[len("budget_exceeded_"):]
		*events = append(*events, func() { e.o.Metrics.BudgetDenials.WithLabelValues(limit).Inc() })
		st.Status, st.Error, st.UpdatedAt, st.FinishedAt = domain.StepDenied, se, now, &now
		return tx.UpdateStep(st)
	}
	if firstAttempt {
		run.Usage.Steps++
	}
	if p.kind == domain.KindLLM {
		st.EstTokens, st.EstCost = p.estTokens, p.estCost
		run.ReservedTokens += p.estTokens
		run.ReservedCost += p.estCost
	}
	run.UpdatedAt = now
	st.Status, st.Error, st.Attempts = domain.StepRunning, nil, st.Attempts+1
	st.StartedAt, st.UpdatedAt = &now, now
	*execute = true
	if err := tx.UpdateRun(run); err != nil {
		return err
	}
	return tx.UpdateStep(st)
}

// run executes a step outside any transaction and journals the outcome.
func (e *Engine) run(ctx context.Context, run *domain.Run, st *domain.Step, p *prepared) (*StepOutcome, error) {
	// The result must be journaled even if the client disconnects mid-call.
	ctx = context.WithoutCancel(ctx)
	attempt := st.Attempts
	start := e.o.Now()
	var (
		result []byte
		usage  domain.Usage
		se     *domain.StepError
		info   tracing.StepInfo
	)
	switch p.kind {
	case domain.KindLLM:
		cctx, cancel := context.WithTimeout(ctx, e.o.LLMTimeout)
		resp, err := e.o.LLM.Complete(cctx, p.llmReq)
		cancel()
		if err != nil {
			se = providerError(err)
			info.ErrorMessage = err.Error()
			break
		}
		info.Provider, info.StopReason = resp.Provider, resp.StopReason
		usage.InputTokens, usage.OutputTokens = resp.Usage.InputTokens, resp.Usage.OutputTokens
		usage.CostUSD = p.price.Cost(usage.InputTokens, usage.OutputTokens)
		result, err = json.Marshal(resp)
		if err != nil {
			return nil, err
		}
		e.o.Metrics.LLMTokens.WithLabelValues(resp.Provider, p.name, "input").Add(float64(usage.InputTokens))
		e.o.Metrics.LLMTokens.WithLabelValues(resp.Provider, p.name, "output").Add(float64(usage.OutputTokens))
		e.o.Metrics.LLMCost.WithLabelValues(resp.Provider, p.name).Add(usage.CostUSD)
	case domain.KindTool:
		tool, ok := e.o.Tools.Get(p.name)
		if !ok { // config reloaded between admit and run
			se = &domain.StepError{Code: domain.CodeUnknownTool, Message: "tool " + p.name + " is not registered"}
			break
		}
		spec, err := tool.Spec(run.ID, st.Key, attempt, p.toolInput, p.toolRaw)
		if err != nil {
			se = &domain.StepError{Code: domain.CodeInvalidInput, Message: err.Error()}
			break
		}
		if e.o.Tools.Runner == nil {
			se = &domain.StepError{Code: domain.CodeSandboxError, Message: "no sandbox runner configured", Retryable: true}
			e.o.Metrics.ToolExecutions.WithLabelValues(p.name, "error").Inc()
			break
		}
		cctx, cancel := context.WithTimeout(ctx, tool.Timeout()+2*time.Minute) // headroom for image pulls
		res, err := e.o.Tools.Runner.Run(cctx, spec)
		cancel()
		if err != nil {
			se = &domain.StepError{Code: domain.CodeSandboxError, Message: err.Error(), Retryable: true}
			info.ErrorMessage = err.Error()
			e.o.Metrics.ToolExecutions.WithLabelValues(p.name, "error").Inc()
			break
		}
		outcome := "ok"
		switch {
		case res.TimedOut:
			outcome = "timeout"
		case res.ExitCode != 0:
			outcome = "nonzero_exit"
		}
		e.o.Metrics.ToolExecutions.WithLabelValues(p.name, outcome).Inc()
		exit := res.ExitCode
		info.ExitCode, info.TimedOut = &exit, res.TimedOut
		result, err = json.Marshal(res)
		if err != nil {
			return nil, err
		}
	}
	end := e.o.Now()
	e.o.Metrics.StepDuration.WithLabelValues(string(p.kind)).Observe(end.Sub(start).Seconds())

	var final *domain.Step
	err := e.o.Store.InTx(ctx, func(tx *store.Tx) error {
		cur, err := tx.GetStep(run.ID, st.Key)
		if err != nil {
			return err
		}
		if cur.Status != domain.StepRunning || cur.Attempts != attempt {
			// Someone reclaimed the step after our lease expired; their result wins.
			final = cur
			return nil
		}
		r, err := tx.GetRun(run.ID)
		if err != nil {
			return err
		}
		now := e.now()
		r.ReservedTokens = max(0, r.ReservedTokens-cur.EstTokens)
		r.ReservedCost = max(0, r.ReservedCost-cur.EstCost)
		r.Usage.InputTokens += usage.InputTokens
		r.Usage.OutputTokens += usage.OutputTokens
		r.Usage.CostUSD += usage.CostUSD
		r.UpdatedAt = now
		cur.EstTokens, cur.EstCost = 0, 0
		cur.Usage.InputTokens += usage.InputTokens
		cur.Usage.OutputTokens += usage.OutputTokens
		cur.Usage.CostUSD += usage.CostUSD
		cur.UpdatedAt = now
		if se != nil {
			cur.Status, cur.Error = domain.StepFailed, se
			if !se.Retryable {
				cur.FinishedAt = &now
			}
		} else {
			cur.Status, cur.Result, cur.Error, cur.FinishedAt = domain.StepCompleted, result, nil, &now
		}
		if err := tx.UpdateRun(r); err != nil {
			return err
		}
		if err := tx.UpdateStep(cur); err != nil {
			return err
		}
		final, run = cur, r
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.o.Metrics.Steps.WithLabelValues(string(final.Kind), string(final.Status)).Inc()
	e.o.Tracer.RecordStep(run, final, start, end, &info)
	if se != nil {
		e.o.Logger.Warn("step failed", "run_id", run.ID, "key", st.Key, "kind", st.Kind, "code", se.Code,
			"retryable", se.Retryable, "error", se.Message)
	}
	return &StepOutcome{Step: final}, nil
}

func providerError(err error) *domain.StepError {
	var pe *llm.Error
	if errors.As(err, &pe) {
		if pe.Retryable {
			return &domain.StepError{Code: domain.CodeProviderError, Message: pe.Error(), Retryable: true}
		}
		if pe.Status >= 400 && pe.Status < 500 {
			return &domain.StepError{Code: domain.CodeProviderRequest, Message: pe.Error()}
		}
		return &domain.StepError{Code: domain.CodeProviderError, Message: pe.Error()}
	}
	return &domain.StepError{Code: domain.CodeProviderError, Message: err.Error(), Retryable: llm.IsRetryable(err)}
}

// ---------------------------------------------------------------- approvals

// DecisionRequest is the body of POST .../decision.
type DecisionRequest struct {
	Approve bool   `json:"approve"`
	By      string `json:"by"`
	Reason  string `json:"reason,omitempty"`
}

// Decide approves or denies a step awaiting approval. Repeating the same
// decision is a no-op.
func (e *Engine) Decide(ctx context.Context, runID, key string, req *DecisionRequest) (*domain.Step, error) {
	if req.By == "" {
		req.By = "unknown"
	}
	var (
		st      *domain.Step
		changed bool
		waited  time.Duration
	)
	err := e.o.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		st, err = tx.GetStep(runID, key)
		if errors.Is(err, store.ErrNotFound) {
			return notFound("step %s/%s not found", runID, key)
		}
		if err != nil {
			return err
		}
		if st.Status != domain.StepAwaitingApproval {
			if st.Decision != nil && st.Decision.Approved == req.Approve {
				return nil
			}
			return errf(http.StatusConflict, ErrCodeConflict, "step is %s, not awaiting approval", st.Status)
		}
		now := e.now()
		waited = now.Sub(st.CreatedAt)
		st.Decision = &domain.Decision{Approved: req.Approve, By: req.By, Reason: req.Reason, At: now}
		st.UpdatedAt = now
		if req.Approve {
			st.Status = domain.StepApproved
		} else {
			msg := "denied by " + req.By
			if req.Reason != "" {
				msg += ": " + req.Reason
			}
			st.Status, st.Error, st.FinishedAt = domain.StepDenied, &domain.StepError{Code: domain.CodeApprovalDenied, Message: msg}, &now
		}
		changed = true
		return tx.UpdateStep(st)
	})
	if err != nil {
		return nil, err
	}
	if changed {
		decision := "denied"
		if req.Approve {
			decision = "approved"
		} else {
			e.o.Metrics.Steps.WithLabelValues(string(st.Kind), string(domain.StepDenied)).Inc()
		}
		e.o.Metrics.ApprovalDecisions.WithLabelValues(decision).Inc()
		e.o.Metrics.ApprovalWait.Observe(waited.Seconds())
		e.o.Logger.Info("approval decision", "run_id", runID, "key", key, "decision", decision, "by", req.By)
	}
	return st, nil
}

func (e *Engine) notifyApproval(ctx context.Context, run *domain.Run, st *domain.Step) {
	e.o.Logger.Info("step awaiting approval", "run_id", run.ID, "key", st.Key, "kind", st.Kind, "name", st.Name, "rule", st.PolicyRule)
	if e.o.Notifier == nil {
		return
	}
	runCopy, stCopy := *run, *st
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := e.o.Notifier.ApprovalRequested(ctx, &runCopy, &stCopy); err != nil {
			e.o.Logger.Warn("approval notification failed", "run_id", run.ID, "key", st.Key, "error", err)
		}
	}()
}

// ---------------------------------------------------------------- helpers

func toolName(p *prepared) string {
	if p.kind == domain.KindTool {
		return p.name
	}
	return ""
}

func modelName(p *prepared) string {
	if p.kind == domain.KindLLM {
		return p.name
	}
	return ""
}

func compact(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

func newRunID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "run_" + hex.EncodeToString(b[:])
}
