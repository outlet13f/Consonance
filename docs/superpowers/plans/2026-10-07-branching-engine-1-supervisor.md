# 브랜칭 엔진 구현 계획 1/5: 브랜치 감독 프로세스

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 브랜치 파드 안에서 PostgreSQL 17 프로세스를 켜고 끄며, 백업 모드와 접속 계정 생성을 토큰 인증 HTTP API로 제공하는 감독 프로세스와 그 이미지를 만든다.

**Architecture:** `internal/pgctl`이 PostgreSQL 프로세스·백업 세션·부트스트랩을 다룬다. `internal/supervisor`가 이를 HTTP API로 노출하고, `internal/supervisorclient`는 계획 2(에이전트)와 계획 3(오퍼레이터)이 쓸 클라이언트다. `cmd/supervisor`가 이를 묶는다. 이미지 `consonance/postgres:17`은 공식 `postgres:17` 위에 tini(PID 1, 고아 프로세스 회수)와 감독 프로세스를 얹는다.

**Tech Stack:** Go 1.25, `github.com/jackc/pgx/v5`, Go 표준 `net/http`(메서드·와일드카드 라우팅 패턴), PostgreSQL 17, Docker(colima), Lima VM `consonance-poc`(Ubuntu 24.04).

**Spec:** `docs/superpowers/specs/2026-10-07-branching-engine-design.md` — 특히 4.5, 7.3, 7.5, 7.6, 12, 14장.

## Global Constraints

- 모듈 경로 `consonance`, `go 1.25`.
- PostgreSQL 17. 감독 프로세스 기본값: postgres 바이너리 `/usr/lib/postgresql/17/bin/postgres`, 데이터 디렉터리 `/pgdata/current`, 소켓 디렉터리 `/var/run/postgresql`, 포트 5432, API 주소 `:8008`, 토큰 파일 `/etc/consonance/token/token`.
- 내부 API 인증 헤더는 `X-Consonance-Token`.
- fast shutdown 유예 30초, 이후 immediate shutdown. 백업 세션 제한 시간 30초.
- 기동 옵션은 매번 명령행 `-c`로 준다: `port`, `listen_addresses=*`, `unix_socket_directories`, `shared_buffers`(메모리 한도의 25%, 16MB~8192MB, 한도를 모르면 128MB), `max_connections=100`.
- 테넌트 역할 `app_owner`, 데이터베이스 `app`. 슈퍼유저 `postgres`는 로컬 소켓으로만 접속한다(pg_hba 설정은 계획 2의 initdb 단계에서 한다).
- 모든 엔드포인트는 이미 원하는 상태면 아무것도 하지 않고 200. `/healthz`·`/readyz`는 인증 없음. `/readyz`는 PostgreSQL이 접속을 받으면 200, 아니면 503.
- 이미지는 uid/gid 999로 실행한다.
- PostgreSQL이 필요한 테스트는 `CONSONANCE_TEST_PG_BIN`이 없으면 건너뛴다. 실제 실행은 `make test-vm`(Lima VM)으로 한다. Mac에서는 `make test`로 나머지만 돈다.
- 스펙 7.3의 "고아 프로세스를 거둔다"는 이미지의 PID 1인 tini가 맡는다. 감독 프로세스는 tini의 자식이다.
- 모든 커밋 메시지 끝에 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`를 붙인다.

## Review Focus

1. 실행 중 PostgreSQL이 죽는 경우(OOM 등): 상태가 stopped가 되고 `/readyz`가 503을 돌려주며, `/v1/start`로 다시 켤 수 있어야 한다. → Task 3 `TestCrashMovesToStopped`
2. `current`가 없는 디렉터리를 가리키는 경우: `/v1/start`는 원인이 담긴 오류를 돌려주고 감독 프로세스는 계속 응답해야 한다. → Task 3 `TestStartFailsOnMissingDataDir`
3. 백업을 시작한 호출자가 사라진 경우: 제한 시간 뒤 세션이 닫혀 PostgreSQL이 백업을 취소하고, 다음 백업을 바로 시작할 수 있어야 한다. → Task 4 `TestBackupTimeoutAbortsSession`
4. 기동 중에 정지를 요청한 경우: 교착 없이 stopped로 끝나야 한다. → Task 3 `TestStopWhileStarting`
5. 컨테이너 종료(SIGTERM)의 경우: fast shutdown으로 닫혀 클러스터 상태가 "shut down"이어야 한다(다음 기동 때 복구 재생 없음). → Task 8 `TestSupervisorLifecycle`, Task 9 스모크 테스트

---

### Task 1: 저장소 기초와 VM 테스트 환경

**Files:**
- Create: `go.mod`, `.gitignore`, `Makefile`, `hack/vm-setup.sh`, `hack/vm-test.sh`, `internal/testutil/pgtest/pgtest.go`
- Test: `internal/testutil/pgtest/pgtest_test.go`

**Interfaces:**
- Produces: `pgtest.BinDir(t testing.TB) string`, `pgtest.InitDB(t testing.TB, binDir string) string`, `pgtest.InitDBAt(t testing.TB, binDir, dataDir string) string`, `pgtest.FreePort(t testing.TB) int`, `pgtest.SocketDir(t testing.TB) string`, 상수 `pgtest.EnvBinDir = "CONSONANCE_TEST_PG_BIN"`.

- [ ] **Step 1: go.mod와 .gitignore 만들기**

`go.mod`:

```
module consonance

go 1.25
```

`.gitignore`:

```
/bin/
/spikes/
*.test
```

- [ ] **Step 2: 실패하는 테스트 쓰기**

`internal/testutil/pgtest/pgtest_test.go`:

```go
package pgtest_test

import (
	"os"
	"path/filepath"
	"testing"

	"consonance/internal/testutil/pgtest"
)

func TestInitDBCreatesDataDirectory(t *testing.T) {
	bin := pgtest.BinDir(t)
	dir := pgtest.InitDB(t, bin)
	if _, err := os.Stat(filepath.Join(dir, "PG_VERSION")); err != nil {
		t.Fatalf("PG_VERSION missing: %v", err)
	}
}

func TestFreePortIsUsable(t *testing.T) {
	if p := pgtest.FreePort(t); p <= 0 || p > 65535 {
		t.Fatalf("FreePort() = %d", p)
	}
}
```

- [ ] **Step 3: 테스트가 실패하는지 확인**

Run: `go test ./internal/testutil/...`
Expected: FAIL — `package consonance/internal/testutil/pgtest is not in std` 또는 `no non-test Go files`

- [ ] **Step 4: 도우미 구현**

`internal/testutil/pgtest/pgtest.go`:

```go
// Package pgtest provides helpers for tests that need real PostgreSQL binaries.
package pgtest

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// EnvBinDir names the environment variable pointing at PostgreSQL's bin directory.
const EnvBinDir = "CONSONANCE_TEST_PG_BIN"

// BinDir returns the PostgreSQL bin directory or skips the test when it is not configured.
func BinDir(t testing.TB) string {
	t.Helper()
	dir := os.Getenv(EnvBinDir)
	if dir == "" {
		t.Skipf("%s not set; skipping test that needs PostgreSQL binaries", EnvBinDir)
	}
	return dir
}

// InitDB creates a fresh data directory with trust authentication in a temp dir.
func InitDB(t testing.TB, binDir string) string {
	t.Helper()
	return InitDBAt(t, binDir, filepath.Join(t.TempDir(), "data"))
}

// InitDBAt runs initdb into dataDir (superuser "postgres", trust auth) and returns dataDir.
func InitDBAt(t testing.TB, binDir, dataDir string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "initdb"),
		"-D", dataDir, "-U", "postgres", "--auth=trust", "--no-sync")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	return dataDir
}

// FreePort returns a TCP port that was free a moment ago.
func FreePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// SocketDir returns a short directory for unix sockets (socket paths must stay under ~100 bytes).
func SocketDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pgsock")
	if err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
```

- [ ] **Step 5: Mac에서 실행 (PostgreSQL 테스트는 건너뜀)**

Run: `go test -v ./internal/testutil/...`
Expected: `TestFreePortIsUsable` PASS, `TestInitDBCreatesDataDirectory` SKIP, 마지막 줄 `ok  consonance/internal/testutil/pgtest`

- [ ] **Step 6: VM 스크립트와 Makefile 만들기**

`hack/vm-setup.sh` (한 번만 실행: VM을 켜고 Go와 PostgreSQL 17을 설치):

```bash
#!/usr/bin/env bash
# One-time setup of the Lima VM used for tests that need PostgreSQL 17 (and XFS in plan 2).
set -euo pipefail
VM=${VM:-consonance-poc}
GO_VERSION=${GO_VERSION:-1.25.6}

