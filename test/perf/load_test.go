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

// pgbench runs in main's postgres container with kubectl exec. The project's branch-ingress
// policy lets PostgreSQL be reached only from consonance-system and from pods labelled as the
// volume agent, and a pod with that label would also join the agent's Service and take its
// traffic; the measurement does not widen the product's policy (spec 8) to make room for a
// client pod. So pgbench runs next to the server it loads, over the pod's Unix socket, and
// shares main's CPU limit.
//
// The password comes from the API (GET .../connection, as the E2E gets it) and goes to the
// container on stdin, never on a command line or in the output.
const pgbenchShell = `IFS= read -r PGPASSWORD && export PGPASSWORD && `

// pgbenchArgs are the connection arguments: the tenant role over the pod's socket.
const pgbenchConn = `-h /var/run/postgresql -U app_owner -d app`

// mainExec runs script with sh in main's postgres container, the password on its stdin.
func mainExec(ctx context.Context, ns, password, script string) (string, error) {
	return e2eutil.KubectlInput(ctx, []byte(password+"\n"), "-n", ns, "exec", "-i", "br-main", "-c", "postgres", "--",
		"sh", "-c", pgbenchShell+script)
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

// pgbenchPIDFile is where the load's pgbench writes its process ID, in main's socket directory
// (an emptyDir), so that stop can end it.
const pgbenchPIDFile = "/var/run/postgresql/perf-pgbench.pid"

// loadCap is pgbench's -T: the write condition ends it sooner (stop), and the cap ends it should
// the test die without its cleanups. Deleting the project ends it too.
const loadCap = 2 * time.Hour

// load is pgbench's write load on main: 4 clients, -N (no updates of the small tables, so the
// clients do not queue on their rows).
type load struct {
	ns, password string
	started      time.Time
	done         chan struct{}
	out          string
	err          error
	stopped      bool
	cancel       context.CancelFunc
}

// loadResult is what the document reports of the load.
type loadResult struct {
	command  string
	duration time.Duration
	tps      float64 // mean of pgbench's progress reports
	reports  int
}

var progressLine = regexp.MustCompile(`progress: [0-9.]+ s, ([0-9.]+) tps`)

// startLoad starts the load and returns once pgbench reports progress, so that every
// measurement of the condition runs under it.
func startLoad(t *testing.T, ns, password string) *load {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &load{ns: ns, password: password, started: time.Now(), done: make(chan struct{}), cancel: cancel}
	script := fmt.Sprintf("echo $$ > %s && exec pgbench %s -c 4 -T %d -N -P 5 2>&1",
		pgbenchPIDFile, pgbenchConn, int(loadCap.Seconds()))
	go func() {
		defer close(l.done)
		l.out, l.err = mainExec(ctx, ns, password, script)
	}()
	t.Cleanup(func() {
		if !l.stopped {
			l.stop(t)
		}
	})
	// The exec's output arrives when it ends, so progress is read from a second exec: the
	// first progress report means the clients run. pgbench vacuums first, which takes a few
	// seconds at this scale.
	e2eutil.Eventually(t, 2*time.Minute, time.Second, "pgbench running in "+ns+"/br-main", func() (bool, string) {
		select {
		case <-l.done:
			t.Fatalf("pgbench ended before the measurements: %v\n%s", l.err, l.out)
		default:
		}
		out, err := mainExec(context.Background(), ns, password,
			fmt.Sprintf(`exec psql %s -Atc "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'pgbench' AND state <> 'idle'"`, pgbenchConn))
		if err != nil {
			return false, err.Error()
		}
		n, _ := strconv.Atoi(strings.TrimSpace(out))
		return n > 0, fmt.Sprintf("%s active pgbench sessions", strings.TrimSpace(out))
	})
	t.Logf("pgbench write load running on %s/br-main", ns)
	return l
}

// stop ends pgbench with SIGINT and returns what it reported. A pgbench that ended before
// stop fails the test: the condition's measurements were not all under load.
func (l *load) stop(t *testing.T) loadResult {
	l.stopped = true
	res := loadResult{
		command:  fmt.Sprintf("pgbench -c 4 -T %d -N -P 5 (조건이 끝나면 SIGINT로 멈춤)", int(loadCap.Seconds())),
		duration: time.Since(l.started),
	}
	select {
	case <-l.done:
		t.Errorf("pgbench ended before the write condition did: %v\n%s", l.err, l.out)
	default:
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if _, err := mainExec(ctx, l.ns, l.password, fmt.Sprintf(`kill -INT "$(cat %s)"`, pgbenchPIDFile)); err != nil {
			t.Errorf("stop pgbench: %v", err)
		}
		cancel()
		select {
		case <-l.done:
		case <-time.After(time.Minute):
			t.Errorf("pgbench did not end a minute after SIGINT")
		}
	}
	l.cancel()
	var sum float64
	for _, m := range progressLine.FindAllStringSubmatch(l.out, -1) {
		v, err := strconv.ParseFloat(m[1], 64)
		if err == nil {
			sum += v
			res.reports++
		}
	}
	if res.reports > 0 {
		res.tps = sum / float64(res.reports)
	} else {
		t.Errorf("pgbench printed no progress; output:\n%s", l.out)
	}
	return res
}

// environment is where the measurements ran.
type environmentInfo struct {
	host, node, kernel, k3s, runtime, arch string
	cpu, memory, allocCPU, allocMemory     string
	storageClass, provisioner, fsType      string
	branchCPU, branchMemory                string
	commit                                 string
}

func environment(t *testing.T, c *e2eutil.Client, id, ns string) environmentInfo {
	t.Helper()
	var e environmentInfo
	f := strings.Split(e2eutil.Kubectl(t, "get", "nodes", "-o",
		`jsonpath={.items[0].metadata.name}|{.items[0].status.nodeInfo.kernelVersion}|{.items[0].status.nodeInfo.kubeletVersion}|{.items[0].status.nodeInfo.containerRuntimeVersion}|{.items[0].status.nodeInfo.architecture}|{.items[0].status.capacity.cpu}|{.items[0].status.capacity.memory}|{.items[0].status.allocatable.cpu}|{.items[0].status.allocatable.memory}`), "|")
	if len(f) == 9 {
		e.node, e.kernel, e.k3s, e.runtime, e.arch = f[0], f[1], f[2], f[3], f[4]
		e.cpu, e.memory, e.allocCPU, e.allocMemory = f[5], f[6], f[7], f[8]
	}
	e.provisioner = e2eutil.Kubectl(t, "get", "storageclass", "consonance-xfs", "-o", "jsonpath={.provisioner}")
	e.fsType = e2eutil.Kubectl(t, "-n", ns, "exec", "volume-agent", "--", "stat", "-f", "-c", "%T", "/volume")
	e.branchCPU, e.branchMemory = projectCompute(t, c, id)
	e.host = hostInfo()
	e.commit = gitCommit()
	return e
}

// operatorSnapshot is the sums and counts of the operator's histograms (spec 11) at one moment.
type operatorSnapshot map[string]float64

// operatorHistograms are the series read; their means over a condition cross-check the client's
// times, from the operator's side (they include the warm-ups and every event of the kind).
var operatorHistograms = []string{
	"consonance_branch_create_seconds",
	"consonance_branch_first_ready_seconds",
	`consonance_restore_seconds{kind="rollback",result="done"}`,
	"consonance_start_seconds",
	"consonance_project_create_seconds",
}

// operatorMetrics reads the operator's metrics through the API server's pod proxy.
func operatorMetrics(t *testing.T) operatorSnapshot {
	t.Helper()
	snap := operatorSnapshot{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pod, err := e2eutil.KubectlContext(ctx, "-n", e2eutil.SystemNamespace, "get", "pods", "-l",
		"app.kubernetes.io/name=consonance-operator", "--field-selector=status.phase=Running", "-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || pod == "" {
		t.Logf("operator metrics: no operator pod: %v", err)
		return snap
	}
	out, err := e2eutil.KubectlContext(ctx, "get", "--raw",
		"/api/v1/namespaces/"+e2eutil.SystemNamespace+"/pods/"+pod+":8080/proxy/metrics")
	if err != nil {
		t.Logf("operator metrics: %v", err)
		return snap
	}
	snap["pod:"+pod] = 1
	for _, line := range strings.Split(out, "\n") {
		for _, h := range operatorHistograms {
			name, labels, _ := strings.Cut(h, "{")
			if labels != "" {
				labels = "{" + labels
			}
			for _, suffix := range []string{"_sum", "_count"} {
				prefix := name + suffix + labels + " "
				if strings.HasPrefix(line, prefix) {
					if v, err := strconv.ParseFloat(strings.TrimPrefix(line, prefix), 64); err == nil {
						snap[h+suffix] = v
					}
				}
			}
		}
	}
	return snap
}

// operatorMean is a histogram's mean over a condition and how many it saw.
type operatorMean struct {
	name  string
	count int
	mean  time.Duration
}

// since is the mean of each histogram between before and s; nothing when the operator was
// restarted in between (its histograms began again).
func (s operatorSnapshot) since(before operatorSnapshot) []operatorMean {
	var out []operatorMean
	for k := range s {
		if strings.HasPrefix(k, "pod:") && before[k] == 0 {
			return nil
		}
	}
	for _, h := range operatorHistograms {
		n := s[h+"_count"] - before[h+"_count"]
		if n <= 0 {
			continue
		}
		sum := s[h+"_sum"] - before[h+"_sum"]
		out = append(out, operatorMean{name: h, count: int(n), mean: time.Duration(sum / n * float64(time.Second))})
	}
	return out
}
