// Package store persists runs and steps (the journal) in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/anupam2105/agentd/internal/domain"
)

// ErrNotFound is returned when a run or step does not exist.
var ErrNotFound = errors.New("not found")

// Store is the SQLite-backed journal.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One connection serialises writers (SQLite allows one at a time) and
	// makes every transaction effectively serializable.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the database is usable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

var migrations = []string{
	`CREATE TABLE runs (
		id              TEXT PRIMARY KEY,
		agent           TEXT NOT NULL,
		status          TEXT NOT NULL,
		input           TEXT,
		output          TEXT,
		error           TEXT NOT NULL DEFAULT '',
		max_steps       INTEGER NOT NULL DEFAULT 0,
		max_tokens      INTEGER NOT NULL DEFAULT 0,
		max_cost_usd    REAL NOT NULL DEFAULT 0,
		steps_used      INTEGER NOT NULL DEFAULT 0,
		input_tokens    INTEGER NOT NULL DEFAULT 0,
		output_tokens   INTEGER NOT NULL DEFAULT 0,
		cost_usd        REAL NOT NULL DEFAULT 0,
		reserved_tokens INTEGER NOT NULL DEFAULT 0,
		reserved_cost   REAL NOT NULL DEFAULT 0,
		trace_id        TEXT NOT NULL DEFAULT '',
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL,
		finished_at     INTEGER
	);
	CREATE INDEX runs_status_created ON runs(status, created_at DESC);
	CREATE INDEX runs_agent_created ON runs(agent, created_at DESC);
	CREATE TABLE steps (
		run_id          TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		key             TEXT NOT NULL,
		seq             INTEGER NOT NULL,
		kind            TEXT NOT NULL,
		name            TEXT NOT NULL DEFAULT '',
		status          TEXT NOT NULL,
		request         TEXT NOT NULL,
		request_hash    TEXT NOT NULL,
		result          TEXT,
		error           TEXT,
		attempts        INTEGER NOT NULL DEFAULT 0,
		input_tokens    INTEGER NOT NULL DEFAULT 0,
		output_tokens   INTEGER NOT NULL DEFAULT 0,
		cost_usd        REAL NOT NULL DEFAULT 0,
		est_tokens      INTEGER NOT NULL DEFAULT 0,
		est_cost        REAL NOT NULL DEFAULT 0,
		policy_rule     TEXT NOT NULL DEFAULT '',
		decision        TEXT,
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL,
		started_at      INTEGER,
		finished_at     INTEGER,
		PRIMARY KEY (run_id, key)
	);
	CREATE INDEX steps_status ON steps(status, created_at);`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}

// queryer is satisfied by *sql.DB and *sql.Tx.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx is a journal transaction. All reads and writes inside it are atomic.
type Tx struct {
	q   queryer
	ctx context.Context
}

// InTx runs fn inside a transaction, committing if it returns nil.
func (s *Store) InTx(ctx context.Context, fn func(tx *Tx) error) error {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(&Tx{q: sqlTx, ctx: ctx}); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (s *Store) reader(ctx context.Context) *Tx { return &Tx{q: s.db, ctx: ctx} }

// GetRun returns a run by ID.
func (s *Store) GetRun(ctx context.Context, id string) (*domain.Run, error) {
	return s.reader(ctx).GetRun(id)
}

// GetStep returns one step.
func (s *Store) GetStep(ctx context.Context, runID, key string) (*domain.Step, error) {
	return s.reader(ctx).GetStep(runID, key)
}

// RunFilter narrows ListRuns.
type RunFilter struct {
	Status domain.RunStatus
	Agent  string
	Limit  int
}

// ListRuns returns runs newest first.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]*domain.Run, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, string(f.Status))
	}
	if f.Agent != "" {
		where = append(where, "agent = ?")
		args = append(args, f.Agent)
	}
	q := "SELECT " + runCols + " FROM runs"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // clauses are constants; values are bound parameters
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q += " ORDER BY created_at DESC, id LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*domain.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListSteps returns a run's steps in sequence order.
func (s *Store) ListSteps(ctx context.Context, runID string) ([]*domain.Step, error) {
	return s.querySteps(ctx, "SELECT "+stepCols+" FROM steps WHERE run_id = ? ORDER BY seq", runID)
}

