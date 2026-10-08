package volume_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
	"consonance/internal/volume"
)

func TestListGenerations(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, false)
	mkGeneration(t, root, "main", 2, true)
	v := openVolume(t, root, nil, nil)
	got, err := v.ListGenerations("main")
	if err != nil || got.Current != 2 || !slices.Equal(got.Generations, []int{1, 2}) {
		t.Fatalf("ListGenerations = %+v, %v", got, err)
	}
	if _, err := v.ListGenerations("dev"); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("unknown branch: %v, want ErrNotFound", err)
	}
	if _, err := v.ListGenerations("../x"); !errors.Is(err, volume.ErrInvalid) {
		t.Fatalf("bad name: %v, want ErrInvalid", err)
	}
}

func TestCreateGenerationValidates(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	mkGeneration(t, root, "dev", 1, true)
	writeCatalog(t, root, rpFixture{id: "rp-20261007T031500Z-ab12", branch: "main", reason: "api", createdAt: newClock().Now()})
	v := openVolume(t, root, nil, nil)
	ctx := testCtx(t)
	rpOf := func(id string) agentapi.Source {
		return agentapi.Source{Type: agentapi.SourceRestorePoint, RestorePointID: id}
	}
	latest := agentapi.Source{Type: agentapi.SourceParentLatest}
	for _, tc := range []struct {
		branch string
		req    agentapi.CreateGenerationRequest
		want   error
	}{
		{"dev", agentapi.CreateGenerationRequest{Source: agentapi.Source{Type: "Snapshot"}}, volume.ErrInvalid},
		{"dev", agentapi.CreateGenerationRequest{Source: rpOf("bad")}, volume.ErrInvalid},
		{"dev", agentapi.CreateGenerationRequest{Source: latest}, volume.ErrInvalid}, // a reset needs the parent
		{"dev", agentapi.CreateGenerationRequest{Parent: "dev", Source: latest}, volume.ErrInvalid},
		{"dev", agentapi.CreateGenerationRequest{Source: rpOf("rp-20261007T031500Z-ab12")}, volume.ErrNotFound}, // main's, not dev's
		{"gone", agentapi.CreateGenerationRequest{Source: rpOf("rp-20261007T031500Z-ab12")}, volume.ErrNotFound},
		{"dev", agentapi.CreateGenerationRequest{Parent: "nobody", Source: latest}, volume.ErrNotFound},
	} {
		if _, err := v.CreateGeneration(ctx, tc.branch, tc.req); !errors.Is(err, tc.want) {
			t.Errorf("CreateGeneration(%q, %+v) = %v, want %v", tc.branch, tc.req, err, tc.want)
		}
	}
}

func TestCreateGenerationStopsAtAParentItCannotRead(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "dev", 1, true)
	// main's current is a real directory: whether main holds data cannot be judged.
	writeFile(t, filepath.Join(root, "branches", "main", "current", "keep"), "x", 0o600)
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{running("data-1")}}}, nil)
	_, err := v.CreateGeneration(testCtx(t), "dev", agentapi.CreateGenerationRequest{
		Parent: "main", Source: agentapi.Source{Type: agentapi.SourceParentLatest},
	})
	if err == nil || errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("CreateGeneration = %v, want the read error, not ErrNotFound", err)
	}
	if gens, err := v.ListGenerations("dev"); err != nil || !slices.Equal(gens.Generations, []int{1}) {
		t.Fatalf("dev generations = %+v, %v; want [1]", gens, err)
	}
}