limactl start "$VM" >/dev/null 2>&1 || true
limactl shell "$VM" -- sudo bash -euo pipefail -c "
  export DEBIAN_FRONTEND=noninteractive
  ARCH=\$(dpkg --print-architecture)
  if ! /usr/local/go/bin/go version 2>/dev/null | grep -q 'go$GO_VERSION '; then
    curl -fsSL https://go.dev/dl/go$GO_VERSION.linux-\$ARCH.tar.gz -o /tmp/go.tgz
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
  fi
  if ! command -v gcc >/dev/null; then
    apt-get update -qq
    apt-get install -y -qq gcc >/dev/null   # go test -race needs cgo
  fi
  if [ ! -x /usr/lib/postgresql/17/bin/postgres ]; then
    apt-get update -qq
    apt-get install -y -qq postgresql-common >/dev/null
    /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y >/dev/null
    apt-get install -y -qq postgresql-17 >/dev/null
    systemctl disable --now postgresql >/dev/null 2>&1 || true
  fi
  /usr/local/go/bin/go version
  /usr/lib/postgresql/17/bin/postgres --version
"
```

`hack/vm-test.sh` (VM 안에서 `go test` 실행. 저장소는 Lima가 같은 경로에 읽기 전용으로 마운트한다):

```bash
#!/usr/bin/env bash
# Runs `go test` inside the Lima VM where PostgreSQL 17 is installed.
# Usage: hack/vm-test.sh [go test args...]   (default: ./...)
set -euo pipefail
VM=${VM:-consonance-poc}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
ARGS=("$@")
if [ ${#ARGS[@]} -eq 0 ]; then ARGS=(./...); fi
QUOTED=$(printf ' %q' "${ARGS[@]}")
limactl shell "$VM" -- bash -c "cd $(printf %q "$ROOT") && export PATH=/usr/local/go/bin:\$PATH GOFLAGS=-buildvcs=false CONSONANCE_TEST_PG_BIN=/usr/lib/postgresql/17/bin && go test$QUOTED"
```

`Makefile` (명령 앞은 탭 문자):

```make
.PHONY: test test-vm vm-setup

test: ## host tests (PostgreSQL tests skip)
	go vet ./...
	go test ./...

vm-setup: ## one-time: start the Lima VM and install Go + PostgreSQL 17
	hack/vm-setup.sh

test-vm: ## all tests inside the Lima VM
	hack/vm-test.sh ./...
```

Run: `chmod +x hack/vm-setup.sh hack/vm-test.sh && make vm-setup`
Expected: 마지막 두 줄이 `go version go1.25.6 linux/arm64`와 `postgres (PostgreSQL) 17.x` (gcc도 함께 설치된다)

- [ ] **Step 7: VM에서 테스트 통과 확인**

Run: `hack/vm-test.sh -v ./internal/testutil/...`
Expected: 두 테스트 모두 PASS(SKIP 없음), `ok  consonance/internal/testutil/pgtest`

- [ ] **Step 8: 커밋**

```bash
git add go.mod .gitignore Makefile hack/ internal/testutil/
git commit -m "chore: Go 모듈, VM 테스트 환경, PostgreSQL 테스트 도우미" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: PostgreSQL 기동 옵션

**Files:**
- Create: `internal/pgctl/args.go`
- Test: `internal/pgctl/args_test.go`

**Interfaces:**
- Produces: `type pgctl.Options struct { DataDir, SocketDir string; Port int; MemoryLimitBytes int64; MaxConnections int }`, `pgctl.Args(o Options) []string`, `pgctl.SharedBuffers(limitBytes int64) string`

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/pgctl/args_test.go`:

```go
package pgctl_test

import (
	"slices"
	"testing"

	"consonance/internal/pgctl"
)

func TestArgsIncludeRuntimeSettings(t *testing.T) {
	got := pgctl.Args(pgctl.Options{
		DataDir:          "/pgdata/current",
		SocketDir:        "/var/run/postgresql",
		Port:             5432,
		MemoryLimitBytes: 1 << 30,
		MaxConnections:   100,
	})
	want := []string{
		"-D", "/pgdata/current",
		"-c", "port=5432",
		"-c", "listen_addresses=*",
		"-c", "unix_socket_directories=/var/run/postgresql",
		"-c", "shared_buffers=256MB",
		"-c", "max_connections=100",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Args() =\n%q\nwant\n%q", got, want)
	}
}

func TestSharedBuffersIsQuarterOfMemoryLimit(t *testing.T) {
	cases := []struct {
		limit int64
		want  string
	}{
		{0, "128MB"},         // unknown limit: PostgreSQL's default
		{1 << 30, "256MB"},   // 1GiB
		{2 << 30, "512MB"},   // 2GiB
		{32 << 20, "16MB"},   // 32MiB -> 8MB, clamped up to 16MB
		{64 << 30, "8192MB"}, // 64GiB -> 16GB, clamped down to 8GB
	}
	for _, c := range cases {
		if got := pgctl.SharedBuffers(c.limit); got != c.want {
			t.Errorf("SharedBuffers(%d) = %s, want %s", c.limit, got, c.want)
		}
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/pgctl/...`
Expected: FAIL — `undefined: pgctl.Args`

- [ ] **Step 3: 구현**

`internal/pgctl/args.go`:

```go
// Package pgctl runs and controls one PostgreSQL server process.
package pgctl

import "fmt"

// Options configure how the postmaster is launched.
type Options struct {
	DataDir          string // data directory, usually the generation symlink /pgdata/current
	SocketDir        string // unix_socket_directories
	Port             int
	MemoryLimitBytes int64 // container memory limit; 0 when unknown
	MaxConnections   int
}

// Args returns the postgres command-line arguments. All runtime settings are passed
// with -c so branches copied from one another never inherit each other's settings.
func Args(o Options) []string {
	return []string{
		"-D", o.DataDir,
		"-c", fmt.Sprintf("port=%d", o.Port),
		"-c", "listen_addresses=*",
		"-c", "unix_socket_directories=" + o.SocketDir,
		"-c", "shared_buffers=" + SharedBuffers(o.MemoryLimitBytes),
		"-c", fmt.Sprintf("max_connections=%d", o.MaxConnections),
	}
}

// SharedBuffers is 25% of the memory limit in MB, clamped to [16MB, 8192MB].
// It returns PostgreSQL's default of 128MB when the limit is unknown.
func SharedBuffers(limitBytes int64) string {
	if limitBytes <= 0 {
		return "128MB"
	}
	mb := limitBytes / 4 / (1 << 20)
	mb = max(mb, 16)
	mb = min(mb, 8192)
	return fmt.Sprintf("%dMB", mb)
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/pgctl/...`
Expected: `ok  consonance/internal/pgctl`

- [ ] **Step 5: 커밋**

```bash
git add internal/pgctl/
git commit -m "feat(pgctl): PostgreSQL 기동 옵션과 shared_buffers 계산" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: PostgreSQL 프로세스 관리자

**Files:**
- Create: `internal/pgctl/process.go`
- Test: `internal/pgctl/process_test.go`
- Modify: `go.mod`, `go.sum` (pgx 추가)

**Interfaces:**
- Consumes: Task 2 `Options`, `Args`; Task 1 `pgtest.*`
- Produces:
  - `type pgctl.State string` 상수 `StateStopped="stopped"`, `StateStarting="starting"`, `StateRunning="running"`, `StateStopping="stopping"`
  - `var pgctl.ErrBusy error`
  - `pgctl.NewManager(postgresBin string, opts Options) *Manager`
  - `(*Manager).Start(ctx context.Context) error`, `(*Manager).Stop(ctx context.Context, grace time.Duration) error`, `(*Manager).State() (State, time.Time)`, `(*Manager).Ready(ctx context.Context) bool`, `(*Manager).PID() int`, `(*Manager).ConnString() string`
  - `pgctl.LocalConnString(socketDir string, port int) string`

- [ ] **Step 1: pgx 의존성 추가**

Run: `go get github.com/jackc/pgx/v5@latest`
Expected: `go.mod`에 `require github.com/jackc/pgx/v5 v5.x.y`가 생긴다.

- [ ] **Step 2: 실패하는 테스트 쓰기**

`internal/pgctl/process_test.go`:

```go
package pgctl_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"consonance/internal/pgctl"
	"consonance/internal/testutil/pgtest"
)

// newTestManager returns a stopped Manager over a fresh data directory.
func newTestManager(t *testing.T) *pgctl.Manager {
	t.Helper()
	bin := pgtest.BinDir(t)
	m := pgctl.NewManager(filepath.Join(bin, "postgres"), pgctl.Options{
		DataDir:        pgtest.InitDB(t, bin),
		SocketDir:      pgtest.SocketDir(t),
		Port:           pgtest.FreePort(t),
		MaxConnections: 20,
	})
	t.Cleanup(func() { _ = m.Stop(context.Background(), 5*time.Second) })
	return m
}

// startedManager returns a running Manager.
func startedManager(t *testing.T) *pgctl.Manager {
	t.Helper()
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	return m
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestStartStopRestart(t *testing.T) {
	m := newTestManager(t)
	ctx := testCtx(t)

	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if s, _ := m.State(); s != pgctl.StateRunning {
		t.Fatalf("state = %s, want running", s)
	}
	if !m.Ready(ctx) {
		t.Fatal("not ready after start")
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("second start must be a no-op: %v", err)
	}
	if err := m.Stop(ctx, 10*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if s, _ := m.State(); s != pgctl.StateStopped {
		t.Fatalf("state = %s, want stopped", s)
	}
	if m.Ready(ctx) {
		t.Fatal("ready after stop")
	}
	if err := m.Stop(ctx, time.Second); err != nil {
		t.Fatalf("second stop must be a no-op: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}
}

func TestCrashMovesToStopped(t *testing.T) {
	m := startedManager(t)
	ctx := testCtx(t)

	// Kill the whole postgres process group, as an OOM kill would.
	if err := syscall.Kill(-m.PID(), syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, _ := m.State(); s == pgctl.StateStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state did not become stopped after the crash")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m.Ready(ctx) {
		t.Fatal("ready after crash")
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start after crash: %v", err)
	}
}

func TestStartFailsOnMissingDataDir(t *testing.T) {
	bin := pgtest.BinDir(t)
	m := pgctl.NewManager(filepath.Join(bin, "postgres"), pgctl.Options{
		DataDir:        filepath.Join(t.TempDir(), "current"), // nothing there
		SocketDir:      pgtest.SocketDir(t),
		Port:           pgtest.FreePort(t),
		MaxConnections: 20,
	})
	err := m.Start(testCtx(t))
	if err == nil {
		t.Fatal("Start succeeded on a missing data directory")
	}
	if !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("unexpected error: %v", err)
	}
	if s, _ := m.State(); s != pgctl.StateStopped {
		t.Fatalf("state = %s, want stopped", s)
	}
}

func TestStopWhileStarting(t *testing.T) {
	m := newTestManager(t)
	ctx := testCtx(t)

	startErr := make(chan error, 1)
	go func() { startErr <- m.Start(ctx) }()
	for m.PID() == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := m.Stop(ctx, 10*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	<-startErr // nil (became ready first) or an exit error; both are fine
	if s, _ := m.State(); s != pgctl.StateStopped {
		t.Fatalf("state = %s, want stopped", s)
	}
}

func TestConcurrentStartLaunchesOnePostmaster(t *testing.T) {
	m := newTestManager(t)
	ctx := testCtx(t)

	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- m.Start(ctx) }()
	}
	ok := 0
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			ok++
		case errors.Is(err, pgctl.ErrBusy):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok == 0 {
		t.Fatal("no start succeeded")
	}
	if s, _ := m.State(); s != pgctl.StateRunning {
		t.Fatalf("state = %s, want running", s)
	}
}
```

- [ ] **Step 3: 테스트가 실패하는지 확인**

Run: `go test ./internal/pgctl/...`
Expected: FAIL — `undefined: pgctl.NewManager`

- [ ] **Step 4: 구현**

`internal/pgctl/process.go`:

```go
package pgctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// State is the lifecycle state of the managed postmaster.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
)

// ErrBusy is returned when a start is requested while a start or stop is in progress.
var ErrBusy = errors.New("postgres is starting or stopping")

// Manager runs one postmaster as a child process.
type Manager struct {
	postgresBin string
	opts        Options

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{} // closed after the current postmaster exits and state is updated
	exitErr error
	state   State
	since   time.Time
}

// NewManager returns a Manager in the stopped state.
func NewManager(postgresBin string, opts Options) *Manager {
	return &Manager{postgresBin: postgresBin, opts: opts, state: StateStopped, since: time.Now()}
}

// LocalConnString returns a connection string for the superuser over the unix socket.
func LocalConnString(socketDir string, port int) string {
	return fmt.Sprintf("host=%s port=%d user=postgres dbname=postgres sslmode=disable connect_timeout=2", socketDir, port)
}

// ConnString returns the local superuser connection string for this server.
func (m *Manager) ConnString() string {
	return LocalConnString(m.opts.SocketDir, m.opts.Port)
}

func (m *Manager) setStateLocked(s State) {
	m.state = s
	m.since = time.Now()
}

// State reports the current state and when it began.
func (m *Manager) State() (State, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, m.since
}

// PID returns the postmaster's process id, or 0 when it is not running.
func (m *Manager) PID() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.cmd.Process == nil {
		return 0
	}
	return m.cmd.Process.Pid
}

// Ready reports whether postgres accepts connections right now.
func (m *Manager) Ready(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, m.ConnString())
	if err != nil {
		return false
	}
	defer conn.Close(context.Background())
	return conn.Ping(ctx) == nil
}

// Start launches postgres if needed and waits until it accepts connections.
// It is a no-op when postgres is already running and returns ErrBusy while
// another start or a stop is in progress.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	switch m.state {
	case StateRunning:
		m.mu.Unlock()
		return nil
	case StateStarting, StateStopping:
		m.mu.Unlock()
		return ErrBusy
	}
	cmd := exec.Command(m.postgresBin, Args(m.opts)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Own process group, so signals aimed at the supervisor never reach postgres directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("start postgres: %w", err)
	}
	done := make(chan struct{})
	m.cmd, m.done, m.exitErr = cmd, done, nil
	m.setStateLocked(StateStarting)
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		if m.cmd == cmd {
			m.cmd, m.exitErr = nil, err
			m.setStateLocked(StateStopped)
		}
		m.mu.Unlock()
		close(done)
	}()

	ready := make(chan struct{})
	go m.awaitReady(cmd, done, ready)

	select {
	case <-ready:
		return nil
	case <-done:
		m.mu.Lock()
		err := m.exitErr
		m.mu.Unlock()
		return fmt.Errorf("postgres exited during startup: %v", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitReady polls until cmd accepts connections, then marks it running.
// It keeps going after Start's caller gives up, so the state never sticks at "starting".
func (m *Manager) awaitReady(cmd *exec.Cmd, done, ready chan struct{}) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		if !m.Ready(context.Background()) {
			continue
		}
		m.mu.Lock()
		if m.cmd == cmd && m.state == StateStarting {
			m.setStateLocked(StateRunning)
		}
		m.mu.Unlock()
		close(ready)
		return
	}
}

// Stop shuts postgres down with a fast shutdown (SIGINT). If it has not exited
// after grace, or ctx ends first, it escalates to an immediate shutdown (SIGQUIT).
// It is a no-op when postgres is not running.
func (m *Manager) Stop(ctx context.Context, grace time.Duration) error {
	m.mu.Lock()
	cmd, done := m.cmd, m.done
	if cmd == nil {
		m.mu.Unlock()
		return nil
	}
	m.setStateLocked(StateStopping)
	m.mu.Unlock()

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal postgres: %w", err)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
	case <-ctx.Done():
	}
	_ = cmd.Process.Signal(syscall.SIGQUIT)
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("postgres did not exit after immediate shutdown")
	}
}
```

- [ ] **Step 5: 의존성 정리 후 Mac에서 컴파일·건너뛰기 확인**

Run: `go mod tidy && go vet ./... && go test ./internal/pgctl/...`
Expected: `ok  consonance/internal/pgctl` (PostgreSQL 테스트는 SKIP)

- [ ] **Step 6: VM에서 테스트 통과 확인**

Run: `hack/vm-test.sh -race -v ./internal/pgctl/...`
Expected: `TestStartStopRestart`, `TestCrashMovesToStopped`, `TestStartFailsOnMissingDataDir`, `TestStopWhileStarting`, `TestConcurrentStartLaunchesOnePostmaster` 모두 PASS, 경쟁 상태 경고 없음

- [ ] **Step 7: 커밋**

```bash
git add go.mod go.sum internal/pgctl/
git commit -m "feat(pgctl): PostgreSQL 프로세스 기동·정지·상태 관리" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: 백업 세션

**Files:**
- Create: `internal/pgctl/backup.go`
- Test: `internal/pgctl/backup_test.go`

**Interfaces:**
- Consumes: Task 3 `Manager.ConnString()`, 테스트 도우미 `startedManager`, `testCtx`(같은 `pgctl_test` 패키지)
- Produces: `var pgctl.ErrBackupInProgress, pgctl.ErrNoBackup error`, `pgctl.NewBackups(connString string, timeout time.Duration) *Backups`, `(*Backups).Begin(ctx context.Context, label string) (startLSN string, err error)`, `(*Backups).End(ctx context.Context) (labelFile, stopLSN string, err error)`, `(*Backups).Abort()`, `(*Backups).InProgress() bool`

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/pgctl/backup_test.go`:

```go
package pgctl_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"consonance/internal/pgctl"
)

func TestBackupBeginEnd(t *testing.T) {
	m := startedManager(t)
	ctx := testCtx(t)
	b := pgctl.NewBackups(m.ConnString(), 30*time.Second)

	lsn, err := b.Begin(ctx, "test-label")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if !strings.Contains(lsn, "/") {
		t.Fatalf("start LSN = %q, want an LSN like 0/2000028", lsn)
	}
	if _, err := b.Begin(ctx, "again"); !errors.Is(err, pgctl.ErrBackupInProgress) {
		t.Fatalf("second begin: err = %v, want ErrBackupInProgress", err)
	}
	label, stopLSN, err := b.End(ctx)
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if !strings.Contains(label, "LABEL: test-label") {
		t.Fatalf("label file does not name the backup:\n%s", label)
	}
	if stopLSN == "" {
		t.Fatal("empty stop LSN")
	}
	if _, _, err := b.End(ctx); !errors.Is(err, pgctl.ErrNoBackup) {
		t.Fatalf("second end: err = %v, want ErrNoBackup", err)
	}
}

func TestBackupTimeoutAbortsSession(t *testing.T) {
	m := startedManager(t)
	ctx := testCtx(t)
	b := pgctl.NewBackups(m.ConnString(), 200*time.Millisecond)

	if _, err := b.Begin(ctx, "will-time-out"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if b.InProgress() {
		t.Fatal("session still open after the timeout")
	}
	// PostgreSQL aborted the backup when the session closed, so a new one starts at once.
	if _, err := b.Begin(ctx, "after-timeout"); err != nil {
		t.Fatalf("begin after timeout: %v", err)
	}
	if _, _, err := b.End(ctx); err != nil {
		t.Fatalf("end: %v", err)
	}
}

func TestBackupBeginFailsWhenPostgresIsDown(t *testing.T) {
	m := newTestManager(t) // never started
	b := pgctl.NewBackups(m.ConnString(), time.Second)
	if _, err := b.Begin(testCtx(t), "x"); err == nil {
		t.Fatal("begin succeeded with postgres down")
	}
	if b.InProgress() {
		t.Fatal("failed begin left a session open")
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/pgctl/...`
Expected: FAIL — `undefined: pgctl.NewBackups`

- [ ] **Step 3: 구현**

`internal/pgctl/backup.go`:

```go
package pgctl

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrBackupInProgress = errors.New("a backup is already in progress")
	ErrNoBackup         = errors.New("no backup in progress")
)

// Backups runs at most one non-exclusive base backup at a time.
//
// PostgreSQL ties a non-exclusive backup to the session that started it, so the
// session stays open between Begin and End. When End does not arrive within the
// timeout, the session is closed and PostgreSQL aborts the backup by itself.
type Backups struct {
	connString string
	timeout    time.Duration

	mu    sync.Mutex
	conn  *pgx.Conn
	timer *time.Timer
}

// NewBackups returns a backup controller for the server at connString.
func NewBackups(connString string, timeout time.Duration) *Backups {
	return &Backups{connString: connString, timeout: timeout}
}

// Begin opens a dedicated session and runs pg_backup_start(label, fast := true),
// which forces an immediate checkpoint. It returns the backup start LSN.
func (b *Backups) Begin(ctx context.Context, label string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return "", ErrBackupInProgress
	}
	conn, err := pgx.Connect(ctx, b.connString)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	var lsn string
	if err := conn.QueryRow(ctx, "SELECT pg_backup_start($1, true)::text", label).Scan(&lsn); err != nil {
		conn.Close(context.Background())
		return "", fmt.Errorf("pg_backup_start: %w", err)
	}
	b.conn = conn
	b.timer = time.AfterFunc(b.timeout, b.Abort)
	return lsn, nil
}

// End runs pg_backup_stop(false) on the backup session and closes it.
// It returns the backup_label contents and the stop LSN.
func (b *Backups) End(ctx context.Context) (labelFile, stopLSN string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return "", "", ErrNoBackup
	}
	b.timer.Stop()
	conn := b.conn
	b.conn, b.timer = nil, nil
	defer conn.Close(context.Background())
	if err := conn.QueryRow(ctx, "SELECT lsn::text, labelfile FROM pg_backup_stop(false)").Scan(&stopLSN, &labelFile); err != nil {
		return "", "", fmt.Errorf("pg_backup_stop: %w", err)
	}
	return labelFile, stopLSN, nil
}

// Abort closes the backup session if one is open; PostgreSQL then aborts the backup.
func (b *Backups) Abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	b.conn.Close(context.Background())
	b.conn, b.timer = nil, nil
}

// InProgress reports whether a backup session is open.
func (b *Backups) InProgress() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn != nil
}
```

- [ ] **Step 4: Mac에서 컴파일 확인, VM에서 테스트 통과 확인**

Run: `go vet ./... && hack/vm-test.sh -race -v -run 'Backup' ./internal/pgctl/...`
Expected: `TestBackupBeginEnd`, `TestBackupTimeoutAbortsSession`, `TestBackupBeginFailsWhenPostgresIsDown` PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/pgctl/backup.go internal/pgctl/backup_test.go
git commit -m "feat(pgctl): 세션에 묶인 비배타적 백업 시작·종료와 자동 취소" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: 접속 계정 부트스트랩

**Files:**
- Create: `internal/pgctl/bootstrap.go`
- Test: `internal/pgctl/bootstrap_test.go`

**Interfaces:**
- Consumes: Task 3 `startedManager`, `testCtx`, `Manager.ConnString()`
- Produces: `pgctl.Bootstrap(ctx context.Context, connString, role, database, password string) error`, 상수 `pgctl.TenantRole = "app_owner"`, `pgctl.TenantDatabase = "app"`

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/pgctl/bootstrap_test.go`:

```go
package pgctl_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"consonance/internal/pgctl"
)

func TestBootstrapCreatesRoleAndDatabaseOnce(t *testing.T) {
	m := startedManager(t)
	ctx := testCtx(t)

	if err := pgctl.Bootstrap(ctx, m.ConnString(), pgctl.TenantRole, pgctl.TenantDatabase, "first-pw"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := pgctl.Bootstrap(ctx, m.ConnString(), pgctl.TenantRole, pgctl.TenantDatabase, "second-pw"); err != nil {
		t.Fatalf("second bootstrap must be a no-op: %v", err)
	}

	conn, err := pgx.Connect(ctx, m.ConnString())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	var owner string
	if err := conn.QueryRow(ctx,
		"SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1", pgctl.TenantDatabase).Scan(&owner); err != nil {
		t.Fatalf("query owner: %v", err)
	}
	if owner != pgctl.TenantRole {
		t.Fatalf("database owner = %q, want %q", owner, pgctl.TenantRole)
	}
	var super, login bool
	if err := conn.QueryRow(ctx,
		"SELECT rolsuper, rolcanlogin FROM pg_roles WHERE rolname = $1", pgctl.TenantRole).Scan(&super, &login); err != nil {
		t.Fatalf("query role: %v", err)
	}
	if super || !login {
		t.Fatalf("role flags: superuser=%v login=%v, want superuser=false login=true", super, login)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/pgctl/...`
Expected: FAIL — `undefined: pgctl.Bootstrap`

- [ ] **Step 3: 구현**

`internal/pgctl/bootstrap.go`:

```go
package pgctl

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Tenant role and database every project gets (spec 4.5).
const (
	TenantRole     = "app_owner"
	TenantDatabase = "app"
)

// Bootstrap creates the tenant login role and its database when they do not exist.
// It is safe to call repeatedly; an existing role keeps its password.
func Bootstrap(ctx context.Context, connString, role, database, password string) error {
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
		return fmt.Errorf("check role: %w", err)
	}
	if !exists {
		if err := execFormatted(ctx, conn,
			"SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', $1::text, $2::text)", role, password); err != nil {
			return fmt.Errorf("create role: %w", err)
		}
	}
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", database).Scan(&exists); err != nil {
		return fmt.Errorf("check database: %w", err)
	}
	if !exists {
		if err := execFormatted(ctx, conn,
			"SELECT format('CREATE DATABASE %I OWNER %I', $1::text, $2::text)", database, role); err != nil {
			return fmt.Errorf("create database: %w", err)
		}
	}
	return nil
}

// execFormatted builds a statement with server-side format() quoting, then runs it.
// Identifiers and literals never pass through Go string concatenation.
func execFormatted(ctx context.Context, conn *pgx.Conn, formatQuery string, args ...any) error {
	var stmt string
	if err := conn.QueryRow(ctx, formatQuery, args...).Scan(&stmt); err != nil {
		return err
	}
	_, err := conn.Exec(ctx, stmt)
	return err
}
```

- [ ] **Step 4: VM에서 테스트 통과 확인**

Run: `go vet ./... && hack/vm-test.sh -race -v -run 'Bootstrap' ./internal/pgctl/...`
Expected: `TestBootstrapCreatesRoleAndDatabaseOnce` PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/pgctl/bootstrap.go internal/pgctl/bootstrap_test.go
git commit -m "feat(pgctl): 테넌트 역할과 데이터베이스 부트스트랩" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: 감독 프로세스 HTTP API

**Files:**
- Create: `internal/supervisorapi/types.go`, `internal/supervisor/server.go`, `internal/supervisor/supervisortest/fakes.go`
- Test: `internal/supervisor/server_test.go`

**Interfaces:**
- Consumes: `pgctl.State`, `pgctl.StateRunning`, `pgctl.ErrBusy`, `pgctl.ErrBackupInProgress`, `pgctl.ErrNoBackup`
- Produces:
  - `supervisorapi`: `TokenHeader = "X-Consonance-Token"`, `DefaultPort = 8008`, `type PGState string`(`PGStopped`, `PGStarting`, `PGRunning`, `PGStopping`), `type Status struct { PG PGState; Generation string; Since time.Time }`, `BackupStartRequest{Label string}`, `BackupStartResponse{StartLSN string}`, `BackupStopResponse{LabelFile, StopLSN string}`, `Error{Error string}`
  - `supervisor.Postgres` 인터페이스(Start, Stop, State, Ready), `supervisor.Backups` 인터페이스(Begin, End), `type supervisor.Server struct { Token string; PG Postgres; Backups Backups; Bootstrap func(context.Context) error; Generation func() string; StopGrace, StartTimeout time.Duration }`, `(*Server).Handler() http.Handler`
  - `supervisortest.NewFakePostgres(state pgctl.State) *FakePostgres`(필드 `StartErr`, `StopErr`), `supervisortest.FakeBackups`(필드 `BeginErr`)

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/supervisor/server_test.go`:

```go
package supervisor_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"consonance/internal/pgctl"
	"consonance/internal/supervisor"
	"consonance/internal/supervisor/supervisortest"
	"consonance/internal/supervisorapi"
)

const token = "t0k"

func newHandler(pg *supervisortest.FakePostgres, b *supervisortest.FakeBackups, bootstrap func(context.Context) error) http.Handler {
	s := &supervisor.Server{
		Token:        token,
		PG:           pg,
		Backups:      b,
		Bootstrap:    bootstrap,
		Generation:   func() string { return "data-1" },
		StopGrace:    time.Second,
		StartTimeout: time.Second,
	}
	return s.Handler()
}

func call(h http.Handler, method, path, tok, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set(supervisorapi.TokenHeader, tok)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestHealthzNeedsNoToken(t *testing.T) {
	h := newHandler(supervisortest.NewFakePostgres(pgctl.StateStopped), &supervisortest.FakeBackups{}, nil)
	if rec := call(h, "GET", "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}
}

func TestReadyzFollowsPostgres(t *testing.T) {
	running := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{}, nil)
	if rec := call(running, "GET", "/readyz", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("readyz while running = %d, want 200", rec.Code)
	}
	stopped := newHandler(supervisortest.NewFakePostgres(pgctl.StateStopped), &supervisortest.FakeBackups{}, nil)
	if rec := call(stopped, "GET", "/readyz", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while stopped = %d, want 503", rec.Code)
	}
}

func TestRejectsMissingOrWrongToken(t *testing.T) {
	h := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{}, nil)
	for _, tok := range []string{"", "nope"} {
		if rec := call(h, "GET", "/v1/status", tok, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", tok, rec.Code)
		}
	}
}

func TestStartReportsRunningStatus(t *testing.T) {
	pg := supervisortest.NewFakePostgres(pgctl.StateStopped)
	h := newHandler(pg, &supervisortest.FakeBackups{}, nil)
	rec := call(h, "POST", "/v1/start", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	st := decode[supervisorapi.Status](t, rec)
	if st.PG != supervisorapi.PGRunning || st.Generation != "data-1" {
		t.Fatalf("status = %+v, want running on data-1", st)
	}
}

func TestStartWhileBusyIs409(t *testing.T) {
	pg := supervisortest.NewFakePostgres(pgctl.StateStarting)
	pg.StartErr = pgctl.ErrBusy
	h := newHandler(pg, &supervisortest.FakeBackups{}, nil)
	if rec := call(h, "POST", "/v1/start", token, ""); rec.Code != http.StatusConflict {
		t.Fatalf("start while busy = %d, want 409", rec.Code)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	pg := supervisortest.NewFakePostgres(pgctl.StateRunning)
	h := newHandler(pg, &supervisortest.FakeBackups{}, nil)
	for i := range 2 {
		rec := call(h, "POST", "/v1/stop", token, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("stop #%d = %d", i+1, rec.Code)
		}
		if st := decode[supervisorapi.Status](t, rec); st.PG != supervisorapi.PGStopped {
			t.Fatalf("stop #%d status = %s, want stopped", i+1, st.PG)
		}
	}
}

func TestBackupStartNeedsLabel(t *testing.T) {
	h := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{}, nil)
	if rec := call(h, "POST", "/v1/backup/start", token, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("backup without label = %d, want 400", rec.Code)
	}
}

func TestBackupStartNeedsRunningPostgres(t *testing.T) {
	h := newHandler(supervisortest.NewFakePostgres(pgctl.StateStopped), &supervisortest.FakeBackups{}, nil)
	if rec := call(h, "POST", "/v1/backup/start", token, `{"label":"x"}`); rec.Code != http.StatusConflict {
		t.Fatalf("backup while stopped = %d, want 409", rec.Code)
	}
}

func TestBackupRoundTrip(t *testing.T) {
	h := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{}, nil)

	rec := call(h, "POST", "/v1/backup/start", token, `{"label":"b1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("backup start = %d %s", rec.Code, rec.Body)
	}
	if got := decode[supervisorapi.BackupStartResponse](t, rec); got.StartLSN == "" {
		t.Fatal("empty start LSN")
	}
	if rec := call(h, "POST", "/v1/backup/start", token, `{"label":"b2"}`); rec.Code != http.StatusConflict {
		t.Fatalf("second backup start = %d, want 409", rec.Code)
	}
	rec = call(h, "POST", "/v1/backup/stop", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("backup stop = %d %s", rec.Code, rec.Body)
	}
	if got := decode[supervisorapi.BackupStopResponse](t, rec); !strings.Contains(got.LabelFile, "LABEL: b1") {
		t.Fatalf("label file = %q", got.LabelFile)
	}
	if rec := call(h, "POST", "/v1/backup/stop", token, ""); rec.Code != http.StatusConflict {
		t.Fatalf("backup stop without session = %d, want 409", rec.Code)
	}
}

func TestBootstrapOnlyOnMain(t *testing.T) {
	notMain := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{}, nil)
	if rec := call(notMain, "POST", "/v1/bootstrap", token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bootstrap on non-main = %d, want 404", rec.Code)
	}
	calls := 0
	main := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{},
		func(context.Context) error { calls++; return nil })
	if rec := call(main, "POST", "/v1/bootstrap", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("bootstrap on main = %d %s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("bootstrap called %d times, want 1", calls)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/supervisor/...`
Expected: FAIL — `package consonance/internal/supervisor is not in std` 또는 `undefined: supervisor.Server`

- [ ] **Step 3: 통신 규약 타입 구현**

`internal/supervisorapi/types.go`:

```go
// Package supervisorapi is the wire contract of the branch supervisor's internal HTTP API.
package supervisorapi

import "time"

const (
	// TokenHeader carries the per-project internal token.
	TokenHeader = "X-Consonance-Token"
	// DefaultPort is the supervisor API port.
	DefaultPort = 8008
)

// PGState is the PostgreSQL process state as reported by the supervisor.
type PGState string

const (
	PGStopped  PGState = "stopped"
	PGStarting PGState = "starting"
	PGRunning  PGState = "running"
	PGStopping PGState = "stopping"
)

// Status is returned by GET /v1/status and by the start, stop and bootstrap calls.
type Status struct {
	PG         PGState   `json:"pg"`
	Generation string    `json:"generation"` // target of /pgdata/current, e.g. "data-3"
	Since      time.Time `json:"since"`
}

// BackupStartRequest is the body of POST /v1/backup/start.
type BackupStartRequest struct {
	Label string `json:"label"`
}

// BackupStartResponse is returned by POST /v1/backup/start.
type BackupStartResponse struct {
	StartLSN string `json:"startLSN"`
}

// BackupStopResponse is returned by POST /v1/backup/stop.
type BackupStopResponse struct {
	LabelFile string `json:"labelFile"`
	StopLSN   string `json:"stopLSN"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}
```

- [ ] **Step 4: 테스트용 가짜 구현**

`internal/supervisor/supervisortest/fakes.go`:

```go
// Package supervisortest provides in-memory fakes of the supervisor's dependencies.
package supervisortest

import (
	"context"
	"sync"
	"time"

	"consonance/internal/pgctl"
)

// FakePostgres implements supervisor.Postgres in memory.
type FakePostgres struct {
	mu       sync.Mutex
	state    pgctl.State
	StartErr error
	StopErr  error
}

// NewFakePostgres returns a FakePostgres in the given state.
func NewFakePostgres(state pgctl.State) *FakePostgres {
	return &FakePostgres{state: state}
}

func (f *FakePostgres) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StartErr != nil {
		return f.StartErr
	}
	f.state = pgctl.StateRunning
	return nil
}

func (f *FakePostgres) Stop(ctx context.Context, grace time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StopErr != nil {
		return f.StopErr
	}
	f.state = pgctl.StateStopped
	return nil
}

func (f *FakePostgres) State() (pgctl.State, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, time.Unix(0, 0).UTC()
}

func (f *FakePostgres) Ready(ctx context.Context) bool {
	s, _ := f.State()
	return s == pgctl.StateRunning
}

// FakeBackups implements supervisor.Backups in memory.
type FakeBackups struct {
	mu       sync.Mutex
	open     bool
	label    string
	BeginErr error
}

func (f *FakeBackups) Begin(ctx context.Context, label string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.BeginErr != nil {
		return "", f.BeginErr
	}
	if f.open {
		return "", pgctl.ErrBackupInProgress
	}
	f.open, f.label = true, label
	return "0/2000028", nil
}

func (f *FakeBackups) End(ctx context.Context) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open {
		return "", "", pgctl.ErrNoBackup
	}
	f.open = false
	return "START WAL LOCATION: 0/2000028\nLABEL: " + f.label + "\n", "0/2000100", nil
}
```

- [ ] **Step 5: 서버 구현**

`internal/supervisor/server.go`:

```go
// Package supervisor serves the branch supervisor's internal HTTP API (spec 7.3).
package supervisor

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"consonance/internal/pgctl"
	"consonance/internal/supervisorapi"
)

// Postgres is the process control the server needs (implemented by *pgctl.Manager).
type Postgres interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context, grace time.Duration) error
	State() (pgctl.State, time.Time)
	Ready(ctx context.Context) bool
}