// ListStepsByStatus returns steps in the given status across all runs, oldest first.
func (s *Store) ListStepsByStatus(ctx context.Context, status domain.StepStatus, limit int) ([]*domain.Step, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.querySteps(ctx, "SELECT "+stepCols+" FROM steps WHERE status = ? ORDER BY created_at, run_id, seq LIMIT ?", string(status), limit)
}

// CountStepsByStatus counts steps in a status (used for gauges).
func (s *Store) CountStepsByStatus(ctx context.Context, status domain.StepStatus) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM steps WHERE status = ?", string(status)).Scan(&n)
	return n, err
}

func (s *Store) querySteps(ctx context.Context, q string, args ...any) ([]*domain.Step, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list steps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*domain.Step
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

const runCols = `id, agent, status, input, output, error, max_steps, max_tokens, max_cost_usd,
	steps_used, input_tokens, output_tokens, cost_usd, reserved_tokens, reserved_cost, trace_id,
	created_at, updated_at, finished_at`

const stepCols = `run_id, key, seq, kind, name, status, request, request_hash, result, error,
	attempts, input_tokens, output_tokens, cost_usd, est_tokens, est_cost, policy_rule, decision,
	created_at, updated_at, started_at, finished_at`

type scanner interface{ Scan(dest ...any) error }

func scanRun(sc scanner) (*domain.Run, error) {
	var (
		r                domain.Run
		input, output    sql.NullString
		created, updated int64
		finished         sql.NullInt64
		status           string
	)
	err := sc.Scan(&r.ID, &r.Agent, &status, &input, &output, &r.Error,
		&r.Budget.MaxSteps, &r.Budget.MaxTokens, &r.Budget.MaxCostUSD,
		&r.Usage.Steps, &r.Usage.InputTokens, &r.Usage.OutputTokens, &r.Usage.CostUSD,
		&r.ReservedTokens, &r.ReservedCost, &r.TraceID, &created, &updated, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan run: %w", err)
	}
	r.Status = domain.RunStatus(status)
	r.Input = rawOrNil(input)
	r.Output = rawOrNil(output)
	r.CreatedAt = fromMillis(created)
	r.UpdatedAt = fromMillis(updated)
	r.FinishedAt = optMillis(finished)
	return &r, nil
}

func scanStep(sc scanner) (*domain.Step, error) {
	var (
		st                        domain.Step
		kind, status, request     string
		result, errJSON, decision sql.NullString
		created, updated          int64
		started, finished         sql.NullInt64
	)
	err := sc.Scan(&st.RunID, &st.Key, &st.Seq, &kind, &st.Name, &status, &request, &st.RequestHash,
		&result, &errJSON, &st.Attempts, &st.Usage.InputTokens, &st.Usage.OutputTokens, &st.Usage.CostUSD,
		&st.EstTokens, &st.EstCost, &st.PolicyRule, &decision, &created, &updated, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan step: %w", err)
	}
	st.Kind = domain.StepKind(kind)
	st.Status = domain.StepStatus(status)
	st.Request = json.RawMessage(request)
	st.Result = rawOrNil(result)
	if errJSON.Valid && errJSON.String != "" {
		st.Error = &domain.StepError{}
		if err := json.Unmarshal([]byte(errJSON.String), st.Error); err != nil {
			return nil, fmt.Errorf("decode step error: %w", err)
		}
	}
	if decision.Valid && decision.String != "" {
		st.Decision = &domain.Decision{}
		if err := json.Unmarshal([]byte(decision.String), st.Decision); err != nil {
			return nil, fmt.Errorf("decode decision: %w", err)
		}
	}
	st.CreatedAt = fromMillis(created)
	st.UpdatedAt = fromMillis(updated)
	st.StartedAt = optMillis(started)
	st.FinishedAt = optMillis(finished)
	return &st, nil
}

// GetRun returns a run by ID.
func (t *Tx) GetRun(id string) (*domain.Run, error) {
	return scanRun(t.q.QueryRowContext(t.ctx, "SELECT "+runCols+" FROM runs WHERE id = ?", id))
}

// GetStep returns one step.
func (t *Tx) GetStep(runID, key string) (*domain.Step, error) {
	return scanStep(t.q.QueryRowContext(t.ctx, "SELECT "+stepCols+" FROM steps WHERE run_id = ? AND key = ?", runID, key))
}

// InsertRun creates a run.
func (t *Tx) InsertRun(r *domain.Run) error {
	_, err := t.q.ExecContext(t.ctx, `INSERT INTO runs (id, agent, status, input, output, error,
		max_steps, max_tokens, max_cost_usd, trace_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Agent, string(r.Status), nullRaw(r.Input), nullRaw(r.Output),
		r.Budget.MaxSteps, r.Budget.MaxTokens, r.Budget.MaxCostUSD, r.TraceID,
		millis(r.CreatedAt), millis(r.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

// UpdateRun writes back a run's mutable fields.
func (t *Tx) UpdateRun(r *domain.Run) error {
	_, err := t.q.ExecContext(t.ctx, `UPDATE runs SET status = ?, output = ?, error = ?,
		steps_used = ?, input_tokens = ?, output_tokens = ?, cost_usd = ?,
		reserved_tokens = ?, reserved_cost = ?, updated_at = ?, finished_at = ?
		WHERE id = ?`,
		string(r.Status), nullRaw(r.Output), r.Error,
		r.Usage.Steps, r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CostUSD,
		r.ReservedTokens, r.ReservedCost, millis(r.UpdatedAt), nullMillis(r.FinishedAt), r.ID)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	return nil
}

// NextSeq returns the next step sequence number for a run.
func (t *Tx) NextSeq(runID string) (int64, error) {
	var n int64
	err := t.q.QueryRowContext(t.ctx, "SELECT COALESCE(MAX(seq), 0) + 1 FROM steps WHERE run_id = ?", runID).Scan(&n)
	return n, err
}

// InsertStep creates a step.
func (t *Tx) InsertStep(st *domain.Step) error {
	_, err := t.q.ExecContext(t.ctx, `INSERT INTO steps (`+stepCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, stepArgs(st)...)
	if err != nil {
		return fmt.Errorf("insert step: %w", err)
	}
	return nil
}

// UpdateStep writes back a step's mutable fields.
func (t *Tx) UpdateStep(st *domain.Step) error {
	errJSON, err := jsonOrNull(st.Error)
	if err != nil {
		return err
	}
	decision, err := jsonOrNull(st.Decision)
	if err != nil {
		return err
	}
	_, err = t.q.ExecContext(t.ctx, `UPDATE steps SET status = ?, result = ?, error = ?, attempts = ?,
		input_tokens = ?, output_tokens = ?, cost_usd = ?, est_tokens = ?, est_cost = ?,
		policy_rule = ?, decision = ?, updated_at = ?, started_at = ?, finished_at = ?
		WHERE run_id = ? AND key = ?`,
		string(st.Status), nullRaw(st.Result), errJSON, st.Attempts,
		st.Usage.InputTokens, st.Usage.OutputTokens, st.Usage.CostUSD, st.EstTokens, st.EstCost,
		st.PolicyRule, decision, millis(st.UpdatedAt), nullMillis(st.StartedAt), nullMillis(st.FinishedAt),
		st.RunID, st.Key)
	if err != nil {
		return fmt.Errorf("update step: %w", err)
	}
	return nil
}

// PendingSteps returns a run's steps awaiting approval.
func (t *Tx) PendingSteps(runID string) ([]*domain.Step, error) {
	rows, err := t.q.QueryContext(t.ctx, "SELECT "+stepCols+" FROM steps WHERE run_id = ? AND status IN (?, ?)",
		runID, string(domain.StepAwaitingApproval), string(domain.StepApproved))
	if err != nil {
		return nil, fmt.Errorf("pending steps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*domain.Step
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func stepArgs(st *domain.Step) []any {
	errJSON, _ := jsonOrNull(st.Error)
	decision, _ := jsonOrNull(st.Decision)
	return []any{st.RunID, st.Key, st.Seq, string(st.Kind), st.Name, string(st.Status), string(st.Request),
		st.RequestHash, nullRaw(st.Result), errJSON, st.Attempts, st.Usage.InputTokens, st.Usage.OutputTokens,
		st.Usage.CostUSD, st.EstTokens, st.EstCost, st.PolicyRule, decision,
		millis(st.CreatedAt), millis(st.UpdatedAt), nullMillis(st.StartedAt), nullMillis(st.FinishedAt)}
}

func jsonOrNull[T any](v *T) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return string(b), nil
}

func nullRaw(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func rawOrNil(s sql.NullString) json.RawMessage {
	if !s.Valid || s.String == "" {
		return nil
	}
	return json.RawMessage(s.String)
}

func millis(t time.Time) int64 { return t.UnixMilli() }

func nullMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func optMillis(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromMillis(n.Int64)
	return &t
}
