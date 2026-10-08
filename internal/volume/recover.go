package volume

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Recover clears up after operations that an agent restart cut short (spec 6, 10). Run it before
// serving requests. It trashes generation and restore point directories still marked
// .incomplete, current.tmp links, branch directories whose current link is missing or leads
// nowhere (a branch whose creation did not finish), restore point directories the catalog does
// not list and preflight leftovers, and drops catalog entries whose directory is gone. Complete
// data is not touched. It deletes only on certain evidence (a marker, a missing or dangling link,
// a missing directory); any other error stops it, so the agent fails to start rather than delete
// data on a guess. It returns what it removed, relative to the volume root; a dropped catalog
// entry appears as "catalog:<id>".
func (v *Volume) Recover() ([]string, error) {
	var removed []string
	drop := func(path string) error {
		if err := v.trash(path); err != nil {
			return err
		}
		removed = append(removed, v.rel(path))
		return nil
	}

	meta, err := os.ReadDir(filepath.Join(v.root, metaDir))
	if err != nil {
		return nil, err
	}
	for _, e := range meta {
		if strings.HasPrefix(e.Name(), "preflight-") {
			if err := drop(filepath.Join(v.root, metaDir, e.Name())); err != nil {
				return removed, err
			}
		}
	}

	branches, err := os.ReadDir(filepath.Join(v.root, branchesDir))
	if err != nil {
		return removed, err
	}
	for _, b := range branches {
		if !b.IsDir() {
			continue
		}
		bd := v.branchDir(b.Name())
		tmp := filepath.Join(bd, currentTmpLink)
		if err := os.Remove(tmp); err == nil {
			removed = append(removed, v.rel(tmp))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		entries, err := os.ReadDir(bd)
		if err != nil {
			return removed, err
		}
		for _, e := range entries {
			if _, ok := parseGeneration(e.Name()); !ok || !e.IsDir() {
				continue
			}
			dir := filepath.Join(bd, e.Name())
			st, err := stateOf(dir)
			if err != nil {
				return removed, err // never delete on a guess
			}
			if st == dirMarked {
				if err := drop(dir); err != nil {
					return removed, err
				}
			}
		}
		if _, err := v.currentGeneration(b.Name()); errors.Is(err, ErrNotFound) {
			if err := drop(bd); err != nil {
				return removed, err
			}
		} else if err != nil {
			return removed, err
		}
	}

	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return removed, err
	}
	listed := map[string]bool{}
	var kept []catalogEntry
	for _, e := range cat.RestorePoints {
		dir := filepath.Join(v.root, filepath.FromSlash(e.Path))
		st, err := stateOf(dir)
		if err != nil {
			return removed, err
		}
		if st == dirComplete {
			kept = append(kept, e)
			listed[dir] = true
		} else {
			removed = append(removed, "catalog:"+e.ID)
		}
	}
	if len(kept) != len(cat.RestorePoints) {
		cat.RestorePoints = kept
		if err := saveCatalog(v.catalogPath(), cat); err != nil {
			return removed, err
		}
	}
	restoreBranches, err := os.ReadDir(filepath.Join(v.root, restoreDir))
	if err != nil {
		return removed, err
	}
	for _, rb := range restoreBranches {
		if !rb.IsDir() {
			continue
		}
		rbd := v.restoreBranchDir(rb.Name())
		rps, err := os.ReadDir(rbd)
		if err != nil {
			return removed, err
		}
		for _, rp := range rps {
			if dir := filepath.Join(rbd, rp.Name()); !listed[dir] {
				if err := drop(dir); err != nil {
					return removed, err
				}
			}
		}
		_ = os.Remove(rbd) // succeeds only when nothing is left in it
	}
	return removed, nil
}
