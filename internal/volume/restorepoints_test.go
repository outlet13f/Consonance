package volume_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
	"consonance/internal/volume"
)

func fromRestorePoint(parent, id string) agentapi.CreateBranchDataRequest {
	return agentapi.CreateBranchDataRequest{
		Parent: parent,
		Source: agentapi.Source{Type: agentapi.SourceRestorePoint, RestorePointID: id},
	}
}

func TestRestorePointsAndBranchesFromThem(t *testing.T) {
	e := newPGEnv(t, func(c *volume.Config) { c.MinCopyInterval = time.Hour })
	ctx := testCtx(t)
	e.exec(t, "main", "CREATE TABLE t (id int); INSERT INTO t SELECT generate_series(1, 1000)")

	rp, err := e.vol.CreateRestorePoint(ctx, "main", agentapi.CreateRestorePointRequest{Label: "before cleanup"})
	if err != nil {
		t.Fatalf("CreateRestorePoint: %v", err)
	}
	if !agentapi.ValidRestorePointID(rp.ID) || rp.Branch != "main" || rp.Reason != agentapi.ReasonAPI ||
		rp.Label != "before cleanup" || rp.BackupStartLSN == "" {
		t.Fatalf("restore point %+v", rp)
	}
	dir := filepath.Join(e.root, "restore", "main", rp.ID)
	if !exists(filepath.Join(dir, "backup_label")) || exists(filepath.Join(dir, ".incomplete")) {
		t.Fatal("the restore point is not a finished backup-mode copy")
	}
	if list, err := e.vol.ListRestorePoints("main"); err != nil || len(list) != 1 || list[0].ID != rp.ID {
		t.Fatalf("ListRestorePoints = %+v, %v", list, err)
	}

	e.exec(t, "main", "DELETE FROM t")
	// Copies of main are limited for an hour now; a restore point does not touch main's server.
	got, err := e.vol.CreateBranchData(ctx, "restored", fromRestorePoint("main", rp.ID))
	if err != nil {
		t.Fatalf("branch from the restore point: %v", err)
	}
	if got.Generation != 1 || got.BackupStartLSN != rp.BackupStartLSN {
		t.Fatalf("got %+v", got)
	}
	e.start(t, "restored")
	if n := e.count(t, "restored"); n != 1000 {
		t.Fatalf("restored has %d rows, want 1000", n)
	}
	var rl *volume.RateLimitError
	if _, err := e.vol.CreateRestorePoint(ctx, "main", agentapi.CreateRestorePointRequest{}); !errors.As(err, &rl) {
		t.Fatalf("second restore point within the interval = %v, want a rate limit", err)
	}

	if err := e.vol.DeleteRestorePoint(ctx, "main", rp.ID); err != nil {
		t.Fatalf("DeleteRestorePoint: %v", err)
	}
	if exists(dir) {
		t.Fatal("the restore point's directory is still there")
	}
	if list, err := e.vol.ListRestorePoints("main"); err != nil || len(list) != 0 {
		t.Fatalf("after delete: %+v, %v", list, err)
	}
	if err := e.vol.DeleteRestorePoint(ctx, "main", rp.ID); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestCreateRestorePointValidates(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{running("data-1")}}}, nil)
	ctx := testCtx(t)
	for _, tc := range []struct {
		branch string
		req    agentapi.CreateRestorePointRequest
		want   error
	}{
		{"main", agentapi.CreateRestorePointRequest{Reason: agentapi.ReasonReset}, volume.ErrInvalid},
		{"main", agentapi.CreateRestorePointRequest{Label: strings.Repeat("x", 257)}, volume.ErrInvalid},
		{"Main", agentapi.CreateRestorePointRequest{}, volume.ErrInvalid},
		{"dev", agentapi.CreateRestorePointRequest{}, volume.ErrNotFound},
	} {
		if _, err := v.CreateRestorePoint(ctx, tc.branch, tc.req); !errors.Is(err, tc.want) {
			t.Errorf("CreateRestorePoint(%q, %+v) = %v, want %v", tc.branch, tc.req, err, tc.want)
		}
	}
}

func TestCreateRestorePointNeedsRunningBranch(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{stopped("data-1")}}}, nil)
	if _, err := v.CreateRestorePoint(testCtx(t), "main", agentapi.CreateRestorePointRequest{}); !errors.Is(err, volume.ErrNotRunning) {
		t.Fatalf("got %v, want ErrNotRunning", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "restore", "main")); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
}

