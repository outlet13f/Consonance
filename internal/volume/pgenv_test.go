package volume_test

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"consonance/internal/pgctl"
	"consonance/internal/supervisor"
	"consonance/internal/supervisorclient"
	"consonance/internal/testutil/pgtest"
	"consonance/internal/testutil/xfstest"
	"consonance/internal/volume"
)

const testToken = "test-token"

// pgEnv is a volume on XFS whose branches run real PostgreSQL servers behind real supervisor
// APIs, as branch pods do.
type pgEnv struct {
	root string
	bin  string
	vol  *volume.Volume

	mu   sync.Mutex
	urls map[string]string         // branch → supervisor API URL
	pgs  map[string]*pgctl.Manager // branch → its PostgreSQL
}

// newPGEnv makes main with initdb, starts it and opens the volume. mod changes the volume
// configuration (newPGEnv sets Root and Supervisor; LockWait is 2s unless mod changes it).
func newPGEnv(t *testing.T, mod func(*volume.Config)) *pgEnv {
	t.Helper()
	e := &pgEnv{root: xfstest.Dir(t), bin: pgtest.BinDir(t), urls: map[string]string{}, pgs: map[string]*pgctl.Manager{}}
	mainDir := filepath.Join(e.root, "branches", "main")
	if err := os.MkdirAll(mainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pgtest.InitDBAt(t, e.bin, filepath.Join(mainDir, "data-1"))
	if err := os.Symlink("data-1", filepath.Join(mainDir, "current")); err != nil {
		t.Fatal(err)
	}
	cfg := volume.Config{LockWait: 2 * time.Second}
	if mod != nil {
		mod(&cfg)
	}
	cfg.Root, cfg.Supervisor = e.root, e.supervisor
	v, err := volume.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.vol = v
	e.start(t, "main")
	return e
}

func (e *pgEnv) supervisor(branch string) volume.Supervisor { return e.client(branch) }

// client returns a client of the branch's supervisor API. A branch that runs no PostgreSQL
// gets an address where nothing listens, like a branch without a pod.
func (e *pgEnv) client(branch string) *supervisorclient.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	url, ok := e.urls[branch]
	if !ok {
		url = "http://127.0.0.1:1"
	}
	return supervisorclient.New(url, testToken, nil)
}

// start runs PostgreSQL on branches/<branch>/current with the supervisor API in front of it.
func (e *pgEnv) start(t *testing.T, branch string) {
	t.Helper()
	m := pgctl.NewManager(filepath.Join(e.bin, "postgres"), pgctl.Options{
		DataDir:        filepath.Join(e.root, "branches", branch, "current"),
		SocketDir:      pgtest.SocketDir(t),
		Port:           pgtest.FreePort(t),
		MaxConnections: 20,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start %s: %v", branch, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = m.Stop(ctx, 10*time.Second)
	})
	srv := httptest.NewServer((&supervisor.Server{
		Token:        testToken,
		PG:           m,
		Backups:      pgctl.NewBackups(m.ConnString(), 30*time.Second),
		Generation:   m.Generation,
		StopGrace:    10 * time.Second,
		StartTimeout: 60 * time.Second,
	}).Handler())
	t.Cleanup(srv.Close)
	e.mu.Lock()
	e.urls[branch], e.pgs[branch] = srv.URL, m
	e.mu.Unlock()
}

func (e *pgEnv) connect(t *testing.T, branch string) *pgx.Conn {
	t.Helper()
	e.mu.Lock()
	m := e.pgs[branch]
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, m.ConnString())
	if err != nil {
		t.Fatalf("connect %s: %v", branch, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// exec runs sql on the branch as the superuser.
func (e *pgEnv) exec(t *testing.T, branch, sql string) {
	t.Helper()
	if _, err := e.connect(t, branch).Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %s: %v", branch, sql, err)
	}
}

// count returns the number of rows in table t on the branch.
func (e *pgEnv) count(t *testing.T, branch string) int {
	t.Helper()
	var n int
	if err := e.connect(t, branch).QueryRow(context.Background(), "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("%s: count: %v", branch, err)
	}
	return n
}

// queryBool returns the boolean that sql yields on the branch.
func (e *pgEnv) queryBool(t *testing.T, branch, sql string) bool {
	t.Helper()
	var b bool
	if err := e.connect(t, branch).QueryRow(context.Background(), sql).Scan(&b); err != nil {
		t.Fatalf("%s: %s: %v", branch, sql, err)
	}
	return b
}
