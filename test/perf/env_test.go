//go:build perf

package perf

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"consonance/test/internal/e2eutil"
)

// environmentInfo is where the measurements ran, every value probed during the run.
type environmentInfo struct {
	host                               string
	machine, machineOS, k3sContainer   string
	node, kernel, k3s, runtime, arch   string
	cpu, memory, allocCPU, allocMemory string
	storageClass, provisioner, fsType  string
	volumePath, reflink, volumeDisk    string
	branchResources, agentResources    string
	images                             []string
	commit                             string
}

func environment(t *testing.T, ns string) environmentInfo {
	t.Helper()
	var e environmentInfo
	f := strings.Split(e2eutil.Kubectl(t, "get", "nodes", "-o",
		`jsonpath={.items[0].metadata.name}|{.items[0].status.nodeInfo.kernelVersion}|{.items[0].status.nodeInfo.kubeletVersion}|{.items[0].status.nodeInfo.containerRuntimeVersion}|{.items[0].status.nodeInfo.architecture}|{.items[0].status.capacity.cpu}|{.items[0].status.capacity.memory}|{.items[0].status.allocatable.cpu}|{.items[0].status.allocatable.memory}`), "|")
	if len(f) == 9 {
		e.node, e.kernel, e.k3s, e.runtime, e.arch = f[0], f[1], f[2], f[3], f[4]
		e.cpu, e.memory, e.allocCPU, e.allocMemory = f[5], f[6], f[7], f[8]
	}
	e.storageClass = e2eutil.Kubectl(t, "-n", ns, "get", "pvc", "project-volume", "-o", "jsonpath={.spec.storageClassName}")
	e.provisioner = e2eutil.Kubectl(t, "get", "storageclass", e.storageClass, "-o", "jsonpath={.provisioner}")
	e.fsType = e2eutil.Kubectl(t, "-n", ns, "exec", "volume-agent", "--", "stat", "-f", "-c", "%T", "/volume")
	pv := e2eutil.Kubectl(t, "-n", ns, "get", "pvc", "project-volume", "-o", "jsonpath={.spec.volumeName}")
	e.volumePath = e2eutil.Kubectl(t, "get", "pv", pv, "-o", "jsonpath={.spec.hostPath.path}{.spec.local.path}")
	resources := `jsonpath=요청 CPU {.spec.containers[0].resources.requests.cpu}·메모리 {.spec.containers[0].resources.requests.memory}, 한도 CPU {.spec.containers[0].resources.limits.cpu}·메모리 {.spec.containers[0].resources.limits.memory}`
	e.branchResources = e2eutil.Kubectl(t, "-n", ns, "get", "pod", "br-main", "-o", resources)
	e.agentResources = e2eutil.Kubectl(t, "-n", ns, "get", "pod", "volume-agent", "-o", resources)
	images := e2eutil.Kubectl(t, "get", "pods", "-n", ns, "-o", `jsonpath={range .items[*]}{.spec.containers[0].image} ({.spec.containers[0].imagePullPolicy}){"\n"}{end}`) + "\n" +
		e2eutil.Kubectl(t, "get", "pods", "-n", e2eutil.SystemNamespace, "-o", `jsonpath={range .items[*]}{.spec.containers[0].image} ({.spec.containers[0].imagePullPolicy}){"\n"}{end}`)
	for _, line := range strings.Split(images, "\n") {
		if line = strings.TrimSpace(line); line != "" && !slices.Contains(e.images, line) {
			e.images = append(e.images, line)
		}
	}
	slices.Sort(e.images)

	// The Podman machine, read-only.
	e.machine = fmt.Sprintf("%s(%s), CPU %s·메모리 %s MiB·디스크 %s GiB",
		command("podman", "machine", "inspect", "--format", "{{.Name}}"),
		command("podman", "machine", "info", "--format", "{{.Host.VMType}}"),
		command("podman", "machine", "inspect", "--format", "{{.Resources.CPUs}}"),
		command("podman", "machine", "inspect", "--format", "{{.Resources.Memory}}"),
		command("podman", "machine", "inspect", "--format", "{{.Resources.DiskSize}}"))
	e.machineOS = command("podman", "machine", "ssh", "--", `sh -c '. /etc/os-release && echo "$PRETTY_NAME"'`)
	e.k3sContainer = command("podman", "machine", "ssh", "--",
		`sudo podman inspect consonance-k3s --format '{{.Name}} {{.ImageName}} privileged={{.HostConfig.Privileged}}'`)
	if e.volumePath != "" {
		e.reflink = command("podman", "machine", "ssh", "--",
			fmt.Sprintf(`sh -c 'sudo xfs_info "$(sudo df --output=target %s | tail -1)" | grep -o "reflink=[01]"'`, e.volumePath))
		e.volumeDisk = command("podman", "machine", "ssh", "--",
			fmt.Sprintf(`sh -c 'sudo df -h --output=target,size,avail %s | tail -1'`, e.volumePath))
	}
	e.host = hostInfo()
	e.commit = gitCommit()
	return e
}

// command runs a probe and returns its trimmed output, or "?" when it fails.
func command(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}