func TestFailedGenerationIsRemoved(t *testing.T) {
	// No XFS: the clone fails.
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	writeCatalog(t, root, rpFixture{id: "rp-20261007T031500Z-ab12", branch: "main", reason: "api", createdAt: newClock().Now()})
	v := openVolume(t, root, nil, nil)
	_, err := v.CreateGeneration(testCtx(t), "main", agentapi.CreateGenerationRequest{
		Source: agentapi.Source{Type: agentapi.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-ab12"},
	})
	if !errors.Is(err, volume.ErrReflinkUnsupported) {
		t.Fatalf("got %v, want ErrReflinkUnsupported", err)
	}
	if exists(filepath.Join(root, "branches", "main", "data-2")) {
		t.Fatal("the failed generation was left behind")
	}
}

func TestActivateGeneration(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	mkGeneration(t, root, "main", 2, false)
	main := &fakeSupervisor{statuses: []supervisorapi.Status{running("data-1")}}
	v := openVolume(t, root, supervisors{"main": main}, nil)
	ctx := testCtx(t)
	current := func() string {
		target, _ := os.Readlink(filepath.Join(root, "branches", "main", "current"))
		return target
	}
	if _, err := v.ActivateGeneration(ctx, "main", 2); !errors.Is(err, volume.ErrConflict) {
		t.Fatalf("activate while running = %v, want ErrConflict", err)
	}
	main.set(stopped("data-1"))
	got, err := v.ActivateGeneration(ctx, "main", 2)
	if err != nil || got.Current != 2 || !slices.Equal(got.Generations, []int{1, 2}) {
		t.Fatalf("ActivateGeneration = %+v, %v", got, err)
	}
	if current() != "data-2" {
		t.Fatalf("current = %q, want data-2", current())
	}
	main.set(running("data-2"))
	// Already current: nothing to ask and nothing to change.
	if got, err := v.ActivateGeneration(ctx, "main", 2); err != nil || got.Current != 2 {
		t.Fatalf("activating the current generation = %+v, %v", got, err)
	}
	if _, err := v.ActivateGeneration(ctx, "main", 3); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("missing generation: %v, want ErrNotFound", err)
	}
	if _, err := v.ActivateGeneration(ctx, "main", 0); !errors.Is(err, volume.ErrInvalid) {
		t.Fatalf("generation 0: %v, want ErrInvalid", err)
	}
	// Without an answer from the supervisor nothing is switched.
	noPod := openVolume(t, root, nil, nil)
	if _, err := noPod.ActivateGeneration(ctx, "main", 1); !errors.Is(err, volume.ErrSupervisorUnavailable) {
		t.Fatalf("without a supervisor: %v, want ErrSupervisorUnavailable", err)
	}
	if current() != "data-2" {
		t.Fatalf("current = %q after a refused activation", current())
	}
}

func TestDetachToBranch(t *testing.T) {
	root := t.TempDir()
	old := mkGeneration(t, root, "main", 1, false)
	mkGeneration(t, root, "main", 2, true)
	writeFile(t, filepath.Join(old, "marker"), "old", 0o600)
	main := &fakeSupervisor{statuses: []supervisorapi.Status{running("data-2")}} // running the new generation is fine
	v := openVolume(t, root, supervisors{"main": main}, nil)
	ctx := testCtx(t)
	req := agentapi.DetachRequest{ToBranch: "main-before-20261007031500"}
	got, err := v.DetachGeneration(ctx, "main", 1, req)
	if err != nil || got.Branch != req.ToBranch || got.RestorePoint != nil {
		t.Fatalf("DetachGeneration = %+v, %v", got, err)
	}
	backup := filepath.Join(root, "branches", req.ToBranch)
	if target, err := os.Readlink(filepath.Join(backup, "current")); err != nil || target != "data-1" {
		t.Fatalf("backup branch current = %q, %v", target, err)
	}
	if got := readFile(t, filepath.Join(backup, "data-1", "marker")); got != "old" {
		t.Fatalf("backup branch holds %q, want the old generation", got)
	}
	if exists(old) {
		t.Fatal("the old generation is still in main")
	}
	// A retried call reports the same result.
	if again, err := v.DetachGeneration(ctx, "main", 1, req); err != nil || again.Branch != req.ToBranch {
		t.Fatalf("retried DetachGeneration = %+v, %v", again, err)
	}
	if gens, err := v.ListGenerations("main"); err != nil || !slices.Equal(gens.Generations, []int{2}) {
		t.Fatalf("main generations = %+v, %v", gens, err)
	}
}