// Backups is the backup-session control the server needs (implemented by *pgctl.Backups).
type Backups interface {
	Begin(ctx context.Context, label string) (string, error)
	End(ctx context.Context) (labelFile, stopLSN string, err error)
}

// Server serves the supervisor API.
type Server struct {
	Token        string
	PG           Postgres
	Backups      Backups
	Bootstrap    func(ctx context.Context) error // nil unless this pod runs main
	Generation   func() string                   // current generation, e.g. "data-3"
	StopGrace    time.Duration
	StartTimeout time.Duration
}

// Handler returns the HTTP handler. /healthz and /readyz need no token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /v1/status", s.auth(s.status))
	mux.Handle("POST /v1/start", s.auth(s.start))
	mux.Handle("POST /v1/stop", s.auth(s.stop))
	mux.Handle("POST /v1/backup/start", s.auth(s.backupStart))
	mux.Handle("POST /v1/backup/stop", s.auth(s.backupStop))
	mux.Handle("POST /v1/bootstrap", s.auth(s.bootstrap))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(supervisorapi.TokenHeader)
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r)
	})
}

func (s *Server) currentStatus() supervisorapi.Status {
	st, since := s.PG.State()
	return supervisorapi.Status{PG: supervisorapi.PGState(st), Generation: s.Generation(), Since: since}
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.PG.Ready(r.Context()) {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentStatus())
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.StartTimeout)
	defer cancel()
	if err := s.PG.Start(ctx); err != nil {
		if errors.Is(err, pgctl.ErrBusy) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.currentStatus())
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if err := s.PG.Stop(r.Context(), s.StopGrace); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.currentStatus())
}

