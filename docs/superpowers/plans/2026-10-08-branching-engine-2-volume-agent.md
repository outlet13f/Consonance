# 브랜칭 엔진 구현 계획 2/5: 볼륨 에이전트

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 프로젝트 볼륨 하나를 맡아 브랜치 데이터의 reflink 복사, 데이터 세대, 복구 지점과 그 카탈로그를 관리하고 토큰 인증 HTTP API로 제공하는 볼륨 에이전트, 그 클라이언트, `consonance/agent` 이미지를 만든다.

**Architecture:** `internal/volume`이 볼륨 레이아웃(스펙 6장)과 모든 볼륨 작업을 맡는다. 파일은 FICLONE으로 범위를 공유해 복사하고(`CloneTree`), 실행 중인 DB는 부모 감독 프로세스의 백업 모드(계획 1의 `supervisorclient`)를 건 채 복사한다. 볼륨을 바꾸는 작업은 잠금 하나로 한 번에 하나씩 하고, 끊긴 작업의 흔적은 `.incomplete` 표시와 `current` 링크를 기준으로 다음 기동 때 `Recover`가 정리한다. `internal/agent`가 이를 HTTP API로 내놓고, `internal/agentclient`는 계획 3(오퍼레이터)과 계획 4(API 서버)가 쓸 클라이언트다. `cmd/agent`가 이를 묶고, 이미지 `consonance/agent`는 `postgres:17` 위에 tini와 에이전트를 얹는다(`initdb` 포함).

**Tech Stack:** Go 1.25(FICLONE ioctl은 표준 `syscall`로 부르고 새 의존성은 없다), PostgreSQL 17(`initdb`, 백업 모드), XFS reflink, Go 표준 `net/http`, Lima VM `consonance-poc`(Ubuntu 24.04), Docker(colima).

**Spec:** `docs/superpowers/specs/2026-10-07-branching-engine-design.md` — 특히 4.4, 4.5, 6, 7.2, 7.5, 7.6, 9.2~9.4, 10, 12, 14장. 계획 1에서 넘어온 사항: `docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`.

## Global Constraints

- 모듈 `consonance`, `go 1.25.0` 그대로 둔다. 새 모듈 의존성을 넣지 않는다(`golang.org/x/sys` 최신판은 Go 1.26 이상을 요구하므로 FICLONE은 표준 `syscall`로 부른다).
- 에이전트 기본값: 볼륨 루트 `/volume`, API 주소 `:8443`(평문 HTTP), 토큰 파일 `/etc/consonance/token/token`, initdb `/usr/lib/postgresql/17/bin/initdb`, 감독 프로세스 주소 `http://br-{branch}:8008`(`{branch}`를 브랜치 이름으로 바꾼다).
- 인증 헤더는 `X-Consonance-Token`이다. 에이전트와 감독 프로세스는 프로젝트의 같은 토큰을 쓰고(Secret `internal-token`, 스펙 7.1), 에이전트는 그 토큰으로 감독 프로세스도 부른다.
- 볼륨 레이아웃(스펙 6): `branches/<브랜치>/current -> data-N`(상대 링크), `branches/<브랜치>/data-N/`, `restore/<브랜치>/<복구 지점 ID>/`, `.consonance/catalog.json`. 에이전트만 쓰는 `.consonance/trash/`(지울 것)와 `.consonance/preflight-*`(점검용)를 더 둔다.
- 이름: 브랜치는 DNS-1123 라벨 40자 이하, 복구 지점 ID는 `rp-<UTC yyyymmddThhmmssZ>-<a-z0-9 4자>`, 세대 디렉터리는 `data-N`(N ≥ 1, 앞자리 0 없음). main의 데이터는 init-main만 만든다.
- 실행 중인 DB의 복사(스펙 7.2): 백업 시작 → `pg_wal`·`postmaster.pid`·`postmaster.opts`를 뺀 reflink 복사 → 백업 종료 → `backup_label` 기록 → `pg_wal` reflink 복사. 복사 원본은 감독 프로세스 `/v1/status`가 보고하는 실행 중인 세대다(`current` 링크가 아니다).
- 기본값(스펙 14): 같은 원본 브랜치의 백업 모드 복사는 직전 복사가 끝난 시각부터 5초 간격(복구 지점에서 하는 복사는 제외), 잠금 대기 2초, 사용률 90% 초과 시 복사 거부, 복구 지점은 브랜치당 20개·24시간(만들 때와 1시간마다 적용).
- initdb 인자: `-U postgres --auth-local=peer --auth-host=scram-sha-256 --encoding=UTF8 --locale-provider=builtin --locale=C.UTF-8 --data-checksums`. `pg_hba.conf`는 Task 7의 내용 그대로다(슈퍼유저는 로컬 소켓 peer만, TCP 슈퍼유저는 reject, 나머지는 scram-sha-256 — 스펙 4.5).
- API: 성공한 POST·GET은 200, DELETE는 204. 오류 본문은 `{"error","code"}`이고 상태 코드와 `code`는 400 `invalid`, 401 `unauthorized`, 404 `not-found`, 409 `conflict`·`not-running`·`busy`, 429 `rate-limited`(`Retry-After` 초 포함), 502 `supervisor-unavailable`, 507 `insufficient-storage`, 500 `internal`.
- 테스트: PostgreSQL이 필요한 테스트는 `CONSONANCE_TEST_PG_BIN`이, XFS가 필요한 테스트는 `CONSONANCE_TEST_XFS_DIR`이 없으면 건너뛴다. 둘 다 `hack/vm-test.sh`가 Lima VM 안에서 설정한다. Mac에서 `make test`는 나머지만 돌며 통과해야 한다. VM 명령에는 항상 패키지 패턴을 준다.
- 이미지는 uid/gid 999로 돌고 PID 1은 tini, `STOPSIGNAL SIGTERM`이다.
- 모든 커밋 메시지 끝에 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`를 붙인다(`-m`을 하나 더). `spikes/`는 커밋하지 않고, 푸시하지 않는다.

## 스펙에 없는 결정

계획에서 정한 것이다. Task 14에서 스펙 7.2에 반영한다.

- 에이전트 API에 `GET /v1/branches/{name}/generations`(목록)와 `DELETE /v1/branches/{name}/generations/{n}`(삭제)을 더한다. 오퍼레이터가 롤백 도중 재시작했을 때 남은 세대를 확인하고 치우는 데 쓴다.
- 리셋(스펙 9.4의 5단계)에서 옛 세대를 복구 지점으로 옮기는 일은 `detach` 본문 `{"asRestorePoint": true}`로 한다.
- 오류마다 기계가 읽는 `code`를 붙인다(계획 1의 이월 사항 M5에서 배운 것).
- 지울 것은 잠금 안에서 `.consonance/trash/`로 이름만 바꾸고, 실제 삭제는 잠금 밖에서 정리 작업(janitor)이 한다.
- 세대 전환·분리·삭제와 브랜치 삭제 전에 감독 프로세스 상태를 직접 확인해, PostgreSQL이 쓰고 있는 세대를 옮기거나 지우지 않는다.
- 백업 시작까지 간 복사는 실패해도 복사 간격에 센다(체크포인트는 이미 걸렸다). 백업 시작 뒤에 실패하면 그 자리에서 백업을 끝낸다(감독 프로세스의 30초 자동 취소를 기다리지 않는다).
- 복사한 `pg_wal`에 백업 시작 WAL 세그먼트가 있는지 확인한다. 복사 사이에 체크포인트가 그 세그먼트를 재활용하는 드문 경우를 잡는다.
- 복구 지점은 백업 모드 복사이므로 브랜치가 실행 중이어야 만든다. 정지된 브랜치면 409 `not-running`.
- 메트릭(스펙 11)은 이 계획에 넣지 않는다(계획 5).
- 알려진 한계: 리셋에서 옛 세대를 복구 지점으로 옮긴 직후, 카탈로그에 쓰기 전에 에이전트가 죽으면 그 디렉터리는 다음 기동 때 지워진다. 리셋 자체는 이미 끝난 뒤다.

## Review Focus

1. 복사 도중 부모가 재시작해 다른 세대를 실행하는 경우: 복사를 거부하고 반쯤 만든 브랜치를 남기지 않아야 한다. → Task 8 `TestCopyRefusesParentThatRestarted`
2. 백업을 시작한 뒤 복사가 실패하는 경우: 부모의 백업을 그 자리에서 끝내고(다음 복사가 30초 동안 막히지 않게) 만들던 디렉터리를 지워야 한다. → Task 8 `TestFailedCopyEndsBackupAndCleansUp`
3. 작업 도중 에이전트가 죽는 경우: 다음 기동이 미완성 디렉터리·링크·카탈로그 항목을 정리하고 완성된 데이터는 건드리지 않으며, 같은 요청을 다시 하면 성공해야 한다. → Task 6 `TestRecoverRemovesLeftovers`, Task 13 `TestAgentRecoversOnStart`
4. PostgreSQL이 아직 실행 중인 세대를 분리하거나 지우려는 경우: 409로 거부하고 아무것도 옮기지 않아야 한다. → Task 10 `TestDetachRefusesRunningGeneration`
5. 경로에 `..`, `%2F`, 대문자, 41자 이름이 오는 경우: 400으로 거부하고 볼륨 밖을 건드리지 않아야 한다. → Task 5 `TestValidBranchName`, Task 13 `TestAgentRejectsHostileNames`

## 파일 구성

| 파일 | 맡는 일 |
| --- | --- |
| `hack/vm-setup.sh`, `hack/vm-test.sh`, `Makefile` | VM의 XFS 테스트 볼륨(재시작 뒤에도 마운트), `CONSONANCE_TEST_XFS_DIR`, `make test-xfs`·`image-agent`·`smoke-agent` |
| `internal/testutil/xfstest/` | XFS 위 임시 디렉터리 도우미 |
| `internal/pgctl/process.go` | (계획 1 이월) 해석되지 않는 데이터 디렉터리 링크로는 기동하지 않음 |
| `internal/volume/errors.go`, `doc.go` | 오류 값, 패키지 설명 |
| `internal/volume/clone.go`, `ficlone_linux.go`, `ficlone_other.go` | FICLONE으로 디렉터리 트리 복사 |
| `internal/volume/layout.go` | 경로, 세대 이름, `current` 링크, `.incomplete` 표시, fsync |
| `internal/agentapi/types.go` | 에이전트 API의 요청·응답·오류 코드, 이름 검사 |
| `internal/volume/catalog.go` | `catalog.json` 읽기·원자적 쓰기, 복구 지점 ID, 보존 정책 계산 |
| `internal/volume/volume.go`, `recover.go` | `Volume`, 설정, 잠금, 복사 간격, 사용량, preflight, 휴지통, 기동 시 정리 |
| `internal/volume/initmain.go` | init-main(initdb와 pg_hba) |
| `internal/volume/snapshot.go`, `branches.go` | 백업 모드 복사, 브랜치 데이터 생성·삭제 |
| `internal/volume/restorepoints.go` | 복구 지점 생성·목록·삭제·보존, 정리 작업(janitor) |
| `internal/volume/generations.go` | 세대 목록·생성·전환·분리·삭제 |
| `internal/agent/`, `internal/agent/agenttest/` | HTTP API, 테스트용 가짜 볼륨 |
| `internal/agentclient/` | 에이전트 API 클라이언트 |
| `internal/secretfile/`, `cmd/agent/`, `cmd/supervisor/main.go` | 비밀 파일 읽기(감독 프로세스와 공유), 에이전트 실행 파일 |
| `images/agent/Dockerfile`, `test/image/agent-smoke.sh` | 이미지와 스모크 테스트 |

---

### Task 1: VM의 XFS 테스트 볼륨

**Files:**
- Modify: `hack/vm-setup.sh`, `hack/vm-test.sh`, `Makefile`
- Create: `internal/testutil/xfstest/xfstest.go`
- Test: `internal/testutil/xfstest/xfstest_test.go`

**Interfaces:**
- Produces: `xfstest.Dir(t testing.TB) string`(`$CONSONANCE_TEST_XFS_DIR` 아래 새 디렉터리, 없으면 테스트를 건너뜀), 상수 `xfstest.EnvDir = "CONSONANCE_TEST_XFS_DIR"`, VM 디렉터리 `/mnt/consonance-xfs/test`, `make test-xfs`.

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/testutil/xfstest/xfstest_test.go`:

```go
package xfstest_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"consonance/internal/testutil/xfstest"
)

func TestDirIsFreshWritableXFS(t *testing.T) {
	dir := xfstest.Dir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s is not empty: %v", dir, entries)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	const xfsMagic = 0x58465342
	if st.Type != xfsMagic {
		t.Fatalf("%s is on filesystem type %#x, want XFS (%#x)", dir, st.Type, xfsMagic)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe"), []byte("x"), 0o600); err != nil {
		t.Fatalf("not writable: %v", err)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/testutil/xfstest/`
Expected: FAIL — `no non-test Go files` 또는 `undefined: xfstest.Dir`

- [ ] **Step 3: 도우미 구현**

`internal/testutil/xfstest/xfstest.go`:

```go
// Package xfstest provides scratch directories on an XFS filesystem with reflink for tests.
package xfstest

import (
	"os"
	"testing"
)

// EnvDir names the environment variable pointing at a writable directory on XFS with reflink
// (spec 12). hack/vm-test.sh sets it inside the Lima VM.
const EnvDir = "CONSONANCE_TEST_XFS_DIR"

// Dir returns a fresh directory under $CONSONANCE_TEST_XFS_DIR, removed when the test ends,
// or skips the test when the variable is not set.
func Dir(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvDir)
	if base == "" {
		t.Skipf("%s not set; skipping test that needs XFS with reflink", EnvDir)
	}
	dir, err := os.MkdirTemp(base, "t-")
	if err != nil {
		t.Fatalf("mkdir under %s: %v", base, err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
```

- [ ] **Step 4: Mac에서 실행 (건너뜀 확인)**

Run: `go test -v ./internal/testutil/xfstest/`
Expected: `--- SKIP: TestDirIsFreshWritableXFS`, `ok  consonance/internal/testutil/xfstest`

- [ ] **Step 5: VM 스크립트와 Makefile 바꾸기**

`hack/vm-setup.sh` 전체를 다음으로 바꾼다. 추가된 것은 xfsprogs 설치와 XFS 루프 볼륨 블록이다. 계획 1의 PoC 볼륨 `/mnt/project`는 건드리지 않는다.

```bash
#!/usr/bin/env bash
# One-time setup of the Lima VM used for tests that need PostgreSQL 17 or XFS with reflink.
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
  if ! command -v mkfs.xfs >/dev/null; then
    apt-get update -qq
    apt-get install -y -qq xfsprogs >/dev/null
  fi
  # XFS with reflink for internal/volume tests (CONSONANCE_TEST_XFS_DIR, spec 12). A loop file
  # stands in for a CSI volume; the fstab entry mounts it again after a VM restart.
  if [ ! -f /var/lib/consonance/xfs.img ]; then
    mkdir -p /var/lib/consonance
    truncate -s 8G /var/lib/consonance/xfs.img
    mkfs.xfs -q -m reflink=1 /var/lib/consonance/xfs.img
  fi
  mkdir -p /mnt/consonance-xfs
  grep -q ' /mnt/consonance-xfs ' /etc/fstab ||
    echo '/var/lib/consonance/xfs.img /mnt/consonance-xfs xfs loop,nofail 0 0' >> /etc/fstab
  mountpoint -q /mnt/consonance-xfs || mount /mnt/consonance-xfs
  install -d -o \$SUDO_UID -g \$SUDO_GID /mnt/consonance-xfs/test
  xfs_info /mnt/consonance-xfs | grep -q 'reflink=1'
  /usr/local/go/bin/go version
  /usr/lib/postgresql/17/bin/postgres --version
  echo 'xfs: /mnt/consonance-xfs (reflink=1); tests use /mnt/consonance-xfs/test'
"
```

`hack/vm-test.sh` 전체를 다음으로 바꾼다(`CONSONANCE_TEST_XFS_DIR`를 더했다):

```bash
#!/usr/bin/env bash
# Runs `go test` inside the Lima VM, which has PostgreSQL 17 and an XFS filesystem with reflink.
# Usage: hack/vm-test.sh [go test args...]   (default: ./...; always give package patterns)
set -euo pipefail
VM=${VM:-consonance-poc}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
ARGS=("$@")
if [ ${#ARGS[@]} -eq 0 ]; then ARGS=(./...); fi
QUOTED=$(printf ' %q' "${ARGS[@]}")
limactl shell "$VM" -- bash -c "cd $(printf %q "$ROOT") && export PATH=/usr/local/go/bin:\$PATH GOFLAGS=-buildvcs=false CONSONANCE_TEST_PG_BIN=/usr/lib/postgresql/17/bin CONSONANCE_TEST_XFS_DIR=/mnt/consonance-xfs/test && go test$QUOTED"
```

`Makefile` 전체를 다음으로 바꾼다(레시피 줄은 TAB으로 시작한다):

```make
.PHONY: test test-vm test-xfs vm-setup image-postgres smoke-postgres

test: ## host tests (PostgreSQL and XFS tests skip)
	go vet ./...
	go test ./...

vm-setup: ## one-time: start the Lima VM, install Go and PostgreSQL 17, mount the XFS test volume
	hack/vm-setup.sh

test-vm: ## all tests inside the Lima VM
	hack/vm-test.sh ./...

test-xfs: ## internal/volume tests on XFS with reflink inside the Lima VM
	hack/vm-test.sh -race ./internal/volume/... ./internal/testutil/xfstest/...

image-postgres: ## build consonance/postgres:17-dev for the local architecture
	docker build -f images/postgres/Dockerfile -t consonance/postgres:17-dev .

smoke-postgres: image-postgres ## run the image smoke test
	test/image/supervisor-smoke.sh
```

- [ ] **Step 6: VM 준비와 테스트 통과 확인**

Run: `make vm-setup && hack/vm-test.sh -v ./internal/testutil/xfstest/...`
Expected: 설치 출력 마지막 줄이 `xfs: /mnt/consonance-xfs (reflink=1); tests use /mnt/consonance-xfs/test`, 이어서 `--- PASS: TestDirIsFreshWritableXFS`, `ok  consonance/internal/testutil/xfstest`

- [ ] **Step 7: VM을 재시작해도 마운트가 남는지 확인**

Run: `limactl stop consonance-poc && limactl start consonance-poc && hack/vm-test.sh -v ./internal/testutil/xfstest/...`
Expected: `--- PASS: TestDirIsFreshWritableXFS`(SKIP이나 "filesystem type" 실패가 아님)

- [ ] **Step 8: 커밋**

```bash
git add hack/vm-setup.sh hack/vm-test.sh Makefile internal/testutil/xfstest/
git commit -m "chore: VM의 XFS(reflink) 테스트 볼륨과 xfstest 도우미" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: 해석되지 않는 데이터 디렉터리 링크로는 기동하지 않기 (계획 1 이월)

지금 `pgctl.Manager.Start`는 `current`가 아무 데도 이어지지 않으면 링크 경로를 그대로 `-D`로 넘긴다. 그러면 PostgreSQL이 실행 중에 링크 경로로 `pg_control`을 써서, 나중에 그 링크가 가리키게 될 세대를 손상시킬 수 있다(스펙 7.3). 기동하기 전에 거부한다.

**Files:**
- Modify: `internal/pgctl/process.go` (오류 값 추가, `Start`의 데이터 디렉터리 해석 부분)
- Modify: `internal/pgctl/process_test.go` (`TestStartFailsOnMissingDataDir`를 바꾼다)
- Modify: `cmd/supervisor/main_test.go` (`TestSupervisorShutsDownWhileAutostartWaits`가 데이터 디렉터리를 먼저 만든다)

**Interfaces:**
- Produces: `pgctl.ErrNoDataDir`. `Start`는 데이터 디렉터리 링크가 해석되지 않으면 아무것도 띄우지 않고 `fmt.Errorf("%w: %s", ErrNoDataDir, dataDir)`를 돌려주며, 상태는 stopped로 남는다. 감독 프로세스 `/v1/start`는 이를 지금처럼 500으로 돌려준다.

- [ ] **Step 1: 테스트 바꾸기**

`internal/pgctl/process_test.go`에서 `TestStartFailsOnMissingDataDir` 함수 전체를 다음으로 바꾼다(PostgreSQL 바이너리가 없어도 돈다). 파일의 import에 `errors`와 `os`가 없으면 더한다.

```go
func TestStartRefusesUnresolvableDataDir(t *testing.T) {
	dir := t.TempDir()
	// A generation link that leads nowhere, as on a branch whose data is not made yet.
	if err := os.Symlink("data-1", filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	// No postgres binary: Start must refuse before it launches anything.
	m := pgctl.NewManager(filepath.Join(dir, "no-such-postgres"), pgctl.Options{
		DataDir:        filepath.Join(dir, "current"),
		SocketDir:      dir,
		Port:           5432,
		MaxConnections: 20,
	})
	err := m.Start(testCtx(t))
	if !errors.Is(err, pgctl.ErrNoDataDir) {
		t.Fatalf("Start = %v, want ErrNoDataDir", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "current")) {
		t.Fatalf("error does not name the data directory: %v", err)
	}
	if s, _ := m.State(); s != pgctl.StateStopped {
		t.Fatalf("state = %s, want stopped", s)
	}
	if g := m.Generation(); g != "" {
		t.Fatalf("Generation = %q, want empty", g)
	}
}
```

`cmd/supervisor/main_test.go`의 `TestSupervisorShutsDownWhileAutostartWaits`는 가짜 postmaster를 없는 디렉터리 `root/data`로 띄운다. 바뀐 `Start`는 이를 거부하므로, 함수 안에서 `fakePG`의 `os.Chmod` 블록 바로 뒤에 다음을 더한다:

```go
	// Start refuses a data directory that does not exist; the fake postmaster needs nothing in it.
	if err := os.Mkdir(filepath.Join(root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test -run TestStartRefusesUnresolvableDataDir ./internal/pgctl/`
Expected: FAIL — `undefined: pgctl.ErrNoDataDir`

- [ ] **Step 3: 구현**

`internal/pgctl/process.go`에서 `ErrGenerationMismatch` 선언 바로 아래에 더한다:

```go
// ErrNoDataDir is returned by Start when the data directory (the generation link) does not
// lead to a directory, so there is no generation to start.
var ErrNoDataDir = errors.New("data directory does not resolve")
```

`Start`에서 다음 부분을

```go
	// Start postgres on the directory the link points to now, not through the link: for as long
	// as it runs, postgres opens some files (pg_control) by their path under its data directory,
	// so given the link, a switch made while it runs would send those writes into the other
	// generation. When the link does not resolve, postgres gets it as is and reports the error.
	dir := resolveDir(m.opts.DataDir)
	opts := m.opts
	if dir != "" {
		opts.DataDir = dir
	}
```

다음으로 바꾼다:

```go
	// Start postgres on the directory the link points to now, not through the link: for as long
	// as it runs, postgres opens some files (pg_control) by their path under its data directory,
	// so given the link, a switch made while it runs would send those writes into the other
	// generation. A link that leads nowhere is refused rather than handed to postgres.
	dir := resolveDir(m.opts.DataDir)
	if dir == "" {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoDataDir, m.opts.DataDir)
	}
	opts := m.opts
	opts.DataDir = dir
```

- [ ] **Step 4: Mac과 VM에서 통과 확인**

Run: `go test -run TestStartRefusesUnresolvableDataDir -v ./internal/pgctl/ && go test -race ./cmd/supervisor/ && hack/vm-test.sh -race ./internal/pgctl/... ./internal/supervisor/... ./cmd/supervisor/...`
Expected: Mac에서 `--- PASS: TestStartRefusesUnresolvableDataDir`와 `ok  consonance/cmd/supervisor`(`TestSupervisorShutsDownWhileAutostartWaits` 포함), VM에서 세 패키지 모두 `ok`

- [ ] **Step 5: 커밋**

```bash
git add internal/pgctl/process.go internal/pgctl/process_test.go cmd/supervisor/main_test.go
git commit -m "fix(pgctl): 해석되지 않는 데이터 디렉터리 링크로는 PostgreSQL을 띄우지 않음" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 3: reflink 트리 복사

**Files:**
- Create: `internal/volume/doc.go`, `internal/volume/errors.go`, `internal/volume/clone.go`, `internal/volume/ficlone_linux.go`, `internal/volume/ficlone_other.go`
- Test: `internal/volume/clone_test.go`

**Interfaces:**
- Produces: `volume.CloneTree(src, dst string, skip volume.SkipFunc) error`, `type SkipFunc func(rel string) bool`; 오류 값 `ErrInvalid`, `ErrNotFound`, `ErrConflict`, `ErrNotRunning`, `ErrBusy`, `ErrInsufficientStorage`, `ErrSupervisorUnavailable`, `ErrReflinkUnsupported`와 `type RateLimitError struct { Source string; RetryAfter time.Duration }`; 패키지 내부 `cloneFile(src, dst string, perm fs.FileMode) error`.
- 테스트 도우미 `writeFile`, `readFile`, `permOf`(`clone_test.go`, 패키지 `volume_test`)는 뒤 작업의 테스트도 쓴다.

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/volume/clone_test.go`:

```go
package volume_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"consonance/internal/testutil/xfstest"
	"consonance/internal/volume"
)

// writeFile writes content to path with exactly perm, creating parent directories.
func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil { // WriteFile's mode is subject to the umask
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestCloneTreeCopiesTheTree(t *testing.T) {
	root := xfstest.Dir(t)
	src := filepath.Join(root, "src")
	big := string(bytes.Repeat([]byte("0123456789abcdef"), 4096)) // 64 KiB
	writeFile(t, filepath.Join(src, "PG_VERSION"), "17\n", 0o600)
	writeFile(t, filepath.Join(src, "base", "5", "1259"), big, 0o600)
	writeFile(t, filepath.Join(src, "empty"), "", 0o640)
	writeFile(t, filepath.Join(src, "pg_wal", "000000010000000000000001"), "wal", 0o600)
	writeFile(t, filepath.Join(src, "postmaster.pid"), "4242\n", 0o600)
	if err := os.Symlink("base/5", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "base"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	skip := func(rel string) bool { return rel == "pg_wal" || rel == "postmaster.pid" }
	if err := volume.CloneTree(src, dst, skip); err != nil {
		t.Fatalf("CloneTree: %v", err)
	}

	if got := readFile(t, filepath.Join(dst, "PG_VERSION")); got != "17\n" {
		t.Errorf("PG_VERSION = %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "base", "5", "1259")); got != big {
		t.Error("cloned file differs from its source")
	}
	if got := readFile(t, filepath.Join(dst, "empty")); got != "" {
		t.Errorf("empty = %q", got)
	}
	for path, want := range map[string]os.FileMode{
		dst:                              0o700,
		filepath.Join(dst, "base"):       0o750,
		filepath.Join(dst, "PG_VERSION"): 0o600,
		filepath.Join(dst, "empty"):      0o640,
	} {
		if got := permOf(t, path); got != want {
			t.Errorf("mode of %s = %o, want %o", path, got, want)
		}
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "base/5" {
		t.Errorf("link = %q, %v; want base/5", target, err)
	}
	for _, name := range []string{"pg_wal", "postmaster.pid"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was copied although skipped (Lstat: %v)", name, err)
		}
	}
	// The copy is independent: writing it leaves the source alone.
	writeFile(t, filepath.Join(dst, "base", "5", "1259"), "changed", 0o600)
	if got := readFile(t, filepath.Join(src, "base", "5", "1259")); got != big {
		t.Fatal("writing the clone changed the source")
	}
}

func TestCloneTreeManyFiles(t *testing.T) {
	root := xfstest.Dir(t)
	src := filepath.Join(root, "src")
	for i := range 300 {
		writeFile(t, filepath.Join(src, fmt.Sprintf("d%02d", i%17), fmt.Sprintf("f%03d", i)), fmt.Sprintf("file %d", i), 0o600)
	}
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := volume.CloneTree(src, dst, nil); err != nil {
		t.Fatalf("CloneTree: %v", err)
	}
	for i := range 300 {
		rel := filepath.Join(fmt.Sprintf("d%02d", i%17), fmt.Sprintf("f%03d", i))
		if got := readFile(t, filepath.Join(dst, rel)); got != fmt.Sprintf("file %d", i) {
			t.Fatalf("%s = %q", rel, got)
		}
	}
}

func TestCloneTreeWithoutReflinkFails(t *testing.T) {
	// t.TempDir is not XFS: ext4 in the Lima VM, APFS on a Mac.
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a"), "data", 0o600)
	err := volume.CloneTree(src, t.TempDir(), nil)
	if !errors.Is(err, volume.ErrReflinkUnsupported) {
		t.Fatalf("CloneTree = %v, want ErrReflinkUnsupported", err)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/volume/`
Expected: FAIL — `no non-test Go files` 또는 `undefined: volume.CloneTree`

- [ ] **Step 3: 구현**

`internal/volume/doc.go`:

```go
// Package volume manages a project volume (spec 6, 7.2): reflink copies of branch data, data
// generations, restore points and their catalog.
package volume
```

`internal/volume/errors.go`:

```go
package volume

import (
	"errors"
	"fmt"
	"time"
)

// Errors of Volume operations. The agent API maps each to a status code and an error code.
var (
	ErrInvalid               = errors.New("invalid request")
	ErrNotFound              = errors.New("not found")
	ErrConflict              = errors.New("conflict")
	ErrNotRunning            = errors.New("postgres is not running")
	ErrBusy                  = errors.New("another volume operation is in progress")
	ErrInsufficientStorage   = errors.New("volume usage is above the copy limit")
	ErrSupervisorUnavailable = errors.New("branch supervisor unavailable")
	ErrReflinkUnsupported    = errors.New("reflink copies are not supported here")
)

// RateLimitError is returned when a backup-mode copy of a branch comes sooner than
// MinCopyInterval after the previous one ended (spec 7.2).
type RateLimitError struct {
	Source     string        // the branch copied from
	RetryAfter time.Duration // how long until the next copy is allowed
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("copies of branch %q are limited: retry in %s", e.Source, e.RetryAfter.Round(time.Millisecond))
}
```

`internal/volume/clone.go`:

```go
package volume

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// SkipFunc reports whether the entry at rel (slash-separated, relative to the source root) is
// left out of a copy. A skipped directory is left out with everything in it.
type SkipFunc func(rel string) bool

// CloneTree reflink-copies the contents of the directory src into the existing directory dst.
// Regular files are cloned with FICLONE: no data is copied, and the copy shares extents with
// the source until either side writes. Directories and symlinks are re-created, permission bits
// are kept, and dst gets src's. Entries that disappear while the tree is walked are left out: a
// running PostgreSQL deletes files during a backup, and WAL replay accounts for that. Other file
// types are an error. src and dst must be on one filesystem that supports reflink; otherwise
// the error wraps ErrReflinkUnsupported.
func CloneTree(src, dst string, skip SkipFunc) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("clone %s: not a directory", src)
	}

	type fileJob struct {
		src, dst string
		perm     fs.FileMode
	}
	type dirMode struct {
		path string
		perm fs.FileMode
	}
	var (
		jobs     = make(chan fileJob)
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		dirs     = []dirMode{{dst, info.Mode().Perm()}}
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if err := cloneFile(j.src, j.dst, j.perm); err != nil {
					fail(err)
				}
			}
		}()
	}

	walkErr := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path != src && errors.Is(err, fs.ErrNotExist) {
				return nil // deleted while we walked
			}
			return err
		}
		if failed() {
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skip != nil && skip(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.IsDir():
			fi, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			if err != nil {
				return err
			}
			// Writable while the files go in; the real permission bits come at the end.
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{target, fi.Mode().Perm()})
			return nil
		case d.Type().IsRegular():
			fi, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			jobs <- fileJob{path, target, fi.Mode().Perm()}
			return nil
		default:
			return fmt.Errorf("clone %s: unsupported file type %s", path, d.Type())
		}
	})
	close(jobs)
	wg.Wait()
	if walkErr != nil {
		return walkErr
	}
	if firstErr != nil {
		return firstErr
	}
	// Deepest first, so every directory can still be entered while its children get their modes.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].perm); err != nil {
			return err
		}
	}
	return nil
}

