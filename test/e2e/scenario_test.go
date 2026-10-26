//go:build e2e

package e2e

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"consonance/test/internal/e2eutil"
)

// TestScenario is spec 12's scenario: create a project, branch it, write on the branch while
// main stays as it was, take a restore point, drop the table, roll back, stop and start, delete
// the branch and the project.
func TestScenario(t *testing.T) {
	c := e2eutil.API(t)

	// The project, Ready within five minutes, on the XFS class: k3s's default class local-path
	// would also bind the volume, so a missing class only shows in the PVC.
	id := e2eutil.CreateProject(t, c, "e2e-scenario")
	ns := e2eutil.Namespace(id)
	p := e2eutil.WaitProjectReady(t, c, id, 5*time.Minute)
	if p.Storage.StorageClassName != "consonance-xfs" {
		t.Errorf("project storage class %q, want consonance-xfs", p.Storage.StorageClassName)
	}
	if sc := e2eutil.Kubectl(t, "-n", ns, "get", "pvc", "project-volume", "-o", "jsonpath={.spec.storageClassName}"); sc != "consonance-xfs" {
		t.Errorf("PVC project-volume has storageClassName %q, want consonance-xfs", sc)
	}

	// Branch dev: 201 when the data is ready within five seconds, else 202; then Running.
	var dev e2eutil.Branch
	r := c.Expect(t, http.MethodPost, e2eutil.ProjectPath(id)+"/branches", map[string]any{"name": "dev"}, &dev,
		http.StatusCreated, http.StatusAccepted)
	t.Logf("create branch dev: %d in %s, phase %s", r.Status, r.Elapsed, dev.Phase)
	if dev.Name != "dev" || dev.Parent != "main" {
		t.Fatalf("created branch: %+v", dev)
	}
	e2eutil.WaitBranchPhase(t, c, id, "dev", "Running", 3*time.Minute)

	// Write on dev.
	devConn := e2eutil.GetConnection(t, c, id, "dev")
	if devConn.Host != "br-dev."+ns+".svc" || devConn.Port != 5432 || devConn.User != "app_owner" || devConn.Database != "app" {
		t.Errorf("connection of dev: host %s port %d user %s database %s", devConn.Host, devConn.Port, devConn.User, devConn.Database)
	}
	db := e2eutil.SQL(t, devConn)
	e2eutil.Exec(t, db, `CREATE TABLE items (id int PRIMARY KEY, name text NOT NULL)`)
	e2eutil.Exec(t, db, `INSERT INTO items VALUES (1, 'one'), (2, 'two'), (3, 'three')`)
	want := []string{"one", "two", "three"}
	if got := items(t, db); !slices.Equal(got, want) {
		t.Fatalf("dev items %v, want %v", got, want)
	}

	// main does not have it.
	mainDB := e2eutil.SQL(t, e2eutil.GetConnection(t, c, id, "main"))
	if got := e2eutil.QueryStrings(t, mainDB, `SELECT coalesce(to_regclass('public.items')::text, '')`); !slices.Equal(got, []string{""}) {
		t.Fatalf("main has table items: %v", got)
	}

	// A restore point of dev, then the table goes.
	var rp e2eutil.RestorePoint
	c.Expect(t, http.MethodPost, e2eutil.BranchPath(id, "dev")+"/restore-points", map[string]any{"label": "before drop"}, &rp,
		http.StatusCreated)
	if rp.ID == "" || rp.Branch != "dev" || rp.Label != "before drop" {
		t.Fatalf("restore point: %+v", rp)
	}
	e2eutil.Exec(t, db, `DROP TABLE items`)

	// Rollback: 200 with the backup branch that keeps the data without the table.
	var res e2eutil.RestoreResult
	r = c.Expect(t, http.MethodPost, e2eutil.BranchPath(id, "dev")+"/rollback", map[string]any{"restorePointId": rp.ID}, &res,
		http.StatusOK)
	t.Logf("rollback: %d in %s, backup branch %s", r.Status, r.Elapsed, res.BackupBranch)
	if res.BackupBranch == "" {
		t.Fatalf("rollback answered without backupBranch: %s", r)
	}
	if res.Branch.LastRestore == nil || res.Branch.LastRestore.Phase != "Done" {
		t.Errorf("rollback answered lastRestore %+v, want phase Done", res.Branch.LastRestore)
	}
	e2eutil.WaitBranchPhase(t, c, id, "dev", "Running", 2*time.Minute)

	// The rows are back, and the backup branch exists.
	db = e2eutil.SQL(t, e2eutil.GetConnection(t, c, id, "dev"))
	if got := items(t, db); !slices.Equal(got, want) {
		t.Fatalf("dev items after rollback %v, want %v", got, want)
	}
	backup := e2eutil.GetBranch(t, c, id, res.BackupBranch)
	if backup.BackupOf != "dev" {
		t.Errorf("backup branch %s: backupOf %q, want dev", backup.Name, backup.BackupOf)
	}

	// Stop: 200 and Stopped. Start: 200, and the rows are still there.
	var stopped e2eutil.Branch
	r = c.Expect(t, http.MethodPost, e2eutil.BranchPath(id, "dev")+"/stop", nil, &stopped, http.StatusOK)
	t.Logf("stop: %d in %s", r.Status, r.Elapsed)
	if stopped.Phase != "Stopped" || stopped.State != "Stopped" {
		t.Fatalf("stop answered phase %s state %s, want Stopped", stopped.Phase, stopped.State)
	}
	var started e2eutil.Branch
	r = c.Expect(t, http.MethodPost, e2eutil.BranchPath(id, "dev")+"/start", nil, &started, http.StatusOK)
	t.Logf("start: %d in %s", r.Status, r.Elapsed)
	if started.Phase != "Running" || started.State != "Running" {
		t.Fatalf("start answered phase %s state %s, want Running", started.Phase, started.State)
	}
	db = e2eutil.SQL(t, e2eutil.GetConnection(t, c, id, "dev"))
	if got := items(t, db); !slices.Equal(got, want) {
		t.Fatalf("dev items after stop and start %v, want %v", got, want)
	}

	// Delete dev: 202, then gone.
	c.Expect(t, http.MethodDelete, e2eutil.BranchPath(id, "dev"), nil, nil, http.StatusAccepted)
	e2eutil.WaitBranchGone(t, c, id, "dev", 2*time.Minute)

	// Delete the project: 202, then the namespace is gone.
	c.Expect(t, http.MethodDelete, e2eutil.ProjectPath(id), nil, nil, http.StatusAccepted)
	e2eutil.WaitNamespaceGone(t, ns, 5*time.Minute)
	c.Expect(t, http.MethodGet, e2eutil.ProjectPath(id), nil, nil, http.StatusNotFound)
}

// items returns the names in table items of a branch, by id.
func items(t *testing.T, db *pgx.Conn) []string {
	t.Helper()
	return e2eutil.QueryStrings(t, db, `SELECT name FROM items ORDER BY id`)
}
