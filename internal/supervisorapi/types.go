// Package supervisorapi is the wire contract of the branch supervisor's internal HTTP API.
package supervisorapi

import "time"

const (
	// TokenHeader carries the per-project internal token.
	TokenHeader = "X-Consonance-Token"
	// DefaultPort is the supervisor API port.
	DefaultPort = 8008
)

// PGState is the PostgreSQL process state as reported by the supervisor.
type PGState string

const (
	PGStopped  PGState = "stopped"
	PGStarting PGState = "starting"
	PGRunning  PGState = "running"
	PGStopping PGState = "stopping"
)

// Status is returned by GET /v1/status and by the start, stop and bootstrap calls.
type Status struct {
	PG         PGState   `json:"pg"`
	Generation string    `json:"generation"` // target of /pgdata/current, e.g. "data-3"
	Since      time.Time `json:"since"`
}

// BackupStartRequest is the body of POST /v1/backup/start.
type BackupStartRequest struct {
	Label string `json:"label"`
}

// BackupStartResponse is returned by POST /v1/backup/start.
type BackupStartResponse struct {
	StartLSN string `json:"startLSN"`
}

// BackupStopResponse is returned by POST /v1/backup/stop.
type BackupStopResponse struct {
	LabelFile string `json:"labelFile"`
	StopLSN   string `json:"stopLSN"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}
