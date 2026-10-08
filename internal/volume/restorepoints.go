package volume

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"consonance/internal/agentapi"
)

// maxLabelLen bounds a restore point label.
const maxLabelLen = 256

// CreateRestorePoint copies the branch's running database into restore/<branch>/<id> the way a
// branch is created, adds it to the catalog and applies the branch's retention (spec 7.2, 9.3).
func (v *Volume) CreateRestorePoint(ctx context.Context, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error) {
	if !agentapi.ValidBranchName(branch) {
		return agentapi.RestorePoint{}, fmt.Errorf("%w: branch name %q", ErrInvalid, branch)
	}
	if req.Reason != "" && req.Reason != agentapi.ReasonAPI {
		return agentapi.RestorePoint{}, fmt.Errorf("%w: reason %q: only %q can be requested", ErrInvalid, req.Reason, agentapi.ReasonAPI)
	}
	if len(req.Label) > maxLabelLen {
		return agentapi.RestorePoint{}, fmt.Errorf("%w: label longer than %d bytes", ErrInvalid, maxLabelLen)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.RestorePoint{}, err
	}
	defer release()

	if _, err := v.currentGeneration(branch); err != nil {
		return agentapi.RestorePoint{}, err
	}
	if err := v.checkCopyInterval(branch); err != nil {
		return agentapi.RestorePoint{}, err
	}
	if err := v.checkSpace(); err != nil {
		return agentapi.RestorePoint{}, err
	}
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return agentapi.RestorePoint{}, err
	}
	now := v.cfg.Now()
	id := v.unusedRestorePointID(cat, branch, now)
	if err := os.MkdirAll(v.restoreBranchDir(branch), 0o700); err != nil {
		return agentapi.RestorePoint{}, err
	}
	dir := v.restorePointDir(branch, id)
	if err := makeMarked(dir); err != nil {
		return agentapi.RestorePoint{}, err
	}
	lsn, err := v.snapshot(ctx, branch, dir, "consonance restore point "+id)
	if err == nil {
		err = finishMarked(dir)
	}
	if err != nil {
		v.trashOrWarn(dir)
		return agentapi.RestorePoint{}, err
	}
	rp := agentapi.RestorePoint{
		ID: id, Branch: branch, CreatedAt: now.UTC().Truncate(time.Second),
		Reason: agentapi.ReasonAPI, Label: req.Label, BackupStartLSN: lsn,
	}
	// The catalog entry is the commit point: while a catalog file exists, Recover removes a
	// restore point directory the catalog does not list.
	if err := v.addRestorePoint(cat, rp, dir); err != nil {
		v.trashOrWarn(dir)
		return agentapi.RestorePoint{}, err
	}
	return rp, nil
}

// addRestorePoint adds rp, stored in dir, to cat, applies the branch's retention, saves the
// catalog and then trashes the directories of the restore points that expired.
func (v *Volume) addRestorePoint(cat catalog, rp agentapi.RestorePoint, dir string) error {
	cat.RestorePoints = append(cat.RestorePoints, catalogEntry{RestorePoint: rp, Path: v.rel(dir)})
	gone := v.retain(&cat, rp.Branch)
	if err := saveCatalog(v.catalogPath(), cat); err != nil {
		return err
	}
	for _, d := range gone {
		v.trashOrWarn(d)
	}
	return nil
}

// unusedRestorePointID returns a new restore point ID that no catalog entry and no directory uses.
func (v *Volume) unusedRestorePointID(cat catalog, branch string, now time.Time) string {
	for {
		id := newRestorePointID(now)
		taken := slices.ContainsFunc(cat.RestorePoints, func(e catalogEntry) bool { return e.ID == id })
		if _, err := os.Lstat(v.restorePointDir(branch, id)); !taken && errors.Is(err, fs.ErrNotExist) {
			return id
		}
	}
}

