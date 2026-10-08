package volume_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"consonance/internal/testutil/pgtest"
	"consonance/internal/volume"
)

// hbaRules returns the rule lines of a pg_hba.conf with their fields single-spaced.
func hbaRules(conf string) []string {
	var out []string
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Join(strings.Fields(line), " "))
	}
	return out
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

func TestInitMainCreatesFirstGeneration(t *testing.T) {
	bin := pgtest.BinDir(t)
	root := t.TempDir()
	v := openVolume(t, root, nil, func(c *volume.Config) {
		c.InitDB = filepath.Join(bin, "initdb")
		c.InitDBArgs = []string{"--no-sync"}
	})
	ctx := testCtx(t)
	got, err := v.InitMain(ctx)
	if err != nil {
		t.Fatalf("InitMain: %v", err)
	}
	if got.Generation != 1 {
		t.Fatalf("generation = %d, want 1", got.Generation)
	}
	main := filepath.Join(root, "branches", "main")
	if target, err := os.Readlink(filepath.Join(main, "current")); err != nil || target != "data-1" {
		t.Fatalf("current = %q, %v; want data-1", target, err)
	}
	if ver := strings.TrimSpace(readFile(t, filepath.Join(main, "data-1", "PG_VERSION"))); ver != "17" {
		t.Fatalf("PG_VERSION = %q", ver)
	}
	wantHBA := []string{
		"local all postgres peer",
		"local all all scram-sha-256",
		"host all postgres all reject",
		"host all all all scram-sha-256",
	}
	if rules := hbaRules(readFile(t, filepath.Join(main, "data-1", "pg_hba.conf"))); !slices.Equal(rules, wantHBA) {
		t.Fatalf("pg_hba.conf rules = %q, want %q", rules, wantHBA)
	}
	out, err := exec.Command(filepath.Join(bin, "pg_controldata"), filepath.Join(main, "data-1")).CombinedOutput()
	if err != nil {
		t.Fatalf("pg_controldata: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`Data page checksum version:\s+1`).Match(out) {
		t.Fatalf("data checksums are off:\n%s", out)
	}

	// A second call keeps the data.
	before := inode(t, filepath.Join(main, "data-1", "PG_VERSION"))
	again, err := v.InitMain(ctx)
	if err != nil || again.Generation != 1 {
		t.Fatalf("second InitMain = %+v, %v", again, err)
	}
	if inode(t, filepath.Join(main, "data-1", "PG_VERSION")) != before {
		t.Fatal("the second InitMain made main again")
	}
}

func TestInitMainReplacesAnUnfinishedAttempt(t *testing.T) {
	bin := pgtest.BinDir(t)
	root := t.TempDir()
	// An earlier initdb was cut short: data without a current link.
	writeFile(t, filepath.Join(root, "branches", "main", "data-1", "junk"), "half", 0o600)
	v := openVolume(t, root, nil, func(c *volume.Config) {
		c.InitDB = filepath.Join(bin, "initdb")
		c.InitDBArgs = []string{"--no-sync"}
	})
	if _, err := v.InitMain(testCtx(t)); err != nil {
		t.Fatalf("InitMain: %v", err)
	}
	if exists(filepath.Join(root, "branches", "main", "data-1", "junk")) {
		t.Fatal("the leftover was kept")
	}
	if !exists(filepath.Join(root, "branches", "main", "data-1", "PG_VERSION")) {
		t.Fatal("no new data")
	}
}

func TestInitMainFailureLeavesNoMain(t *testing.T) {
	root := t.TempDir()
	v := openVolume(t, root, nil, func(c *volume.Config) { c.InitDB = filepath.Join(root, "no-such-initdb") })
	if _, err := v.InitMain(testCtx(t)); err == nil {
		t.Fatal("InitMain succeeded without initdb")
	}
	if exists(filepath.Join(root, "branches", "main")) {
		t.Fatal("a failed InitMain left branches/main")
	}
}

func TestInitMainKeepsAMainItCannotRead(t *testing.T) {
	root := t.TempDir()
	// current is a real directory, not a link: main's state cannot be judged.
	writeFile(t, filepath.Join(root, "branches", "main", "current", "keep"), "x", 0o600)
	v := openVolume(t, root, nil, func(c *volume.Config) { c.InitDB = filepath.Join(root, "no-such-initdb") })
	_, err := v.InitMain(testCtx(t))
	if err == nil || errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("InitMain = %v, want an error that is not ErrNotFound", err)
	}
	if !exists(filepath.Join(root, "branches", "main", "current", "keep")) {
		t.Fatal("InitMain removed a main it could not read")
	}
}
