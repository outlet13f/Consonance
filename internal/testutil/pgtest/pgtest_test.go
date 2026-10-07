package pgtest_test

import (
	"os"
	"path/filepath"
	"testing"

	"consonance/internal/testutil/pgtest"
)

func TestInitDBCreatesDataDirectory(t *testing.T) {
	bin := pgtest.BinDir(t)
	dir := pgtest.InitDB(t, bin)
	if _, err := os.Stat(filepath.Join(dir, "PG_VERSION")); err != nil {
		t.Fatalf("PG_VERSION missing: %v", err)
	}
}

func TestFreePortIsUsable(t *testing.T) {
	if p := pgtest.FreePort(t); p <= 0 || p > 65535 {
		t.Fatalf("FreePort() = %d", p)
	}
}