func (s *Server) backupStart(w http.ResponseWriter, r *http.Request) {
	var req supervisorapi.BackupStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Label == "" {
		writeError(w, http.StatusBadRequest, "label is required")
		return
	}
	if st, _ := s.PG.State(); st != pgctl.StateRunning {
		writeError(w, http.StatusConflict, "postgres is not running")
		return
	}
	lsn, err := s.Backups.Begin(r.Context(), req.Label)
	switch {
	case errors.Is(err, pgctl.ErrBackupInProgress):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, supervisorapi.BackupStartResponse{StartLSN: lsn})
	}
}

func (s *Server) backupStop(w http.ResponseWriter, r *http.Request) {
	label, lsn, err := s.Backups.End(r.Context())
	switch {
	case errors.Is(err, pgctl.ErrNoBackup):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, supervisorapi.BackupStopResponse{LabelFile: label, StopLSN: lsn})
	}
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if s.Bootstrap == nil {
		writeError(w, http.StatusNotFound, "bootstrap is only available on main")
		return
	}
	if err := s.Bootstrap(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.currentStatus())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, supervisorapi.Error{Error: msg})
}
```

- [ ] **Step 6: 테스트 통과 확인**

Run: `go vet ./... && go test -race ./internal/supervisor/...`
Expected: `ok  consonance/internal/supervisor`

- [ ] **Step 7: 커밋**

```bash
git add internal/supervisorapi/ internal/supervisor/
git commit -m "feat(supervisor): 토큰 인증 내부 HTTP API" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: 감독 프로세스 클라이언트

