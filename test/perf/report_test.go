//go:build perf

package perf

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
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
	loadTPS  []float64       // write condition, rb: the tps of rb's own load during each run
	latest   time.Duration   // the value of the last run, warm-up included
}

// addLoad records the tps of the load on rb during run i, the warm-up left out.
func (m *metric) addLoad(i int, tps float64) {
	if i > 0 {
		m.loadTPS = append(m.loadTPS, tps)
	}
}

// last is the value of the last run added.
func (m *metric) last() time.Duration { return m.latest }

// add records run i; run 0 is the warm-up, which is kept apart (plan 5 Review Focus 4).
func (m *metric) add(i int, d time.Duration) {
	m.latest = d
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
	copies   copyStats  // the copies of main of the branch creations
	operator []operatorMean
	pending  int // FailedScheduling events in the measured project
}

// results is everything the document reports.
type results struct {
	started, finished time.Time
	runs, scale       int
	copyInterval      time.Duration
	hostStart         string // hostLoad and machinePressure when the run started and ended
	hostEnd           string
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
	w("\n- 측정한 코드: `%s`(저장소 HEAD. 이미지는 `make images load-images deploy-dev`로 올렸고, 이 문서는 이미지가 어느 커밋에서 빌드됐는지 확인하지 않는다)\n\n", r.env.commit)

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
			if len(m.loadTPS) > 0 {
				var sum float64
				for _, v := range m.loadTPS {
					sum += v
				}
				w(". 표본마다 `rb`에도 쓰기 부하(평균 %.0f tps)", sum/float64(len(m.loadTPS)))
			}
			w("\n")
		}
		if cp := c.copies; len(cp.backupStart) > 0 {
			w("- main 복사의 내역(브랜치 생성 %d회, 워밍업 포함, main과 에이전트의 로그에서): 백업 시작(즉시 체크포인트) P50 %s·P95 %s, 그 체크포인트가 쓴 버퍼 P50 %d개(%s), 쓰기 단계 P50 %s·P95 %s, sync 단계 P50 %s·P95 %s; 에이전트의 복사 전체(백업 시작·reflink 복제·백업 종료·pg_wal) P50 %s·P95 %s\n",
				len(cp.backupStart), seconds(percentile(cp.backupStart, 50)), seconds(percentile(cp.backupStart, 95)),
				intPercentile(cp.ckptBuffers, 50), bytesShort(int64(intPercentile(cp.ckptBuffers, 50))*8192),
				seconds(percentile(cp.ckptWrite, 50)), seconds(percentile(cp.ckptWrite, 95)),
				seconds(percentile(cp.ckptSync, 50)), seconds(percentile(cp.ckptSync, 95)),
				seconds(percentile(cp.agentCopy, 50)), seconds(percentile(cp.agentCopy, 95)))
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
	w("- 정지 → 실행: `rb`를 `POST .../stop`(200, Stopped) 뒤 `POST .../start`가 200(Running)을 줄 때까지. 정지 시간도 보조로 적는다.\n")
	w("- 롤백 요청 → rb Running(보조): 롤백 요청을 보낸 때부터 API가 `rb`를 Running으로 보일 때까지, 250ms마다 조회.\n")
	w("- 쓰기 부하 조건의 롤백과 정지 → 실행은 main의 부하에 더해 `rb` 자신에도 `pgbench -c 4 -N`을 같은 방법으로 돌린다. 표본마다 부하를 시작해 `pg_stat_activity`에 활성 세션이 보인 뒤 %s 더 쓰고서 잰다(복구 지점도 그동안 만든다). 롤백과 정지가 `rb`의 PostgreSQL을 내리므로 부하는 표본마다 끝나고 다음 표본에서 다시 시작한다. 정지는 부하가 더럽힌 버퍼의 종료 체크포인트를 치른다.\n", rbLoadHold)
	w("- 프로젝트 생성: `POST /v1/projects`부터 phase Ready까지(init-main 포함), 200ms마다 조회. 매번 지우고 네임스페이스가 사라질 때까지 기다린다.\n")
	w("- 측정한 브랜치는 다음 측정 전에 지우고 사라질 때까지 기다린다. 노드(아래)에 브랜치 파드(500m·1Gi)가 20개 들어가지 않기 때문이다. 두 조건은 차례로 돈다.\n")
	w("- 백분위는 최근접 순위다(20회면 P50은 10번째, P95는 19번째).\n")
	w("- 쓰기 부하: main에 `pgbench -i -s %d`(%s 걸림, 데이터베이스 %s) 뒤, 쓰기 부하 조건 내내 main에서 `pgbench -c 4 -N`. ", r.scale, r.data.initTime.Round(time.Second), bytesString(r.data.bytes))
	w("pgbench는 **main 파드의 postgres 컨테이너 안에서 `kubectl exec`로** 파드의 Unix 소켓에 붙어 돈다. 프로젝트의 `branch-ingress` 정책은 5432를 `consonance-system`과 볼륨 에이전트 라벨(`app.kubernetes.io/component`)의 파드에만 연다. 그 라벨을 단 파드는 에이전트 Service의 엔드포인트가 되어 에이전트로 가는 요청을 받게 되므로 쓸 수 없고, 측정 때문에 제품의 네트워크 정책을 넓히지 않는다. 그래서 부하를 만드는 클라이언트가 부하를 받는 브랜치의 CPU 한도(브랜치 파드 %s)를 PostgreSQL과 나눠 쓴다. 비밀번호는 E2E처럼 API(`GET .../connection`)에서 받아 exec의 표준 입력으로 넘기고 어디에도 찍지 않는다. 클라이언트가 중단(`aborted`)되거나 0 tps인 5초 구간이 있으면 테스트가 실패한다.\n\n", r.env.branchResources)
	causeSection(w, r)

	w("## 측정 환경\n\n")
	w("| 항목 | 값 |\n| --- | --- |\n")
	w("| 호스트 | %s |\n", r.env.host)
	w("| 호스트 상태(시작) | %s |\n", r.hostStart)
	w("| 호스트 상태(끝) | %s |\n", r.hostEnd)
	w("| 머신 | Podman 머신 %s, %s |\n", r.env.machine, r.env.machineOS)
	w("| k3s 컨테이너 | %s |\n", r.env.k3sContainer)
	w("| 노드 | `%s`, %s, CPU %s·메모리 %s(할당 가능 CPU %s·메모리 %s) |\n", r.env.node, r.env.arch, r.env.cpu, r.env.memory, r.env.allocCPU, r.env.allocMemory)
	w("| 커널 | %s |\n", r.env.kernel)
	w("| k3s | %s, %s |\n", r.env.k3s, r.env.runtime)
	w("| 스토리지 | 클래스 `%s`(프로비저너 `%s`), 볼륨 파일 시스템 %s(%s), 볼륨 경로 `%s`, 그 파일 시스템(마운트 지점·크기·남은 공간) %s |\n",
		r.env.storageClass, r.env.provisioner, r.env.fsType, r.env.reflink, r.env.volumePath, r.env.volumeDisk)
	w("| 브랜치 파드(main) | %s |\n", r.env.branchResources)
	w("| 에이전트 파드 | %s |\n", r.env.agentResources)
	w("| 이미지(이미지 (pull 정책)) | %s |\n\n", "`"+strings.Join(r.env.images, "`, `")+"`")

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

func intPercentile(v []int, p float64) int {
	d := make([]time.Duration, len(v))
	for i, x := range v {
		d[i] = time.Duration(x)
	}
	return int(percentile(d, p))
}

func bytesShort(n int64) string { return fmt.Sprintf("%.0fMiB", float64(n)/(1<<20)) }

// causeSection explains, when branch creation under write load misses its criterion, where the
// time goes: this run's copy breakdown, and the bounded experiment of Task 11's fix round 1
// (2026-10-26) on a project like perf, which the run itself does not repeat.
func causeSection(w func(string, ...any), r *results) {
	var failed *conditionResult
	for _, c := range r.conditions {
		if c.name != writeLoad || len(c.metrics) == 0 {
			continue
		}
		if ok, applies := c.metrics[0].passed(); applies && !ok {
			failed = c
		}
	}
	if failed == nil {
		return
	}
	cp := failed.copies
	w("## 쓰기 부하에서 브랜치 생성이 기준을 넘는 까닭\n\n")
	w("- 이 실행에서: 백업 모드 복사가 main에 거는 즉시 체크포인트(`pg_backup_start(fast := true)`, 스펙 2장 \"체크포인트 비용\")가 P50 %s 걸렸다. 그 체크포인트는 버퍼 P50 %d개를 썼고, 쓰기 단계가 P50 %s, sync 단계가 P50 %s다. 에이전트의 복사 전체는 P50 %s다. 부하 없음에서는 같은 체크포인트가 P50 %s다.\n",
		seconds(percentile(cp.backupStart, 50)), intPercentile(cp.ckptBuffers, 50), seconds(percentile(cp.ckptWrite, 50)),
		seconds(percentile(cp.ckptSync, 50)), seconds(percentile(cp.agentCopy, 50)), seconds(percentile(r.conditions[0].copies.backupStart, 50)))
	w("- 체크포인트가 쓰는 버퍼는 직전 체크포인트 뒤에 부하가 더럽힌 페이지다. `-N`은 1GB `pgbench_accounts`의 임의 행을 고치므로 복사 사이(약 7초)에 버퍼가 더럽혀진다. 이 실행에서는 P50 %d개이고, 버퍼 하나가 8KB라서 약 %s다.\n",
		intPercentile(cp.ckptBuffers, 50), bytesShort(int64(intPercentile(cp.ckptBuffers, 50))*8192))
	w("- 시간은 쓰기 단계에서 든다. 2026-10-26의 제한된 실험(같은 크기의 프로젝트, main에 같은 부하, 설정마다 브랜치 생성 5회, main cgroup의 PSI와 `cpu.stat`)에서:\n")
	w("  - 기본 설정: 쓰기 0.94~3.45초, sync 0.02~0.10초. 생성 한 번 사이에 main의 `io.pressure` 0.5~1.5초, CPU 스로틀링 0.2~1.8초, `memory.pressure` 10ms 이하.\n")
	w("  - main을 CPU 2로 늘림(스로틀링 0): 쓰기 1.29~2.78초로 그대로, `io.pressure`는 0.6~1.6초로 늘어남. CPU 한도는 주된 원인이 아니다.\n")
	w("  - `checkpoint_flush_after = 0`(쓰기 중 강제 writeback 끔): 쓰기 1.43~2.85초, sync가 0.37~1.10초로 늘어 합은 그대로. memcg의 더러운 페이지가 105~125MB에서 멈췄다 줄어든다. 1Gi 한도의 memcg에서 dirty 한계에 닿아 쓰기가 장치의 writeback 속도로 묶인다.\n")
	w("  - `bgwriter_lru_maxpages = 1000`, `bgwriter_lru_multiplier = 10`, `bgwriter_delay = 10ms`: 쓰기 시간과 생성 P50이 나아지지 않았다(생성 P50 2.44초). 체크포인트가 쓰는 버퍼 수의 변화는 확인하지 못했다.\n")
	w("  - `checkpoint_timeout = 30s`: 버퍼는 반으로 줄지만 체크포인트가 겹쳐 생성 P50 3.96초, 202 두 번. 더 나쁘다.\n")
	w("  - 머신 디스크: 8k `O_DIRECT` 쓰기 99MB/s, 1M `O_DIRECT` 996MB/s, 8k 버퍼 쓰기 + fsync(순차) 733MB/s. 체크포인트의 흩어진 8k 페이지는 작은 쓰기의 속도(동시 WAL fsync와 나눠 약 60~80MB/s)로 나간다.\n")
	w("  - 부하 중 reflink 복제(`FICLONE`)가 0.3~1.5초로 길어졌다(부하 없음 0.1초 안팎). 원본 파일의 더러운 페이지를 복제 전에 먼저 내보내기 때문일 가능성이 높지만, 따로 가려내지는 못했다.\n")
	w("- 결론(추정): 가장 큰 원인으로 보이는 것은 Podman 머신 가상 디스크의 작은 쓰기 속도이고, 호스트 경합이 두 번째 변수다(아래 측정 환경 표의 호스트 상태 행, 스왑 사용량과 머신 PSI). 위 실험은 호스트 경합을 따로 떼어 보지 않았으므로 둘의 몫은 나누지 못했다. 복사마다 강제 체크포인트를 거는 설계(스펙 2장)가 그 비용을 생성 시간에 넣는다. 위의 설정은 어느 것도 P50을 1초 아래로 내리지 못해 채택하지 않았다. 제품 설정은 바꾸지 않았다.\n")
	if len(failed.metrics) > 1 {
		if q := failed.metrics[1]; q.target > 0 {
			if ok, applies := q.passed(); applies && !ok {
				w("- 같은 조건에서 새 브랜치 첫 쿼리도 기준을 넘었다(P95 %s, 기준 %s). **검증하지 않은 가설**: 새 브랜치는 복사하는 동안(에이전트의 복사 전체 P50 %s) main이 쓴 WAL을 기동할 때 다시 적용하므로 부하가 있으면 첫 접속까지가 길어지고, 여기에 호스트 경합이 더해졌을 것이다. 이 실행에서 가려내지 않았다. 2026-10-26 첫 전체 실행(ce6bce9)에서는 같은 조건의 P95가 1.63초였다.\n",
					seconds(q.value()), seconds(q.target), seconds(percentile(failed.copies.agentCopy, 50)))
			}
		}
	}
	w("\n")
}
