// Package agentapi is the wire contract of the volume agent's internal HTTP API (spec 7.2).
package agentapi

import (
	"regexp"
	"time"
)

const (
	// TokenHeader carries the project's internal token, the same one the supervisors use.
	TokenHeader = "X-Consonance-Token"
	// DefaultPort is the agent API port.
	DefaultPort = 8443
	// MainBranch is the project's first branch. Only POST /v1/init-main makes its data.
	MainBranch = "main"
)

var (
	branchNameRE     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	restorePointIDRE = regexp.MustCompile(`^rp-[0-9]{8}T[0-9]{6}Z-[a-z0-9]{4}$`)
)

// ValidBranchName reports whether name is a DNS-1123 label of at most 40 characters (spec 4.3).
func ValidBranchName(name string) bool {
	return len(name) <= 40 && branchNameRE.MatchString(name)
}

// ValidRestorePointID reports whether id has the form rp-<UTC yyyymmddThhmmssZ>-<4 characters>
// (spec 4.4).
func ValidRestorePointID(id string) bool { return restorePointIDRE.MatchString(id) }

// SourceType says what a new branch or generation is copied from.
type SourceType string

const (
	// SourceParentLatest is the parent's running database, copied in backup mode.
	SourceParentLatest SourceType = "ParentLatest"
	// SourceRestorePoint is a restore point: the parent's for a new branch, the branch's own
	// for a rollback.
	SourceRestorePoint SourceType = "RestorePoint"
)

// Source is the origin of a new branch or generation.
type Source struct {
	Type           SourceType `json:"type"`
	RestorePointID string     `json:"restorePointId,omitempty"`
}

// Restore point reasons (spec 4.4).
const (
	ReasonAPI   = "api"   // requested through the API
	ReasonReset = "reset" // the generation a reset replaced, kept automatically
)

// RestorePoint is one restore point (spec 4.4, without the catalog's path).
type RestorePoint struct {
	ID             string    `json:"id"`
	Branch         string    `json:"branch"`
	CreatedAt      time.Time `json:"createdAt"`
	Reason         string    `json:"reason"`
	Label          string    `json:"label"`
	BackupStartLSN string    `json:"backupStartLSN"`
}

// Preflight is returned by GET /v1/preflight.
type Preflight struct {
	Reflink bool   `json:"reflink"`
	Kernel  string `json:"kernel"`
	Error   string `json:"error,omitempty"` // why the reflink probe failed
}

// Usage is returned by GET /v1/usage (statfs of the volume).
type Usage struct {
	CapacityBytes  int64 `json:"capacityBytes"`
	UsedBytes      int64 `json:"usedBytes"`
	AvailableBytes int64 `json:"availableBytes"`
}

// Generation is returned by POST /v1/init-main and POST /v1/branches/{name}/generations.
type Generation struct {
	Generation int `json:"generation"`
}

// CreateBranchDataRequest is the body of POST /v1/branches/{name}/data.
type CreateBranchDataRequest struct {
	Parent string `json:"parent"`
	Source Source `json:"source"`
}

// BranchData is returned by POST /v1/branches/{name}/data.
type BranchData struct {
	Generation     int    `json:"generation"`
	BackupStartLSN string `json:"backupStartLSN"` // empty when the data existed already
}

// CreateRestorePointRequest is the body of POST /v1/branches/{name}/restore-points.
type CreateRestorePointRequest struct {
	Label  string `json:"label"`
	Reason string `json:"reason"` // "api" or empty
}

// RestorePointList is returned by GET /v1/branches/{name}/restore-points, newest first.
type RestorePointList struct {
	RestorePoints []RestorePoint `json:"restorePoints"`
}

// Generations is returned by GET /v1/branches/{name}/generations and by activate.
type Generations struct {
	Current     int   `json:"current"`
	Generations []int `json:"generations"` // complete generations, ascending
}

// CreateGenerationRequest is the body of POST /v1/branches/{name}/generations. Parent is needed
// when Source.Type is ParentLatest (a reset).
type CreateGenerationRequest struct {
	Parent string `json:"parent,omitempty"`
	Source Source `json:"source"`
}

// DetachRequest is the body of POST /v1/branches/{name}/generations/{n}/detach. Exactly one of
// ToBranch (a rollback keeps the old generation as a backup branch) and AsRestorePoint (a reset
// keeps it as a restore point with reason "reset") is set.
type DetachRequest struct {
	ToBranch       string `json:"toBranch,omitempty"`
	AsRestorePoint bool   `json:"asRestorePoint,omitempty"`
}

// DetachResponse says where the generation went.
type DetachResponse struct {
	Branch       string        `json:"branch,omitempty"`
	RestorePoint *RestorePoint `json:"restorePoint,omitempty"`
}

// ErrorCode tells callers what kind of failure a non-2xx response is.
type ErrorCode string

const (
	CodeInvalid               ErrorCode = "invalid"                // 400
	CodeUnauthorized          ErrorCode = "unauthorized"           // 401
	CodeNotFound              ErrorCode = "not-found"              // 404
	CodeConflict              ErrorCode = "conflict"               // 409: exists already, wrong state
	CodeNotRunning            ErrorCode = "not-running"            // 409: the source branch's PostgreSQL is not running
	CodeBusy                  ErrorCode = "busy"                   // 409: another volume operation held the lock too long
	CodeRateLimited           ErrorCode = "rate-limited"           // 429, with Retry-After
	CodeSupervisorUnavailable ErrorCode = "supervisor-unavailable" // 502
	CodeInsufficientStorage   ErrorCode = "insufficient-storage"   // 507
	CodeReflinkUnsupported    ErrorCode = "reflink-unsupported"    // 501: the volume cannot make reflink copies
	CodeInternal              ErrorCode = "internal"               // 500
)

// Error is the body of every non-2xx response.
type Error struct {
	Error string    `json:"error"`
	Code  ErrorCode `json:"code"`
}
