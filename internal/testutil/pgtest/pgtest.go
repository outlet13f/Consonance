// Package pgtest provides helpers for tests that need real PostgreSQL binaries.
package pgtest

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// EnvBinDir names the environment variable pointing at PostgreSQL's bin directory.
const EnvBinDir = "CONSONANCE_TEST_PG_BIN"

// BinDir returns the PostgreSQL bin directory or skips the test when it is not configured.
func BinDir(t testing.TB) string {
	t.Helper()
	dir := os.Getenv(EnvBinDir)
	if dir == "" {
		t.Skipf("%s not set; skipping test that needs PostgreSQL binaries", EnvBinDir)
	}
	return dir
}

// InitDB creates a fresh data directory with trust authentication in a temp dir.
func InitDB(t testing.TB, binDir string) string {
	t.Helper()
	return InitDBAt(t, binDir, filepath.Join(t.TempDir(), "data"))
}

// InitDBAt runs initdb into dataDir (superuser "postgres", trust auth) and returns dataDir.
func InitDBAt(t testing.TB, binDir, dataDir string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "initdb"),
		"-D", dataDir, "-U", "postgres", "--auth=trust", "--no-sync")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	return dataDir
}

// FreePort returns a TCP port that was free a moment ago.
func FreePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// SocketDir returns a short directory for unix sockets (socket paths must stay under ~100 bytes).
func SocketDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pgsock")
	if err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
