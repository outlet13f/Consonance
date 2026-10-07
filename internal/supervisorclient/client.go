// Package supervisorclient calls a branch supervisor's internal HTTP API.
package supervisorclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"consonance/internal/supervisorapi"
)

// Client talks to one supervisor.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

// New returns a client for the supervisor at baseURL, e.g. "http://10.42.0.7:8008".
// A nil hc gets a client whose timeout covers a 60s start.
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 75 * time.Second}
	}
	return &Client{baseURL: baseURL, token: token, hc: hc}
}

// APIError is returned for non-2xx responses.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("supervisor: %d: %s", e.StatusCode, e.Message)
}

// IsConflict reports whether err is a 409 from the supervisor.
func IsConflict(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.StatusCode == http.StatusConflict
}

func (c *Client) Status(ctx context.Context) (supervisorapi.Status, error) {
	var s supervisorapi.Status
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &s)
	return s, err
}

func (c *Client) Start(ctx context.Context) (supervisorapi.Status, error) {
	var s supervisorapi.Status
	err := c.do(ctx, http.MethodPost, "/v1/start", nil, &s)
	return s, err
}

func (c *Client) Stop(ctx context.Context) (supervisorapi.Status, error) {
	var s supervisorapi.Status
	err := c.do(ctx, http.MethodPost, "/v1/stop", nil, &s)
	return s, err
}

func (c *Client) Bootstrap(ctx context.Context) (supervisorapi.Status, error) {
	var s supervisorapi.Status
	err := c.do(ctx, http.MethodPost, "/v1/bootstrap", nil, &s)
	return s, err
}

func (c *Client) BackupStart(ctx context.Context, label string) (supervisorapi.BackupStartResponse, error) {
	var r supervisorapi.BackupStartResponse
	err := c.do(ctx, http.MethodPost, "/v1/backup/start", supervisorapi.BackupStartRequest{Label: label}, &r)
	return r, err
}

func (c *Client) BackupStop(ctx context.Context) (supervisorapi.BackupStopResponse, error) {
	var r supervisorapi.BackupStopResponse
	err := c.do(ctx, http.MethodPost, "/v1/backup/stop", nil, &r)
	return r, err
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set(supervisorapi.TokenHeader, c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e supervisorapi.Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return &APIError{StatusCode: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
