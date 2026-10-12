package volume_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
	"consonance/internal/volume"
)

// fakeSupervisor answers like a branch supervisor without a PostgreSQL behind it. Like the real
// client, each call fails with the context's error when the context it gets is already done; such
// a call never reaches the supervisor, so it is not counted and changes nothing.
type fakeSupervisor struct {
	mu           sync.Mutex
	statuses     []supervisorapi.Status // Status returns these in order, then repeats the last one
	startErr     error                  // BackupStart fails with it when set, without opening a backup
	startErrOpen error                  // BackupStart opens the backup and then fails with it, as a call that timed out after the supervisor began the backup
	stopErr      error                  // the next BackupStop fails with it, once and without closing the backup, as a call whose answer never arrived
	open         bool                   // a backup is open: BackupStart opens it, BackupStop closes it
	statusCalls  int                    // Status calls
	starts       int                    // BackupStart calls
	stops        int                    // BackupStop calls
	onStatus     func(call int)         // called inside Status once it has chosen its answer, with the call's number (1, 2, ...); runs under mu, so it must not call the fake
	onStop       func()                 // called inside BackupStop when set; runs under mu, so it must not call the fake
}

func (f *fakeSupervisor) Status(ctx context.Context) (supervisorapi.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return supervisorapi.Status{}, err
	}
	f.statusCalls++
	st := supervisorapi.Status{PG: supervisorapi.PGStopped}
	if len(f.statuses) > 0 {
		st = f.statuses[0]
		if len(f.statuses) > 1 {
			f.statuses = f.statuses[1:]
		}
	}
	if f.onStatus != nil {
		f.onStatus(f.statusCalls)
	}
	return st, nil
}

func (f *fakeSupervisor) BackupStart(ctx context.Context, _ string) (supervisorapi.BackupStartResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return supervisorapi.BackupStartResponse{}, err
	}
	f.starts++
	if f.startErr != nil {
		return supervisorapi.BackupStartResponse{}, f.startErr
	}
	if f.open { // like the real supervisor, which runs one backup at a time
		return supervisorapi.BackupStartResponse{}, &supervisorclient.APIError{StatusCode: http.StatusConflict, Message: "a backup is already in progress"}
	}
	f.open = true
	if f.startErrOpen != nil {
		return supervisorapi.BackupStartResponse{}, f.startErrOpen
	}
	return supervisorapi.BackupStartResponse{StartLSN: "0/2000028"}, nil
}

func (f *fakeSupervisor) BackupStop(ctx context.Context) (supervisorapi.BackupStopResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return supervisorapi.BackupStopResponse{}, err
	}
	f.stops++
	if f.onStop != nil {
		f.onStop()
	}
	if err := f.stopErr; err != nil {
		f.stopErr = nil
		return supervisorapi.BackupStopResponse{}, err
	}
	if !f.open { // the real supervisor's answer to a stop without a backup
		return supervisorapi.BackupStopResponse{}, &supervisorclient.APIError{StatusCode: http.StatusConflict, Message: "no backup in progress"}
	}
	f.open = false
	return supervisorapi.BackupStopResponse{
		LabelFile: "START WAL LOCATION: 0/2000028 (file 000000010000000000000002)\n",
		StopLSN:   "0/2000100",
	}, nil
}

// set replaces the statuses Status returns.
func (f *fakeSupervisor) set(statuses ...supervisorapi.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = statuses
}

// calls returns how often BackupStart and BackupStop reached the supervisor.
func (f *fakeSupervisor) calls() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

// backupOpen reports whether a backup is open on the supervisor.
func (f *fakeSupervisor) backupOpen() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open
}

func running(gen string) supervisorapi.Status {
	return supervisorapi.Status{PG: supervisorapi.PGRunning, Generation: gen}
}

func stopped(gen string) supervisorapi.Status {
	return supervisorapi.Status{PG: supervisorapi.PGStopped, Generation: gen}
}

// errNoAnswer is how a call to the supervisor of a branch without a pod fails.
var errNoAnswer = errors.New("dial tcp 10.42.0.9:8008: connect: connection refused")

// unreachable is the supervisor of a branch without a pod.
type unreachable struct{}

func (unreachable) Status(context.Context) (supervisorapi.Status, error) {
	return supervisorapi.Status{}, errNoAnswer
}

func (unreachable) BackupStart(context.Context, string) (supervisorapi.BackupStartResponse, error) {
	return supervisorapi.BackupStartResponse{}, errNoAnswer
}

func (unreachable) BackupStop(context.Context) (supervisorapi.BackupStopResponse, error) {
	return supervisorapi.BackupStopResponse{}, errNoAnswer
}

// supervisors maps branches to fake supervisors. Other branches have no pod.
type supervisors map[string]*fakeSupervisor

func (s supervisors) get(branch string) volume.Supervisor {
	if f, ok := s[branch]; ok {
		return f
	}
	return unreachable{}
}

// testClock is a settable clock for Config.Now.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 7, 3, 15, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// openVolume opens the volume at root with fake supervisors, a 50ms lock wait and no other
// limits; mod changes the configuration first.
func openVolume(t *testing.T, root string, sups supervisors, mod func(*volume.Config)) *volume.Volume {
	t.Helper()
	cfg := volume.Config{Root: root, Supervisor: sups.get, LockWait: 50 * time.Millisecond}
	if mod != nil {
		mod(&cfg)
	}
	v, err := volume.Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return v
}

// mkGeneration makes branches/<branch>/data-<n> holding PG_VERSION and, when current is true,
// points the branch's current link at it. It returns the generation directory.
func mkGeneration(t *testing.T, root, branch string, n int, current bool) string {
	t.Helper()
	dir := filepath.Join(root, "branches", branch, fmt.Sprintf("data-%d", n))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if current {
		link := filepath.Join(root, "branches", branch, "current")
		_ = os.Remove(link)
		if err := os.Symlink(fmt.Sprintf("data-%d", n), link); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// rpFixture is a restore point for writeCatalog.
type rpFixture struct {
	id, branch, reason string
	createdAt          time.Time
}

// writeCatalog writes .consonance/catalog.json in the spec 4.4 format and makes each restore
// point's directory with a PG_VERSION file.
func writeCatalog(t *testing.T, root string, rps ...rpFixture) {
	t.Helper()
	type entry struct {
		ID             string    `json:"id"`
		Branch         string    `json:"branch"`
		CreatedAt      time.Time `json:"createdAt"`
		Reason         string    `json:"reason"`
		Label          string    `json:"label"`
		BackupStartLSN string    `json:"backupStartLSN"`
		Path           string    `json:"path"`
	}
	doc := struct {
		Version       int     `json:"version"`
		RestorePoints []entry `json:"restorePoints"`
	}{Version: 1, RestorePoints: []entry{}}
	for _, rp := range rps {
		path := "restore/" + rp.branch + "/" + rp.id
		doc.RestorePoints = append(doc.RestorePoints, entry{
			ID: rp.id, Branch: rp.branch, CreatedAt: rp.createdAt, Reason: rp.reason,
			Label: "fixture", BackupStartLSN: "0/1000028", Path: path,
		})
		dir := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".consonance"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".consonance", "catalog.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// exists reports whether path exists, without following a final symlink.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}
