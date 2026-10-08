package volume

import (
	"context"
	"errors"
	"fmt"
	"os"

	"consonance/internal/agentapi"
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