**Files:**
- Create: `internal/supervisorclient/client.go`
- Test: `internal/supervisorclient/client_test.go`

**Interfaces:**
- Consumes: Task 6 `supervisor.Server`, `supervisortest.*`, `supervisorapi.*`
- Produces: `supervisorclient.New(baseURL, token string, hc *http.Client) *Client`; 메서드 `Status`, `Start`, `Stop`, `Bootstrap` → `(supervisorapi.Status, error)`, `BackupStart(ctx, label string) (supervisorapi.BackupStartResponse, error)`, `BackupStop(ctx) (supervisorapi.BackupStopResponse, error)`; `type APIError struct { StatusCode int; Message string }`; `supervisorclient.IsConflict(err error) bool`

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/supervisorclient/client_test.go`:

```go
package supervisorclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"consonance/internal/pgctl"
	"consonance/internal/supervisor"
	"consonance/internal/supervisor/supervisortest"
	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
)

func newClient(t *testing.T, pg *supervisortest.FakePostgres, tok string) *supervisorclient.Client {
	t.Helper()
	srv := httptest.NewServer((&supervisor.Server{
		Token:        "t0k",
		PG:           pg,
		Backups:      &supervisortest.FakeBackups{},
		Generation:   func() string { return "data-2" },
		StopGrace:    time.Second,
		StartTimeout: time.Second,
	}).Handler())
	t.Cleanup(srv.Close)
	return supervisorclient.New(srv.URL, tok, nil)
}

