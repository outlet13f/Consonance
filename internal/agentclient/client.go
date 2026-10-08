// Package agentclient calls a project's volume agent (spec 7.2). The operator (plan 3) and the
// API server (plan 4) use it.
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"consonance/internal/agentapi"
)

// Client talks to one agent.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

// New returns a client for the agent at baseURL, e.g.
// "http://volume-agent.cns-a1b2c3d4e5f6.svc:8443". A nil hc gets a client with a 60s timeout:
// a copy takes about a second, after a lock wait of at most 2s and a backup start whose
// checkpoint takes longer under write load. The client never follows redirects, whatever hc
// says: the agent's mux answers a "." or ".." segment with a 301 to the cleaned path, and
// following it turns a DELETE into a GET of another resource that reports success.
func New(baseURL, token string, hc *http.Client) *Client {
	c := http.Client{Timeout: 60 * time.Second}
	if hc != nil {
		c = *hc
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, hc: &c}
}

// APIError is a non-2xx response from the agent.
type APIError struct {
	StatusCode int
	Code       agentapi.ErrorCode // empty when the body was not the agent's
	Message    string
	RetryAfter time.Duration // from Retry-After, on 429
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("agent: %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("agent: %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// CodeOf returns the error code of an agent API error, or "" for any other error.
func CodeOf(err error) agentapi.ErrorCode {
	var e *APIError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func (c *Client) Preflight(ctx context.Context) (agentapi.Preflight, error) {
	var out agentapi.Preflight
	err := c.do(ctx, http.MethodGet, "/v1/preflight", nil, &out)
	return out, err
}

func (c *Client) Usage(ctx context.Context) (agentapi.Usage, error) {
	var out agentapi.Usage
	err := c.do(ctx, http.MethodGet, "/v1/usage", nil, &out)
	return out, err
}

func (c *Client) InitMain(ctx context.Context) (agentapi.Generation, error) {
	var out agentapi.Generation
	err := c.do(ctx, http.MethodPost, "/v1/init-main", nil, &out)
	return out, err
}

func (c *Client) CreateBranchData(ctx context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error) {
	var out agentapi.BranchData
	err := c.do(ctx, http.MethodPost, branchPath(name, "data"), req, &out)
	return out, err
}

func (c *Client) DeleteBranchData(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, branchPath(name, "data"), nil, nil)
}

func (c *Client) CreateRestorePoint(ctx context.Context, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error) {
	var out agentapi.RestorePoint
	err := c.do(ctx, http.MethodPost, branchPath(branch, "restore-points"), req, &out)
	return out, err
}

func (c *Client) ListRestorePoints(ctx context.Context, branch string) ([]agentapi.RestorePoint, error) {
	var out agentapi.RestorePointList
	err := c.do(ctx, http.MethodGet, branchPath(branch, "restore-points"), nil, &out)
	return out.RestorePoints, err
}

func (c *Client) DeleteRestorePoint(ctx context.Context, branch, id string) error {
	return c.do(ctx, http.MethodDelete, branchPath(branch, "restore-points", url.PathEscape(id)), nil, nil)
}

func (c *Client) ListGenerations(ctx context.Context, branch string) (agentapi.Generations, error) {
	var out agentapi.Generations
	err := c.do(ctx, http.MethodGet, branchPath(branch, "generations"), nil, &out)
	return out, err
}

func (c *Client) CreateGeneration(ctx context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error) {
	var out agentapi.Generation
	err := c.do(ctx, http.MethodPost, branchPath(branch, "generations"), req, &out)
	return out, err
}

func (c *Client) ActivateGeneration(ctx context.Context, branch string, n int) (agentapi.Generations, error) {
	var out agentapi.Generations
	err := c.do(ctx, http.MethodPost, branchPath(branch, "generations", strconv.Itoa(n), "activate"), nil, &out)
	return out, err
}

func (c *Client) DetachGeneration(ctx context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error) {
	var out agentapi.DetachResponse
	err := c.do(ctx, http.MethodPost, branchPath(branch, "generations", strconv.Itoa(n), "detach"), req, &out)
	return out, err
}

func (c *Client) DeleteGeneration(ctx context.Context, branch string, n int) error {
	return c.do(ctx, http.MethodDelete, branchPath(branch, "generations", strconv.Itoa(n)), nil, nil)
}

// branchPath returns /v1/branches/<branch>/<rest...>.
func branchPath(branch string, rest ...string) string {
	return "/v1/branches/" + url.PathEscape(branch) + "/" + strings.Join(rest, "/")
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
	req.Header.Set(agentapi.TokenHeader, c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("agent: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// apiError reads an error response. A body that is not the agent's JSON, such as a proxy's
// error page, becomes the message as it is.
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &APIError{StatusCode: resp.StatusCode}
	var body agentapi.Error
	if err := json.Unmarshal(b, &body); err == nil && body.Error != "" {
		e.Code, e.Message = body.Code, body.Error
	} else if e.Message = strings.TrimSpace(string(b)); e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		e.RetryAfter = time.Duration(secs) * time.Second
	}
	return e
}
