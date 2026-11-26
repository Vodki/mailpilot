// Package todoist is a minimal client for the Todoist API v1
// (https://api.todoist.com/api/v1). Phase 0 only verifies the token; task
// creation arrives in phase 2.
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
