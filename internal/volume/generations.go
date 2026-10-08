package volume

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

// ListGenerations returns the branch's current generation and its complete generations.
func (v *Volume) ListGenerations(branch string) (agentapi.Generations, error) {
	if !agentapi.ValidBranchName(branch) {
		return agentapi.Generations{}, fmt.Errorf("%w: branch name %q", ErrInvalid, branch)
	}
	cur, err := v.currentGeneration(branch)
	if err != nil {
		return agentapi.Generations{}, err
	}
	gens, err := v.generations(branch)
	if err != nil {
		return agentapi.Generations{}, err
	}
	return agentapi.Generations{Current: cur, Generations: gens}, nil
}

// CreateGeneration makes data-N+1 without activating it (spec 7.2, 9.4 Preparing): for a
// rollback a copy of one of the branch's restore points, for a reset a backup-mode copy of the
// parent's running database.
func (v *Volume) CreateGeneration(ctx context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error) {
	if !agentapi.ValidBranchName(branch) {
		return agentapi.Generation{}, fmt.Errorf("%w: branch name %q", ErrInvalid, branch)
	}
	switch req.Source.Type {
	case agentapi.SourceRestorePoint:
		if !agentapi.ValidRestorePointID(req.Source.RestorePointID) {
			return agentapi.Generation{}, fmt.Errorf("%w: restore point %q", ErrInvalid, req.Source.RestorePointID)
		}
	case agentapi.SourceParentLatest:
		if !agentapi.ValidBranchName(req.Parent) || req.Parent == branch {
			return agentapi.Generation{}, fmt.Errorf("%w: parent %q", ErrInvalid, req.Parent)
		}
	default:
		return agentapi.Generation{}, fmt.Errorf("%w: source type %q", ErrInvalid, req.Source.Type)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.Generation{}, err
	}
	defer release()

	if _, err := v.currentGeneration(branch); err != nil {
		return agentapi.Generation{}, err
	}
	var fill func(dst string) error
	if req.Source.Type == agentapi.SourceRestorePoint {
		_, dir, err := v.findRestorePoint(branch, req.Source.RestorePointID)
		if err != nil {
			return agentapi.Generation{}, err
		}
		fill = func(dst string) error { return CloneTree(dir, dst, skipMarker) }
	} else {
		if _, err := v.currentGeneration(req.Parent); errors.Is(err, ErrNotFound) {
			return agentapi.Generation{}, fmt.Errorf("%w: parent %q has no data", ErrNotFound, req.Parent)
		} else if err != nil {
			return agentapi.Generation{}, err
		}
		if err := v.checkCopyInterval(req.Parent); err != nil {
			return agentapi.Generation{}, err
		}
		fill = func(dst string) error {
			_, err := v.snapshot(ctx, req.Parent, dst, "consonance reset "+branch)
			return err
		}
	}
	if err := v.checkSpace(); err != nil {
		return agentapi.Generation{}, err
	}
	n, err := v.nextGeneration(branch)
	if err != nil {
		return agentapi.Generation{}, err
	}
	dir := v.genDir(branch, n)
	if err := makeMarked(dir); err != nil {
		return agentapi.Generation{}, err
	}
	err = fill(dir)
	if err == nil {
		err = finishMarked(dir)
	}
	if err != nil {
		v.trashOrWarn(dir)
		return agentapi.Generation{}, err
	}
	return agentapi.Generation{Generation: n}, nil
}

// ActivateGeneration points the branch's current link at data-n (spec 7.2, 9.4 Activating). The
// branch's PostgreSQL must be stopped, which its supervisor is asked to confirm.
func (v *Volume) ActivateGeneration(ctx context.Context, branch string, n int) (agentapi.Generations, error) {
	if !agentapi.ValidBranchName(branch) || n < 1 {
		return agentapi.Generations{}, fmt.Errorf("%w: branch %q, generation %d", ErrInvalid, branch, n)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.Generations{}, err
	}
	defer release()

	cur, err := v.currentGeneration(branch)
	if err != nil {
		return agentapi.Generations{}, err
	}
	if !complete(v.genDir(branch, n)) {
		return agentapi.Generations{}, fmt.Errorf("%w: generation %d of branch %q", ErrNotFound, n, branch)
	}
	if cur != n {
		st, err := v.status(ctx, branch)
		if err != nil {
			return agentapi.Generations{}, err
		}
		if st.PG != supervisorapi.PGStopped {
			return agentapi.Generations{}, fmt.Errorf("%w: branch %q is %s; stop it before switching generations", ErrConflict, branch, st.PG)
		}
		if err := switchCurrent(v.branchDir(branch), n); err != nil {
			return agentapi.Generations{}, err
		}
	}
	gens, err := v.generations(branch)
	if err != nil {
		return agentapi.Generations{}, err
	}
	return agentapi.Generations{Current: n, Generations: gens}, nil
}

