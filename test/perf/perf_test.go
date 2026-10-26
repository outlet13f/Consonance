//go:build perf

// Package perf measures spec 15's performance criteria on the development k3s (make dev-cluster
// images load-images deploy-dev): branch creation, a new branch's first query, rollback and stop
// → start, each 20 times after a warm-up that is thrown away, without load and under a pgbench
// write load on main, and the time a project takes to become Ready. It writes the results to
// docs/perf/2026-10-26-branching-engine.md and fails when a criterion is missed. Run it with make
// perf. Without the cluster it fails rather than skips, like the E2E.
package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"consonance/test/internal/e2eutil"
)

// Overridable for a short trial run; the documented run uses the defaults.
var (
	runs  = envInt("PERF_RUNS", 20)  // measured runs per metric and condition (spec 15)
	scale = envInt("PERF_SCALE", 70) // pgbench scale of the data rolled back: 70 is about 1GB
)

// docPath is the result document, from the package directory go test runs in.
const docPath = "../../docs/perf/2026-10-26-branching-engine.md"

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	if err := e2eutil.CheckCluster(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: the development cluster is not ready: %v\n", err)
		os.Exit(1)
	}
	before, err := projectNamespaces()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: list project namespaces: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if !leavesNothingBehind(before) {
		code = 1
	}
	os.Exit(code)
}

func projectNamespaces() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return e2eutil.ProjectNamespaces(ctx)
}

// leavesNothingBehind is TestPerfLeavesNothingBehind: after the measurements, passed or not, no
// cns- namespace made during the run is left. Those there before are not this run's to judge.
func leavesNothingBehind(before []string) bool {
	const name = "TestPerfLeavesNothingBehind"
	fmt.Printf("=== RUN   %s\n", name)
	start := time.Now()
	now, err := projectNamespaces()
	elapsed := time.Since(start).Seconds()
	if err != nil {
		fmt.Printf("    %v\n--- FAIL: %s (%.2fs)\n", err, name, elapsed)
		return false
	}
	var left []string
	for _, ns := range now {
		if !slices.Contains(before, ns) {
			left = append(left, ns)
		}
	}
	if len(left) > 0 {
		fmt.Printf("    project namespaces left behind: %v\n--- FAIL: %s (%.2fs)\n", left, name, elapsed)
		return false
	}
	fmt.Printf("--- PASS: %s (%.2fs)\n", name, elapsed)
	return true
}

// The two conditions of spec 15, measured one after the other: the node (4 CPU, about 5.8Gi)
// holds a project's main, the branch rolled back and one more branch, not two sets at once.
const (
	noLoad    = "부하 없음"
	writeLoad = "쓰기 부하"
)

// TestPerformance runs the measurements on one project, perf. Its main holds pgbench's tables
// (scale 70, about 1GB) and carries the write load; its branch rb, a copy of them, is what is
// rolled back and stopped and started; the branches measured for creation are copies of main.
// Every measured branch, every backup branch a rollback makes and every project measured is
// deleted, and gone, before the next run, so that the node never holds more than it can run.
func TestPerformance(t *testing.T) {
	res := &results{started: time.Now(), runs: runs, scale: scale}
	// Registered first, so it runs last: the document is written whatever happened, after the
	// project is gone.
	t.Cleanup(func() {
		res.finished = time.Now()
		if err := writeDoc(docPath, res); err != nil {
			t.Errorf("write %s: %v", docPath, err)
		} else {
			t.Logf("wrote %s", docPath)
		}
	})

	c := e2eutil.API(t)
	id := e2eutil.CreateProject(t, c, "perf")
	ns := e2eutil.Namespace(id)
	project := e2eutil.WaitProjectReady(t, c, id, 5*time.Minute)
	res.env = environment(t, c, id, ns)
	res.env.storageClass = project.Storage.StorageClassName

	mainConn := e2eutil.GetConnection(t, c, id, "main")
	res.data = initData(t, ns, mainConn.Password, scale)
	t.Logf("pgbench -i -s %d: %s, database %s", scale, res.data.initTime.Round(time.Second), bytesString(res.data.bytes))

	c.Expect(t, http.MethodPost, e2eutil.ProjectPath(id)+"/branches", map[string]any{"name": rollbackBranch}, nil,
		http.StatusCreated, http.StatusAccepted)
	e2eutil.WaitBranchPhase(t, c, id, rollbackBranch, "Running", 3*time.Minute)

	// rb was the last copy of main.
	m := &measurer{t: t, c: c, id: id, ns: ns, conn: mainConn, copyInterval: copyInterval(t, c, id), lastMainCopy: time.Now()}
	res.copyInterval = m.copyInterval
	for _, cond := range []string{noLoad, writeLoad} {
		cr := &conditionResult{name: cond}
		res.conditions = append(res.conditions, cr)
		before := operatorMetrics(t)
		var l *load
		if cond == writeLoad {
			l = startLoad(t, ns, mainConn.Password)
		}
		// One group after the other, and none after a failure: the document then shows how far
		// the run got.
		for _, group := range []func(string) []*metric{m.branches, m.rollbacks, m.starts, m.projects} {
			if !t.Failed() {
				cr.metrics = append(cr.metrics, group(cond)...)
			}
		}
		if l != nil {
			cr.load = l.stop(t)
		}
		cr.operator = operatorMetrics(t).since(before)
		cr.pending = failedScheduling(t, ns)
		if t.Failed() {
			return
		}
	}

	// The criteria, after the document has everything (its cleanup writes it).
	for _, cr := range res.conditions {
		for _, mt := range cr.metrics {
			if ok, applies := mt.passed(); applies && !ok {
				t.Errorf("%s, %s: %s %s, criterion %s", cr.name, mt.name, mt.stat, seconds(mt.value()), mt.criterion())
			}
		}
	}
}