func TestDetachRefusesRunningGeneration(t *testing.T) {
	root := t.TempDir()
	old := mkGeneration(t, root, "main", 1, false)
	mkGeneration(t, root, "main", 2, true)
	// current already points at data-2, but the server still runs data-1 (started before the switch).
	main := &fakeSupervisor{statuses: []supervisorapi.Status{running("data-1")}}
	v := openVolume(t, root, supervisors{"main": main}, nil)
	ctx := testCtx(t)
	for _, req := range []agentapi.DetachRequest{{ToBranch: "main-before-20261007031500"}, {AsRestorePoint: true}} {
		if _, err := v.DetachGeneration(ctx, "main", 1, req); !errors.Is(err, volume.ErrConflict) {
			t.Errorf("DetachGeneration(%+v) = %v, want ErrConflict", req, err)
		}
	}
	if err := v.DeleteGeneration(ctx, "main", 1); !errors.Is(err, volume.ErrConflict) {
		t.Errorf("DeleteGeneration = %v, want ErrConflict", err)
	}
	if !exists(filepath.Join(old, "PG_VERSION")) || exists(filepath.Join(root, "branches", "main-before-20261007031500")) {
		t.Fatal("a running generation was moved")
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "restore")); len(entries) != 0 {
		t.Fatalf("restore has %v", entries)
	}
}

func TestDetachValidates(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, false)
	mkGeneration(t, root, "main", 2, true)
	mkGeneration(t, root, "taken", 1, true)
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{stopped("data-2")}}}, nil)
	ctx := testCtx(t)
	for _, req := range []agentapi.DetachRequest{{}, {ToBranch: "x", AsRestorePoint: true}, {ToBranch: "main"}, {ToBranch: "Bad"}} {
		if _, err := v.DetachGeneration(ctx, "main", 1, req); !errors.Is(err, volume.ErrInvalid) {
			t.Errorf("DetachGeneration(%+v) = %v, want ErrInvalid", req, err)
		}
	}
	if _, err := v.DetachGeneration(ctx, "main", 2, agentapi.DetachRequest{ToBranch: "x"}); !errors.Is(err, volume.ErrConflict) {
		t.Errorf("detaching the current generation: %v, want ErrConflict", err)
	}
	if _, err := v.DetachGeneration(ctx, "main", 7, agentapi.DetachRequest{ToBranch: "x"}); !errors.Is(err, volume.ErrNotFound) {
		t.Errorf("missing generation: %v, want ErrNotFound", err)
	}
	if _, err := v.DetachGeneration(ctx, "main", 1, agentapi.DetachRequest{ToBranch: "taken"}); !errors.Is(err, volume.ErrConflict) {
		t.Errorf("existing target branch: %v, want ErrConflict", err)
	}
}

func TestDetachKeepsATargetItCannotRead(t *testing.T) {
	root := t.TempDir()
	old := mkGeneration(t, root, "main", 1, false)
	mkGeneration(t, root, "main", 2, true)
	// The target's current is a real directory: whether the target holds data cannot be judged.
	writeFile(t, filepath.Join(root, "branches", "taken", "current", "keep"), "x", 0o600)
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{stopped("data-2")}}}, nil)
	_, err := v.DetachGeneration(testCtx(t), "main", 1, agentapi.DetachRequest{ToBranch: "taken"})
	if err == nil || errors.Is(err, volume.ErrNotFound) || errors.Is(err, volume.ErrConflict) {
		t.Fatalf("DetachGeneration = %v, want the read error", err)
	}
	if !exists(filepath.Join(root, "branches", "taken", "current", "keep")) || !exists(filepath.Join(old, "PG_VERSION")) {
		t.Fatal("DetachGeneration touched data it could not judge")
	}
}