// hostInfo describes the Mac the machine runs on, as far as it tells.
func hostInfo() string {
	if runtime.GOOS != "darwin" {
		return runtime.GOOS + "/" + runtime.GOARCH
	}
	var gib string
	if n, err := strconv.ParseInt(command("sysctl", "-n", "hw.memsize"), 10, 64); err == nil {
		gib = fmt.Sprintf("%d GiB", n>>30)
	}
	return fmt.Sprintf("macOS %s, %s, %s CPU, 메모리 %s", command("sw_vers", "-productVersion"),
		command("sysctl", "-n", "machdep.cpu.brand_string"), command("sysctl", "-n", "hw.ncpu"), gib)
}

// hostLoad is how busy the Mac is: its load averages, swap and free memory. The machine's memory
// is the Mac's, so a Mac that swaps slows the measurements down; the document records it at the
// start and the end of the run.
func hostLoad() string {
	if runtime.GOOS != "darwin" {
		return "?"
	}
	free := "?"
	for _, line := range strings.Split(command("memory_pressure", "-Q"), "\n") {
		if _, v, ok := strings.Cut(line, "free percentage: "); ok {
			free = v
		}
	}
	return fmt.Sprintf("부하 평균 %s, 스왑 %s, 남은 메모리 %s",
		strings.Trim(command("sysctl", "-n", "vm.loadavg"), "{ }"), command("sysctl", "-n", "vm.swapusage"), free)
}

// machinePressure is the machine's pressure stall averages over the last 5 minutes.
func machinePressure() string {
	return command("podman", "machine", "ssh", "--",
		`sh -c 'for r in cpu io memory; do printf "%s %s  " $r "$(grep some /proc/pressure/$r | cut -d" " -f4)"; done'`)
}

// otherContainers counts the containers running on the machine besides consonance's, the user's
// and root's: they share the machine's CPU, memory and disk with the measurements.
func otherContainers() string {
	out := command("podman", "machine", "ssh", "--", `sh -c 'podman ps --format "{{.Names}}"; sudo podman ps --format "{{.Names}}"'`)
	if out == "?" {
		return out
	}
	n := 0
	for _, name := range strings.Fields(out) {
		if !strings.HasPrefix(name, "consonance-") {
			n++
		}
	}
	return strconv.Itoa(n)
}

// gitCommit is the commit measured, marked when files outside the tests and this document
// differ from it (then the images may not be what the commit builds).
func gitCommit() string {
	commit := command("git", "rev-parse", "--short", "HEAD")
	if changed := command("git", "status", "--porcelain", "--", "../..", ":!../../test", ":!../../docs", ":!../../Makefile"); changed != "" && changed != "?" {
		commit += " + 커밋하지 않은 변경"
	}
	return commit
}

// copyStats is how the copies of main of one condition's branch creations spent their time,
// from the logs of main's supervisor and PostgreSQL and of the agent: the backup start (an
// immediate checkpoint, spec 2), that checkpoint's write and sync phases, and the agent's whole
// copy (backup start, reflink clone, backup stop, pg_wal). The warm-up is included.
type copyStats struct {
	backupStart, ckptWrite, ckptSync, agentCopy []time.Duration
	ckptBuffers                                 []int
}

var (
	ckptStarting = regexp.MustCompile(`checkpoint starting: (.*)`)
	ckptComplete = regexp.MustCompile(`checkpoint complete: wrote ([0-9]+) buffers.* write=([0-9.]+) s, sync=([0-9.]+) s`)
	backupStart  = regexp.MustCompile(`"op":"POST /v1/backup/start".*"durationMs":([0-9]+)`)
)

func copyBreakdown(t *testing.T, ns, prefix string, since time.Time) copyStats {
	t.Helper()
	var s copyStats
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sinceArg := "--since-time=" + since.UTC().Format(time.RFC3339)
	mainLog, err := e2eutil.KubectlContext(ctx, "-n", ns, "logs", "br-main", sinceArg)
	if err != nil {
		t.Logf("main's log: %v", err)
	}
	immediate := false
	for _, line := range strings.Split(mainLog, "\n") {
		if m := ckptStarting.FindStringSubmatch(line); m != nil {
			immediate = strings.Contains(m[1], "immediate")
		} else if m := ckptComplete.FindStringSubmatch(line); m != nil && immediate {
			n, _ := strconv.Atoi(m[1])
			s.ckptBuffers = append(s.ckptBuffers, n)
			s.ckptWrite = append(s.ckptWrite, parseSeconds(m[2]))
			s.ckptSync = append(s.ckptSync, parseSeconds(m[3]))
			immediate = false
		} else if m := backupStart.FindStringSubmatch(line); m != nil {
			ms, _ := strconv.Atoi(m[1])
			s.backupStart = append(s.backupStart, time.Duration(ms)*time.Millisecond)
		}
	}
	agentLog, err := e2eutil.KubectlContext(ctx, "-n", ns, "logs", "volume-agent", sinceArg)
	if err != nil {
		t.Logf("agent's log: %v", err)
	}
	agentCopy := regexp.MustCompile(`"op":"POST /v1/branches/\{name\}/data","branch":"` + prefix + `-[0-9]+".*"status":2[0-9][0-9],"durationMs":([0-9]+)`)
	for _, line := range strings.Split(agentLog, "\n") {
		if m := agentCopy.FindStringSubmatch(line); m != nil {
			ms, _ := strconv.Atoi(m[1])
			s.agentCopy = append(s.agentCopy, time.Duration(ms)*time.Millisecond)
		}
	}
	return s
}

func parseSeconds(s string) time.Duration {
	f, _ := strconv.ParseFloat(s, 64)
	return time.Duration(f * float64(time.Second))
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