// cloneFile creates dst with permission bits perm and clones src's contents into it. A source
// deleted before it could be opened is left out.
func cloneFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := ficlone(out, in); err != nil {
		out.Close()
		return fmt.Errorf("clone %s: %w", src, err)
	}
	if err := out.Chmod(perm); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
```

`internal/volume/ficlone_linux.go`:

```go
//go:build linux && (amd64 || arm64)

package volume

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// ficloneRequest is the FICLONE ioctl, _IOW(0x94, 9, int), as encoded on amd64 and arm64.
const ficloneRequest = 0x40049409

// ficlone makes dst share all of src's extents.
func ficlone(dst, src *os.File) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficloneRequest, src.Fd())
	runtime.KeepAlive(dst)
	runtime.KeepAlive(src)
	switch errno {
	case 0:
		return nil
	case syscall.EOPNOTSUPP, syscall.EXDEV, syscall.ENOTTY, syscall.EINVAL:
		return fmt.Errorf("%w: %v", ErrReflinkUnsupported, errno)
	default:
		return errno
	}
}
```

`internal/volume/ficlone_other.go`:

```go
//go:build !(linux && (amd64 || arm64))

package volume

import (
	"fmt"
	"os"
	"runtime"
)

// ficlone is implemented only on Linux for amd64 and arm64.
func ficlone(dst, src *os.File) error {
	return fmt.Errorf("%w on %s/%s", ErrReflinkUnsupported, runtime.GOOS, runtime.GOARCH)
}
```

- [ ] **Step 4: Mac에서 확인 (다른 플랫폼 빌드 포함)**

Run: `go vet ./... && go test -v ./internal/volume/ && GOOS=linux GOARCH=amd64 go vet ./internal/volume/ && GOOS=linux GOARCH=arm64 go vet ./internal/volume/ && GOOS=linux GOARCH=riscv64 go build ./internal/volume/`
Expected: `TestCloneTreeWithoutReflinkFails` PASS, XFS 테스트 두 개 SKIP, 네 플랫폼 모두 오류 없음

- [ ] **Step 5: VM에서 테스트 통과 확인**

Run: `hack/vm-test.sh -race -v ./internal/volume/...`
Expected: `TestCloneTreeCopiesTheTree`, `TestCloneTreeManyFiles`, `TestCloneTreeWithoutReflinkFails` 모두 PASS(SKIP 없음), 경쟁 상태 경고 없음

- [ ] **Step 6: 커밋**

```bash
git add internal/volume/
git commit -m "feat(volume): FICLONE으로 디렉터리 트리를 reflink 복사" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: 볼륨 레이아웃

**Files:**
- Create: `internal/volume/layout.go`
- Test: `internal/volume/layout_test.go` (패키지 `volume`, 내부 테스트)

