// Package telegram is a minimal Bot API client. Phase 0 only verifies the
// token; long polling, commands and callbacks arrive in phase 1.
package telegram

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

// Client calls the Bot API.
type Client struct {
	base  string
	token config.Secret
	http  *http.Client
}

// New builds a client. apiBase is normally https://api.telegram.org.
func New(apiBase string, token config.Secret, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 70 * time.Second}
	}
	return &Client{base: strings.TrimRight(apiBase, "/"), token: token, http: hc}
}

// User is the subset of the Bot API User object we use.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
}

// APIError is a Bot API error response.
type APIError struct {
	Method      string
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// GetMe returns the bot's identity, proving the token works.
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var u User
	if err := c.call(ctx, "getMe", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	if c.token.Empty() {
		return errors.New("telegram token is not configured")
	}
	var body io.Reader
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b))
	}
	// The token is part of the URL by Bot API design; errors below are
	// rebuilt so the URL (and thus the token) never reaches a log line.
	url := c.base + "/bot" + c.token.Reveal() + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("telegram %s: build request", method)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, scrubURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: read body: %w", method, err)
	}
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d, invalid JSON", method, resp.StatusCode)
	}
	if !ar.OK {
		return &APIError{Method: method, Code: ar.ErrorCode, Description: ar.Description}
	}
	if out != nil {
		if err := json.Unmarshal(ar.Result, out); err != nil {
			return fmt.Errorf("telegram %s: decode result: %w", method, err)
		}
	}
	return nil
}

// scrubURLError drops the request URL, which embeds the bot token.
func scrubURLError(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner
		}
	}
	return errors.New("request failed")
}
