// Package notify tells humans that a step is waiting for approval.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/anupam2105/agentd/internal/domain"
)

// Notifier is told about steps that need a human decision.
type Notifier interface {
	ApprovalRequested(ctx context.Context, run *domain.Run, step *domain.Step) error
}

// Slack posts to an incoming webhook.
type Slack struct {
	WebhookURL string
	// PublicURL is agentd's externally reachable base URL, used in the message.
	PublicURL string
	Client    *http.Client
}

// NewSlack builds a Slack notifier.
func NewSlack(webhookURL, publicURL string) *Slack {
	return &Slack{WebhookURL: webhookURL, PublicURL: publicURL, Client: &http.Client{Timeout: 10 * time.Second}}
}

// ApprovalRequested implements Notifier.
func (s *Slack) ApprovalRequested(ctx context.Context, run *domain.Run, step *domain.Step) error {
	req := string(step.Request)
	if len(req) > 1500 {
		req = req[:1500] + "…"
	}
	text := fmt.Sprintf(":raising_hand: *agentd approval needed*\nAgent `%s` · run `%s` · step `%s` (%s %s) · rule `%s`\n```%s```\nApprove: `agentd approve %s %s`   Deny: `agentd deny %s %s --reason \"...\"`",
		run.Agent, run.ID, step.Key, step.Kind, step.Name, step.PolicyRule, req, run.ID, step.Key, run.ID, step.Key)
	if s.PublicURL != "" {
		text += fmt.Sprintf("\nAPI: POST %s/v1/runs/%s/steps/%s/decision", s.PublicURL, run.ID, step.Key)
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, s.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(r)
	if err != nil {
		return fmt.Errorf("post slack webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