**Interfaces:**
- Consumes: `ErrNotFound` (Task 3)
- Produces (패키지 내부): 상수 `branchesDir`, `restoreDir`, `metaDir`, `currentLink`, `currentTmpLink`, `incompleteMarker`, `generationPrefix`; `type layout struct{ root string }`와 메서드 `branchDir(branch)`, `genDir(branch, n)`, `restoreBranchDir(branch)`, `restorePointDir(branch, id)`, `catalogPath()`, `trashPath()`, `rel(path) string`, `currentGeneration(branch) (int, error)`, `generations(branch) ([]int, error)`, `nextGeneration(branch) (int, error)`; 함수 `generationName(n int) string`, `parseGeneration(name string) (int, bool)`, `complete(dir string) bool`, `switchCurrent(branchDir string, n int) error`, `makeMarked(dir string) error`, `finishMarked(dir string) error`, `syncDir(dir string) error`, `writeFileSync(path string, data []byte, perm fs.FileMode) error`.

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/volume/layout_test.go`:

```go
package volume

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func mustMkdirAll(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParseGeneration(t *testing.T) {
	for name, want := range map[string]int{"data-1": 1, "data-12": 12} {
		if n, ok := parseGeneration(name); !ok || n != want {
			t.Errorf("parseGeneration(%q) = %d, %v; want %d", name, n, ok, want)
		}
	}
	for _, name := range []string{"data-0", "data-01", "data-", "data--1", "data-+1", "data-1a", "data", "current", "data-1.tmp"} {
		if n, ok := parseGeneration(name); ok {
			t.Errorf("parseGeneration(%q) = %d, want rejected", name, n)
		}
	}
	if got := generationName(7); got != "data-7" {
		t.Errorf("generationName(7) = %q", got)
	}
}

func TestCurrentLink(t *testing.T) {
	l := layout{root: t.TempDir()}
	bd := l.branchDir("dev")
	mustMkdirAll(t, l.genDir("dev", 1), l.genDir("dev", 2))
	if _, err := l.currentGeneration("dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("without a link: %v, want ErrNotFound", err)
	}
	for _, n := range []int{1, 2} {
		if err := switchCurrent(bd, n); err != nil {
			t.Fatal(err)
		}
		if got, err := l.currentGeneration("dev"); err != nil || got != n {
			t.Fatalf("after switching to %d: %d, %v", n, got, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(bd, "current")); err != nil || target != "data-2" {
		t.Fatalf("link = %q, %v; want the relative target data-2", target, err)
	}
	if _, err := os.Lstat(filepath.Join(bd, "current.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("current.tmp left behind")
	}
	// A link to a missing or unfinished generation is no current generation.
	if err := switchCurrent(bd, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := l.currentGeneration("dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link to a missing generation: %v, want ErrNotFound", err)
	}
	if err := makeMarked(l.genDir("dev", 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.currentGeneration("dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link to an unfinished generation: %v, want ErrNotFound", err)
	}
	if err := finishMarked(l.genDir("dev", 3)); err != nil {
		t.Fatal(err)
	}
	if got, err := l.currentGeneration("dev"); err != nil || got != 3 {
		t.Fatalf("after finishing data-3: %d, %v", got, err)
	}
}

func TestGenerationListing(t *testing.T) {
	l := layout{root: t.TempDir()}
	mustMkdirAll(t, l.genDir("main", 1), l.genDir("main", 3), filepath.Join(l.branchDir("main"), "data-x"))
	if err := makeMarked(l.genDir("main", 4)); err != nil {
		t.Fatal(err)
	}
	gens, err := l.generations("main")
	if err != nil || !slices.Equal(gens, []int{1, 3}) {
		t.Fatalf("generations = %v, %v; want [1 3] (data-4 is unfinished)", gens, err)
	}
	if next, err := l.nextGeneration("main"); err != nil || next != 5 {
		t.Fatalf("nextGeneration = %d, %v; want 5 (the unfinished data-4 counts)", next, err)
	}
	if _, err := l.generations("nobody"); err == nil {
		t.Fatal("generations of a missing branch succeeded")
	}
}

func TestMarkedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rp")
	if err := makeMarked(dir); err != nil {
		t.Fatal(err)
	}
	if complete(dir) {
		t.Fatal("a marked directory counts as complete")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %o, want 700", fi.Mode().Perm())
	}
	if err := makeMarked(dir); err == nil {
		t.Fatal("makeMarked reused an existing directory")
	}
	if err := finishMarked(dir); err != nil {
		t.Fatal(err)
	}
	if !complete(dir) {
		t.Fatal("a finished directory is not complete")
	}
	if err := finishMarked(dir); err == nil {
		t.Fatal("finishMarked succeeded without a marker")
	}
}

func TestWriteFileSyncReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup_label")
	for _, s := range []string{"first and longer", "second"} {
		if err := writeFileSync(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "second" {
		t.Fatalf("file = %q, %v; want second", b, err)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/volume/`
Expected: FAIL — `undefined: parseGeneration`, `undefined: layout` 등

- [ ] **Step 3: 구현**

`internal/volume/layout.go`:

```go
package volume

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The volume layout (spec 6), relative to the volume root.
const (
	branchesDir      = "branches"
	restoreDir       = "restore"
	metaDir          = ".consonance"
	currentLink      = "current"
	currentTmpLink   = "current.tmp"
	incompleteMarker = ".incomplete"
	generationPrefix = "data-"
)

// layout turns names into paths in a volume.
type layout struct{ root string }

func (l layout) branchDir(branch string) string {
	return filepath.Join(l.root, branchesDir, branch)
}

func (l layout) genDir(branch string, n int) string {
	return filepath.Join(l.branchDir(branch), generationName(n))
}

func (l layout) restoreBranchDir(branch string) string {
	return filepath.Join(l.root, restoreDir, branch)
}

func (l layout) restorePointDir(branch, id string) string {
	return filepath.Join(l.restoreBranchDir(branch), id)
}

func (l layout) catalogPath() string { return filepath.Join(l.root, metaDir, "catalog.json") }

func (l layout) trashPath() string { return filepath.Join(l.root, metaDir, "trash") }

// rel returns path relative to the volume root, slash-separated (catalog paths, log lines).
func (l layout) rel(path string) string {
	r, err := filepath.Rel(l.root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

// generationName is the directory name of generation n.
func generationName(n int) string { return generationPrefix + strconv.Itoa(n) }

// parseGeneration returns n for "data-n" (n ≥ 1, without sign or leading zeros).
func parseGeneration(name string) (int, bool) {
	digits, ok := strings.CutPrefix(name, generationPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || generationName(n) != name {
		return 0, false
	}
	return n, true
}

// complete reports whether dir is a directory without the .incomplete marker (spec 6).
func complete(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	_, err = os.Lstat(filepath.Join(dir, incompleteMarker))
	return errors.Is(err, fs.ErrNotExist)
}

// currentGeneration returns the generation the branch's current link points at. A branch whose
// link is missing or leads to no complete generation has no data: ErrNotFound.
func (l layout) currentGeneration(branch string) (int, error) {
	target, err := os.Readlink(filepath.Join(l.branchDir(branch), currentLink))
	if err != nil {
		return 0, fmt.Errorf("%w: branch %q has no data", ErrNotFound, branch)
	}
	n, ok := parseGeneration(target)
	if !ok || !complete(l.genDir(branch, n)) {
		return 0, fmt.Errorf("%w: branch %q: current points at %q, which is no complete generation", ErrNotFound, branch, target)
	}
	return n, nil
}

// generations returns the branch's complete generations in ascending order.
func (l layout) generations(branch string) ([]int, error) {
	entries, err := os.ReadDir(l.branchDir(branch))
	if err != nil {
		return nil, err
	}
	var gens []int
	for _, e := range entries {
		if n, ok := parseGeneration(e.Name()); ok && e.IsDir() && complete(l.genDir(branch, n)) {
			gens = append(gens, n)
		}
	}
	slices.Sort(gens)
	return gens, nil
}

// nextGeneration returns one more than the highest data-n in the branch directory, finished or
// not, so a new generation never reuses a name.
func (l layout) nextGeneration(branch string) (int, error) {
	entries, err := os.ReadDir(l.branchDir(branch))
	if err != nil {
		return 0, err
	}
	next := 1
	for _, e := range entries {
		if n, ok := parseGeneration(e.Name()); ok && n >= next {
			next = n + 1
		}
	}
	return next, nil
}

// switchCurrent points branchDir/current at data-n atomically (spec 6): it makes the link
// current.tmp, renames it over current and fsyncs the directory.
func switchCurrent(branchDir string, n int) error {
	tmp := filepath.Join(branchDir, currentTmpLink)
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Symlink(generationName(n), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(branchDir, currentLink)); err != nil {
		return err
	}
	return syncDir(branchDir)
}

// makeMarked creates dir (mode 0700) holding the .incomplete marker (spec 6). It stays marked
// until finishMarked; Recover deletes it if the agent stops first.
func makeMarked(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, incompleteMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// finishMarked removes the marker of a directory whose copy is complete and makes that durable.
func finishMarked(dir string) error {
	if err := os.Remove(filepath.Join(dir, incompleteMarker)); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
}

// syncDir fsyncs a directory so that the entries created, renamed or removed in it are durable.
// On XFS it forces the log, which makes every earlier metadata change durable too.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeFileSync writes data to path (created or truncated, mode perm) and fsyncs it.
func writeFileSync(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go vet ./internal/volume/ && go test -v -run 'Generation|CurrentLink|Marked|WriteFileSync' ./internal/volume/`
Expected: `TestParseGeneration`, `TestCurrentLink`, `TestGenerationListing`, `TestMarkedDirectory`, `TestWriteFileSyncReplaces` PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/volume/layout.go internal/volume/layout_test.go
git commit -m "feat(volume): 볼륨 레이아웃 — 세대 이름, current 링크, 미완성 표시" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: 에이전트 API 계약과 복구 지점 카탈로그

**Files:**
- Create: `internal/agentapi/types.go`, `internal/volume/catalog.go`
- Test: `internal/agentapi/types_test.go`, `internal/volume/catalog_test.go` (패키지 `volume`, 내부 테스트)

**Interfaces:**
- Consumes: `writeFileSync`, `syncDir` (Task 4)
- Produces: 패키지 `agentapi` 전체(아래 코드 그대로: `TokenHeader`, `DefaultPort`, `MainBranch`, `ValidBranchName`, `ValidRestorePointID`, `SourceType`·`SourceParentLatest`·`SourceRestorePoint`, `Source`, `ReasonAPI`·`ReasonReset`, `RestorePoint`, `Preflight`, `Usage`, `Generation`, `CreateBranchDataRequest`, `BranchData`, `CreateRestorePointRequest`, `RestorePointList`, `Generations`, `CreateGenerationRequest`, `DetachRequest`, `DetachResponse`, `ErrorCode`와 `Code*` 상수, `Error`); 패키지 `volume` 내부 `type catalog`, `type catalogEntry`, `loadCatalog(path string) (catalog, error)`, `saveCatalog(path string, c catalog) error`, `newRestorePointID(now time.Time) string`, `expired(entries []catalogEntry, branch string, now time.Time, maxPerBranch int, maxAge time.Duration) []catalogEntry`.

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/agentapi/types_test.go`:

```go
package agentapi_test

import (
	"strings"
	"testing"

	"consonance/internal/agentapi"
)

func TestValidBranchName(t *testing.T) {
	for _, name := range []string{"main", "dev-vibe-1", "a", "0", strings.Repeat("a", 40), "main-before-20261007031500"} {
		if !agentapi.ValidBranchName(name) {
			t.Errorf("ValidBranchName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "Dev", "-dev", "dev-", "dev_1", "dev.1", "..", "../x", "a/b", "%2F", "dev 1", strings.Repeat("a", 41)} {
		if agentapi.ValidBranchName(name) {
			t.Errorf("ValidBranchName(%q) = true", name)
		}
	}
}

func TestValidRestorePointID(t *testing.T) {
	if !agentapi.ValidRestorePointID("rp-20261007T031500Z-ab12") {
		t.Error("the spec 4.4 example is rejected")
	}
	for _, id := range []string{"", "rp-20261007T031500Z-AB12", "rp-20261007T031500-ab12", "rp-20261007T031500Z-ab1", "../rp-20261007T031500Z-ab12", "rp-20261007T031500Z-ab12/x"} {
		if agentapi.ValidRestorePointID(id) {
			t.Errorf("ValidRestorePointID(%q) = true", id)
		}
	}
}
```

`internal/volume/catalog_test.go`:

```go
package volume

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"consonance/internal/agentapi"
)

func TestCatalogRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	created := time.Date(2026, 10, 7, 3, 15, 0, 0, time.UTC)
	want := catalogEntry{
		RestorePoint: agentapi.RestorePoint{
			ID: "rp-20261007T031500Z-ab12", Branch: "main", CreatedAt: created,
			Reason: agentapi.ReasonAPI, Label: "before schema change", BackupStartLSN: "0/3000028",
		},
		Path: "restore/main/rp-20261007T031500Z-ab12",
	}
	if err := saveCatalog(path, catalog{RestorePoints: []catalogEntry{want}}); err != nil {
		t.Fatal(err)
	}
	got, err := loadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || len(got.RestorePoints) != 1 {
		t.Fatalf("loaded %+v", got)
	}
	entry := got.RestorePoints[0]
	if !entry.CreatedAt.Equal(created) {
		t.Fatalf("createdAt = %v, want %v", entry.CreatedAt, created)
	}
	entry.CreatedAt = created
	if entry != want {
		t.Fatalf("entry = %+v, want %+v", entry, want)
	}

	// The file uses the spec 4.4 field names.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version       int              `json:"version"`
		RestorePoints []map[string]any `json:"restorePoints"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "branch", "createdAt", "reason", "label", "backupStartLSN", "path"} {
		if _, ok := doc.RestorePoints[0][key]; !ok {
			t.Errorf("catalog entry lacks %q:\n%s", key, raw)
		}
	}
	if doc.RestorePoints[0]["createdAt"] != "2026-10-07T03:15:00Z" {
		t.Errorf("createdAt is written as %v", doc.RestorePoints[0]["createdAt"])
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Error("temporary file left behind")
	}
}

func TestLoadCatalogMissingIsEmpty(t *testing.T) {
	c, err := loadCatalog(filepath.Join(t.TempDir(), "catalog.json"))
	if err != nil || c.Version != 1 || len(c.RestorePoints) != 0 {
		t.Fatalf("loadCatalog = %+v, %v; want an empty version 1 catalog", c, err)
	}
}

func TestLoadCatalogRejectsOtherVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"restorePoints":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCatalog(path); err == nil {
		t.Fatal("loaded a version 2 catalog")
	}
}

func TestNewRestorePointID(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 15, 0, 0, time.FixedZone("KST", 9*60*60))
	id := newRestorePointID(now)
	if !agentapi.ValidRestorePointID(id) {
		t.Fatalf("id %q has the wrong form", id)
	}
	if !strings.HasPrefix(id, "rp-20261007T031500Z-") {
		t.Fatalf("id %q does not carry the UTC time", id)
	}
	if newRestorePointID(now) == id && newRestorePointID(now) == id {
		t.Fatal("ids do not vary")
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	entry := func(id, branch string, age time.Duration) catalogEntry {
		return catalogEntry{RestorePoint: agentapi.RestorePoint{ID: id, Branch: branch, CreatedAt: now.Add(-age)}}
	}
	entries := []catalogEntry{
		entry("a", "main", time.Hour), entry("b", "main", 2*time.Hour), entry("c", "main", 3*time.Hour),
		entry("d", "main", 30*time.Hour), entry("x", "dev", 40*time.Hour),
	}
	ids := func(es []catalogEntry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.ID)
		}
		slices.Sort(out)
		return out
	}
	// Two per branch and 24 hours: d is too old, c is the third newest.
	if got := ids(expired(entries, "main", now, 2, 24*time.Hour)); !slices.Equal(got, []string{"c", "d"}) {
		t.Errorf("expired = %v, want [c d]", got)
	}
	if got := expired(entries, "main", now, 0, 0); len(got) != 0 {
		t.Errorf("without limits expired = %v, want none", ids(got))
	}
	if got := ids(expired(entries, "dev", now, 20, 24*time.Hour)); !slices.Equal(got, []string{"x"}) {
		t.Errorf("dev expired = %v, want [x]", got)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/agentapi/ ./internal/volume/`
Expected: FAIL — `no non-test Go files`(agentapi), `undefined: catalogEntry`(volume)

- [ ] **Step 3: 구현**

`internal/agentapi/types.go`:

```go
// Package agentapi is the wire contract of the volume agent's internal HTTP API (spec 7.2).
package agentapi

import (
	"regexp"
	"time"
)

const (
	// TokenHeader carries the project's internal token, the same one the supervisors use.
	TokenHeader = "X-Consonance-Token"
	// DefaultPort is the agent API port.
	DefaultPort = 8443
	// MainBranch is the project's first branch. Only POST /v1/init-main makes its data.
	MainBranch = "main"
)

var (
	branchNameRE     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	restorePointIDRE = regexp.MustCompile(`^rp-[0-9]{8}T[0-9]{6}Z-[a-z0-9]{4}$`)
)

// ValidBranchName reports whether name is a DNS-1123 label of at most 40 characters (spec 4.3).
func ValidBranchName(name string) bool {
	return len(name) <= 40 && branchNameRE.MatchString(name)
}

// ValidRestorePointID reports whether id has the form rp-<UTC yyyymmddThhmmssZ>-<4 characters>
// (spec 4.4).
func ValidRestorePointID(id string) bool { return restorePointIDRE.MatchString(id) }

// SourceType says what a new branch or generation is copied from.
type SourceType string

const (
	// SourceParentLatest is the parent's running database, copied in backup mode.
	SourceParentLatest SourceType = "ParentLatest"
	// SourceRestorePoint is a restore point: the parent's for a new branch, the branch's own
	// for a rollback.
	SourceRestorePoint SourceType = "RestorePoint"
)

// Source is the origin of a new branch or generation.
type Source struct {
	Type           SourceType `json:"type"`
	RestorePointID string     `json:"restorePointId,omitempty"`
}

// Restore point reasons (spec 4.4).
const (
	ReasonAPI   = "api"   // requested through the API
	ReasonReset = "reset" // the generation a reset replaced, kept automatically
)

// RestorePoint is one restore point (spec 4.4, without the catalog's path).
type RestorePoint struct {
	ID             string    `json:"id"`
	Branch         string    `json:"branch"`
	CreatedAt      time.Time `json:"createdAt"`
	Reason         string    `json:"reason"`
	Label          string    `json:"label"`
	BackupStartLSN string    `json:"backupStartLSN"`
}

// Preflight is returned by GET /v1/preflight.
type Preflight struct {
	Reflink bool   `json:"reflink"`
	Kernel  string `json:"kernel"`
	Error   string `json:"error,omitempty"` // why the reflink probe failed
}

// Usage is returned by GET /v1/usage (statfs of the volume).
type Usage struct {
	CapacityBytes  int64 `json:"capacityBytes"`
	UsedBytes      int64 `json:"usedBytes"`
	AvailableBytes int64 `json:"availableBytes"`
}

// Generation is returned by POST /v1/init-main and POST /v1/branches/{name}/generations.
type Generation struct {
	Generation int `json:"generation"`
}

// CreateBranchDataRequest is the body of POST /v1/branches/{name}/data.
type CreateBranchDataRequest struct {
	Parent string `json:"parent"`
	Source Source `json:"source"`
}

// BranchData is returned by POST /v1/branches/{name}/data.
type BranchData struct {
	Generation     int    `json:"generation"`
	BackupStartLSN string `json:"backupStartLSN"` // empty when the data existed already
}

// CreateRestorePointRequest is the body of POST /v1/branches/{name}/restore-points.
type CreateRestorePointRequest struct {
	Label  string `json:"label"`
	Reason string `json:"reason"` // "api" or empty
}

// RestorePointList is returned by GET /v1/branches/{name}/restore-points, newest first.
type RestorePointList struct {
	RestorePoints []RestorePoint `json:"restorePoints"`
}

// Generations is returned by GET /v1/branches/{name}/generations and by activate.
type Generations struct {
	Current     int   `json:"current"`
	Generations []int `json:"generations"` // complete generations, ascending
}

// CreateGenerationRequest is the body of POST /v1/branches/{name}/generations. Parent is needed
// when Source.Type is ParentLatest (a reset).
type CreateGenerationRequest struct {
	Parent string `json:"parent,omitempty"`
	Source Source `json:"source"`
}

// DetachRequest is the body of POST /v1/branches/{name}/generations/{n}/detach. Exactly one of
// ToBranch (a rollback keeps the old generation as a backup branch) and AsRestorePoint (a reset
// keeps it as a restore point with reason "reset") is set.
type DetachRequest struct {
	ToBranch       string `json:"toBranch,omitempty"`
	AsRestorePoint bool   `json:"asRestorePoint,omitempty"`
}

// DetachResponse says where the generation went.
type DetachResponse struct {
	Branch       string        `json:"branch,omitempty"`
	RestorePoint *RestorePoint `json:"restorePoint,omitempty"`
}

// ErrorCode tells callers what kind of failure a non-2xx response is.
type ErrorCode string

const (
	CodeInvalid               ErrorCode = "invalid"                // 400
	CodeUnauthorized          ErrorCode = "unauthorized"           // 401
	CodeNotFound              ErrorCode = "not-found"              // 404
	CodeConflict              ErrorCode = "conflict"               // 409: exists already, wrong state
	CodeNotRunning            ErrorCode = "not-running"            // 409: the source branch's PostgreSQL is not running
	CodeBusy                  ErrorCode = "busy"                   // 409: another volume operation held the lock too long
	CodeRateLimited           ErrorCode = "rate-limited"           // 429, with Retry-After
	CodeSupervisorUnavailable ErrorCode = "supervisor-unavailable" // 502
	CodeInsufficientStorage   ErrorCode = "insufficient-storage"   // 507
	CodeInternal              ErrorCode = "internal"               // 500
)

// Error is the body of every non-2xx response.
type Error struct {
	Error string    `json:"error"`
	Code  ErrorCode `json:"code"`
}
```

`internal/volume/catalog.go`:

```go
package volume

import (
	"cmp"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"consonance/internal/agentapi"
)

const catalogVersion = 1

// catalog is .consonance/catalog.json (spec 4.4). Only the agent reads and writes it.
type catalog struct {
	Version       int            `json:"version"`
	RestorePoints []catalogEntry `json:"restorePoints"`
}

// catalogEntry is a restore point and its directory, relative to the volume root.
type catalogEntry struct {
	agentapi.RestorePoint
	Path string `json:"path"`
}

// loadCatalog reads the catalog at path. A missing file is an empty catalog.
func loadCatalog(path string) (catalog, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return catalog{Version: catalogVersion}, nil
	}
	if err != nil {
		return catalog{}, err
	}
	var c catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return catalog{}, fmt.Errorf("catalog %s: %w", path, err)
	}
	if c.Version != catalogVersion {
		return catalog{}, fmt.Errorf("catalog %s: version %d, want %d", path, c.Version, catalogVersion)
	}
	return c, nil
}

// saveCatalog replaces the catalog at path atomically (spec 4.4): it writes a temporary file,
// fsyncs it, renames it over the old one and fsyncs the directory.
func saveCatalog(path string, c catalog) error {
	c.Version = catalogVersion
	if c.RestorePoints == nil {
		c.RestorePoints = []catalogEntry{}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// newRestorePointID returns rp-<UTC yyyymmddThhmmssZ>-<4 random characters> (spec 4.4).
func newRestorePointID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	suffix := make([]byte, len(b))
	for i, x := range b {
		suffix[i] = idAlphabet[int(x)%len(idAlphabet)]
	}
	return "rp-" + now.UTC().Format("20060102T150405Z") + "-" + string(suffix)
}

// expired returns the restore points of branch that the retention policy removes (spec 7.2):
// those older than maxAge, then the oldest beyond maxPerBranch. A zero limit is no limit.
func expired(entries []catalogEntry, branch string, now time.Time, maxPerBranch int, maxAge time.Duration) []catalogEntry {
	var mine []catalogEntry
	for _, e := range entries {
		if e.Branch == branch {
			mine = append(mine, e)
		}
	}
	slices.SortFunc(mine, func(a, b catalogEntry) int { // newest first
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	var out []catalogEntry
	for i, e := range mine {
		tooOld := maxAge > 0 && now.Sub(e.CreatedAt) > maxAge
		tooMany := maxPerBranch > 0 && i >= maxPerBranch
		if tooOld || tooMany {
			out = append(out, e)
		}
	}
	return out
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go vet ./internal/agentapi/ ./internal/volume/ && go test -v ./internal/agentapi/ && go test -v -run 'Catalog|RestorePointID|Expired' ./internal/volume/`
Expected: `TestValidBranchName`, `TestValidRestorePointID`, `TestCatalogRoundTrip`, `TestLoadCatalogMissingIsEmpty`, `TestLoadCatalogRejectsOtherVersions`, `TestNewRestorePointID`, `TestExpired` PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/agentapi/ internal/volume/catalog.go internal/volume/catalog_test.go
git commit -m "feat(agentapi,volume): 에이전트 API 계약과 복구 지점 카탈로그" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 6: Volume 핵심과 기동 시 정리

**Files:**
- Create: `internal/volume/volume.go`, `internal/volume/recover.go`, `internal/volume/export_test.go`
- Test: `internal/volume/helpers_test.go`, `internal/volume/volume_test.go`, `internal/volume/recover_test.go` (패키지 `volume_test`)

**Interfaces:**
- Consumes: `layout`과 그 메서드, `complete`, `parseGeneration`, `currentTmpLink` (Task 4), `loadCatalog`, `saveCatalog`, `catalogEntry` (Task 5), `cloneFile` (Task 3), `agentapi.Preflight`, `agentapi.Usage`, `supervisorapi.Status`·`BackupStartResponse`·`BackupStopResponse`(계획 1)
- Produces: `type volume.Supervisor interface { Status; BackupStart; BackupStop }`(`*supervisorclient.Client`의 메서드와 같은 시그니처), `type volume.Config struct { Root, InitDB string; InitDBArgs []string; Supervisor func(branch string) Supervisor; MinCopyInterval, LockWait time.Duration; MaxUsedPercent float64; RestorePointsPerBranch int; RestorePointMaxAge time.Duration; Now func() time.Time }`, `volume.Open(cfg Config) (*Volume, error)`, `(*Volume).Preflight() agentapi.Preflight`, `(*Volume).Usage() (agentapi.Usage, error)`, `(*Volume).Recover() ([]string, error)`, `(*Volume).EmptyTrash() error`; 패키지 내부 `acquire(ctx) (release func(), err error)`, `checkCopyInterval(source string) error`, `copyEnded(source string)`, `checkSpace() error`, `trash(path string) error`, `trashOrWarn(path string)`; 테스트 훅 `AcquireForTest`, `CopyEndedForTest`.
- 테스트 도우미(`helpers_test.go`): `fakeSupervisor`(필드 `statuses`, `startErr`; 메서드 `set(...)`, `calls() (starts, stops int)`), `running(gen)`, `stopped(gen)`, `errNoAnswer`, `unreachable`, `type supervisors map[string]*fakeSupervisor`와 `get`, `testClock`과 `newClock()`, `openVolume(t, root, sups, mod)`, `mkGeneration(t, root, branch, n, current) string`, `rpFixture`, `writeCatalog(t, root, rps...)`, `exists(path) bool`, `testCtx(t)`. 뒤 작업의 테스트가 그대로 쓴다.

- [ ] **Step 1: 테스트 도우미 쓰기**

`internal/volume/helpers_test.go`:

```go
package volume_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"consonance/internal/supervisorapi"
	"consonance/internal/volume"
)

// fakeSupervisor answers like a branch supervisor without a PostgreSQL behind it.
type fakeSupervisor struct {
	mu       sync.Mutex
	statuses []supervisorapi.Status // Status returns these in order, then repeats the last one
	startErr error                  // BackupStart fails with it when set
	starts   int                    // BackupStart calls
	stops    int                    // BackupStop calls
}

func (f *fakeSupervisor) Status(context.Context) (supervisorapi.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.statuses) == 0 {
		return supervisorapi.Status{PG: supervisorapi.PGStopped}, nil
	}
	st := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return st, nil
}

func (f *fakeSupervisor) BackupStart(context.Context, string) (supervisorapi.BackupStartResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.startErr != nil {
		return supervisorapi.BackupStartResponse{}, f.startErr
	}
	return supervisorapi.BackupStartResponse{StartLSN: "0/2000028"}, nil
}

func (f *fakeSupervisor) BackupStop(context.Context) (supervisorapi.BackupStopResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return supervisorapi.BackupStopResponse{
		LabelFile: "START WAL LOCATION: 0/2000028 (file 000000010000000000000002)\n",
		StopLSN:   "0/2000100",
	}, nil
}

// set replaces the statuses Status returns.
func (f *fakeSupervisor) set(statuses ...supervisorapi.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = statuses
}

// calls returns how often BackupStart and BackupStop were called.
func (f *fakeSupervisor) calls() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

func running(gen string) supervisorapi.Status {
	return supervisorapi.Status{PG: supervisorapi.PGRunning, Generation: gen}
}

func stopped(gen string) supervisorapi.Status {
	return supervisorapi.Status{PG: supervisorapi.PGStopped, Generation: gen}
}

// errNoAnswer is how a call to the supervisor of a branch without a pod fails.
var errNoAnswer = errors.New("dial tcp 10.42.0.9:8008: connect: connection refused")

// unreachable is the supervisor of a branch without a pod.
type unreachable struct{}

func (unreachable) Status(context.Context) (supervisorapi.Status, error) {
	return supervisorapi.Status{}, errNoAnswer
}

func (unreachable) BackupStart(context.Context, string) (supervisorapi.BackupStartResponse, error) {
	return supervisorapi.BackupStartResponse{}, errNoAnswer
}

func (unreachable) BackupStop(context.Context) (supervisorapi.BackupStopResponse, error) {
	return supervisorapi.BackupStopResponse{}, errNoAnswer
}

// supervisors maps branches to fake supervisors. Other branches have no pod.
type supervisors map[string]*fakeSupervisor

func (s supervisors) get(branch string) volume.Supervisor {
	if f, ok := s[branch]; ok {
		return f
	}
	return unreachable{}
}

// testClock is a settable clock for Config.Now.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 7, 3, 15, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// openVolume opens the volume at root with fake supervisors, a 50ms lock wait and no other
// limits; mod changes the configuration first.
func openVolume(t *testing.T, root string, sups supervisors, mod func(*volume.Config)) *volume.Volume {
	t.Helper()
	cfg := volume.Config{Root: root, Supervisor: sups.get, LockWait: 50 * time.Millisecond}
	if mod != nil {
		mod(&cfg)
	}
	v, err := volume.Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return v
}

// mkGeneration makes branches/<branch>/data-<n> holding PG_VERSION and, when current is true,
// points the branch's current link at it. It returns the generation directory.
func mkGeneration(t *testing.T, root, branch string, n int, current bool) string {
	t.Helper()
	dir := filepath.Join(root, "branches", branch, fmt.Sprintf("data-%d", n))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if current {
		link := filepath.Join(root, "branches", branch, "current")
		_ = os.Remove(link)
		if err := os.Symlink(fmt.Sprintf("data-%d", n), link); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// rpFixture is a restore point for writeCatalog.
type rpFixture struct {
	id, branch, reason string
	createdAt          time.Time
}

// writeCatalog writes .consonance/catalog.json in the spec 4.4 format and makes each restore
// point's directory with a PG_VERSION file.
func writeCatalog(t *testing.T, root string, rps ...rpFixture) {
	t.Helper()
	type entry struct {
		ID             string    `json:"id"`
		Branch         string    `json:"branch"`
		CreatedAt      time.Time `json:"createdAt"`
		Reason         string    `json:"reason"`
		Label          string    `json:"label"`
		BackupStartLSN string    `json:"backupStartLSN"`
		Path           string    `json:"path"`
	}
	doc := struct {
		Version       int     `json:"version"`
		RestorePoints []entry `json:"restorePoints"`
	}{Version: 1, RestorePoints: []entry{}}
	for _, rp := range rps {
		path := "restore/" + rp.branch + "/" + rp.id
		doc.RestorePoints = append(doc.RestorePoints, entry{
			ID: rp.id, Branch: rp.branch, CreatedAt: rp.createdAt, Reason: rp.reason,
			Label: "fixture", BackupStartLSN: "0/1000028", Path: path,
		})
		dir := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".consonance"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".consonance", "catalog.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// exists reports whether path exists, without following a final symlink.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}
```

- [ ] **Step 2: 실패하는 테스트 쓰기**

`internal/volume/volume_test.go`:

```go
package volume_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"consonance/internal/testutil/xfstest"
	"consonance/internal/volume"
)

func TestOpenCreatesLayout(t *testing.T) {
	root := t.TempDir()
	openVolume(t, root, nil, nil)
	for _, d := range []string{"branches", "restore", ".consonance", ".consonance/trash"} {
		if fi, err := os.Stat(filepath.Join(root, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
}

func TestOpenChecksItsConfiguration(t *testing.T) {
	sup := supervisors{}.get
	for name, cfg := range map[string]volume.Config{
		"no root":       {Supervisor: sup},
		"no supervisor": {Root: t.TempDir()},
		"missing root":  {Root: filepath.Join(t.TempDir(), "missing"), Supervisor: sup},
	} {
		if _, err := volume.Open(cfg); err == nil {
			t.Errorf("%s: Open succeeded", name)
		}
	}
}

func TestPreflightOnXFS(t *testing.T) {
	v := openVolume(t, xfstest.Dir(t), nil, nil)
	got := v.Preflight()
	if !got.Reflink || got.Error != "" {
		t.Fatalf("Preflight = %+v, want reflink", got)
	}
	if got.Kernel == "" || got.Kernel == "unknown" {
		t.Fatalf("kernel = %q", got.Kernel)
	}
}

func TestPreflightWithoutReflink(t *testing.T) {
	root := t.TempDir() // not XFS
	v := openVolume(t, root, nil, nil)
	got := v.Preflight()
	if got.Reflink || !strings.Contains(got.Error, "not supported") {
		t.Fatalf("Preflight = %+v, want no reflink and the reason", got)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".consonance"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "preflight-") {
			t.Fatalf("the probe left %s", e.Name())
		}
	}
}

func TestUsage(t *testing.T) {
	v := openVolume(t, t.TempDir(), nil, nil)
	u, err := v.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if u.CapacityBytes <= 0 || u.UsedBytes < 0 || u.UsedBytes > u.CapacityBytes || u.AvailableBytes > u.CapacityBytes {
		t.Fatalf("implausible usage %+v", u)
	}
}

func TestLockWaitsThenReportsBusy(t *testing.T) {
	v := openVolume(t, t.TempDir(), nil, func(c *volume.Config) { c.LockWait = 100 * time.Millisecond })
	ctx := testCtx(t)
	release, err := v.AcquireForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := v.AcquireForTest(ctx); !errors.Is(err, volume.ErrBusy) {
		t.Fatalf("second acquire = %v, want ErrBusy", err)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("gave up after %s, before the lock wait ended", waited)
	}
	// A holder that lets go within the wait hands the lock over.
	go func() {
		time.Sleep(20 * time.Millisecond)
		release()
	}()
	release2, err := v.AcquireForTest(ctx)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}
```

`internal/volume/recover_test.go`:

```go
package volume_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRecoverRemovesLeftovers(t *testing.T) {
	root := t.TempDir()
	created := newClock().Now()
	// Complete data: it must survive.
	mkGeneration(t, root, "main", 1, true)
	mkGeneration(t, root, "main", 2, false) // finished, not activated yet
	writeCatalog(t, root,
		rpFixture{id: "rp-20261007T031500Z-ab12", branch: "main", reason: "api", createdAt: created},
		rpFixture{id: "rp-20261007T031600Z-gone", branch: "main", reason: "api", createdAt: created},
	)
	if err := os.RemoveAll(filepath.Join(root, "restore", "main", "rp-20261007T031600Z-gone")); err != nil {
		t.Fatal(err)
	}
	// What interrupted operations leave behind: a generation copy cut short,
	writeFile(t, filepath.Join(root, "branches", "main", "data-3", ".incomplete"), "", 0o600)
	// a link switch cut short,
	if err := os.Symlink("data-2", filepath.Join(root, "branches", "main", "current.tmp")); err != nil {
		t.Fatal(err)
	}
	// a branch copy cut short after linking,
	half := mkGeneration(t, root, "half", 1, true)
	writeFile(t, filepath.Join(half, ".incomplete"), "", 0o600)
	// an initdb that never got its link,
	mkGeneration(t, root, "nolink", 1, false)
	// a restore point that never made it into the catalog, one cut short,
	writeFile(t, filepath.Join(root, "restore", "main", "rp-20261007T031700Z-cd34", "PG_VERSION"), "17\n", 0o600)
	writeFile(t, filepath.Join(root, "restore", "dev", "rp-20261007T031800Z-ef56", ".incomplete"), "", 0o600)
	// and an interrupted preflight.
	writeFile(t, filepath.Join(root, ".consonance", "preflight-123", "src"), "x", 0o600)

	v := openVolume(t, root, nil, nil)
	removed, err := v.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	slices.Sort(removed)
	want := []string{
		".consonance/preflight-123",
		"branches/half",
		"branches/half/data-1",
		"branches/main/current.tmp",
		"branches/main/data-3",
		"branches/nolink",
		"catalog:rp-20261007T031600Z-gone",
		"restore/dev/rp-20261007T031800Z-ef56",
		"restore/main/rp-20261007T031700Z-cd34",
	}
	if !slices.Equal(removed, want) {
		t.Fatalf("removed %q\nwant    %q", removed, want)
	}
	for _, p := range []string{"branches/main/data-1", "branches/main/data-2", "restore/main/rp-20261007T031500Z-ab12"} {
		if !exists(filepath.Join(root, p)) {
			t.Errorf("%s was removed", p)
		}
	}
	if target, err := os.Readlink(filepath.Join(root, "branches", "main", "current")); err != nil || target != "data-1" {
		t.Errorf("main/current = %q, %v; want data-1", target, err)
	}
	cat := readFile(t, filepath.Join(root, ".consonance", "catalog.json"))
	if !strings.Contains(cat, "rp-20261007T031500Z-ab12") || strings.Contains(cat, "gone") {
		t.Errorf("catalog after Recover:\n%s", cat)
	}
	// What Recover removed waits in the trash until EmptyTrash.
	if err := v.EmptyTrash(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, ".consonance", "trash")); err != nil || len(entries) != 0 {
		t.Fatalf("trash after EmptyTrash: %v, %v", entries, err)
	}
	if again, err := v.Recover(); err != nil || len(again) != 0 {
		t.Fatalf("second Recover = %q, %v; want nothing to do", again, err)
	}
}
```

- [ ] **Step 3: 테스트가 실패하는지 확인**

Run: `go test ./internal/volume/`
Expected: FAIL — `undefined: volume.Config`, `undefined: volume.Open` 등

- [ ] **Step 4: 구현**

`internal/volume/volume.go`:

```go
package volume

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

// Supervisor is what the volume needs from a branch's supervisor (spec 7.3). A
// *supervisorclient.Client is one.
type Supervisor interface {
	Status(ctx context.Context) (supervisorapi.Status, error)
	BackupStart(ctx context.Context, label string) (supervisorapi.BackupStartResponse, error)
	BackupStop(ctx context.Context) (supervisorapi.BackupStopResponse, error)
}

// Config configures a Volume. A zero limit is no limit; cmd/agent passes the spec 14 defaults.
type Config struct {
	Root                   string                         // volume root, e.g. /volume
	InitDB                 string                         // initdb binary for InitMain
	InitDBArgs             []string                       // more initdb arguments (tests pass --no-sync)
	Supervisor             func(branch string) Supervisor // the supervisor of a branch
	MinCopyInterval        time.Duration                  // between backup-mode copies of one branch (5s)
	LockWait               time.Duration                  // longest wait for the operation lock (2s)
	MaxUsedPercent         float64                        // copies are refused above this volume usage (90)
	RestorePointsPerBranch int                            // restore points kept per branch (20)
	RestorePointMaxAge     time.Duration                  // older restore points are removed (24h)
	Now                    func() time.Time               // clock; nil means time.Now
}

// Volume is one project volume. Operations that change it take the operation lock and so run
// one at a time (spec 7.2); reads do without.
type Volume struct {
	layout
	cfg     Config
	lock    chan struct{} // the operation lock: held while it holds a value
	trashed chan struct{} // tells the janitor that the trash has something

	mu       sync.Mutex
	lastCopy map[string]time.Time // source branch → when its last backup-mode copy ended
}

// Open checks the volume root and creates the top-level directories. Call Recover before
// serving requests.
func Open(cfg Config) (*Volume, error) {
	if cfg.Root == "" {
		return nil, errors.New("volume: Root is required")
	}
	if cfg.Supervisor == nil {
		return nil, errors.New("volume: Supervisor is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	fi, err := os.Stat(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("volume: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("volume: %s is not a directory", cfg.Root)
	}
	v := &Volume{
		layout:   layout{root: cfg.Root},
		cfg:      cfg,
		lock:     make(chan struct{}, 1),
		trashed:  make(chan struct{}, 1),
		lastCopy: map[string]time.Time{},
	}
	for _, dir := range []string{
		filepath.Join(cfg.Root, branchesDir), filepath.Join(cfg.Root, restoreDir),
		filepath.Join(cfg.Root, metaDir), v.trashPath(),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("volume: %w", err)
		}
	}
	return v, nil
}

// acquire takes the operation lock, waiting at most LockWait (spec 7.2: then 409 busy).
func (v *Volume) acquire(ctx context.Context) (release func(), err error) {
	release = func() { <-v.lock }
	select {
	case v.lock <- struct{}{}:
		return release, nil
	default:
	}
	t := time.NewTimer(v.cfg.LockWait)
	defer t.Stop()
	select {
	case v.lock <- struct{}{}:
		return release, nil
	case <-t.C:
		return nil, fmt.Errorf("%w (waited %s)", ErrBusy, v.cfg.LockWait)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// checkCopyInterval refuses a backup-mode copy of source that comes sooner than
// MinCopyInterval after the previous one ended (spec 7.2).
func (v *Volume) checkCopyInterval(source string) error {
	if v.cfg.MinCopyInterval <= 0 {
		return nil
	}
	v.mu.Lock()
	last, ok := v.lastCopy[source]
	v.mu.Unlock()
	if !ok {
		return nil
	}
	if wait := v.cfg.MinCopyInterval - v.cfg.Now().Sub(last); wait > 0 {
		return &RateLimitError{Source: source, RetryAfter: wait}
	}
	return nil
}

// copyEnded records that a backup-mode copy of source ended now.
func (v *Volume) copyEnded(source string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lastCopy[source] = v.cfg.Now()
}

// Usage reports the volume's size and use (statfs).
func (v *Volume) Usage() (agentapi.Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(v.root, &st); err != nil {
		return agentapi.Usage{}, err
	}
	bs := uint64(st.Bsize)
	return agentapi.Usage{
		CapacityBytes:  int64(st.Blocks * bs),
		UsedBytes:      int64((st.Blocks - st.Bfree) * bs),
		AvailableBytes: int64(st.Bavail * bs),
	}, nil
}

// checkSpace refuses a copy while the volume is fuller than MaxUsedPercent (spec 7.2: 507).
func (v *Volume) checkSpace() error {
	if v.cfg.MaxUsedPercent <= 0 {
		return nil
	}
	u, err := v.Usage()
	if err != nil {
		return err
	}
	if u.CapacityBytes == 0 {
		return nil
	}
	if used := float64(u.UsedBytes) * 100 / float64(u.CapacityBytes); used > v.cfg.MaxUsedPercent {
		return fmt.Errorf("%w: %.1f%% used, copies stop above %g%%", ErrInsufficientStorage, used, v.cfg.MaxUsedPercent)
	}
	return nil
}

// Preflight clones a probe file to check that the volume can make reflink copies (spec 7.2
// GET /v1/preflight) and reports the kernel version.
func (v *Volume) Preflight() agentapi.Preflight {
	res := agentapi.Preflight{Kernel: kernelVersion()}
	if err := v.probeReflink(); err != nil {
		res.Error = err.Error()
		return res
	}
	res.Reflink = true
	return res
}

func (v *Volume) probeReflink() error {
	dir, err := os.MkdirTemp(filepath.Join(v.root, metaDir), "preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	want := make([]byte, 64<<10)
	_, _ = rand.Read(want)
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, want, 0o600); err != nil {
		return err
	}
	dst := filepath.Join(dir, "dst")
	if err := cloneFile(src, dst, 0o600); err != nil {
		return err
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errors.New("the reflink copy differs from its source")
	}
	return nil
}

// kernelVersion returns the running kernel's release, or "unknown" off Linux.
func kernelVersion() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}

// trash moves path into .consonance/trash, from where EmptyTrash deletes it outside the
// operation lock. A missing path is not an error.
func (v *Volume) trash(path string) error {
	var b [8]byte
	_, _ = rand.Read(b[:])
	if err := os.Rename(path, filepath.Join(v.trashPath(), fmt.Sprintf("%x", b))); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	select {
	case v.trashed <- struct{}{}:
	default:
	}
	return nil
}

// trashOrWarn trashes path and only logs a failure: the caller has a more important error to
// report, and Recover removes what is left at the next start.
func (v *Volume) trashOrWarn(path string) {
	if err := v.trash(path); err != nil {
		slog.Warn("remove directory", "path", v.rel(path), "err", err)
	}
}

// EmptyTrash deletes everything in the trash.
func (v *Volume) EmptyTrash() error {
	entries, err := os.ReadDir(v.trashPath())
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(v.trashPath(), e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

`internal/volume/recover.go`:

```go
package volume

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Recover clears up after operations that an agent restart cut short (spec 6, 10). Run it before
// serving requests. It trashes generation and restore point directories still marked
// .incomplete, current.tmp links, branch directories whose current link is missing or leads
// nowhere (a branch whose creation did not finish), restore point directories the catalog does
// not list and preflight leftovers, and drops catalog entries whose directory is gone. Complete
// data is not touched. It returns what it removed, relative to the volume root; a dropped
// catalog entry appears as "catalog:<id>".
func (v *Volume) Recover() ([]string, error) {
	var removed []string
	drop := func(path string) error {
		if err := v.trash(path); err != nil {
			return err
		}
		removed = append(removed, v.rel(path))
		return nil
	}

	meta, err := os.ReadDir(filepath.Join(v.root, metaDir))
	if err != nil {
		return nil, err
	}
	for _, e := range meta {
		if strings.HasPrefix(e.Name(), "preflight-") {
			if err := drop(filepath.Join(v.root, metaDir, e.Name())); err != nil {
				return removed, err
			}
		}
	}

	branches, err := os.ReadDir(filepath.Join(v.root, branchesDir))
	if err != nil {
		return removed, err
	}
	for _, b := range branches {
		if !b.IsDir() {
			continue
		}
		bd := v.branchDir(b.Name())
		tmp := filepath.Join(bd, currentTmpLink)
		if err := os.Remove(tmp); err == nil {
			removed = append(removed, v.rel(tmp))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		entries, err := os.ReadDir(bd)
		if err != nil {
			return removed, err
		}
		for _, e := range entries {
			dir := filepath.Join(bd, e.Name())
			if _, ok := parseGeneration(e.Name()); ok && e.IsDir() && !complete(dir) {
				if err := drop(dir); err != nil {
					return removed, err
				}
			}
		}
		if _, err := v.currentGeneration(b.Name()); err != nil {
			if err := drop(bd); err != nil {
				return removed, err
			}
		}
	}

	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return removed, err
	}
	listed := map[string]bool{}
	var kept []catalogEntry
	for _, e := range cat.RestorePoints {
		dir := filepath.Join(v.root, filepath.FromSlash(e.Path))
		if complete(dir) {
			kept = append(kept, e)
			listed[dir] = true
		} else {
			removed = append(removed, "catalog:"+e.ID)
		}
	}
	if len(kept) != len(cat.RestorePoints) {
		cat.RestorePoints = kept
		if err := saveCatalog(v.catalogPath(), cat); err != nil {
			return removed, err
		}
	}
	restoreBranches, err := os.ReadDir(filepath.Join(v.root, restoreDir))
	if err != nil {
		return removed, err
	}
	for _, rb := range restoreBranches {
		if !rb.IsDir() {
			continue
		}
		rbd := v.restoreBranchDir(rb.Name())
		rps, err := os.ReadDir(rbd)
		if err != nil {
			return removed, err
		}
		for _, rp := range rps {
			if dir := filepath.Join(rbd, rp.Name()); !listed[dir] {
				if err := drop(dir); err != nil {
					return removed, err
				}
			}
		}
		_ = os.Remove(rbd) // succeeds only when nothing is left in it
	}
	return removed, nil
}
```

`internal/volume/export_test.go`:

```go
package volume

import "context"

// AcquireForTest takes the operation lock the way an operation does.
func (v *Volume) AcquireForTest(ctx context.Context) (release func(), err error) {
	return v.acquire(ctx)
}

// CopyEndedForTest records a backup-mode copy of source that ended now.
func (v *Volume) CopyEndedForTest(source string) { v.copyEnded(source) }
```

- [ ] **Step 5: Mac에서 통과 확인**

Run: `go vet ./... && go test -race ./internal/volume/`
Expected: `ok  consonance/internal/volume` (`TestPreflightOnXFS`와 XFS 복사 테스트는 SKIP)

- [ ] **Step 6: VM에서 통과 확인**

Run: `hack/vm-test.sh -race -v ./internal/volume/...`
Expected: `TestPreflightOnXFS`를 포함해 모두 PASS(SKIP 없음)

- [ ] **Step 7: 커밋**

```bash
git add internal/volume/
git commit -m "feat(volume): Volume 설정·잠금·사용량·preflight와 기동 시 정리" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: init-main

**Files:**
- Create: `internal/volume/initmain.go`
- Test: `internal/volume/initmain_test.go` (패키지 `volume_test`)

**Interfaces:**
- Consumes: `acquire`, `trash`, `trashOrWarn` (Task 6), `currentGeneration`, `branchDir`, `generationName`, `switchCurrent`, `writeFileSync` (Task 4), `pgtest.BinDir`(계획 1)
- Produces: `(*Volume).InitMain(ctx context.Context) (agentapi.Generation, error)`, 상수 `pgHBA`(패키지 내부).

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/volume/initmain_test.go`:

```go
package volume_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"consonance/internal/testutil/pgtest"
	"consonance/internal/volume"
)

// hbaRules returns the rule lines of a pg_hba.conf with their fields single-spaced.
func hbaRules(conf string) []string {
	var out []string
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Join(strings.Fields(line), " "))
	}
	return out
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

func TestInitMainCreatesFirstGeneration(t *testing.T) {
	bin := pgtest.BinDir(t)
	root := t.TempDir()
	v := openVolume(t, root, nil, func(c *volume.Config) {
		c.InitDB = filepath.Join(bin, "initdb")
		c.InitDBArgs = []string{"--no-sync"}
	})
	ctx := testCtx(t)
	got, err := v.InitMain(ctx)
	if err != nil {
		t.Fatalf("InitMain: %v", err)
	}
	if got.Generation != 1 {
		t.Fatalf("generation = %d, want 1", got.Generation)
	}
	main := filepath.Join(root, "branches", "main")
	if target, err := os.Readlink(filepath.Join(main, "current")); err != nil || target != "data-1" {
		t.Fatalf("current = %q, %v; want data-1", target, err)
	}
	if ver := strings.TrimSpace(readFile(t, filepath.Join(main, "data-1", "PG_VERSION"))); ver != "17" {
		t.Fatalf("PG_VERSION = %q", ver)
	}
	wantHBA := []string{
		"local all postgres peer",
		"local all all scram-sha-256",
		"host all postgres all reject",
		"host all all all scram-sha-256",
	}
	if rules := hbaRules(readFile(t, filepath.Join(main, "data-1", "pg_hba.conf"))); !slices.Equal(rules, wantHBA) {
		t.Fatalf("pg_hba.conf rules = %q, want %q", rules, wantHBA)
	}
	out, err := exec.Command(filepath.Join(bin, "pg_controldata"), filepath.Join(main, "data-1")).CombinedOutput()
	if err != nil {
		t.Fatalf("pg_controldata: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`Data page checksum version:\s+1`).Match(out) {
		t.Fatalf("data checksums are off:\n%s", out)
	}

	// A second call keeps the data.
	before := inode(t, filepath.Join(main, "data-1", "PG_VERSION"))
	again, err := v.InitMain(ctx)
	if err != nil || again.Generation != 1 {
		t.Fatalf("second InitMain = %+v, %v", again, err)
	}
	if inode(t, filepath.Join(main, "data-1", "PG_VERSION")) != before {
		t.Fatal("the second InitMain made main again")
	}
}

func TestInitMainReplacesAnUnfinishedAttempt(t *testing.T) {
	bin := pgtest.BinDir(t)
	root := t.TempDir()
	// An earlier initdb was cut short: data without a current link.
	writeFile(t, filepath.Join(root, "branches", "main", "data-1", "junk"), "half", 0o600)
	v := openVolume(t, root, nil, func(c *volume.Config) {
		c.InitDB = filepath.Join(bin, "initdb")
		c.InitDBArgs = []string{"--no-sync"}
	})
	if _, err := v.InitMain(testCtx(t)); err != nil {
		t.Fatalf("InitMain: %v", err)
	}
	if exists(filepath.Join(root, "branches", "main", "data-1", "junk")) {
		t.Fatal("the leftover was kept")
	}
	if !exists(filepath.Join(root, "branches", "main", "data-1", "PG_VERSION")) {
		t.Fatal("no new data")
	}
}

func TestInitMainFailureLeavesNoMain(t *testing.T) {
	root := t.TempDir()
	v := openVolume(t, root, nil, func(c *volume.Config) { c.InitDB = filepath.Join(root, "no-such-initdb") })
	if _, err := v.InitMain(testCtx(t)); err == nil {
		t.Fatal("InitMain succeeded without initdb")
	}
	if exists(filepath.Join(root, "branches", "main")) {
		t.Fatal("a failed InitMain left branches/main")
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/volume/`
Expected: FAIL — `v.InitMain undefined`

- [ ] **Step 3: 구현**

`internal/volume/initmain.go`:

```go
package volume

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"consonance/internal/agentapi"
)

// pgHBA is every branch's client authentication (spec 4.5): the superuser only through the
// pod-local socket with peer authentication, everyone else with SCRAM, and the superuser
// rejected over TCP. Branches inherit it with their data.
const pgHBA = `# Written by the Consonance volume agent (spec 4.5).
# TYPE  DATABASE  USER      ADDRESS  METHOD
local   all       postgres           peer
local   all       all                scram-sha-256
host    all       postgres  all      reject
host    all       all       all      scram-sha-256
`

// InitMain creates main's first generation with initdb and points current at it (spec 7.2
// POST /v1/init-main). It is the only way main's data is made. When main has data already it
// changes nothing and returns main's current generation.
func (v *Volume) InitMain(ctx context.Context) (agentapi.Generation, error) {
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.Generation{}, err
	}
	defer release()

	if n, err := v.currentGeneration(agentapi.MainBranch); err == nil {
		return agentapi.Generation{Generation: n}, nil
	}
	bd := v.branchDir(agentapi.MainBranch)
	if err := v.trash(bd); err != nil { // what an unfinished earlier attempt left
		return agentapi.Generation{}, err
	}
	if err := v.initMain(ctx, bd); err != nil {
		v.trashOrWarn(bd)
		return agentapi.Generation{}, err
	}
	return agentapi.Generation{Generation: 1}, nil
}

func (v *Volume) initMain(ctx context.Context, bd string) error {
	if err := os.Mkdir(bd, 0o700); err != nil {
		return err
	}
	data := filepath.Join(bd, generationName(1))
	args := append([]string{
		"-D", data,
		"-U", "postgres",
		"--auth-local=peer",
		"--auth-host=scram-sha-256",
		"--encoding=UTF8",
		"--locale-provider=builtin",
		"--locale=C.UTF-8",
		"--data-checksums",
	}, v.cfg.InitDBArgs...)
	if out, err := exec.CommandContext(ctx, v.cfg.InitDB, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("initdb: %w: %s", err, bytes.TrimSpace(out))
	}
	if err := writeFileSync(filepath.Join(data, "pg_hba.conf"), []byte(pgHBA), 0o600); err != nil {
		return err
	}
	// The link is the commit point: Recover removes a main without one.
	return switchCurrent(bd, 1)
}
```

- [ ] **Step 4: Mac과 VM에서 통과 확인**

Run: `go vet ./... && go test -run InitMain -v ./internal/volume/ && hack/vm-test.sh -race -run InitMain -v ./internal/volume/...`
Expected: Mac에서 `TestInitMainFailureLeavesNoMain` PASS(나머지 둘은 SKIP), VM에서 세 테스트 모두 PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/volume/initmain.go internal/volume/initmain_test.go
git commit -m "feat(volume): init-main — initdb와 pg_hba로 main의 첫 세대 생성" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 8: 실행 중인 부모에서 브랜치 데이터 만들기

**Files:**
- Create: `internal/volume/snapshot.go`, `internal/volume/branches.go`
- Test: `internal/volume/snapshot_internal_test.go` (패키지 `volume`), `internal/volume/pgenv_test.go`, `internal/volume/branches_test.go` (패키지 `volume_test`)

**Interfaces:**
- Consumes: Task 4~6의 레이아웃·잠금·간격·공간·휴지통 도우미, `CloneTree`(Task 3), `supervisorclient.APIError`·`IsConflict`·`New`, `supervisor.Server`, `pgctl.NewManager`·`NewBackups`·`Options`, `pgtest.BinDir`·`InitDBAt`·`SocketDir`·`FreePort`(계획 1), `xfstest.Dir`(Task 1)
- Produces: `(*Volume).CreateBranchData(ctx, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error)`(이 작업에서는 `ParentLatest`만, Task 9가 `RestorePoint`를 더한다); 패키지 내부 `snapshot(ctx, source, dst, label string) (string, error)`, `status(ctx, branch) (supervisorapi.Status, error)`, `supervisorErr(branch, call string, err error) error`, `skipLive`, `skipMarker`, `startSegment(labelFile string) string`, `makeFirstGeneration(name string, fill func(dst string) (string, error)) (string, error)`, 상수 `statusTimeout = 5s`, `backupTimeout = 30s`.
- 테스트 도우미(`pgenv_test.go`): `type pgEnv`, `newPGEnv(t, mod) *pgEnv`(XFS 위 볼륨, initdb한 main을 진짜 PostgreSQL과 진짜 감독 프로세스 API로 띄움), 메서드 `start(t, branch)`, `client(branch) *supervisorclient.Client`, `exec(t, branch, sql)`, `count(t, branch) int`, `queryBool(t, branch, sql) bool`; `parentLatest(parent)`(`branches_test.go`). 뒤 작업의 테스트가 쓴다.

- [ ] **Step 1: 실제 PostgreSQL 테스트 환경 쓰기**

`internal/volume/pgenv_test.go`:

```go
package volume_test

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"consonance/internal/pgctl"
	"consonance/internal/supervisor"
	"consonance/internal/supervisorclient"
	"consonance/internal/testutil/pgtest"
	"consonance/internal/testutil/xfstest"
	"consonance/internal/volume"
)

const testToken = "test-token"

// pgEnv is a volume on XFS whose branches run real PostgreSQL servers behind real supervisor
// APIs, as branch pods do.
type pgEnv struct {
	root string
	bin  string
	vol  *volume.Volume

	mu   sync.Mutex
	urls map[string]string         // branch → supervisor API URL
	pgs  map[string]*pgctl.Manager // branch → its PostgreSQL
}

// newPGEnv makes main with initdb, starts it and opens the volume. mod changes the volume
// configuration (newPGEnv sets Root and Supervisor; LockWait is 2s unless mod changes it).
func newPGEnv(t *testing.T, mod func(*volume.Config)) *pgEnv {
	t.Helper()
	e := &pgEnv{root: xfstest.Dir(t), bin: pgtest.BinDir(t), urls: map[string]string{}, pgs: map[string]*pgctl.Manager{}}
	mainDir := filepath.Join(e.root, "branches", "main")
	if err := os.MkdirAll(mainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pgtest.InitDBAt(t, e.bin, filepath.Join(mainDir, "data-1"))
	if err := os.Symlink("data-1", filepath.Join(mainDir, "current")); err != nil {
		t.Fatal(err)
	}
	cfg := volume.Config{LockWait: 2 * time.Second}
	if mod != nil {
		mod(&cfg)
	}
	cfg.Root, cfg.Supervisor = e.root, e.supervisor
	v, err := volume.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.vol = v
	e.start(t, "main")
	return e
}

func (e *pgEnv) supervisor(branch string) volume.Supervisor { return e.client(branch) }

// client returns a client of the branch's supervisor API. A branch that runs no PostgreSQL
// gets an address where nothing listens, like a branch without a pod.
func (e *pgEnv) client(branch string) *supervisorclient.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	url, ok := e.urls[branch]
	if !ok {
		url = "http://127.0.0.1:1"
	}
	return supervisorclient.New(url, testToken, nil)
}

// start runs PostgreSQL on branches/<branch>/current with the supervisor API in front of it.
func (e *pgEnv) start(t *testing.T, branch string) {
	t.Helper()
	m := pgctl.NewManager(filepath.Join(e.bin, "postgres"), pgctl.Options{
		DataDir:        filepath.Join(e.root, "branches", branch, "current"),
		SocketDir:      pgtest.SocketDir(t),
		Port:           pgtest.FreePort(t),
		MaxConnections: 20,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start %s: %v", branch, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = m.Stop(ctx, 10*time.Second)
	})
	srv := httptest.NewServer((&supervisor.Server{
		Token:        testToken,
		PG:           m,
		Backups:      pgctl.NewBackups(m.ConnString(), 30*time.Second),
		Generation:   m.Generation,
		StopGrace:    10 * time.Second,
		StartTimeout: 60 * time.Second,
	}).Handler())
	t.Cleanup(srv.Close)
	e.mu.Lock()
	e.urls[branch], e.pgs[branch] = srv.URL, m
	e.mu.Unlock()
}

func (e *pgEnv) connect(t *testing.T, branch string) *pgx.Conn {
	t.Helper()
	e.mu.Lock()
	m := e.pgs[branch]
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, m.ConnString())
	if err != nil {
		t.Fatalf("connect %s: %v", branch, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// exec runs sql on the branch as the superuser.
func (e *pgEnv) exec(t *testing.T, branch, sql string) {
	t.Helper()
	if _, err := e.connect(t, branch).Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %s: %v", branch, sql, err)
	}
}

// count returns the number of rows in table t on the branch.
func (e *pgEnv) count(t *testing.T, branch string) int {
	t.Helper()
	var n int
	if err := e.connect(t, branch).QueryRow(context.Background(), "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("%s: count: %v", branch, err)
	}
	return n
}

// queryBool returns the boolean that sql yields on the branch.
func (e *pgEnv) queryBool(t *testing.T, branch, sql string) bool {
	t.Helper()
	var b bool
	if err := e.connect(t, branch).QueryRow(context.Background(), sql).Scan(&b); err != nil {
		t.Fatalf("%s: %s: %v", branch, sql, err)
	}
	return b
}
```

- [ ] **Step 2: 실패하는 테스트 쓰기**

`internal/volume/snapshot_internal_test.go`:

```go
package volume

import "testing"

func TestStartSegment(t *testing.T) {
	label := "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n" +
		"CHECKPOINT LOCATION: 0/3000080\nBACKUP METHOD: streamed\nBACKUP FROM: primary\n" +
		"START TIME: 2026-10-07 03:15:00 UTC\nLABEL: consonance branch dev\nSTART TIMELINE: 1\n"
	if got := startSegment(label); got != "000000010000000000000003" {
		t.Fatalf("startSegment = %q", got)
	}
	if got := startSegment("LABEL: x\n"); got != "" {
		t.Fatalf("startSegment without the line = %q", got)
	}
}
```

`internal/volume/branches_test.go`:

```go
package volume_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
	"consonance/internal/volume"
)

func parentLatest(parent string) agentapi.CreateBranchDataRequest {
	return agentapi.CreateBranchDataRequest{Parent: parent, Source: agentapi.Source{Type: agentapi.SourceParentLatest}}
}

func TestCreateBranchFromRunningParent(t *testing.T) {
	e := newPGEnv(t, nil)
	ctx := testCtx(t)
	e.exec(t, "main", "CREATE TABLE t (id int); INSERT INTO t SELECT generate_series(1, 1000)")

	got, err := e.vol.CreateBranchData(ctx, "dev", parentLatest("main"))
	if err != nil {
		t.Fatalf("CreateBranchData: %v", err)
	}
	if got.Generation != 1 || got.BackupStartLSN == "" {
		t.Fatalf("got %+v", got)
	}
	data := filepath.Join(e.root, "branches", "dev", "data-1")
	if target, err := os.Readlink(filepath.Join(e.root, "branches", "dev", "current")); err != nil || target != "data-1" {
		t.Fatalf("dev/current = %q, %v", target, err)
	}
	if label := readFile(t, filepath.Join(data, "backup_label")); !strings.Contains(label, "LABEL: consonance branch dev") {
		t.Fatalf("backup_label:\n%s", label)
	}
	for _, name := range []string{"postmaster.pid", "postmaster.opts", ".incomplete"} {
		if exists(filepath.Join(data, name)) {
			t.Errorf("the copy has %s", name)
		}
	}
	if wal, err := os.ReadDir(filepath.Join(data, "pg_wal")); err != nil || len(wal) == 0 {
		t.Fatalf("pg_wal: %v, %v", wal, err)
	}

	e.exec(t, "main", "INSERT INTO t VALUES (1001)") // after the copy: must not reach dev
	e.start(t, "dev")
	if n := e.count(t, "dev"); n != 1000 {
		t.Fatalf("dev has %d rows, want 1000", n)
	}
	e.exec(t, "dev", "INSERT INTO t SELECT generate_series(2001, 2100)")
	if n := e.count(t, "main"); n != 1001 {
		t.Fatalf("main has %d rows, want 1001", n)
	}

	// A retried call finds the data and leaves it alone.
	again, err := e.vol.CreateBranchData(ctx, "dev", parentLatest("main"))
	if err != nil || again.Generation != 1 || again.BackupStartLSN != "" {
		t.Fatalf("retried CreateBranchData = %+v, %v", again, err)
	}
	if n := e.count(t, "dev"); n != 1100 {
		t.Fatalf("dev has %d rows after the retry, want 1100", n)
	}
}

func TestCreateBranchDataValidates(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{running("data-1")}}}, nil)
	ctx := testCtx(t)
	for _, tc := range []struct {
		name string
		req  agentapi.CreateBranchDataRequest
	}{
		{"main", parentLatest("main")}, // main's data comes only from init-main
		{"Dev", parentLatest("main")},
		{"../dev", parentLatest("main")},
		{"dev", parentLatest("")},
		{"dev", parentLatest("dev")},
		{"dev", agentapi.CreateBranchDataRequest{Parent: "main", Source: agentapi.Source{Type: "Snapshot"}}},
	} {
		if _, err := v.CreateBranchData(ctx, tc.name, tc.req); !errors.Is(err, volume.ErrInvalid) {
			t.Errorf("CreateBranchData(%q, %+v) = %v, want ErrInvalid", tc.name, tc.req, err)
		}
	}
}

func TestCreateBranchDataNeedsParentData(t *testing.T) {
	v := openVolume(t, t.TempDir(), nil, nil)
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestCreateBranchDataNeedsRunningParent(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	main := &fakeSupervisor{statuses: []supervisorapi.Status{stopped("data-1")}}
	v := openVolume(t, root, supervisors{"main": main}, nil)
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrNotRunning) {
		t.Fatalf("got %v, want ErrNotRunning", err)
	}
	if starts, _ := main.calls(); starts != 0 {
		t.Fatal("a backup was started on a stopped parent")
	}
	if exists(filepath.Join(root, "branches", "dev")) {
		t.Fatal("branches/dev was left behind")
	}
}

func TestCreateBranchDataWithoutSupervisorAnswer(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	v := openVolume(t, root, nil, nil) // main has no pod
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrSupervisorUnavailable) {
		t.Fatalf("got %v, want ErrSupervisorUnavailable", err)
	}
}

func TestCopyRefusesParentThatRestarted(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	mkGeneration(t, root, "main", 2, false)
	// The backup lands on a server that was restarted on data-2 after the first status call.
	main := &fakeSupervisor{statuses: []supervisorapi.Status{running("data-1"), running("data-2")}}
	v := openVolume(t, root, supervisors{"main": main}, nil)
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	if starts, stops := main.calls(); starts != 1 || stops != 1 {
		t.Fatalf("backup start/stop calls = %d/%d, want 1/1", starts, stops)
	}
	if exists(filepath.Join(root, "branches", "dev")) {
		t.Fatal("a partial branch was left behind")
	}
}

func TestFailedCopyEndsBackupAndCleansUp(t *testing.T) {
	// t.TempDir is not XFS, so the clone fails after the backup has started.
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	main := &fakeSupervisor{statuses: []supervisorapi.Status{running("data-1")}}
	clock := newClock()
	v := openVolume(t, root, supervisors{"main": main}, func(c *volume.Config) {
		c.MinCopyInterval = time.Minute
		c.Now = clock.Now
	})
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrReflinkUnsupported) {
		t.Fatalf("got %v, want ErrReflinkUnsupported", err)
	}
	if starts, stops := main.calls(); starts != 1 || stops != 1 {
		t.Fatalf("backup start/stop calls = %d/%d, want 1/1 (the backup ends at once)", starts, stops)
	}
	if exists(filepath.Join(root, "branches", "dev")) {
		t.Fatal("a partial branch was left behind")
	}
	// The failed copy still cost main a checkpoint, so it counts for the interval.
	var rl *volume.RateLimitError
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.As(err, &rl) || rl.RetryAfter != time.Minute {
		t.Fatalf("second copy = %v, want a rate limit of 1m", err)
	}
}

func TestCopyIntervalCountsFromTheEndOfTheLastCopy(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	main := &fakeSupervisor{statuses: []supervisorapi.Status{stopped("data-1")}}
	clock := newClock()
	v := openVolume(t, root, supervisors{"main": main}, func(c *volume.Config) {
		c.MinCopyInterval = 5 * time.Second
		c.Now = clock.Now
	})
	v.CopyEndedForTest("main")
	clock.Advance(4 * time.Second)
	var rl *volume.RateLimitError
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.As(err, &rl) || rl.RetryAfter != time.Second || rl.Source != "main" {
		t.Fatalf("got %v, want main limited for 1s", err)
	}
	clock.Advance(time.Second)
	// The interval is over: the copy goes ahead and stops at the stopped parent.
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrNotRunning) {
		t.Fatalf("got %v, want ErrNotRunning", err)
	}
}

func TestCreateBranchDataRefusesFullVolume(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	main := &fakeSupervisor{statuses: []supervisorapi.Status{running("data-1")}}
	// Any filesystem in use is above a 0.0001% limit.
	v := openVolume(t, root, supervisors{"main": main}, func(c *volume.Config) { c.MaxUsedPercent = 0.0001 })
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrInsufficientStorage) {
		t.Fatalf("got %v, want ErrInsufficientStorage", err)
	}
	if starts, _ := main.calls(); starts != 0 {
		t.Fatal("a backup was started on a full volume")
	}
}

func TestCreateBranchDataWaitsForTheLock(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	v := openVolume(t, root, nil, nil)
	release, err := v.AcquireForTest(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
}

func TestOpenBackupOnParentIsBusy(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	main := &fakeSupervisor{
		statuses: []supervisorapi.Status{running("data-1")},
		startErr: &supervisorclient.APIError{StatusCode: 409, Message: "a backup is already in progress"},
	}
	v := openVolume(t, root, supervisors{"main": main}, nil)
	if _, err := v.CreateBranchData(testCtx(t), "dev", parentLatest("main")); !errors.Is(err, volume.ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
	if exists(filepath.Join(root, "branches", "dev")) {
		t.Fatal("branches/dev was left behind")
	}
}
```

- [ ] **Step 3: 테스트가 실패하는지 확인**

Run: `go test ./internal/volume/`
Expected: FAIL — `undefined: startSegment`, `v.CreateBranchData undefined`

- [ ] **Step 4: 구현**

`internal/volume/snapshot.go`:

```go
package volume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
)

const (
	statusTimeout = 5 * time.Second  // one status call
	backupTimeout = 30 * time.Second // backup start (a checkpoint) or backup stop
)

// skipLive leaves out of a running data directory's copy what must not come along (spec 7.2):
// pg_wal is copied after the backup ends, and the postmaster files belong to the running server.
func skipLive(rel string) bool {
	switch rel {
	case "pg_wal", "postmaster.pid", "postmaster.opts", incompleteMarker:
		return true
	}
	return false
}

// skipMarker leaves out only the .incomplete marker (copies of restore points).
func skipMarker(rel string) bool { return rel == incompleteMarker }

// supervisorErr wraps a failed supervisor call. A call that got no answer is
// ErrSupervisorUnavailable; an answered one keeps the supervisor's error.
func supervisorErr(branch, call string, err error) error {
	var apiErr *supervisorclient.APIError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("supervisor of %q: %s: %w", branch, call, err)
	}
	return fmt.Errorf("%w: %s of branch %q: %v", ErrSupervisorUnavailable, call, branch, err)
}

// status asks a branch's supervisor for its PostgreSQL state.
func (v *Volume) status(ctx context.Context, branch string) (supervisorapi.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	st, err := v.cfg.Supervisor(branch).Status(ctx)
	if err != nil {
		return st, supervisorErr(branch, "status", err)
	}
	return st, nil
}

var startSegmentRE = regexp.MustCompile(`START WAL LOCATION: \S+ \(file ([0-9A-F]{24})\)`)

// startSegment returns the WAL segment named on a backup label's START WAL LOCATION line.
func startSegment(labelFile string) string {
	if m := startSegmentRE.FindStringSubmatch(labelFile); m != nil {
		return m[1]
	}
	return ""
}

// snapshot copies the running database of branch source into dst, an existing marked
// directory, so that the copy starts on its own as a consistent server (spec 7.2, PoC): backup
// start, clone all but pg_wal and the postmaster files, backup stop, write backup_label, clone
// pg_wal. It copies the generation the supervisor reports running, which is not always the one
// current points at. It returns the backup's start LSN.
func (v *Volume) snapshot(ctx context.Context, source, dst, label string) (string, error) {
	sup := v.cfg.Supervisor(source)
	pre, err := v.status(ctx, source)
	if err != nil {
		return "", err
	}
	if pre.PG != supervisorapi.PGRunning {
		return "", fmt.Errorf("%w: branch %q is %s", ErrNotRunning, source, pre.PG)
	}

	// From here on the source pays for a checkpoint, so the copy counts for MinCopyInterval
	// even when it fails.
	defer v.copyEnded(source)
	startCtx, cancel := context.WithTimeout(ctx, backupTimeout)
	start, err := sup.BackupStart(startCtx, label)
	cancel()
	if err != nil {
		if supervisorclient.IsConflict(err) { // a backup is still open, e.g. from a copy cut short
			return "", fmt.Errorf("%w: backup start on branch %q: %v", ErrBusy, source, err)
		}
		return "", supervisorErr(source, "backup start", err)
	}
	ended := false
	defer func() {
		if !ended { // end the backup now rather than when the supervisor's 30s timeout fires
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusTimeout)
			defer cancel()
			_, _ = sup.BackupStop(stopCtx)
		}
	}()

	// The backup runs on the server that answered. If that server was restarted between the two
	// status calls, it may run another generation than the one copied below: refuse. A restart
	// after this point closes the backup session, and backup stop fails.
	post, err := v.status(ctx, source)
	if err != nil {
		return "", err
	}
	gen, ok := parseGeneration(post.Generation)
	if post.PG != supervisorapi.PGRunning || post.Generation != pre.Generation || !ok {
		return "", fmt.Errorf("%w: branch %q restarted during the copy (%s %q, then %s %q)",
			ErrConflict, source, pre.PG, pre.Generation, post.PG, post.Generation)
	}
	srcDir := v.genDir(source, gen)
	if err := CloneTree(srcDir, dst, skipLive); err != nil {
		return "", err
	}

	ended = true
	stopCtx, cancel := context.WithTimeout(ctx, backupTimeout)
	stop, err := sup.BackupStop(stopCtx)
	cancel()
	if err != nil {
		return "", supervisorErr(source, "backup stop", err)
	}
	if err := writeFileSync(filepath.Join(dst, "backup_label"), []byte(stop.LabelFile), 0o600); err != nil {
		return "", err
	}
	walDst := filepath.Join(dst, "pg_wal")
	if err := os.Mkdir(walDst, 0o700); err != nil {
		return "", err
	}
	if err := CloneTree(filepath.Join(srcDir, "pg_wal"), walDst, nil); err != nil {
		return "", err
	}
	// Recovery replays WAL from the backup's start. A checkpoint right after the backup started
	// could have recycled that segment before pg_wal was cloned, and then the copy is useless.
	seg := startSegment(stop.LabelFile)
	if seg == "" {
		return "", fmt.Errorf("the backup label of branch %q names no start segment", source)
	}
	if _, err := os.Lstat(filepath.Join(walDst, seg)); err != nil {
		return "", fmt.Errorf("the copy of branch %q lacks WAL segment %s: %w", source, seg, err)
	}
	return start.StartLSN, nil
}
```

`internal/volume/branches.go`:

```go
package volume

import (
	"context"
	"fmt"
	"os"

	"consonance/internal/agentapi"
)

// CreateBranchData makes the data of a new branch (spec 7.2 POST /v1/branches/{name}/data,
// 9.2): generation data-1 holding a copy of the parent's running database, with current
// pointing at it. When the branch already has data it changes nothing and returns the current
// generation, so the operator can retry.
func (v *Volume) CreateBranchData(ctx context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error) {
	if !agentapi.ValidBranchName(name) || name == agentapi.MainBranch {
		return agentapi.BranchData{}, fmt.Errorf("%w: branch name %q", ErrInvalid, name)
	}
	if !agentapi.ValidBranchName(req.Parent) || req.Parent == name {
		return agentapi.BranchData{}, fmt.Errorf("%w: parent %q", ErrInvalid, req.Parent)
	}
	if req.Source.Type != agentapi.SourceParentLatest {
		return agentapi.BranchData{}, fmt.Errorf("%w: source type %q", ErrInvalid, req.Source.Type)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.BranchData{}, err
	}
	defer release()

	if n, err := v.currentGeneration(name); err == nil {
		return agentapi.BranchData{Generation: n}, nil
	}
	if _, err := v.currentGeneration(req.Parent); err != nil {
		return agentapi.BranchData{}, fmt.Errorf("%w: parent %q has no data", ErrNotFound, req.Parent)
	}
	if err := v.checkCopyInterval(req.Parent); err != nil {
		return agentapi.BranchData{}, err
	}
	if err := v.checkSpace(); err != nil {
		return agentapi.BranchData{}, err
	}
	lsn, err := v.makeFirstGeneration(name, func(dst string) (string, error) {
		return v.snapshot(ctx, req.Parent, dst, "consonance branch "+name)
	})
	if err != nil {
		return agentapi.BranchData{}, err
	}
	return agentapi.BranchData{Generation: 1, BackupStartLSN: lsn}, nil
}

// makeFirstGeneration creates branches/<name>/data-1, fills it, points current at it and only
// then removes its .incomplete marker, so that Recover removes the branch if the agent stops
// on the way. On failure it removes the branch directory again. fill returns the copy's
// backup start LSN.
func (v *Volume) makeFirstGeneration(name string, fill func(dst string) (string, error)) (string, error) {
	bd := v.branchDir(name)
	if err := v.trash(bd); err != nil { // what an unfinished earlier attempt left
		return "", err
	}
	if err := os.Mkdir(bd, 0o700); err != nil {
		return "", err
	}
	data := v.genDir(name, 1)
	lsn, err := func() (string, error) {
		if err := makeMarked(data); err != nil {
			return "", err
		}
		lsn, err := fill(data)
		if err != nil {
			return "", err
		}
		if err := switchCurrent(bd, 1); err != nil {
			return "", err
		}
		return lsn, finishMarked(data)
	}()
	if err != nil {
		v.trashOrWarn(bd)
		return "", err
	}
	return lsn, nil
}
```

- [ ] **Step 5: Mac에서 통과 확인**

Run: `go vet ./... && go test -race ./internal/volume/`
Expected: `ok  consonance/internal/volume` (`TestCreateBranchFromRunningParent`는 SKIP)

- [ ] **Step 6: VM에서 통과 확인**

Run: `hack/vm-test.sh -race -v ./internal/volume/...`
Expected: `TestCreateBranchFromRunningParent`, `TestCopyRefusesParentThatRestarted`, `TestFailedCopyEndsBackupAndCleansUp`, `TestStartSegment`를 포함해 모두 PASS, 경쟁 상태 경고 없음

- [ ] **Step 7: 커밋**

```bash
git add internal/volume/
git commit -m "feat(volume): 실행 중인 부모를 백업 모드로 복사해 브랜치 데이터 생성" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: 복구 지점

**Files:**
- Create: `internal/volume/restorepoints.go`
- Modify: `internal/volume/branches.go` (`CreateBranchData`를 바꿔 복구 지점 출처를 받는다)
- Test: `internal/volume/restorepoints_test.go` (패키지 `volume_test`)

**Interfaces:**
- Consumes: `snapshot`, `makeFirstGeneration`, `skipMarker` (Task 8), `loadCatalog`, `saveCatalog`, `newRestorePointID`, `expired`, `catalogEntry` (Task 5), 휴지통·잠금·간격 도우미 (Task 6), `newPGEnv` 등 테스트 도우미 (Task 6, 8)
- Produces: `(*Volume).CreateRestorePoint(ctx, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error)`, `(*Volume).ListRestorePoints(branch string) ([]agentapi.RestorePoint, error)`(최신순), `(*Volume).DeleteRestorePoint(ctx, branch, id string) error`, `(*Volume).ApplyRetention(ctx) error`, `(*Volume).RunJanitor(ctx, retentionEvery time.Duration)`(`retentionEvery` > 0); `CreateBranchData`가 `SourceRestorePoint`(부모의 복구 지점, 복사 간격 제한 없음)를 받는다; 패키지 내부 `addRestorePoint(cat catalog, rp agentapi.RestorePoint, dir string) error`, `unusedRestorePointID(cat catalog, branch string, now time.Time) string`, `retain(cat *catalog, branch string) []string`, `findRestorePoint(branch, id string) (agentapi.RestorePoint, string, error)`, 상수 `maxLabelLen = 256`.

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/volume/restorepoints_test.go`:

```go
package volume_test

import (
	"context"
	"errors"
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

func fromRestorePoint(parent, id string) agentapi.CreateBranchDataRequest {
	return agentapi.CreateBranchDataRequest{
		Parent: parent,
		Source: agentapi.Source{Type: agentapi.SourceRestorePoint, RestorePointID: id},
	}
}

func TestRestorePointsAndBranchesFromThem(t *testing.T) {
	e := newPGEnv(t, func(c *volume.Config) { c.MinCopyInterval = time.Hour })
	ctx := testCtx(t)
	e.exec(t, "main", "CREATE TABLE t (id int); INSERT INTO t SELECT generate_series(1, 1000)")

	rp, err := e.vol.CreateRestorePoint(ctx, "main", agentapi.CreateRestorePointRequest{Label: "before cleanup"})
	if err != nil {
		t.Fatalf("CreateRestorePoint: %v", err)
	}
	if !agentapi.ValidRestorePointID(rp.ID) || rp.Branch != "main" || rp.Reason != agentapi.ReasonAPI ||
		rp.Label != "before cleanup" || rp.BackupStartLSN == "" {
		t.Fatalf("restore point %+v", rp)
	}
	dir := filepath.Join(e.root, "restore", "main", rp.ID)
	if !exists(filepath.Join(dir, "backup_label")) || exists(filepath.Join(dir, ".incomplete")) {
		t.Fatal("the restore point is not a finished backup-mode copy")
	}
	if list, err := e.vol.ListRestorePoints("main"); err != nil || len(list) != 1 || list[0].ID != rp.ID {
		t.Fatalf("ListRestorePoints = %+v, %v", list, err)
	}

	e.exec(t, "main", "DELETE FROM t")
	// Copies of main are limited for an hour now; a restore point does not touch main's server.
	got, err := e.vol.CreateBranchData(ctx, "restored", fromRestorePoint("main", rp.ID))
	if err != nil {
		t.Fatalf("branch from the restore point: %v", err)
	}
	if got.Generation != 1 || got.BackupStartLSN != rp.BackupStartLSN {
		t.Fatalf("got %+v", got)
	}
	e.start(t, "restored")
	if n := e.count(t, "restored"); n != 1000 {
		t.Fatalf("restored has %d rows, want 1000", n)
	}
	var rl *volume.RateLimitError
	if _, err := e.vol.CreateRestorePoint(ctx, "main", agentapi.CreateRestorePointRequest{}); !errors.As(err, &rl) {
		t.Fatalf("second restore point within the interval = %v, want a rate limit", err)
	}

	if err := e.vol.DeleteRestorePoint(ctx, "main", rp.ID); err != nil {
		t.Fatalf("DeleteRestorePoint: %v", err)
	}
	if exists(dir) {
		t.Fatal("the restore point's directory is still there")
	}
	if list, err := e.vol.ListRestorePoints("main"); err != nil || len(list) != 0 {
		t.Fatalf("after delete: %+v, %v", list, err)
	}
	if err := e.vol.DeleteRestorePoint(ctx, "main", rp.ID); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestCreateRestorePointValidates(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{running("data-1")}}}, nil)
	ctx := testCtx(t)
	for _, tc := range []struct {
		branch string
		req    agentapi.CreateRestorePointRequest
		want   error
	}{
		{"main", agentapi.CreateRestorePointRequest{Reason: agentapi.ReasonReset}, volume.ErrInvalid},
		{"main", agentapi.CreateRestorePointRequest{Label: strings.Repeat("x", 257)}, volume.ErrInvalid},
		{"Main", agentapi.CreateRestorePointRequest{}, volume.ErrInvalid},
		{"dev", agentapi.CreateRestorePointRequest{}, volume.ErrNotFound},
	} {
		if _, err := v.CreateRestorePoint(ctx, tc.branch, tc.req); !errors.Is(err, tc.want) {
			t.Errorf("CreateRestorePoint(%q, %+v) = %v, want %v", tc.branch, tc.req, err, tc.want)
		}
	}
}

func TestCreateRestorePointNeedsRunningBranch(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	v := openVolume(t, root, supervisors{"main": {statuses: []supervisorapi.Status{stopped("data-1")}}}, nil)
	if _, err := v.CreateRestorePoint(testCtx(t), "main", agentapi.CreateRestorePointRequest{}); !errors.Is(err, volume.ErrNotRunning) {
		t.Fatalf("got %v, want ErrNotRunning", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "restore", "main")); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
}

func TestListRestorePointsNewestFirst(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	t0 := newClock().Now()
	writeCatalog(t, root,
		rpFixture{id: "rp-20261007T031500Z-aaaa", branch: "main", reason: "api", createdAt: t0},
		rpFixture{id: "rp-20261007T041500Z-bbbb", branch: "main", reason: "reset", createdAt: t0.Add(time.Hour)},
		rpFixture{id: "rp-20261007T051500Z-cccc", branch: "dev", reason: "api", createdAt: t0.Add(2 * time.Hour)},
	)
	v := openVolume(t, root, nil, nil)
	list, err := v.ListRestorePoints("main")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, rp := range list {
		ids = append(ids, rp.ID)
	}
	if !slices.Equal(ids, []string{"rp-20261007T041500Z-bbbb", "rp-20261007T031500Z-aaaa"}) {
		t.Fatalf("ids = %v", ids)
	}
	if list[0].Reason != agentapi.ReasonReset || !list[0].CreatedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("newest = %+v", list[0])
	}
	if _, err := v.ListRestorePoints("gone"); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("unknown branch: %v, want ErrNotFound", err)
	}
}

func TestApplyRetention(t *testing.T) {
	root := t.TempDir()
	clock := newClock()
	now := clock.Now()
	writeCatalog(t, root,
		rpFixture{id: "rp-20261007T031400Z-new1", branch: "main", reason: "api", createdAt: now.Add(-time.Minute)},
		rpFixture{id: "rp-20261007T031300Z-new2", branch: "main", reason: "api", createdAt: now.Add(-2 * time.Minute)},
		rpFixture{id: "rp-20261007T031200Z-new3", branch: "main", reason: "reset", createdAt: now.Add(-3 * time.Minute)},
		rpFixture{id: "rp-20261006T021500Z-old1", branch: "dev", reason: "api", createdAt: now.Add(-25 * time.Hour)},
		rpFixture{id: "rp-20261007T020000Z-dev2", branch: "dev", reason: "api", createdAt: now.Add(-75 * time.Minute)},
	)
	v := openVolume(t, root, nil, func(c *volume.Config) {
		c.Now = clock.Now
		c.RestorePointsPerBranch = 2
		c.RestorePointMaxAge = 24 * time.Hour
	})
	if err := v.ApplyRetention(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"restore/main/rp-20261007T031200Z-new3", "restore/dev/rp-20261006T021500Z-old1"} {
		if exists(filepath.Join(root, p)) {
			t.Errorf("%s survived the retention", p)
		}
	}
	for _, p := range []string{"restore/main/rp-20261007T031400Z-new1", "restore/main/rp-20261007T031300Z-new2", "restore/dev/rp-20261007T020000Z-dev2"} {
		if !exists(filepath.Join(root, p)) {
			t.Errorf("%s was removed", p)
		}
	}
	if cat := readFile(t, filepath.Join(root, ".consonance", "catalog.json")); strings.Contains(cat, "new3") || strings.Contains(cat, "old1") {
		t.Errorf("the catalog still lists removed restore points:\n%s", cat)
	}
}

func TestCreateBranchFromRestorePointIsNotRateLimited(t *testing.T) {
	// No XFS here: the clone fails, which shows that the copy got past the interval check.
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	writeCatalog(t, root, rpFixture{id: "rp-20261007T031500Z-ab12", branch: "main", reason: "api", createdAt: newClock().Now()})
	v := openVolume(t, root, nil, func(c *volume.Config) { c.MinCopyInterval = time.Hour })
	v.CopyEndedForTest("main")
	_, err := v.CreateBranchData(testCtx(t), "dev", fromRestorePoint("main", "rp-20261007T031500Z-ab12"))
	if !errors.Is(err, volume.ErrReflinkUnsupported) {
		t.Fatalf("got %v, want the clone to be tried (ErrReflinkUnsupported off XFS)", err)
	}
	if exists(filepath.Join(root, "branches", "dev")) {
		t.Fatal("a partial branch was left behind")
	}
}

func TestCreateBranchFromUnknownRestorePoint(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	mkGeneration(t, root, "dev", 1, true)
	writeCatalog(t, root, rpFixture{id: "rp-20261007T031500Z-ab12", branch: "dev", reason: "api", createdAt: newClock().Now()})
	v := openVolume(t, root, nil, nil)
	for _, id := range []string{"rp-20261007T031500Z-zzzz", "rp-20261007T031500Z-ab12"} { // unknown; dev's, not main's
		if _, err := v.CreateBranchData(testCtx(t), "x", fromRestorePoint("main", id)); !errors.Is(err, volume.ErrNotFound) {
			t.Errorf("restore point %s: %v, want ErrNotFound", id, err)
		}
	}
}

func TestRunJanitorEmptiesTheTrash(t *testing.T) {
	root := t.TempDir()
	mkGeneration(t, root, "main", 1, true)
	writeCatalog(t, root, rpFixture{id: "rp-20261007T031500Z-ab12", branch: "main", reason: "api", createdAt: newClock().Now()})
	v := openVolume(t, root, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.RunJanitor(ctx, time.Hour)
	}()
	defer func() {
		cancel()
		<-done
	}()
	if err := v.DeleteRestorePoint(testCtx(t), "main", "rp-20261007T031500Z-ab12"); err != nil {
		t.Fatal(err)
	}
	trash := filepath.Join(root, ".consonance", "trash")
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(trash)
		if err == nil && len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the janitor did not empty the trash: %v, %v", entries, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/volume/`
Expected: FAIL — `v.CreateRestorePoint undefined` 등

- [ ] **Step 3: 구현**

`internal/volume/restorepoints.go`:

```go
package volume

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"consonance/internal/agentapi"
)

// maxLabelLen bounds a restore point label.
const maxLabelLen = 256

// CreateRestorePoint copies the branch's running database into restore/<branch>/<id> the way a
// branch is created, adds it to the catalog and applies the branch's retention (spec 7.2, 9.3).
func (v *Volume) CreateRestorePoint(ctx context.Context, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error) {
	if !agentapi.ValidBranchName(branch) {
		return agentapi.RestorePoint{}, fmt.Errorf("%w: branch name %q", ErrInvalid, branch)
	}
	if req.Reason != "" && req.Reason != agentapi.ReasonAPI {
		return agentapi.RestorePoint{}, fmt.Errorf("%w: reason %q: only %q can be requested", ErrInvalid, req.Reason, agentapi.ReasonAPI)
	}
	if len(req.Label) > maxLabelLen {
		return agentapi.RestorePoint{}, fmt.Errorf("%w: label longer than %d bytes", ErrInvalid, maxLabelLen)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.RestorePoint{}, err
	}
	defer release()

	if _, err := v.currentGeneration(branch); err != nil {
		return agentapi.RestorePoint{}, err
	}
	if err := v.checkCopyInterval(branch); err != nil {
		return agentapi.RestorePoint{}, err
	}
	if err := v.checkSpace(); err != nil {
		return agentapi.RestorePoint{}, err
	}
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return agentapi.RestorePoint{}, err
	}
	now := v.cfg.Now()
	id := v.unusedRestorePointID(cat, branch, now)
	if err := os.MkdirAll(v.restoreBranchDir(branch), 0o700); err != nil {
		return agentapi.RestorePoint{}, err
	}
	dir := v.restorePointDir(branch, id)
	if err := makeMarked(dir); err != nil {
		return agentapi.RestorePoint{}, err
	}
	lsn, err := v.snapshot(ctx, branch, dir, "consonance restore point "+id)
	if err == nil {
		err = finishMarked(dir)
	}
	if err != nil {
		v.trashOrWarn(dir)
		return agentapi.RestorePoint{}, err
	}
	rp := agentapi.RestorePoint{
		ID: id, Branch: branch, CreatedAt: now.UTC().Truncate(time.Second),
		Reason: agentapi.ReasonAPI, Label: req.Label, BackupStartLSN: lsn,
	}
	// The catalog entry is the commit point: Recover removes a directory the catalog lacks.
	if err := v.addRestorePoint(cat, rp, dir); err != nil {
		v.trashOrWarn(dir)
		return agentapi.RestorePoint{}, err
	}
	return rp, nil
}

// addRestorePoint adds rp, stored in dir, to cat, applies the branch's retention, saves the
// catalog and then trashes the directories of the restore points that expired.
func (v *Volume) addRestorePoint(cat catalog, rp agentapi.RestorePoint, dir string) error {
	cat.RestorePoints = append(cat.RestorePoints, catalogEntry{RestorePoint: rp, Path: v.rel(dir)})
	gone := v.retain(&cat, rp.Branch)
	if err := saveCatalog(v.catalogPath(), cat); err != nil {
		return err
	}
	for _, d := range gone {
		v.trashOrWarn(d)
	}
	return nil
}

// unusedRestorePointID returns a new restore point ID that no catalog entry and no directory uses.
func (v *Volume) unusedRestorePointID(cat catalog, branch string, now time.Time) string {
	for {
		id := newRestorePointID(now)
		taken := slices.ContainsFunc(cat.RestorePoints, func(e catalogEntry) bool { return e.ID == id })
		if _, err := os.Lstat(v.restorePointDir(branch, id)); !taken && errors.Is(err, fs.ErrNotExist) {
			return id
		}
	}
}

// retain drops the branch's restore points that the retention policy expires (spec 7.2) from
// cat and returns their directories.
func (v *Volume) retain(cat *catalog, branch string) []string {
	gone := expired(cat.RestorePoints, branch, v.cfg.Now(), v.cfg.RestorePointsPerBranch, v.cfg.RestorePointMaxAge)
	if len(gone) == 0 {
		return nil
	}
	var dirs []string
	for _, e := range gone {
		dirs = append(dirs, filepath.Join(v.root, filepath.FromSlash(e.Path)))
	}
	cat.RestorePoints = slices.DeleteFunc(cat.RestorePoints, func(e catalogEntry) bool {
		return slices.ContainsFunc(gone, func(g catalogEntry) bool { return g.ID == e.ID })
	})
	return dirs
}

// findRestorePoint returns one of the branch's restore points and its directory.
func (v *Volume) findRestorePoint(branch, id string) (agentapi.RestorePoint, string, error) {
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return agentapi.RestorePoint{}, "", err
	}
	for _, e := range cat.RestorePoints {
		if e.ID == id && e.Branch == branch {
			return e.RestorePoint, filepath.Join(v.root, filepath.FromSlash(e.Path)), nil
		}
	}
	return agentapi.RestorePoint{}, "", fmt.Errorf("%w: restore point %q of branch %q", ErrNotFound, id, branch)
}

// ListRestorePoints returns the branch's restore points, newest first.
func (v *Volume) ListRestorePoints(branch string) ([]agentapi.RestorePoint, error) {
	if !agentapi.ValidBranchName(branch) {
		return nil, fmt.Errorf("%w: branch name %q", ErrInvalid, branch)
	}
	if _, err := os.Stat(v.branchDir(branch)); err != nil {
		return nil, fmt.Errorf("%w: branch %q", ErrNotFound, branch)
	}
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return nil, err
	}
	out := []agentapi.RestorePoint{}
	for _, e := range cat.RestorePoints {
		if e.Branch == branch {
			out = append(out, e.RestorePoint)
		}
	}
	slices.SortFunc(out, func(a, b agentapi.RestorePoint) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	return out, nil
}

// DeleteRestorePoint removes one of the branch's restore points.
func (v *Volume) DeleteRestorePoint(ctx context.Context, branch, id string) error {
	if !agentapi.ValidBranchName(branch) || !agentapi.ValidRestorePointID(id) {
		return fmt.Errorf("%w: branch %q, restore point %q", ErrInvalid, branch, id)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return err
	}
	i := slices.IndexFunc(cat.RestorePoints, func(e catalogEntry) bool { return e.ID == id && e.Branch == branch })
	if i < 0 {
		return fmt.Errorf("%w: restore point %q of branch %q", ErrNotFound, id, branch)
	}
	dir := filepath.Join(v.root, filepath.FromSlash(cat.RestorePoints[i].Path))
	cat.RestorePoints = slices.Delete(cat.RestorePoints, i, i+1)
	if err := saveCatalog(v.catalogPath(), cat); err != nil {
		return err
	}
	return v.trash(dir)
}

// ApplyRetention applies the retention policy to every branch's restore points (spec 7.2: hourly).
func (v *Volume) ApplyRetention(ctx context.Context) error {
	release, err := v.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return err
	}
	var branches []string
	for _, e := range cat.RestorePoints {
		if !slices.Contains(branches, e.Branch) {
			branches = append(branches, e.Branch)
		}
	}
	var dirs []string
	for _, b := range branches {
		dirs = append(dirs, v.retain(&cat, b)...)
	}
	if len(dirs) == 0 {
		return nil
	}
	if err := saveCatalog(v.catalogPath(), cat); err != nil {
		return err
	}
	var errs []error
	for _, d := range dirs {
		if err := v.trash(d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RunJanitor empties the trash whenever something is trashed and applies the restore point
// retention every retentionEvery (spec 7.2: hourly), until ctx ends. retentionEvery must be
// positive.
func (v *Volume) RunJanitor(ctx context.Context, retentionEvery time.Duration) {
	if err := v.EmptyTrash(); err != nil {
		slog.Warn("empty trash", "err", err)
	}
	tick := time.NewTicker(retentionEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-v.trashed:
			if err := v.EmptyTrash(); err != nil {
				slog.Warn("empty trash", "err", err)
			}
		case <-tick.C:
			if err := v.ApplyRetention(ctx); err != nil && !errors.Is(err, ErrBusy) {
				slog.Warn("restore point retention", "err", err)
			}
		}
	}
}
```

`internal/volume/branches.go`의 `CreateBranchData` 함수 전체를 다음으로 바꾼다(`makeFirstGeneration`은 그대로 둔다):

```go
// CreateBranchData makes the data of a new branch (spec 7.2 POST /v1/branches/{name}/data,
// 9.2): generation data-1 holding a copy of the parent's running database (ParentLatest) or of
// one of the parent's restore points, with current pointing at it. When the branch already has
// data it changes nothing and returns the current generation, so the operator can retry.
func (v *Volume) CreateBranchData(ctx context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error) {
	if !agentapi.ValidBranchName(name) || name == agentapi.MainBranch {
		return agentapi.BranchData{}, fmt.Errorf("%w: branch name %q", ErrInvalid, name)
	}
	if !agentapi.ValidBranchName(req.Parent) || req.Parent == name {
		return agentapi.BranchData{}, fmt.Errorf("%w: parent %q", ErrInvalid, req.Parent)
	}
	switch req.Source.Type {
	case agentapi.SourceParentLatest:
	case agentapi.SourceRestorePoint:
		if !agentapi.ValidRestorePointID(req.Source.RestorePointID) {
			return agentapi.BranchData{}, fmt.Errorf("%w: restore point %q", ErrInvalid, req.Source.RestorePointID)
		}
	default:
		return agentapi.BranchData{}, fmt.Errorf("%w: source type %q", ErrInvalid, req.Source.Type)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.BranchData{}, err
	}
	defer release()

	if n, err := v.currentGeneration(name); err == nil {
		return agentapi.BranchData{Generation: n}, nil
	}
	var fill func(dst string) (string, error)
	if req.Source.Type == agentapi.SourceRestorePoint {
		// A restore point is copied without touching the parent's server: no interval applies.
		rp, dir, err := v.findRestorePoint(req.Parent, req.Source.RestorePointID)
		if err != nil {
			return agentapi.BranchData{}, err
		}
		fill = func(dst string) (string, error) { return rp.BackupStartLSN, CloneTree(dir, dst, skipMarker) }
	} else {
		if _, err := v.currentGeneration(req.Parent); err != nil {
			return agentapi.BranchData{}, fmt.Errorf("%w: parent %q has no data", ErrNotFound, req.Parent)
		}
		if err := v.checkCopyInterval(req.Parent); err != nil {
			return agentapi.BranchData{}, err
		}
		fill = func(dst string) (string, error) {
			return v.snapshot(ctx, req.Parent, dst, "consonance branch "+name)
		}
	}
	if err := v.checkSpace(); err != nil {
		return agentapi.BranchData{}, err
	}
	lsn, err := v.makeFirstGeneration(name, fill)
	if err != nil {
		return agentapi.BranchData{}, err
	}
	return agentapi.BranchData{Generation: 1, BackupStartLSN: lsn}, nil
}
```

- [ ] **Step 4: Mac과 VM에서 통과 확인**

Run: `go vet ./... && go test -race ./internal/volume/ && hack/vm-test.sh -race -v ./internal/volume/...`
Expected: Mac에서 `ok`, VM에서 `TestRestorePointsAndBranchesFromThem`을 포함해 모두 PASS(SKIP 없음)

- [ ] **Step 5: 커밋**

```bash
git add internal/volume/
git commit -m "feat(volume): 복구 지점 생성·목록·삭제·보존과 복구 지점에서 브랜치 생성" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 10: 세대(롤백·리셋)와 브랜치 데이터 삭제

**Files:**
- Create: `internal/volume/generations.go`
- Modify: `internal/volume/branches.go` (import 교체, `DeleteBranchData` 추가)
- Test: `internal/volume/generations_test.go` (패키지 `volume_test`)

**Interfaces:**
- Consumes: `snapshot`, `status`, `skipMarker` (Task 8), `findRestorePoint`, `addRestorePoint`, `unusedRestorePointID` (Task 9), 레이아웃·잠금·휴지통 도우미 (Task 4, 6)
- Produces: `(*Volume).ListGenerations(branch string) (agentapi.Generations, error)`, `(*Volume).CreateGeneration(ctx, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error)`, `(*Volume).ActivateGeneration(ctx, branch string, n int) (agentapi.Generations, error)`, `(*Volume).DetachGeneration(ctx, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error)`, `(*Volume).DeleteGeneration(ctx, branch string, n int) error`, `(*Volume).DeleteBranchData(ctx, name string) error`. 이 작업으로 `*Volume`이 Task 11의 `agent.Ops`를 모두 갖춘다.

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/volume/generations_test.go`:

```go
package volume_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/volume/`
Expected: FAIL — `v.ListGenerations undefined` 등

- [ ] **Step 3: 구현**

`internal/volume/generations.go`:

```go
package volume

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

// ListGenerations returns the branch's current generation and its complete generations.
func (v *Volume) ListGenerations(branch string) (agentapi.Generations, error) {
	if !agentapi.ValidBranchName(branch) {
		return agentapi.Generations{}, fmt.Errorf("%w: branch name %q", ErrInvalid, branch)
	}
	cur, err := v.currentGeneration(branch)
	if err != nil {
		return agentapi.Generations{}, err
	}
	gens, err := v.generations(branch)
	if err != nil {
		return agentapi.Generations{}, err
	}
	return agentapi.Generations{Current: cur, Generations: gens}, nil
}

// CreateGeneration makes data-N+1 without activating it (spec 7.2, 9.4 Preparing): for a
// rollback a copy of one of the branch's restore points, for a reset a backup-mode copy of the
// parent's running database.
func (v *Volume) CreateGeneration(ctx context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error) {
	if !agentapi.ValidBranchName(branch) {
		return agentapi.Generation{}, fmt.Errorf("%w: branch name %q", ErrInvalid, branch)
	}
	switch req.Source.Type {
	case agentapi.SourceRestorePoint:
		if !agentapi.ValidRestorePointID(req.Source.RestorePointID) {
			return agentapi.Generation{}, fmt.Errorf("%w: restore point %q", ErrInvalid, req.Source.RestorePointID)
		}
	case agentapi.SourceParentLatest:
		if !agentapi.ValidBranchName(req.Parent) || req.Parent == branch {
			return agentapi.Generation{}, fmt.Errorf("%w: parent %q", ErrInvalid, req.Parent)
		}
	default:
		return agentapi.Generation{}, fmt.Errorf("%w: source type %q", ErrInvalid, req.Source.Type)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.Generation{}, err
	}
	defer release()

	if _, err := v.currentGeneration(branch); err != nil {
		return agentapi.Generation{}, err
	}
	var fill func(dst string) error
	if req.Source.Type == agentapi.SourceRestorePoint {
		_, dir, err := v.findRestorePoint(branch, req.Source.RestorePointID)
		if err != nil {
			return agentapi.Generation{}, err
		}
		fill = func(dst string) error { return CloneTree(dir, dst, skipMarker) }
	} else {
		if _, err := v.currentGeneration(req.Parent); err != nil {
			return agentapi.Generation{}, fmt.Errorf("%w: parent %q has no data", ErrNotFound, req.Parent)
		}
		if err := v.checkCopyInterval(req.Parent); err != nil {
			return agentapi.Generation{}, err
		}
		fill = func(dst string) error {
			_, err := v.snapshot(ctx, req.Parent, dst, "consonance reset "+branch)
			return err
		}
	}
	if err := v.checkSpace(); err != nil {
		return agentapi.Generation{}, err
	}
	n, err := v.nextGeneration(branch)
	if err != nil {
		return agentapi.Generation{}, err
	}
	dir := v.genDir(branch, n)
	if err := makeMarked(dir); err != nil {
		return agentapi.Generation{}, err
	}
	err = fill(dir)
	if err == nil {
		err = finishMarked(dir)
	}
	if err != nil {
		v.trashOrWarn(dir)
		return agentapi.Generation{}, err
	}
	return agentapi.Generation{Generation: n}, nil
}

// ActivateGeneration points the branch's current link at data-n (spec 7.2, 9.4 Activating). The
// branch's PostgreSQL must be stopped, which its supervisor is asked to confirm.
func (v *Volume) ActivateGeneration(ctx context.Context, branch string, n int) (agentapi.Generations, error) {
	if !agentapi.ValidBranchName(branch) || n < 1 {
		return agentapi.Generations{}, fmt.Errorf("%w: branch %q, generation %d", ErrInvalid, branch, n)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.Generations{}, err
	}
	defer release()

	cur, err := v.currentGeneration(branch)
	if err != nil {
		return agentapi.Generations{}, err
	}
	if !complete(v.genDir(branch, n)) {
		return agentapi.Generations{}, fmt.Errorf("%w: generation %d of branch %q", ErrNotFound, n, branch)
	}
	if cur != n {
		st, err := v.status(ctx, branch)
		if err != nil {
			return agentapi.Generations{}, err
		}
		if st.PG != supervisorapi.PGStopped {
			return agentapi.Generations{}, fmt.Errorf("%w: branch %q is %s; stop it before switching generations", ErrConflict, branch, st.PG)
		}
		if err := switchCurrent(v.branchDir(branch), n); err != nil {
			return agentapi.Generations{}, err
		}
	}
	gens, err := v.generations(branch)
	if err != nil {
		return agentapi.Generations{}, err
	}
	return agentapi.Generations{Current: n, Generations: gens}, nil
}

// DetachGeneration moves data-n out of the branch (spec 7.2, 9.4 Detaching): into
// branches/<toBranch>/data-1 as a new branch (a rollback's backup branch) or into a restore
// point with reason "reset". data-n must not be current, and the branch's PostgreSQL must not
// be running it. A retried detach into a branch that was completed returns that branch.
func (v *Volume) DetachGeneration(ctx context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error) {
	if !agentapi.ValidBranchName(branch) || n < 1 {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: branch %q, generation %d", ErrInvalid, branch, n)
	}
	if (req.ToBranch != "") == req.AsRestorePoint {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: give exactly one of toBranch and asRestorePoint", ErrInvalid)
	}
	if req.ToBranch != "" && (!agentapi.ValidBranchName(req.ToBranch) || req.ToBranch == branch || req.ToBranch == agentapi.MainBranch) {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: target branch %q", ErrInvalid, req.ToBranch)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.DetachResponse{}, err
	}
	defer release()

	src := v.genDir(branch, n)
	if req.ToBranch != "" {
		if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
			if _, err := v.currentGeneration(req.ToBranch); err == nil {
				return agentapi.DetachResponse{Branch: req.ToBranch}, nil // done by an earlier call
			}
		}
	}
	if !complete(src) {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: generation %d of branch %q", ErrNotFound, n, branch)
	}
	cur, err := v.currentGeneration(branch)
	if err != nil {
		return agentapi.DetachResponse{}, err
	}
	if cur == n {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: generation %d is branch %q's current one", ErrConflict, n, branch)
	}
	if err := v.checkNotRunning(ctx, branch, n); err != nil {
		return agentapi.DetachResponse{}, err
	}
	if req.ToBranch != "" {
		return v.detachToBranch(branch, n, req.ToBranch)
	}
	return v.detachToRestorePoint(branch, n)
}

// checkNotRunning fails when the branch's PostgreSQL runs data-n. A server started before
// current was switched still runs the old generation.
func (v *Volume) checkNotRunning(ctx context.Context, branch string, n int) error {
	st, err := v.status(ctx, branch)
	if err != nil {
		return err
	}
	if st.PG != supervisorapi.PGStopped && st.Generation == generationName(n) {
		return fmt.Errorf("%w: branch %q is %s on generation %d", ErrConflict, branch, st.PG, n)
	}
	return nil
}

func (v *Volume) detachToBranch(branch string, n int, to string) (agentapi.DetachResponse, error) {
	if _, err := v.currentGeneration(to); err == nil {
		return agentapi.DetachResponse{}, fmt.Errorf("%w: branch %q exists", ErrConflict, to)
	}
	td := v.branchDir(to)
	if err := v.trash(td); err != nil { // half made by an earlier call
		return agentapi.DetachResponse{}, err
	}
	if err := os.Mkdir(td, 0o700); err != nil {
		return agentapi.DetachResponse{}, err
	}
	// Link first, then move: if the agent stops in between, Recover removes the branch with the
	// dangling link and the generation is still in its old place. The other order would leave
	// the data in a branch without a link, which Recover deletes.
	if err := switchCurrent(td, 1); err != nil {
		v.trashOrWarn(td)
		return agentapi.DetachResponse{}, err
	}
	if err := os.Rename(v.genDir(branch, n), v.genDir(to, 1)); err != nil {
		v.trashOrWarn(td)
		return agentapi.DetachResponse{}, err
	}
	if err := syncDir(td); err != nil {
		return agentapi.DetachResponse{}, err
	}
	if err := syncDir(v.branchDir(branch)); err != nil {
		return agentapi.DetachResponse{}, err
	}
	return agentapi.DetachResponse{Branch: to}, nil
}

func (v *Volume) detachToRestorePoint(branch string, n int) (agentapi.DetachResponse, error) {
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return agentapi.DetachResponse{}, err
	}
	now := v.cfg.Now()
	id := v.unusedRestorePointID(cat, branch, now)
	if err := os.MkdirAll(v.restoreBranchDir(branch), 0o700); err != nil {
		return agentapi.DetachResponse{}, err
	}
	dir := v.restorePointDir(branch, id)
	if err := os.Rename(v.genDir(branch, n), dir); err != nil {
		return agentapi.DetachResponse{}, err
	}
	if err := syncDir(v.restoreBranchDir(branch)); err != nil {
		return agentapi.DetachResponse{}, err
	}
	if err := syncDir(v.branchDir(branch)); err != nil {
		return agentapi.DetachResponse{}, err
	}
	rp := agentapi.RestorePoint{
		ID: id, Branch: branch, CreatedAt: now.UTC().Truncate(time.Second),
		Reason: agentapi.ReasonReset, Label: "before reset (" + generationName(n) + ")",
	}
	if err := v.addRestorePoint(cat, rp, dir); err != nil {
		// Put the generation back rather than leave it for Recover to delete.
		if rerr := os.Rename(dir, v.genDir(branch, n)); rerr != nil {
			slog.Warn("put back a detached generation", "branch", branch, "generation", n, "err", rerr)
		}
		return agentapi.DetachResponse{}, err
	}
	return agentapi.DetachResponse{RestorePoint: &rp}, nil
}

// DeleteGeneration removes data-n, for instance the new generation of a rollback whose start
// failed. It must be neither current nor running.
func (v *Volume) DeleteGeneration(ctx context.Context, branch string, n int) error {
	if !agentapi.ValidBranchName(branch) || n < 1 {
		return fmt.Errorf("%w: branch %q, generation %d", ErrInvalid, branch, n)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	if _, err := os.Lstat(v.genDir(branch, n)); err != nil {
		return fmt.Errorf("%w: generation %d of branch %q", ErrNotFound, n, branch)
	}
	cur, err := v.currentGeneration(branch)
	if err != nil {
		return err
	}
	if cur == n {
		return fmt.Errorf("%w: generation %d is branch %q's current one", ErrConflict, n, branch)
	}
	if err := v.checkNotRunning(ctx, branch, n); err != nil {
		return err
	}
	return v.trash(v.genDir(branch, n))
}
```

`internal/volume/branches.go`의 import 블록을 다음으로 바꾼다:

```go
import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)
```

같은 파일 끝에 더한다:

```go
// DeleteBranchData removes a branch's directory and its restore points (spec 7.2, 9.6). The
// operator deletes the branch's pod first; if its supervisor still answers, PostgreSQL must be
// stopped. main cannot be deleted. A branch without data is deleted already: success.
func (v *Volume) DeleteBranchData(ctx context.Context, name string) error {
	if !agentapi.ValidBranchName(name) {
		return fmt.Errorf("%w: branch name %q", ErrInvalid, name)
	}
	if name == agentapi.MainBranch {
		return fmt.Errorf("%w: main cannot be deleted", ErrConflict)
	}
	release, err := v.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	st, err := v.status(ctx, name)
	switch {
	case errors.Is(err, ErrSupervisorUnavailable):
		// No answer: the pod is gone, as it should be by now.
	case err != nil:
		return err
	case st.PG != supervisorapi.PGStopped:
		return fmt.Errorf("%w: branch %q is %s", ErrConflict, name, st.PG)
	}
	cat, err := loadCatalog(v.catalogPath())
	if err != nil {
		return err
	}
	kept := slices.DeleteFunc(slices.Clone(cat.RestorePoints), func(e catalogEntry) bool { return e.Branch == name })
	if len(kept) != len(cat.RestorePoints) {
		cat.RestorePoints = kept
		if err := saveCatalog(v.catalogPath(), cat); err != nil {
			return err
		}
	}
	// Catalog first: if the agent stops right after it, Recover removes the restore point
	// directories the catalog no longer lists, and the retried call removes the branch.
	if err := v.trash(v.restoreBranchDir(name)); err != nil {
		return err
	}
	if err := v.trash(v.branchDir(name)); err != nil {
		return err
	}
	v.mu.Lock()
	delete(v.lastCopy, name)
	v.mu.Unlock()
	return nil
}
```

- [ ] **Step 4: Mac과 VM에서 통과 확인**

Run: `go vet ./... && go test -race ./internal/volume/ && hack/vm-test.sh -race -v ./internal/volume/...`
Expected: Mac에서 `ok`, VM에서 `TestRollbackToRestorePoint`, `TestResetFromParent`, `TestDetachRefusesRunningGeneration`을 포함해 모두 PASS(SKIP 없음)

- [ ] **Step 5: 커밋**

```bash
git add internal/volume/
git commit -m "feat(volume): 세대 생성·전환·분리·삭제(롤백·리셋)와 브랜치 데이터 삭제" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 11: 에이전트 HTTP API

**Files:**
- Create: `internal/agent/server.go`, `internal/agent/agenttest/fakes.go`
- Test: `internal/agent/server_test.go`

**Interfaces:**
- Consumes: `agentapi` 전체 (Task 5), `volume`의 오류 값과 `RateLimitError` (Task 3)
- Produces: `type agent.Ops interface`(아래 13개 메서드, `*volume.Volume`이 구현), `type agent.Server struct { Token string; Ops Ops }`, `(*Server).Handler() http.Handler`; `agenttest.FakeOps`(결과 필드 `PreflightResult`, `UsageResult`, `GenerationResult`, `BranchDataResult`, `RestorePoint`, `RestorePoints`, `GenerationsResult`, `DetachResult`; 메서드 `SetErr(error)`, `Last() Call`), `agenttest.Call{Op, Branch, ID string; N int; Body any}`. Task 12와 계획 4의 테스트가 쓴다.

- [ ] **Step 1: 가짜 볼륨 쓰기**

`internal/agent/agenttest/fakes.go`:

```go
// Package agenttest provides a fake volume for tests of the agent API and its clients.
package agenttest

import (
	"context"
	"sync"

	"consonance/internal/agentapi"
)

// Call is one operation the fake received.
type Call struct {
	Op     string // the method name, e.g. "CreateBranchData"
	Branch string
	ID     string // restore point ID
	N      int    // generation
	Body   any    // the request value, for operations that take one
}

// FakeOps implements agent.Ops. Every operation records its call and returns the error set with
// SetErr, or else the matching result field. Set the result fields before serving.
type FakeOps struct {
	PreflightResult   agentapi.Preflight
	UsageResult       agentapi.Usage
	GenerationResult  agentapi.Generation
	BranchDataResult  agentapi.BranchData
	RestorePoint      agentapi.RestorePoint
	RestorePoints     []agentapi.RestorePoint
	GenerationsResult agentapi.Generations
	DetachResult      agentapi.DetachResponse

	mu    sync.Mutex
	err   error
	calls []Call
}

// SetErr makes every following operation fail with err (nil: succeed again).
func (f *FakeOps) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Last returns the most recent call, or a zero Call when there was none.
func (f *FakeOps) Last() Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return Call{}
	}
	return f.calls[len(f.calls)-1]
}

func (f *FakeOps) record(c Call) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.err
}

func (f *FakeOps) Preflight() agentapi.Preflight {
	_ = f.record(Call{Op: "Preflight"})
	return f.PreflightResult
}

func (f *FakeOps) Usage() (agentapi.Usage, error) {
	err := f.record(Call{Op: "Usage"})
	return f.UsageResult, err
}

func (f *FakeOps) InitMain(context.Context) (agentapi.Generation, error) {
	err := f.record(Call{Op: "InitMain"})
	return f.GenerationResult, err
}

func (f *FakeOps) CreateBranchData(_ context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error) {
	err := f.record(Call{Op: "CreateBranchData", Branch: name, Body: req})
	return f.BranchDataResult, err
}

func (f *FakeOps) DeleteBranchData(_ context.Context, name string) error {
	return f.record(Call{Op: "DeleteBranchData", Branch: name})
}

func (f *FakeOps) CreateRestorePoint(_ context.Context, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error) {
	err := f.record(Call{Op: "CreateRestorePoint", Branch: branch, Body: req})
	return f.RestorePoint, err
}

func (f *FakeOps) ListRestorePoints(branch string) ([]agentapi.RestorePoint, error) {
	err := f.record(Call{Op: "ListRestorePoints", Branch: branch})
	return f.RestorePoints, err
}

func (f *FakeOps) DeleteRestorePoint(_ context.Context, branch, id string) error {
	return f.record(Call{Op: "DeleteRestorePoint", Branch: branch, ID: id})
}

func (f *FakeOps) ListGenerations(branch string) (agentapi.Generations, error) {
	err := f.record(Call{Op: "ListGenerations", Branch: branch})
	return f.GenerationsResult, err
}

func (f *FakeOps) CreateGeneration(_ context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error) {
	err := f.record(Call{Op: "CreateGeneration", Branch: branch, Body: req})
	return f.GenerationResult, err
}

func (f *FakeOps) ActivateGeneration(_ context.Context, branch string, n int) (agentapi.Generations, error) {
	err := f.record(Call{Op: "ActivateGeneration", Branch: branch, N: n})
	return f.GenerationsResult, err
}

func (f *FakeOps) DetachGeneration(_ context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error) {
	err := f.record(Call{Op: "DetachGeneration", Branch: branch, N: n, Body: req})
	return f.DetachResult, err
}

func (f *FakeOps) DeleteGeneration(_ context.Context, branch string, n int) error {
	return f.record(Call{Op: "DeleteGeneration", Branch: branch, N: n})
}
```

- [ ] **Step 2: 실패하는 테스트 쓰기**

`internal/agent/server_test.go`:

```go
package agent_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"consonance/internal/agent"
	"consonance/internal/agent/agenttest"
	"consonance/internal/agentapi"
	"consonance/internal/volume"
)

const token = "test-token"

func newServer(t *testing.T, ops *agenttest.FakeOps, tok string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer((&agent.Server{Token: tok, Ops: ops}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, withToken bool) (*http.Response, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if withToken {
		req.Header.Set(agentapi.TokenHeader, token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

const rpID = "rp-20261007T031500Z-ab12"

// routes lists every /v1 route with a request that reaches the volume.
var routes = []struct {
	method, path, body string
	status             int
	want               agenttest.Call
}{
	{"GET", "/v1/preflight", "", 200, agenttest.Call{Op: "Preflight"}},
	{"GET", "/v1/usage", "", 200, agenttest.Call{Op: "Usage"}},
	{"POST", "/v1/init-main", "", 200, agenttest.Call{Op: "InitMain"}},
	{"POST", "/v1/branches/dev/data", `{"parent":"main","source":{"type":"ParentLatest"}}`, 200,
		agenttest.Call{Op: "CreateBranchData", Branch: "dev", Body: agentapi.CreateBranchDataRequest{
			Parent: "main", Source: agentapi.Source{Type: agentapi.SourceParentLatest}}}},
	{"DELETE", "/v1/branches/dev/data", "", 204, agenttest.Call{Op: "DeleteBranchData", Branch: "dev"}},
	{"POST", "/v1/branches/main/restore-points", `{"label":"before"}`, 200,
		agenttest.Call{Op: "CreateRestorePoint", Branch: "main", Body: agentapi.CreateRestorePointRequest{Label: "before"}}},
	{"GET", "/v1/branches/main/restore-points", "", 200, agenttest.Call{Op: "ListRestorePoints", Branch: "main"}},
	{"DELETE", "/v1/branches/main/restore-points/" + rpID, "", 204,
		agenttest.Call{Op: "DeleteRestorePoint", Branch: "main", ID: rpID}},
	{"GET", "/v1/branches/main/generations", "", 200, agenttest.Call{Op: "ListGenerations", Branch: "main"}},
	{"POST", "/v1/branches/main/generations", `{"source":{"type":"RestorePoint","restorePointId":"` + rpID + `"}}`, 200,
		agenttest.Call{Op: "CreateGeneration", Branch: "main", Body: agentapi.CreateGenerationRequest{
			Source: agentapi.Source{Type: agentapi.SourceRestorePoint, RestorePointID: rpID}}}},
	{"POST", "/v1/branches/main/generations/2/activate", "", 200,
		agenttest.Call{Op: "ActivateGeneration", Branch: "main", N: 2}},
	{"POST", "/v1/branches/main/generations/1/detach", `{"toBranch":"main-before-20261007031500"}`, 200,
		agenttest.Call{Op: "DetachGeneration", Branch: "main", N: 1, Body: agentapi.DetachRequest{ToBranch: "main-before-20261007031500"}}},
	{"DELETE", "/v1/branches/main/generations/3", "", 204, agenttest.Call{Op: "DeleteGeneration", Branch: "main", N: 3}},
}

func TestRoutesReachTheVolume(t *testing.T) {
	ops := &agenttest.FakeOps{}
	srv := newServer(t, ops, token)
	for _, rt := range routes {
		resp, body := call(t, srv, rt.method, rt.path, rt.body, true)
		if resp.StatusCode != rt.status {
			t.Errorf("%s %s = %d %s, want %d", rt.method, rt.path, resp.StatusCode, body, rt.status)
			continue
		}
		if got := ops.Last(); !reflect.DeepEqual(got, rt.want) {
			t.Errorf("%s %s reached %+v, want %+v", rt.method, rt.path, got, rt.want)
		}
	}
}

func TestEveryV1RouteNeedsTheToken(t *testing.T) {
	ops := &agenttest.FakeOps{}
	srv := newServer(t, ops, token)
	noToken := newServer(t, ops, "") // a server without a token lets nobody in
	for _, rt := range routes {
		resp, body := call(t, srv, rt.method, rt.path, rt.body, false)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, `"code":"unauthorized"`) {
			t.Errorf("%s %s without token = %d %s", rt.method, rt.path, resp.StatusCode, body)
		}
		if resp, _ := call(t, noToken, rt.method, rt.path, rt.body, true); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s on a server without token = %d", rt.method, rt.path, resp.StatusCode)
		}
	}
	if got := ops.Last(); got.Op != "" {
		t.Fatalf("an unauthorized request reached the volume: %+v", got)
	}
	if resp, _ := call(t, srv, "GET", "/healthz", "", false); resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 without a token", resp.StatusCode)
	}
}

func TestErrorsMapToStatusAndCode(t *testing.T) {
	ops := &agenttest.FakeOps{}
	srv := newServer(t, ops, token)
	for _, tc := range []struct {
		err    error
		status int
		code   agentapi.ErrorCode
	}{
		{fmt.Errorf("%w: branch name %q", volume.ErrInvalid, "Dev"), 400, agentapi.CodeInvalid},
		{fmt.Errorf("%w: branch %q", volume.ErrNotFound, "dev"), 404, agentapi.CodeNotFound},
		{fmt.Errorf("%w: branch %q is stopped", volume.ErrNotRunning, "main"), 409, agentapi.CodeNotRunning},
		{fmt.Errorf("%w (waited 2s)", volume.ErrBusy), 409, agentapi.CodeBusy},
		{fmt.Errorf("%w: branch %q exists", volume.ErrConflict, "x"), 409, agentapi.CodeConflict},
		{fmt.Errorf("%w: 91.0%% used", volume.ErrInsufficientStorage), 507, agentapi.CodeInsufficientStorage},
		{fmt.Errorf("%w: status of branch %q", volume.ErrSupervisorUnavailable, "main"), 502, agentapi.CodeSupervisorUnavailable},
		{&volume.RateLimitError{Source: "main", RetryAfter: 1200 * time.Millisecond}, 429, agentapi.CodeRateLimited},
		{errors.New("disk on fire"), 500, agentapi.CodeInternal},
	} {
		ops.SetErr(tc.err)
		resp, body := call(t, srv, "POST", "/v1/branches/dev/data", `{"parent":"main","source":{"type":"ParentLatest"}}`, true)
		var e agentapi.Error
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			t.Fatalf("%v: body %q: %v", tc.err, body, err)
		}
		if resp.StatusCode != tc.status || e.Code != tc.code || e.Error != tc.err.Error() {
			t.Errorf("%v → %d %+v, want %d %s", tc.err, resp.StatusCode, e, tc.status, tc.code)
		}
		if tc.status == 429 && resp.Header.Get("Retry-After") != "2" {
			t.Errorf("Retry-After = %q, want 2 (1.2s rounded up)", resp.Header.Get("Retry-After"))
		}
	}
}

func TestBadRequestsStopAtTheAPI(t *testing.T) {
	ops := &agenttest.FakeOps{}
	srv := newServer(t, ops, token)
	for _, rt := range []struct{ method, path, body string }{
		{"POST", "/v1/branches/dev/data", `{"parent":`},                  // broken JSON
		{"POST", "/v1/branches/dev/data", `{"parent":"main","extra":1}`}, // unknown field
		{"POST", "/v1/branches/main/generations/0/activate", ""},
		{"POST", "/v1/branches/main/generations/x/activate", ""},
		{"DELETE", "/v1/branches/main/generations/-1", ""},
		{"POST", "/v1/branches/main/restore-points", `{"label":"` + strings.Repeat("x", 70<<10) + `"}`}, // over 64 KiB
	} {
		resp, body := call(t, srv, rt.method, rt.path, rt.body, true)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"code":"invalid"`) {
			t.Errorf("%s %s = %d %s, want 400 invalid", rt.method, rt.path, resp.StatusCode, body)
		}
	}
	if got := ops.Last(); got.Op != "" {
		t.Fatalf("a bad request reached the volume: %+v", got)
	}
}

func TestEmptyBodyIsAnEmptyRequest(t *testing.T) {
	ops := &agenttest.FakeOps{}
	srv := newServer(t, ops, token)
	if resp, body := call(t, srv, "POST", "/v1/branches/main/restore-points", "", true); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST without a body = %d %s", resp.StatusCode, body)
	}
	if got := ops.Last(); got.Body != (agentapi.CreateRestorePointRequest{}) {
		t.Fatalf("the volume got %+v", got.Body)
	}
}

func TestRestorePointListIsNeverNull(t *testing.T) {
	ops := &agenttest.FakeOps{} // returns a nil slice
	srv := newServer(t, ops, token)
	_, body := call(t, srv, "GET", "/v1/branches/main/restore-points", "", true)
	if strings.TrimSpace(body) != `{"restorePoints":[]}` {
		t.Fatalf("body = %s", body)
	}
}
```

- [ ] **Step 3: 테스트가 실패하는지 확인**

Run: `go test ./internal/agent/...`
Expected: FAIL — `undefined: agent.Server`

- [ ] **Step 4: 구현**

`internal/agent/server.go`:

```go
// Package agent serves the volume agent's internal HTTP API (spec 7.2).
package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/volume"
)

// Ops is the volume work behind the API (implemented by *volume.Volume).
type Ops interface {
	Preflight() agentapi.Preflight
	Usage() (agentapi.Usage, error)
	InitMain(ctx context.Context) (agentapi.Generation, error)
	CreateBranchData(ctx context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error)
	DeleteBranchData(ctx context.Context, name string) error
	CreateRestorePoint(ctx context.Context, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error)
	ListRestorePoints(branch string) ([]agentapi.RestorePoint, error)
	DeleteRestorePoint(ctx context.Context, branch, id string) error
	ListGenerations(branch string) (agentapi.Generations, error)
	CreateGeneration(ctx context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error)
	ActivateGeneration(ctx context.Context, branch string, n int) (agentapi.Generations, error)
	DetachGeneration(ctx context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error)
	DeleteGeneration(ctx context.Context, branch string, n int) error
}

// Server serves the agent API. Every route but /healthz needs the token; an empty Token lets
// nobody in.
type Server struct {
	Token string
	Ops   Ops
}

// maxBody bounds request bodies.
const maxBody = 64 << 10

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("GET /v1/preflight", s.auth(s.preflight))
	mux.Handle("GET /v1/usage", s.auth(s.usage))
	mux.Handle("POST /v1/init-main", s.auth(s.initMain))
	mux.Handle("POST /v1/branches/{name}/data", s.auth(s.createBranchData))
	mux.Handle("DELETE /v1/branches/{name}/data", s.auth(s.deleteBranchData))
	mux.Handle("POST /v1/branches/{name}/restore-points", s.auth(s.createRestorePoint))
	mux.Handle("GET /v1/branches/{name}/restore-points", s.auth(s.listRestorePoints))
	mux.Handle("DELETE /v1/branches/{name}/restore-points/{id}", s.auth(s.deleteRestorePoint))
	mux.Handle("GET /v1/branches/{name}/generations", s.auth(s.listGenerations))
	mux.Handle("POST /v1/branches/{name}/generations", s.auth(s.createGeneration))
	mux.Handle("POST /v1/branches/{name}/generations/{n}/activate", s.auth(s.activateGeneration))
	mux.Handle("POST /v1/branches/{name}/generations/{n}/detach", s.auth(s.detachGeneration))
	mux.Handle("DELETE /v1/branches/{name}/generations/{n}", s.auth(s.deleteGeneration))
	return logRequests(mux)
}

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(agentapi.TokenHeader)
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, agentapi.CodeUnauthorized, "invalid token")
			return
		}
		next(w, r)
	})
}

func (s *Server) preflight(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Ops.Preflight())
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	u, err := s.Ops.Usage()
	reply(w, u, err)
}

func (s *Server) initMain(w http.ResponseWriter, r *http.Request) {
	g, err := s.Ops.InitMain(r.Context())
	reply(w, g, err)
}

func (s *Server) createBranchData(w http.ResponseWriter, r *http.Request) {
	var req agentapi.CreateBranchDataRequest
	if !decode(w, r, &req) {
		return
	}
	d, err := s.Ops.CreateBranchData(r.Context(), r.PathValue("name"), req)
	reply(w, d, err)
}

func (s *Server) deleteBranchData(w http.ResponseWriter, r *http.Request) {
	replyNoContent(w, s.Ops.DeleteBranchData(r.Context(), r.PathValue("name")))
}

func (s *Server) createRestorePoint(w http.ResponseWriter, r *http.Request) {
	var req agentapi.CreateRestorePointRequest
	if !decode(w, r, &req) {
		return
	}
	rp, err := s.Ops.CreateRestorePoint(r.Context(), r.PathValue("name"), req)
	reply(w, rp, err)
}

func (s *Server) listRestorePoints(w http.ResponseWriter, r *http.Request) {
	rps, err := s.Ops.ListRestorePoints(r.PathValue("name"))
	if rps == nil {
		rps = []agentapi.RestorePoint{}
	}
	reply(w, agentapi.RestorePointList{RestorePoints: rps}, err)
}

func (s *Server) deleteRestorePoint(w http.ResponseWriter, r *http.Request) {
	replyNoContent(w, s.Ops.DeleteRestorePoint(r.Context(), r.PathValue("name"), r.PathValue("id")))
}

func (s *Server) listGenerations(w http.ResponseWriter, r *http.Request) {
	g, err := s.Ops.ListGenerations(r.PathValue("name"))
	reply(w, g, err)
}

func (s *Server) createGeneration(w http.ResponseWriter, r *http.Request) {
	var req agentapi.CreateGenerationRequest
	if !decode(w, r, &req) {
		return
	}
	g, err := s.Ops.CreateGeneration(r.Context(), r.PathValue("name"), req)
	reply(w, g, err)
}

func (s *Server) activateGeneration(w http.ResponseWriter, r *http.Request) {
	n, ok := generation(w, r)
	if !ok {
		return
	}
	g, err := s.Ops.ActivateGeneration(r.Context(), r.PathValue("name"), n)
	reply(w, g, err)
}

func (s *Server) detachGeneration(w http.ResponseWriter, r *http.Request) {
	n, ok := generation(w, r)
	if !ok {
		return
	}
	var req agentapi.DetachRequest
	if !decode(w, r, &req) {
		return
	}
	d, err := s.Ops.DetachGeneration(r.Context(), r.PathValue("name"), n, req)
	reply(w, d, err)
}

func (s *Server) deleteGeneration(w http.ResponseWriter, r *http.Request) {
	n, ok := generation(w, r)
	if !ok {
		return
	}
	replyNoContent(w, s.Ops.DeleteGeneration(r.Context(), r.PathValue("name"), n))
}

// generation parses the {n} path segment.
func generation(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, agentapi.CodeInvalid, "generation must be a positive integer")
		return 0, false
	}
	return n, true
}

// decode reads a JSON body into v. An empty body leaves v at its zero value; unknown fields
// and bodies over 64 KiB are refused.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, agentapi.CodeInvalid, "request body: "+err.Error())
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func replyNoContent(w http.ResponseWriter, err error) {
	if err != nil {
		writeOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeOpError maps a volume error to its status code and error code.
func writeOpError(w http.ResponseWriter, err error) {
	var rl *volume.RateLimitError
	switch {
	case errors.As(err, &rl):
		secs := int(math.Ceil(rl.RetryAfter.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
		writeError(w, http.StatusTooManyRequests, agentapi.CodeRateLimited, err.Error())
	case errors.Is(err, volume.ErrInvalid):
		writeError(w, http.StatusBadRequest, agentapi.CodeInvalid, err.Error())
	case errors.Is(err, volume.ErrNotFound):
		writeError(w, http.StatusNotFound, agentapi.CodeNotFound, err.Error())
	case errors.Is(err, volume.ErrNotRunning):
		writeError(w, http.StatusConflict, agentapi.CodeNotRunning, err.Error())
	case errors.Is(err, volume.ErrBusy):
		writeError(w, http.StatusConflict, agentapi.CodeBusy, err.Error())
	case errors.Is(err, volume.ErrConflict):
		writeError(w, http.StatusConflict, agentapi.CodeConflict, err.Error())
	case errors.Is(err, volume.ErrInsufficientStorage):
		writeError(w, http.StatusInsufficientStorage, agentapi.CodeInsufficientStorage, err.Error())
	case errors.Is(err, volume.ErrSupervisorUnavailable):
		writeError(w, http.StatusBadGateway, agentapi.CodeSupervisorUnavailable, err.Error())
	default:
		slog.Error("volume operation failed", "err", err)
		writeError(w, http.StatusInternalServerError, agentapi.CodeInternal, err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, code agentapi.ErrorCode, msg string) {
	writeJSON(w, status, agentapi.Error{Error: msg, Code: code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// logRequests logs every API request with its route, branch, status and duration (spec 11).
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		// The mux sets the matched pattern and path values on r while routing.
		slog.Info("request", "op", r.Pattern, "branch", r.PathValue("name"),
			"status", rec.status, "durationMs", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
```

- [ ] **Step 5: 테스트 통과 확인**

Run: `go vet ./... && go test -race -v ./internal/agent/...`
Expected: `TestRoutesReachTheVolume`, `TestEveryV1RouteNeedsTheToken`, `TestErrorsMapToStatusAndCode`, `TestBadRequestsStopAtTheAPI`, `TestEmptyBodyIsAnEmptyRequest`, `TestRestorePointListIsNeverNull` PASS

- [ ] **Step 6: 커밋**

```bash
git add internal/agent/
git commit -m "feat(agent): 토큰 인증 에이전트 HTTP API와 오류 코드" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 12: 에이전트 클라이언트

**Files:**
- Create: `internal/agentclient/client.go`
- Test: `internal/agentclient/client_test.go`

**Interfaces:**
- Consumes: `agent.Server`, `agenttest.FakeOps`, `agenttest.Call` (Task 11), `agentapi` (Task 5)
- Produces: `agentclient.New(baseURL, token string, hc *http.Client) *Client`(nil hc는 60초 제한, `baseURL` 끝의 `/`는 무시), `type APIError struct { StatusCode int; Code agentapi.ErrorCode; Message string; RetryAfter time.Duration }`, `agentclient.CodeOf(err error) agentapi.ErrorCode`, 메서드 `Preflight(ctx)`, `Usage(ctx)`, `InitMain(ctx)`, `CreateBranchData(ctx, name, req)`, `DeleteBranchData(ctx, name)`, `CreateRestorePoint(ctx, branch, req)`, `ListRestorePoints(ctx, branch)`, `DeleteRestorePoint(ctx, branch, id)`, `ListGenerations(ctx, branch)`, `CreateGeneration(ctx, branch, req)`, `ActivateGeneration(ctx, branch, n)`, `DetachGeneration(ctx, branch, n, req)`, `DeleteGeneration(ctx, branch, n)` — 반환형은 `agent.Ops`의 같은 이름 메서드와 같다. 계획 3(오퍼레이터)과 계획 4(API 서버)가 쓴다.

- [ ] **Step 1: 실패하는 테스트 쓰기**

`internal/agentclient/client_test.go`:

```go
package agentclient_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"consonance/internal/agent"
	"consonance/internal/agent/agenttest"
	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/volume"
)

const rpID = "rp-20261007T031500Z-ab12"

func newAgent(t *testing.T, ops *agenttest.FakeOps) string {
	t.Helper()
	srv := httptest.NewServer((&agent.Server{Token: "t", Ops: ops}).Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestClientCallsEveryOperation(t *testing.T) {
	ops := &agenttest.FakeOps{
		PreflightResult:   agentapi.Preflight{Reflink: true, Kernel: "6.8.0-63-generic"},
		UsageResult:       agentapi.Usage{CapacityBytes: 100, UsedBytes: 10, AvailableBytes: 90},
		GenerationResult:  agentapi.Generation{Generation: 2},
		BranchDataResult:  agentapi.BranchData{Generation: 1, BackupStartLSN: "0/3000028"},
		RestorePoint:      agentapi.RestorePoint{ID: rpID, Branch: "main", Reason: agentapi.ReasonAPI},
		RestorePoints:     []agentapi.RestorePoint{{ID: rpID, Branch: "main"}},
		GenerationsResult: agentapi.Generations{Current: 2, Generations: []int{1, 2}},
		DetachResult:      agentapi.DetachResponse{Branch: "main-before-20261007031500"},
	}
	c := agentclient.New(newAgent(t, ops)+"/", "t", nil) // a trailing slash must not matter
	ctx := context.Background()
	check := func(what string, err error, want agenttest.Call) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := ops.Last(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s reached %+v, want %+v", what, got, want)
		}
	}

	p, err := c.Preflight(ctx)
	check("Preflight", err, agenttest.Call{Op: "Preflight"})
	if p != ops.PreflightResult {
		t.Errorf("Preflight = %+v", p)
	}
	u, err := c.Usage(ctx)
	check("Usage", err, agenttest.Call{Op: "Usage"})
	if u != ops.UsageResult {
		t.Errorf("Usage = %+v", u)
	}
	g, err := c.InitMain(ctx)
	check("InitMain", err, agenttest.Call{Op: "InitMain"})
	if g.Generation != 2 {
		t.Errorf("InitMain = %+v", g)
	}
	branchReq := agentapi.CreateBranchDataRequest{Parent: "main", Source: agentapi.Source{Type: agentapi.SourceParentLatest}}
	d, err := c.CreateBranchData(ctx, "dev", branchReq)
	check("CreateBranchData", err, agenttest.Call{Op: "CreateBranchData", Branch: "dev", Body: branchReq})
	if d != ops.BranchDataResult {
		t.Errorf("CreateBranchData = %+v", d)
	}
	check("DeleteBranchData", c.DeleteBranchData(ctx, "dev"), agenttest.Call{Op: "DeleteBranchData", Branch: "dev"})
	rpReq := agentapi.CreateRestorePointRequest{Label: "before"}
	rp, err := c.CreateRestorePoint(ctx, "main", rpReq)
	check("CreateRestorePoint", err, agenttest.Call{Op: "CreateRestorePoint", Branch: "main", Body: rpReq})
	if rp.ID != rpID || rp.Reason != agentapi.ReasonAPI {
		t.Errorf("CreateRestorePoint = %+v", rp)
	}
	rps, err := c.ListRestorePoints(ctx, "main")
	check("ListRestorePoints", err, agenttest.Call{Op: "ListRestorePoints", Branch: "main"})
	if len(rps) != 1 || rps[0].ID != rpID {
		t.Errorf("ListRestorePoints = %+v", rps)
	}
	check("DeleteRestorePoint", c.DeleteRestorePoint(ctx, "main", rpID), agenttest.Call{Op: "DeleteRestorePoint", Branch: "main", ID: rpID})
	gens, err := c.ListGenerations(ctx, "main")
	check("ListGenerations", err, agenttest.Call{Op: "ListGenerations", Branch: "main"})
	if gens.Current != 2 || len(gens.Generations) != 2 {
		t.Errorf("ListGenerations = %+v", gens)
	}
	genReq := agentapi.CreateGenerationRequest{Source: agentapi.Source{Type: agentapi.SourceRestorePoint, RestorePointID: rpID}}
	g, err = c.CreateGeneration(ctx, "main", genReq)
	check("CreateGeneration", err, agenttest.Call{Op: "CreateGeneration", Branch: "main", Body: genReq})
	if g.Generation != 2 {
		t.Errorf("CreateGeneration = %+v", g)
	}
	gens, err = c.ActivateGeneration(ctx, "main", 2)
	check("ActivateGeneration", err, agenttest.Call{Op: "ActivateGeneration", Branch: "main", N: 2})
	if gens.Current != 2 {
		t.Errorf("ActivateGeneration = %+v", gens)
	}
	detReq := agentapi.DetachRequest{ToBranch: "main-before-20261007031500"}
	det, err := c.DetachGeneration(ctx, "main", 1, detReq)
	check("DetachGeneration", err, agenttest.Call{Op: "DetachGeneration", Branch: "main", N: 1, Body: detReq})
	if det.Branch != detReq.ToBranch {
		t.Errorf("DetachGeneration = %+v", det)
	}
	check("DeleteGeneration", c.DeleteGeneration(ctx, "main", 3), agenttest.Call{Op: "DeleteGeneration", Branch: "main", N: 3})
}

func TestClientErrors(t *testing.T) {
	ops := &agenttest.FakeOps{}
	url := newAgent(t, ops)
	c := agentclient.New(url, "t", nil)
	ctx := context.Background()

	ops.SetErr(&volume.RateLimitError{Source: "main", RetryAfter: 1500 * time.Millisecond})
	_, err := c.CreateRestorePoint(ctx, "main", agentapi.CreateRestorePointRequest{})
	var apiErr *agentclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests ||
		apiErr.Code != agentapi.CodeRateLimited || apiErr.RetryAfter != 2*time.Second {
		t.Fatalf("rate limited: %#v", err)
	}
	ops.SetErr(fmt.Errorf("%w (waited 2s)", volume.ErrBusy))
	if code := agentclient.CodeOf(c.DeleteBranchData(ctx, "dev")); code != agentapi.CodeBusy {
		t.Fatalf("busy: code %q", code)
	}
	ops.SetErr(nil)
	if code := agentclient.CodeOf(agentclient.New(url, "wrong", nil).DeleteBranchData(ctx, "dev")); code != agentapi.CodeUnauthorized {
		t.Fatalf("wrong token: code %q", code)
	}
	if code := agentclient.CodeOf(errors.New("plain")); code != "" {
		t.Fatalf("CodeOf(plain error) = %q", code)
	}
}

func TestClientReadsForeignErrorBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream connect error", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	_, err := agentclient.New(srv.URL, "t", nil).Usage(context.Background())
	var apiErr *agentclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway || apiErr.Code != "" || apiErr.Message != "upstream connect error" {
		t.Fatalf("got %#v", err)
	}
}
```

- [ ] **Step 2: 테스트가 실패하는지 확인**

Run: `go test ./internal/agentclient/`
Expected: FAIL — `no non-test Go files` 또는 `undefined: agentclient.New`

- [ ] **Step 3: 구현**

`internal/agentclient/client.go`:

```go
// Package agentclient calls a project's volume agent (spec 7.2). The operator (plan 3) and the
// API server (plan 4) use it.
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"consonance/internal/agentapi"
)

// Client talks to one agent.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

// New returns a client for the agent at baseURL, e.g.
// "http://volume-agent.cns-a1b2c3d4e5f6.svc:8443". A nil hc gets a client with a 60s timeout:
// a copy takes about a second, after a lock wait of at most 2s and a backup start whose
// checkpoint takes longer under write load.
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, hc: hc}
}

// APIError is a non-2xx response from the agent.
type APIError struct {
	StatusCode int
	Code       agentapi.ErrorCode // empty when the body was not the agent's
	Message    string
	RetryAfter time.Duration // from Retry-After, on 429
}

func (e *APIError) Error() string {
	return fmt.Sprintf("agent: %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// CodeOf returns the error code of an agent API error, or "" for any other error.
func CodeOf(err error) agentapi.ErrorCode {
	var e *APIError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func (c *Client) Preflight(ctx context.Context) (agentapi.Preflight, error) {
	var out agentapi.Preflight
	err := c.do(ctx, http.MethodGet, "/v1/preflight", nil, &out)
	return out, err
}

func (c *Client) Usage(ctx context.Context) (agentapi.Usage, error) {
	var out agentapi.Usage
	err := c.do(ctx, http.MethodGet, "/v1/usage", nil, &out)
	return out, err
}

func (c *Client) InitMain(ctx context.Context) (agentapi.Generation, error) {
	var out agentapi.Generation
	err := c.do(ctx, http.MethodPost, "/v1/init-main", nil, &out)
	return out, err
}

func (c *Client) CreateBranchData(ctx context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error) {
	var out agentapi.BranchData
	err := c.do(ctx, http.MethodPost, branchPath(name, "data"), req, &out)
	return out, err
}

func (c *Client) DeleteBranchData(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, branchPath(name, "data"), nil, nil)
}

func (c *Client) CreateRestorePoint(ctx context.Context, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error) {
	var out agentapi.RestorePoint
	err := c.do(ctx, http.MethodPost, branchPath(branch, "restore-points"), req, &out)
	return out, err
}

func (c *Client) ListRestorePoints(ctx context.Context, branch string) ([]agentapi.RestorePoint, error) {
	var out agentapi.RestorePointList
	err := c.do(ctx, http.MethodGet, branchPath(branch, "restore-points"), nil, &out)
	return out.RestorePoints, err
}

func (c *Client) DeleteRestorePoint(ctx context.Context, branch, id string) error {
	return c.do(ctx, http.MethodDelete, branchPath(branch, "restore-points", url.PathEscape(id)), nil, nil)
}

func (c *Client) ListGenerations(ctx context.Context, branch string) (agentapi.Generations, error) {
	var out agentapi.Generations
	err := c.do(ctx, http.MethodGet, branchPath(branch, "generations"), nil, &out)
	return out, err
}

func (c *Client) CreateGeneration(ctx context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error) {
	var out agentapi.Generation
	err := c.do(ctx, http.MethodPost, branchPath(branch, "generations"), req, &out)
	return out, err
}

func (c *Client) ActivateGeneration(ctx context.Context, branch string, n int) (agentapi.Generations, error) {
	var out agentapi.Generations
	err := c.do(ctx, http.MethodPost, branchPath(branch, "generations", strconv.Itoa(n), "activate"), nil, &out)
	return out, err
}

func (c *Client) DetachGeneration(ctx context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error) {
	var out agentapi.DetachResponse
	err := c.do(ctx, http.MethodPost, branchPath(branch, "generations", strconv.Itoa(n), "detach"), req, &out)
	return out, err
}

func (c *Client) DeleteGeneration(ctx context.Context, branch string, n int) error {
	return c.do(ctx, http.MethodDelete, branchPath(branch, "generations", strconv.Itoa(n)), nil, nil)
}

// branchPath returns /v1/branches/<branch>/<rest...>.
func branchPath(branch string, rest ...string) string {
	return "/v1/branches/" + url.PathEscape(branch) + "/" + strings.Join(rest, "/")
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
	req.Header.Set(agentapi.TokenHeader, c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("agent: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// apiError reads an error response. A body that is not the agent's JSON, such as a proxy's
// error page, becomes the message as it is.
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &APIError{StatusCode: resp.StatusCode}
	var body agentapi.Error
	if err := json.Unmarshal(b, &body); err == nil && body.Error != "" {
		e.Code, e.Message = body.Code, body.Error
	} else if e.Message = strings.TrimSpace(string(b)); e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		e.RetryAfter = time.Duration(secs) * time.Second
	}
	return e
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go vet ./... && go test -race -v ./internal/agentclient/`
Expected: `TestClientCallsEveryOperation`, `TestClientErrors`, `TestClientReadsForeignErrorBodies` PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/agentclient/
git commit -m "feat(agentclient): 볼륨 에이전트 API 클라이언트" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 13: 에이전트 실행 파일

**Files:**
- Create: `internal/secretfile/secretfile.go`, `cmd/agent/main.go`
- Modify: `cmd/supervisor/main.go` (`readSecret`를 `secretfile.Read`로 바꾼다)
- Test: `internal/secretfile/secretfile_test.go`, `cmd/agent/main_test.go`

**Interfaces:**
- Consumes: `volume.Open`·`Config`·`Recover`·`RunJanitor` (Task 6, 9), `agent.Server`·`Ops` (Task 11), `agentclient` (Task 12), `supervisorclient.New`(계획 1), `pgtest.FreePort`·`BinDir`
- Produces: `secretfile.Read(path string) (string, error)`(앞뒤 공백을 자른 비밀, 비어 있으면 `"<path> is empty"` 오류); 실행 파일 `consonance-agent`와 플래그 `--volume`(/volume), `--listen`(:8443), `--token-file`(/etc/consonance/token/token), `--initdb`(/usr/lib/postgresql/17/bin/initdb), `--supervisor-url`(http://br-{branch}:8008), `--project-id`, `--min-copy-interval`(5s), `--lock-wait`(2s), `--max-used-percent`(90), `--restore-points-per-branch`(20), `--restore-point-max-age`(24h), `--retention-interval`(1h). 기동하면 `Recover`로 정리하고 정리한 경로를 로그로 남긴 뒤 정리 작업(janitor)과 API를 돌린다. SIGHUP은 무시하고, SIGTERM·SIGINT를 받으면 진행 중인 요청을 최대 25초 기다린 뒤 종료 코드 0으로 끝난다.

- [ ] **Step 1: 비밀 파일 읽기를 공용 패키지로 옮기기**

`internal/secretfile/secretfile_test.go`:

```go
package secretfile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"consonance/internal/secretfile"
)

func TestRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := secretfile.Read(path); err != nil || got != "s3cret" {
		t.Fatalf("Read = %q, %v; want s3cret", got, err)
	}
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secretfile.Read(path); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty secret: %v, want an 'is empty' error", err)
	}
	if _, err := secretfile.Read(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file was read")
	}
}
```

`internal/secretfile/secretfile.go`:

```go
// Package secretfile reads secrets mounted as files.
package secretfile

import (
	"fmt"
	"os"
	"strings"
)

// Read returns the trimmed contents of a secret file. An empty secret is an error: an empty
// token would make an API answer 401 to everyone, and an empty password would create a login
// role without one.
func Read(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return secret, nil
}
```

`cmd/supervisor/main.go`에서 `readSecret(` 호출 세 곳을 `secretfile.Read(`로 바꾸고, 함수 `readSecret`(주석 포함)와 import `"strings"`를 지우고, import `"consonance/internal/secretfile"`을 더한다. 동작은 같다(같은 오류 문구).

Run: `go vet ./... && go test -race ./internal/secretfile/ ./cmd/supervisor/`
Expected: 두 패키지 모두 `ok`(`TestSupervisorRefusesEmptySecrets` 포함, PostgreSQL 테스트는 SKIP)

- [ ] **Step 2: 실패하는 테스트 쓰기**

`cmd/agent/main_test.go`:

```go
package main_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/testutil/pgtest"
)

const token = "agent-token"

func buildAgent(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "consonance-agent")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

// newVolume returns an empty volume directory and a token file.
func newVolume(t *testing.T) (vol, tokenFile string) {
	t.Helper()
	dir := t.TempDir()
	vol = filepath.Join(dir, "volume")
	if err := os.Mkdir(vol, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile = filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return vol, tokenFile
}

// agentProc is an agent binary started by a test, with its output in a log file.
type agentProc struct {
	cmd     *exec.Cmd
	exited  chan struct{} // closed once the process has exited; waitErr is set by then
	waitErr error
	logPath string
	url     string
}

// startAgent runs the agent on vol and waits until it answers /healthz. The process is killed
// when the test ends, and its output is logged if the test failed.
func startAgent(t *testing.T, bin, vol, tokenFile string, extra ...string) *agentProc {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", pgtest.FreePort(t))
	p := &agentProc{exited: make(chan struct{}), logPath: filepath.Join(t.TempDir(), "agent.log"), url: "http://" + addr}
	logFile, err := os.Create(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	args := append([]string{"--volume", vol, "--token-file", tokenFile, "--listen", addr}, extra...)
	p.cmd = exec.Command(bin, args...)
	p.cmd.Stdout, p.cmd.Stderr = logFile, logFile
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.waitErr = p.cmd.Wait(); close(p.exited) }()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill() // nothing happens once it has exited
		<-p.exited
		if t.Failed() {
			t.Logf("agent output:\n%s", p.logs())
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get(p.url + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return p
			}
		}
		select {
		case <-p.exited:
			t.Fatalf("the agent exited: %v\n%s", p.waitErr, p.logs())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent did not answer /healthz:\n%s", p.logs())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (p *agentProc) logs() string {
	b, _ := os.ReadFile(p.logPath)
	return string(b)
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestAgentServesAndShutsDown(t *testing.T) {
	bin := buildAgent(t)
	vol, tok := newVolume(t)
	p := startAgent(t, bin, vol, tok)
	ctx := testCtx(t)
	c := agentclient.New(p.url, token, nil)
	pre, err := c.Preflight(ctx)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if pre.Reflink || pre.Error == "" {
		t.Fatalf("Preflight on a temp directory = %+v, want no reflink and the reason", pre)
	}
	if _, err := agentclient.New(p.url, "wrong", nil).Usage(ctx); agentclient.CodeOf(err) != agentapi.CodeUnauthorized {
		t.Fatalf("wrong token: %v", err)
	}
	// SIGHUP, a reload habit, must not stop the agent.
	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := c.Usage(ctx); err != nil {
		t.Fatalf("after SIGHUP: %v", err)
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not exit within 10s of SIGTERM")
	}
	if p.waitErr != nil {
		t.Fatalf("exit after SIGTERM: %v", p.waitErr)
	}
}

func TestAgentRefusesEmptyToken(t *testing.T) {
	bin := buildAgent(t)
	vol, tok := newVolume(t)
	if err := os.WriteFile(tok, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--volume", vol, "--token-file", tok, "--listen", "127.0.0.1:0").CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("the agent did not exit on its own")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("agent with an empty token: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "is empty") {
		t.Fatalf("the output does not say why:\n%s", out)
	}
}

func TestAgentRecoversOnStart(t *testing.T) {
	bin := buildAgent(t)
	vol, tok := newVolume(t)
	// A branch copy that the previous agent did not finish.
	half := filepath.Join(vol, "branches", "dev", "data-1")
	if err := os.MkdirAll(half, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(half, ".incomplete"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data-1", filepath.Join(vol, "branches", "dev", "current")); err != nil {
		t.Fatal(err)
	}
	// Complete data that must survive.
	mainData := filepath.Join(vol, "branches", "main", "data-1")
	if err := os.MkdirAll(mainData, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainData, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data-1", filepath.Join(vol, "branches", "main", "current")); err != nil {
		t.Fatal(err)
	}

	p := startAgent(t, bin, vol, tok)
	ctx := testCtx(t)
	c := agentclient.New(p.url, token, nil)
	if _, err := c.ListGenerations(ctx, "dev"); agentclient.CodeOf(err) != agentapi.CodeNotFound {
		t.Fatalf("dev after the restart: %v, want not-found", err)
	}
	if gens, err := c.ListGenerations(ctx, "main"); err != nil || gens.Current != 1 {
		t.Fatalf("main after the restart: %+v, %v", gens, err)
	}
	if !strings.Contains(p.logs(), "branches/dev") {
		t.Fatalf("the cleanup was not logged:\n%s", p.logs())
	}
	trash := filepath.Join(vol, ".consonance", "trash")
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(trash)
		if err == nil && len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the janitor did not empty the trash: %v, %v", entries, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAgentRejectsHostileNames(t *testing.T) {
	bin := buildAgent(t)
	vol, tok := newVolume(t)
	// Something next to the volume that a path trick could reach.
	outside := filepath.Join(filepath.Dir(vol), "outside")
	if err := os.MkdirAll(filepath.Join(outside, "data-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data-1", filepath.Join(outside, "current")); err != nil {
		t.Fatal(err)
	}
	p := startAgent(t, bin, vol, tok)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/v1/branches/..%2F..%2Foutside/generations"},
		{"GET", "/v1/branches/..%2F..%2Fetc/restore-points"},
		{"GET", "/v1/branches/Main/generations"},
		{"GET", "/v1/branches/" + strings.Repeat("a", 41) + "/generations"},
		{"DELETE", "/v1/branches/..%2F..%2Foutside/data"},
		{"DELETE", "/v1/branches/main/restore-points/..%2F..%2Fbranches%2Fmain"},
	} {
		req, err := http.NewRequest(rt.method, p.url+rt.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(agentapi.TokenHeader, token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s = %d %s, want 400", rt.method, rt.path, resp.StatusCode, body)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "data-1")); err != nil {
		t.Fatalf("something outside the volume was touched: %v", err)
	}
}

func TestAgentInitializesMain(t *testing.T) {
	pgBin := pgtest.BinDir(t)
	bin := buildAgent(t)
	vol, tok := newVolume(t)
	p := startAgent(t, bin, vol, tok, "--initdb", filepath.Join(pgBin, "initdb"))
	ctx := testCtx(t)
	c := agentclient.New(p.url, token, nil)
	if g, err := c.InitMain(ctx); err != nil || g.Generation != 1 {
		t.Fatalf("InitMain = %+v, %v", g, err)
	}
	if gens, err := c.ListGenerations(ctx, "main"); err != nil || gens.Current != 1 {
		t.Fatalf("ListGenerations = %+v, %v", gens, err)
	}
}
```

- [ ] **Step 3: 테스트가 실패하는지 확인**

Run: `go test ./cmd/agent/`
Expected: FAIL — `go build`가 실패한다(`no Go files in .../cmd/agent`)

- [ ] **Step 4: 구현**

`cmd/agent/main.go`:

```go
// Command consonance-agent manages one project volume and serves the internal API that the
// operator and the API server use (spec 7.2).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"consonance/internal/agent"
	"consonance/internal/secretfile"
	"consonance/internal/supervisorclient"
	"consonance/internal/volume"
)

var _ agent.Ops = (*volume.Volume)(nil)

type config struct {
	volume          string
	listen          string
	tokenFile       string
	initdb          string
	supervisorURL   string
	projectID       string
	minCopyInterval time.Duration
	lockWait        time.Duration
	maxUsedPercent  float64
	perBranch       int
	maxAge          time.Duration
	retentionEvery  time.Duration
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("agent exited", "err", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("consonance-agent", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.volume, "volume", "/volume", "project volume root")
	fs.StringVar(&c.listen, "listen", ":8443", "API listen address")
	fs.StringVar(&c.tokenFile, "token-file", "/etc/consonance/token/token", "file holding the project's internal token, for this API and the supervisors'")
	fs.StringVar(&c.initdb, "initdb", "/usr/lib/postgresql/17/bin/initdb", "initdb binary")
	fs.StringVar(&c.supervisorURL, "supervisor-url", "http://br-{branch}:8008", "a branch supervisor's URL; {branch} becomes the branch name")
	fs.StringVar(&c.projectID, "project-id", "", "project ID added to every log line")
	fs.DurationVar(&c.minCopyInterval, "min-copy-interval", 5*time.Second, "minimum time between backup-mode copies of one branch")
	fs.DurationVar(&c.lockWait, "lock-wait", 2*time.Second, "longest wait for another volume operation")
	fs.Float64Var(&c.maxUsedPercent, "max-used-percent", 90, "refuse copies above this volume usage")
	fs.IntVar(&c.perBranch, "restore-points-per-branch", 20, "restore points kept per branch")
	fs.DurationVar(&c.maxAge, "restore-point-max-age", 24*time.Hour, "restore points older than this are removed")
	fs.DurationVar(&c.retentionEvery, "retention-interval", time.Hour, "how often the restore point retention runs")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if !strings.Contains(c.supervisorURL, "{branch}") {
		return c, fmt.Errorf("--supervisor-url %q has no {branch}", c.supervisorURL)
	}
	if c.retentionEvery <= 0 {
		return c, fmt.Errorf("--retention-interval must be positive")
	}
	return c, nil
}

func run(args []string) error {
	// Go exits on SIGHUP by default, and tini would follow. There is nothing to reload.
	signal.Ignore(syscall.SIGHUP)

	c, err := parseFlags(args)
	if err != nil {
		return err
	}
	if c.projectID != "" {
		slog.SetDefault(slog.Default().With("projectId", c.projectID))
	}
	token, err := secretfile.Read(c.tokenFile)
	if err != nil {
		return fmt.Errorf("token file: %w", err)
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	vol, err := volume.Open(volume.Config{
		Root:   c.volume,
		InitDB: c.initdb,
		Supervisor: func(branch string) volume.Supervisor {
			return supervisorclient.New(strings.ReplaceAll(c.supervisorURL, "{branch}", branch), token, hc)
		},
		MinCopyInterval:        c.minCopyInterval,
		LockWait:               c.lockWait,
		MaxUsedPercent:         c.maxUsedPercent,
		RestorePointsPerBranch: c.perBranch,
		RestorePointMaxAge:     c.maxAge,
	})
	if err != nil {
		return err
	}
	removed, err := vol.Recover()
	if err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	if len(removed) > 0 {
		slog.Warn("removed what interrupted operations left", "paths", removed)
	}

	ln, err := net.Listen("tcp", c.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", c.listen, err)
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	janitorDone := make(chan struct{})
	go func() {
		defer close(janitorDone)
		vol.RunJanitor(janitorCtx, c.retentionEvery)
	}()
	defer func() {
		stopJanitor()
		<-janitorDone
	}()

	httpSrv := &http.Server{Handler: (&agent.Server{Token: token, Ops: vol}).Handler(), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	slog.Info("agent listening", "addr", ln.Addr().String(), "volume", c.volume)

	select {
	case <-sigCtx.Done():
		slog.Info("shutting down")
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	}
	// Let running operations finish: a copy takes about a second, and Kubernetes waits 30s
	// before it kills the pod.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	return httpSrv.Shutdown(ctx)
}
```

- [ ] **Step 5: Mac에서 통과 확인**

Run: `go vet ./... && go test -race -v ./cmd/agent/`
Expected: `TestAgentServesAndShutsDown`, `TestAgentRefusesEmptyToken`, `TestAgentRecoversOnStart`, `TestAgentRejectsHostileNames` PASS, `TestAgentInitializesMain` SKIP

- [ ] **Step 6: VM에서 통과 확인**

Run: `hack/vm-test.sh -race -v ./cmd/agent/... ./cmd/supervisor/...`
Expected: 두 패키지의 테스트 모두 PASS(SKIP 없음)

- [ ] **Step 7: 커밋**

```bash
git add internal/secretfile/ cmd/agent/ cmd/supervisor/main.go
git commit -m "feat(agent): 에이전트 실행 파일 — 기동 시 정리, 정리 작업, 신호 처리" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 14: 에이전트 이미지, 스모크 테스트, 문서

**Files:**
- Create: `images/agent/Dockerfile`, `test/image/agent-smoke.sh`
- Modify: `Makefile`, `docs/superpowers/specs/2026-10-07-branching-engine-design.md` (7.2), `docs/superpowers/plans/2026-10-07-branching-engine-index.md`, `docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`

**Interfaces:**
- Consumes: `cmd/agent` (Task 13)
- Produces: 이미지 `consonance/agent:dev`(`make image-agent`), `make smoke-agent`, `make images`; 스펙 7.2와 이월 사항 문서에 이 계획의 결정이 반영된다.

- [ ] **Step 1: 실패하는 스모크 테스트 쓰기**

`test/image/agent-smoke.sh` (실행 권한 755):

```bash
#!/usr/bin/env bash
# Smoke test for consonance/agent: the agent serves its API on a volume, refuses requests
# without the token, makes main's data with initdb, and exits cleanly on SIGTERM.
set -euo pipefail
IMAGE=${IMAGE:-consonance/agent:dev}
NAME=consonance-agent-smoke-$$
TOKEN=smoke-token
PORT=${PORT:-18443}

cleanup() {
  docker rm -f -v "$NAME" >/dev/null 2>&1 || true
  docker volume rm "$NAME" "$NAME-token" >/dev/null 2>&1 || true
}
trap cleanup EXIT

stopsignal=$(docker image inspect --format '{{.Config.StopSignal}}' "$IMAGE")
[ "$stopsignal" = SIGTERM ] || { echo "FAIL: image stop signal is '$stopsignal', want SIGTERM"; exit 1; }

# The project volume: a new volume takes the owner of the image's /volume (999).
docker volume create "$NAME" >/dev/null
docker volume create "$NAME-token" >/dev/null
docker run --rm --user 0 -v "$NAME-token:/token" --entrypoint sh "$IMAGE" \
  -c 'printf %s "$1" > /token/token && chmod 644 /token/token' _ "$TOKEN"

docker run -d --name "$NAME" -v "$NAME:/volume" -v "$NAME-token:/etc/consonance/token:ro" \
  -p "127.0.0.1:$PORT:8443" "$IMAGE" >/dev/null

for _ in $(seq 1 100); do
  curl -fsS "localhost:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "localhost:$PORT/healthz" >/dev/null || { docker logs "$NAME"; echo "FAIL: the agent does not answer"; exit 1; }

code=$(curl -s -o /dev/null -w '%{http_code}' "localhost:$PORT/v1/usage")
[ "$code" = 401 ] || { echo "FAIL: usage without the token returned $code"; exit 1; }

auth=(-H "X-Consonance-Token: $TOKEN")
# Docker's volume is not XFS: preflight answers and reports no reflink.
preflight=$(curl -fsS "${auth[@]}" "localhost:$PORT/v1/preflight")
case "$preflight" in *'"reflink":false'*) ;; *) echo "FAIL: preflight: $preflight"; exit 1 ;; esac

init=$(curl -fsS -X POST "${auth[@]}" "localhost:$PORT/v1/init-main")
case "$init" in *'"generation":1'*) ;; *) docker logs "$NAME"; echo "FAIL: init-main: $init"; exit 1 ;; esac
gens=$(curl -fsS "${auth[@]}" "localhost:$PORT/v1/branches/main/generations")
case "$gens" in *'"current":1'*) ;; *) echo "FAIL: generations: $gens"; exit 1 ;; esac

docker stop -t 30 "$NAME" >/dev/null
exit_code=$(docker inspect --format '{{.State.ExitCode}}' "$NAME")
[ "$exit_code" = 0 ] || { docker logs "$NAME"; echo "FAIL: the agent exited with $exit_code"; exit 1; }
echo PASS
```

Run: `chmod +x test/image/agent-smoke.sh && (docker info >/dev/null 2>&1 || colima start) && test/image/agent-smoke.sh`
Expected: FAIL — 이미지가 아직 없어 `docker image inspect`가 `No such image: consonance/agent:dev`로 실패한다

- [ ] **Step 2: Dockerfile과 Makefile**

`images/agent/Dockerfile`:

```dockerfile
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/consonance-agent ./cmd/agent

# postgres:17 brings initdb (spec 7.6) and the postgres user, uid/gid 999.
FROM postgres:17-bookworm
RUN apt-get update \
 && apt-get install -y --no-install-recommends tini \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /volume \
 && chown 999:999 /volume
COPY --from=build /out/consonance-agent /usr/local/bin/consonance-agent
USER 999:999
STOPSIGNAL SIGTERM
# tini is PID 1: it reaps initdb's children and forwards SIGTERM to the agent.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/consonance-agent"]
```

`Makefile` 전체를 다음으로 바꾼다(레시피 줄은 TAB으로 시작한다):

```make
.PHONY: test test-vm test-xfs vm-setup images image-postgres smoke-postgres image-agent smoke-agent

test: ## host tests (PostgreSQL and XFS tests skip)
	go vet ./...
	go test ./...

vm-setup: ## one-time: start the Lima VM, install Go and PostgreSQL 17, mount the XFS test volume
	hack/vm-setup.sh

test-vm: ## all tests inside the Lima VM
	hack/vm-test.sh ./...

test-xfs: ## internal/volume tests on XFS with reflink inside the Lima VM
	hack/vm-test.sh -race ./internal/volume/... ./internal/testutil/xfstest/...

images: image-postgres image-agent ## build every image

image-postgres: ## build consonance/postgres:17-dev for the local architecture
	docker build -f images/postgres/Dockerfile -t consonance/postgres:17-dev .

smoke-postgres: image-postgres ## run the postgres image smoke test
	test/image/supervisor-smoke.sh

image-agent: ## build consonance/agent:dev for the local architecture
	docker build -f images/agent/Dockerfile -t consonance/agent:dev .

smoke-agent: image-agent ## run the agent image smoke test
	test/image/agent-smoke.sh
```

- [ ] **Step 3: 스모크 테스트 통과 확인**

Run: `make smoke-agent`
Expected: 이미지 빌드 뒤 마지막 줄 `PASS`. 이어서 `docker volume ls -qf dangling=true`를 실행 전과 비교해 이 테스트가 남긴 볼륨이 없는지 확인한다.

- [ ] **Step 4: 스펙 7.2에 이 계획의 결정을 반영**

`docs/superpowers/specs/2026-10-07-branching-engine-design.md` 7.2 표에서 다음 두 줄을

```
| `POST /v1/branches/{name}/generations/{n}/activate` | 브랜치가 정지된 상태에서 `current`를 `data-n`으로 전환 |
| `POST /v1/branches/{name}/generations/{n}/detach` | 본문 `{toBranch}`. `data-n`을 `branches/<toBranch>/data-1`로 옮기고 `current` 연결 |
```

다음 네 줄로 바꾼다:

```
| `POST /v1/branches/{name}/generations/{n}/activate` | 브랜치가 정지된 상태에서 `current`를 `data-n`으로 전환. 감독 프로세스가 정지 상태를 보이지 않으면 409 |
| `POST /v1/branches/{name}/generations/{n}/detach` | 본문 `{toBranch}` 또는 `{asRestorePoint: true}`. 롤백이면 `data-n`을 `branches/<toBranch>/data-1`로 옮기고 `current` 연결, 리셋이면 복구 지점(`reason: reset`)으로 옮긴다. 현재 세대이거나 PostgreSQL이 쓰고 있는 세대면 409 |
| `GET /v1/branches/{name}/generations` | 현재 세대와 완성된 세대 목록 |
| `DELETE /v1/branches/{name}/generations/{n}` | 현재 세대도, PostgreSQL이 쓰는 세대도 아닌 세대를 지운다(롤백 실패 뒤 정리) |
```

같은 절의 마지막 줄 `- 복구 지점 보존 정책(...)은 복구 지점을 만들 때와 1시간마다 적용해 오래된 것부터 지운다.` 뒤에 두 줄을 더한다:

```
- 오류 본문은 `{error, code}`다. `code`: `invalid`(400), `unauthorized`(401), `not-found`(404), `conflict`·`not-running`·`busy`(409), `rate-limited`(429, `Retry-After`), `supervisor-unavailable`(502), `insufficient-storage`(507), `internal`(500).
- 감독 프로세스는 `http://br-<브랜치>:8008`로 부른다. 브랜치 Service가 8008을 노출해야 한다.
```

- [ ] **Step 5: 목차와 이월 사항 갱신**

`docs/superpowers/plans/2026-10-07-branching-engine-index.md`의 계획 2 행에서 상태 `작성 완료`를 `구현 완료`로 바꾼다.

`docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`에서 `### 계획 2(볼륨 에이전트)가 반영할 것` 절(제목부터 `### 계획 3(CRD·오퍼레이터)이 반영할 것` 바로 앞까지)을 지운다. 이 계획이 모두 반영했다. 파일 끝에 다음을 더한다. 실행을 마칠 때 컨트롤러가 리뷰에서 미뤄 둔 지적을 이 절 아래 `### 미뤄 둔 개선 사항`으로 더한다.

```markdown
## 계획 2(볼륨 에이전트)에서

### 계획 3(CRD·오퍼레이터)이 반영할 것

- 브랜치 Service `br-<이름>`은 5432와 함께 감독 프로세스 포트 8008을 노출한다. 에이전트는 `http://br-<브랜치>:8008`로 감독 프로세스를 부른다(`--supervisor-url`).
- 에이전트 파드 인자는 Project spec에서 온다: `--min-copy-interval`(`limits.minCopyInterval`), `--restore-points-per-branch`·`--restore-point-max-age`(`restorePoints`), `--project-id`. 값을 바꾸면 에이전트 파드를 다시 띄워야 반영된다. 볼륨은 `/volume`, 토큰은 Secret `internal-token`을 `/etc/consonance/token`에 마운트한다.
- main 파드는 init-main이 성공한 뒤에 만든다. 그 전에 뜬 감독 프로세스는 `/v1/start`에 500(`data directory does not resolve`)을 돌려준다.
- 에이전트 오류 코드에 따라 다르게 한다: `busy`·`supervisor-unavailable`·500은 지수 백오프로 재시도, `rate-limited`는 `Retry-After` 뒤 재시도, `not-running`은 부모가 실행 중이 아님(API 409), `conflict`는 상태 문제라 그대로 재시도하지 않는다. 복사 도중 원본 브랜치를 정지하면 그 뒤 최대 30초 동안 `busy`가 올 수 있다.
- 재시도해도 안전한 호출: `CreateBranchData`(데이터가 있으면 그 세대와 빈 LSN), `InitMain`, `DetachGeneration`(toBranch로 이미 끝났으면 그 브랜치), `DeleteBranchData`(없으면 성공). 롤백·리셋 도중 오퍼레이터가 재시작하면 `ListGenerations`로 남은 세대를 확인하고, 현재 세대가 아닌 남은 세대는 `DeleteGeneration`으로 치운 뒤 다시 만든다.
- `ActivateGeneration`은 감독 프로세스가 `stopped`를 보일 때만, `DetachGeneration`·`DeleteGeneration`은 PostgreSQL이 그 세대를 쓰지 않을 때만 성공한다(아니면 409).
- 스펙 11장의 메트릭은 에이전트에 아직 없다(계획 5).
```

- [ ] **Step 6: 전체 확인**

Run: `make test && make test-vm`
Expected: 둘 다 종료 코드 0. VM에서는 SKIP 없이 통과한다(`internal/volume`, `cmd/agent`, `cmd/supervisor`, `internal/pgctl` 포함).

- [ ] **Step 7: 커밋**

```bash
git add images/agent/ test/image/agent-smoke.sh Makefile docs/superpowers/specs/2026-10-07-branching-engine-design.md docs/superpowers/plans/2026-10-07-branching-engine-index.md docs/superpowers/plans/2026-10-07-branching-engine-carryover.md
git commit -m "build: consonance/agent 이미지와 스모크 테스트, 스펙 7.2와 이월 사항 갱신" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