func TestDetachAsRestorePoint(t *testing.T) {
	root := t.TempDir()
	old := mkGeneration(t, root, "dev", 1, false)
	mkGeneration(t, root, "dev", 2, true)
	clock := newClock()
	dev := &fakeSupervisor{statuses: []supervisorapi.Status{running("data-2")}}
	v := openVolume(t, root, supervisors{"dev": dev}, func(c *volume.Config) { c.Now = clock.Now })
	got, err := v.DetachGeneration(testCtx(t), "dev", 1, agentapi.DetachRequest{AsRestorePoint: true})
	if err != nil || got.RestorePoint == nil || got.Branch != "" {
		t.Fatalf("DetachGeneration = %+v, %v", got, err)
	}
	rp := *got.RestorePoint
	if rp.Branch != "dev" || rp.Reason != agentapi.ReasonReset || rp.Label != "before reset (data-1)" ||
		!rp.CreatedAt.Equal(clock.Now()) || !agentapi.ValidRestorePointID(rp.ID) {
		t.Fatalf("restore point %+v", rp)
	}
	if exists(old) || !exists(filepath.Join(root, "restore", "dev", rp.ID, "PG_VERSION")) {
		t.Fatal("the generation was not moved into the restore point")
	}
	if list, err := v.ListRestorePoints("dev"); err != nil || len(list) != 1 || list[0].ID != rp.ID {
		t.Fatalf("ListRestorePoints = %+v, %v", list, err)
	}
}

func TestDetachAsRestorePointKeepsTheGenerationWhenTheMoveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	old := mkGeneration(t, root, "dev", 1, false)
	mkGeneration(t, root, "dev", 2, true)
	rdir := filepath.Join(root, "restore", "dev")
	if err := os.MkdirAll(rdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rdir, 0o500); err != nil { // nothing can be moved in
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(rdir, 0o700) })
	v := openVolume(t, root, supervisors{"dev": {statuses: []supervisorapi.Status{running("data-2")}}}, nil)
	if _, err := v.DetachGeneration(testCtx(t), "dev", 1, agentapi.DetachRequest{AsRestorePoint: true}); err == nil {
		t.Fatal("DetachGeneration succeeded although the move failed")
	}
	if !exists(filepath.Join(old, "PG_VERSION")) {
		t.Fatal("the generation left its branch")
	}
	if list, err := v.ListRestorePoints("dev"); err != nil || len(list) != 0 {
		t.Fatalf("restore points = %+v, %v; want none (the entry is removed again)", list, err)
	}
}

// seedRestorePoints writes a catalog of n "api" restore points of the branch, one every three
// minutes back from the clock, and returns their IDs, newest first.
func seedRestorePoints(t *testing.T, root, branch string, clock *testClock, n int) []string {
	t.Helper()
	var fixtures []rpFixture
	var ids []string
	for i := range n {
		at := clock.Now().Add(-time.Duration(3*(i+1)) * time.Minute)
		id := fmt.Sprintf("rp-%s-a%03d", at.Format("20060102T150405Z"), i)
		fixtures = append(fixtures, rpFixture{id: id, branch: branch, reason: "api", createdAt: at})
		ids = append(ids, id)
	}
	writeCatalog(t, root, fixtures...)
	return ids
}

