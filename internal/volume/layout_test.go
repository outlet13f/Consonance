package volume

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func mustMkdirAll(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParseGeneration(t *testing.T) {
	for name, want := range map[string]int{"data-1": 1, "data-12": 12} {
		if n, ok := parseGeneration(name); !ok || n != want {
			t.Errorf("parseGeneration(%q) = %d, %v; want %d", name, n, ok, want)
		}
	}
	for _, name := range []string{"data-0", "data-01", "data-", "data--1", "data-+1", "data-1a", "data", "current", "data-1.tmp"} {
		if n, ok := parseGeneration(name); ok {
			t.Errorf("parseGeneration(%q) = %d, want rejected", name, n)
		}
	}
	if got := generationName(7); got != "data-7" {
		t.Errorf("generationName(7) = %q", got)
	}
}

func TestCurrentLink(t *testing.T) {
	l := layout{root: t.TempDir()}
	bd := l.branchDir("dev")
	mustMkdirAll(t, l.genDir("dev", 1), l.genDir("dev", 2))
	if _, err := l.currentGeneration("dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("without a link: %v, want ErrNotFound", err)
	}
	for _, n := range []int{1, 2} {
		if err := switchCurrent(bd, n); err != nil {
			t.Fatal(err)
		}
		if got, err := l.currentGeneration("dev"); err != nil || got != n {
			t.Fatalf("after switching to %d: %d, %v", n, got, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(bd, "current")); err != nil || target != "data-2" {
		t.Fatalf("link = %q, %v; want the relative target data-2", target, err)
	}
	if _, err := os.Lstat(filepath.Join(bd, "current.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("current.tmp left behind")
	}
	// A link to a missing or unfinished generation is no current generation.
	if err := switchCurrent(bd, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := l.currentGeneration("dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link to a missing generation: %v, want ErrNotFound", err)
	}
	if err := makeMarked(l.genDir("dev", 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.currentGeneration("dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link to an unfinished generation: %v, want ErrNotFound", err)
	}
	if err := finishMarked(l.genDir("dev", 3)); err != nil {
		t.Fatal(err)
	}
	if got, err := l.currentGeneration("dev"); err != nil || got != 3 {
		t.Fatalf("after finishing data-3: %d, %v", got, err)
	}
}

func TestGenerationListing(t *testing.T) {
	l := layout{root: t.TempDir()}
	mustMkdirAll(t, l.genDir("main", 1), l.genDir("main", 3), filepath.Join(l.branchDir("main"), "data-x"))
	if err := makeMarked(l.genDir("main", 4)); err != nil {
		t.Fatal(err)
	}
	gens, err := l.generations("main")
	if err != nil || !slices.Equal(gens, []int{1, 3}) {
		t.Fatalf("generations = %v, %v; want [1 3] (data-4 is unfinished)", gens, err)
	}
	if next, err := l.nextGeneration("main"); err != nil || next != 5 {
		t.Fatalf("nextGeneration = %d, %v; want 5 (the unfinished data-4 counts)", next, err)
	}
	if _, err := l.generations("nobody"); err == nil {
		t.Fatal("generations of a missing branch succeeded")
	}
}

func TestMarkedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rp")
	if err := makeMarked(dir); err != nil {
		t.Fatal(err)
	}
	if complete(dir) {
		t.Fatal("a marked directory counts as complete")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %o, want 700", fi.Mode().Perm())
	}
	if err := makeMarked(dir); err == nil {
		t.Fatal("makeMarked reused an existing directory")
	}
	if err := finishMarked(dir); err != nil {
		t.Fatal(err)
	}
	if !complete(dir) {
		t.Fatal("a finished directory is not complete")
	}
	if err := finishMarked(dir); err == nil {
		t.Fatal("finishMarked succeeded without a marker")
	}
}

func TestWriteFileSyncReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup_label")
	for _, s := range []string{"first and longer", "second"} {
		if err := writeFileSync(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "second" {
		t.Fatalf("file = %q, %v; want second", b, err)
	}
}
