package volume_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRecoverRemovesLeftovers(t *testing.T) {
	root := t.TempDir()
	created := newClock().Now()
	// Complete data: it must survive.
	mkGeneration(t, root, "main", 1, true)
	mkGeneration(t, root, "main", 2, false) // finished, not activated yet
	writeCatalog(t, root,
		rpFixture{id: "rp-20261007T031500Z-ab12", branch: "main", reason: "api", createdAt: created},
		rpFixture{id: "rp-20261007T031600Z-gone", branch: "main", reason: "api", createdAt: created},
	)
	if err := os.RemoveAll(filepath.Join(root, "restore", "main", "rp-20261007T031600Z-gone")); err != nil {
		t.Fatal(err)
	}
	// What interrupted operations leave behind: a generation copy cut short,
	writeFile(t, filepath.Join(root, "branches", "main", "data-3", ".incomplete"), "", 0o600)
	// a link switch cut short,
	if err := os.Symlink("data-2", filepath.Join(root, "branches", "main", "current.tmp")); err != nil {
		t.Fatal(err)
	}
	// a branch copy cut short after linking,
	half := mkGeneration(t, root, "half", 1, true)
	writeFile(t, filepath.Join(half, ".incomplete"), "", 0o600)
	// an initdb that never got its link,
	mkGeneration(t, root, "nolink", 1, false)
	// a restore point that never made it into the catalog, one cut short,
	writeFile(t, filepath.Join(root, "restore", "main", "rp-20261007T031700Z-cd34", "PG_VERSION"), "17\n", 0o600)
	writeFile(t, filepath.Join(root, "restore", "dev", "rp-20261007T031800Z-ef56", ".incomplete"), "", 0o600)
	// and an interrupted preflight.
	writeFile(t, filepath.Join(root, ".consonance", "preflight-123", "src"), "x", 0o600)

	v := openVolume(t, root, nil, nil)
	removed, err := v.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	slices.Sort(removed)
	want := []string{
		".consonance/preflight-123",
		"branches/half",
		"branches/half/data-1",
		"branches/main/current.tmp",
		"branches/main/data-3",
		"branches/nolink",
		"catalog:rp-20261007T031600Z-gone",
		"restore/dev/rp-20261007T031800Z-ef56",
		"restore/main/rp-20261007T031700Z-cd34",
	}
	if !slices.Equal(removed, want) {
		t.Fatalf("removed %q\nwant    %q", removed, want)
	}
	for _, p := range []string{"branches/main/data-1", "branches/main/data-2", "restore/main/rp-20261007T031500Z-ab12"} {
		if !exists(filepath.Join(root, p)) {
			t.Errorf("%s was removed", p)
		}
	}
	if target, err := os.Readlink(filepath.Join(root, "branches", "main", "current")); err != nil || target != "data-1" {
		t.Errorf("main/current = %q, %v; want data-1", target, err)
	}
	cat := readFile(t, filepath.Join(root, ".consonance", "catalog.json"))
	if !strings.Contains(cat, "rp-20261007T031500Z-ab12") || strings.Contains(cat, "gone") {
		t.Errorf("catalog after Recover:\n%s", cat)
	}
	// What Recover removed waits in the trash until EmptyTrash.
	if err := v.EmptyTrash(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, ".consonance", "trash")); err != nil || len(entries) != 0 {
		t.Fatalf("trash after EmptyTrash: %v, %v", entries, err)
	}
	if again, err := v.Recover(); err != nil || len(again) != 0 {
		t.Fatalf("second Recover = %q, %v; want nothing to do", again, err)
	}
}

func TestRecoverStopsOnACurrentItCannotRead(t *testing.T) {
	root := t.TempDir()
	gen := mkGeneration(t, root, "main", 1, false)
	// current is a regular file: reading the link fails with EINVAL, which proves nothing.
	writeFile(t, filepath.Join(root, "branches", "main", "current"), "data-1", 0o600)
	v := openVolume(t, root, nil, nil)
	if _, err := v.Recover(); err == nil {
		t.Fatal("Recover went on past a current link it could not read")
	}
	if !exists(filepath.Join(gen, "PG_VERSION")) || !exists(filepath.Join(root, "branches", "main", "current")) {
		t.Fatal("Recover removed main though it could not judge it")
	}
}