func TestClientStartStopStatus(t *testing.T) {
	c := newClient(t, supervisortest.NewFakePostgres(pgctl.StateStopped), "t0k")
	ctx := context.Background()

	st, err := c.Start(ctx)
	if err != nil || st.PG != supervisorapi.PGRunning || st.Generation != "data-2" {
		t.Fatalf("Start() = %+v, %v", st, err)
	}
	if st, err = c.Status(ctx); err != nil || st.PG != supervisorapi.PGRunning {
		t.Fatalf("Status() = %+v, %v", st, err)
	}
	if st, err = c.Stop(ctx); err != nil || st.PG != supervisorapi.PGStopped {
		t.Fatalf("Stop() = %+v, %v", st, err)
	}
}

func TestClientBackupRoundTrip(t *testing.T) {
	c := newClient(t, supervisortest.NewFakePostgres(pgctl.StateRunning), "t0k")
	ctx := context.Background()

	if _, err := c.BackupStart(ctx, "rp-1"); err != nil {
		t.Fatalf("BackupStart: %v", err)
	}
	stop, err := c.BackupStop(ctx)
	if err != nil {
		t.Fatalf("BackupStop: %v", err)
	}
	if !strings.Contains(stop.LabelFile, "LABEL: rp-1") {
		t.Fatalf("label file = %q", stop.LabelFile)
	}
}