// retain drops the branch's restore points that the retention policy expires (spec 7.2) from
// cat and returns their directories.
func (v *Volume) retain(cat *catalog, branch string) []string {
	gone := expired(cat.RestorePoints, branch, v.cfg.Now(), v.cfg.RestorePointsPerBranch, v.cfg.RestorePointMaxAge)
	if len(gone) == 0 {
		return nil
	}
	var dirs []string
	for _, e := range gone {
		dirs = append(dirs, filepath.Join(v.root, filepath.FromSlash(e.Path)))
	}
	cat.RestorePoints = slices.DeleteFunc(cat.RestorePoints, func(e catalogEntry) bool {
		return slices.ContainsFunc(gone, func(g catalogEntry) bool { return g.ID == e.ID })
	})
	return dirs
}

// findRestorePoint returns one of the branch's restore points and its directory.
func (v *Volume) findRestorePoint(branch, id string) (agentapi.RestorePoint, string, error) {
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return agentapi.RestorePoint{}, "", err
	}
	for _, e := range cat.RestorePoints {
		if e.ID == id && e.Branch == branch {
			return e.RestorePoint, filepath.Join(v.root, filepath.FromSlash(e.Path)), nil
		}
	}
	return agentapi.RestorePoint{}, "", fmt.Errorf("%w: restore point %q of branch %q", ErrNotFound, id, branch)
}

// ListRestorePoints returns the branch's restore points, newest first.
func (v *Volume) ListRestorePoints(branch string) ([]agentapi.RestorePoint, error) {
	if !agentapi.ValidBranchName(branch) {
		return nil, fmt.Errorf("%w: branch name %q", ErrInvalid, branch)
	}
	if _, err := os.Stat(v.branchDir(branch)); err != nil {
		return nil, fmt.Errorf("%w: branch %q", ErrNotFound, branch)
	}
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return nil, err
	}
	out := []agentapi.RestorePoint{}
	for _, e := range cat.RestorePoints {
		if e.Branch == branch {
			out = append(out, e.RestorePoint)
		}
	}
	slices.SortFunc(out, func(a, b agentapi.RestorePoint) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	return out, nil
}

// DeleteRestorePoint removes one of the branch's restore points.
func (v *Volume) DeleteRestorePoint(ctx context.Context, branch, id string) error {
	if !agentapi.ValidBranchName(branch) || !agentapi.ValidRestorePointID(id) {
		return fmt.Errorf("%w: branch %q, restore point %q", ErrInvalid, branch, id)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return err
	}
	i := slices.IndexFunc(cat.RestorePoints, func(e catalogEntry) bool { return e.ID == id && e.Branch == branch })
	if i < 0 {
		return fmt.Errorf("%w: restore point %q of branch %q", ErrNotFound, id, branch)
	}
	dir := filepath.Join(v.root, filepath.FromSlash(cat.RestorePoints[i].Path))
	cat.RestorePoints = slices.Delete(cat.RestorePoints, i, i+1)
	if err := saveCatalog(v.catalogPath(), cat); err != nil {
		return err
	}
	return v.trash(dir)
}

// ApplyRetention applies the retention policy to every branch's restore points (spec 7.2: hourly).
func (v *Volume) ApplyRetention(ctx context.Context) error {
	release, err := v.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return err
	}
	var branches []string
	for _, e := range cat.RestorePoints {
		if !slices.Contains(branches, e.Branch) {
			branches = append(branches, e.Branch)
		}
	}
	var dirs []string
	for _, b := range branches {
		dirs = append(dirs, v.retain(&cat, b)...)
	}
	if len(dirs) == 0 {
		return nil
	}
	if err := saveCatalog(v.catalogPath(), cat); err != nil {
		return err
	}
	var errs []error
	for _, d := range dirs {
		if err := v.trash(d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RunJanitor empties the trash whenever something is trashed and applies the restore point
// retention every retentionEvery (spec 7.2: hourly), until ctx ends. retentionEvery must be
// positive.
func (v *Volume) RunJanitor(ctx context.Context, retentionEvery time.Duration) {
	if err := v.EmptyTrash(); err != nil {
		slog.Warn("empty trash", "err", err)
	}
	tick := time.NewTicker(retentionEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-v.trashed:
			if err := v.EmptyTrash(); err != nil {
				slog.Warn("empty trash", "err", err)
			}
		case <-tick.C:
			if err := v.ApplyRetention(ctx); err != nil && !errors.Is(err, ErrBusy) {
				slog.Warn("restore point retention", "err", err)
			}
		}
	}
}