// restorePointIDs returns the IDs of the branch's restore points, sorted.
func restorePointIDs(t *testing.T, v *volume.Volume, branch string) []string {
	t.Helper()
	list, err := v.ListRestorePoints(branch)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, rp := range list {
		ids = append(ids, rp.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestDetachAsRestorePointKeepsOlderRestorePointsWhenTheMoveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	const keep = 20
	root := t.TempDir()
	old := mkGeneration(t, root, "dev", 1, false)
	mkGeneration(t, root, "dev", 2, true)
	clock := newClock()
	// dev holds as many restore points as the retention keeps: a 21st would push the oldest out.
	seeded := seedRestorePoints(t, root, "dev", clock, keep)
	rdir := filepath.Join(root, "restore", "dev")
	if err := os.Chmod(rdir, 0o500); err != nil { // nothing can be moved in
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(rdir, 0o700) })
	v := openVolume(t, root, supervisors{"dev": {statuses: []supervisorapi.Status{running("data-2")}}}, func(c *volume.Config) {
		c.Now = clock.Now
		c.RestorePointsPerBranch = keep
	})
	if _, err := v.DetachGeneration(testCtx(t), "dev", 1, agentapi.DetachRequest{AsRestorePoint: true}); err == nil {
		t.Fatal("DetachGeneration succeeded although the move failed")
	}
	if !exists(filepath.Join(old, "PG_VERSION")) {
		t.Fatal("the generation left its branch")
	}
	if got, want := restorePointIDs(t, v, "dev"), slices.Sorted(slices.Values(seeded)); !slices.Equal(got, want) {
		t.Fatalf("restore points = %v; want the %d that were there: a detach that did not happen must not push one out", got, keep)
	}
}

func TestDetachAsRestorePointAppliesRetentionAfterTheMove(t *testing.T) {
	const keep = 20
	root := t.TempDir()
	mkGeneration(t, root, "dev", 1, false)
	mkGeneration(t, root, "dev", 2, true)
	clock := newClock()
	seeded := seedRestorePoints(t, root, "dev", clock, keep)
	v := openVolume(t, root, supervisors{"dev": {statuses: []supervisorapi.Status{running("data-2")}}}, func(c *volume.Config) {
		c.Now = clock.Now
		c.RestorePointsPerBranch = keep
	})
	got, err := v.DetachGeneration(testCtx(t), "dev", 1, agentapi.DetachRequest{AsRestorePoint: true})
	if err != nil || got.RestorePoint == nil {
		t.Fatalf("DetachGeneration = %+v, %v", got, err)
	}
	// The reset is the 21st restore point: the oldest goes, the rest and the reset stay.
	oldest := seeded[keep-1]
	want := slices.Sorted(slices.Values(append(slices.Clone(seeded[:keep-1]), got.RestorePoint.ID)))
	if ids := restorePointIDs(t, v, "dev"); !slices.Equal(ids, want) {
		t.Fatalf("restore points = %v, want the reset's and the %d newest older ones", ids, keep-1)
	}
	if exists(filepath.Join(root, "restore", "dev", oldest)) {
		t.Fatal("the oldest restore point's directory was kept")
	}
	if !exists(filepath.Join(root, "restore", "dev", got.RestorePoint.ID, "PG_VERSION")) {
		t.Fatal("the generation was not moved into the restore point")
	}
}

func TestDetachAsRestorePointIsSafeToRetry(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "dev", 1, false)
	mkGeneration(t, root, "dev", 2, true)
	v := openVolume(t, root, supervisors{"dev": {statuses: []supervisorapi.Status{running("data-2")}}}, nil)
	ctx := testCtx(t)
	req := agentapi.DetachRequest{AsRestorePoint: true}
	first, err := v.DetachGeneration(ctx, "dev", 1, req)
	if err != nil || first.RestorePoint == nil {
		t.Fatalf("DetachGeneration = %+v, %v", first, err)
	}
	// The response was lost and the operator asks again: the same restore point comes back.
	again, err := v.DetachGeneration(ctx, "dev", 1, req)
	if err != nil || again.RestorePoint == nil || again.RestorePoint.ID != first.RestorePoint.ID {
		t.Fatalf("retried DetachGeneration = %+v, %v; want restore point %s", again, err, first.RestorePoint.ID)
	}
	if list, err := v.ListRestorePoints("dev"); err != nil || len(list) != 1 {
		t.Fatalf("restore points = %+v, %v; want exactly one", list, err)
	}
	// Only a generation that was reset is answered: another number is still not found.
	if _, err := v.DetachGeneration(ctx, "dev", 3, req); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("a generation that never existed: %v, want ErrNotFound", err)
	}
}

