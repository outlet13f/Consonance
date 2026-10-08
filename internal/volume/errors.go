package volume

import (
	"errors"
	"fmt"
	"time"
)

// Errors of Volume operations. The agent API maps each to a status code and an error code.
var (
	ErrInvalid               = errors.New("invalid request")
	ErrNotFound              = errors.New("not found")
	ErrConflict              = errors.New("conflict")
	ErrNotRunning            = errors.New("postgres is not running")
	ErrBusy                  = errors.New("another volume operation is in progress")
	ErrInsufficientStorage   = errors.New("volume usage is above the copy limit")
	ErrSupervisorUnavailable = errors.New("branch supervisor unavailable")
	ErrReflinkUnsupported    = errors.New("reflink copies are not supported here")
)

// RateLimitError is returned when a backup-mode copy of a branch comes sooner than
// MinCopyInterval after the previous one ended (spec 7.2).
type RateLimitError struct {
	Source     string        // the branch copied from
	RetryAfter time.Duration // how long until the next copy is allowed
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("copies of branch %q are limited: retry in %s", e.Source, e.RetryAfter.Round(time.Millisecond))
}
