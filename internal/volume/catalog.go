package volume

import (
	"cmp"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"consonance/internal/agentapi"
)

const catalogVersion = 1

// catalog is .consonance/catalog.json (spec 4.4). Only the agent reads and writes it.
type catalog struct {
	Version       int            `json:"version"`
	RestorePoints []catalogEntry `json:"restorePoints"`
}

// catalogEntry is a restore point and its directory, relative to the volume root.
type catalogEntry struct {
	agentapi.RestorePoint
	Path string `json:"path"`
}

// loadCatalog reads the catalog at path. A missing file is an empty catalog.
func loadCatalog(path string) (catalog, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return catalog{Version: catalogVersion}, nil
	}
	if err != nil {
		return catalog{}, err
	}
	var c catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return catalog{}, fmt.Errorf("catalog %s: %w", path, err)
	}
	if c.Version != catalogVersion {
		return catalog{}, fmt.Errorf("catalog %s: version %d, want %d", path, c.Version, catalogVersion)
	}
	return c, nil
}

// saveCatalog replaces the catalog at path atomically (spec 4.4): it writes a temporary file,
// fsyncs it, renames it over the old one and fsyncs the directory.
func saveCatalog(path string, c catalog) error {
	c.Version = catalogVersion
	if c.RestorePoints == nil {
		c.RestorePoints = []catalogEntry{}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// newRestorePointID returns rp-<UTC yyyymmddThhmmssZ>-<4 random characters> (spec 4.4).
func newRestorePointID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	suffix := make([]byte, len(b))
	for i, x := range b {
		suffix[i] = idAlphabet[int(x)%len(idAlphabet)]
	}
	return "rp-" + now.UTC().Format("20060102T150405Z") + "-" + string(suffix)
}

// expired returns the restore points of branch that the retention policy removes (spec 7.2):
// those older than maxAge, then the oldest beyond maxPerBranch. A zero limit is no limit.
func expired(entries []catalogEntry, branch string, now time.Time, maxPerBranch int, maxAge time.Duration) []catalogEntry {
	var mine []catalogEntry
	for _, e := range entries {
		if e.Branch == branch {
			mine = append(mine, e)
		}
	}
	slices.SortFunc(mine, func(a, b catalogEntry) int { // newest first
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	var out []catalogEntry
	for i, e := range mine {
		tooOld := maxAge > 0 && now.Sub(e.CreatedAt) > maxAge
		tooMany := maxPerBranch > 0 && i >= maxPerBranch
		if tooOld || tooMany {
			out = append(out, e)
		}
	}
	return out
}