// DetachGeneration moves data-n out of the branch (spec 7.2, 9.4 Detaching): into
// branches/<toBranch>/data-1 as a new branch (a rollback's backup branch) or into a restore
// point with reason "reset". data-n must not be current, and the branch's PostgreSQL must not
// be running it. A retried detach that was completed answers like the first call: with the
// branch, or with the restore point the reset made.
func (v *Volume) DetachGeneration(ctx context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error) {
	if !agentapi.ValidBranchName(branch) || n < 1 {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: branch %q, generation %d", ErrInvalid, branch, n)
	}
	if (req.ToBranch != "") == req.AsRestorePoint {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: give exactly one of toBranch and asRestorePoint", ErrInvalid)
	}
	if req.ToBranch != "" && (!agentapi.ValidBranchName(req.ToBranch) || req.ToBranch == branch || req.ToBranch == agentapi.MainBranch) {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: target branch %q", ErrInvalid, req.ToBranch)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.DetachResponse{}, err
	}
	defer release()

	src := v.genDir(branch, n)
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		// data-n is gone: an earlier call may have moved it and its answer got lost.
		if req.ToBranch != "" {
			if _, err := v.currentGeneration(req.ToBranch); err == nil {
				return agentapi.DetachResponse{Branch: req.ToBranch}, nil // done by an earlier call
			}
		} else {
			rp, found, err := v.resetRestorePoint(branch, n)
			if err != nil {
				return agentapi.DetachResponse{}, err
			}
			if found {
				return agentapi.DetachResponse{RestorePoint: &rp}, nil // done by an earlier call
			}
		}
	}
	if !complete(src) {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: generation %d of branch %q", ErrNotFound, n, branch)
	}
	cur, err := v.currentGeneration(branch)
	if err != nil {
		return agentapi.DetachResponse{}, err
	}
	if cur == n {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: generation %d is branch %q's current one", ErrConflict, n, branch)
	}
	if err := v.checkNotRunning(ctx, branch, n); err != nil {
		return agentapi.DetachResponse{}, err
	}
	if req.ToBranch != "" {
		return v.detachToBranch(branch, n, req.ToBranch)
	}
	return v.detachToRestorePoint(branch, n)
}

// checkNotRunning fails when the branch's PostgreSQL runs data-n. A server started before
// current was switched still runs the old generation.
func (v *Volume) checkNotRunning(ctx context.Context, branch string, n int) error {
	st, err := v.status(ctx, branch)
	if err != nil {
		return err
	}
	if st.PG != supervisorapi.PGStopped && st.Generation == generationName(n) {
		return fmt.Errorf("%w: branch %q is %s on generation %d", ErrConflict, branch, st.PG, n)
	}
	return nil
}

func (v *Volume) detachToBranch(branch string, n int, to string) (agentapi.DetachResponse, error) {
	_, err := v.currentGeneration(to)
	if err == nil {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: branch %q exists", ErrConflict, to)
	}
	if !errors.Is(err, ErrNotFound) {
		return agentapi.DetachResponse{}, err // never trash a target whose data could not be read
	}
	td := v.branchDir(to)
	if err := v.trash(td); err != nil { // half made by an earlier call
		return agentapi.DetachResponse{}, err
	}
	if err := os.Mkdir(td, 0o700); err != nil {
		return agentapi.DetachResponse{}, err
	}
	// Link first, then move: if the agent stops in between, Recover removes the branch with the
	// dangling link and the generation is still in its old place. The other order would leave
	// the data in a branch without a link, which Recover deletes.
	if err := switchCurrent(td, 1); err != nil {
		v.trashOrWarn(td)
		return agentapi.DetachResponse{}, err
	}
	if err := os.Rename(v.genDir(branch, n), v.genDir(to, 1)); err != nil {
		v.trashOrWarn(td)
		return agentapi.DetachResponse{}, err
	}
	if err := syncDir(td); err != nil {
		return agentapi.DetachResponse{}, err
	}
	if err := syncDir(v.branchDir(branch)); err != nil {
		return agentapi.DetachResponse{}, err
	}
	return agentapi.DetachResponse{Branch: to}, nil
}