// rollbackBranch is the branch with the data that is rolled back, stopped and started.
const rollbackBranch = "rb"

// measurer takes the measurements of one project.
type measurer struct {
	t            *testing.T
	c            *e2eutil.Client
	id, ns       string
	conn         e2eutil.Connection // main's; every branch has the same tenant role
	copyInterval time.Duration      // the project's minCopyInterval
	lastMainCopy time.Time          // when the last copy of main ended (its branch answered)
	lastRBCopy   time.Time          // when the last restore point of rb was made
}

// pace waits until the project's minimum copy interval has passed since last, plus a margin:
// the agent refuses a backup-mode copy of a branch sooner than that after the previous one
// (spec 7.2), and a refused copy would measure the limit instead of the copy.
func (m *measurer) pace(last time.Time) {
	if last.IsZero() {
		return
	}
	if wait := time.Until(last.Add(m.copyInterval + 500*time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
}

// each runs fn once for the warm-up and runs times for the measurement, each in its own
// subtest, so that what a run registers for cleanup (a port-forward, a branch, a project) goes
// before the next. It stops at the first run that fails.
func (m *measurer) each(name string, fn func(t *testing.T, i int)) bool {
	for i := 0; i <= runs; i++ {
		label := fmt.Sprintf("%s-run%02d", name, i)
		if i == 0 {
			label = name + "-warmup"
		}
		if !m.t.Run(label, func(t *testing.T) { fn(t, i) }) {
			return false
		}
	}
	return true
}

// branches measures branch creation (POST until 201) and the new branch's first query (from the
// 201 until SELECT 1 answers through a port-forward), then deletes the branch.
func (m *measurer) branches(cond string) []*metric {
	create := &metric{name: "브랜치 생성 API(201까지)", stat: statP50, target: time.Second, statuses: map[int]int{}}
	first := &metric{name: "새 브랜치 첫 쿼리(201 → SELECT 1)", stat: statP95, target: 3 * time.Second}
	prefix := map[string]string{noLoad: "nl", writeLoad: "wl"}[cond]
	m.each("branches-"+prefix, func(t *testing.T, i int) {
		name := fmt.Sprintf("%s-%02d", prefix, i)
		m.pace(m.lastMainCopy)
		var b e2eutil.Branch
		r := m.c.Expect(t, http.MethodPost, e2eutil.ProjectPath(m.id)+"/branches", map[string]any{"name": name}, &b,
			http.StatusCreated, http.StatusAccepted)
		answered := time.Now()
		m.lastMainCopy = answered
		t.Cleanup(func() { m.deleteBranch(t, name) })

		conn := m.conn
		conn.Host = "br-" + name + "." + m.ns + ".svc"
		if b.Endpoint != nil {
			conn.Host, conn.Port = b.Endpoint.Host, b.Endpoint.Port
		}
		q := firstQuery(t, conn, answered)
		t.Logf("%s: %d in %s; SELECT 1 after %s (%d tries, the last forward took %s)",
			name, r.Status, r.Elapsed.Round(time.Millisecond), q.total.Round(time.Millisecond), q.tries, q.forward.Round(time.Millisecond))
		create.add(i, r.Elapsed)
		first.add(i, q.total)
		if i > 0 {
			create.statuses[r.Status]++
			first.forward = append(first.forward, q.forward)
			first.tries = append(first.tries, q.tries)
		}
	})
	return []*metric{create, first}
}

// firstQueryResult is one first query: from the 201 to SELECT 1, how many tries it took and how
// long the port-forward of the try that worked took to listen.
type firstQueryResult struct {
	total, forward time.Duration
	tries          int
}

// firstQuery tries a fresh port-forward, a connection and SELECT 1 every 100ms until one
// answers, and times it from since. e2eutil.SQL retries each second, too coarse for a 3-second
// criterion.
func firstQuery(t *testing.T, conn e2eutil.Connection, since time.Time) firstQueryResult {
	t.Helper()
	deadline := since.Add(2 * time.Minute)
	var res firstQueryResult
	for {
		res.tries++
		err := func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			db, release, forward, err := e2eutil.Dial(ctx, conn)
			if err != nil {
				return err
			}
			defer release()
			var one int
			if err := db.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
				return err
			}
			res.total, res.forward = time.Since(since), forward
			return nil
		}()
		if err == nil {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("SELECT 1 on %s: no answer within 2m after %d tries; last: %v", conn.Host, res.tries, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// deleteBranch deletes a branch and waits until it is gone, so its pod no longer holds the node.
func (m *measurer) deleteBranch(t *testing.T, name string) {
	r, err := m.c.Call(http.MethodDelete, e2eutil.BranchPath(m.id, name), nil)
	switch {
	case err != nil:
		t.Errorf("cleanup: delete branch %s: %v", name, err)
		return
	case r.Status != http.StatusAccepted && r.Status != http.StatusNotFound:
		t.Errorf("cleanup: delete branch %s: %s", name, r)
		return
	}
	e2eutil.WaitBranchGone(t, m.c, m.id, name, 3*time.Minute)
}

// rollbacks measures a rollback of rb (POST until 200) to a restore point taken just before.
// The backup branch it makes and the restore point are deleted after each run.
func (m *measurer) rollbacks(cond string) []*metric {
	mt := &metric{name: fmt.Sprintf("롤백(200까지, pgbench -s %d)", scale), stat: statMax, target: 10 * time.Second}
	m.each("rollbacks-"+map[string]string{noLoad: "nl", writeLoad: "wl"}[cond], func(t *testing.T, i int) {
		m.pace(m.lastRBCopy)
		var rp e2eutil.RestorePoint
		m.c.Expect(t, http.MethodPost, e2eutil.BranchPath(m.id, rollbackBranch)+"/restore-points",
			map[string]any{"label": fmt.Sprintf("perf %d", i)}, &rp, http.StatusCreated)
		m.lastRBCopy = time.Now()
		t.Cleanup(func() {
			r, err := m.c.Call(http.MethodDelete, e2eutil.BranchPath(m.id, rollbackBranch)+"/restore-points/"+rp.ID, nil)
			if err != nil || (r.Status != http.StatusNoContent && r.Status != http.StatusOK && r.Status != http.StatusNotFound) {
				t.Errorf("cleanup: delete restore point %s: %v %v", rp.ID, r, err)
			}
		})

		var out e2eutil.RestoreResult
		r := m.c.Expect(t, http.MethodPost, e2eutil.BranchPath(m.id, rollbackBranch)+"/rollback",
			map[string]any{"restorePointId": rp.ID}, &out, http.StatusOK)
		if out.BackupBranch == "" {
			t.Fatalf("rollback answered without a backup branch: %s", r)
		}
		t.Cleanup(func() { m.deleteBranch(t, out.BackupBranch) })
		t.Logf("rollback to %s: %d in %s, backup %s", rp.ID, r.Status, r.Elapsed.Round(time.Millisecond), out.BackupBranch)
		mt.add(i, r.Elapsed)
		e2eutil.WaitBranchPhase(t, m.c, m.id, rollbackBranch, "Running", 2*time.Minute)
	})
	return []*metric{mt}
}

// starts measures starting rb after it was stopped (POST start until 200).
func (m *measurer) starts(cond string) []*metric {
	mt := &metric{name: "정지 → 실행(기동 200까지)", stat: statP50, target: time.Second}
	m.each("starts-"+map[string]string{noLoad: "nl", writeLoad: "wl"}[cond], func(t *testing.T, i int) {
		var b e2eutil.Branch
		m.c.Expect(t, http.MethodPost, e2eutil.BranchPath(m.id, rollbackBranch)+"/stop", nil, &b, http.StatusOK)
		if b.Phase != "Stopped" {
			t.Fatalf("stop answered phase %s", b.Phase)
		}
		r := m.c.Expect(t, http.MethodPost, e2eutil.BranchPath(m.id, rollbackBranch)+"/start", nil, &b, http.StatusOK)
		if b.Phase != "Running" {
			t.Fatalf("start answered phase %s", b.Phase)
		}
		t.Logf("start: %d in %s", r.Status, r.Elapsed.Round(time.Millisecond))
		mt.add(i, r.Elapsed)
	})
	return []*metric{mt}
}

// projects measures a project from POST until Ready (init-main included), then deletes it and
// waits until its namespace is gone. There is no criterion (spec 15.3).
func (m *measurer) projects(cond string) []*metric {
	mt := &metric{name: "프로젝트 생성(요청 → Ready)", stat: statP50}
	prefix := map[string]string{noLoad: "nl", writeLoad: "wl"}[cond]
	m.each("projects-"+prefix, func(t *testing.T, i int) {
		start := time.Now()
		id := e2eutil.CreateProject(t, m.c, fmt.Sprintf("perf-%s-%02d", prefix, i))
		e2eutil.Eventually(t, 5*time.Minute, 200*time.Millisecond, "project "+id+" Ready", func() (bool, string) {
			p := e2eutil.GetProject(t, m.c, id)
			if p.Phase == "Failed" {
				t.Fatalf("project %s failed: %s: %s", id, p.Reason, p.Message)
			}
			return p.Phase == "Ready", p.Phase + " " + p.Reason
		})
		d := time.Since(start)
		// Before the project's deletion, which CreateProject registered earlier and so runs later.
		t.Cleanup(func() { mt.pending += failedScheduling(t, e2eutil.Namespace(id)) })
		t.Logf("project %s Ready in %s", id, d.Round(time.Millisecond))
		mt.add(i, d)
	})
	return []*metric{mt}
}

// copyInterval reads the project's minCopyInterval.
func copyInterval(t *testing.T, c *e2eutil.Client, id string) time.Duration {
	t.Helper()
	var p struct {
		Limits struct {
			MinCopyInterval string `json:"minCopyInterval"`
		} `json:"limits"`
	}
	c.Expect(t, http.MethodGet, e2eutil.ProjectPath(id), nil, &p, http.StatusOK)
	d, err := time.ParseDuration(p.Limits.MinCopyInterval)
	if err != nil {
		t.Fatalf("minCopyInterval %q: %v", p.Limits.MinCopyInterval, err)
	}
	return d
}

// failedScheduling counts the FailedScheduling events in a namespace: a pod that waited Pending
// for room on the node, which is the environment's limit rather than the engine's.
func failedScheduling(t *testing.T, ns string) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := e2eutil.KubectlContext(ctx, "-n", ns, "get", "events", "--field-selector", "reason=FailedScheduling",
		"-o", "jsonpath={range .items[*]}{.count}{\"\\n\"}{end}")
	if err != nil {
		t.Logf("events of %s: %v", ns, err)
		return 0
	}
	n := 0
	for _, f := range strings.Fields(out) {
		c, _ := strconv.Atoi(f)
		n += max(c, 1)
	}
	return n
}

// projectCompute reads the project's default compute, which every branch here runs with.
func projectCompute(t *testing.T, c *e2eutil.Client, id string) (cpu, memory string) {
	t.Helper()
	var p struct {
		DefaultCompute struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		} `json:"defaultCompute"`
	}
	r := c.Expect(t, http.MethodGet, e2eutil.ProjectPath(id), nil, nil, http.StatusOK)
	if err := json.Unmarshal(r.Body, &p); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	return p.DefaultCompute.CPU, p.DefaultCompute.Memory
}
