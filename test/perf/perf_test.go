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
	res := &results{started: time.Now(), runs: runs, scale: scale, hostStart: hostState()}
	// Registered first, so it runs last: the document is written whatever happened, after the
	// project is gone.
	t.Cleanup(func() {
		res.finished = time.Now()
		res.hostEnd = hostState()
		if err := writeDoc(docPath, res); err != nil {
			t.Errorf("write %s: %v", docPath, err)
		} else {
			t.Logf("wrote %s", docPath)
		}
	})

	c := e2eutil.API(t)
	id := e2eutil.CreateProject(t, c, "perf")
	ns := e2eutil.Namespace(id)
	e2eutil.WaitProjectReady(t, c, id, 5*time.Minute)
	res.env = environment(t, ns)

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
			l = startLoad(t, ns, "br-main", mainConn.Password, 5)
			t.Logf("pgbench write load running on %s/br-main", ns)
		}
		// One group after the other, and none after a failure: the document then shows how far
		// the run got. The copies of main are broken down from the logs right after their group.
		groups := []func(string) []*metric{m.branches, m.rollbacks, m.starts, m.projects}
		for gi, group := range groups {
			if t.Failed() {
				break
			}
			start := time.Now()
			cr.metrics = append(cr.metrics, group(cond)...)
			if gi == 0 {
				cr.copies = copyBreakdown(t, ns, condPrefix[cond], start.Add(-time.Second))
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

// hostState is the Mac's load, the machine's 5-minute pressure stall averages (PSI some) and
// how many other containers share the machine.
func hostState() string {
	return hostLoad() + "; 머신 PSI avg300 " + machinePressure() + "; 머신에서 도는 consonance 밖의 컨테이너 " + otherContainers() + "개"
}

// condPrefix names a condition in branch names and subtests.
var condPrefix = map[string]string{noLoad: "nl", writeLoad: "wl"}

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
	prefix := condPrefix[cond]
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

// rollbacks measures a rollback of rb (POST until 200) to a restore point taken just before,
// and, recorded only, the time from the request until the API shows rb Running again (polled
// every 250ms). Under the write condition rb itself is written by pgbench while the restore
// point is taken and the rollback runs, besides main's load. The backup branch the rollback
// makes and the restore point are deleted after each run.
func (m *measurer) rollbacks(cond string) []*metric {
	mt := &metric{name: fmt.Sprintf("롤백(200까지, pgbench -s %d)", scale), stat: statMax, target: 10 * time.Second}
	running := &metric{name: "롤백 요청 → rb Running(보조)", stat: statP50}
	m.each("rollbacks-"+condPrefix[cond], func(t *testing.T, i int) {
		m.pace(m.lastRBCopy)
		sample := func() {
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
			sent := time.Now()
			r := m.c.Expect(t, http.MethodPost, e2eutil.BranchPath(m.id, rollbackBranch)+"/rollback",
				map[string]any{"restorePointId": rp.ID}, &out, http.StatusOK)
			if out.BackupBranch == "" {
				t.Fatalf("rollback answered without a backup branch: %s", r)
			}
			t.Cleanup(func() { m.deleteBranch(t, out.BackupBranch) })
			e2eutil.WaitBranchPhase(t, m.c, m.id, rollbackBranch, "Running", 2*time.Minute)
			back := time.Since(sent)
			t.Logf("rollback to %s: %d in %s, Running after %s, backup %s", rp.ID, r.Status,
				r.Elapsed.Round(time.Millisecond), back.Round(time.Millisecond), out.BackupBranch)
			mt.add(i, r.Elapsed)
			running.add(i, back)
		}
		if cond == writeLoad {
			mt.addLoad(i, m.underLoad(t, sample))
		} else {
			sample()
		}
	})
	return []*metric{mt, running}
}

// starts measures starting rb after it was stopped (POST start until 200) and, recorded only,
// the stop (POST stop until 200). Under the write condition rb is written by pgbench until the
// stop, which then pays for a shutdown checkpoint of what the load dirtied.
func (m *measurer) starts(cond string) []*metric {
	mt := &metric{name: "정지 → 실행(기동 200까지)", stat: statP50, target: time.Second}
	stop := &metric{name: "정지(200까지, 보조)", stat: statP50}
	m.each("starts-"+condPrefix[cond], func(t *testing.T, i int) {
		sample := func() {
			var b e2eutil.Branch
			r := m.c.Expect(t, http.MethodPost, e2eutil.BranchPath(m.id, rollbackBranch)+"/stop", nil, &b, http.StatusOK)
			if b.Phase != "Stopped" {
				t.Fatalf("stop answered phase %s", b.Phase)
			}
			stop.add(i, r.Elapsed)
			r = m.c.Expect(t, http.MethodPost, e2eutil.BranchPath(m.id, rollbackBranch)+"/start", nil, &b, http.StatusOK)
			if b.Phase != "Running" {
				t.Fatalf("start answered phase %s", b.Phase)
			}
			t.Logf("stop: %s, start: %d in %s", stop.last().Round(time.Millisecond), r.Status, r.Elapsed.Round(time.Millisecond))
			mt.add(i, r.Elapsed)
		}
		if cond == writeLoad {
			mt.addLoad(i, m.underLoad(t, sample))
		} else {
			sample()
		}
	})
	return []*metric{mt, stop}
}

// projects measures a project from POST until Ready (init-main included), then deletes it and
// waits until its namespace is gone. There is no criterion (spec 15.3).
func (m *measurer) projects(cond string) []*metric {
	mt := &metric{name: "프로젝트 생성(요청 → Ready)", stat: statP50}
	prefix := condPrefix[cond]
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
