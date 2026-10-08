package volume_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
	"consonance/internal/volume"
)

func parentLatest(parent string) agentapi.CreateBranchDataRequest {
	return agentapi.CreateBranchDataRequest{Parent: parent, Source: agentapi.Source{Type: agentapi.SourceParentLatest}}
}

func TestCreateBranchFromRunningParent(t *testing.T) {
	e := newPGEnv(t, nil)
	ctx := testCtx(t)
	e.exec(t, "main", "CREATE TABLE t (id int); INSERT INTO t SELECT generate_series(1, 1000)")

	got, err := e.vol.CreateBranchData(ctx, "dev", parentLatest("main"))
	if err != nil {
		t.Fatalf("CreateBranchData: %v", err)
	}
	if got.Generation != 1 || got.BackupStartLSN == "" {
		t.Fatalf("got %+v", got)
	}
	data := filepath.Join(e.root, "branches", "dev", "data-1")
	if target, err := os.Readlink(filepath.Join(e.root, "branches", "dev", "current")); err != nil || target != "data-1" {
		t.Fatalf("dev/current = %q, %v", target, err)
	}
	if label := readFile(t, filepath.Join(data, "backup_label")); !strings.Contains(label, "LABEL: consonance branch dev") {
		t.Fatalf("backup_label:\n%s", label)
	}
	for _, name := range []string{"postmaster.pid", "postmaster.opts", ".incomplete"} {
		if exists(filepath.Join(data, name)) {
			t.Errorf("the copy has %s", name)
		}
	}
	if wal, err := os.ReadDir(filepath.Join(data, "pg_wal")); err != nil || len(wal) == 0 {
		t.Fatalf("pg_wal: %v, %v", wal, err)
	}

	e.exec(t, "main", "INSERT INTO t VALUES (1001)") // after the copy: must not reach dev
	e.start(t, "dev")
	if n := e.count(t, "dev"); n != 1000 {
		t.Fatalf("dev has %d rows, want 1000", n)
	}
	e.exec(t, "dev", "INSERT INTO t SELECT generate_series(2001, 2100)")
	if n := e.count(t, "main"); n != 1001 {
		t.Fatalf("main has %d rows, want 1001", n)
	}

	// A retried call finds the data and leaves it alone.
	again, err := e.vol.CreateBranchData(ctx, "dev", parentLatest("main"))
	if err != nil || again.Generation != 1 || again.BackupStartLSN != "" {
		t.Fatalf("retried CreateBranchData = %+v, %v", again, err)
	}
	if n := e.count(t, "dev"); n != 1100 {
		t.Fatalf("dev has %d rows after the retry, want 1100", n)
	}
}

func TestCreateBranchDataValidates(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{running("data-1")}}}, nil)
	ctx := testCtx(t)
	for _, tc := range []struct {
		name string
		req  agentapi.CreateBranchDataRequest
	}{
		{"main", parentLatest("main")}, // main's data comes only from init-main
		{"Dev", parentLatest("main")},
		{"../dev", parentLatest("main")},
		{"dev", parentLatest("")},
		{"dev", parentLatest("dev")},
		{"dev", agentapi.CreateBranchDataRequest{Parent: "main", Source: agentapi.Source{Type: "Snapshot"}}},
	} {
		if _, err := v.CreateBranchData(ctx, tc.name, tc.req); !errors.Is(err, volume.ErrInvalid) {
			t.Errorf("CreateBranchData(%q, %+v) = %v, want ErrInvalid", tc.name, tc.req, err)
		}
	}
}

func TestCreateBranchDataNeedsParentData(t *testing.T) {
	v := openVolume(t, t.TempDir(), nil, nil)
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestCreateBranchDataKeepsABranchItCannotRead(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	// dev/current is a real directory, not a link: dev's state cannot be judged.
	writeFile(t, filepath.Join(root, "branches", "dev", "current", "keep"), "x", 0o600)
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{running("data-1")}}}, nil)
	_, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main"))
	if err == nil || errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("CreateBranchData = %v, want an error that is not ErrNotFound", err)
	}
	if !exists(filepath.Join(root, "branches", "dev", "current", "keep")) {
		t.Fatal("CreateBranchData replaced a branch it could not read")
	}
}