func TestClientSurfacesConflict(t *testing.T) {
	c := newClient(t, supervisortest.NewFakePostgres(pgctl.StateRunning), "t0k")
	_, err := c.BackupStop(context.Background())
	if !supervisorclient.IsConflict(err) {
		t.Fatalf("BackupStop without session: err = %v, want a 409 APIError", err)
	}
}

func TestClientWrongTokenIsUnauthorized(t *testing.T) {
	c := newClient(t, supervisortest.NewFakePostgres(pgctl.StateRunning), "wrong")
	_, err := c.Status(context.Background())
	var apiErr *supervisorclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Status with wrong token: err = %v, want 401 APIError", err)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/supervisorclient/...`
Expected: FAIL — `undefined: supervisorclient.New`

- [ ] **Step 3: 구현**

`internal/supervisorclient/client.go`:

```go
// Package supervisorclient calls a branch supervisor's internal HTTP API.
package supervisorclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"consonance/internal/supervisorapi"
)

// Client talks to one supervisor.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

// New returns a client for the supervisor at baseURL, e.g. "http://10.42.0.7:8008".
// A nil hc gets a client whose timeout covers a 60s start.
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 75 * time.Second}
	}
	return &Client{baseURL: baseURL, token: token, hc: hc}
}

// APIError is returned for non-2xx responses.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("supervisor: %d: %s", e.StatusCode, e.Message)
}

// IsConflict reports whether err is a 409 from the supervisor.
func IsConflict(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.StatusCode == http.StatusConflict
}

func (c *Client) Status(ctx context.Context) (supervisorapi.Status, error) {
	var s supervisorapi.Status
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &s)
	return s, err
}

func (c *Client) Start(ctx context.Context) (supervisorapi.Status, error) {
	var s supervisorapi.Status
	err := c.do(ctx, http.MethodPost, "/v1/start", nil, &s)
	return s, err
}

func (c *Client) Stop(ctx context.Context) (supervisorapi.Status, error) {
	var s supervisorapi.Status
	err := c.do(ctx, http.MethodPost, "/v1/stop", nil, &s)
	return s, err
}

func (c *Client) Bootstrap(ctx context.Context) (supervisorapi.Status, error) {
	var s supervisorapi.Status
	err := c.do(ctx, http.MethodPost, "/v1/bootstrap", nil, &s)
	return s, err
}

func (c *Client) BackupStart(ctx context.Context, label string) (supervisorapi.BackupStartResponse, error) {
	var r supervisorapi.BackupStartResponse
	err := c.do(ctx, http.MethodPost, "/v1/backup/start", supervisorapi.BackupStartRequest{Label: label}, &r)
	return r, err
}

func (c *Client) BackupStop(ctx context.Context) (supervisorapi.BackupStopResponse, error) {
	var r supervisorapi.BackupStopResponse
	err := c.do(ctx, http.MethodPost, "/v1/backup/stop", nil, &r)
	return r, err
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set(supervisorapi.TokenHeader, c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e supervisorapi.Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return &APIError{StatusCode: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go vet ./... && go test -race ./internal/supervisorclient/...`
Expected: `ok  consonance/internal/supervisorclient`

- [ ] **Step 5: 커밋**

```bash
git add internal/supervisorclient/
git commit -m "feat(supervisorclient): 감독 프로세스 API 클라이언트" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: 감독 프로세스 실행 파일

**Files:**
- Create: `cmd/supervisor/main.go`
- Test: `cmd/supervisor/main_test.go`

**Interfaces:**
- Consumes: `pgctl.NewManager`, `pgctl.NewBackups`, `pgctl.Bootstrap`, `pgctl.TenantRole`, `pgctl.TenantDatabase`, `supervisor.Server`, `supervisorclient.*`, `pgtest.*`
- Produces: 실행 파일 `consonance-supervisor`. 플래그: `--postgres-bin`, `--data-dir`, `--socket-dir`, `--port`, `--listen`, `--token-file`, `--db-password-file`(지정하면 `/v1/bootstrap` 활성), `--max-connections`, `--memory-limit-bytes`(기본값은 환경 변수 `MEMORY_LIMIT_BYTES`), `--stop-grace`, `--backup-timeout`, `--start-timeout`, `--autostart`(기본값은 환경 변수 `AUTOSTART`, 없으면 true). SIGTERM·SIGINT를 받으면 PostgreSQL을 fast shutdown하고 종료 코드 0.

- [ ] **Step 1: 실패하는 통합 테스트 쓰기**

`cmd/supervisor/main_test.go`:

```go
package main_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
	"consonance/internal/testutil/pgtest"
)

