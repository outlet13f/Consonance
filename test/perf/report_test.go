//go:build perf

package perf

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// The statistics a criterion is on (spec 15).
const (
	statP50 = "P50"
	statP95 = "P95"
	statMax = "최대"
)

// metric is one measurement of one condition: the runs after the warm-up, and the criterion.
type metric struct {
	name    string
	stat    string        // what the criterion is on
	target  time.Duration // zero: recorded only
	warmup  time.Duration
	samples []time.Duration

	statuses map[int]int     // branch creation: answers by status
	forward  []time.Duration // first query: the port-forward of the try that answered
	tries    []int           // first query: tries until one answered
	pending  int             // project creation: FailedScheduling events in the projects
}

// add records run i; run 0 is the warm-up, which is kept apart (plan 5 Review Focus 4).
func (m *metric) add(i int, d time.Duration) {
	if i == 0 {
		m.warmup = d
		return
	}
	m.samples = append(m.samples, d)
}

// percentile is the nearest-rank percentile: the smallest sample with at least p% of the samples
// at or below it. For 20 samples P50 is the 10th, P95 the 19th.
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	return s[max(rank, 1)-1]
}

func (m *metric) p50() time.Duration { return percentile(m.samples, 50) }
func (m *metric) p95() time.Duration { return percentile(m.samples, 95) }
func (m *metric) max() time.Duration { return percentile(m.samples, 100) }

// value is the statistic the criterion is on.
func (m *metric) value() time.Duration {
	switch m.stat {
	case statP95:
		return m.p95()
	case statMax:
		return m.max()
	}
	return m.p50()
}

// complete tells whether every run was measured.
func (m *metric) complete() bool { return len(m.samples) == runs }

// passed tells whether the criterion is met; applies is false when there is none.
func (m *metric) passed() (ok, applies bool) {
	if m.target == 0 {
		return false, false
	}
	return m.complete() && m.value() <= m.target, true
}

func (m *metric) criterion() string {
	if m.target == 0 {
		return "없음(기록)"
	}
	return fmt.Sprintf("%s ≤ %s", m.stat, seconds(m.target))
}

// conditionResult is one condition's measurements.
type conditionResult struct {
	name     string
	metrics  []*metric
	load     loadResult // the write condition's
	operator []operatorMean
	pending  int // FailedScheduling events in the measured project
}

// results is everything the document reports.
type results struct {
	started, finished time.Time
	runs, scale       int
	copyInterval      time.Duration
	env               environmentInfo
	data              dataResult
	conditions        []*conditionResult
}

func seconds(d time.Duration) string { return fmt.Sprintf("%.2fs", d.Seconds()) }

func bytesString(n int64) string { return fmt.Sprintf("%.2f GiB (%d bytes)", float64(n)/(1<<30), n) }