// The agent writes only data-N link targets. A current that names no generation was put there by
// hand, and its branch may still hold complete generations: Recover stops instead of trashing it.
func TestRecoverStopsOnACurrentThatNamesNoGeneration(t *testing.T) {
	root := t.TempDir()
	gen1 := mkGeneration(t, root, "dev", 1, false)
	gen2 := mkGeneration(t, root, "dev", 2, false)
	link := filepath.Join(root, "branches", "dev", "current")
	if err := os.Symlink("./data-2", link); err != nil {
		t.Fatal(err)
	}
	v := openVolume(t, root, nil, nil)
	if removed, err := v.Recover(); err == nil {
		t.Fatalf("Recover went on past a current link that names no generation, removing %q", removed)
	}
	for _, gen := range []string{gen1, gen2} {
		if !exists(filepath.Join(gen, "PG_VERSION")) {
			t.Errorf("Recover removed %s", gen)
		}
	}
	if target, err := os.Readlink(link); err != nil || target != "./data-2" {
		t.Errorf("dev/current = %q, %v; want it left as it was", target, err)
	}
}

func TestRecoverWithoutACatalogKeepsFinishedRestorePoints(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	writeFile(t, filepath.Join(root, "restore", "main", "rp-20261007T031500Z-ab12", "PG_VERSION"), "17\n", 0o600)
	writeFile(t, filepath.Join(root, "restore", "main", "rp-20261007T031600Z-cd34", ".incomplete"), "", 0o600)
	v := openVolume(t, root, nil, nil) // no catalog.json was ever written
	removed, err := v.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !slices.Equal(removed, []string{"restore/main/rp-20261007T031600Z-cd34"}) {
		t.Fatalf("removed %q, want only the unfinished restore point", removed)
	}
	if !exists(filepath.Join(root, "restore", "main", "rp-20261007T031500Z-ab12", "PG_VERSION")) {
		t.Fatal("a finished restore point was removed although no catalog said it was abandoned")
	}
}

// A directory that can be renamed but not searched (mode 0200) is the case where a guess would
// really delete data: Recover cannot see whether the marker is there, and trashing the directory
// would succeed. (With mode 0000 the rename fails by itself, which would hide the guess.)
func TestRecoverKeepsWhatItCannotInspect(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	const rpID = "rp-20261007T031500Z-ab12"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string) (dir string)
	}{
		{"generation", func(t *testing.T, root string) string {
			return mkGeneration(t, root, "main", 1, true)
		}},
		{"restore point", func(t *testing.T, root string) string {
			writeCatalog(t, root, rpFixture{id: rpID, branch: "main", reason: "api", createdAt: newClock().Now()})
			return filepath.Join(root, "restore", "main", rpID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := tc.setup(t, root)
			if err := os.Chmod(dir, 0o200); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			v := openVolume(t, root, nil, nil)
			if removed, err := v.Recover(); err == nil {
				t.Fatalf("Recover went on past a directory it cannot inspect, removing %q", removed)
			}
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if !exists(filepath.Join(dir, "PG_VERSION")) {
				t.Fatal("Recover removed what it could not inspect")
			}
		})
	}
}

// Without a catalog Recover looks at each restore point directory itself, and one it cannot
// inspect stops it like anywhere else (mode 0200, see TestRecoverKeepsWhatItCannotInspect).
func TestRecoverWithoutACatalogStopsAtARestorePointItCannotInspect(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "restore", "main", "rp-20261007T031500Z-ab12")
	writeFile(t, filepath.Join(dir, "PG_VERSION"), "17\n", 0o600)
	if err := os.Chmod(dir, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	v := openVolume(t, root, nil, nil) // no catalog.json was ever written
	if removed, err := v.Recover(); err == nil {
		t.Fatalf("Recover went on past a restore point it cannot inspect, removing %q", removed)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "PG_VERSION")) {
		t.Fatal("Recover removed what it could not inspect")
	}
}
