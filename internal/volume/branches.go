package volume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

// CreateBranchData makes the data of a new branch (spec 7.2 POST /v1/branches/{name}/data,
// 9.2): generation data-1 holding a copy of the parent's running database (ParentLatest) or of
// one of the parent's restore points, with current pointing at it. When the branch already has
// data it changes nothing and returns the current generation, so the operator can retry.
func (v *Volume) CreateBranchData(ctx context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error) {
	if !agentapi.ValidBranchName(name) || name == agentapi.MainBranch {
		return agentapi.BranchData{}, fmt.Errorf("%w: branch name %q", ErrInvalid, name)
	}
	if !agentapi.ValidBranchName(req.Parent) || req.Parent == name {
		return agentapi.BranchData{}, fmt.Errorf("%w: parent %q", ErrInvalid, req.Parent)
	}
	switch req.Source.Type {
	case agentapi.SourceParentLatest:
	case agentapi.SourceRestorePoint:
		if !agentapi.ValidRestorePointID(req.Source.RestorePointID) {
			return agentapi.BranchData{}, fmt.Errorf("%w: restore point %q", ErrInvalid, req.Source.RestorePointID)
		}
	default:
		return agentapi.BranchData{}, fmt.Errorf("%w: source type %q", ErrInvalid, req.Source.Type)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.BranchData{}, err
	}
	defer release()

	n, err := v.currentGeneration(name)
	if err == nil {
		return agentapi.BranchData{Generation: n}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return agentapi.BranchData{}, err // never replace a branch whose data could not be read
	}
	var fill func(dst string) (string, error)
	if req.Source.Type == agentapi.SourceRestorePoint {
		// A restore point is copied without touching the parent's server: no interval applies.
		rp, dir, err := v.findRestorePoint(req.Parent, req.Source.RestorePointID)
		if err != nil {
			return agentapi.BranchData{}, err
		}
		fill = func(dst string) (string, error) { return rp.BackupStartLSN, CloneTree(dir, dst, skipMarker) }
	} else {
		if _, err := v.currentGeneration(req.Parent); errors.Is(err, ErrNotFound) {
			return agentapi.BranchData{}, fmt.Errorf("%w: parent %q has no data", ErrNotFound, req.Parent)
		} else if err != nil {
			return agentapi.BranchData{}, err
		}
		if err := v.checkCopyInterval(req.Parent); err != nil {
			return agentapi.BranchData{}, err
		}
		fill = func(dst string) (string, error) {
			return v.snapshot(ctx, req.Parent, dst, "consonance branch "+name)
		}
	}
	if err := v.checkSpace(); err != nil {
		return agentapi.BranchData{}, err
	}
	lsn, err := v.makeFirstGeneration(name, fill)
	if err != nil {
		return agentapi.BranchData{}, err
	}
	return agentapi.BranchData{Generation: 1, BackupStartLSN: lsn}, nil
}

// makeFirstGeneration creates branches/<name>/data-1, fills it, points current at it and only
// then removes its .incomplete marker, so that Recover removes the branch if the agent stops
// on the way. On failure it removes the branch directory again. fill returns the copy's
// backup start LSN.
func (v *Volume) makeFirstGeneration(name string, fill func(dst string) (string, error)) (string, error) {
	bd := v.branchDir(name)
	if err := v.trash(bd); err != nil { // what an unfinished earlier attempt left
		return "", err
	}
	if err := os.Mkdir(bd, 0o700); err != nil {
		return "", err
	}
	data := v.genDir(name, 1)
	lsn, err := func() (string, error) {
		if err := makeMarked(data); err != nil {
			return "", err
		}
		lsn, err := fill(data)
		if err != nil {
			return "", err
		}
		if err := switchCurrent(bd, 1); err != nil {
			return "", err
		}
		return lsn, finishMarked(data)
	}()
	if err != nil {
		v.trashOrWarn(bd)
		return "", err
	}
	return lsn, nil
}

// DeleteBranchData removes a branch's directory and its restore points (spec 7.2, 9.6). The
// operator deletes the branch's pod first; if its supervisor still answers, PostgreSQL must be
// stopped. main cannot be deleted. A branch without data is deleted already: success.
func (v *Volume) DeleteBranchData(ctx context.Context, name string) error {
	if !agentapi.ValidBranchName(name) {
		return fmt.Errorf("%w: branch name %q", ErrInvalid, name)
	}
	if name == agentapi.MainBranch {
		return fmt.Errorf("%w: main cannot be deleted", ErrConflict)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	st, err := v.status(ctx, name)
	switch {
	case errors.Is(err, ErrSupervisorUnavailable):
		// No answer: the pod is gone, as it should be by now.
	case err != nil:
		return err
	case st.PG != supervisorapi.PGStopped:
		return fmt.Errorf("%w: branch %q is %s", ErrConflict, name, st.PG)
	}
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return err
	}
	kept := slices.DeleteFunc(slices.Clone(cat.RestorePoints), func(e catalogEntry) bool { return e.Branch == name })
	if len(kept) != len(cat.RestorePoints) {
		cat.RestorePoints = kept
		if err := saveCatalog(v.catalogPath(), cat); err != nil {
			return err
		}
	}
	// Catalog first: if the agent stops right after it, Recover removes the restore point
	// directories the catalog no longer lists, and the retried call removes the branch.
	if err := v.trash(v.restoreBranchDir(name)); err != nil {
		return err
	}
	if err := v.trash(v.branchDir(name)); err != nil {
		return err
	}
	v.mu.Lock()
	delete(v.lastCopy, name)
	v.mu.Unlock()
	return nil
}
