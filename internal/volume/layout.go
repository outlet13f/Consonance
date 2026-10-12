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

// dirState is what is known for sure about a generation or restore point directory.
type dirState int

const (
	dirUnknown  dirState = iota // stateOf failed: nothing is known for sure
	dirMissing                  // the directory does not exist
	dirMarked                   // it exists and still holds the .incomplete marker
	dirComplete                 // it exists without the marker
)

// stateOf reports dir's state. An error other than "does not exist" is returned as it is, so
// that no caller deletes or re-creates data on a guess; the state is dirUnknown then.
func stateOf(dir string) (dirState, error) {
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return dirMissing, nil
	}
	if err != nil {
		return dirUnknown, err
	}
	if !fi.IsDir() {
		return dirUnknown, fmt.Errorf("%s is not a directory", dir)
	}
	_, err = os.Lstat(filepath.Join(dir, incompleteMarker))
	switch {
	case err == nil:
		return dirMarked, nil
	case errors.Is(err, fs.ErrNotExist):
		return dirComplete, nil
	default:
		return dirUnknown, err
	}
}

// complete reports whether dir is a directory without the .incomplete marker (spec 6).
// An error counts as not complete; where a wrong answer could delete or re-create data, use stateOf.
func complete(dir string) bool {
	st, err := stateOf(dir)
	return err == nil && st == dirComplete
}

// currentGeneration returns the generation the branch's current link points at. It is
// ErrNotFound only when the branch certainly has no data: the link is missing, or leads to a
// missing or unfinished generation. Any other error is returned as it is, including a link that
// names no generation: the agent writes only data-N targets, so anything else was put there by
// hand, and the branch may still hold complete generations.
func (l layout) currentGeneration(branch string) (int, error) {
	target, err := os.Readlink(filepath.Join(l.branchDir(branch), currentLink))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("%w: branch %q has no data", ErrNotFound, branch)
	}
	if err != nil {
		return 0, fmt.Errorf("branch %q: read %s: %w", branch, currentLink, err)
	}
	n, ok := parseGeneration(target)
	if !ok {
		return 0, fmt.Errorf("branch %q: current points at %q, which is no generation", branch, target)
	}
	st, err := stateOf(l.genDir(branch, n))
	if err != nil {
		return 0, fmt.Errorf("branch %q: %w", branch, err)
	}
	if st != dirComplete {
		return 0, fmt.Errorf("%w: branch %q: current points at %q, which is missing or unfinished", ErrNotFound, branch, target)
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
		os.Remove(dir) // an unmarked directory would read as complete
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
