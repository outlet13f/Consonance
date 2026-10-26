//go:build e2e || perf

package e2eutil

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// The views of the REST API (internal/httpapi/openapi.json), as far as the tests read them.
type (
	Project struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
		Phase       string `json:"phase"`
		Reason      string `json:"reason"`
		Message     string `json:"message"`
		Storage     struct {
			Size             string `json:"size"`
			StorageClassName string `json:"storageClassName"`
			CapacityBytes    int64  `json:"capacityBytes"`
			UsedBytes        int64  `json:"usedBytes"`
		} `json:"storage"`
		CreatedAt time.Time `json:"createdAt"`
	}

	Branch struct {
		Name        string       `json:"name"`
		Parent      string       `json:"parent"`
		Phase       string       `json:"phase"`
		State       string       `json:"state"`
		Protected   bool         `json:"protected"`
		BackupOf    string       `json:"backupOf"`
		Reason      string       `json:"reason"`
		Message     string       `json:"message"`
		Endpoint    *Endpoint    `json:"endpoint"`
		LastRestore *LastRestore `json:"lastRestore"`
	}

	Endpoint struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}

	LastRestore struct {
		RequestID      string `json:"requestId"`
		Phase          string `json:"phase"`
		BackupBranch   string `json:"backupBranch"`
		RestorePointID string `json:"restorePointId"`
		Message        string `json:"message"`
	}

	// Connection is GET .../connection. Host is the branch Service's in-cluster DNS name,
	// br-<branch>.cns-<projectId>.svc.
	Connection struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Database string `json:"database"`
		User     string `json:"user"`
		Password string `json:"password"`
		URI      string `json:"uri"`
	}

	RestorePoint struct {
		ID     string `json:"id"`
		Branch string `json:"branch"`
		Reason string `json:"reason"`
		Label  string `json:"label"`
	}

	RestoreResult struct {
		Branch         Branch `json:"branch"`
		RequestID      string `json:"requestId"`
		BackupBranch   string `json:"backupBranch"`
		RestorePointID string `json:"restorePointId"`
	}

	Problem struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
)

// Client calls the API server with the admin key through a port-forward.
type Client struct {
	base string
	key  string
	http *http.Client
}

// API forwards a local port to the API server's Service for the test and returns a client for
// it. Call it before CreateProject: cleanups run last first, and the project's needs the client.
func API(t testing.TB) *Client {
	t.Helper()
	mustLoad(t)
	port := PortForward(t, SystemNamespace, "svc/consonance-apiserver", 8080)
	// Rollbacks and resets wait up to 30 seconds on the server.
	return &Client{base: fmt.Sprintf("http://127.0.0.1:%d", port), key: env.adminKey, http: &http.Client{Timeout: 90 * time.Second}}
}

// Response is an answer of the API server.
type Response struct {
	Status    int
	Header    http.Header
	Body      []byte
	RequestID string // the X-Request-Id the call sent
	Elapsed   time.Duration
}

func (r *Response) String() string {
	return fmt.Sprintf("%d %s (request %s)", r.Status, strings.TrimSpace(string(r.Body)), r.RequestID)
}

// NewRequestID returns a fresh X-Request-Id. Every call sends a new one: the ID is the
// idempotency key of rollbacks and resets (spec 5), so reusing one would replay an answer.
func NewRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "e2e-" + hex.EncodeToString(b)
}

// Call sends a request with a fresh request ID; body, when not nil, is sent as JSON.
func (c *Client) Call(method, path string, body any) (*Response, error) {
	return c.CallWithID(method, path, body, NewRequestID())
}

// CallWithID is Call with the given X-Request-Id.
func (c *Client) CallWithID(method, path string, body any, requestID string) (*Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("X-Request-Id", requestID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: b, RequestID: requestID, Elapsed: time.Since(start)}, nil
}

// Do is Call that fails the test on a transport error.
func (c *Client) Do(t testing.TB, method, path string, body any) *Response {
	t.Helper()
	r, err := c.Call(method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Expect calls the API, fails the test unless the status is one of want, and decodes the body
// into out when out is not nil.
func (c *Client) Expect(t testing.TB, method, path string, body, out any, want ...int) *Response {
	t.Helper()
	r := c.Do(t, method, path, body)
	if !slices.Contains(want, r.Status) {
		t.Fatalf("%s %s: want status %v, got %s", method, path, want, r)
	}
	if out != nil {
		if err := json.Unmarshal(r.Body, out); err != nil {
			// Not the body: a connection's carries the database password.
			t.Fatalf("%s %s: decode %T from a %d answer of %d bytes: %v", method, path, out, r.Status, len(r.Body), err)
		}
	}
	return r
}

// ProjectPath and BranchPath build API paths.
func ProjectPath(id string) string { return "/v1/projects/" + id }

func BranchPath(id, branch string) string { return ProjectPath(id) + "/branches/" + branch }
