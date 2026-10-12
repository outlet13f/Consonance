package volume

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

// Supervisor is what the volume needs from a branch's supervisor (spec 7.3). A
// *supervisorclient.Client is one.
type Supervisor interface {
	Status(ctx context.Context) (supervisorapi.Status, error)
	BackupStart(ctx context.Context, label string) (supervisorapi.BackupStartResponse, error)
	BackupStop(ctx context.Context) (supervisorapi.BackupStopResponse, error)
}

// Config configures a Volume. A zero MinCopyInterval, MaxUsedPercent, RestorePointsPerBranch or
// RestorePointMaxAge is no limit; a zero LockWait is no wait: an operation reports busy at once
// when the lock is held. cmd/agent passes the spec 14 defaults.
type Config struct {
	Root                   string                         // volume root, e.g. /volume
	InitDB                 string                         // initdb binary for InitMain
	InitDBArgs             []string                       // more initdb arguments (tests pass --no-sync)
	Supervisor             func(branch string) Supervisor // the supervisor of a branch
	MinCopyInterval        time.Duration                  // between backup-mode copies of one branch (5s)
	LockWait               time.Duration                  // longest wait for the operation lock (2s)
	MaxUsedPercent         float64                        // copies are refused above this volume usage (90)
	RestorePointsPerBranch int                            // restore points kept per branch (20)
	RestorePointMaxAge     time.Duration                  // older restore points are removed (24h)
	Now                    func() time.Time               // clock; nil means time.Now
}

// Volume is one project volume. Operations that change it take the operation lock and so run
// one at a time (spec 7.2); reads do without.
type Volume struct {
	layout
	cfg     Config
	lock    chan struct{} // the operation lock: held while it holds a value
	trashed chan struct{} // tells the janitor that the trash has something

	mu       sync.Mutex
	lastCopy map[string]time.Time // source branch → when its last backup-mode copy ended
}

// Open checks the volume root and creates the top-level directories. Call Recover before
// serving requests.
func Open(cfg Config) (*Volume, error) {
	if cfg.Root == "" {
		return nil, errors.New("volume: Root is required")
	}
	if cfg.Supervisor == nil {
		return nil, errors.New("volume: Supervisor is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	fi, err := os.Stat(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("volume: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("volume: %s is not a directory", cfg.Root)
	}
	v := &Volume{
		layout:   layout{root: cfg.Root},
		cfg:      cfg,
		lock:     make(chan struct{}, 1),
		trashed:  make(chan struct{}, 1),
		lastCopy: map[string]time.Time{},
	}
	for _, dir := range []string{
		filepath.Join(cfg.Root, branchesDir), filepath.Join(cfg.Root, restoreDir),
		filepath.Join(cfg.Root, metaDir), v.trashPath(),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("volume: %w", err)
		}
	}
	return v, nil
}