func TestCreateRestorePointStopsOnADirectoryItCannotCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	rdir := filepath.Join(root, "restore", "main")
	if err := os.MkdirAll(rdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rdir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(rdir, 0o700) })
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{running("data-1")}}}, nil)
	ctx := testCtx(t)
	done := make(chan error, 1)
	go func() {
		_, err := v.CreateRestorePoint(ctx, "main", agentapi.CreateRestorePointRequest{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("CreateRestorePoint succeeded on a directory it cannot check")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CreateRestorePoint did not return")
	}
	release, err := v.AcquireForTest(ctx) // the operation lock is free again
	if err != nil {
		t.Fatalf("lock after the failure: %v", err)
	}
	release()
}

func TestListRestorePointsNewestFirst(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	t0 := newClock().Now()
	writeCatalog(t, root,
		rpFixture{id: "rp-20261007T031500Z-aaaa", branch: "main", reason: "api", createdAt: t0},
		rpFixture{id: "rp-20261007T041500Z-bbbb", branch: "main", reason: "reset", createdAt: t0.Add(time.Hour)},
		rpFixture{id: "rp-20261007T051500Z-cccc", branch: "dev", reason: "api", createdAt: t0.Add(2 * time.Hour)},
	)
	v := openVolume(t, root, nil, nil)
	list, err := v.ListRestorePoints("main")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, rp := range list {
		ids = append(ids, rp.ID)
	}
	if !slices.Equal(ids, []string{"rp-20261007T041500Z-bbbb", "rp-20261007T031500Z-aaaa"}) {
		t.Fatalf("ids = %v", ids)
	}
	if list[0].Reason != agentapi.ReasonReset || !list[0].CreatedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("newest = %+v", list[0])
	}
	if _, err := v.ListRestorePoints("gone"); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("unknown branch: %v, want ErrNotFound", err)
	}
}

func TestApplyRetention(t *testing.T) {
	root := t.TempDir()
	clock := newClock()
	now := clock.Now()
	writeCatalog(t, root,
		rpFixture{id: "rp-20261007T031400Z-new1", branch: "main", reason: "api", createdAt: now.Add(-time.Minute)},
		rpFixture{id: "rp-20261007T031300Z-new2", branch: "main", reason: "api", createdAt: now.Add(-2 * time.Minute)},
		rpFixture{id: "rp-20261007T031200Z-new3", branch: "main", reason: "reset", createdAt: now.Add(-3 * time.Minute)},
		rpFixture{id: "rp-20261006T021500Z-old1", branch: "dev", reason: "api", createdAt: now.Add(-25 * time.Hour)},
		rpFixture{id: "rp-20261007T020000Z-dev2", branch: "dev", reason: "api", createdAt: now.Add(-75 * time.Minute)},
	)
	v := openVolume(t, root, nil, func(c *volume.Config) {
		c.Now = clock.Now
		c.RestorePointsPerBranch = 2
		c.RestorePointMaxAge = 24 * time.Hour
	})
	if err := v.ApplyRetention(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"restore/main/rp-20261007T031200Z-new3", "restore/dev/rp-20261006T021500Z-old1"} {
		if exists(filepath.Join(root, p)) {
			t.Errorf("%s survived the retention", p)
		}
	}
	for _, p := range []string{"restore/main/rp-20261007T031400Z-new1", "restore/main/rp-20261007T031300Z-new2", "restore/dev/rp-20261007T020000Z-dev2"} {
		if !exists(filepath.Join(root, p)) {
			t.Errorf("%s was removed", p)
		}
	}
	if cat := readFile(t, filepath.Join(root, ".consonance", "catalog.json")); strings.Contains(cat, "new3") || strings.Contains(cat, "old1") {
		t.Errorf("the catalog still lists removed restore points:\n%s", cat)
	}
}

func TestCreateBranchFromRestorePointIsNotRateLimited(t *testing.T) {
	// No XFS here: the clone fails, which shows that the copy got past the interval check.
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	writeCatalog(t, root, rpFixture{id: "rp-20261007T031500Z-ab12", branch: "main", reason: "api", createdAt: newClock().Now()})
	v := openVolume(t, root, nil, func(c *volume.Config) { c.MinCopyInterval = time.Hour })
	v.CopyEndedForTest("main")
	_, err := v.CreateBranchData(testCtx(t), "dev", fromRestorePoint("main", "rp-20261007T031500Z-ab12"))
	if !errors.Is(err, volume.ErrReflinkUnsupported) {
		t.Fatalf("got %v, want the clone to be tried (ErrReflinkUnsupported off XFS)", err)
	}
	if exists(filepath.Join(root, "branches", "dev")) {
		t.Fatal("a partial branch was left behind")
	}
}

func TestCreateBranchFromUnknownRestorePoint(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	mkGeneration(t, root, "dev", 1, true)
	writeCatalog(t, root, rpFixture{id: "rp-20261007T031500Z-ab12", branch: "dev", reason: "api", createdAt: newClock().Now()})
	v := openVolume(t, root, nil, nil)
	for _, id := range []string{"rp-20261007T031500Z-zzzz", "rp-20261007T031500Z-ab12"} { // unknown; dev's, not main's
		if _, err := v.CreateBranchData(testCtx(t), "x", fromRestorePoint("main", id)); !errors.Is(err, volume.ErrNotFound) {
			t.Errorf("restore point %s: %v, want ErrNotFound", id, err)
		}
	}
}

func TestRunJanitorEmptiesTheTrash(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	writeCatalog(t, root, rpFixture{id: "rp-20261007T031500Z-ab12", branch: "main", reason: "api", createdAt: newClock().Now()})
	v := openVolume(t, root, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.RunJanitor(ctx, time.Hour)
	}()
	defer func() {
		cancel()
		<-done
	}()
	if err := v.DeleteRestorePoint(testCtx(t), "main", "rp-20261007T031500Z-ab12"); err != nil {
		t.Fatal(err)
	}
	trash := filepath.Join(root, ".consonance", "trash")
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(trash)
		if err == nil && len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the janitor did not empty the trash: %v, %v", entries, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