// writeDoc writes the result document. It is generated: a run replaces it.
func writeDoc(path string, r *results) error {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# 브랜칭 엔진 성능 측정\n\n")
	w("`make perf`(`test/perf`)가 쓰는 문서다. 손으로 고치지 않고 다시 돌려 바꾼다. 기준은 스펙 15장 2·3항이다.\n\n")
	w("- 측정: %s ~ %s (%s)\n", r.started.Format("2006-01-02 15:04:05 MST"), r.finished.Format("15:04:05"), r.finished.Sub(r.started).Round(time.Second))
	w("- 횟수: 지표·조건마다 워밍업 1회를 버리고 %d회", r.runs)
	if r.runs != 20 {
		w(" (**스펙의 20회가 아닌 시험 실행**)")
	}
	w("\n- 코드: `%s`(이미지는 이 커밋에서 빌드해 `make images load-images deploy-dev`로 올림)\n\n", r.env.commit)

	w("## 결과\n\n")
	w("| 조건 | 지표 | 횟수 | P50 | P95 | 최대 | 기준 | 통과 |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, c := range r.conditions {
		for _, m := range c.metrics {
			verdict := "—"
			if ok, applies := m.passed(); applies && ok {
				verdict = "통과"
			} else if applies {
				verdict = "**실패**"
			}
			if !m.complete() {
				verdict += fmt.Sprintf(" (측정 %d/%d회에서 멈춤)", len(m.samples), r.runs)
			}
			w("| %s | %s | %d | %s | %s | %s | %s | %s |\n", c.name, m.name, len(m.samples),
				seconds(m.p50()), seconds(m.p95()), seconds(m.max()), m.criterion(), verdict)
		}
	}

	w("\n## 보조 수치\n\n")
	for _, c := range r.conditions {
		w("### %s\n\n", c.name)
		for _, m := range c.metrics {
			w("- %s: 워밍업 %s(버림)", m.name, seconds(m.warmup))
			if len(m.statuses) > 0 {
				w(", 응답 상태 %s", statusString(m.statuses))
			}
			if len(m.forward) > 0 {
				var rest []time.Duration
				for i, f := range m.forward {
					rest = append(rest, m.samples[i]-f)
				}
				w(". 성공한 시도의 port-forward 준비 P50 %s·P95 %s, 그것을 뺀 나머지 P50 %s·P95 %s, 시도 횟수 최대 %d",
					seconds(percentile(m.forward, 50)), seconds(percentile(m.forward, 95)),
					seconds(percentile(rest, 50)), seconds(percentile(rest, 95)), slices.Max(m.tries))
			}
			if m.pending > 0 {
				w(", 측정한 프로젝트들의 FailedScheduling 이벤트 %d건", m.pending)
			}
			w("\n")
		}
		w("- 측정 프로젝트 `perf`의 FailedScheduling(자리가 없어 Pending) 이벤트: %d건\n", c.pending)
		if c.load.reports > 0 || c.load.command != "" {
			w("- 쓰기 부하: `%s`, %s 동안, 5초 진행 보고 %d번의 평균 %.0f tps\n", c.load.command, c.load.duration.Round(time.Second), c.load.reports, c.load.tps)
		}
		if len(c.operator) > 0 {
			w("- 오퍼레이터 메트릭(스펙 11장)의 이 조건 동안 평균, 교차 확인용. 워밍업과 같은 종류의 다른 사건도 들어 있다: `branch_first_ready`에는 측정한 프로젝트들의 main(init-main을 기다린다)이, `project_create`에는 워밍업 프로젝트가 들어 있다. 오퍼레이터가 보는 구간이라 API 응답과 port-forward 시간은 없다:\n")
			for _, o := range c.operator {
				w("  - `%s`: %d건, 평균 %s\n", o.name, o.count, seconds(o.mean))
			}
		}
		w("\n")
	}

	w("## 측정 방법\n\n")
	w("- 시간은 Mac의 테스트가 잰다. API 호출은 `kubectl port-forward`로 API 서버 Service에 보내고(계획 5 결정 3), 응답 시간은 요청을 보내 본문을 다 받을 때까지다. port-forward의 왕복이 매번 더해진다.\n")
	w("- 브랜치 생성: `POST .../branches`가 201(데이터 준비 끝, 5초 안)을 줄 때까지. 202는 5초 안에 데이터가 준비되지 않은 것이다. 같은 부모의 백업 모드 복사 사이 최소 간격(`minCopyInterval`, 스펙 7.2) %s에 0.5초를 더해 기다린 뒤 만든다. 간격 제한에 걸린 시간을 재지 않으려는 것이다. 롤백 전의 복구 지점(`rb`의 복사)도 같은 간격을 둔다.\n", r.copyInterval)
	w("- 첫 쿼리: 201을 받은 때부터 새 port-forward → 접속 → `SELECT 1`이 성공할 때까지, 100ms마다 다시 시도한다. port-forward 준비 시간이 들어 있고, 보조 수치에 따로 적는다. 실제 클라이언트는 클러스터 안에서 Service로 바로 붙으므로 이 몫이 없다.\n")
	w("- 롤백: `rb`(main의 복사본, pgbench 데이터 %s)에 복구 지점을 만들고 곧바로 `POST .../rollback`이 200을 줄 때까지. 매번 롤백이 만든 백업 브랜치(정지 상태지만 파드가 노드 자리를 차지한다)와 복구 지점을 지우고, 백업 브랜치가 사라질 때까지 기다린다.\n", bytesString(r.data.bytes))
	w("- 정지 → 실행: `rb`를 `POST .../stop`(200, Stopped) 뒤 `POST .../start`가 200(Running)을 줄 때까지.\n")
	w("- 프로젝트 생성: `POST /v1/projects`부터 phase Ready까지(init-main 포함), 200ms마다 조회. 매번 지우고 네임스페이스가 사라질 때까지 기다린다.\n")
	w("- 측정한 브랜치는 다음 측정 전에 지우고 사라질 때까지 기다린다. 노드(아래)에 브랜치 파드(500m·1Gi)가 20개 들어가지 않기 때문이다. 두 조건은 차례로 돈다.\n")
	w("- 백분위는 최근접 순위다(20회면 P50은 10번째, P95는 19번째).\n")
	w("- 쓰기 부하: main에 `pgbench -i -s %d`(%s 걸림, 데이터베이스 %s) 뒤, 쓰기 부하 조건 내내 main에서 `pgbench -c 4 -N`. ", r.scale, r.data.initTime.Round(time.Second), bytesString(r.data.bytes))
	w("pgbench는 **main 파드의 postgres 컨테이너 안에서 `kubectl exec`로** 파드의 Unix 소켓에 붙어 돈다. 프로젝트의 `branch-ingress` 정책은 5432를 `consonance-system`과 볼륨 에이전트 라벨(`app.kubernetes.io/component`)의 파드에만 연다. 그 라벨을 단 파드는 에이전트 Service의 엔드포인트가 되어 에이전트로 가는 요청을 받게 되므로 쓸 수 없고, 측정 때문에 제품의 네트워크 정책을 넓히지 않는다. 그래서 부하를 만드는 클라이언트가 main의 CPU 한도(%s)를 PostgreSQL과 나눠 쓴다. 비밀번호는 E2E처럼 API(`GET .../connection`)에서 받아 exec의 표준 입력으로 넘기고 어디에도 찍지 않는다.\n\n", r.env.branchCPU)

	w("## 측정 환경\n\n")
	w("| 항목 | 값 |\n| --- | --- |\n")
	w("| 호스트 | %s |\n", r.env.host)
	w("| 머신 | Podman 머신 `podman-machine-default`(applehv, Fedora CoreOS), k3s는 그 안의 rootful 특권 컨테이너 `consonance-k3s` |\n")
	w("| 노드 | `%s`, %s, CPU %s·메모리 %s(할당 가능 CPU %s·메모리 %s) |\n", r.env.node, r.env.arch, r.env.cpu, r.env.memory, r.env.allocCPU, r.env.allocMemory)
	w("| 커널 | %s |\n", r.env.kernel)
	w("| k3s | %s, %s |\n", r.env.k3s, r.env.runtime)
	w("| 스토리지 | 클래스 `%s`(프로비저너 `%s`), 볼륨 파일 시스템 %s(reflink=1, 머신의 `/var/lib/consonance/storage`, 머신 디스크는 Mac SSD 위 가상 디스크). local-path는 PVC 크기를 강제하지 않는다 |\n", r.env.storageClass, r.env.provisioner, r.env.fsType)
	w("| 브랜치 파드 | 요청 = 한도 CPU %s·메모리 %s(프로젝트 기본값), 에이전트 요청 CPU 100m·메모리 128Mi, 한도 CPU 1·메모리 128Mi |\n", r.env.branchCPU, r.env.branchMemory)
	w("| 이미지 | `consonance/postgres:17-dev`, `consonance/agent:dev`, `consonance/operator:dev`, `consonance/apiserver:dev`, 미리 노드에 넣어 둠(`imagePullPolicy: Never`/`IfNotPresent`) |\n\n")

	w("## 원자료(초, 워밍업 제외)\n\n")
	for _, c := range r.conditions {
		for _, m := range c.metrics {
			vals := make([]string, len(m.samples))
			for i, s := range m.samples {
				vals[i] = fmt.Sprintf("%.3f", s.Seconds())
			}
			w("- %s, %s: %s\n", c.name, m.name, strings.Join(vals, " "))
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func statusString(s map[int]int) string {
	var keys []int
	for k := range s {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d×%d", k, s[k]))
	}
	return strings.Join(parts, ", ")
}

// hostInfo describes the Mac the machine runs on, as far as it tells.
func hostInfo() string {
	if runtime.GOOS != "darwin" {
		return runtime.GOOS + "/" + runtime.GOARCH
	}
	out := func(args ...string) string {
		b, err := exec.Command(args[0], args[1:]...).Output()
		if err != nil {
			return "?"
		}
		return strings.TrimSpace(string(b))
	}
	mem := out("sysctl", "-n", "hw.memsize")
	var gib string
	var n int64
	if _, err := fmt.Sscan(mem, &n); err == nil {
		gib = fmt.Sprintf("%d GiB", n>>30)
	}
	return fmt.Sprintf("macOS %s, %s, %s CPU, 메모리 %s", out("sw_vers", "-productVersion"),
		out("sysctl", "-n", "machdep.cpu.brand_string"), out("sysctl", "-n", "hw.ncpu"), gib)
}

// gitCommit is the commit measured, marked when files outside the tests and this document
// differ from it (then the images may not be what the commit builds).
func gitCommit() string {
	head, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "?"
	}
	commit := strings.TrimSpace(string(head))
	changed, err := exec.Command("git", "status", "--porcelain", "--", "../..", ":!../../test", ":!../../docs", ":!../../Makefile").Output()
	if err == nil && len(strings.TrimSpace(string(changed))) > 0 {
		commit += " + 커밋하지 않은 변경"
	}
	return commit
}
