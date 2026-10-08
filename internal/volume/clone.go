package volume

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// SkipFunc reports whether the entry at rel (slash-separated, relative to the source root) is
// left out of a copy. A skipped directory is left out with everything in it.
type SkipFunc func(rel string) bool

// CloneTree reflink-copies the contents of the directory src into the existing directory dst.
// Regular files are cloned with FICLONE: no data is copied, and the copy shares extents with
// the source until either side writes. Directories and symlinks are re-created, permission bits
// are kept, and dst gets src's. Entries that disappear while the tree is walked are left out: a
// running PostgreSQL deletes files during a backup, and WAL replay accounts for that. Other file
// types are an error. src and dst must be on one filesystem that supports reflink; otherwise
// the error wraps ErrReflinkUnsupported.
func CloneTree(src, dst string, skip SkipFunc) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("clone %s: not a directory", src)
	}

	type fileJob struct {
		src, dst string
		perm     fs.FileMode
	}
	type dirMode struct {
		path string
		perm fs.FileMode
	}
	var (
		jobs     = make(chan fileJob)
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		dirs     = []dirMode{{dst, info.Mode().Perm()}}
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if err := cloneFile(j.src, j.dst, j.perm); err != nil {
					fail(err)
				}
			}
		}()
	}

	walkErr := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path != src && errors.Is(err, fs.ErrNotExist) {
				return nil // deleted while we walked
			}
			return err
		}
		if failed() {
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skip != nil && skip(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.IsDir():
			fi, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			if err != nil {
				return err
			}
			// Writable while the files go in; the real permission bits come at the end.
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{target, fi.Mode().Perm()})
			return nil
		case d.Type().IsRegular():
			fi, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			jobs <- fileJob{path, target, fi.Mode().Perm()}
			return nil
		default:
			return fmt.Errorf("clone %s: unsupported file type %s", path, d.Type())
		}
	})
	close(jobs)
	wg.Wait()
	if walkErr != nil {
		return walkErr
	}
	if firstErr != nil {
		return firstErr
	}
	// Deepest first, so every directory can still be entered while its children get their modes.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].perm); err != nil {
			return err
		}
	}
	return nil
}

// cloneFile creates dst with permission bits perm and clones src's contents into it. A source
// deleted before it could be opened is left out.
func cloneFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := ficlone(out, in); err != nil {
		out.Close()
		return fmt.Errorf("clone %s: %w", src, err)
	}
	if err := out.Chmod(perm); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