func TestDetachAsRestorePointRetryAnswersOnlyForItsOwnReset(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, false)
	mkGeneration(t, root, "main", 2, true)
	mkGeneration(t, root, "dev", 2, true) // dev never had a data-1
	clock := newClock()
	sups := supervisors{
		"main": {statuses: []supervisorapi.Status{running("data-2")}},
		"dev":  {statuses: []supervisorapi.Status{running("data-2")}},
	}
	v := openVolume(t, root, sups, func(c *volume.Config) { c.Now = clock.Now })
	ctx := testCtx(t)
	req := agentapi.DetachRequest{AsRestorePoint: true}
	first, err := v.DetachGeneration(ctx, "main", 1, req)
	if err != nil || first.RestorePoint == nil {
		t.Fatalf("DetachGeneration(main) = %+v, %v", first, err)
	}
	// main's restore point has the label a reset of dev's data-1 would have, but it is main's.
	if _, err := v.DetachGeneration(ctx, "dev", 1, req); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("dev's data-1 was never reset: %v, want ErrNotFound", err)
	}
	// A generation number can come back once its directory was moved away; a retry then
	// belongs to the newest reset of it.
	clock.Advance(time.Minute)
	mkGeneration(t, root, "main", 1, false)
	second, err := v.DetachGeneration(ctx, "main", 1, req)
	if err != nil || second.RestorePoint == nil || second.RestorePoint.ID == first.RestorePoint.ID {
		t.Fatalf("second DetachGeneration(main) = %+v, %v", second, err)
	}
	if again, err := v.DetachGeneration(ctx, "main", 1, req); err != nil || again.RestorePoint == nil || again.RestorePoint.ID != second.RestorePoint.ID {
		t.Fatalf("retried DetachGeneration = %+v, %v; want the newest restore point %s", again, err, second.RestorePoint.ID)
	}
}

func TestDeleteGeneration(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	failed := mkGeneration(t, root, "main", 2, false)
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{stopped("data-1")}}}, nil)
	ctx := testCtx(t)
	if err := v.DeleteGeneration(ctx, "main", 1); !errors.Is(err, volume.ErrConflict) {
		t.Fatalf("deleting the current generation: %v, want ErrConflict", err)
	}
	if err := v.DeleteGeneration(ctx, "main", 2); err != nil {
		t.Fatalf("DeleteGeneration: %v", err)
	}
	if exists(failed) {
		t.Fatal("the generation is still there")
	}
	if err := v.DeleteGeneration(ctx, "main", 2); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
}

func TestDeleteBranchData(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	mkGeneration(t, root, "dev", 1, true)
	now := newClock().Now()
	writeCatalog(t, root,
		rpFixture{id: "rp-20261007T031500Z-dev1", branch: "dev", reason: "api", createdAt: now},
		rpFixture{id: "rp-20261007T031500Z-mai1", branch: "main", reason: "api", createdAt: now},
	)
	sups := supervisors{"dev": {statuses: []supervisorapi.Status{running("data-1")}}}
	v := openVolume(t, root, sups, nil)
	ctx := testCtx(t)
	if err := v.DeleteBranchData(ctx, "dev"); !errors.Is(err, volume.ErrConflict) {
		t.Fatalf("deleting a running branch: %v, want ErrConflict", err)
	}
	delete(sups, "dev") // the operator deleted the pod: no answer any more
	if err := v.DeleteBranchData(ctx, "dev"); err != nil {
		t.Fatalf("DeleteBranchData: %v", err)
	}
	if exists(filepath.Join(root, "branches", "dev")) || exists(filepath.Join(root, "restore", "dev")) {
		t.Fatal("dev's data or restore points are still there")
	}
	if list, err := v.ListRestorePoints("main"); err != nil || len(list) != 1 {
		t.Fatalf("main's restore points = %+v, %v; want them kept", list, err)
	}
	if strings.Contains(readFile(t, filepath.Join(root, ".consonance", "catalog.json")), "dev1") {
		t.Fatal("the catalog still lists dev's restore point")
	}
	if err := v.DeleteBranchData(ctx, "dev"); err != nil {
		t.Fatalf("deleting again: %v, want success", err)
	}
	if err := v.DeleteBranchData(ctx, "main"); !errors.Is(err, volume.ErrConflict) {
		t.Fatalf("deleting main: %v, want ErrConflict", err)
	}
}