// acquire takes the operation lock, waiting at most LockWait (spec 7.2: then 409 busy).
func (v *Volume) acquire(ctx context.Context) (release func(), err error) {
	release = func() { <-v.lock }
	select {
	case v.lock <- struct{}{}:
		return release, nil
	default:
	}
	t := time.NewTimer(v.cfg.LockWait)
	defer t.Stop()
	select {
	case v.lock <- struct{}{}:
		return release, nil
	case <-t.C:
		return nil, fmt.Errorf("%w (waited %s)", ErrBusy, v.cfg.LockWait)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// checkCopyInterval refuses a backup-mode copy of source that comes sooner than
// MinCopyInterval after the previous one ended (spec 7.2).
func (v *Volume) checkCopyInterval(source string) error {
	if v.cfg.MinCopyInterval <= 0 {
		return nil
	}
	v.mu.Lock()
	last, ok := v.lastCopy[source]
	v.mu.Unlock()
	if !ok {
		return nil
	}
	if wait := v.cfg.MinCopyInterval - v.cfg.Now().Sub(last); wait > 0 {
		return &RateLimitError{Source: source, RetryAfter: wait}
	}
	return nil
}

// copyEnded records that a backup-mode copy of source ended now.
func (v *Volume) copyEnded(source string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lastCopy[source] = v.cfg.Now()
}

// Usage reports the volume's size and use (statfs).
func (v *Volume) Usage() (agentapi.Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(v.root, &st); err != nil {
		return agentapi.Usage{}, err
	}
	bs := uint64(st.Bsize)
	return agentapi.Usage{
		CapacityBytes:  int64(st.Blocks * bs),
		UsedBytes:      int64((st.Blocks - st.Bfree) * bs),
		AvailableBytes: int64(st.Bavail * bs),
	}, nil
}

// checkSpace refuses a copy while the volume is fuller than MaxUsedPercent (spec 7.2: 507).
func (v *Volume) checkSpace() error {
	if v.cfg.MaxUsedPercent <= 0 {
		return nil
	}
	u, err := v.Usage()
	if err != nil {
		return err
	}
	if u.CapacityBytes == 0 {
		return nil
	}
	if used := float64(u.UsedBytes) * 100 / float64(u.CapacityBytes); used > v.cfg.MaxUsedPercent {
		return fmt.Errorf("%w: %.1f%% used, copies stop above %g%%", ErrInsufficientStorage, used, v.cfg.MaxUsedPercent)
	}
	return nil
}

// errReflinkMismatch is the probe's finding that a reflink copy differs from its source.
var errReflinkMismatch = errors.New("the reflink copy differs from its source")

// Preflight clones a probe file to check that the volume can make reflink copies (spec 7.2
// GET /v1/preflight) and reports the kernel version. Reflink is true when the probe passes. It is
// false, with the reason in Error, only when the volume cannot make reflink copies: the clone is
// refused (ErrReflinkUnsupported) or its copy differs from its source, which is as unusable as no
// reflink. Any other probe failure (EACCES, ENOSPC, EIO...) says nothing about reflink support
// and is returned as an error, wrapped as "reflink probe: ...".
func (v *Volume) Preflight() (agentapi.Preflight, error) {
	res := agentapi.Preflight{Kernel: kernelVersion()}
	err := v.probeReflink()
	switch {
	case err == nil:
		res.Reflink = true
	case errors.Is(err, ErrReflinkUnsupported), errors.Is(err, errReflinkMismatch):
		res.Error = err.Error()
	default:
		return agentapi.Preflight{}, fmt.Errorf("reflink probe: %w", err)
	}
	return res, nil
}

func (v *Volume) probeReflink() error {
	dir, err := os.MkdirTemp(filepath.Join(v.root, metaDir), "preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	want := make([]byte, 64<<10)
	_, _ = rand.Read(want)
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, want, 0o600); err != nil {
		return err
	}
	dst := filepath.Join(dir, "dst")
	if err := cloneFile(src, dst, 0o600); err != nil {
		return err
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errReflinkMismatch
	}
	return nil
}

// kernelVersion returns the running kernel's release, or "unknown" off Linux.
func kernelVersion() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}

// trash moves path into .consonance/trash, from where EmptyTrash deletes it outside the
// operation lock. The move is durable when trash returns. A missing path is not an error.
func (v *Volume) trash(path string) error {
	var b [8]byte
	_, _ = rand.Read(b[:])
	if err := os.Rename(path, filepath.Join(v.trashPath(), fmt.Sprintf("%x", b))); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	// Make the move durable before the caller reports success: after a crash, a deleted branch
	// must not come back and hand its old data to a new branch of the same name.
	err := errors.Join(syncDir(filepath.Dir(path)), syncDir(v.trashPath()))
	select {
	case v.trashed <- struct{}{}:
	default:
	}
	return err
}

// trashOrWarn trashes path and only logs a failure: the caller has a more important error to
// report, and Recover removes what is left at the next start.
func (v *Volume) trashOrWarn(path string) {
	if err := v.trash(path); err != nil {
		slog.Warn("remove directory", "path", v.rel(path), "err", err)
	}
}

// EmptyTrash deletes everything in the trash.
func (v *Volume) EmptyTrash() error {
	entries, err := os.ReadDir(v.trashPath())
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(v.trashPath(), e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
