package volume_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"consonance/internal/testutil/xfstest"
	"consonance/internal/volume"
)

// writeFile writes content to path with exactly perm, creating parent directories.
func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil { // WriteFile's mode is subject to the umask
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestCloneTreeCopiesTheTree(t *testing.T) {
	root := xfstest.Dir(t)
	src := filepath.Join(root, "src")
	big := string(bytes.Repeat([]byte("0123456789abcdef"), 4096)) // 64 KiB
	writeFile(t, filepath.Join(src, "PG_VERSION"), "17\n", 0o600)
	writeFile(t, filepath.Join(src, "base", "5", "1259"), big, 0o600)
	writeFile(t, filepath.Join(src, "empty"), "", 0o640)
	writeFile(t, filepath.Join(src, "pg_wal", "000000010000000000000001"), "wal", 0o600)
	writeFile(t, filepath.Join(src, "postmaster.pid"), "4242\n", 0o600)
	if err := os.Symlink("base/5", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "base"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	skip := func(rel string) bool { return rel == "pg_wal" || rel == "postmaster.pid" }
	if err := volume.CloneTree(src, dst, skip); err != nil {
		t.Fatalf("CloneTree: %v", err)
	}

	if got := readFile(t, filepath.Join(dst, "PG_VERSION")); got != "17\n" {
		t.Errorf("PG_VERSION = %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "base", "5", "1259")); got != big {
		t.Error("cloned file differs from its source")
	}
	if got := readFile(t, filepath.Join(dst, "empty")); got != "" {
		t.Errorf("empty = %q", got)
	}
	for path, want := range map[string]os.FileMode{
		dst:                              0o700,
		filepath.Join(dst, "base"):       0o750,
		filepath.Join(dst, "PG_VERSION"): 0o600,
		filepath.Join(dst, "empty"):      0o640,
	} {
		if got := permOf(t, path); got != want {
			t.Errorf("mode of %s = %o, want %o", path, got, want)
		}
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "base/5" {
		t.Errorf("link = %q, %v; want base/5", target, err)
	}
	for _, name := range []string{"pg_wal", "postmaster.pid"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was copied although skipped (Lstat: %v)", name, err)
		}
	}
	// The copy is independent: writing it leaves the source alone.
	writeFile(t, filepath.Join(dst, "base", "5", "1259"), "changed", 0o600)
	if got := readFile(t, filepath.Join(src, "base", "5", "1259")); got != big {
		t.Fatal("writing the clone changed the source")
	}
}

func TestCloneTreeManyFiles(t *testing.T) {
	root := xfstest.Dir(t)
	src := filepath.Join(root, "src")
	for i := range 300 {
		writeFile(t, filepath.Join(src, fmt.Sprintf("d%02d", i%17), fmt.Sprintf("f%03d", i)), fmt.Sprintf("file %d", i), 0o600)
	}
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := volume.CloneTree(src, dst, nil); err != nil {
		t.Fatalf("CloneTree: %v", err)
	}
	for i := range 300 {
		rel := filepath.Join(fmt.Sprintf("d%02d", i%17), fmt.Sprintf("f%03d", i))
		if got := readFile(t, filepath.Join(dst, rel)); got != fmt.Sprintf("file %d", i) {
			t.Fatalf("%s = %q", rel, got)
		}
	}
}

func TestCloneTreeWithoutReflinkFails(t *testing.T) {
	// t.TempDir is not XFS: ext4 or overlayfs in the test container, APFS on a Mac.
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a"), "data", 0o600)
	err := volume.CloneTree(src, t.TempDir(), nil)
	if !errors.Is(err, volume.ErrReflinkUnsupported) {
		t.Fatalf("CloneTree = %v, want ErrReflinkUnsupported", err)
	}
}

func TestCloneTreeFollowsALinkedSource(t *testing.T) {
	root := xfstest.Dir(t)
	src := filepath.Join(root, "src")
	writeFile(t, filepath.Join(src, "file.txt"), "content", 0o600)
	writeFile(t, filepath.Join(src, "nested", "deep", "file2.txt"), "nested content", 0o600)

	link := filepath.Join(root, "src-link")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := volume.CloneTree(link, dst, nil); err != nil {
		t.Fatalf("CloneTree(link): %v", err)
	}

	if got := readFile(t, filepath.Join(dst, "file.txt")); got != "content" {
		t.Errorf("file.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "nested", "deep", "file2.txt")); got != "nested content" {
		t.Errorf("nested/deep/file2.txt = %q", got)
	}
}

func TestCloneTreeToleratesEntriesDeletedDuringTheWalk(t *testing.T) {
	src := t.TempDir()
	if err := os.Mkdir(filepath.Join(src, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "z"), "z content", 0o600)
	writeFile(t, filepath.Join(src, "zdir", "inner", "f"), "nested", 0o600)

	dst := t.TempDir()

	skip := func(rel string) bool {
		if rel == "a" {
			// Delete z and zdir while the walker is running; the root entries were already listed
			os.Remove(filepath.Join(src, "z"))
			os.RemoveAll(filepath.Join(src, "zdir"))
		}
		return false
	}

	err := volume.CloneTree(src, dst, skip)
	if err != nil {
		t.Fatalf("CloneTree: %v", err)
	}

	// Only "a" should be in dst
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "a" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("dst entries = %v, want [a]", names)
	}
	// "a" is a directory, so verify it exists
	aInfo, err := os.Stat(filepath.Join(dst, "a"))
	if err != nil {
		t.Errorf("Stat(a): %v", err)
	} else if !aInfo.IsDir() {
		t.Error("a is not a directory")
	}
}
