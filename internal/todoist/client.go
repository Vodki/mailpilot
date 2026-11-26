// Package todoist is a minimal client for the Todoist API v1
// (https://api.todoist.com/api/v1): token check and task creation.
package todoist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Vodki/mailpilot/internal/config"
)

// Client calls the Todoist API.
type Client struct {
	base  string
	token config.Secret
	http  *http.Client
}

// New builds a client. apiBase is normally https://api.todoist.com/api/v1.
func New(apiBase string, token config.Secret, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimRight(apiBase, "/"), token: token, http: hc}
}

// User is the subset of the user object we display.
type User struct {
	ID       string `json:"id"`
	FullName string `json:"full_name"`
}

// CurrentUser returns the token owner, proving the token works.
func (c *Client) CurrentUser(ctx context.Context) (*User, error) {
	if c.token.Empty() {
		return nil, errors.New("todoist token is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("todoist user: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("todoist user: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("todoist user: HTTP %d", resp.StatusCode)
	}
	var u User
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("todoist user: decode: %w", err)
	}
	return &u, nil
}

// NewTask is the payload of POST /tasks.
type NewTask struct {
	Content     string   `json:"content"`
	Description string   `json:"description,omitempty"`
	ProjectID   string   `json:"project_id,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Priority    int      `json:"priority,omitempty"`     // 1 (normal) .. 4 (urgent)
	DueDate     string   `json:"due_date,omitempty"`     // YYYY-MM-DD
	DueDatetime string   `json:"due_datetime,omitempty"` // RFC 3339
}

// Task is the subset of the created task we keep.
type Task struct {
	ID string `json:"id"`
}

// CreateTask creates a task. requestID makes the call idempotent on the
// Todoist side (X-Request-Id), so a retry after a timeout cannot duplicate.
func (c *Client) CreateTask(ctx context.Context, t NewTask, requestID string) (*Task, error) {
	if c.token.Empty() {
		return nil, errors.New("todoist token is not configured")
	}
	body, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/tasks", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	req.Header.Set("Content-Type", "application/json")
	if requestID != "" {
		req.Header.Set("X-Request-Id", requestID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("todoist create task: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("todoist create task: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("todoist create task: HTTP %d", resp.StatusCode)
	}
	var task Task
	if err := json.Unmarshal(raw, &task); err != nil || task.ID == "" {
		return nil, fmt.Errorf("todoist create task: unexpected response")
	}
	return &task, nil
}
