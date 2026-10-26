//go:build perf

package perf

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"consonance/test/internal/e2eutil"
)

// pgbench runs in the loaded branch's postgres container with kubectl exec. The project's
// branch-ingress policy lets PostgreSQL be reached only from consonance-system and from pods
// labelled as the volume agent, and a pod with that label would also join the agent's Service
// and take its traffic; the measurement does not widen the product's policy (spec 8) to make
// room for a client pod. So pgbench runs next to the server it loads, over the pod's Unix
// socket, and shares the branch's CPU limit.
//
// The password comes from the API (GET .../connection, as the E2E gets it) and goes to the
// container on stdin, never on a command line or in the output.
const pgbenchShell = `IFS= read -r PGPASSWORD && export PGPASSWORD && `

// pgbenchConn are the connection arguments: the tenant role over the pod's socket.
const pgbenchConn = `-h /var/run/postgresql -U app_owner -d app`

// podExec runs script with sh in a branch pod's postgres container, the password on its stdin.
func podExec(ctx context.Context, ns, pod, password, script string) (string, error) {
	return e2eutil.KubectlInput(ctx, []byte(password+"\n"), "-n", ns, "exec", "-i", pod, "-c", "postgres", "--",
		"sh", "-c", pgbenchShell+script)
}

// mainExec is podExec on main.
func mainExec(ctx context.Context, ns, password, script string) (string, error) {
	return podExec(ctx, ns, "br-main", password, script)
}

// dataResult is the data the rollbacks carry.
type dataResult struct {
	initTime time.Duration
	bytes    int64
}

// initData fills main with pgbench's tables at scale and returns the database's size. Every
// branch is a copy of main, so rb, which is rolled back, carries them too.
func initData(t *testing.T, ns, password string, scale int) dataResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	start := time.Now()
	if out, err := mainExec(ctx, ns, password, fmt.Sprintf("exec pgbench %s -i -q -s %d 2>&1", pgbenchConn, scale)); err != nil {
		t.Fatalf("pgbench -i -s %d: %v\n%s", scale, err, out)
	}
	res := dataResult{initTime: time.Since(start)}
	out, err := mainExec(ctx, ns, password, "exec psql "+pgbenchConn+` -Atc "SELECT pg_database_size(current_database())"`)
	if err != nil {
		t.Fatalf("database size: %v", err)
	}
	if res.bytes, err = strconv.ParseInt(strings.TrimSpace(out), 10, 64); err != nil {
		t.Fatalf("database size %q: %v", out, err)
	}
	return res
}

// pgbenchPIDFile is where a load's pgbench writes its process ID, in the pod's socket directory
// (an emptyDir), so that stop can end it.
const pgbenchPIDFile = "/var/run/postgresql/perf-pgbench.pid"

// loadCap is pgbench's -T: the write condition ends it sooner, and the cap ends it should the
// test die without its cleanups. Deleting the project ends it too.
const loadCap = 2 * time.Hour

// load is pgbench's write load on one branch: 4 clients, -N (no updates of the small tables,
// so the clients do not queue on their rows).
type load struct {
	ns, pod, password string
	started           time.Time
	done              chan struct{}
	out               string
	err               error
	ended             bool
	cancel            context.CancelFunc
}

// loadResult is what the document reports of the load.
type loadResult struct {
	command  string
	duration time.Duration
	tps      float64 // mean of pgbench's progress reports
	reports  int
}

var progressLine = regexp.MustCompile(`progress: [0-9.]+ s, ([0-9.]+) tps`)

// loadCommand is the pgbench run; progress every interval seconds.
func loadCommand(interval int) string {
	return fmt.Sprintf("pgbench -c 4 -T %d -N -P %d", int(loadCap.Seconds()), interval)
}

