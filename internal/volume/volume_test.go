package volume_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"consonance/internal/testutil/xfstest"
	"consonance/internal/volume"
)

func TestOpenCreatesLayout(t *testing.T) {
	root := t.TempDir()
	openVolume(t, root, nil, nil)
	for _, d := range []string{"branches", "restore", ".consonance", ".consonance/trash"} {
		if fi, err := os.Stat(filepath.Join(root, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
}

func TestOpenChecksItsConfiguration(t *testing.T) {
	sup := supervisors{}.get
	for name, cfg := range map[string]volume.Config{
		"no root":       {Supervisor: sup},
		"no supervisor": {Root: t.TempDir()},
		"missing root":  {Root: filepath.Join(t.TempDir(), "missing"), Supervisor: sup},
	} {
		if _, err := volume.Open(cfg); err == nil {
			t.Errorf("%s: Open succeeded", name)
		}
	}
}

func TestPreflightOnXFS(t *testing.T) {
	v := openVolume(t, xfstest.Dir(t), nil, nil)
	got := v.Preflight()
	if !got.Reflink || got.Error != "" {
		t.Fatalf("Preflight = %+v, want reflink", got)
	}
	if got.Kernel == "" || got.Kernel == "unknown" {
		t.Fatalf("kernel = %q", got.Kernel)
	}
}

func TestPreflightWithoutReflink(t *testing.T) {
	root := t.TempDir() // not XFS
	v := openVolume(t, root, nil, nil)
	got := v.Preflight()
	if got.Reflink || !strings.Contains(got.Error, "not supported") {
		t.Fatalf("Preflight = %+v, want no reflink and the reason", got)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".consonance"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "preflight-") {
			t.Fatalf("the probe left %s", e.Name())
		}
	}
}

func TestUsage(t *testing.T) {
	v := openVolume(t, t.TempDir(), nil, nil)
	u, err := v.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if u.CapacityBytes <= 0 || u.UsedBytes < 0 || u.UsedBytes > u.CapacityBytes || u.AvailableBytes > u.CapacityBytes {
		t.Fatalf("implausible usage %+v", u)
	}
}

func TestLockWaitsThenReportsBusy(t *testing.T) {
	v := openVolume(t, t.TempDir(), nil, func(c *volume.Config) { c.LockWait = 100 * time.Millisecond })
	ctx := testCtx(t)
	release, err := v.AcquireForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := v.AcquireForTest(ctx); !errors.Is(err, volume.ErrBusy) {
		t.Fatalf("second acquire = %v, want ErrBusy", err)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("gave up after %s, before the lock wait ended", waited)
	}
	// A holder that lets go within the wait hands the lock over.
	go func() {
		time.Sleep(20 * time.Millisecond)
		release()
	}()
	release2, err := v.AcquireForTest(ctx)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}
