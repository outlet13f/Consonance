package volume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
)

const (
	statusTimeout = 5 * time.Second  // one status call
	backupTimeout = 30 * time.Second // backup start (a checkpoint) or backup stop
)

// skipLive leaves out of a running data directory's copy what must not come along (spec 7.2):
// pg_wal is copied after the backup ends, and the postmaster files belong to the running server.
func skipLive(rel string) bool {
	switch rel {
	case "pg_wal", "postmaster.pid", "postmaster.opts", incompleteMarker:
		return true
	}
	return false
}

// skipMarker leaves out only the .incomplete marker (copies of restore points).
func skipMarker(rel string) bool { return rel == incompleteMarker }

// supervisorErr wraps a failed supervisor call. A call that got no answer is
// ErrSupervisorUnavailable; an answered one keeps the supervisor's error.
func supervisorErr(branch, call string, err error) error {
	var apiErr *supervisorclient.APIError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("supervisor of %q: %s: %w", branch, call, err)
	}
	return fmt.Errorf("%w: %s of branch %q: %v", ErrSupervisorUnavailable, call, branch, err)
}

// status asks a branch's supervisor for its PostgreSQL state.
func (v *Volume) status(ctx context.Context, branch string) (supervisorapi.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	st, err := v.cfg.Supervisor(branch).Status(ctx)
	if err != nil {
		return st, supervisorErr(branch, "status", err)
	}
	return st, nil
}

var startSegmentRE = regexp.MustCompile(`START WAL LOCATION: \S+ \(file ([0-9A-F]{24})\)`)

// startSegment returns the WAL segment named on a backup label's START WAL LOCATION line.
func startSegment(labelFile string) string {
	if m := startSegmentRE.FindStringSubmatch(labelFile); m != nil {
		return m[1]
	}
	return ""
}

// snapshot copies the running database of branch source into dst, an existing marked
// directory, so that the copy starts on its own as a consistent server (spec 7.2, PoC): backup
// start, clone all but pg_wal and the postmaster files, backup stop, write backup_label, clone
// pg_wal. It copies the generation the supervisor reports running, which is not always the one
// current points at. It returns the backup's start LSN.
func (v *Volume) snapshot(ctx context.Context, source, dst, label string) (string, error) {
	sup := v.cfg.Supervisor(source)
	pre, err := v.status(ctx, source)
	if err != nil {
		return "", err
	}
	if pre.PG != supervisorapi.PGRunning {
		return "", fmt.Errorf("%w: branch %q is %s", ErrNotRunning, source, pre.PG)
	}

	// From here on the source pays for a checkpoint, so the copy counts for MinCopyInterval
	// even when it fails.
	defer v.copyEnded(source)
	startCtx, cancel := context.WithTimeout(ctx, backupTimeout)
	start, err := sup.BackupStart(startCtx, label)
	cancel()
	if err != nil {
		if supervisorclient.IsConflict(err) { // a backup is still open, e.g. from a copy cut short
			return "", fmt.Errorf("%w: backup start on branch %q: %v", ErrBusy, source, err)
		}
		return "", supervisorErr(source, "backup start", err)
	}
	ended := false
	defer func() {
		if !ended { // end the backup now rather than when the supervisor's 30s timeout fires
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusTimeout)
			defer cancel()
			_, _ = sup.BackupStop(stopCtx)
		}
	}()

	// The backup runs on the server that answered. If that server was restarted between the two
	// status calls, it may run another generation than the one copied below: refuse. A restart
	// after this point closes the backup session, and backup stop fails.
	post, err := v.status(ctx, source)
	if err != nil {
		return "", err
	}
	gen, ok := parseGeneration(post.Generation)
	if post.PG != supervisorapi.PGRunning || post.Generation != pre.Generation || !ok {
		return "", fmt.Errorf("%w: branch %q restarted during the copy (%s %q, then %s %q)",
			ErrConflict, source, pre.PG, pre.Generation, post.PG, post.Generation)
	}
	srcDir := v.genDir(source, gen)
	if err := CloneTree(srcDir, dst, skipLive); err != nil {
		return "", err
	}

	ended = true
	stopCtx, cancel := context.WithTimeout(ctx, backupTimeout)
	stop, err := sup.BackupStop(stopCtx)
	cancel()
	if err != nil {
		return "", supervisorErr(source, "backup stop", err)
	}
	if err := writeFileSync(filepath.Join(dst, "backup_label"), []byte(stop.LabelFile), 0o600); err != nil {
		return "", err
	}
	walDst := filepath.Join(dst, "pg_wal")
	if err := os.Mkdir(walDst, 0o700); err != nil {
		return "", err
	}
	if err := CloneTree(filepath.Join(srcDir, "pg_wal"), walDst, nil); err != nil {
		return "", err
	}
	// Recovery replays WAL from the backup's start. A checkpoint right after the backup started
	// could have recycled that segment before pg_wal was cloned, and then the copy is useless.
	seg := startSegment(stop.LabelFile)
	if seg == "" {
		return "", fmt.Errorf("the backup label of branch %q names no start segment", source)
	}
	if _, err := os.Lstat(filepath.Join(walDst, seg)); err != nil {
		return "", fmt.Errorf("the copy of branch %q lacks WAL segment %s: %w", source, seg, err)
	}
	return start.StartLSN, nil
}