// startLoad starts pgbench on pod and returns once its clients are active in pg_stat_activity,
// so that what is measured next runs under the load.
func startLoad(t *testing.T, ns, pod, password string, progress int) *load {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &load{ns: ns, pod: pod, password: password, started: time.Now(), done: make(chan struct{}), cancel: cancel}
	script := fmt.Sprintf("echo $$ > %s && exec %s %s 2>&1", pgbenchPIDFile, loadCommand(progress), pgbenchConn)
	go func() {
		defer close(l.done)
		l.out, l.err = podExec(ctx, ns, pod, password, script)
	}()
	t.Cleanup(func() {
		if !l.ended {
			l.end()
		}
	})
	// The exec's output arrives when it ends, so whether the clients run is read from a second
	// exec. pgbench vacuums the small tables first.
	e2eutil.Eventually(t, 2*time.Minute, 500*time.Millisecond, "pgbench running in "+ns+"/"+pod, func() (bool, string) {
		select {
		case <-l.done:
			t.Fatalf("pgbench on %s ended before the measurement: %v\n%s", pod, l.err, l.out)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := podExec(ctx, ns, pod, password,
			fmt.Sprintf(`exec psql %s -Atc "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'pgbench' AND state <> 'idle'"`, pgbenchConn))
		if err != nil {
			return false, err.Error()
		}
		n, _ := strconv.Atoi(strings.TrimSpace(out))
		return n > 0, fmt.Sprintf("%s active pgbench sessions", strings.TrimSpace(out))
	})
	return l
}

// running tells whether pgbench has not ended yet.
func (l *load) running() bool {
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

// end stops pgbench with SIGINT if it still runs and waits for it. A load on a branch that was
// stopped or restarted has ended already: its clients lost their server.
func (l *load) end() {
	l.ended = true
	if l.running() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, _ = podExec(ctx, l.ns, l.pod, l.password, fmt.Sprintf(`kill -INT "$(cat %s)" 2>/dev/null`, pgbenchPIDFile))
		cancel()
		select {
		case <-l.done:
		case <-time.After(time.Minute):
		}
	}
	l.cancel()
}

// progress is the mean tps of pgbench's progress reports and how many there were.
func (l *load) progress() (tps float64, reports int, zero bool) {
	var sum float64
	for _, m := range progressLine.FindAllStringSubmatch(l.out, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			sum += v
			reports++
			zero = zero || v == 0
		}
	}
	if reports > 0 {
		tps = sum / float64(reports)
	}
	return tps, reports, zero
}

// stop ends the condition's load on main and returns what it reported. It fails the test when
// pgbench ended before the condition did, when a client aborted, or when a progress report
// shows no transactions: then not every measurement ran under the load.
func (l *load) stop(t *testing.T) loadResult {
	early := !l.running()
	l.end()
	res := loadResult{command: loadCommand(5) + " (조건이 끝나면 SIGINT로 멈춤)", duration: time.Since(l.started)}
	if early {
		t.Errorf("pgbench on %s ended before the write condition did: %v\n%s", l.pod, l.err, l.out)
	}
	var zero bool
	res.tps, res.reports, zero = l.progress()
	switch {
	case res.reports == 0:
		t.Errorf("pgbench on %s printed no progress; output:\n%s", l.pod, l.out)
	case strings.Contains(l.out, "aborted"):
		t.Errorf("a pgbench client on %s aborted; output:\n%s", l.pod, l.out)
	case zero:
		t.Errorf("pgbench on %s reported a 5-second interval without transactions; output:\n%s", l.pod, l.out)
	}
	return res
}

// rbLoadHold is how long rb is written before a sample, so that the sample meets dirty buffers
// and a busy server, not only a pgbench that has just connected.
const rbLoadHold = 5 * time.Second

// underLoad runs sample while pgbench writes rb, for the write condition's rollbacks and stops
// and starts: the load starts, its clients are active, it runs rbLoadHold, then sample runs.
// The sample stops or restarts rb's PostgreSQL, which ends the load; what is left of it is ended
// after. It returns the mean tps pgbench reported.
func (m *measurer) underLoad(t *testing.T, sample func()) float64 {
	t.Helper()
	l := startLoad(t, m.ns, "br-"+rollbackBranch, m.conn.Password, 1)
	time.Sleep(rbLoadHold)
	if !l.running() {
		t.Fatalf("pgbench on rb ended before the sample: %v\n%s", l.err, l.out)
	}
	sample()
	l.end()
	tps, _, _ := l.progress()
	return tps
}