func (v *Volume) detachToRestorePoint(branch string, n int) (agentapi.DetachResponse, error) {
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return agentapi.DetachResponse{}, err
	}
	now := v.cfg.Now()
	id, err := v.unusedRestorePointID(cat, branch, now)
	if err != nil {
		return agentapi.DetachResponse{}, err
	}
	if err := os.MkdirAll(v.restoreBranchDir(branch), 0o700); err != nil {
		return agentapi.DetachResponse{}, err
	}
	dir := v.restorePointDir(branch, id)
	rp := agentapi.RestorePoint{
		ID: id, Branch: branch, CreatedAt: now.UTC().Truncate(time.Second),
		Reason: agentapi.ReasonReset, Label: resetLabel(n),
	}
	// Record first, then move: if the agent stops in between, Recover drops the entry whose
	// directory is missing and the generation is still in its branch. The other order would
	// leave a finished directory the catalog does not list, which Recover removes. Retention
	// waits for the move.
	cat.RestorePoints = append(cat.RestorePoints, catalogEntry{RestorePoint: rp, Path: v.rel(dir)})
	if err := saveCatalog(v.catalogPath(), cat); err != nil {
		return agentapi.DetachResponse{}, err
	}
	if err := os.Rename(v.genDir(branch, n), dir); err != nil {
		v.forgetRestorePoint(id)
		return agentapi.DetachResponse{}, err
	}
	if err := syncDir(v.restoreBranchDir(branch)); err != nil {
		return agentapi.DetachResponse{}, err
	}
	if err := syncDir(v.branchDir(branch)); err != nil {
		return agentapi.DetachResponse{}, err
	}
	// Retention runs only after the move, so a failed or interrupted move never costs an older
	// restore point; if it fails here, the hourly pass applies it.
	if gone := v.retain(&cat, branch); len(gone) > 0 {
		if err := saveCatalog(v.catalogPath(), cat); err != nil {
			slog.Warn("restore point retention after a reset", "branch", branch, "err", err)
		} else {
			for _, d := range gone {
				v.trashOrWarn(d)
			}
		}
	}
	return agentapi.DetachResponse{RestorePoint: &rp}, nil
}

// resetLabel is the label of the restore point a reset makes of the branch's data-n.
func resetLabel(n int) string { return "before reset (" + generationName(n) + ")" }

// resetRestorePoint finds the restore point an earlier reset made of the branch's data-n: the
// newest entry with reason "reset" and that generation's label.
func (v *Volume) resetRestorePoint(branch string, n int) (agentapi.RestorePoint, bool, error) {
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return agentapi.RestorePoint{}, false, err
	}
	var found *agentapi.RestorePoint
	for _, e := range cat.RestorePoints {
		if e.Branch == branch && e.Reason == agentapi.ReasonReset && e.Label == resetLabel(n) {
			if found == nil || e.CreatedAt.After(found.CreatedAt) {
				rp := e.RestorePoint
				found = &rp
			}
		}
	}
	if found == nil {
		return agentapi.RestorePoint{}, false, nil
	}
	return *found, true, nil
}

// forgetRestorePoint removes the entry id from the catalog again. A failure is only logged:
// Recover drops an entry whose directory is missing at the next start.
func (v *Volume) forgetRestorePoint(id string) {
	cat, err := loadCatalog(v.catalogPath())
	if err == nil {
		cat.RestorePoints = slices.DeleteFunc(cat.RestorePoints, func(e catalogEntry) bool { return e.ID == id })
		err = saveCatalog(v.catalogPath(), cat)
	}
	if err != nil {
		slog.Warn("remove a restore point entry", "id", id, "err", err)
	}
}

// DeleteGeneration removes data-n, for instance the new generation of a rollback whose start
// failed. It must be neither current nor running.
func (v *Volume) DeleteGeneration(ctx context.Context, branch string, n int) error {
	if !agentapi.ValidBranchName(branch) || n < 1 {
		return fmt.Errorf("%w: branch %q, generation %d", ErrInvalid, branch, n)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	if _, err := os.Lstat(v.genDir(branch, n)); err != nil {
		return fmt.Errorf("%w: generation %d of branch %q", ErrNotFound, n, branch)
	}
	cur, err := v.currentGeneration(branch)
	if err != nil {
		return err
	}
	if cur == n {
		return fmt.Errorf("%w: generation %d is branch %q's current one", ErrConflict, n, branch)
	}
	if err := v.checkNotRunning(ctx, branch, n); err != nil {
		return err
	}
	return v.trash(v.genDir(branch, n))
}
