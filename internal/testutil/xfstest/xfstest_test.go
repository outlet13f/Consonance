package xfstest_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"consonance/internal/testutil/xfstest"
)

func TestDirIsFreshWritableXFS(t *testing.T) {
	dir := xfstest.Dir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s is not empty: %v", dir, entries)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	const xfsMagic = 0x58465342
	if st.Type != xfsMagic {
		t.Fatalf("%s is on filesystem type %#x, want XFS (%#x)", dir, st.Type, xfsMagic)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe"), []byte("x"), 0o600); err != nil {
		t.Fatalf("not writable: %v", err)
	}
}