func buildSupervisor(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "consonance-supervisor")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSupervisorLifecycle(t *testing.T) {
	bin := pgtest.BinDir(t)
	root := t.TempDir()
	pgtest.InitDBAt(t, bin, filepath.Join(root, "data-1"))
	current := filepath.Join(root, "current")
	if err := os.Symlink("data-1", current); err != nil {
		t.Fatal(err)
	}
	tokenFile := writeFile(t, filepath.Join(root, "token"), "tok\n")
	pwFile := writeFile(t, filepath.Join(root, "password"), "pw\n")
	apiPort := pgtest.FreePort(t)

	cmd := exec.Command(buildSupervisor(t),
		"--postgres-bin", filepath.Join(bin, "postgres"),
		"--data-dir", current,
		"--socket-dir", pgtest.SocketDir(t),
		"--port", strconv.Itoa(pgtest.FreePort(t)),
		"--listen", "127.0.0.1:"+strconv.Itoa(apiPort),
		"--token-file", tokenFile,
		"--db-password-file", pwFile,
		"--autostart=true",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := supervisorclient.New("http://127.0.0.1:"+strconv.Itoa(apiPort), "tok", nil)

	// autostart brings postgres up on the generation that current points to.
	var st supervisorapi.Status
	eventually(t, 30*time.Second, func() bool {
		var err error
		st, err = c.Status(ctx)
		return err == nil && st.PG == supervisorapi.PGRunning
	})
	if st.Generation != "data-1" {
		t.Fatalf("generation = %q, want data-1", st.Generation)
	}
	if _, err := c.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := c.BackupStart(ctx, "it"); err != nil {
		t.Fatalf("backup start: %v", err)
	}
	stop, err := c.BackupStop(ctx)
	if err != nil || !strings.Contains(stop.LabelFile, "LABEL: it") {
		t.Fatalf("backup stop = %+v, %v", stop, err)
	}
	if st, err = c.Stop(ctx); err != nil || st.PG != supervisorapi.PGStopped {
		t.Fatalf("stop = %+v, %v", st, err)
	}
	if st, err = c.Start(ctx); err != nil || st.PG != supervisorapi.PGRunning {
		t.Fatalf("start = %+v, %v", st, err)
	}

	// SIGTERM: fast shutdown, then a clean exit.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("supervisor exit: %v", err)
	}
	out, err := exec.Command(filepath.Join(bin, "pg_controldata"), filepath.Join(root, "data-1")).CombinedOutput()
	if err != nil {
		t.Fatalf("pg_controldata: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "shut down") {
		t.Fatalf("cluster was not shut down cleanly:\n%s", out)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `hack/vm-test.sh -v ./cmd/supervisor/...`
Expected: FAIL — `go build`가 `no Go files` 또는 `undefined: main`으로 실패

- [ ] **Step 3: 구현**

`cmd/supervisor/main.go`:

```go
// Command consonance-supervisor runs PostgreSQL inside a branch pod and serves
// the internal API used by the volume agent and the operator (spec 7.3).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"consonance/internal/pgctl"
	"consonance/internal/supervisor"
)

type config struct {
	postgresBin    string
	dataDir        string
	socketDir      string
	port           int
	listen         string
	tokenFile      string
	dbPasswordFile string
	maxConnections int
	memoryLimit    int64
	stopGrace      time.Duration
	backupTimeout  time.Duration
	startTimeout   time.Duration
	autostart      bool
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("supervisor exited", "err", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("consonance-supervisor", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.postgresBin, "postgres-bin", "/usr/lib/postgresql/17/bin/postgres", "path to the postgres binary")
	fs.StringVar(&c.dataDir, "data-dir", "/pgdata/current", "data directory (the generation symlink)")
	fs.StringVar(&c.socketDir, "socket-dir", "/var/run/postgresql", "unix socket directory")
	fs.IntVar(&c.port, "port", 5432, "postgres port")
	fs.StringVar(&c.listen, "listen", ":8008", "API listen address")
	fs.StringVar(&c.tokenFile, "token-file", "/etc/consonance/token/token", "file holding the internal API token")
	fs.StringVar(&c.dbPasswordFile, "db-password-file", "", "file holding the tenant password; enables /v1/bootstrap (main only)")
	fs.IntVar(&c.maxConnections, "max-connections", 100, "max_connections")
	fs.Int64Var(&c.memoryLimit, "memory-limit-bytes", envInt64("MEMORY_LIMIT_BYTES"), "container memory limit in bytes (Downward API)")
	fs.DurationVar(&c.stopGrace, "stop-grace", 30*time.Second, "fast shutdown grace before immediate shutdown")
	fs.DurationVar(&c.backupTimeout, "backup-timeout", 30*time.Second, "abort a backup session not stopped within this time")
	fs.DurationVar(&c.startTimeout, "start-timeout", 60*time.Second, "how long a start waits for readiness")
	fs.BoolVar(&c.autostart, "autostart", envBool("AUTOSTART", true), "start postgres when the supervisor starts")
	err := fs.Parse(args)
	return c, err
}

func run(args []string) error {
	c, err := parseFlags(args)
	if err != nil {
		return err
	}
	token, err := readSecret(c.tokenFile)
	if err != nil {
		return err
	}

	pg := pgctl.NewManager(c.postgresBin, pgctl.Options{
		DataDir:          c.dataDir,
		SocketDir:        c.socketDir,
		Port:             c.port,
		MemoryLimitBytes: c.memoryLimit,
		MaxConnections:   c.maxConnections,
	})
	srv := &supervisor.Server{
		Token:        token,
		PG:           pg,
		Backups:      pgctl.NewBackups(pg.ConnString(), c.backupTimeout),
		Generation:   func() string { return generationOf(c.dataDir) },
		StopGrace:    c.stopGrace,
		StartTimeout: c.startTimeout,
	}
	if c.dbPasswordFile != "" {
		srv.Bootstrap = func(ctx context.Context) error {
			pw, err := readSecret(c.dbPasswordFile)
			if err != nil {
				return err
			}
			return pgctl.Bootstrap(ctx, pg.ConnString(), pgctl.TenantRole, pgctl.TenantDatabase, pw)
		}
	}

	ln, err := net.Listen("tcp", c.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", c.listen, err)
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()
	slog.Info("supervisor listening", "addr", ln.Addr().String(), "dataDir", c.dataDir)

	if c.autostart {
		ctx, cancel := context.WithTimeout(context.Background(), c.startTimeout)
		if err := pg.Start(ctx); err != nil {
			// Keep serving: the operator retries through POST /v1/start.
			slog.Error("autostart failed", "err", err)
		}
		cancel()
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigs
	slog.Info("shutting down", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), c.stopGrace+15*time.Second)
	defer cancel()
	stopErr := pg.Stop(ctx, c.stopGrace)
	_ = httpSrv.Shutdown(ctx)
	return stopErr
}

// generationOf returns the generation that the data directory symlink points to.
func generationOf(dataDir string) string {
	target, err := os.Readlink(dataDir)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func envInt64(name string) int64 {
	v, _ := strconv.ParseInt(os.Getenv(name), 10, 64)
	return v
}

func envBool(name string, def bool) bool {
	v, err := strconv.ParseBool(os.Getenv(name))
	if err != nil {
		return def
	}
	return v
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go vet ./... && go build ./... && hack/vm-test.sh -race -v ./cmd/supervisor/...`
Expected: `TestSupervisorLifecycle` PASS

- [ ] **Step 5: 전체 테스트 한 번 더**

Run: `make test && make test-vm`
Expected: 두 명령 모두 모든 패키지 `ok`

- [ ] **Step 6: 커밋**

```bash
git add cmd/supervisor/
git commit -m "feat(supervisor): 실행 파일 — 자동 기동, 부트스트랩, SIGTERM 시 fast shutdown" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: consonance/postgres 이미지와 스모크 테스트

**Files:**
- Create: `images/postgres/Dockerfile`, `.dockerignore`, `test/image/supervisor-smoke.sh`
- Modify: `Makefile` (대상 `image-postgres`, `smoke-postgres` 추가)

**Interfaces:**
- Consumes: Task 8 `cmd/supervisor`
- Produces: 이미지 `consonance/postgres:17-dev` (ENTRYPOINT `tini -- consonance-supervisor`, USER 999:999). 계획 5에서 k3s로 옮긴다.

- [ ] **Step 1: Docker(colima) 준비 확인**

Run: `docker info --format '{{.ServerVersion}} {{.Architecture}}' || colima start`
Expected: 서버 버전과 `aarch64` 출력 (안 켜져 있었으면 `colima start` 후 다시 실행)

- [ ] **Step 2: 실패하는 스모크 테스트 쓰기**

`test/image/supervisor-smoke.sh`:

```bash
#!/usr/bin/env bash
# Smoke test for consonance/postgres: the supervisor starts postgres, answers its API,
# and shuts postgres down cleanly when the container stops.
set -euo pipefail
IMAGE=${IMAGE:-consonance/postgres:17-dev}
NAME=consonance-smoke-$$
TOKEN=smoke-token
PORT=${PORT:-18008}

cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker volume rm "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker volume create "$NAME" >/dev/null
docker run --rm -v "$NAME:/pgdata" --user 0 --entrypoint chown "$IMAGE" 999:999 /pgdata
docker run --rm -v "$NAME:/pgdata" --entrypoint bash "$IMAGE" -c \
  'initdb -D /pgdata/data-1 -U postgres --auth-local=peer --auth-host=scram-sha-256 >/dev/null &&
   ln -s data-1 /pgdata/current && printf %s "$1" > /pgdata/token' _ "$TOKEN"

docker run -d --name "$NAME" -v "$NAME:/pgdata" -p "$PORT:8008" "$IMAGE" --token-file /pgdata/token >/dev/null

status=""
for _ in $(seq 1 150); do
  status=$(curl -fsS -H "X-Consonance-Token: $TOKEN" "localhost:$PORT/v1/status" 2>/dev/null || true)
  case "$status" in *'"pg":"running"'*) break ;; esac
  sleep 0.2
done
case "$status" in *'"pg":"running"'*) ;; *) docker logs "$NAME"; echo "FAIL: postgres not running"; exit 1 ;; esac

curl -fsS "localhost:$PORT/readyz" >/dev/null
code=$(curl -s -o /dev/null -w '%{http_code}' "localhost:$PORT/v1/status")
[ "$code" = 401 ] || { echo "FAIL: unauthenticated status returned $code"; exit 1; }

curl -fsS -X POST -H "X-Consonance-Token: $TOKEN" -d '{"label":"smoke"}' "localhost:$PORT/v1/backup/start" >/dev/null
curl -fsS -X POST -H "X-Consonance-Token: $TOKEN" "localhost:$PORT/v1/backup/stop" | grep -q 'LABEL: smoke'

docker stop -t 40 "$NAME" >/dev/null
docker logs "$NAME" 2>&1 | grep -q 'database system is shut down' || { docker logs "$NAME"; echo "FAIL: no clean shutdown"; exit 1; }
echo PASS
```

Run: `chmod +x test/image/supervisor-smoke.sh && test/image/supervisor-smoke.sh`
Expected: FAIL — `Unable to find image 'consonance/postgres:17-dev' locally` 후 pull 실패

- [ ] **Step 3: Dockerfile과 .dockerignore 쓰기**

`images/postgres/Dockerfile`:

```dockerfile
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/consonance-supervisor ./cmd/supervisor

FROM postgres:17-bookworm
RUN apt-get update \
 && apt-get install -y --no-install-recommends tini \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/consonance-supervisor /usr/local/bin/consonance-supervisor
USER 999:999
# tini is PID 1: it reaps orphaned processes and forwards SIGTERM to the supervisor.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/consonance-supervisor"]
```

`.dockerignore`:

```
.git
spikes
docs
bin
```

`Makefile`에 다음을 덧붙인다(명령 앞은 탭 문자). 맨 위 `.PHONY` 줄도 `.PHONY: test test-vm vm-setup image-postgres smoke-postgres`로 바꾼다.

```make
image-postgres: ## build consonance/postgres:17-dev for the local architecture
	docker build -f images/postgres/Dockerfile -t consonance/postgres:17-dev .

smoke-postgres: image-postgres ## run the image smoke test
	test/image/supervisor-smoke.sh
```

- [ ] **Step 4: 이미지를 빌드하고 스모크 테스트 통과 확인**

Run: `make smoke-postgres`
Expected: 마지막 줄 `PASS`

- [ ] **Step 5: 커밋**

```bash
git add images/postgres/Dockerfile .dockerignore test/image/supervisor-smoke.sh Makefile
git commit -m "build: consonance/postgres 이미지(tini + 감독 프로세스)와 스모크 테스트" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