func TestCreateBranchDataNeedsRunningParent(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	main := &fakeSupervisor{statuses: []supervisorapi.Status{stopped("data-1")}}
	v := openVolume(t, root, supervisors{"main": main}, nil)
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrNotRunning) {
		t.Fatalf("got %v, want ErrNotRunning", err)
	}
	if starts, _ := main.calls(); starts != 0 {
		t.Fatal("a backup was started on a stopped parent")
	}
	if exists(filepath.Join(root, "branches", "dev")) {
		t.Fatal("branches/dev was left behind")
	}
}

func TestCreateBranchDataWithoutSupervisorAnswer(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	v := openVolume(t, root, nil, nil) // main has no pod
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrSupervisorUnavailable) {
		t.Fatalf("got %v, want ErrSupervisorUnavailable", err)
	}
}

func TestCopyRefusesParentThatRestarted(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	mkGeneration(t, root, "main", 2, false)
	// The backup lands on a server that was restarted on data-2 after the first status call.
	main := &fakeSupervisor{statuses: []supervisorapi.Status{running("data-1"), running("data-2")}}
	v := openVolume(t, root, supervisors{"main": main}, nil)
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	if starts, stops := main.calls(); starts != 1 || stops != 1 {
		t.Fatalf("backup start/stop calls = %d/%d, want 1/1", starts, stops)
	}
	if exists(filepath.Join(root, "branches", "dev")) {
		t.Fatal("a partial branch was left behind")
	}
}

func TestFailedCopyEndsBackupAndCleansUp(t *testing.T) {
	// t.TempDir is not XFS, so the clone fails after the backup has started.
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	main := &fakeSupervisor{statuses: []supervisorapi.Status{running("data-1")}}
	clock := newClock()
	v := openVolume(t, root, supervisors{"main": main}, func(c *volume.Config) {
		c.MinCopyInterval = time.Minute
		c.Now = clock.Now
	})
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrReflinkUnsupported) {
		t.Fatalf("got %v, want ErrReflinkUnsupported", err)
	}
	if starts, stops := main.calls(); starts != 1 || stops != 1 {
		t.Fatalf("backup start/stop calls = %d/%d, want 1/1 (the backup ends at once)", starts, stops)
	}
	if exists(filepath.Join(root, "branches", "dev")) {
		t.Fatal("a partial branch was left behind")
	}
	// The failed copy still cost main a checkpoint, so it counts for the interval.
	var rl *volume.RateLimitError
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.As(err, &rl) || rl.RetryAfter != time.Minute {
		t.Fatalf("second copy = %v, want a rate limit of 1m", err)
	}
}

func TestCopyIntervalCountsFromTheEndOfTheLastCopy(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	main := &fakeSupervisor{statuses: []supervisorapi.Status{stopped("data-1")}}
	clock := newClock()
	v := openVolume(t, root, supervisors{"main": main}, func(c *volume.Config) {
		c.MinCopyInterval = 5 * time.Second
		c.Now = clock.Now
	})
	v.CopyEndedForTest("main")
	clock.Advance(4 * time.Second)
	var rl *volume.RateLimitError
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.As(err, &rl) || rl.RetryAfter != time.Second || rl.Source != "main" {
		t.Fatalf("got %v, want main limited for 1s", err)
	}
	clock.Advance(time.Second)
	// The interval is over: the copy goes ahead and stops at the stopped parent.
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrNotRunning) {
		t.Fatalf("got %v, want ErrNotRunning", err)
	}
}

func TestCreateBranchDataRefusesFullVolume(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	main := &fakeSupervisor{statuses: []supervisorapi.Status{running("data-1")}}
	// Any filesystem in use is above a 0.0001% limit.
	v := openVolume(t, root, supervisors{"main": main}, func(c *volume.Config) { c.MaxUsedPercent = 0.0001 })
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrInsufficientStorage) {
		t.Fatalf("got %v, want ErrInsufficientStorage", err)
	}
	if starts, _ := main.calls(); starts != 0 {
		t.Fatal("a backup was started on a full volume")
	}
}

func TestCreateBranchDataWaitsForTheLock(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	v := openVolume(t, root, nil, nil)
	release, err := v.AcquireForTest(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
}

func TestOpenBackupOnParentIsBusy(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	main := &fakeSupervisor{
		statuses: []supervisorapi.Status{running("data-1")},
		startErr: &supervisorclient.APIError{StatusCode: 409, Message: "a backup is already in progress"},
	}
	v := openVolume(t, root, supervisors{"main": main}, nil)
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
	if exists(filepath.Join(root, "branches", "dev")) {
		t.Fatal("branches/dev was left behind")
	}
}