func TestRollbackToRestorePoint(t *testing.T) {
	e := newPGEnv(t, nil)
	ctx := testCtx(t)
	e.exec(t, "main", "CREATE TABLE t (id int); INSERT INTO t SELECT generate_series(1, 1000)")
	rp, err := e.vol.CreateRestorePoint(ctx, "main", agentapi.CreateRestorePointRequest{Label: "before drop"})
	if err != nil {
		t.Fatal(err)
	}
	e.exec(t, "main", "DROP TABLE t")

	// Spec 9.4: Preparing, Stopping, Activating, Starting, Detaching.
	gen, err := e.vol.CreateGeneration(ctx, "main", agentapi.CreateGenerationRequest{
		Source: agentapi.Source{Type: agentapi.SourceRestorePoint, RestorePointID: rp.ID},
	})
	if err != nil || gen.Generation != 2 {
		t.Fatalf("CreateGeneration = %+v, %v", gen, err)
	}
	if _, err := e.vol.ActivateGeneration(ctx, "main", 2); !errors.Is(err, volume.ErrConflict) {
		t.Fatalf("activate while main runs = %v, want ErrConflict", err)
	}
	sup := e.client("main")
	if _, err := sup.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.vol.ActivateGeneration(ctx, "main", 2); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}
	if st, err := sup.Start(ctx); err != nil || st.Generation != "data-2" {
		t.Fatalf("start = %+v, %v", st, err)
	}
	if n := e.count(t, "main"); n != 1000 {
		t.Fatalf("main has %d rows after the rollback, want 1000", n)
	}
	backup := "main-before-20261007031500"
	if got, err := e.vol.DetachGeneration(ctx, "main", 1, agentapi.DetachRequest{ToBranch: backup}); err != nil || got.Branch != backup {
		t.Fatalf("DetachGeneration = %+v, %v", got, err)
	}
	if gens, err := e.vol.ListGenerations("main"); err != nil || gens.Current != 2 || !slices.Equal(gens.Generations, []int{2}) {
		t.Fatalf("main generations = %+v, %v", gens, err)
	}
	// The backup branch holds main as it was just before the rollback: without t.
	e.start(t, backup)
	if e.queryBool(t, backup, "SELECT to_regclass('t') IS NOT NULL") {
		t.Fatal("the backup branch has table t")
	}
}

func TestResetFromParent(t *testing.T) {
	e := newPGEnv(t, nil)
	ctx := testCtx(t)
	e.exec(t, "main", "CREATE TABLE t (id int); INSERT INTO t SELECT generate_series(1, 10)")
	if _, err := e.vol.CreateBranchData(ctx, "dev", parentLatest("main")); err != nil {
		t.Fatal(err)
	}
	e.start(t, "dev")
	e.exec(t, "dev", "DELETE FROM t")
	e.exec(t, "main", "INSERT INTO t VALUES (11)")

	gen, err := e.vol.CreateGeneration(ctx, "dev", agentapi.CreateGenerationRequest{
		Parent: "main", Source: agentapi.Source{Type: agentapi.SourceParentLatest},
	})
	if err != nil || gen.Generation != 2 {
		t.Fatalf("CreateGeneration = %+v, %v", gen, err)
	}
	sup := e.client("dev")
	if _, err := sup.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.vol.ActivateGeneration(ctx, "dev", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, "dev"); n != 11 {
		t.Fatalf("dev has %d rows after the reset, want 11", n)
	}
	got, err := e.vol.DetachGeneration(ctx, "dev", 1, agentapi.DetachRequest{AsRestorePoint: true})
	if err != nil || got.RestorePoint == nil || got.RestorePoint.Reason != agentapi.ReasonReset {
		t.Fatalf("DetachGeneration = %+v, %v", got, err)
	}
}
