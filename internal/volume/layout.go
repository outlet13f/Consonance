package volume

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The volume layout (spec 6), relative to the volume root.
const (
	branchesDir      = "branches"
	restoreDir       = "restore"
	metaDir          = ".consonance"
	currentLink      = "current"
	currentTmpLink   = "current.tmp"
	incompleteMarker = ".incomplete"
	generationPrefix = "data-"
)

// layout turns names into paths in a volume.
type layout struct{ root string }

func (l layout) branchDir(branch string) string {
	return filepath.Join(l.root, branchesDir, branch)
}

func (l layout) genDir(branch string, n int) string {
	return filepath.Join(l.branchDir(branch), generationName(n))
}

func (l layout) restoreBranchDir(branch string) string {
	return filepath.Join(l.root, restoreDir, branch)
}

func (l layout) restorePointDir(branch, id string) string {
	return filepath.Join(l.restoreBranchDir(branch), id)
}

func (l layout) catalogPath() string { return filepath.Join(l.root, metaDir, "catalog.json") }

func (l layout) trashPath() string { return filepath.Join(l.root, metaDir, "trash") }

// rel returns path relative to the volume root, slash-separated (catalog paths, log lines).
func (l layout) rel(path string) string {
	r, err := filepath.Rel(l.root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

// generationName is the directory name of generation n.
func generationName(n int) string { return generationPrefix + strconv.Itoa(n) }

// parseGeneration returns n for "data-n" (n ≥ 1, without sign or leading zeros).
func parseGeneration(name string) (int, bool) {
	digits, ok := strings.CutPrefix(name, generationPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || generationName(n) != name {
		return 0, false
	}
	return n, true
}

// complete reports whether dir is a directory without the .incomplete marker (spec 6).
func complete(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	_, err = os.Lstat(filepath.Join(dir, incompleteMarker))
	return errors.Is(err, fs.ErrNotExist)
}

// currentGeneration returns the generation the branch's current link points at. A branch whose
// link is missing or leads to no complete generation has no data: ErrNotFound.
func (l layout) currentGeneration(branch string) (int, error) {
	target, err := os.Readlink(filepath.Join(l.branchDir(branch), currentLink))
	if err != nil {
		return 0, fmt.Errorf("%w: branch %q has no data", ErrNotFound, branch)
	}
	n, ok := parseGeneration(target)
	if !ok || !complete(l.genDir(branch, n)) {
		return 0, fmt.Errorf("%w: branch %q: current points at %q, which is no complete generation", ErrNotFound, branch, target)
	}
	return n, nil
}

// generations returns the branch's complete generations in ascending order.
func (l layout) generations(branch string) ([]int, error) {
	entries, err := os.ReadDir(l.branchDir(branch))
	if err != nil {
		return nil, err
	}
	var gens []int
	for _, e := range entries {
		if n, ok := parseGeneration(e.Name()); ok && e.IsDir() && complete(l.genDir(branch, n)) {
			gens = append(gens, n)
		}
	}
	slices.Sort(gens)
	return gens, nil
}

// nextGeneration returns one more than the highest data-n in the branch directory, finished or
// not, so a new generation never reuses a name.
func (l layout) nextGeneration(branch string) (int, error) {
	entries, err := os.ReadDir(l.branchDir(branch))
	if err != nil {
		return 0, err
	}
	next := 1
	for _, e := range entries {
		if n, ok := parseGeneration(e.Name()); ok && n >= next {
			next = n + 1
		}
	}
	return next, nil
}

// switchCurrent points branchDir/current at data-n atomically (spec 6): it makes the link
// current.tmp, renames it over current and fsyncs the directory.
func switchCurrent(branchDir string, n int) error {
	tmp := filepath.Join(branchDir, currentTmpLink)
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Symlink(generationName(n), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(branchDir, currentLink)); err != nil {
		return err
	}
	return syncDir(branchDir)
}

// makeMarked creates dir (mode 0700) holding the .incomplete marker (spec 6). It stays marked
// until finishMarked; Recover deletes it if the agent stops first.
func makeMarked(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, incompleteMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// finishMarked removes the marker of a directory whose copy is complete and makes that durable.
func finishMarked(dir string) error {
	if err := os.Remove(filepath.Join(dir, incompleteMarker)); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
}

// syncDir fsyncs a directory so that the entries created, renamed or removed in it are durable.
// On XFS it forces the log, which makes every earlier metadata change durable too.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeFileSync writes data to path (created or truncated, mode perm) and fsyncs it.
func writeFileSync(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
