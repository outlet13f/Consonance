# 브랜칭 엔진 구현 계획 3/5: 리소스 타입과 오퍼레이터

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Project·Branch 커스텀 리소스와, 이를 조정해 프로젝트의 Secret·네트워크 정책·쿼터·볼륨·에이전트를 만들고 브랜치의 데이터·파드·정지/기동·롤백/리셋·삭제를 볼륨 에이전트와 감독 프로세스를 불러 수행하는 오퍼레이터 `consonance-operator`를 만든다.

**Architecture:** `api/v1alpha1`이 두 리소스의 타입이다. 스펙 14장의 기본값과 이름 규칙은 CRD 스키마의 기본값과 CEL 규칙으로 둔다. `internal/controller`에는 두 가지가 들어 있다.
- 순수 함수: 하위 리소스 명세(파드, 서비스, Secret, 네트워크 정책, 쿼터, PVC)를 만든다.
- controller-runtime 조정기 둘: Project와 Branch를 조정한다.

조정기는 계획 2의 `agentclient`와 계획 1의 `supervisorclient`를 `Connector` 인터페이스 뒤에서 부른다. 테스트는 envtest API 서버 위에서 가짜 에이전트·감독 프로세스로 한다. 롤백·리셋(스펙 9.4)은 Branch status의 `lastRestore`에 단계를 저장하며 진행하므로, 오퍼레이터가 재시작해도 이어서 한다. `cmd/operator`가 이를 묶는다. 권한(ClusterRole)과 CRD는 controller-gen이 만든다.

**Tech Stack:**
- Go 1.27(툴체인 go1.27.2, Task 1에서 올린다)
- `sigs.k8s.io/controller-runtime` v0.25.2(k8s.io/api·apimachinery·client-go v0.37)
- controller-gen v0.22.0과 setup-envtest v0.25.2: `go run`으로 쓰고 go.mod에는 넣지 않는다.
- envtest: kube-apiserver·etcd 1.37.0, `bin/envtest`에 내려받는다.
- Lima VM `consonance-poc`: PostgreSQL 17과 XFS 테스트는 이전처럼 여기서 돈다.

**Spec:** `docs/superpowers/specs/2026-10-07-branching-engine-design.md` — 특히 4.1~4.5, 7.1, 7.5, 8, 9.1~9.6, 10, 12, 14장. 앞 계획에서 넘어온 사항: `docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`의 "계획 3(CRD·오퍼레이터)이 반영할 것" 두 절.

## Global Constraints

**모듈과 도구**
- 모듈 `consonance`, `go 1.27.0`, `toolchain go1.27.2`(Task 1).
- 새 의존성은 `sigs.k8s.io/controller-runtime v0.25.2`(와 그것이 요구하는 k8s.io 모듈)뿐이다.
- controller-gen(`v0.22.0`)과 setup-envtest(`v0.25.2`, k8s `1.37.0`)는 Makefile에서 `go run 경로@버전`으로 부른다.

**리소스와 이름**
- API 그룹은 `consonance.io/v1alpha1`이다(스펙 4).
- 프로젝트 네임스페이스는 `cns-<projectId>`이고, 그 안의 Project 이름은 항상 `project`다.
- 브랜치 이름은 DNS-1123 라벨이고 40자 이하다. main에는 부모가 없고, 다른 브랜치에는 반드시 있다.
- 하위 객체 이름:
  - PVC `project-volume`
  - 에이전트 파드와 Service `volume-agent`
  - 브랜치 파드와 Service `br-<브랜치>`
  - Secret `db-credentials`(키 `username`=`app_owner`, `password`(32자), `database`=`app`)
  - Secret `internal-token`(키 `token`)
  - ResourceQuota `project-quota`
  - 파이널라이저 `consonance.io/branch-cleanup`
- 라벨:
  - 오퍼레이터가 만드는 모든 객체에 `app.kubernetes.io/managed-by: consonance-operator`
  - `app.kubernetes.io/component`: `project`·`volume-agent`·`branch`
  - 브랜치 파드·Service에 `consonance.io/branch: <브랜치>`
  - 백업 브랜치에 `consonance.io/backup-of: <원래 브랜치>`
  - 네트워크 정책은 `consonance-system` 네임스페이스의 파드를 `app.kubernetes.io/name`(`consonance-operator`, `consonance-apiserver`)으로 가린다.

**파드(스펙 7.5)**
- PSS restricted: `runAsNonRoot`, uid/gid 999, `allowPrivilegeEscalation: false`, capabilities `ALL` 제거, seccomp `RuntimeDefault`.
- 서비스 계정 토큰은 마운트하지 않는다. `terminationGracePeriodSeconds: 45`.
- 에이전트: 100m/128Mi, `fsGroup: 999`, `fsGroupChangePolicy: OnRootMismatch`.
- 브랜치:
  - requests와 limits 모두 compute(브랜치 값, 없으면 Project의 `defaultCompute`)
  - 에이전트 파드와 같은 노드(필수 podAffinity)
  - 볼륨 subPath `branches/<이름>` → `/pgdata`
  - main만 Secret `db-credentials`의 `password`를 `/etc/consonance/db/password`로 받고 `--db-password-file`을 준다.
- 토큰은 Secret `internal-token`을 `/etc/consonance/token`에 마운트한다.

**호출**
- 에이전트: `http://volume-agent.<네임스페이스>.svc:8443`
- 감독 프로세스: `http://br-<브랜치>.<네임스페이스>.svc:8008`
- 헤더 `X-Consonance-Token`은 `internal-token`이다.
- 제한 시간:
  - 복사 3분, init-main 5분
  - 기동 75초, 정지 60초, 부트스트랩 30초
  - 그 밖 10초

**오류 처리(스펙 10, 계획 2 이월)**
- 영구 오류는 phase Failed로 두고 다시 시도하지 않는다: `reflink-unsupported`, `insufficient-storage`, `invalid`, 그리고 복사의 `not-found`.
- `rate-limited`는 `Retry-After`가 지나기 전에는 다시 하지 않는다. 다른 이벤트가 깨워도 마찬가지다.
- `not-running`은 2초 뒤 다시 본다.
- 나머지(`busy`, `supervisor-unavailable`, 500, 무응답, 복사의 `conflict`)는 100ms에서 30초까지 늘어나는 백오프로 다시 한다.
- 감독 프로세스 `/v1/start`의 503과 409는 실패가 아니다(다시 본다). 500만 기동 실패다.

**기본값(스펙 14)**
- CRD 기본값으로 둔다: 볼륨 10Gi, compute 500m/1Gi, 브랜치 최대 10개, 복사 간격 5s, 복구 지점 20개·24h, PostgreSQL `"17"`, state `Running`, source `ParentLatest`.

**테스트**
- API 서버가 필요한 테스트는 `KUBEBUILDER_ASSETS`가 없으면 건너뛴다. `make test-envtest`가 이를 설정해 전부 돌린다.
- Mac에서 `make test`(envtest 없이)와 `make test-envtest`가 모두 통과해야 한다.
- 한 패키지만 envtest로 돌릴 때는 다음 명령을 먼저 실행한다:
  `export KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2 use 1.37.0 --bin-dir "$PWD/bin/envtest" -p path)`
- PostgreSQL·XFS 테스트는 이전처럼 `hack/vm-test.sh <패키지 패턴>`으로 VM에서 돈다.

**커밋**
- 모든 커밋 메시지 끝에 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`를 붙인다(`-m`을 하나 더).
- `spikes/`와 `bin/`은 커밋하지 않고, 푸시하지 않는다.

## 스펙에 없는 결정

계획에서 정한 것이다. Task 11에서 스펙에 반영한다.

**툴체인과 생성 도구**
- 툴체인을 Go 1.27로 올린다(사용자 결정, 2026-10-12). Go 1.25는 지원이 끝났고, controller-runtime v0.24부터는 Go 1.26 이상을 요구한다.
- Go 1.27의 `ServeMux`는 경로 정리 리다이렉트를 301 대신 307로 준다. 이에 맞춰 계획 2의 클라이언트 테스트 하나와 문서를 고친다(Task 1).
- 생성물(DeepCopy, CRD, ClusterRole)은 controller-gen으로 만들어 커밋한다.

**리소스와 상태**
- 이름 규칙과 불변 필드(`parent`, `source`)는 CRD의 CEL 규칙으로 API 서버가 막는다. API 서버(계획 4)도 같은 검사를 한다.
- `status.lastRestore`에 이어서 진행하는 데 필요한 필드를 더한다: `source`, `fromGeneration`, `toGeneration`, `restorePointId`, `reverting`, `message`, `startedAt`, `finishedAt`.
- Branch 조건은 `DataReady`와 `Ready`다. Project에는 네 조건을 요약하는 `Ready`를 더한다.
- PVC와 Secret 두 개에는 소유자를 두지 않는다. Project 객체만 지워져도 데이터와 비밀번호는 남고, 네임스페이스를 지울 때 함께 지워진다.
- 소유 관계:
  - Project는 네트워크 정책, 쿼터, 에이전트 파드·Service, main Branch를 소유한다.
  - Branch는 자기 파드와 Service를 소유한다.
- 에이전트 Service `volume-agent`(8443)를 둔다. 스펙에는 주소만 있다.
- 브랜치 Service는 `publishNotReadyAddresses: true`다. PostgreSQL이 정지해 파드가 준비되지 않아도 감독 프로세스(8008)에 닿아야 정지 상태 확인·기동·세대 전환을 할 수 있다.
- ResourceQuota는 파드 `2 × maxBranches + 1`(브랜치, 롤백이 더하는 백업 브랜치, 에이전트), PVC 1개, 저장 용량 `storage.size`다.
- 에이전트 아웃바운드의 DNS는 `kube-system`의 53번 포트(UDP·TCP)다.

**조정 방식**
- main의 데이터(init-main)는 main의 Branch 조정이 만든다. Project 조정은 main Branch 객체만 만든다. 그래야 "데이터를 만든다"가 브랜치마다 한 곳에 있다.
- 파드는 한 번 만들고 고치지 않는다. 바꾸는 경우는 셋뿐이다:
  - 끝난(Failed·Succeeded) 파드는 지우고 다시 만든다.
  - 에이전트 인자가 바뀌면 에이전트 파드를 다시 만든다(spec-hash 주석).
  - 브랜치 파드의 `AUTOSTART`는 만들 때의 `spec.state`다.
- 매니저 캐시는 managed-by 라벨이 붙은 객체로 한정한다. 클러스터의 모든 Secret과 파드를 들고 있지 않는다.
- 롤백·리셋:
  - 리셋에서 부모가 실행 중이 아니면 기다린다.
  - main의 리셋은 실패로 끝낸다(부모가 없다).
  - 정지된 브랜치도 새 데이터가 기동하는지 확인하려고 잠시 기동했다가 다시 정지한다.
  - 같은 초에 롤백이 두 번이면 백업 브랜치 이름이 겹친다. 이때는 다음 초의 이름을 쓴다.
- main Branch 객체를 지우면 데이터는 지우지 않는다. Project 조정이 main을 다시 만든다.
- 브랜치를 지울 때 Project가 없거나 삭제 중이거나, reflink 점검을 통과한 적이 없으면 에이전트를 부르지 않는다.

**미룬 것**
- 메트릭(스펙 11)은 계획 5로 미룬다. status의 `timings`와 `lastRestore`의 시각만 남긴다.
- `requestId`는 오퍼레이터의 롤백·리셋 로그에만 남긴다.
- 오퍼레이터 이미지와 배포 매니페스트는 계획 5다.

## Review Focus

1. **롤백·리셋 도중 한 단계가 실패하거나 오퍼레이터가 재시작하는 경우:** 저장된 단계부터 이어서 하고, 새 세대를 두 번 만들지 않아야 한다. 앞 시도가 남긴 세대는 지운다. → Task 9 `TestARestoreGoesOnAfterAFailedStep`, `TestARestoreClearsGenerationsEarlierTriesLeft`
2. **롤백한 데이터로 PostgreSQL이 기동하지 않는 경우:** 옛 세대로 되돌려 다시 기동하고, 새 세대를 지우고, 실패를 기록해야 한다. → Task 9 `TestARestoreThatDoesNotStartGoesBack`
3. **브랜치 삭제:**
   - 파드가 아직 실행 중이면 데이터를 지우지 않아야 한다.
   - 프로젝트가 통째로 지워지는 중이면 에이전트 없이도 삭제가 끝나야 한다.
   - main의 데이터는 남아야 한다.
   → Task 8 `TestDeletingABranchRemovesItsPodAndData`(가짜 에이전트는 실행 중인 브랜치의 데이터 삭제를 거부한다), `TestDeletingABranchOfAGoneProjectLeavesTheAgentAlone`, `TestDeletingMainKeepsItsData`
4. **에이전트가 429와 `Retry-After`를 돌려주는 경우:** 다른 이벤트가 조정을 깨워도 그 시간이 지나기 전에는 다시 복사하지 않아야 한다. → Task 7 `TestRateLimitedCopyComesBackAfterRetryAfter`
5. **영구 오류(reflink 미지원 볼륨, 없는 복구 지점):**
   - 프로젝트·브랜치가 Failed로 남는다.
   - 같은 호출을 되풀이하지 않는다.
   - 이유가 조건에 남는다.
   → Task 6 `TestProjectFailsWithoutReflink`, Task 7 `TestBranchFromAMissingRestorePointFails`

## 파일 구성

| 파일 | 맡는 일 |
| --- | --- |
| `go.mod`, `images/*/Dockerfile`, `hack/vm-setup.sh` | Go 1.27 툴체인 |
| `api/v1alpha1/` | Project·Branch 타입, 기본값·CEL 표지, 생성된 DeepCopy |
| `config/crd/bases/`, `config/rbac/role.yaml` | controller-gen이 만드는 CRD와 오퍼레이터 ClusterRole |
| `internal/testutil/kubetest/` | envtest API 서버 도우미(CRD 설치, 프로젝트 네임스페이스, 사용자 추가) |
| `internal/controller/names.go` | 이름·라벨·키 상수 |
| `internal/controller/project_resources.go` | 프로젝트 하위 리소스 명세 |
| `internal/controller/branch_resources.go` | 브랜치 파드·서비스 명세, 백업 브랜치 이름 |
| `internal/controller/clients.go`, `errors.go` | 에이전트·감독 프로세스 인터페이스와 HTTP 연결, 오류 분류 |
| `internal/controller/manager.go` | 캐시 범위, 재시도 간격, 매니저 권한 표지 |
| `internal/controller/project_controller.go` | Project 조정(스펙 7.1) |
| `internal/controller/branch_controller.go` | Branch 조정: 데이터, 파드·서비스, 정지·기동 |
| `internal/controller/branch_delete.go` | 브랜치 삭제(파이널라이저) |
| `internal/controller/restore.go` | 롤백·리셋 단계 진행(스펙 9.4) |
| `internal/controller/fake_test.go`, `suite_test.go` | 가짜 에이전트·감독 프로세스, envtest 위의 매니저 |
| `cmd/operator/` | 실행 파일과 그 테스트(생성된 ClusterRole만으로 동작) |
| `Makefile` | `generate`, `envtest`, `test-envtest` |

---

### Task 1: Go 1.27 툴체인

**Files:**
- Modify: `go.mod`, `images/agent/Dockerfile`, `images/postgres/Dockerfile`, `hack/vm-setup.sh`, `internal/agentclient/client.go`, `internal/agentclient/client_test.go`, `docs/superpowers/specs/2026-10-07-branching-engine-design.md`, `docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`

**Interfaces:**
- Produces: 저장소 전체가 Go 1.27로 빌드된다(`go 1.27.0`, `toolchain go1.27.2`). 뒤 작업이 쓰는 controller-runtime v0.25는 Go 1.26 이상을 요구한다.

- [ ] **Step 1: go.mod를 Go 1.27로**

Run: `go mod edit -go=1.27.0 -toolchain=go1.27.2 && go version`
Expected: `go version go1.27.2 darwin/arm64`. 처음에는 `go: downloading go1.27.2`가 먼저 나온다. `GOTOOLCHAIN=auto`라서 툴체인을 저절로 내려받는다.

- [ ] **Step 2: 실패하는 테스트 확인**

Go 1.27의 `ServeMux`는 경로의 `.`·`..` 조각을 정리하면서 메서드와 상관없이 307을 준다(Go 1.25는 301). 클라이언트는 여전히 따라가지 않지만, 301을 기대하는 테스트가 깨진다.

Run: `go test ./internal/agentclient/`
Expected: FAIL — `TestClientDoesNotFollowRedirects: default client: DeleteRestorePoint(main, ".") = agent: 307: Temporary Redirect, want a 301 APIError`

- [ ] **Step 3: 테스트와 주석을 3xx에 맞추기**

`internal/agentclient/client_test.go`에서 다음을:

```go
// The agent's mux answers a "." or ".." segment with a 301 to the cleaned path. Following it
```

다음으로 바꾼다:

```go
// The agent's mux answers a "." or ".." segment with a redirect to the cleaned path (307 since
// Go 1.27, 301 before). Following it
```

`internal/agentclient/client_test.go`에서 다음을:

```go
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusMovedPermanently {
			t.Fatalf("%s: DeleteRestorePoint(main, \".\") = %v, want a 301 APIError", name, err)
```

다음으로 바꾼다:

```go
		if !errors.As(err, &apiErr) || apiErr.StatusCode < 300 || apiErr.StatusCode > 399 {
			t.Fatalf("%s: DeleteRestorePoint(main, \".\") = %v, want a 3xx APIError", name, err)
```

`internal/agentclient/client.go`에서 다음을:

```go
// says: the agent's mux answers a "." or ".." segment with a 301 to the cleaned path, and
// following it turns a DELETE into a GET of another resource that reports success.
```

다음으로 바꾼다:

```go
// says: the agent's mux answers a "." or ".." segment with a redirect to the cleaned path, and
// following it sends the request to another resource (after a 301 from a server built with Go
// 1.25, a DELETE even became a GET that reported success).
```

- [ ] **Step 4: 이미지, VM, 문서**

`images/agent/Dockerfile`에서 다음을:

```dockerfile
FROM golang:1.25-bookworm AS build
```

다음으로 바꾼다:

```dockerfile
FROM golang:1.27-bookworm AS build
```

`images/postgres/Dockerfile`에서 다음을:

```dockerfile
FROM golang:1.25-bookworm AS build
```

다음으로 바꾼다:

```dockerfile
FROM golang:1.27-bookworm AS build
```

`hack/vm-setup.sh`에서 다음을:

```bash
GO_VERSION=${GO_VERSION:-1.25.6}
```

다음으로 바꾼다:

```bash
GO_VERSION=${GO_VERSION:-1.27.2}
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
(에이전트의 라우터가 직접 내는 301·404·405는 제외)
```

다음으로 바꾼다:

```markdown
(에이전트의 라우터가 직접 내는 3xx·404·405는 제외)
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
- Go 1.25 이상, 모듈 하나.
```

다음으로 바꾼다:

```markdown
- Go 1.27 이상(툴체인 go1.27.2), 모듈 하나.
```

`docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`에서 다음을:

```markdown
`baseURL` 끝의 `/`를 잘라내지 않아 `//v1/...` 요청이 301을 거쳐 405가 된다.
```

다음으로 바꾼다:

```markdown
`baseURL` 끝의 `/`를 잘라내지 않아 `//v1/...` 요청이 307 리다이렉트로 한 번 더 왕복한다(Go 1.25로 빌드한 서버는 301을 줘서 POST가 405가 됐다).
```

`docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`에서 다음을:

```markdown
에이전트의 라우터가 직접 낸 301(경로에
```

다음으로 바꾼다:

```markdown
에이전트의 라우터가 직접 낸 307(Go 1.27부터. 경로에
```

`docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`에서 다음을:

```markdown
- `--supervisor-url` 끝에 `/`가 있으면 실행 중인 브랜치에서의 복사가 모두 실패한다(POST가 301을 거쳐 405). `supervisorclient.New`에서 잘라낸다(계획 1 항목과 같고, 에이전트가 이것에 기대므로 먼저 한다).
```

다음으로 바꾼다:

```markdown
- `--supervisor-url` 끝에 `/`가 있으면 감독 프로세스 호출마다 307 리다이렉트로 한 번 더 왕복한다(Go 1.25에서는 POST가 405가 되어 복사가 모두 실패했다). `supervisorclient.New`에서 잘라낸다(계획 1 항목과 같다).
```

- [ ] **Step 5: 확인**

Run: `go vet ./... && make test`
Expected: 모든 패키지 `ok`

Run: `hack/vm-test.sh -count=1 ./...`
Expected: 모든 패키지 `ok`. VM의 Go(1.25.6)도 go.mod를 보고 go1.27.2를 내려받는다. 처음에는 `go: downloading go1.27.2 (linux/arm64)`가 나온다.

Run: `(docker info >/dev/null 2>&1 || colima start) && make smoke-postgres smoke-agent`
Expected: 두 스모크 테스트 모두 마지막 줄이 `PASS`다. 이미지는 `golang:1.27-bookworm`으로 다시 빌드된다. 빌드가 남기는 이름 없는 이미지를 포함해 Docker 이미지·볼륨은 지우지 않는다. 정리는 사용자가 정한다.

- [ ] **Step 6: 커밋**

```bash
git add go.mod images/agent/Dockerfile images/postgres/Dockerfile hack/vm-setup.sh internal/agentclient/client.go internal/agentclient/client_test.go docs/superpowers/specs/2026-10-07-branching-engine-design.md docs/superpowers/plans/2026-10-07-branching-engine-carryover.md
git commit -m "build: Go 1.27 툴체인(go1.27.2)으로 올리고, ServeMux의 307 리다이렉트에 테스트와 문서를 맞춤" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 2: Project·Branch 리소스 타입과 CRD

**Files:**
- Create: `api/v1alpha1/groupversion_info.go`, `api/v1alpha1/project_types.go`, `api/v1alpha1/branch_types.go`, `internal/testutil/kubetest/kubetest.go`
- Generate: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/consonance.io_projects.yaml`, `config/crd/bases/consonance.io_branches.yaml`
- Modify: `Makefile`, `go.mod`, `go.sum`
- Test: `api/v1alpha1/crd_test.go`

**Interfaces:**
- Consumes: 없음
- Produces:
  - 패키지 `consonance/api/v1alpha1`:
    - `GroupVersion`, `AddToScheme`
    - 타입 `Project`, `ProjectList`, `ProjectSpec`, `StorageSpec`, `ComputeSpec{CPU, Memory resource.Quantity}`, `ProjectLimits{MaxBranches int32; MinCopyInterval metav1.Duration}`, `RestorePointPolicy{MaxPerBranch int32; MaxAge metav1.Duration}`, `ProjectStatus{Phase, ObservedGeneration, Volume VolumeStatus{PVCName, CapacityBytes, UsedBytes}, Conditions}`
    - 타입 `Branch`, `BranchList`, `BranchSpec{Parent string; Source BranchSource; Protected bool; State DesiredState; Compute *ComputeSpec; Restore *RestoreRequest}`, `BranchSource{Type SourceType; RestorePointID string}`, `RestoreRequest{RequestID string; Source BranchSource}`, `BranchStatus{Phase; ObservedGeneration; DataGeneration int32; CreatedFrom *CreatedFrom; Endpoint *Endpoint; LastRestore *RestoreStatus; Timings Timings; Conditions}`, `RestoreStatus{RequestID, Phase, BackupBranch, RestorePointID, Source, FromGeneration, ToGeneration int32, Reverting, Message, StartedAt, FinishedAt}`
    - 상수:
      - `MainBranch`, `SourceParentLatest`/`SourceRestorePoint`, `StateRunning`/`StateStopped`
      - Project 단계 `Project{Pending,Provisioning,Ready,Failed}`, Branch 단계 `Branch{Creating,DataReady,Starting,Running,Stopping,Stopped,Restoring,Failed,Deleting}`, 복원 단계 `Restore{Preparing,Stopping,Activating,Starting,Detaching,Done,Failed}`
      - 조건 `Condition{VolumeReady,AgentReady,ReflinkSupported,MainReady,Ready,DataReady}`
  - 패키지 `consonance/internal/testutil/kubetest`:
    - `Env{Config, Scheme, Client}`
    - `Main(m, &env) int`, `Start() (*Env, error)`, `(*Env).Stop()`, `Need(t, env)`
    - `Scheme()`, `RepoRoot()`
    - `(*Env).Namespace(t) string`(`cns-<12자>`를 만든다)
    - `(*Env).AddUser(name, groups...) ([]byte, error)`(kubeconfig)
  - Makefile 대상 `generate`, `envtest`, `test-envtest`.

- [ ] **Step 1: controller-runtime 추가와 Makefile 대상**

Run: `go get sigs.k8s.io/controller-runtime@v0.25.2`
Expected: go.mod에 `sigs.k8s.io/controller-runtime v0.25.2`와 `k8s.io/api`·`k8s.io/apimachinery`·`k8s.io/client-go`(v0.37.x)가 들어간다.

`Makefile`에서 다음을:

```make
.PHONY: test test-vm test-xfs vm-setup images image-postgres smoke-postgres image-agent smoke-agent
```

다음으로 바꾼다:

```make
.PHONY: test test-vm test-xfs vm-setup images image-postgres smoke-postgres image-agent smoke-agent generate envtest test-envtest

CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0
SETUP_ENVTEST = go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
ENVTEST_K8S_VERSION = 1.37.0
```

`Makefile`에서 다음 줄을 찾아:

```make
vm-setup: ##
```

그 바로 앞에 다음을 넣는다(넣은 내용과 찾은 줄 사이에 빈 줄을 하나 둔다):

```make
generate: ## deepcopy code and CRD manifests from api/ (controller-gen)
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases

envtest: ## download the envtest API server and etcd into bin/envtest
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest

test-envtest: envtest ## host tests with a Kubernetes API server (envtest) for the API types and the operator
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest -p path)" go test ./...
```

- [ ] **Step 2: envtest 도우미와 실패하는 테스트**

`internal/testutil/kubetest/kubetest.go`:

```go
// Package kubetest runs a Kubernetes API server (envtest: kube-apiserver and etcd, no kubelet and
// no controllers) with the Consonance CRDs for the tests of a package.
package kubetest

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"consonance/api/v1alpha1"
)

// Env is a running test API server.
type Env struct {
	Config *rest.Config
	Scheme *k8sruntime.Scheme
	Client client.Client // uncached
	env    *envtest.Environment
}

// Main starts the API server, runs the package's tests and stops it. Call it from TestMain:
//
//	var env *kubetest.Env
//	func TestMain(m *testing.M) { os.Exit(kubetest.Main(m, &env)) }
//
// Without KUBEBUILDER_ASSETS (make test-envtest sets it) it leaves *envp nil and the tests that
// call Need skip.
func Main(m *testing.M, envp **Env) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run()
	}
	e, err := Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubetest:", err)
		return 1
	}
	defer e.Stop()
	*envp = e
	return m.Run()
}

// Need skips the test when no API server runs.
func Need(t testing.TB, e *Env) {
	t.Helper()
	if e == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test-envtest")
	}
}

// Scheme returns a scheme with the Kubernetes types and the Consonance types.
func Scheme() *k8sruntime.Scheme {
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// RepoRoot is the repository root, found from this file's path.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// Start starts an API server with the CRDs of config/crd/bases. Main does it for packages that
// need nothing else before their tests run.
func Start() (*Env, error) {
	scheme := Scheme()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(RepoRoot(), "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
	}
	cfg, err := env.Start()
	if err != nil {
		return nil, fmt.Errorf("start envtest: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = env.Stop()
		return nil, err
	}
	return &Env{Config: cfg, Scheme: scheme, Client: c, env: env}, nil
}

// Stop stops the API server.
func (e *Env) Stop() {
	if err := e.env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "kubetest: stop:", err)
	}
}

// Namespace creates a project namespace cns-<12 random characters> for one test. envtest runs no
// namespace controller, so the namespace and its objects stay until the API server stops.
func (e *Env) Namespace(t testing.TB) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cns-" + randomID()}}
	if err := e.Client.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}

// AddUser adds an API server user and returns its kubeconfig (for running the operator binary
// with its own permissions).
func (e *Env) AddUser(name string, groups ...string) ([]byte, error) {
	u, err := e.env.AddUser(envtest.User{Name: name, Groups: groups}, nil)
	if err != nil {
		return nil, err
	}
	return u.KubeConfig()
}

// randomID is 12 lowercase letters and digits, like a projectId (spec 4.1).
func randomID() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}
```

`api/v1alpha1/crd_test.go`:

```go
package v1alpha1_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/testutil/kubetest"
)

var env *kubetest.Env

func TestMain(m *testing.M) { os.Exit(kubetest.Main(m, &env)) }

func TestProjectGetsTheSpecDefaults(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	ns := env.Namespace(t)
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: ns},
		Spec:       v1alpha1.ProjectSpec{DisplayName: "my-app"},
	}
	if err := env.Client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.Project
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
		t.Fatal(err)
	}
	s := got.Spec
	checks := []struct {
		name      string
		got, want string
	}{
		{"postgresVersion", s.PostgresVersion, "17"},
		{"storage.size", s.Storage.Size.String(), "10Gi"},
		{"defaultCompute.cpu", s.DefaultCompute.CPU.String(), "500m"},
		{"defaultCompute.memory", s.DefaultCompute.Memory.String(), "1Gi"},
		{"limits.maxBranches", itoa(s.Limits.MaxBranches), "10"},
		{"limits.minCopyInterval", s.Limits.MinCopyInterval.Duration.String(), (5 * time.Second).String()},
		{"restorePoints.maxPerBranch", itoa(s.RestorePoints.MaxPerBranch), "20"},
		{"restorePoints.maxAge", s.RestorePoints.MaxAge.Duration.String(), (24 * time.Hour).String()},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestProjectKeepsGivenValues(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	ns := env.Namespace(t)
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: ns},
		Spec: v1alpha1.ProjectSpec{
			DisplayName: "my-app",
			Storage:     v1alpha1.StorageSpec{Size: resource.MustParse("20Gi"), StorageClassName: "consonance-xfs"},
			Limits:      v1alpha1.ProjectLimits{MaxBranches: 3},
		},
	}
	if err := env.Client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.Project
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Storage.Size.String() != "20Gi" || got.Spec.Storage.StorageClassName != "consonance-xfs" ||
		got.Spec.Limits.MaxBranches != 3 || got.Spec.Limits.MinCopyInterval.Duration != 5*time.Second {
		t.Fatalf("spec = %+v", got.Spec)
	}
}

func TestProjectMustBeNamedProject(t *testing.T) {
	kubetest.Need(t, env)
	ns := env.Namespace(t)
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: ns},
		Spec:       v1alpha1.ProjectSpec{DisplayName: "my-app"},
	}
	if err := env.Client.Create(context.Background(), p); !apierrors.IsInvalid(err) {
		t.Fatalf("create Project named other: %v, want Invalid", err)
	}
}

func TestBranchGetsTheSpecDefaults(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	ns := env.Namespace(t)
	b := branch(ns, "dev", "main")
	if err := env.Client.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.Branch
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(b), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Source.Type != v1alpha1.SourceParentLatest || got.Spec.State != v1alpha1.StateRunning || got.Spec.Protected {
		t.Fatalf("spec = %+v", got.Spec)
	}
}

func TestBranchValidation(t *testing.T) {
	kubetest.Need(t, env)
	ns := env.Namespace(t)
	rp := v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-ab12"}
	cases := []struct {
		name   string
		branch *v1alpha1.Branch
		valid  bool
	}{
		{"main without a parent", branch(ns, "main", ""), true},
		{"main with a parent", branch(ns, "main", "dev"), false},
		{"another branch without a parent", branch(ns, "dev", ""), false},
		{"40 characters", branch(ns, strings.Repeat("a", 40), "main"), true},
		{"41 characters", branch(ns, strings.Repeat("a", 41), "main"), false},
		{"a dot", branch(ns, "a.b", "main"), false},
		{"a parent with a capital", branch(ns, "cap", "Main"), false},
		{"from a restore point", with(branch(ns, "from-rp", "main"), func(b *v1alpha1.Branch) { b.Spec.Source = rp }), true},
		{"a restore point source without an id", with(branch(ns, "no-id", "main"), func(b *v1alpha1.Branch) {
			b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint}
		}), false},
		{"a malformed restore point id", with(branch(ns, "bad-id", "main"), func(b *v1alpha1.Branch) {
			b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-1"}
		}), false},
		{"an unknown state", with(branch(ns, "paused", "main"), func(b *v1alpha1.Branch) { b.Spec.State = "Paused" }), false},
		{"a restore without a requestId", with(branch(ns, "no-request", "main"), func(b *v1alpha1.Branch) {
			b.Spec.Restore = &v1alpha1.RestoreRequest{Source: rp}
		}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := env.Client.Create(context.Background(), c.branch)
			if c.valid && err != nil {
				t.Fatalf("create: %v, want success", err)
			}
			if !c.valid && !apierrors.IsInvalid(err) {
				t.Fatalf("create: %v, want Invalid", err)
			}
		})
	}
}

func TestBranchParentAndSourceAreImmutable(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	ns := env.Namespace(t)
	b := branch(ns, "dev", "main")
	if err := env.Client.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	update := func(change func(*v1alpha1.Branch)) error {
		var cur v1alpha1.Branch
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(b), &cur); err != nil {
			t.Fatal(err)
		}
		change(&cur)
		return env.Client.Update(ctx, &cur)
	}
	if err := update(func(b *v1alpha1.Branch) { b.Spec.Parent = "other" }); !apierrors.IsInvalid(err) {
		t.Fatalf("change parent: %v, want Invalid", err)
	}
	if err := update(func(b *v1alpha1.Branch) {
		b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-ab12"}
	}); !apierrors.IsInvalid(err) {
		t.Fatalf("change source: %v, want Invalid", err)
	}
	if err := update(func(b *v1alpha1.Branch) { b.Spec.State = v1alpha1.StateStopped }); err != nil {
		t.Fatalf("change state: %v", err)
	}
}

func branch(ns, name, parent string) *v1alpha1.Branch {
	return &v1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.BranchSpec{Parent: parent},
	}
}

func with(b *v1alpha1.Branch, change func(*v1alpha1.Branch)) *v1alpha1.Branch {
	change(b)
	return b
}

func itoa(n int32) string { return resource.NewQuantity(int64(n), resource.DecimalSI).String() }
```

Run: `go test ./api/...`
Expected: FAIL — 빌드 실패(`no non-test Go files` 또는 `undefined: v1alpha1.Project`)

- [ ] **Step 3: 타입**

`api/v1alpha1/groupversion_info.go`:

```go
// Package v1alpha1 holds the Project and Branch custom resources (spec 4).
//
// +kubebuilder:object:generate=true
// +groupName=consonance.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is consonance.io/v1alpha1, a working name until the product is named (spec 4).
	GroupVersion = schema.GroupVersion{Group: "consonance.io", Version: "v1alpha1"}

	// SchemeBuilder registers the types of this group.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types of this group to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
```

`api/v1alpha1/project_types.go`:

```go
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProjectSpec is the project the API server asks for (spec 4.2). Omitted fields get the spec 14
// defaults from the API server.
type ProjectSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	DisplayName string `json:"displayName"`

	// +optional
	// +kubebuilder:default="17"
	// +kubebuilder:validation:Enum="17"
	PostgresVersion string `json:"postgresVersion,omitempty"`

	// +optional
	// +kubebuilder:default={}
	Storage StorageSpec `json:"storage,omitzero"`

	// +optional
	// +kubebuilder:default={cpu: "500m", memory: "1Gi"}
	DefaultCompute ComputeSpec `json:"defaultCompute,omitzero"`

	// +optional
	// +kubebuilder:default={}
	Limits ProjectLimits `json:"limits,omitzero"`

	// +optional
	// +kubebuilder:default={}
	RestorePoints RestorePointPolicy `json:"restorePoints,omitzero"`
}

// StorageSpec is the project volume. It must be XFS with reflink (spec 2).
type StorageSpec struct {
	// +optional
	// +kubebuilder:default="10Gi"
	Size resource.Quantity `json:"size,omitzero"`

	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
}

// ComputeSpec is the CPU and memory of a branch pod: its requests and its limits.
type ComputeSpec struct {
	CPU    resource.Quantity `json:"cpu"`
	Memory resource.Quantity `json:"memory"`
}

// ProjectLimits are the project's limits (spec 4.2).
type ProjectLimits struct {
	// MaxBranches counts main. The API server enforces it.
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	MaxBranches int32 `json:"maxBranches,omitempty"`

	// MinCopyInterval is the shortest time between two copies of one source branch.
	// +optional
	// +kubebuilder:default="5s"
	MinCopyInterval metav1.Duration `json:"minCopyInterval,omitzero"`
}

// RestorePointPolicy is the restore point retention (spec 4.2).
type RestorePointPolicy struct {
	// +optional
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	MaxPerBranch int32 `json:"maxPerBranch,omitempty"`

	// +optional
	// +kubebuilder:default="24h"
	MaxAge metav1.Duration `json:"maxAge,omitzero"`
}

// ProjectPhase is where a project is in its life (spec 4.2).
type ProjectPhase string

const (
	ProjectPending      ProjectPhase = "Pending"
	ProjectProvisioning ProjectPhase = "Provisioning"
	ProjectReady        ProjectPhase = "Ready"
	ProjectFailed       ProjectPhase = "Failed"
)

// Project condition types (spec 4.2), and Ready, which sums them up.
const (
	ConditionVolumeReady      = "VolumeReady"
	ConditionAgentReady       = "AgentReady"
	ConditionReflinkSupported = "ReflinkSupported"
	ConditionMainReady        = "MainReady"
	ConditionReady            = "Ready"
)

// ProjectStatus is what the operator reports.
type ProjectStatus struct {
	// +optional
	Phase ProjectPhase `json:"phase,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Volume VolumeStatus `json:"volume,omitzero"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VolumeStatus is the project volume as the agent last saw it.
type VolumeStatus struct {
	// +optional
	PVCName string `json:"pvcName,omitempty"`
	// +optional
	CapacityBytes int64 `json:"capacityBytes,omitempty"`
	// +optional
	UsedBytes int64 `json:"usedBytes,omitempty"`
}

// Project is one project: its namespace cns-<projectId> holds this Project, named project, and
// its branches (spec 4.1).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'project'",message="the Project of a project namespace is named project"
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ProjectSpec `json:"spec"`

	// +optional
	Status ProjectStatus `json:"status,omitzero"`
}

// ProjectList is a list of projects.
//
// +kubebuilder:object:root=true
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Project{}, &ProjectList{})
}
```

`api/v1alpha1/branch_types.go`:

```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MainBranch is the branch every project starts with. It has no parent.
const MainBranch = "main"

// SourceType says what a branch's data is copied from (spec 4.3).
type SourceType string

const (
	// SourceParentLatest is the parent's running database, copied in backup mode.
	SourceParentLatest SourceType = "ParentLatest"
	// SourceRestorePoint is a restore point.
	SourceRestorePoint SourceType = "RestorePoint"
)

// BranchSource is where a branch's data, or the data of a rollback or reset, comes from.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'RestorePoint' || has(self.restorePointId)",message="a RestorePoint source needs restorePointId"
type BranchSource struct {
	// +optional
	// +kubebuilder:default=ParentLatest
	// +kubebuilder:validation:Enum=ParentLatest;RestorePoint
	Type SourceType `json:"type,omitempty"`

	// +optional
	// +kubebuilder:validation:Pattern=`^rp-[0-9]{8}T[0-9]{6}Z-[a-z0-9]{4}$`
	RestorePointID string `json:"restorePointId,omitempty"`
}

// DesiredState is the PostgreSQL process state a branch should be in (spec 4.3).
type DesiredState string

const (
	StateRunning DesiredState = "Running"
	StateStopped DesiredState = "Stopped"
)

// BranchSpec is the branch the API server asks for (spec 4.3).
//
// +kubebuilder:validation:XValidation:rule="has(self.parent) == has(oldSelf.parent) && (!has(self.parent) || self.parent == oldSelf.parent)",message="parent is immutable"
// +kubebuilder:validation:XValidation:rule="self.source == oldSelf.source",message="source is immutable"
type BranchSpec struct {
	// Parent is the branch this one was copied from. main has none.
	// +optional
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Parent string `json:"parent,omitempty"`

	// +optional
	// +kubebuilder:default={type: ParentLatest}
	Source BranchSource `json:"source,omitzero"`

	// Protected marks main and other branches that need approval to change (spec 1). The API
	// server enforces it.
	// +optional
	Protected bool `json:"protected,omitempty"`

	// +optional
	// +kubebuilder:default=Running
	// +kubebuilder:validation:Enum=Running;Stopped
	State DesiredState `json:"state,omitempty"`

	// Compute overrides the project's defaultCompute.
	// +optional
	Compute *ComputeSpec `json:"compute,omitempty"`

	// Restore asks for a rollback (source RestorePoint) or a reset (source ParentLatest). The
	// operator carries out each requestId once (spec 9.4).
	// +optional
	Restore *RestoreRequest `json:"restore,omitempty"`
}

// RestoreRequest is one rollback or reset.
type RestoreRequest struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	RequestID string `json:"requestId"`

	Source BranchSource `json:"source"`
}

// BranchPhase is where a branch is (spec 4.3).
type BranchPhase string

const (
	BranchCreating  BranchPhase = "Creating"
	BranchDataReady BranchPhase = "DataReady"
	BranchStarting  BranchPhase = "Starting"
	BranchRunning   BranchPhase = "Running"
	BranchStopping  BranchPhase = "Stopping"
	BranchStopped   BranchPhase = "Stopped"
	BranchRestoring BranchPhase = "Restoring"
	BranchFailed    BranchPhase = "Failed"
	BranchDeleting  BranchPhase = "Deleting"
)

// Branch condition types: DataReady once the branch has data, and Ready, which says whether
// the branch is in its desired state and why not.
const (
	ConditionDataReady = "DataReady"
)

// RestorePhase is the step a rollback or reset is at (spec 9.4).
type RestorePhase string

const (
	RestorePreparing  RestorePhase = "Preparing"
	RestoreStopping   RestorePhase = "Stopping"
	RestoreActivating RestorePhase = "Activating"
	RestoreStarting   RestorePhase = "Starting"
	RestoreDetaching  RestorePhase = "Detaching"
	RestoreDone       RestorePhase = "Done"
	RestoreFailed     RestorePhase = "Failed"
)

// BranchStatus is what the operator reports (spec 4.3).
type BranchStatus struct {
	// +optional
	Phase BranchPhase `json:"phase,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// DataGeneration is the data-N directory the branch runs on.
	// +optional
	DataGeneration int32 `json:"dataGeneration,omitempty"`

	// +optional
	CreatedFrom *CreatedFrom `json:"createdFrom,omitempty"`

	// +optional
	Endpoint *Endpoint `json:"endpoint,omitempty"`

	// +optional
	LastRestore *RestoreStatus `json:"lastRestore,omitempty"`

	// +optional
	Timings Timings `json:"timings,omitzero"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// CreatedFrom is where a branch's data came from.
type CreatedFrom struct {
	// +optional
	Branch string `json:"branch,omitempty"`
	// +optional
	RestorePointID string `json:"restorePointId,omitempty"`
	// BackupStartLSN is the start of the backup the copy was made in. It is empty when the
	// copy came from a restore point, or when a retried request found the data made already.
	// +optional
	BackupStartLSN string `json:"backupStartLSN,omitempty"`
}

// Endpoint is where clients reach the branch's PostgreSQL.
type Endpoint struct {
	Host string `json:"host"`
	Port int32  `json:"port"`
}

// RestoreStatus is the last rollback or reset (spec 4.3). Source, the generations, Reverting and
// the times are not in the spec: the operator keeps them to resume after a restart (spec 10).
type RestoreStatus struct {
	// +optional
	RequestID string `json:"requestId,omitempty"`
	// +optional
	Phase RestorePhase `json:"phase,omitempty"`
	// BackupBranch holds the generation a rollback replaced.
	// +optional
	BackupBranch string `json:"backupBranch,omitempty"`
	// RestorePointID holds the generation a reset replaced.
	// +optional
	RestorePointID string `json:"restorePointId,omitempty"`
	// +optional
	Source BranchSource `json:"source,omitzero"`
	// FromGeneration is the generation the branch ran before.
	// +optional
	FromGeneration int32 `json:"fromGeneration,omitempty"`
	// ToGeneration is the new generation, once made.
	// +optional
	ToGeneration int32 `json:"toGeneration,omitempty"`
	// Reverting is set when the new generation did not start and the branch goes back to
	// FromGeneration.
	// +optional
	Reverting bool `json:"reverting,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// Timings are first-time marks of a branch's life.
type Timings struct {
	// +optional
	DataReadyAt *metav1.Time `json:"dataReadyAt,omitempty"`
	// +optional
	RunningAt *metav1.Time `json:"runningAt,omitempty"`
}

// Branch is one branch of a project: a PostgreSQL data directory on the project volume and a
// pod that runs it (spec 4.3).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Parent",type=string,JSONPath=`.spec.parent`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 40 && self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="a branch name is a DNS-1123 label of at most 40 characters"
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'main' ? !has(self.spec.parent) : has(self.spec.parent)",message="main has no parent and every other branch has one"
type Branch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BranchSpec `json:"spec"`

	// +optional
	Status BranchStatus `json:"status,omitzero"`
}

// BranchList is a list of branches.
//
// +kubebuilder:object:root=true
type BranchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Branch `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Branch{}, &BranchList{})
}
```

주의: 표지(`+kubebuilder:...`)는 문서 주석 안에 있다. gofmt는 문서 주석 안의 작은따옴표 두 개(`''`)를 `”`로 바꿔 CEL 규칙을 깨뜨린다. 그래서 규칙에 빈 문자열 리터럴을 쓰지 않는다(`has()`로 쓴다).

- [ ] **Step 4: 생성하고 확인**

Run: `go mod tidy && make generate && ls api/v1alpha1/zz_generated.deepcopy.go config/crd/bases/`
Expected: `zz_generated.deepcopy.go`, `consonance.io_branches.yaml`, `consonance.io_projects.yaml`

Run: `gofmt -l . && go vet ./... && go test ./api/...`
Expected: gofmt 출력 없음, `ok`. 테스트는 envtest가 없어 건너뛴다.

Run: `make envtest && export KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2 use 1.37.0 --bin-dir "$PWD/bin/envtest" -p path) && go test -count=1 -v ./api/...`
Expected: 6개 테스트가 PASS다: `TestProjectGetsTheSpecDefaults`, `TestProjectKeepsGivenValues`, `TestProjectMustBeNamedProject`, `TestBranchGetsTheSpecDefaults`, `TestBranchValidation`(하위 테스트 12개), `TestBranchParentAndSourceAreImmutable`.

- [ ] **Step 5: 커밋**

```bash
git add Makefile go.mod go.sum api/ config/crd/ internal/testutil/kubetest/
git commit -m "feat(api): Project·Branch 리소스 타입과 CRD(기본값·CEL 검증), envtest 도우미" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 3: 프로젝트 하위 리소스 명세

**Files:**
- Create: `internal/controller/names.go`, `internal/controller/project_resources.go`
- Test: `internal/controller/project_resources_test.go`

**Interfaces:**
- Consumes: `v1alpha1` 타입(Task 2), `agentapi.DefaultPort`, `supervisorapi.DefaultPort`
- Produces:
  - 상수: `ProjectName`, `NamespacePrefix`, `PVCName`, `AgentName`, `DBCredentialsSecret`, `InternalTokenSecret`, `QuotaName`, `BranchFinalizer`, `TokenKey`/`UsernameKey`/`PasswordKey`/`DatabaseKey`, `TenantRole`/`TenantDatabase`, 라벨·주석 상수(`LabelManagedBy`, `ManagedBy`, `LabelComponent`, `ComponentAgent`, `ComponentBranch`, `LabelBranch`, `LabelBackupOf`, `AnnotationSpecHash`), `SystemNamespace`, `LabelAppName`, `OperatorAppName`, `APIServerAppName`
  - 함수: `BranchObjectName(branch) string`, `ProjectID(namespace) (string, bool)`, `managedLabels(component)`, `isMain(*Branch)`
  - 명세 함수(모두 순수 함수):
    - Secret: `newDBCredentialsSecret(ns)`, `newInternalTokenSecret(ns)`
    - 정책·쿼터·볼륨: `desiredNetworkPolicies(ns)`, `desiredQuota(p)`, `desiredPVC(p)`
    - 에이전트: `desiredAgentPod(p, projectID, image)`, `desiredAgentService(ns)`
    - 도우미: `restrictedPod()`, `restrictedContainer()`, `tokenVolume()`, `httpProbe(path, port, period)`, `specHash(container)`
  - 테스트 도우미(뒤 작업의 테스트도 쓴다): `testProject(ns)`, `checkRestricted(t, pod)`, `mounts(container, volume, path)`

- [ ] **Step 1: 실패하는 테스트**

`internal/controller/project_resources_test.go`:

```go
package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"consonance/api/v1alpha1"
)

// testProject is a Project with the defaults the API server fills in (spec 14).
func testProject(ns string) *v1alpha1.Project {
	return &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: ProjectName, Namespace: ns},
		Spec: v1alpha1.ProjectSpec{
			DisplayName:     "my-app",
			PostgresVersion: "17",
			Storage:         v1alpha1.StorageSpec{Size: resource.MustParse("10Gi")},
			DefaultCompute:  v1alpha1.ComputeSpec{CPU: resource.MustParse("500m"), Memory: resource.MustParse("1Gi")},
			Limits:          v1alpha1.ProjectLimits{MaxBranches: 10, MinCopyInterval: metav1.Duration{Duration: 5 * time.Second}},
			RestorePoints:   v1alpha1.RestorePointPolicy{MaxPerBranch: 20, MaxAge: metav1.Duration{Duration: 24 * time.Hour}},
		},
	}
}

func TestProjectID(t *testing.T) {
	for ns, want := range map[string]string{"cns-a1b2c3d4e5f6": "a1b2c3d4e5f6", "cns-": "", "default": ""} {
		got, ok := ProjectID(ns)
		if got != want || ok != (want != "") {
			t.Errorf("ProjectID(%q) = %q, %v", ns, got, ok)
		}
	}
}

func TestSecretsAreRandom(t *testing.T) {
	a, err := newDBCredentialsSecret("cns-x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newDBCredentialsSecret("cns-x")
	pw := a.StringData[PasswordKey]
	if len(pw) != 32 || pw == b.StringData[PasswordKey] || strings.Trim(pw, alphanumeric) != "" {
		t.Fatalf("password %q", pw)
	}
	if a.StringData[UsernameKey] != "app_owner" || a.StringData[DatabaseKey] != "app" || a.Name != "db-credentials" {
		t.Fatalf("secret %+v", a)
	}
	tok, err := newInternalTokenSecret("cns-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.StringData[TokenKey]) != 48 || tok.Name != "internal-token" || tok.Labels[LabelManagedBy] != ManagedBy {
		t.Fatalf("token secret %+v", tok)
	}
}

func TestNetworkPolicies(t *testing.T) {
	pols := desiredNetworkPolicies("cns-x")
	byName := map[string]*networkingv1.NetworkPolicy{}
	for _, p := range pols {
		byName[p.Name] = p
		if p.Namespace != "cns-x" || p.Labels[LabelManagedBy] != ManagedBy {
			t.Errorf("%s: namespace %q labels %v", p.Name, p.Namespace, p.Labels)
		}
	}
	deny := byName["default-deny-ingress"]
	if deny == nil || len(deny.Spec.PodSelector.MatchLabels) != 0 || len(deny.Spec.Ingress) != 0 {
		t.Fatalf("default deny: %+v", deny)
	}
	branchIn := byName["branch-ingress"]
	if branchIn == nil || len(branchIn.Spec.Ingress) != 2 {
		t.Fatalf("branch ingress: %+v", branchIn)
	}
	if got := ruleFor(branchIn, 5432); got == nil || len(got.From) != 2 {
		t.Fatalf("5432 rule: %+v", got)
	}
	sup := ruleFor(branchIn, 8008)
	if sup == nil || !admitsApp(sup.From, OperatorAppName) || admitsApp(sup.From, APIServerAppName) {
		t.Fatalf("8008 rule: %+v", sup)
	}
	agentIn := ruleFor(byName["agent-ingress"], 8443)
	if agentIn == nil || !admitsApp(agentIn.From, OperatorAppName) || !admitsApp(agentIn.From, APIServerAppName) {
		t.Fatalf("8443 rule: %+v", agentIn)
	}
	branchOut := byName["branch-egress"]
	if branchOut == nil || !slices.Contains(branchOut.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) || len(branchOut.Spec.Egress) != 0 {
		t.Fatalf("branch egress must deny all: %+v", branchOut)
	}
	if agentOut := byName["agent-egress"]; agentOut == nil || len(agentOut.Spec.Egress) != 2 {
		t.Fatalf("agent egress: %+v", agentOut)
	}
}

func ruleFor(p *networkingv1.NetworkPolicy, port int32) *networkingv1.NetworkPolicyIngressRule {
	for i, r := range p.Spec.Ingress {
		for _, pp := range r.Ports {
			if pp.Port.IntVal == port {
				return &p.Spec.Ingress[i]
			}
		}
	}
	return nil
}

// admitsApp reports whether a peer admits pods named app in consonance-system.
func admitsApp(peers []networkingv1.NetworkPolicyPeer, app string) bool {
	for _, peer := range peers {
		if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != SystemNamespace || peer.PodSelector == nil {
			continue
		}
		for _, e := range peer.PodSelector.MatchExpressions {
			if e.Key == LabelAppName && slices.Contains(e.Values, app) {
				return true
			}
		}
	}
	return false
}

func TestQuotaAndPVC(t *testing.T) {
	p := testProject("cns-x")
	q := desiredQuota(p)
	if got := q.Spec.Hard[corev1.ResourcePods]; got.Value() != 21 {
		t.Errorf("pods quota %s, want 21 (10 branches, 10 backups, the agent)", got.String())
	}
	if got := q.Spec.Hard[corev1.ResourceRequestsStorage]; got.String() != "10Gi" {
		t.Errorf("storage quota %s", got.String())
	}
	pvc := desiredPVC(p)
	if pvc.Name != "project-volume" || pvc.Spec.StorageClassName != nil || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("pvc %+v", pvc.Spec)
	}
	p.Spec.Storage.StorageClassName = "consonance-xfs"
	if sc := desiredPVC(p).Spec.StorageClassName; sc == nil || *sc != "consonance-xfs" {
		t.Fatalf("storage class %v", sc)
	}
}

func TestAgentPod(t *testing.T) {
	p := testProject("cns-a1b2c3d4e5f6")
	pod := desiredAgentPod(p, "a1b2c3d4e5f6", "consonance/agent:dev")
	c := pod.Spec.Containers[0]
	wantArgs := []string{"--project-id=a1b2c3d4e5f6", "--min-copy-interval=5s", "--restore-points-per-branch=20", "--restore-point-max-age=24h0m0s"}
	if !slices.Equal(c.Args, wantArgs) {
		t.Errorf("args %q, want %q", c.Args, wantArgs)
	}
	if pod.Name != "volume-agent" || pod.Labels[LabelComponent] != ComponentAgent || c.Image != "consonance/agent:dev" {
		t.Errorf("pod %s labels %v image %s", pod.Name, pod.Labels, c.Image)
	}
	checkRestricted(t, pod)
	sc := pod.Spec.SecurityContext
	if sc.FSGroup == nil || *sc.FSGroup != 999 || sc.FSGroupChangePolicy == nil || *sc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("fsGroup %v policy %v", sc.FSGroup, sc.FSGroupChangePolicy)
	}
	if mem := c.Resources.Limits[corev1.ResourceMemory]; mem.String() != "128Mi" {
		t.Errorf("memory limit %s", mem.String())
	}
	if !mounts(c, "volume", "/volume") || !mounts(c, "token", "/etc/consonance/token") {
		t.Errorf("mounts %+v", c.VolumeMounts)
	}
	if *pod.Spec.TerminationGracePeriodSeconds < 45 {
		t.Errorf("grace %d", *pod.Spec.TerminationGracePeriodSeconds)
	}

	// A setting change changes the hash, so the operator replaces the pod.
	h := pod.Annotations[AnnotationSpecHash]
	p.Spec.Limits.MinCopyInterval.Duration = time.Minute
	if h2 := desiredAgentPod(p, "a1b2c3d4e5f6", "consonance/agent:dev").Annotations[AnnotationSpecHash]; h2 == h || h == "" {
		t.Errorf("hash %q then %q", h, h2)
	}
	if svc := desiredAgentService("cns-x"); svc.Spec.Ports[0].Port != 8443 || svc.Spec.Selector[LabelComponent] != ComponentAgent {
		t.Errorf("service %+v", svc.Spec)
	}
}

// checkRestricted checks the Pod Security Standard restricted settings (spec 7.5).
func checkRestricted(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	sc := pod.Spec.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || *sc.RunAsUser != 999 || *sc.RunAsGroup != 999 ||
		sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context %+v", sc)
	}
	for _, c := range pod.Spec.Containers {
		s := c.SecurityContext
		if s == nil || s.AllowPrivilegeEscalation == nil || *s.AllowPrivilegeEscalation || s.Capabilities == nil ||
			!slices.Equal(s.Capabilities.Drop, []corev1.Capability{"ALL"}) {
			t.Errorf("container %s security context %+v", c.Name, s)
		}
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("service account token is mounted")
	}
}

func mounts(c corev1.Container, volume, path string) bool {
	return slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == volume && m.MountPath == path })
}
```

Run: `go test ./internal/controller/`
Expected: FAIL — 빌드 실패(`no non-test Go files` 또는 `undefined: ProjectName`)

- [ ] **Step 2: 구현**

`internal/controller/names.go`:

```go
// Package controller reconciles Projects and Branches (spec 7.1): it makes each project's
// secrets, network policies, quota, volume and agent pod, and each branch's data, pod and
// service, and drives starts, stops, rollbacks, resets and deletions through the volume agent
// and the branch supervisors.
package controller

import (
	"strings"

	"consonance/api/v1alpha1"
)

// Names of the objects in a project namespace (spec 4.1, 7.1).
const (
	ProjectName         = "project"        // the one Project of a project namespace
	NamespacePrefix     = "cns-"           // a project namespace is cns-<projectId>
	PVCName             = "project-volume" // the project volume
	AgentName           = "volume-agent"   // the agent pod and its service
	DBCredentialsSecret = "db-credentials" // app_owner's password, for main's bootstrap
	InternalTokenSecret = "internal-token" // the token of the agent and supervisor APIs
	QuotaName           = "project-quota"
	BranchFinalizer     = "consonance.io/branch-cleanup"
)

// Secret keys.
const (
	TokenKey    = "token"    // internal-token
	UsernameKey = "username" // db-credentials
	PasswordKey = "password" // db-credentials
	DatabaseKey = "database" // db-credentials
)

// The tenant role and database that main's supervisor creates (pgctl.TenantRole and
// pgctl.TenantDatabase, spec 4.5).
const (
	TenantRole     = "app_owner"
	TenantDatabase = "app"
)

// Labels and annotations.
const (
	LabelManagedBy     = "app.kubernetes.io/managed-by"
	ManagedBy          = "consonance-operator" // on every object the operator makes
	LabelComponent     = "app.kubernetes.io/component"
	ComponentAgent     = "volume-agent"
	ComponentBranch    = "branch"
	LabelBranch        = "consonance.io/branch"    // a branch pod or service: its branch
	LabelBackupOf      = "consonance.io/backup-of" // a backup branch: the branch rolled back
	AnnotationSpecHash = "consonance.io/spec-hash" // the agent pod: the hash of its container spec
)

// The system namespace and the pod labels the network policies admit from it (spec 8). The
// operator and API server deployments must carry these labels (plan 5).
const (
	SystemNamespace  = "consonance-system"
	LabelAppName     = "app.kubernetes.io/name"
	OperatorAppName  = "consonance-operator"
	APIServerAppName = "consonance-apiserver"
)

// BranchObjectName is the name of a branch's pod and service: br-<branch>.
func BranchObjectName(branch string) string { return "br-" + branch }

// ProjectID returns the project ID of a project namespace cns-<projectId>.
func ProjectID(namespace string) (string, bool) {
	id, ok := strings.CutPrefix(namespace, NamespacePrefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// managedLabels are the labels of every object the operator makes.
func managedLabels(component string) map[string]string {
	return map[string]string{LabelManagedBy: ManagedBy, LabelComponent: component}
}

// isMain reports whether b is the project's main branch.
func isMain(b *v1alpha1.Branch) bool { return b.Name == v1alpha1.MainBranch }
```

`internal/controller/project_resources.go`:

```go
package controller

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

const (
	componentProject = "project"
	uid              = int64(999) // the postgres user of the images (spec 7.5)
	// terminationGrace covers the supervisor's 30s fast shutdown plus 10s after SIGQUIT, and
	// the agent's 25s drain (carry-over from plans 1 and 2).
	terminationGrace = int64(45)
	tokenMountDir    = "/etc/consonance/token" // the agent and supervisors read <dir>/token
)

// randomString returns n characters from the alphabet, read from crypto/rand.
func randomString(n int, alphabet string) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

const alphanumeric = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// newDBCredentialsSecret is app_owner's login: a 32-character random password (spec 4.5). It is
// made once and never changed: main's database keeps the password it was bootstrapped with.
func newDBCredentialsSecret(ns string) (*corev1.Secret, error) {
	pw, err := randomString(32, alphanumeric)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: DBCredentialsSecret, Namespace: ns, Labels: managedLabels(componentProject)},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{UsernameKey: TenantRole, PasswordKey: pw, DatabaseKey: TenantDatabase},
	}, nil
}

// newInternalTokenSecret is the project's token for the agent and supervisor APIs. It is made
// once: the pods read it when they start.
func newInternalTokenSecret(ns string) (*corev1.Secret, error) {
	tok, err := randomString(48, alphanumeric)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: InternalTokenSecret, Namespace: ns, Labels: managedLabels(componentProject)},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{TokenKey: tok},
	}, nil
}

// desiredNetworkPolicies are spec 8's rules for a project namespace.
func desiredNetworkPolicies(ns string) []*networkingv1.NetworkPolicy {
	system := &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": SystemNamespace}}
	fromSystemApps := func(apps ...string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{
			NamespaceSelector: system,
			PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: LabelAppName, Operator: metav1.LabelSelectorOpIn, Values: apps},
			}},
		}
	}
	agent := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: ComponentAgent}}}
	branchPods := metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: ComponentBranch}}
	agentPods := metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: ComponentAgent}}
	port := func(p int32, proto corev1.Protocol) networkingv1.NetworkPolicyPort {
		return networkingv1.NetworkPolicyPort{Protocol: ptr.To(proto), Port: ptr.To(intstr.FromInt32(p))}
	}
	policy := func(name string, pods metav1.LabelSelector, types []networkingv1.PolicyType) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: managedLabels(componentProject)},
			Spec:       networkingv1.NetworkPolicySpec{PodSelector: pods, PolicyTypes: types},
		}
	}
	ingress := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
	egress := []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}

	defaultDeny := policy("default-deny-ingress", metav1.LabelSelector{}, ingress)

	branchIn := policy("branch-ingress", branchPods, ingress)
	branchIn.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{
		{ // PostgreSQL: the system namespace (API server, later the gateway) and the agent
			Ports: []networkingv1.NetworkPolicyPort{port(5432, corev1.ProtocolTCP)},
			From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: system}, agent},
		},
		{ // the supervisor: the operator and the agent
			Ports: []networkingv1.NetworkPolicyPort{port(int32(supervisorapi.DefaultPort), corev1.ProtocolTCP)},
			From:  []networkingv1.NetworkPolicyPeer{fromSystemApps(OperatorAppName), agent},
		},
	}

	agentIn := policy("agent-ingress", agentPods, ingress)
	agentIn.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
		Ports: []networkingv1.NetworkPolicyPort{port(int32(agentapi.DefaultPort), corev1.ProtocolTCP)},
		From:  []networkingv1.NetworkPolicyPeer{fromSystemApps(OperatorAppName, APIServerAppName)},
	}}

	// PostgreSQL has no reason to reach out (spec 8): no egress rule allows nothing.
	branchOut := policy("branch-egress", branchPods, egress)

	agentOut := policy("agent-egress", agentPods, egress)
	agentOut.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{
		{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}}, // this namespace
		{ // DNS
			To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
			}}},
			Ports: []networkingv1.NetworkPolicyPort{port(53, corev1.ProtocolUDP), port(53, corev1.ProtocolTCP)},
		},
	}
	return []*networkingv1.NetworkPolicy{defaultDeny, branchIn, agentIn, branchOut, agentOut}
}

// desiredQuota caps what a project can hold: one volume of the project's size, and pods for
// maxBranches branches, as many backup branches (a rollback adds one beyond the API's count)
// and the agent.
func desiredQuota(p *v1alpha1.Project) *corev1.ResourceQuota {
	pods := 2*int64(p.Spec.Limits.MaxBranches) + 1
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: QuotaName, Namespace: p.Namespace, Labels: managedLabels(componentProject)},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourcePods:                   *resource.NewQuantity(pods, resource.DecimalSI),
			corev1.ResourcePersistentVolumeClaims: *resource.NewQuantity(1, resource.DecimalSI),
			corev1.ResourceRequestsStorage:        p.Spec.Storage.Size,
		}},
	}
}

// desiredPVC is the project volume (spec 7.1 step 2).
func desiredPVC(p *v1alpha1.Project) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: PVCName, Namespace: p.Namespace, Labels: managedLabels(componentProject)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: p.Spec.Storage.Size,
			}},
		},
	}
	if p.Spec.Storage.StorageClassName != "" {
		pvc.Spec.StorageClassName = ptr.To(p.Spec.Storage.StorageClassName)
	}
	return pvc
}

// restrictedPod and restrictedContainer meet Pod Security Standard restricted (spec 7.5).
func restrictedPod() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		RunAsUser:      ptr.To(uid),
		RunAsGroup:     ptr.To(uid),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func restrictedContainer() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// tokenVolume mounts internal-token, which the agent and supervisors read at tokenMountDir/token.
func tokenVolume() (corev1.Volume, corev1.VolumeMount) {
	return corev1.Volume{
			Name:         "token",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: InternalTokenSecret}},
		}, corev1.VolumeMount{
			Name: "token", MountPath: tokenMountDir, ReadOnly: true,
		}
}

func httpProbe(path string, port int, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt(port)}},
		PeriodSeconds: period, TimeoutSeconds: 2, FailureThreshold: 3,
	}
}

// agentArgs are the agent's settings from the project spec (spec 14 defaults come from the CRD).
func agentArgs(p *v1alpha1.Project, projectID string) []string {
	return []string{
		"--project-id=" + projectID,
		"--min-copy-interval=" + p.Spec.Limits.MinCopyInterval.Duration.String(),
		"--restore-points-per-branch=" + strconv.Itoa(int(p.Spec.RestorePoints.MaxPerBranch)),
		"--restore-point-max-age=" + p.Spec.RestorePoints.MaxAge.Duration.String(),
	}
}

// desiredAgentPod is the volume agent (spec 7.1 step 3, 7.2, 7.5). Its spec-hash annotation
// changes with the image and the settings, so the operator replaces a pod whose settings are old:
// the agent reads them only when it starts.
func desiredAgentPod(p *v1alpha1.Project, projectID, image string) *corev1.Pod {
	tokenVol, tokenMount := tokenVolume()
	sc := restrictedPod()
	sc.FSGroup = ptr.To(uid)
	sc.FSGroupChangePolicy = ptr.To(corev1.FSGroupChangeOnRootMismatch) // no recursive chown of a large volume
	res := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}
	c := corev1.Container{
		Name:            "agent",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            agentArgs(p, projectID),
		Ports:           []corev1.ContainerPort{{Name: "api", ContainerPort: int32(agentapi.DefaultPort)}},
		VolumeMounts:    []corev1.VolumeMount{{Name: "volume", MountPath: "/volume"}, tokenMount},
		Resources:       corev1.ResourceRequirements{Requests: res, Limits: res},
		LivenessProbe:   httpProbe("/healthz", agentapi.DefaultPort, 10),
		ReadinessProbe:  httpProbe("/healthz", agentapi.DefaultPort, 2),
		SecurityContext: restrictedContainer(),
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: AgentName, Namespace: p.Namespace,
			Labels:      managedLabels(ComponentAgent),
			Annotations: map[string]string{AnnotationSpecHash: specHash(c)},
		},
		Spec: corev1.PodSpec{
			Containers:                    []corev1.Container{c},
			Volumes:                       []corev1.Volume{{Name: "volume", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: PVCName}}}, tokenVol},
			SecurityContext:               sc,
			TerminationGracePeriodSeconds: ptr.To(terminationGrace),
			AutomountServiceAccountToken:  ptr.To(false),
		},
	}
}

// desiredAgentService gives the agent the address the operator and API server call:
// http://volume-agent.<namespace>.svc:8443.
func desiredAgentService(ns string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: AgentName, Namespace: ns, Labels: managedLabels(ComponentAgent)},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{LabelComponent: ComponentAgent},
			Ports: []corev1.ServicePort{{
				Name: "api", Port: int32(agentapi.DefaultPort), TargetPort: intstr.FromInt(agentapi.DefaultPort), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

// specHash is a short hash of a container spec.
func specHash(c corev1.Container) string {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err) // a corev1.Container always marshals
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
```

- [ ] **Step 3: 통과 확인**

Run: `gofmt -l . && go vet ./internal/controller/ && go test -count=1 -v ./internal/controller/`
Expected: 5개 테스트가 PASS다: `TestProjectID`, `TestSecretsAreRandom`, `TestNetworkPolicies`, `TestQuotaAndPVC`, `TestAgentPod`.

- [ ] **Step 4: 커밋**

```bash
git add internal/controller/
git commit -m "feat(controller): 프로젝트 하위 리소스 명세(Secret, 네트워크 정책, 쿼터, 볼륨, 에이전트 파드·서비스)" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 4: 브랜치 파드·서비스 명세

**Files:**
- Create: `internal/controller/branch_resources.go`
- Test: `internal/controller/branch_resources_test.go`

**Interfaces:**
- Consumes: Task 3의 상수와 도우미(`restrictedPod`, `restrictedContainer`, `tokenVolume`, `httpProbe`, `terminationGrace`), 테스트 도우미 `testProject`, `checkRestricted`, `mounts`
- Produces:
  - 명세 함수: `branchLabels(branch)`, `branchCompute(p, b) ComputeSpec`, `desiredBranchPod(p, b, image) *corev1.Pod`, `desiredBranchService(b) *corev1.Service`
  - 이름·주소: `branchEndpoint(b) *Endpoint`, `backupBranchName(branch, at time.Time) string`
  - 상수: `postgresPort`, `dbSecretDir`, `containerName`
  - 테스트 도우미: `testBranch(ns, name, parent)`, `envVar(container, name)`

- [ ] **Step 1: 실패하는 테스트**

`internal/controller/branch_resources_test.go`:

```go
package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
)

func testBranch(ns, name, parent string) *v1alpha1.Branch {
	return &v1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.BranchSpec{
			Parent: parent,
			Source: v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest},
			State:  v1alpha1.StateRunning,
		},
	}
}

func envVar(c corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

func TestBranchPod(t *testing.T) {
	p := testProject("cns-x")
	b := testBranch("cns-x", "dev", "main")
	pod := desiredBranchPod(p, b, "consonance/postgres:17-dev")
	c := pod.Spec.Containers[0]
	if pod.Name != "br-dev" || pod.Labels[LabelBranch] != "dev" || pod.Labels[LabelComponent] != ComponentBranch {
		t.Errorf("pod %s labels %v", pod.Name, pod.Labels)
	}
	checkRestricted(t, pod)
	if pod.Spec.SecurityContext.FSGroup != nil {
		t.Errorf("a branch pod sets no fsGroup (the agent's pod owns the volume's group)")
	}
	if !mounts(c, "pgdata", "/pgdata") || !mounts(c, "run", "/var/run/postgresql") || !mounts(c, "token", "/etc/consonance/token") {
		t.Errorf("mounts %+v", c.VolumeMounts)
	}
	for _, m := range c.VolumeMounts {
		if m.Name == "pgdata" && m.SubPath != "branches/dev" {
			t.Errorf("subPath %q", m.SubPath)
		}
	}
	if mounts(c, "db", "/etc/consonance/db") || len(c.Args) != 0 {
		t.Errorf("only main gets the tenant password: args %q", c.Args)
	}
	mem := envVar(c, "MEMORY_LIMIT_BYTES")
	if mem == nil || mem.ValueFrom == nil || mem.ValueFrom.ResourceFieldRef.Resource != "limits.memory" {
		t.Errorf("MEMORY_LIMIT_BYTES %+v", mem)
	}
	if lim := c.Resources.Limits[corev1.ResourceMemory]; lim.String() != "1Gi" {
		t.Errorf("memory limit %s, want the project default", lim.String())
	}
	if a := envVar(c, "AUTOSTART"); a == nil || a.Value != "true" {
		t.Errorf("AUTOSTART %+v", a)
	}
	aff := pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(aff) != 1 || aff[0].TopologyKey != "kubernetes.io/hostname" || aff[0].LabelSelector.MatchLabels[LabelComponent] != ComponentAgent {
		t.Errorf("affinity %+v", aff)
	}
	if *pod.Spec.TerminationGracePeriodSeconds < 45 {
		t.Errorf("grace %d", *pod.Spec.TerminationGracePeriodSeconds)
	}
}

func TestMainPodGetsThePasswordAndStoppedBranchesDoNotAutostart(t *testing.T) {
	p := testProject("cns-x")
	m := testBranch("cns-x", "main", "")
	c := desiredBranchPod(p, m, "img").Spec.Containers[0]
	if len(c.Args) != 1 || c.Args[0] != "--db-password-file=/etc/consonance/db/password" || !mounts(c, "db", "/etc/consonance/db") {
		t.Errorf("main: args %q mounts %+v", c.Args, c.VolumeMounts)
	}
	s := testBranch("cns-x", "old", "main")
	s.Spec.State = v1alpha1.StateStopped
	s.Spec.Compute = &v1alpha1.ComputeSpec{CPU: resource.MustParse("250m"), Memory: resource.MustParse("512Mi")}
	c = desiredBranchPod(p, s, "img").Spec.Containers[0]
	if a := envVar(c, "AUTOSTART"); a == nil || a.Value != "false" {
		t.Errorf("AUTOSTART %+v", a)
	}
	if lim := c.Resources.Limits[corev1.ResourceMemory]; lim.String() != "512Mi" {
		t.Errorf("memory limit %s, want the branch's own", lim.String())
	}
}

func TestBranchService(t *testing.T) {
	svc := desiredBranchService(testBranch("cns-x", "dev", "main"))
	if svc.Name != "br-dev" || !svc.Spec.PublishNotReadyAddresses || svc.Spec.Selector[LabelBranch] != "dev" {
		t.Fatalf("service %s %+v", svc.Name, svc.Spec)
	}
	ports := map[int32]bool{}
	for _, p := range svc.Spec.Ports {
		ports[p.Port] = true
	}
	if !ports[5432] || !ports[8008] {
		t.Fatalf("ports %+v", svc.Spec.Ports)
	}
	if e := branchEndpoint(testBranch("cns-x", "dev", "main")); e.Host != "br-dev.cns-x.svc" || e.Port != 5432 {
		t.Fatalf("endpoint %+v", e)
	}
}

func TestBackupBranchName(t *testing.T) {
	at := time.Date(2026, 10, 7, 3, 15, 0, 0, time.FixedZone("KST", 9*3600))
	if got := backupBranchName("main", at); got != "main-before-20261006181500" {
		t.Errorf("main: %q", got)
	}
	long := strings.Repeat("abcdefghij", 4) // 40 characters
	got := backupBranchName(long, at)
	if got != "abcdefghijabcdefgh-before-20261006181500" || !agentapi.ValidBranchName(got) {
		t.Errorf("long: %q (%d characters)", got, len(got))
	}
}
```

Run: `go test ./internal/controller/`
Expected: FAIL — 빌드 실패(`undefined: desiredBranchPod`)

- [ ] **Step 2: 구현**

`internal/controller/branch_resources.go`:

```go
package controller

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"consonance/api/v1alpha1"
	"consonance/internal/supervisorapi"
)

const (
	postgresPort  = 5432
	dbSecretDir   = "/etc/consonance/db" // main's supervisor reads the tenant password here
	containerName = "postgres"
)

// branchLabels select a branch's pod.
func branchLabels(branch string) map[string]string {
	l := managedLabels(ComponentBranch)
	l[LabelBranch] = branch
	return l
}

// branchCompute is the branch's own compute, or the project's default (spec 4.3).
func branchCompute(p *v1alpha1.Project, b *v1alpha1.Branch) v1alpha1.ComputeSpec {
	if b.Spec.Compute != nil {
		return *b.Spec.Compute
	}
	return p.Spec.DefaultCompute
}

// desiredBranchPod runs a branch: the supervisor, which runs PostgreSQL on /pgdata/current
// (spec 7.3, 7.5).
func desiredBranchPod(p *v1alpha1.Project, b *v1alpha1.Branch, image string) *corev1.Pod {
	compute := branchCompute(p, b)
	res := corev1.ResourceList{corev1.ResourceCPU: compute.CPU, corev1.ResourceMemory: compute.Memory}
	tokenVol, tokenMount := tokenVolume()
	autostart := "true"
	if b.Spec.State == v1alpha1.StateStopped {
		autostart = "false"
	}
	c := corev1.Container{
		Name:            containerName,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{
			{Name: "postgres", ContainerPort: postgresPort},
			{Name: "supervisor", ContainerPort: int32(supervisorapi.DefaultPort)},
		},
		Env: []corev1.EnvVar{
			// shared_buffers is a quarter of this (spec 7.3); the limit is always set, or the
			// Downward API would pass the node's memory (carry-over from plan 1).
			{Name: "MEMORY_LIMIT_BYTES", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
				ContainerName: containerName, Resource: "limits.memory", Divisor: resource.MustParse("1"),
			}}},
			// The state at pod creation; later changes go through /v1/start and /v1/stop.
			{Name: "AUTOSTART", Value: autostart},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "pgdata", MountPath: "/pgdata", SubPath: "branches/" + b.Name},
			{Name: "run", MountPath: "/var/run/postgresql"},
			tokenMount,
		},
		Resources:       corev1.ResourceRequirements{Requests: res, Limits: res},
		ReadinessProbe:  httpProbe("/readyz", supervisorapi.DefaultPort, 2),
		LivenessProbe:   httpProbe("/healthz", supervisorapi.DefaultPort, 10),
		SecurityContext: restrictedContainer(),
	}
	volumes := []corev1.Volume{
		{Name: "pgdata", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: PVCName}}},
		{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		tokenVol,
	}
	if isMain(b) {
		// Only main bootstraps the tenant login (spec 4.5, 7.5).
		c.Args = []string{"--db-password-file=" + dbSecretDir + "/" + PasswordKey}
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "db", MountPath: dbSecretDir, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "db", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: DBCredentialsSecret, Items: []corev1.KeyToPath{{Key: PasswordKey, Path: PasswordKey}},
		}}})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: BranchObjectName(b.Name), Namespace: b.Namespace, Labels: branchLabels(b.Name)},
		Spec: corev1.PodSpec{
			Containers:      []corev1.Container{c},
			Volumes:         volumes,
			SecurityContext: restrictedPod(),
			// The volume is RWO: the branch runs where the agent mounted it (spec 7.5).
			Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: ComponentAgent}},
					TopologyKey:   "kubernetes.io/hostname",
				}},
			}},
			TerminationGracePeriodSeconds: ptr.To(terminationGrace),
			AutomountServiceAccountToken:  ptr.To(false),
		},
	}
}

// desiredBranchService exposes PostgreSQL and the supervisor (spec 7.1 Branch step 2). It
// publishes the pod while it is not ready: the readiness probe fails while PostgreSQL is
// stopped, and the operator and the agent must still reach the supervisor to start it or to
// check that it stopped.
func desiredBranchService(b *v1alpha1.Branch) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: BranchObjectName(b.Name), Namespace: b.Namespace, Labels: branchLabels(b.Name)},
		Spec: corev1.ServiceSpec{
			Selector:                 map[string]string{LabelComponent: ComponentBranch, LabelBranch: b.Name},
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{Name: "postgres", Port: postgresPort, TargetPort: intstr.FromInt(postgresPort), Protocol: corev1.ProtocolTCP},
				{Name: "supervisor", Port: int32(supervisorapi.DefaultPort), TargetPort: intstr.FromInt(supervisorapi.DefaultPort), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// branchEndpoint is where clients reach a branch's PostgreSQL.
func branchEndpoint(b *v1alpha1.Branch) *v1alpha1.Endpoint {
	return &v1alpha1.Endpoint{Host: fmt.Sprintf("%s.%s.svc", BranchObjectName(b.Name), b.Namespace), Port: postgresPort}
}

// backupBranchName names the branch that keeps the generation a rollback replaced:
// <first 18 characters of the branch>-before-<UTC yyyymmddhhmmss>, at most 40 characters
// (spec 9.4).
func backupBranchName(branch string, at time.Time) string {
	prefix := branch
	if len(prefix) > 18 {
		prefix = prefix[:18]
	}
	return prefix + "-before-" + at.UTC().Format("20060102150405")
}
```

- [ ] **Step 3: 통과 확인**

Run: `gofmt -l . && go vet ./internal/controller/ && go test -count=1 ./internal/controller/`
Expected: `ok`. 새 테스트 `TestBranchPod`, `TestMainPodGetsThePasswordAndStoppedBranchesDoNotAutostart`, `TestBranchService`, `TestBackupBranchName`가 들어 있다.

- [ ] **Step 4: 커밋**

```bash
git add internal/controller/
git commit -m "feat(controller): 브랜치 파드·서비스 명세와 백업 브랜치 이름" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 5: 에이전트·감독 프로세스 연결, 오류 분류, 테스트용 가짜

**Files:**
- Create: `internal/controller/clients.go`, `internal/controller/errors.go`
- Test: `internal/controller/clients_test.go`, `internal/controller/fake_test.go`

**Interfaces:**
- Consumes: `agentclient.New`, `agentclient.APIError`, `agentclient.CodeOf`, `supervisorclient.New`, `supervisorclient.APIError`, `agentapi`·`supervisorapi` 타입(계획 1·2)
- Produces:
  - 인터페이스(실제 클라이언트가 구현한다):
    - `AgentAPI`: `Preflight`, `Usage`, `InitMain`, `CreateBranchData`, `DeleteBranchData`, `ListGenerations`, `CreateGeneration`, `ActivateGeneration`, `DetachGeneration`, `DeleteGeneration`. 모두 `agentclient.Client`와 같은 시그니처다.
    - `SupervisorAPI`: `Status`, `Start`, `Stop`, `Bootstrap`
    - `Connector{Agent(ns, token) AgentAPI; Supervisor(ns, branch, token) SupervisorAPI}`
  - HTTP 연결: `HTTPConnector`, `NewHTTPConnector()`, `AgentURL(ns)`, `SupervisorURL(ns, branch)`
  - 호출 도우미: `call[T](ctx, d, f)`, 제한 시간 `callTimeout`·`copyTimeout`·`initTimeout`·`startTimeout`·`stopTimeout`
  - 오류 분류:
    - 판정: `agentCode(err)`, `supervisorStatus(err)`, `stillStarting(err)`, `permanentReason(err)`, `retryLater(err) (ctrl.Result, error)`
    - 영구 실패 이유: `ReasonReflinkUnsupported`·`ReasonInsufficientStorage`·`ReasonSourceNotFound`·`ReasonInvalid`
    - 대기 간격: `waitShort`, `waitPoll`
  - 테스트 가짜(`fake_test.go`):
    - `newFakeConnector(pods client.Reader)`, `(*fakeConnector).project(ns) *fakeProject`
    - `fakeProject`의 메서드:
      - 조작: `failNext(op, errs...)`, `setPreflight`, `addRestorePoint(id, branch)`, `failToStart(branch, n)`
      - 관찰: `callLog()`, `generations(branch)`, `postgres(branch)`, `seenTokens()`
    - 오류 생성: `agentErr(status, code, msg)`, `supervisorErr(status, msg)`
    - 동작: 감독 프로세스는 브랜치 파드가 있을 때만 답한다. 파드의 `AUTOSTART`를 따른다.

- [ ] **Step 1: 실패하는 테스트와 가짜**

`internal/controller/fake_test.go`:

```go
package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
)

// fakeConnector stands in for the agent and the supervisors of every project namespace. A
// supervisor answers only while its branch pod exists, so the operator must make the pod first;
// PostgreSQL then runs or not as the pod's AUTOSTART says, like the real supervisor.
type fakeConnector struct {
	pods client.Reader

	mu       sync.Mutex
	projects map[string]*fakeProject
}

func newFakeConnector(pods client.Reader) *fakeConnector {
	return &fakeConnector{pods: pods, projects: map[string]*fakeProject{}}
}

// project returns the fake volume and servers of a project namespace.
func (c *fakeConnector) project(ns string) *fakeProject {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.projects[ns]
	if p == nil {
		p = &fakeProject{
			ns: ns, pods: c.pods,
			preflight: agentapi.Preflight{Reflink: true, Kernel: "6.8.0"},
			data:      map[string]*fakeData{}, pg: map[string]*fakePG{},
			restorePoints: map[string]string{}, resetPoints: map[string]string{},
			failures: map[string][]error{}, badGenerations: map[string]int{}, tokens: map[string]bool{},
		}
		c.projects[ns] = p
	}
	return p
}

func (c *fakeConnector) Agent(ns, token string) AgentAPI {
	return &fakeAgent{p: c.project(ns), token: token}
}

func (c *fakeConnector) Supervisor(ns, branch, token string) SupervisorAPI {
	return &fakeSupervisor{p: c.project(ns), branch: branch, token: token}
}

// fakeProject is one project volume, its agent and its branches' PostgreSQL.
type fakeProject struct {
	ns   string
	pods client.Reader

	mu             sync.Mutex
	preflight      agentapi.Preflight
	data           map[string]*fakeData // branch → its generations
	pg             map[string]*fakePG   // branch → its PostgreSQL, while its pod exists
	restorePoints  map[string]string    // restore point ID → branch
	resetPoints    map[string]string    // "branch/n" → the restore point a reset made of data-n
	failures       map[string][]error   // operation → errors for its next calls, in order
	badGenerations map[string]int       // branch → a generation PostgreSQL does not start on
	tokens         map[string]bool
	calls          []string
}

type fakeData struct {
	current int
	gens    []int // complete generations, ascending; includes current
}

type fakePG struct {
	state        supervisorapi.PGState
	gen          int // the generation it runs; while stopped, current's target
	bootstrapped bool
}

// failNext makes the next calls of an operation fail with errs, in order. Operations are the
// agent method names and, for supervisors, "Status(branch)", "Start(branch)", "Stop(branch)"
// and "Bootstrap(branch)".
func (p *fakeProject) failNext(op string, errs ...error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures[op] = append(p.failures[op], errs...)
}

func (p *fakeProject) setPreflight(pre agentapi.Preflight) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preflight = pre
}

// addRestorePoint puts a restore point of branch on the volume.
func (p *fakeProject) addRestorePoint(id, branch string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.restorePoints[id] = branch
}

// failToStart makes PostgreSQL of branch fail to start on generation n.
func (p *fakeProject) failToStart(branch string, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.badGenerations[branch] = n
}

func (p *fakeProject) callLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func (p *fakeProject) generations(branch string) (current int, gens []int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.data[branch]
	if d == nil {
		return 0, nil, false
	}
	return d.current, slices.Clone(d.gens), true
}

func (p *fakeProject) postgres(branch string) (supervisorapi.PGState, int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pg := p.pg[branch]
	if pg == nil {
		return "", 0, false
	}
	return pg.state, pg.gen, pg.bootstrapped
}

func (p *fakeProject) seenTokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for t := range p.tokens {
		out = append(out, t)
	}
	return out
}

// call records a call and returns the error injected for it, if any. p.mu is held.
func (p *fakeProject) call(token, op, desc string) error {
	p.tokens[token] = true
	p.calls = append(p.calls, desc)
	if errs := p.failures[op]; len(errs) > 0 {
		p.failures[op] = errs[1:]
		return errs[0]
	}
	return nil
}

// up returns the PostgreSQL of branch if its pod exists; a missing pod is a supervisor that does
// not answer. A pod that just came up runs PostgreSQL if its AUTOSTART says so. p.mu is held.
func (p *fakeProject) up(branch string) (*fakePG, error) {
	var pod corev1.Pod
	err := p.pods.Get(context.Background(), client.ObjectKey{Namespace: p.ns, Name: BranchObjectName(branch)}, &pod)
	if apierrors.IsNotFound(err) || err == nil && pod.DeletionTimestamp != nil {
		delete(p.pg, branch)
		return nil, fmt.Errorf("dial tcp: lookup %s.%s.svc: no such host", BranchObjectName(branch), p.ns)
	}
	if err != nil {
		return nil, err
	}
	pg := p.pg[branch]
	if pg == nil {
		pg = &fakePG{state: supervisorapi.PGStopped}
		if d := p.data[branch]; d != nil {
			pg.gen = d.current
			if autostart(pod) && p.badGenerations[branch] != d.current {
				pg.state = supervisorapi.PGRunning
			}
		}
		p.pg[branch] = pg
	}
	return pg, nil
}

func autostart(pod corev1.Pod) bool {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "AUTOSTART" {
			return e.Value == "true"
		}
	}
	return true
}

func agentErr(status int, code agentapi.ErrorCode, msg string) error {
	return &agentclient.APIError{StatusCode: status, Code: code, Message: msg}
}

func supervisorErr(status int, msg string) error {
	return &supervisorclient.APIError{StatusCode: status, Message: msg}
}

func genName(n int) string { return fmt.Sprintf("data-%d", n) }

// fakeAgent is a project's agent: it keeps the volume's generations as the real one does on disk.
type fakeAgent struct {
	p     *fakeProject
	token string
}

func (a *fakeAgent) Preflight(context.Context) (agentapi.Preflight, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(a.token, "Preflight", "Preflight"); err != nil {
		return agentapi.Preflight{}, err
	}
	return p.preflight, nil
}

func (a *fakeAgent) Usage(context.Context) (agentapi.Usage, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(a.token, "Usage", "Usage"); err != nil {
		return agentapi.Usage{}, err
	}
	return agentapi.Usage{CapacityBytes: 10 << 30, UsedBytes: 1 << 30, AvailableBytes: 9 << 30}, nil
}

func (a *fakeAgent) InitMain(context.Context) (agentapi.Generation, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(a.token, "InitMain", "InitMain"); err != nil {
		return agentapi.Generation{}, err
	}
	if p.data[agentapi.MainBranch] == nil {
		p.data[agentapi.MainBranch] = &fakeData{current: 1, gens: []int{1}}
	}
	return agentapi.Generation{Generation: p.data[agentapi.MainBranch].current}, nil
}

func (a *fakeAgent) CreateBranchData(_ context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(a.token, "CreateBranchData", fmt.Sprintf("CreateBranchData(%s,%s,%s)", name, req.Parent, req.Source.Type)); err != nil {
		return agentapi.BranchData{}, err
	}
	if d := p.data[name]; d != nil { // a retry: the data is there
		return agentapi.BranchData{Generation: d.current}, nil
	}
	lsn := ""
	switch req.Source.Type {
	case agentapi.SourceParentLatest:
		if p.data[req.Parent] == nil {
			return agentapi.BranchData{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "parent has no data")
		}
		if pg := p.pg[req.Parent]; pg == nil || pg.state != supervisorapi.PGRunning {
			return agentapi.BranchData{}, agentErr(http.StatusConflict, agentapi.CodeNotRunning, "parent is not running")
		}
		lsn = "0/3000028"
	case agentapi.SourceRestorePoint:
		if p.restorePoints[req.Source.RestorePointID] != req.Parent {
			return agentapi.BranchData{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "no such restore point")
		}
	}
	p.data[name] = &fakeData{current: 1, gens: []int{1}}
	return agentapi.BranchData{Generation: 1, BackupStartLSN: lsn}, nil
}

func (a *fakeAgent) DeleteBranchData(_ context.Context, name string) error {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(a.token, "DeleteBranchData", fmt.Sprintf("DeleteBranchData(%s)", name)); err != nil {
		return err
	}
	if name == agentapi.MainBranch {
		return agentErr(http.StatusConflict, agentapi.CodeConflict, "main cannot be deleted")
	}
	delete(p.data, name)
	for id, b := range p.restorePoints {
		if b == name {
			delete(p.restorePoints, id)
		}
	}
	return nil
}

func (a *fakeAgent) ListGenerations(_ context.Context, branch string) (agentapi.Generations, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(a.token, "ListGenerations", fmt.Sprintf("ListGenerations(%s)", branch)); err != nil {
		return agentapi.Generations{}, err
	}
	d := p.data[branch]
	if d == nil {
		return agentapi.Generations{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "branch has no data")
	}
	return agentapi.Generations{Current: d.current, Generations: slices.Clone(d.gens)}, nil
}

func (a *fakeAgent) CreateGeneration(_ context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(a.token, "CreateGeneration", fmt.Sprintf("CreateGeneration(%s,%s)", branch, req.Source.Type)); err != nil {
		return agentapi.Generation{}, err
	}
	d := p.data[branch]
	if d == nil {
		return agentapi.Generation{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "branch has no data")
	}
	switch req.Source.Type {
	case agentapi.SourceRestorePoint:
		if p.restorePoints[req.Source.RestorePointID] != branch {
			return agentapi.Generation{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "no such restore point")
		}
	case agentapi.SourceParentLatest:
		if pg := p.pg[req.Parent]; p.data[req.Parent] == nil || pg == nil || pg.state != supervisorapi.PGRunning {
			return agentapi.Generation{}, agentErr(http.StatusConflict, agentapi.CodeNotRunning, "parent is not running")
		}
	}
	n := slices.Max(d.gens) + 1
	d.gens = append(d.gens, n)
	return agentapi.Generation{Generation: n}, nil
}

func (a *fakeAgent) ActivateGeneration(_ context.Context, branch string, n int) (agentapi.Generations, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(a.token, "ActivateGeneration", fmt.Sprintf("ActivateGeneration(%s,%d)", branch, n)); err != nil {
		return agentapi.Generations{}, err
	}
	d := p.data[branch]
	if d == nil || !slices.Contains(d.gens, n) {
		return agentapi.Generations{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "no such generation")
	}
	if n != d.current {
		pg, err := p.up(branch)
		if err != nil {
			return agentapi.Generations{}, agentErr(http.StatusBadGateway, agentapi.CodeSupervisorUnavailable, err.Error())
		}
		if pg.state != supervisorapi.PGStopped {
			return agentapi.Generations{}, agentErr(http.StatusConflict, agentapi.CodeConflict, "stop it before switching generations")
		}
		d.current = n
		pg.gen = n
	}
	return agentapi.Generations{Current: d.current, Generations: slices.Clone(d.gens)}, nil
}

func (a *fakeAgent) DetachGeneration(_ context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(a.token, "DetachGeneration", fmt.Sprintf("DetachGeneration(%s,%d,%s,%v)", branch, n, req.ToBranch, req.AsRestorePoint)); err != nil {
		return agentapi.DetachResponse{}, err
	}
	d := p.data[branch]
	if d == nil {
		return agentapi.DetachResponse{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "branch has no data")
	}
	key := fmt.Sprintf("%s/%d", branch, n)
	if !slices.Contains(d.gens, n) { // a retry: answer from what is on the volume
		if req.ToBranch != "" && p.data[req.ToBranch] != nil {
			return agentapi.DetachResponse{Branch: req.ToBranch}, nil
		}
		if id := p.resetPoints[key]; req.AsRestorePoint && id != "" {
			return agentapi.DetachResponse{RestorePoint: &agentapi.RestorePoint{ID: id, Branch: branch, Reason: agentapi.ReasonReset}}, nil
		}
		return agentapi.DetachResponse{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "no such generation")
	}
	if n == d.current {
		return agentapi.DetachResponse{}, agentErr(http.StatusConflict, agentapi.CodeConflict, "the current generation")
	}
	if pg := p.pg[branch]; pg != nil && pg.state != supervisorapi.PGStopped && pg.gen == n {
		return agentapi.DetachResponse{}, agentErr(http.StatusConflict, agentapi.CodeConflict, "postgres runs it")
	}
	if req.ToBranch != "" && p.data[req.ToBranch] != nil {
		return agentapi.DetachResponse{}, agentErr(http.StatusConflict, agentapi.CodeConflict, "the target branch exists")
	}
	d.gens = slices.DeleteFunc(d.gens, func(g int) bool { return g == n })
	if req.ToBranch != "" {
		p.data[req.ToBranch] = &fakeData{current: 1, gens: []int{1}}
		return agentapi.DetachResponse{Branch: req.ToBranch}, nil
	}
	id := fmt.Sprintf("rp-20261007T031500Z-%04d", len(p.restorePoints))
	p.restorePoints[id] = branch
	p.resetPoints[key] = id
	return agentapi.DetachResponse{RestorePoint: &agentapi.RestorePoint{ID: id, Branch: branch, Reason: agentapi.ReasonReset}}, nil
}

func (a *fakeAgent) DeleteGeneration(_ context.Context, branch string, n int) error {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(a.token, "DeleteGeneration", fmt.Sprintf("DeleteGeneration(%s,%d)", branch, n)); err != nil {
		return err
	}
	d := p.data[branch]
	if d == nil || !slices.Contains(d.gens, n) {
		return agentErr(http.StatusNotFound, agentapi.CodeNotFound, "no such generation")
	}
	if n == d.current {
		return agentErr(http.StatusConflict, agentapi.CodeConflict, "the current generation")
	}
	if pg := p.pg[branch]; pg != nil && pg.state != supervisorapi.PGStopped && pg.gen == n {
		return agentErr(http.StatusConflict, agentapi.CodeConflict, "postgres runs it")
	}
	d.gens = slices.DeleteFunc(d.gens, func(g int) bool { return g == n })
	return nil
}

// fakeSupervisor is one branch's supervisor.
type fakeSupervisor struct {
	p      *fakeProject
	branch string
	token  string
}

func (s *fakeSupervisor) status(pg *fakePG) supervisorapi.Status {
	return supervisorapi.Status{PG: pg.state, Generation: genName(pg.gen)}
}

func (s *fakeSupervisor) Status(context.Context) (supervisorapi.Status, error) {
	p := s.p
	p.mu.Lock()
	defer p.mu.Unlock()
	op := "Status(" + s.branch + ")"
	if err := p.call(s.token, op, op); err != nil {
		return supervisorapi.Status{}, err
	}
	pg, err := p.up(s.branch)
	if err != nil {
		return supervisorapi.Status{}, err
	}
	return s.status(pg), nil
}

func (s *fakeSupervisor) Start(context.Context) (supervisorapi.Status, error) {
	p := s.p
	p.mu.Lock()
	defer p.mu.Unlock()
	op := "Start(" + s.branch + ")"
	if err := p.call(s.token, op, op); err != nil {
		return supervisorapi.Status{}, err
	}
	pg, err := p.up(s.branch)
	if err != nil {
		return supervisorapi.Status{}, err
	}
	if pg.state == supervisorapi.PGRunning {
		return s.status(pg), nil
	}
	d := p.data[s.branch]
	if d == nil {
		return supervisorapi.Status{}, supervisorErr(http.StatusInternalServerError, "data directory does not resolve")
	}
	pg.gen = d.current
	if p.badGenerations[s.branch] == d.current {
		pg.state = supervisorapi.PGStopped
		return supervisorapi.Status{}, supervisorErr(http.StatusInternalServerError, "postgres exited during startup")
	}
	pg.state = supervisorapi.PGRunning
	return s.status(pg), nil
}

func (s *fakeSupervisor) Stop(context.Context) (supervisorapi.Status, error) {
	p := s.p
	p.mu.Lock()
	defer p.mu.Unlock()
	op := "Stop(" + s.branch + ")"
	if err := p.call(s.token, op, op); err != nil {
		return supervisorapi.Status{}, err
	}
	pg, err := p.up(s.branch)
	if err != nil {
		return supervisorapi.Status{}, err
	}
	pg.state = supervisorapi.PGStopped
	if d := p.data[s.branch]; d != nil {
		pg.gen = d.current
	}
	return s.status(pg), nil
}

func (s *fakeSupervisor) Bootstrap(context.Context) (supervisorapi.Status, error) {
	p := s.p
	p.mu.Lock()
	defer p.mu.Unlock()
	op := "Bootstrap(" + s.branch + ")"
	if err := p.call(s.token, op, op); err != nil {
		return supervisorapi.Status{}, err
	}
	pg, err := p.up(s.branch)
	if err != nil {
		return supervisorapi.Status{}, err
	}
	if s.branch != agentapi.MainBranch {
		return supervisorapi.Status{}, supervisorErr(http.StatusNotFound, "bootstrap is disabled")
	}
	if pg.state != supervisorapi.PGRunning {
		return supervisorapi.Status{}, supervisorErr(http.StatusConflict, "postgres is not running")
	}
	pg.bootstrapped = true
	return s.status(pg), nil
}
```

`internal/controller/clients_test.go`:

```go
package controller

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/supervisorapi"
)

func TestServiceURLs(t *testing.T) {
	if got := AgentURL("cns-a1b2c3d4e5f6"); got != "http://volume-agent.cns-a1b2c3d4e5f6.svc:8443" {
		t.Errorf("AgentURL = %q", got)
	}
	if got := SupervisorURL("cns-a1b2c3d4e5f6", "dev"); got != "http://br-dev.cns-a1b2c3d4e5f6.svc:8008" {
		t.Errorf("SupervisorURL = %q", got)
	}
}

func TestRetryLater(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		after   time.Duration
		backoff bool
	}{
		{"rate limited", &agentclient.APIError{StatusCode: 429, Code: agentapi.CodeRateLimited, RetryAfter: 3 * time.Second}, 3 * time.Second, false},
		{"rate limited without Retry-After", &agentclient.APIError{StatusCode: 429, Code: agentapi.CodeRateLimited}, time.Second, false},
		{"source not running", agentErr(409, agentapi.CodeNotRunning, ""), waitShort, false},
		{"busy", agentErr(409, agentapi.CodeBusy, ""), 0, true},
		{"supervisor unavailable", agentErr(502, agentapi.CodeSupervisorUnavailable, ""), 0, true},
		{"a copy's conflict", agentErr(409, agentapi.CodeConflict, ""), 0, true},
		{"no answer", errors.New("dial tcp: connection refused"), 0, true},
	}
	for _, c := range cases {
		res, err := retryLater(c.err)
		if res.RequeueAfter != c.after || (err != nil) != c.backoff {
			t.Errorf("%s: %+v, %v", c.name, res, err)
		}
	}
}

func TestPermanentReason(t *testing.T) {
	cases := map[error]string{
		agentErr(501, agentapi.CodeReflinkUnsupported, ""):  ReasonReflinkUnsupported,
		agentErr(507, agentapi.CodeInsufficientStorage, ""): ReasonInsufficientStorage,
		agentErr(400, agentapi.CodeInvalid, ""):             ReasonInvalid,
		agentErr(409, agentapi.CodeBusy, ""):                "",
		agentErr(500, agentapi.CodeInternal, ""):            "",
		errors.New("dial tcp: connection refused"):          "",
	}
	for err, want := range cases {
		if got := permanentReason(err); got != want {
			t.Errorf("permanentReason(%v) = %q, want %q", err, got, want)
		}
	}
	if !stillStarting(supervisorErr(http.StatusServiceUnavailable, "still starting")) || stillStarting(supervisorErr(500, "exited")) {
		t.Error("stillStarting")
	}
}

// The fake supervisor answers only while its pod exists and starts as the pod's AUTOSTART says:
// the controller tests rely on both.
func TestFakeSupervisorFollowsItsPod(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()
	conn := newFakeConnector(c)
	agent := conn.Agent("cns-x", "tok")
	if _, err := agent.InitMain(ctx); err != nil {
		t.Fatal(err)
	}
	sup := conn.Supervisor("cns-x", "main", "tok")
	if _, err := sup.Status(ctx); err == nil {
		t.Fatal("a supervisor without a pod answered")
	}
	pod := desiredBranchPod(testProject("cns-x"), testBranch("cns-x", "main", ""), "img")
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	st, err := sup.Status(ctx)
	if err != nil || st.PG != supervisorapi.PGRunning || st.Generation != "data-1" {
		t.Fatalf("status %+v, %v", st, err)
	}
	if _, err := agent.ActivateGeneration(ctx, "main", 1); err != nil {
		t.Fatalf("activate the current generation: %v", err)
	}
	if _, err := sup.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, boot := conn.project("cns-x").postgres("main"); !boot {
		t.Fatal("not bootstrapped")
	}
}
```

Run: `go test ./internal/controller/`
Expected: FAIL — 빌드 실패(`undefined: AgentAPI` 등)

- [ ] **Step 2: 구현**

`internal/controller/clients.go`:

```go
package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
)

// AgentAPI is the part of a project's volume agent API the operator calls (spec 7.2).
// *agentclient.Client implements it.
type AgentAPI interface {
	Preflight(ctx context.Context) (agentapi.Preflight, error)
	Usage(ctx context.Context) (agentapi.Usage, error)
	InitMain(ctx context.Context) (agentapi.Generation, error)
	CreateBranchData(ctx context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error)
	DeleteBranchData(ctx context.Context, name string) error
	ListGenerations(ctx context.Context, branch string) (agentapi.Generations, error)
	CreateGeneration(ctx context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error)
	ActivateGeneration(ctx context.Context, branch string, n int) (agentapi.Generations, error)
	DetachGeneration(ctx context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error)
	DeleteGeneration(ctx context.Context, branch string, n int) error
}

// SupervisorAPI is the part of a branch supervisor's API the operator calls (spec 7.3).
// *supervisorclient.Client implements it.
type SupervisorAPI interface {
	Status(ctx context.Context) (supervisorapi.Status, error)
	Start(ctx context.Context) (supervisorapi.Status, error)
	Stop(ctx context.Context) (supervisorapi.Status, error)
	Bootstrap(ctx context.Context) (supervisorapi.Status, error)
}

var (
	_ AgentAPI      = (*agentclient.Client)(nil)
	_ SupervisorAPI = (*supervisorclient.Client)(nil)
)

// Connector gives the clients of a project's agent and of a branch's supervisor. Tests swap in
// a fake.
type Connector interface {
	Agent(namespace, token string) AgentAPI
	Supervisor(namespace, branch, token string) SupervisorAPI
}

// Time limits of the operator's calls. The HTTP client has none: each call's context bounds it.
const (
	callTimeout  = 10 * time.Second // status, lists, preflight, usage, activate, detach, deletes
	copyTimeout  = 3 * time.Minute  // CreateBranchData, CreateGeneration: a backup start waits for a checkpoint
	initTimeout  = 5 * time.Minute  // InitMain: initdb on the agent's 100m CPU (carry-over from plan 2)
	startTimeout = 75 * time.Second // the supervisor waits up to 60s for PostgreSQL to accept connections
	stopTimeout  = 60 * time.Second // a 30s fast shutdown, then an immediate one
)

// HTTPConnector reaches the agent and the supervisors through their services, from inside the
// cluster.
type HTTPConnector struct {
	HTTP *http.Client
}

// NewHTTPConnector returns a connector whose calls are bounded by their contexts only.
func NewHTTPConnector() HTTPConnector { return HTTPConnector{HTTP: &http.Client{}} }

// AgentURL is the address of a project's agent: its service volume-agent.
func AgentURL(namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", AgentName, namespace, agentapi.DefaultPort)
}

// SupervisorURL is the address of a branch's supervisor: its service br-<branch>.
func SupervisorURL(namespace, branch string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", BranchObjectName(branch), namespace, supervisorapi.DefaultPort)
}

func (c HTTPConnector) Agent(namespace, token string) AgentAPI {
	return agentclient.New(AgentURL(namespace), token, c.HTTP)
}

func (c HTTPConnector) Supervisor(namespace, branch, token string) SupervisorAPI {
	return supervisorclient.New(SupervisorURL(namespace, branch), token, c.HTTP)
}

// call runs f with a context bounded by d.
func call[T any](ctx context.Context, d time.Duration, f func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return f(ctx)
}
```

`internal/controller/errors.go`:

```go
package controller

import (
	"errors"
	"net/http"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/supervisorclient"
)

// Condition reasons of permanent failures (spec 10: phase Failed, the reason recorded).
const (
	ReasonReflinkUnsupported  = "ReflinkUnsupported"
	ReasonInsufficientStorage = "InsufficientStorage"
	ReasonSourceNotFound      = "SourceNotFound"
	ReasonInvalid             = "Invalid"
)

// agentCode is the error code of an agent error, "" for any other error.
func agentCode(err error) agentapi.ErrorCode { return agentclient.CodeOf(err) }

// supervisorStatus is the HTTP status of a supervisor error, 0 when the supervisor did not
// answer.
func supervisorStatus(err error) int {
	var e *supervisorclient.APIError
	if errors.As(err, &e) {
		return e.StatusCode
	}
	return 0
}

// stillStarting reports a /v1/start that timed out while PostgreSQL still starts (503): not a
// failure (carry-over from plan 1).
func stillStarting(err error) bool { return supervisorStatus(err) == http.StatusServiceUnavailable }

// permanentReason is the condition reason of an agent error that retrying cannot fix, or "".
// A copy's not-found (no parent data, no such restore point) is permanent too; callers that
// copy check it themselves.
func permanentReason(err error) string {
	switch agentCode(err) {
	case agentapi.CodeReflinkUnsupported:
		return ReasonReflinkUnsupported
	case agentapi.CodeInsufficientStorage:
		return ReasonInsufficientStorage
	case agentapi.CodeInvalid:
		return ReasonInvalid
	}
	return ""
}

// retryLater turns an error worth retrying into what Reconcile returns: a rate limit comes back
// after Retry-After, a source branch that is not running after a short wait, and anything else
// (busy, supervisor-unavailable, internal, a copy's conflict, no answer) with backoff
// (carry-over from plan 2).
func retryLater(err error) (ctrl.Result, error) {
	var e *agentclient.APIError
	if errors.As(err, &e) {
		switch e.Code {
		case agentapi.CodeRateLimited:
			return ctrl.Result{RequeueAfter: max(e.RetryAfter, time.Second)}, nil
		case agentapi.CodeNotRunning:
			return ctrl.Result{RequeueAfter: waitShort}, nil
		}
	}
	return ctrl.Result{}, err
}

// How long to wait before looking again at something on its way: a pod coming up, PostgreSQL
// starting or stopping, a source branch to run.
const (
	waitShort = 2 * time.Second
	waitPoll  = 500 * time.Millisecond
)
```

- [ ] **Step 3: 통과 확인**

Run: `go mod tidy && gofmt -l . && go vet ./internal/controller/ && go test -count=1 ./internal/controller/`
Expected: `ok`. `go mod tidy`는 controller-runtime 가짜 클라이언트가 쓰는 모듈을 더한다. 새 테스트는 `TestServiceURLs`, `TestRetryLater`, `TestPermanentReason`, `TestFakeSupervisorFollowsItsPod`다.

- [ ] **Step 4: 커밋**

```bash
git add go.mod go.sum internal/controller/
git commit -m "feat(controller): 에이전트·감독 프로세스 연결, 오류 분류, 테스트용 가짜" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```


### Task 6: Project 조정

**Files:**
- Create: `internal/controller/manager.go`, `internal/controller/project_controller.go`
- Test: `internal/controller/suite_test.go`, `internal/controller/project_controller_test.go`

**Interfaces:**
- Consumes:
  - Task 2: `v1alpha1`, `kubetest.Start`
  - Task 3: 명세 함수
  - Task 5: `Connector`, `call`, 오류 분류, 가짜
- Produces:
  - 매니저 설정: `CacheOptions() cache.Options`, `rateLimiter()`
  - `ProjectReconciler{Client; Scheme; Connector; AgentImage string}`: `SetupWithManager(mgr)`, `Reconcile`
  - 도우미: `summarize(p)`, `setCondition(conds, generation, type, status bool, reason, msg)`, `projectOf`
  - 테스트 묶음(`suite_test.go`):
    - 변수와 이미지: 전역 `kenv`·`conn`, `testAgentImage`·`testPostgresImage`
    - 도우미: `eventually(t, timeout, check)`, `createProject(t, setup)`, `getProject`, `cond(conds, type)`, `bindVolume(t, ns)`, `updateStatus`, `ignoreExists`
    - 로그 보기: 환경 변수 `CONSONANCE_TEST_LOG=1`이면 조정기 로그가 보인다.
  - 테스트 도우미 `runMain`, `ptrProject`, `countCalls`

Project 조정은 스펙 7.1의 순서를 따른다.
1. Secret 둘(없을 때만), 네트워크 정책, 쿼터를 맞춘다.
2. PVC를 만든다. 바인딩되면 `VolumeReady`다.
3. 에이전트 Service와 파드를 둔다. 끝났거나 설정이 바뀐 파드는 지운다.
4. `ReflinkSupported`가 아직 참이 아니면 preflight를 부른다. 무응답이면 `AgentReady=False`이고 2초 뒤 다시 본다. `reflink:false`면 `ReflinkSupported=False`이고 다시 묻지 않는다.
5. main Branch를 둔다.
6. main이 Running이면 bootstrap을 부르고 `MainReady`를 참으로 둔다.
7. usage로 `status.volume`을 채우고 1분 뒤 다시 본다.

상태는 바뀌었을 때만 쓴다. Project 자체의 감시는 spec이 바뀔 때만 깨운다(GenerationChangedPredicate). 자기 상태 갱신으로 다시 깨지 않게 하려는 것이다.

- [ ] **Step 1: 테스트 묶음과 실패하는 테스트**

`internal/controller/suite_test.go`:

```go
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"consonance/api/v1alpha1"
	"consonance/internal/testutil/kubetest"
)

// The controller tests run the reconcilers in one manager against an envtest API server, with
// the fake agent and supervisors of fake_test.go. envtest has no kubelet: pods never run, so the
// fake decides what runs, and no controller binds volumes or deletes namespaces.
var (
	kenv *kubetest.Env
	conn *fakeConnector
)

const (
	testAgentImage    = "consonance/agent:test"
	testPostgresImage = "consonance/postgres:test"
)

func TestMain(m *testing.M) { os.Exit(runSuite(m)) }

func runSuite(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run() // the envtest tests skip; the others run
	}
	e, err := kubetest.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer e.Stop()
	kenv = e
	conn = newFakeConnector(e.Client)
	stop, err := startManager(e)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stop()
	return m.Run()
}

func startManager(e *kubetest.Env) (stop func(), err error) {
	// CONSONANCE_TEST_LOG=1 shows the reconcilers' logs.
	log := logr.Discard()
	if os.Getenv("CONSONANCE_TEST_LOG") != "" {
		log = logr.FromSlogHandler(slog.NewTextHandler(os.Stderr, nil))
	}
	ctrl.SetLogger(log)
	mgr, err := ctrl.NewManager(e.Config, ctrl.Options{
		Scheme:                 e.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  CacheOptions(),
	})
	if err != nil {
		return nil, err
	}
	if err := (&ProjectReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Connector: conn, AgentImage: testAgentImage}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			fmt.Fprintln(os.Stderr, "manager:", err)
		}
	}, nil
}

// eventually calls check every 50ms until it returns "" or timeout passes; then the test fails
// with check's last message.
func eventually(t *testing.T, timeout time.Duration, check func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		msg := check()
		if msg == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s: %s", timeout, msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// createProject makes a project namespace and its Project, as the API server does (spec 9.1).
// setup, if given, prepares the project's fake agent first.
func createProject(t *testing.T, setup func(*fakeProject)) (string, *fakeProject) {
	t.Helper()
	kubetest.Need(t, kenv)
	ns := kenv.Namespace(t)
	fp := conn.project(ns)
	if setup != nil {
		setup(fp)
	}
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: ProjectName, Namespace: ns},
		Spec:       v1alpha1.ProjectSpec{DisplayName: "test"},
	}
	if err := kenv.Client.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return ns, fp
}

func getProject(t *testing.T, ns string) v1alpha1.Project {
	t.Helper()
	var p v1alpha1.Project
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: ProjectName}, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// cond shows a condition as status/reason for test messages.
func cond(conds []metav1.Condition, typ string) string {
	c := meta.FindStatusCondition(conds, typ)
	if c == nil {
		return "none"
	}
	return string(c.Status) + "/" + c.Reason
}

// bindVolume marks the project volume bound, which no controller does under envtest.
func bindVolume(t *testing.T, ns string) {
	t.Helper()
	ctx := context.Background()
	eventually(t, 10*time.Second, func() string {
		var pvc corev1.PersistentVolumeClaim
		if err := kenv.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: PVCName}, &pvc); err != nil {
			return "volume: " + err.Error()
		}
		pvc.Status.Phase = corev1.ClaimBound
		if err := kenv.Client.Status().Update(ctx, &pvc); err != nil {
			return "bind: " + err.Error()
		}
		return ""
	})
}

// updateStatus applies change to the latest version of obj's status, retrying on conflicts.
func updateStatus[T client.Object](t *testing.T, obj T, change func(T)) {
	t.Helper()
	ctx := context.Background()
	eventually(t, 10*time.Second, func() string {
		if err := kenv.Client.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return err.Error()
		}
		change(obj)
		if err := kenv.Client.Status().Update(ctx, obj); err != nil {
			return err.Error()
		}
		return ""
	})
}

// ignoreExists treats a create that lost a race to the operator as done.
func ignoreExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
```

`internal/controller/project_controller_test.go`:

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/testutil/kubetest"
)

func TestProjectMakesItsObjects(t *testing.T) {
	ns, fp := createProject(t, nil)
	ctx := context.Background()
	key := func(name string) client.ObjectKey { return client.ObjectKey{Namespace: ns, Name: name} }
	eventually(t, 10*time.Second, func() string {
		p := getProject(t, ns)
		if !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionAgentReady) ||
			!meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReflinkSupported) {
			return fmt.Sprintf("AgentReady %s, ReflinkSupported %s", cond(p.Status.Conditions, v1alpha1.ConditionAgentReady), cond(p.Status.Conditions, v1alpha1.ConditionReflinkSupported))
		}
		var main v1alpha1.Branch
		if err := kenv.Client.Get(ctx, key(v1alpha1.MainBranch), &main); err != nil {
			return "main: " + err.Error()
		}
		return ""
	})

	var creds, token corev1.Secret
	if err := kenv.Client.Get(ctx, key(DBCredentialsSecret), &creds); err != nil || len(creds.Data[PasswordKey]) != 32 {
		t.Fatalf("db-credentials: %v %v", err, creds.Data)
	}
	if err := kenv.Client.Get(ctx, key(InternalTokenSecret), &token); err != nil {
		t.Fatal(err)
	}
	if got := fp.seenTokens(); !slices.Equal(got, []string{string(token.Data[TokenKey])}) {
		t.Errorf("the agent saw tokens %q", got)
	}
	var pols networkingv1.NetworkPolicyList
	if err := kenv.Client.List(ctx, &pols, client.InNamespace(ns)); err != nil || len(pols.Items) != 5 {
		t.Fatalf("network policies: %v, %d", err, len(pols.Items))
	}
	var quota corev1.ResourceQuota
	var pvc corev1.PersistentVolumeClaim
	var svc corev1.Service
	var pod corev1.Pod
	for name, obj := range map[string]client.Object{QuotaName: &quota, PVCName: &pvc, AgentName: &svc} {
		if err := kenv.Client.Get(ctx, key(name), obj); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := kenv.Client.Get(ctx, key(AgentName), &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.Containers[0].Image != testAgentImage || !metav1.IsControlledBy(&pod, ptrProject(getProject(t, ns))) {
		t.Errorf("agent pod image %s owners %+v", pod.Spec.Containers[0].Image, pod.OwnerReferences)
	}
	if len(pvc.OwnerReferences) != 0 || len(creds.OwnerReferences) != 0 {
		t.Errorf("the volume and the secrets must go only with the namespace: %+v %+v", pvc.OwnerReferences, creds.OwnerReferences)
	}
	var main v1alpha1.Branch
	if err := kenv.Client.Get(ctx, key(v1alpha1.MainBranch), &main); err != nil || !main.Spec.Protected || main.Spec.Parent != "" {
		t.Fatalf("main: %v %+v", err, main.Spec)
	}
	if p := getProject(t, ns); p.Status.Phase != v1alpha1.ProjectProvisioning || p.Status.Volume.PVCName != PVCName {
		t.Errorf("phase %s volume %+v", p.Status.Phase, p.Status.Volume)
	}

	// Reconciles that change nothing write nothing: no update loop.
	versions := func() []string {
		var out []string
		for name, obj := range map[string]client.Object{QuotaName: &corev1.ResourceQuota{}, AgentName: &corev1.Service{}, "branch-ingress": &networkingv1.NetworkPolicy{}} {
			if err := kenv.Client.Get(ctx, key(name), obj); err != nil {
				t.Fatal(err)
			}
			out = append(out, name+"@"+obj.GetResourceVersion())
		}
		slices.Sort(out)
		return out
	}
	before := versions()
	bindVolume(t, ns) // triggers a reconcile
	time.Sleep(time.Second)
	if after := versions(); !slices.Equal(before, after) {
		t.Errorf("objects changed with nothing to change: %v then %v", before, after)
	}
}

func ptrProject(p v1alpha1.Project) *v1alpha1.Project { return &p }

// runMain does what main's Branch reconcile does (task 7): make its data and pod and report it
// running. With that reconciler registered it may have done so already.
func runMain(t *testing.T, ns string) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Agent(ns, "").InitMain(ctx); err != nil {
		t.Fatal(err)
	}
	main := &v1alpha1.Branch{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: v1alpha1.MainBranch}}
	eventually(t, 10*time.Second, func() string {
		if err := kenv.Client.Get(ctx, client.ObjectKeyFromObject(main), main); err != nil {
			return "main: " + err.Error()
		}
		return ""
	})
	pod := desiredBranchPod(ptrProject(getProject(t, ns)), main, testPostgresImage)
	if err := ignoreExists(kenv.Client.Create(ctx, pod)); err != nil {
		t.Fatal(err)
	}
	updateStatus(t, main, func(b *v1alpha1.Branch) { b.Status.Phase = v1alpha1.BranchRunning })
}

func TestProjectBecomesReady(t *testing.T) {
	ns, fp := createProject(t, nil)
	bindVolume(t, ns)
	runMain(t, ns)
	eventually(t, 15*time.Second, func() string {
		p := getProject(t, ns)
		if p.Status.Phase != v1alpha1.ProjectReady {
			return fmt.Sprintf("phase %s, Ready %s: %s", p.Status.Phase, cond(p.Status.Conditions, v1alpha1.ConditionReady),
				meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReady).Message)
		}
		return ""
	})
	p := getProject(t, ns)
	if p.Status.Volume.CapacityBytes != 10<<30 || p.Status.Volume.UsedBytes != 1<<30 {
		t.Errorf("volume %+v", p.Status.Volume)
	}
	if _, _, boot := fp.postgres(v1alpha1.MainBranch); !boot {
		t.Error("main was not bootstrapped")
	}
	if !slices.Contains(fp.callLog(), "Bootstrap(main)") {
		t.Errorf("calls %q", fp.callLog())
	}
}

func TestProjectFailsWithoutReflink(t *testing.T) {
	ns, fp := createProject(t, func(fp *fakeProject) {
		fp.setPreflight(agentapi.Preflight{Reflink: false, Kernel: "6.8.0", Error: "reflink copies are not supported here: operation not supported"})
	})
	eventually(t, 10*time.Second, func() string {
		p := getProject(t, ns)
		if p.Status.Phase != v1alpha1.ProjectFailed {
			return "phase " + string(p.Status.Phase)
		}
		return ""
	})
	p := getProject(t, ns)
	c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReflinkSupported)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonReflinkUnsupported || !strings.Contains(c.Message, "not supported") {
		t.Fatalf("ReflinkSupported %+v", c)
	}
	if got := cond(p.Status.Conditions, v1alpha1.ConditionReady); got != "False/"+ReasonReflinkUnsupported {
		t.Errorf("Ready %s", got)
	}
	// A failed project is not probed again. (A reconcile that read the Project before its
	// status update reached the cache may have probed once more before this point.)
	n := countCalls(fp, "Preflight")
	time.Sleep(time.Second)
	if m := countCalls(fp, "Preflight"); m != n {
		t.Errorf("%d preflights, then %d", n, m)
	}
	var main v1alpha1.Branch
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: v1alpha1.MainBranch}, &main); !apierrors.IsNotFound(err) {
		t.Errorf("main of a failed project: %v", err)
	}
}

func countCalls(fp *fakeProject, prefix string) int {
	n := 0
	for _, c := range fp.callLog() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestProjectWaitsForItsAgent(t *testing.T) {
	refused := errors.New("dial tcp 10.43.0.10:8443: connect: connection refused")
	ns, fp := createProject(t, func(fp *fakeProject) { fp.failNext("Preflight", refused, refused) })
	eventually(t, 15*time.Second, func() string {
		p := getProject(t, ns)
		if !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReflinkSupported) {
			return "ReflinkSupported " + cond(p.Status.Conditions, v1alpha1.ConditionReflinkSupported)
		}
		return ""
	})
	if n := countCalls(fp, "Preflight"); n < 3 {
		t.Errorf("%d preflights, want 2 refused and then one answered", n)
	}
}

func TestAgentPodFollowsTheProjectSettings(t *testing.T) {
	ns, _ := createProject(t, nil)
	ctx := context.Background()
	podArgs := func() []string {
		var pod corev1.Pod
		if err := kenv.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: AgentName}, &pod); err != nil {
			return nil
		}
		return pod.Spec.Containers[0].Args
	}
	eventually(t, 10*time.Second, func() string {
		if !slices.Contains(podArgs(), "--min-copy-interval=5s") {
			return fmt.Sprintf("args %q", podArgs())
		}
		return ""
	})
	p := getProject(t, ns)
	p.Spec.Limits.MinCopyInterval = metav1.Duration{Duration: time.Minute}
	if err := kenv.Client.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() string {
		if !slices.Contains(podArgs(), "--min-copy-interval=1m0s") {
			return fmt.Sprintf("args %q", podArgs())
		}
		return ""
	})
}

func TestProjectOutsideAProjectNamespaceFails(t *testing.T) {
	kubetest.Need(t, kenv)
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "plain-"}}
	if err := kenv.Client.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	p := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: ProjectName, Namespace: ns.Name}, Spec: v1alpha1.ProjectSpec{DisplayName: "x"}}
	if err := kenv.Client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() string {
		got := getProject(t, ns.Name)
		if got.Status.Phase != v1alpha1.ProjectFailed || cond(got.Status.Conditions, v1alpha1.ConditionReady) != "False/InvalidNamespace" {
			return fmt.Sprintf("phase %s Ready %s", got.Status.Phase, cond(got.Status.Conditions, v1alpha1.ConditionReady))
		}
		return ""
	})
	var s corev1.Secret
	if err := kenv.Client.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: DBCredentialsSecret}, &s); !apierrors.IsNotFound(err) {
		t.Errorf("secret in a plain namespace: %v", err)
	}
}
```

Run: `go test ./internal/controller/`
Expected: FAIL — 빌드 실패(`undefined: ProjectReconciler`, `undefined: CacheOptions`)

- [ ] **Step 2: 구현**

`internal/controller/manager.go`:

```go
package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Leader election and events of the manager.
//
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// CacheOptions limit the manager's cache to the objects the operator makes, all labelled
// managed-by=consonance-operator: it then neither holds every secret of the cluster nor watches
// every pod.
func CacheOptions() cache.Options {
	mine := labels.SelectorFromSet(labels.Set{LabelManagedBy: ManagedBy})
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Secret{}:                {Label: mine},
		&corev1.Pod{}:                   {Label: mine},
		&corev1.Service{}:               {Label: mine},
		&corev1.PersistentVolumeClaim{}: {Label: mine},
		&corev1.ResourceQuota{}:         {Label: mine},
		&networkingv1.NetworkPolicy{}:   {Label: mine},
	}}
}

// rateLimiter retries failed reconciles from 100ms, doubling up to 30s: the default goes up to
// 16 minutes, too slow for an agent or supervisor that is back after a restart (spec 10).
func rateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](100*time.Millisecond, 30*time.Second)
}
```

`internal/controller/project_controller.go`:

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"consonance/api/v1alpha1"
)

const (
	bootstrapTimeout = 30 * time.Second
	// usageEvery is how often a ready project's volume usage is refreshed.
	usageEvery = time.Minute
)

// The Project reconcile's permissions (config/rbac/role.yaml, make generate).
//
// +kubebuilder:rbac:groups=consonance.io,resources=projects,verbs=get;list;watch
// +kubebuilder:rbac:groups=consonance.io,resources=projects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=consonance.io,resources=projects/finalizers,verbs=update
// +kubebuilder:rbac:groups=consonance.io,resources=branches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;persistentvolumeclaims;pods;services;resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// ProjectReconciler carries out spec 7.1's Project steps.
type ProjectReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Connector  Connector
	AgentImage string
}

// SetupWithManager registers the reconciler. A project is looked at again when its spec changes
// (not on its own status updates), when one of its objects changes, when its volume binds, and
// when main changes (main's first run leads to the bootstrap).
func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Project{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(projectOf)).
		Watches(&v1alpha1.Branch{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			if o.GetName() != v1alpha1.MainBranch {
				return nil
			}
			return projectOf(ctx, o)
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4, RateLimiter: rateLimiter()}).
		Complete(r)
}

// projectOf maps an object to the Project of its namespace.
func projectOf(_ context.Context, o client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: o.GetNamespace(), Name: ProjectName}}}
}

func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1alpha1.Project
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // deleting a project deletes its namespace and everything in it (spec 4.1)
	}
	id, _ := ProjectID(p.Namespace)
	ctx = logf.IntoContext(ctx, logf.FromContext(ctx).WithValues("projectId", id))

	before := p.Status.DeepCopy()
	res, err := r.reconcile(ctx, &p)
	p.Status.ObservedGeneration = p.Generation
	summarize(&p)
	if !equality.Semantic.DeepEqual(*before, p.Status) {
		if uerr := r.Status().Update(ctx, &p); uerr != nil {
			return ctrl.Result{}, errors.Join(err, uerr)
		}
	}
	return res, err
}

// reconcile takes the project as far as it can; the conditions it sets say how far it got.
func (r *ProjectReconciler) reconcile(ctx context.Context, p *v1alpha1.Project) (ctrl.Result, error) {
	id, ok := ProjectID(p.Namespace)
	if !ok {
		return ctrl.Result{}, nil // summarize reports it; nothing else is made
	}

	// Step 1: secrets, network policies, quota.
	if _, err := r.ensureSecret(ctx, newDBCredentialsSecret, p.Namespace); err != nil {
		return ctrl.Result{}, err
	}
	tokenSecret, err := r.ensureSecret(ctx, newInternalTokenSecret, p.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	token := string(tokenSecret.Data[TokenKey])
	if token == "" {
		return ctrl.Result{}, fmt.Errorf("secret %s has no %s", InternalTokenSecret, TokenKey)
	}
	for _, want := range desiredNetworkPolicies(p.Namespace) {
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
			np.Labels, np.Spec = want.Labels, want.Spec
			return controllerutil.SetControllerReference(p, np, r.Scheme)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	wantQuota := desiredQuota(p)
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: wantQuota.Name, Namespace: wantQuota.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
		quota.Labels, quota.Spec = wantQuota.Labels, wantQuota.Spec
		return controllerutil.SetControllerReference(p, quota, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// Step 2: the volume. A WaitForFirstConsumer class binds it only once the agent pod is
	// scheduled, so nothing below waits for VolumeReady.
	pvc, err := r.ensurePVC(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}
	p.Status.Volume.PVCName = PVCName
	if pvc.Status.Phase == corev1.ClaimBound {
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionVolumeReady, true, "Bound", "")
	} else {
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionVolumeReady, false, "NotBound", "the volume is "+string(pvc.Status.Phase))
	}

	// Step 3: the agent.
	if err := r.ensureAgent(ctx, p, id); err != nil {
		return ctrl.Result{}, err
	}
	agent := r.Connector.Agent(p.Namespace, token)

	// Step 4: reflink. A volume without it stays failed (spec 10); a probe that failed for
	// another reason (500) is tried again.
	if c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReflinkSupported); c == nil || c.Status != metav1.ConditionTrue {
		if c != nil && c.Status == metav1.ConditionFalse {
			return ctrl.Result{}, nil
		}
		pre, err := call(ctx, callTimeout, agent.Preflight)
		if err != nil {
			setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionAgentReady, false, "AgentUnavailable", err.Error())
			return ctrl.Result{RequeueAfter: waitShort}, nil
		}
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionAgentReady, true, "Answers", "")
		if !pre.Reflink {
			setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionReflinkSupported, false, ReasonReflinkUnsupported, pre.Error)
			return ctrl.Result{}, nil
		}
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionReflinkSupported, true, "Supported", "kernel "+pre.Kernel)
	}

	// Step 5: main. Its Branch reconcile makes its data with init-main.
	main, err := r.ensureMainBranch(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Step 6: the tenant login, once main first runs. Bootstrap skips what exists.
	if !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionMainReady) {
		if main.Status.Phase != v1alpha1.BranchRunning {
			setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionMainReady, false, "MainNotRunning", "main is "+phaseOrNone(main.Status.Phase))
			return ctrl.Result{}, nil // the main Branch watch brings the project back
		}
		sup := r.Connector.Supervisor(p.Namespace, v1alpha1.MainBranch, token)
		if _, err := call(ctx, bootstrapTimeout, sup.Bootstrap); err != nil {
			setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionMainReady, false, "BootstrapFailed", err.Error())
			return ctrl.Result{}, err
		}
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionMainReady, true, "Bootstrapped", "")
	}

	// The volume's usage, refreshed while the project lives; it also tells whether the agent
	// still answers.
	u, err := call(ctx, callTimeout, agent.Usage)
	if err != nil {
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionAgentReady, false, "AgentUnavailable", err.Error())
		return ctrl.Result{RequeueAfter: waitShort}, nil
	}
	setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionAgentReady, true, "Answers", "")
	p.Status.Volume.CapacityBytes, p.Status.Volume.UsedBytes = u.CapacityBytes, u.UsedBytes
	return ctrl.Result{RequeueAfter: usageEvery}, nil
}

// ensureSecret returns the secret, made by newSecret if it does not exist. An existing secret is
// never changed: the pods and main's database hold its values.
func (r *ProjectReconciler) ensureSecret(ctx context.Context, newSecret func(string) (*corev1.Secret, error), ns string) (*corev1.Secret, error) {
	want, err := newSecret(ns)
	if err != nil {
		return nil, err
	}
	var got corev1.Secret
	err = r.Get(ctx, client.ObjectKeyFromObject(want), &got)
	if err == nil {
		return &got, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	// No owner: the secrets, like the volume, go only with the namespace.
	if err := r.Create(ctx, want); err != nil {
		return nil, err
	}
	return want, nil
}

// ensurePVC makes the project volume once. It has no owner: deleting the Project object alone
// must not delete the data; deleting the namespace does (spec 4.1).
func (r *ProjectReconciler) ensurePVC(ctx context.Context, p *v1alpha1.Project) (*corev1.PersistentVolumeClaim, error) {
	want := desiredPVC(p)
	var got corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKeyFromObject(want), &got)
	if err == nil {
		return &got, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err := r.Create(ctx, want); err != nil {
		return nil, err
	}
	return want, nil
}

// ensureAgent keeps the agent's service and pod. A pod that ended, or that runs old settings, is
// deleted, and the next reconcile makes it anew.
func (r *ProjectReconciler) ensureAgent(ctx context.Context, p *v1alpha1.Project, projectID string) error {
	wantSvc := desiredAgentService(p.Namespace)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: wantSvc.Name, Namespace: wantSvc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = wantSvc.Labels
		svc.Spec.Selector, svc.Spec.Ports = wantSvc.Spec.Selector, wantSvc.Spec.Ports
		return controllerutil.SetControllerReference(p, svc, r.Scheme)
	}); err != nil {
		return err
	}
	want := desiredAgentPod(p, projectID, r.AgentImage)
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKeyFromObject(want), &pod)
	switch {
	case apierrors.IsNotFound(err):
		if err := controllerutil.SetControllerReference(p, want, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, want)
	case err != nil:
		return err
	case pod.DeletionTimestamp != nil:
		return nil // on its way out; its deletion brings the project back
	case pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded ||
		pod.Annotations[AnnotationSpecHash] != want.Annotations[AnnotationSpecHash]:
		logf.FromContext(ctx).Info("replacing the agent pod", "phase", pod.Status.Phase)
		return client.IgnoreNotFound(r.Delete(ctx, &pod))
	}
	return nil
}

// ensureMainBranch makes the main Branch once (spec 7.1 step 5). The Project owns it.
func (r *ProjectReconciler) ensureMainBranch(ctx context.Context, p *v1alpha1.Project) (*v1alpha1.Branch, error) {
	var main v1alpha1.Branch
	err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: v1alpha1.MainBranch}, &main)
	if err == nil {
		return &main, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	main = v1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.MainBranch, Namespace: p.Namespace},
		Spec: v1alpha1.BranchSpec{
			Source:    v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest},
			Protected: true,
			State:     v1alpha1.StateRunning,
		},
	}
	if err := controllerutil.SetControllerReference(p, &main, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, &main); err != nil {
		return nil, err
	}
	return &main, nil
}

// summarize sets the phase and the Ready condition from the four conditions of spec 4.2.
func summarize(p *v1alpha1.Project) {
	ready := func(status bool, reason, msg string) {
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionReady, status, reason, msg)
	}
	if _, ok := ProjectID(p.Namespace); !ok {
		p.Status.Phase = v1alpha1.ProjectFailed
		ready(false, "InvalidNamespace", "a Project lives in a namespace named cns-<projectId>")
		return
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReflinkSupported); c != nil && c.Status == metav1.ConditionFalse {
		p.Status.Phase = v1alpha1.ProjectFailed
		ready(false, ReasonReflinkUnsupported, c.Message)
		return
	}
	var waiting []string
	for _, t := range []string{v1alpha1.ConditionVolumeReady, v1alpha1.ConditionAgentReady, v1alpha1.ConditionReflinkSupported, v1alpha1.ConditionMainReady} {
		if !meta.IsStatusConditionTrue(p.Status.Conditions, t) {
			waiting = append(waiting, t)
		}
	}
	if len(waiting) == 0 {
		p.Status.Phase = v1alpha1.ProjectReady
		ready(true, "Ready", "")
		return
	}
	p.Status.Phase = v1alpha1.ProjectProvisioning
	ready(false, "Provisioning", "waiting for "+strings.Join(waiting, ", "))
}

// setCondition sets one condition; its transition time changes only with its status.
func setCondition(conds *[]metav1.Condition, generation int64, typ string, status bool, reason, msg string) {
	s := metav1.ConditionFalse
	if status {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conds, metav1.Condition{Type: typ, Status: s, Reason: reason, Message: msg, ObservedGeneration: generation})
}

func phaseOrNone(p v1alpha1.BranchPhase) string {
	if p == "" {
		return "not reconciled yet"
	}
	return string(p)
}
```

- [ ] **Step 3: 통과 확인**

Run: `go mod tidy && gofmt -l . && go vet ./internal/controller/ && go test -count=1 ./internal/controller/`
Expected: `ok`. envtest 테스트는 건너뛴다.

Run: `export KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2 use 1.37.0 --bin-dir "$PWD/bin/envtest" -p path) && go test -count=1 -race -v ./internal/controller/ 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected: 모두 PASS다. Project 테스트 여섯: `TestProjectMakesItsObjects`, `TestProjectBecomesReady`, `TestProjectFailsWithoutReflink`, `TestProjectWaitsForItsAgent`, `TestAgentPodFollowsTheProjectSettings`, `TestProjectOutsideAProjectNamespaceFails`. 같은 명령을 세 번 돌려도 매번 통과해야 한다(흔들리는 테스트 확인).

- [ ] **Step 4: 커밋**

```bash
git add go.mod go.sum internal/controller/
git commit -m "feat(controller): Project 조정 — 하위 리소스, reflink 점검, main 브랜치, 부트스트랩" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 7: Branch 조정 — 데이터, 파드, 정지·기동

**Files:**
- Create: `internal/controller/branch_controller.go`
- Modify: `internal/controller/suite_test.go`(BranchReconciler 등록)
- Test: `internal/controller/branch_controller_test.go`

**Interfaces:**
- Consumes: Task 4 명세 함수, Task 5 연결·오류 분류, Task 6 `setCondition`·테스트 묶음
- Produces:
  - `BranchReconciler{Client; Scheme; Connector; PostgresImage string; Now func() time.Time}`: `SetupWithManager`, `Reconcile`, `reconcile(ctx, p, b, token)`
  - 단계:
    - 데이터: `ensureData(ctx, b, agent)`, `dataFailed(b, err, reason)`, `permanentDataFailure(reason)`
    - 파드·서비스: `ensureService`, `ensurePod(ctx, p, b) (bool, error)`
    - 상태: `ensureState(ctx, b, sup)`, `settled(b, phase)`
  - 재시도 억제: `holdOff(key, d)`, `heldOff(key)`, `(*BranchReconciler).retryLater(b, err)`
  - 도우미: `token(ctx, ns)`, `branchesOf(match)`, `parseGeneration("data-N")`
  - 테스트 도우미: `readyProject(t)`, `getBranch`, `waitPhase(t, ns, name, phase)`, `createBranch(t, ns, name, parent, change...)`, `setState`

Branch 조정은 다음을 한다.
- **프로젝트가 준비될 때까지 기다린다.** Project의 `ReflinkSupported`가 참이 될 때까지 기다리고, Project가 바뀌면 깬다.
- **데이터:** main은 `InitMain`, 다른 브랜치는 `CreateBranchData`로 만든다. 영구 오류면 Failed로 둔다. 부모가 실행 중이 아니면 기다리다가, 부모가 바뀌면 깬다.
- **서비스와 파드:** 데이터가 생긴 뒤에만 둔다(subPath 디렉터리가 있어야 하므로).
- **상태:** 감독 프로세스의 상태를 `spec.state`에 맞춘다.
- **Retry-After:** 다른 이벤트가 깨워도 지켜지게 브랜치별로 메모리에 기억한다(`holdOff`).

`reconcile`의 맨 위에서 에이전트와 감독 프로세스 클라이언트(`agent`, `sup`)를 만든다. Task 9가 같은 변수를 쓴다.

- [ ] **Step 1: 실패하는 테스트와 등록**

`internal/controller/branch_controller_test.go`:

```go
package controller

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/supervisorapi"
)

// readyProject makes a project whose main branch runs.
func readyProject(t *testing.T) (string, *fakeProject) {
	t.Helper()
	ns, fp := createProject(t, nil)
	bindVolume(t, ns)
	waitPhase(t, ns, v1alpha1.MainBranch, v1alpha1.BranchRunning)
	return ns, fp
}

func getBranch(t *testing.T, ns, name string) v1alpha1.Branch {
	t.Helper()
	var b v1alpha1.Branch
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func waitPhase(t *testing.T, ns, name string, phase v1alpha1.BranchPhase) v1alpha1.Branch {
	t.Helper()
	var b v1alpha1.Branch
	eventually(t, 15*time.Second, func() string {
		if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &b); err != nil {
			return name + ": " + err.Error()
		}
		if b.Status.Phase != phase {
			return fmt.Sprintf("%s is %q, DataReady %s, Ready %s", name, b.Status.Phase,
				cond(b.Status.Conditions, v1alpha1.ConditionDataReady), cond(b.Status.Conditions, v1alpha1.ConditionReady))
		}
		return ""
	})
	return b
}

// createBranch makes a branch as the API server does (spec 9.2).
func createBranch(t *testing.T, ns, name, parent string, change ...func(*v1alpha1.Branch)) {
	t.Helper()
	b := &v1alpha1.Branch{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.BranchSpec{Parent: parent}}
	for _, c := range change {
		c(b)
	}
	if err := kenv.Client.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

func setState(t *testing.T, ns, name string, state v1alpha1.DesiredState) {
	t.Helper()
	eventually(t, 10*time.Second, func() string {
		b := getBranch(t, ns, name)
		b.Spec.State = state
		if err := kenv.Client.Update(context.Background(), &b); err != nil {
			return err.Error()
		}
		return ""
	})
}

func TestMainRunsAndTheProjectGetsReady(t *testing.T) {
	ns, fp := readyProject(t)
	main := getBranch(t, ns, v1alpha1.MainBranch)
	if main.Status.DataGeneration != 1 || !meta.IsStatusConditionTrue(main.Status.Conditions, v1alpha1.ConditionDataReady) ||
		main.Status.Timings.DataReadyAt == nil || main.Status.Timings.RunningAt == nil {
		t.Errorf("main status %+v", main.Status)
	}
	if e := main.Status.Endpoint; e == nil || e.Host != "br-main."+ns+".svc" || e.Port != 5432 {
		t.Errorf("endpoint %+v", e)
	}
	var pod corev1.Pod
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "br-main"}, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.Containers[0].Image != testPostgresImage || !metav1.IsControlledBy(&pod, &main) {
		t.Errorf("pod image %s owners %+v", pod.Spec.Containers[0].Image, pod.OwnerReferences)
	}
	var svc corev1.Service
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "br-main"}, &svc); err != nil || !svc.Spec.PublishNotReadyAddresses {
		t.Errorf("service: %v %+v", err, svc.Spec)
	}
	if !slices.Contains(fp.callLog(), "InitMain") {
		t.Errorf("calls %q", fp.callLog())
	}
	eventually(t, 10*time.Second, func() string {
		if p := getProject(t, ns); p.Status.Phase != v1alpha1.ProjectReady {
			return "project " + string(p.Status.Phase)
		}
		return ""
	})
}

func TestBranchFromItsRunningParent(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	b := waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if f := b.Status.CreatedFrom; f == nil || f.Branch != "main" || f.BackupStartLSN != "0/3000028" {
		t.Errorf("createdFrom %+v", f)
	}
	if !slices.Contains(fp.callLog(), "CreateBranchData(dev,main,ParentLatest)") {
		t.Errorf("calls %q", fp.callLog())
	}
	if state, gen, _ := fp.postgres("dev"); state != supervisorapi.PGRunning || gen != 1 {
		t.Errorf("dev's postgres %s on %d", state, gen)
	}
}

func TestBranchWaitsForItsParentToRun(t *testing.T) {
	ns, _ := readyProject(t)
	setState(t, ns, "main", v1alpha1.StateStopped)
	waitPhase(t, ns, "main", v1alpha1.BranchStopped)
	createBranch(t, ns, "dev", "main")
	eventually(t, 10*time.Second, func() string {
		if c := cond(getBranch(t, ns, "dev").Status.Conditions, v1alpha1.ConditionDataReady); c != "False/ParentNotRunning" {
			return "DataReady " + c
		}
		return ""
	})
	setState(t, ns, "main", v1alpha1.StateRunning)
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

func TestBranchFromARestorePoint(t *testing.T) {
	ns, fp := readyProject(t)
	fp.addRestorePoint("rp-20261007T031500Z-ab12", "main")
	createBranch(t, ns, "fix", "main", func(b *v1alpha1.Branch) {
		b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-ab12"}
	})
	b := waitPhase(t, ns, "fix", v1alpha1.BranchRunning)
	if f := b.Status.CreatedFrom; f == nil || f.RestorePointID != "rp-20261007T031500Z-ab12" || f.BackupStartLSN != "" {
		t.Errorf("createdFrom %+v", f)
	}
}

func TestBranchFromAMissingRestorePointFails(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "lost", "main", func(b *v1alpha1.Branch) {
		b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-zzzz"}
	})
	b := waitPhase(t, ns, "lost", v1alpha1.BranchFailed)
	if c := cond(b.Status.Conditions, v1alpha1.ConditionDataReady); c != "False/"+ReasonSourceNotFound {
		t.Errorf("DataReady %s", c)
	}
	n := countCalls(fp, "CreateBranchData(lost") // a permanent failure is not retried
	time.Sleep(time.Second)
	if m := countCalls(fp, "CreateBranchData(lost"); m != n {
		t.Errorf("%d copies, then %d", n, m)
	}
	var pod corev1.Pod
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "br-lost"}, &pod); !apierrors.IsNotFound(err) {
		t.Errorf("a branch without data got a pod: %v", err)
	}
}

func TestRateLimitedCopyComesBackAfterRetryAfter(t *testing.T) {
	ns, fp := readyProject(t)
	fp.failNext("CreateBranchData", &agentclient.APIError{StatusCode: 429, Code: agentapi.CodeRateLimited, RetryAfter: time.Second})
	start := time.Now()
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if n := countCalls(fp, "CreateBranchData(dev"); n < 2 {
		t.Errorf("%d copies, want a refused one and then more", n)
	}
	if took := time.Since(start); took < time.Second {
		t.Errorf("retried after %s, before Retry-After", took)
	}
}

func TestStopAndStart(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	setState(t, ns, "dev", v1alpha1.StateStopped)
	waitPhase(t, ns, "dev", v1alpha1.BranchStopped)
	if state, _, _ := fp.postgres("dev"); state != supervisorapi.PGStopped {
		t.Fatalf("postgres %s", state)
	}
	setState(t, ns, "dev", v1alpha1.StateRunning)
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

func TestAStoppedBranchIsNotStarted(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "idle", "main", func(b *v1alpha1.Branch) { b.Spec.State = v1alpha1.StateStopped })
	waitPhase(t, ns, "idle", v1alpha1.BranchStopped)
	time.Sleep(500 * time.Millisecond)
	if n := countCalls(fp, "Start(idle)"); n != 0 {
		t.Errorf("%d starts", n)
	}
}

func TestStartFailureIsReportedAndRetried(t *testing.T) {
	ns, fp := readyProject(t)
	fp.failToStart("dev", 1)
	createBranch(t, ns, "dev", "main")
	b := waitPhase(t, ns, "dev", v1alpha1.BranchFailed)
	if c := cond(b.Status.Conditions, v1alpha1.ConditionReady); c != "False/StartFailed" {
		t.Errorf("Ready %s", c)
	}
	eventually(t, 10*time.Second, func() string {
		if n := countCalls(fp, "Start(dev)"); n < 2 {
			return fmt.Sprintf("%d starts", n)
		}
		return ""
	})
	fp.failToStart("dev", 0) // whatever kept it from starting is fixed
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

func TestADeletedPodIsMadeAgain(t *testing.T) {
	ns, _ := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: ns, Name: "br-dev"}
	if err := kenv.Client.Get(context.Background(), key, &pod); err != nil {
		t.Fatal(err)
	}
	old := pod.UID
	if err := kenv.Client.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() string {
		var p corev1.Pod
		if err := kenv.Client.Get(context.Background(), key, &p); err != nil || p.UID == old {
			return fmt.Sprintf("pod: %v", err)
		}
		return ""
	})
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}
```

`internal/controller/suite_test.go`에서 다음 줄을 찾아:

```go
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
```

그 바로 앞에 다음을 넣는다:

```go
	if err := (&BranchReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Connector: conn, PostgresImage: testPostgresImage}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
```

Run: `go test ./internal/controller/`
Expected: FAIL — 빌드 실패(`undefined: BranchReconciler`)

- [ ] **Step 2: 구현**

`internal/controller/branch_controller.go`:

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

// The Branch reconcile's permissions (config/rbac/role.yaml, make generate).
//
// +kubebuilder:rbac:groups=consonance.io,resources=branches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=consonance.io,resources=branches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=consonance.io,resources=branches/finalizers,verbs=update
// +kubebuilder:rbac:groups=consonance.io,resources=projects,verbs=get;list;watch

// BranchReconciler carries out spec 7.1's Branch steps.
type BranchReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Connector     Connector
	PostgresImage string
	Now           func() time.Time // nil means time.Now

	mu        sync.Mutex
	notBefore map[client.ObjectKey]time.Time // a rate-limited branch is not tried again before this
}

// holdOff keeps a branch from being tried again for d: a rate limit's Retry-After is honored
// even when an event wakes the branch sooner.
func (r *BranchReconciler) holdOff(key client.ObjectKey, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notBefore == nil {
		r.notBefore = map[client.ObjectKey]time.Time{}
	}
	r.notBefore[key] = r.now().Add(d)
}

// heldOff returns how long a branch is still held off.
func (r *BranchReconciler) heldOff(key client.ObjectKey) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.notBefore[key].Sub(r.now())
	if d <= 0 {
		delete(r.notBefore, key)
		return 0
	}
	return d
}

func (r *BranchReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager registers the reconciler. A branch is looked at again when its pod or service
// changes, when its project changes (a branch waits for the project's agent), and when a branch
// in its namespace changes (a new branch waits for its parent to run).
func (r *BranchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Branch{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Watches(&v1alpha1.Project{}, handler.EnqueueRequestsFromMapFunc(r.branchesOf(func(*v1alpha1.Branch) bool { return true }))).
		Watches(&v1alpha1.Branch{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			parent := o.GetName()
			return r.branchesOf(func(b *v1alpha1.Branch) bool { return b.Spec.Parent == parent && b.Status.DataGeneration == 0 })(ctx, o)
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 8, RateLimiter: rateLimiter()}).
		Complete(r)
}

// branchesOf maps an object to the branches of its namespace that match.
func (r *BranchReconciler) branchesOf(match func(*v1alpha1.Branch) bool) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var list v1alpha1.BranchList
		if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			logf.FromContext(ctx).Error(err, "list branches")
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			if match(&list.Items[i]) {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
			}
		}
		return out
	}
}

func (r *BranchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b v1alpha1.Branch
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	id, _ := ProjectID(b.Namespace)
	ctx = logf.IntoContext(ctx, logf.FromContext(ctx).WithValues("projectId", id, "branch", b.Name))

	if d := r.heldOff(req.NamespacedName); d > 0 {
		return ctrl.Result{RequeueAfter: d}, nil
	}
	var p v1alpha1.Project
	if err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: ProjectName}, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err) // the Project watch brings the branch back
	}
	if !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReflinkSupported) {
		return ctrl.Result{}, nil // the project's agent is not ready; the Project watch brings us back
	}
	token, err := r.token(ctx, b.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	before := b.Status.DeepCopy()
	res, err := r.reconcile(ctx, &p, &b, token)
	b.Status.ObservedGeneration = b.Generation
	if !equality.Semantic.DeepEqual(*before, b.Status) {
		if uerr := r.Status().Update(ctx, &b); uerr != nil {
			return ctrl.Result{}, errors.Join(err, uerr)
		}
	}
	return res, err
}

// token is the project's internal token, which the Project reconcile made.
func (r *BranchReconciler) token(ctx context.Context, ns string) (string, error) {
	var s corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: InternalTokenSecret}, &s); err != nil {
		return "", fmt.Errorf("secret %s: %w", InternalTokenSecret, err)
	}
	t := string(s.Data[TokenKey])
	if t == "" {
		return "", fmt.Errorf("secret %s has no %s", InternalTokenSecret, TokenKey)
	}
	return t, nil
}

func (r *BranchReconciler) reconcile(ctx context.Context, p *v1alpha1.Project, b *v1alpha1.Branch, token string) (ctrl.Result, error) {
	if c := meta.FindStatusCondition(b.Status.Conditions, v1alpha1.ConditionDataReady); c != nil &&
		c.Status == metav1.ConditionFalse && permanentDataFailure(c.Reason) {
		return ctrl.Result{}, nil // spec 10: a permanent failure stays failed
	}

	agent := r.Connector.Agent(b.Namespace, token)
	sup := r.Connector.Supervisor(b.Namespace, b.Name, token)

	// Step 1: data.
	if b.Status.DataGeneration == 0 {
		if res, err := r.ensureData(ctx, b, agent); b.Status.DataGeneration == 0 {
			return res, err
		}
	}

	// Step 2: service and pod. A pod is made only once the data exists, so its subPath does.
	if err := r.ensureService(ctx, b); err != nil {
		return ctrl.Result{}, err
	}
	b.Status.Endpoint = branchEndpoint(b)
	if ready, err := r.ensurePod(ctx, p, b); !ready || err != nil {
		return ctrl.Result{}, err // a pod being replaced: its deletion brings the branch back
	}

	// Step 3: the state.
	return r.ensureState(ctx, b, sup)
}

// permanentDataFailure reports a DataReady reason that retrying cannot fix (spec 10).
func permanentDataFailure(reason string) bool {
	switch reason {
	case ReasonReflinkUnsupported, ReasonInsufficientStorage, ReasonSourceNotFound, ReasonInvalid:
		return true
	}
	return false
}

// ensureData makes the branch's data: main's with init-main (spec 7.1 step 5: main's data is
// made only there), any other's by copying its parent or a restore point. Both calls are safe to
// repeat: a retry answers with the data made already.
func (r *BranchReconciler) ensureData(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI) (ctrl.Result, error) {
	b.Status.Phase = v1alpha1.BranchCreating
	var gen int
	if isMain(b) {
		g, err := call(ctx, initTimeout, agent.InitMain)
		if err != nil {
			return r.dataFailed(b, err, "InitMainFailed")
		}
		gen = g.Generation
	} else {
		req := agentapi.CreateBranchDataRequest{
			Parent: b.Spec.Parent,
			Source: agentapi.Source{Type: agentapi.SourceType(b.Spec.Source.Type), RestorePointID: b.Spec.Source.RestorePointID},
		}
		d, err := call(ctx, copyTimeout, func(ctx context.Context) (agentapi.BranchData, error) {
			return agent.CreateBranchData(ctx, b.Name, req)
		})
		if err != nil {
			return r.dataFailed(b, err, "CopyFailed")
		}
		gen = d.Generation
		if b.Status.CreatedFrom == nil {
			b.Status.CreatedFrom = &v1alpha1.CreatedFrom{Branch: b.Spec.Parent, RestorePointID: b.Spec.Source.RestorePointID}
		}
		if d.BackupStartLSN != "" { // empty when a retry found the data made already
			b.Status.CreatedFrom.BackupStartLSN = d.BackupStartLSN
		}
	}
	b.Status.DataGeneration = int32(gen)
	b.Status.Phase = v1alpha1.BranchDataReady
	setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionDataReady, true, "Made", "")
	if b.Status.Timings.DataReadyAt == nil {
		b.Status.Timings.DataReadyAt = &metav1.Time{Time: r.now()}
	}
	logf.FromContext(ctx).Info("branch data ready", "op", "data", "generation", gen)
	return ctrl.Result{}, nil
}

// dataFailed records why the data could not be made. A permanent failure fails the branch; any
// other is retried.
func (r *BranchReconciler) dataFailed(b *v1alpha1.Branch, err error, reason string) (ctrl.Result, error) {
	permanent := permanentReason(err)
	if permanent == "" && !isMain(b) && agentCode(err) == agentapi.CodeNotFound {
		permanent = ReasonSourceNotFound // no parent data, or no such restore point
	}
	if permanent != "" {
		b.Status.Phase = v1alpha1.BranchFailed
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionDataReady, false, permanent, err.Error())
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, false, permanent, err.Error())
		return ctrl.Result{}, nil
	}
	if agentCode(err) == agentapi.CodeNotRunning {
		reason = "ParentNotRunning"
	}
	setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionDataReady, false, reason, err.Error())
	return r.retryLater(b, err)
}

// retryLater is the package's retryLater, which also holds the branch off for a rate limit.
func (r *BranchReconciler) retryLater(b *v1alpha1.Branch, err error) (ctrl.Result, error) {
	res, rerr := retryLater(err)
	if agentCode(err) == agentapi.CodeRateLimited {
		r.holdOff(client.ObjectKeyFromObject(b), res.RequeueAfter)
	}
	return res, rerr
}

// ensureService keeps the branch's service.
func (r *BranchReconciler) ensureService(ctx context.Context, b *v1alpha1.Branch) error {
	want := desiredBranchService(b)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = want.Labels
		svc.Spec.Selector, svc.Spec.Ports = want.Spec.Selector, want.Spec.Ports
		svc.Spec.PublishNotReadyAddresses = want.Spec.PublishNotReadyAddresses
		return controllerutil.SetControllerReference(b, svc, r.Scheme)
	})
	return err
}

// ensurePod makes the branch's pod if it is missing and replaces one that ended (spec 7.1 step 2).
// It reports whether a pod is there to talk to.
func (r *BranchReconciler) ensurePod(ctx context.Context, p *v1alpha1.Project, b *v1alpha1.Branch) (bool, error) {
	want := desiredBranchPod(p, b, r.PostgresImage)
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKeyFromObject(want), &pod)
	switch {
	case apierrors.IsNotFound(err):
		if err := controllerutil.SetControllerReference(b, want, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, want); err != nil {
			return false, err
		}
		return true, nil
	case err != nil:
		return false, err
	case pod.DeletionTimestamp != nil:
		return false, nil
	case pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded:
		logf.FromContext(ctx).Info("replacing the branch pod", "phase", pod.Status.Phase)
		return false, client.IgnoreNotFound(r.Delete(ctx, &pod))
	}
	return true, nil
}

// ensureState brings PostgreSQL to spec.state (spec 7.1 step 3, 9.5).
func (r *BranchReconciler) ensureState(ctx context.Context, b *v1alpha1.Branch, sup SupervisorAPI) (ctrl.Result, error) {
	st, err := call(ctx, callTimeout, sup.Status)
	if err != nil { // the pod is not up yet, or restarts
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, false, "SupervisorUnavailable", err.Error())
		return ctrl.Result{RequeueAfter: waitShort}, nil
	}
	if n, ok := parseGeneration(st.Generation); ok {
		b.Status.DataGeneration = int32(n)
	}
	if b.Spec.State == v1alpha1.StateStopped {
		switch st.PG {
		case supervisorapi.PGStopped:
			r.settled(b, v1alpha1.BranchStopped)
			return ctrl.Result{}, nil
		case supervisorapi.PGStopping:
			b.Status.Phase = v1alpha1.BranchStopping
			return ctrl.Result{RequeueAfter: waitPoll}, nil
		}
		b.Status.Phase = v1alpha1.BranchStopping
		st, err := call(ctx, stopTimeout, sup.Stop)
		if err != nil {
			return ctrl.Result{}, err
		}
		if st.PG == supervisorapi.PGStopped {
			r.settled(b, v1alpha1.BranchStopped)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: waitPoll}, nil
	}

	switch st.PG {
	case supervisorapi.PGRunning:
		r.settled(b, v1alpha1.BranchRunning)
		return ctrl.Result{}, nil
	case supervisorapi.PGStarting:
		b.Status.Phase = v1alpha1.BranchStarting
		return ctrl.Result{RequeueAfter: waitPoll}, nil
	case supervisorapi.PGStopping:
		b.Status.Phase = v1alpha1.BranchStopping
		return ctrl.Result{RequeueAfter: waitPoll}, nil
	}
	b.Status.Phase = v1alpha1.BranchStarting
	st, err = call(ctx, startTimeout, sup.Start)
	switch {
	case err == nil && st.PG == supervisorapi.PGRunning:
		r.settled(b, v1alpha1.BranchRunning)
		return ctrl.Result{}, nil
	case err == nil, stillStarting(err), supervisorStatus(err) == 409:
		// Still starting (503), or busy with another request (409): look again.
		return ctrl.Result{RequeueAfter: waitPoll}, nil
	case supervisorStatus(err) != 0:
		// PostgreSQL did not start on this data (500). Tried again with backoff.
		b.Status.Phase = v1alpha1.BranchFailed
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, false, "StartFailed", err.Error())
	}
	return ctrl.Result{}, err
}

// settled records that the branch reached its desired state.
func (r *BranchReconciler) settled(b *v1alpha1.Branch, phase v1alpha1.BranchPhase) {
	b.Status.Phase = phase
	setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, true, string(phase), "")
	if phase == v1alpha1.BranchRunning && b.Status.Timings.RunningAt == nil {
		b.Status.Timings.RunningAt = &metav1.Time{Time: r.now()}
	}
}

// parseGeneration reads a supervisor's generation, "data-N".
func parseGeneration(name string) (int, bool) {
	s, ok := strings.CutPrefix(name, "data-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}
```

- [ ] **Step 3: 통과 확인**

Run: `gofmt -l . && go vet ./internal/controller/ && go test -count=1 ./internal/controller/`
Expected: `ok`

Run(KUBEBUILDER_ASSETS를 설정한 채): `for i in 1 2 3; do go test -count=1 -race ./internal/controller/ | tail -1; done`
Expected: 세 번 모두 `ok`. 새 테스트는 아홉 개다:
- `TestMainRunsAndTheProjectGetsReady`, `TestBranchFromItsRunningParent`, `TestBranchWaitsForItsParentToRun`
- `TestBranchFromARestorePoint`, `TestBranchFromAMissingRestorePointFails`
- `TestRateLimitedCopyComesBackAfterRetryAfter`, `TestStopAndStart`, `TestAStoppedBranchIsNotStarted`, `TestStartFailureIsReportedAndRetried`, `TestADeletedPodIsMadeAgain`

Task 6의 `runMain`은 이제 Branch 조정과 함께 돈다. 이미 만든 파드는 그대로 두므로 그 테스트도 통과해야 한다.

- [ ] **Step 4: 커밋**

```bash
git add internal/controller/
git commit -m "feat(controller): Branch 조정 — 데이터, 파드·서비스, 정지·기동, Retry-After 준수" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 8: 브랜치 삭제

**Files:**
- Create: `internal/controller/branch_delete.go`
- Modify: `internal/controller/branch_controller.go`(파이널라이저), `internal/controller/fake_test.go`(실행 중인 브랜치의 데이터 삭제 거부)
- Test: `internal/controller/branch_delete_test.go`

**Interfaces:**
- Consumes: Task 7 `BranchReconciler`, `token`, `retryLater`
- Produces: `(*BranchReconciler).finalize(ctx, b)`, `dataToDelete(ctx, b) (bool, error)`; 테스트 도우미 `deleteBranch`, `waitGone`

삭제 순서는 다음과 같다(스펙 7.1 Branch 5단계, 계획 2 이월).
1. Service와 파드를 지운다.
2. 파드 객체가 완전히 사라질 때까지 기다린다. 에이전트는 응답 없는 감독 프로세스를 "파드 없음"으로 보기 때문이다.
3. 에이전트 `DeleteBranchData`를 부른다.
4. 파이널라이저를 뗀다.

데이터를 지우지 않는 경우도 있다. main이면 프로젝트의 데이터다. Project가 없거나 삭제 중이면 네임스페이스 삭제가 볼륨을 가져간다. reflink 점검을 통과한 적이 없으면 데이터가 있을 수 없다. 가짜 에이전트도 진짜처럼 감독 프로세스가 답하는데 정지 상태가 아니면 데이터 삭제를 409로 거부하게 고친다. 그래야 순서가 테스트된다.

- [ ] **Step 1: 실패하는 테스트**

`internal/controller/branch_delete_test.go`:

```go
package controller

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
)

func deleteBranch(t *testing.T, ns, name string) {
	t.Helper()
	b := getBranch(t, ns, name)
	if err := kenv.Client.Delete(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
}

func waitGone(t *testing.T, ns, name string, obj client.Object) {
	t.Helper()
	eventually(t, 15*time.Second, func() string {
		err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, obj)
		if !apierrors.IsNotFound(err) {
			return fmt.Sprintf("%s still there: %v", name, err)
		}
		return ""
	})
}

func TestDeletingABranchRemovesItsPodAndData(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	b := waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if !slices.Contains(b.Finalizers, BranchFinalizer) {
		t.Fatalf("finalizers %q", b.Finalizers)
	}
	deleteBranch(t, ns, "dev")
	waitGone(t, ns, "dev", &v1alpha1.Branch{})
	waitGone(t, ns, "br-dev", &corev1.Pod{})
	waitGone(t, ns, "br-dev", &corev1.Service{})
	if _, _, ok := fp.generations("dev"); ok {
		t.Error("dev's data is still on the volume")
	}
	if !slices.Contains(fp.callLog(), "DeleteBranchData(dev)") {
		t.Errorf("calls %q", fp.callLog())
	}
	if _, _, ok := fp.generations("main"); !ok {
		t.Error("main's data went too")
	}
}

func TestDeletingMainKeepsItsData(t *testing.T) {
	ns, fp := readyProject(t)
	main := getBranch(t, ns, v1alpha1.MainBranch)
	deleteBranch(t, ns, v1alpha1.MainBranch)
	// The Project makes main again, on the same data.
	eventually(t, 15*time.Second, func() string {
		var b v1alpha1.Branch
		if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: v1alpha1.MainBranch}, &b); err != nil {
			return err.Error()
		}
		if b.UID == main.UID || b.Status.Phase != v1alpha1.BranchRunning {
			return fmt.Sprintf("main %s is %s", b.UID, b.Status.Phase)
		}
		return ""
	})
	if slices.Contains(fp.callLog(), "DeleteBranchData(main)") {
		t.Errorf("calls %q", fp.callLog())
	}
	if cur, _, ok := fp.generations("main"); !ok || cur != 1 {
		t.Errorf("main's data: %d %v", cur, ok)
	}
}

func TestDeletingABranchOfAGoneProjectLeavesTheAgentAlone(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	p := getProject(t, ns)
	if err := kenv.Client.Delete(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	waitGone(t, ns, ProjectName, &v1alpha1.Project{})
	deleteBranch(t, ns, "dev")
	waitGone(t, ns, "dev", &v1alpha1.Branch{})
	if slices.Contains(fp.callLog(), "DeleteBranchData(dev)") {
		t.Errorf("the agent was called for a project that is gone: %q", fp.callLog())
	}
}

func TestDeletingRetriesAnAgentThatDoesNotAnswer(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	fp.failNext("DeleteBranchData", agentErr(502, agentapi.CodeSupervisorUnavailable, "no answer"))
	deleteBranch(t, ns, "dev")
	waitGone(t, ns, "dev", &v1alpha1.Branch{})
	if n := countCalls(fp, "DeleteBranchData(dev)"); n < 2 {
		t.Errorf("%d deletes", n)
	}
}

func TestAFailedBranchCanBeDeleted(t *testing.T) {
	ns, _ := readyProject(t)
	createBranch(t, ns, "lost", "main", func(b *v1alpha1.Branch) {
		b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-zzzz"}
	})
	waitPhase(t, ns, "lost", v1alpha1.BranchFailed)
	deleteBranch(t, ns, "lost")
	waitGone(t, ns, "lost", &v1alpha1.Branch{})
}
```

Run(KUBEBUILDER_ASSETS를 설정한 채): `go test -count=1 -run 'Delet|AFailedBranch' ./internal/controller/`
Expected: FAIL. `TestDeletingABranchRemovesItsPodAndData: finalizers []`와 `TestDeletingRetriesAnAgentThatDoesNotAnswer: 0 deletes`가 실패한다. 파이널라이저가 없어 브랜치가 곧바로 사라지고 데이터는 남는다. 나머지 셋은 파이널라이저 없이도 맞는 동작이라 지금도 통과한다.

- [ ] **Step 2: 구현**

`internal/controller/branch_delete.go`:

```go
package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"consonance/api/v1alpha1"
)

// finalize deletes a branch (spec 7.1 step 5, 9.6): its service and pod, then, once the pod
// object is gone, its data, then the finalizer. The agent removes a branch's data when its
// supervisor does not answer, taking that for "the pod is gone", so the pod must really be gone
// first (carry-over from plan 2).
func (r *BranchReconciler) finalize(ctx context.Context, b *v1alpha1.Branch) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(b, BranchFinalizer) {
		return ctrl.Result{}, nil
	}
	if b.Status.Phase != v1alpha1.BranchDeleting {
		b.Status.Phase = v1alpha1.BranchDeleting
		if err := r.Status().Update(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
	}
	key := client.ObjectKey{Namespace: b.Namespace, Name: BranchObjectName(b.Name)}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	if err := client.IgnoreNotFound(r.Delete(ctx, svc)); err != nil {
		return ctrl.Result{}, err
	}
	var pod corev1.Pod
	err := r.Get(ctx, key, &pod)
	switch {
	case err == nil:
		if pod.DeletionTimestamp == nil {
			if err := client.IgnoreNotFound(r.Delete(ctx, &pod)); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: waitShort}, nil // the pod's deletion also brings us back
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	deleteData, err := r.dataToDelete(ctx, b)
	if err != nil {
		return ctrl.Result{}, err
	}
	if deleteData {
		token, err := r.token(ctx, b.Namespace)
		if err != nil {
			return ctrl.Result{}, err
		}
		agent := r.Connector.Agent(b.Namespace, token)
		if _, err := call(ctx, callTimeout, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, agent.DeleteBranchData(ctx, b.Name) // a branch without data: success
		}); err != nil {
			return retryLater(err)
		}
		logf.FromContext(ctx).Info("branch data deleted", "op", "delete")
	}
	controllerutil.RemoveFinalizer(b, BranchFinalizer)
	return ctrl.Result{}, r.Update(ctx, b)
}

// dataToDelete reports whether the branch's data must be deleted through the agent. Not main's:
// it is the project's data, which the agent refuses to delete; deleting the main Branch object
// only makes the Project reconcile make it again, on the same data. Not when the project goes:
// deleting the namespace takes the volume, and the agent may be gone already. Not when the
// project never had a working agent: no data can exist.
func (r *BranchReconciler) dataToDelete(ctx context.Context, b *v1alpha1.Branch) (bool, error) {
	if isMain(b) {
		return false, nil
	}
	var p v1alpha1.Project
	err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: ProjectName}, &p)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !p.DeletionTimestamp.IsZero() || !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReflinkSupported) {
		return false, nil
	}
	return true, nil
}
```

`internal/controller/branch_controller.go`에서 다음 줄을 찾아:

```go
	if d := r.heldOff(req.NamespacedName); d > 0 {
```

그 바로 앞에 다음을 넣는다:

```go
	if !b.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &b)
	}
	if controllerutil.AddFinalizer(&b, BranchFinalizer) {
		if err := r.Update(ctx, &b); err != nil {
			return ctrl.Result{}, err
		}
	}
```

`internal/controller/fake_test.go`에서 다음 줄을 찾아:

```go
	delete(p.data, name)
	for id, b := range p.restorePoints {
```

그 바로 앞에 다음을 넣는다:

```go
	// Like the agent: a supervisor that answers must report PostgreSQL stopped.
	if pg, err := p.up(name); err == nil && pg.state != supervisorapi.PGStopped {
		return agentErr(http.StatusConflict, agentapi.CodeConflict, "branch "+name+" is "+string(pg.state))
	}
```

- [ ] **Step 3: 통과 확인**

Run: `gofmt -l . && go vet ./internal/controller/ && go test -count=1 ./internal/controller/`
Expected: `ok`

Run(KUBEBUILDER_ASSETS를 설정한 채): `for i in 1 2 3; do go test -count=1 -race ./internal/controller/ | tail -1; done`
Expected: 세 번 모두 `ok`(새 테스트 `TestDeletingABranchRemovesItsPodAndData`, `TestDeletingMainKeepsItsData`, `TestDeletingABranchOfAGoneProjectLeavesTheAgentAlone`, `TestDeletingRetriesAnAgentThatDoesNotAnswer`, `TestAFailedBranchCanBeDeleted`)

- [ ] **Step 4: 커밋**

```bash
git add internal/controller/
git commit -m "feat(controller): 브랜치 삭제 — 파드가 사라진 뒤 데이터 삭제, main과 지워지는 프로젝트는 데이터를 두고" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 9: 롤백·리셋

**Files:**
- Create: `internal/controller/restore.go`
- Modify: `internal/controller/branch_controller.go`(`saveStatus`, 복원 단계)
- Test: `internal/controller/restore_test.go`

**Interfaces:**
- Consumes: Task 7 `BranchReconciler`(`agent`, `sup`, `retryLater`, `now`), Task 4 `backupBranchName`
- Produces:
  - 판정: `restorePending(b) bool`, `restoreFinished(phase)`
  - 진행: `(*BranchReconciler).restore(ctx, b, agent, sup)`, `saveStatus(ctx, b)`
  - 단계: `restorePrepare`, `restoreStop`, `restoreActivate`, `restoreStart`, `restoreRevert`, `restoreDetach`
  - 도우미: `clearUnactivated`, `deleteGeneration`, `activate`, `freeBackupName`, `ensureBackupBranch`, `restoreFailed`

롤백·리셋은 스펙 9.4의 단계를 따른다.
- **Preparing:** 현재 세대보다 번호가 큰 세대를 지우고 새 세대를 만든다. 번호를 status에 먼저 저장한 뒤, 다시 큰 세대를 지운다.
- **Stopping:** PostgreSQL을 멈춘다.
- **Activating:** `current`를 새 세대로 바꾼다. 409면 Stopping으로 돌아간다.
- **Starting:** 기동한다. 503·409는 다시 본다. 500이면 `reverting`으로 옛 세대를 되돌려 기동하고, 새 세대를 지우고 Failed로 끝낸다.
- **Detaching:** 롤백이면 옛 세대를 백업 브랜치로 옮긴 뒤 그 Branch를 만든다. 리셋이면 복구 지점으로 옮긴다.
- **Done**

단계마다 status를 저장해 재시작해도 이어서 한다. 저장은 `saveStatus`로 한다. 이 함수는 사본을 보내고 resourceVersion만 받아온다. `Status().Update(ctx, b)`는 서버 응답을 b에 덮어써 `b.Status.LastRestore` 포인터를 새 것으로 바꾼다. 루프가 옛 포인터를 들고 있으면 같은 단계를 끝없이 되풀이한다.

새 `requestId`는 진행 중인 요청이 끝난 뒤에 받는다.

- [ ] **Step 1: 실패하는 테스트**

`internal/controller/restore_test.go`:

```go
package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

const testRP = "rp-20261007T031500Z-ab12"

// requestRestore asks for a rollback or reset, as the API server does (spec 9.4).
func requestRestore(t *testing.T, ns, name, requestID string, source v1alpha1.BranchSource) {
	t.Helper()
	eventually(t, 10*time.Second, func() string {
		b := getBranch(t, ns, name)
		b.Spec.Restore = &v1alpha1.RestoreRequest{RequestID: requestID, Source: source}
		if err := kenv.Client.Update(context.Background(), &b); err != nil {
			return err.Error()
		}
		return ""
	})
}

// waitRestore waits for a request to finish and returns the branch.
func waitRestore(t *testing.T, ns, name, requestID string, phase v1alpha1.RestorePhase) v1alpha1.Branch {
	t.Helper()
	var b v1alpha1.Branch
	eventually(t, 15*time.Second, func() string {
		b = getBranch(t, ns, name)
		lr := b.Status.LastRestore
		if lr == nil || lr.RequestID != requestID || lr.Phase != phase {
			return fmt.Sprintf("lastRestore %+v", lr)
		}
		return ""
	})
	return b
}

// inOrder reports whether calls holds want in this order, other calls between them allowed.
func inOrder(calls, want []string) bool {
	i := 0
	for _, c := range calls {
		if i < len(want) && c == want[i] {
			i++
		}
	}
	return i == len(want)
}

func runningDev(t *testing.T) (string, *fakeProject) {
	t.Helper()
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	return ns, fp
}

func TestRollback(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP})
	b := waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	lr := b.Status.LastRestore
	if lr.FromGeneration != 1 || lr.ToGeneration != 2 || lr.FinishedAt == nil || lr.Message != "" {
		t.Errorf("lastRestore %+v", lr)
	}
	if !strings.HasPrefix(lr.BackupBranch, "dev-before-") || !agentapi.ValidBranchName(lr.BackupBranch) {
		t.Errorf("backup branch %q", lr.BackupBranch)
	}
	want := []string{
		"CreateGeneration(dev,RestorePoint)", "Stop(dev)", "ActivateGeneration(dev,2)", "Start(dev)",
		fmt.Sprintf("DetachGeneration(dev,1,%s,false)", lr.BackupBranch),
	}
	if !inOrder(fp.callLog(), want) {
		t.Errorf("calls %q, want in order %q", fp.callLog(), want)
	}
	b = waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if b.Status.DataGeneration != 2 {
		t.Errorf("dataGeneration %d", b.Status.DataGeneration)
	}
	if cur, gens, _ := fp.generations("dev"); cur != 2 || !slices.Equal(gens, []int{2}) {
		t.Errorf("dev on the volume: current %d of %v", cur, gens)
	}
	// The replaced data is a stopped backup branch.
	backup := waitPhase(t, ns, lr.BackupBranch, v1alpha1.BranchStopped)
	if backup.Labels[LabelBackupOf] != "dev" || backup.Spec.Parent != "dev" || backup.Spec.State != v1alpha1.StateStopped {
		t.Errorf("backup %+v %+v", backup.Labels, backup.Spec)
	}
	if n := countCalls(fp, "Start("+lr.BackupBranch+")"); n != 0 {
		t.Errorf("the backup branch was started %d times", n)
	}
}

func TestReset(t *testing.T) {
	ns, fp := runningDev(t)
	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest})
	b := waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	lr := b.Status.LastRestore
	if lr.BackupBranch != "" || !agentapi.ValidRestorePointID(lr.RestorePointID) {
		t.Errorf("lastRestore %+v", lr)
	}
	if !inOrder(fp.callLog(), []string{"CreateGeneration(dev,ParentLatest)", "ActivateGeneration(dev,2)", "DetachGeneration(dev,1,,true)"}) {
		t.Errorf("calls %q", fp.callLog())
	}
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

func TestResetOfMainFails(t *testing.T) {
	ns, fp := readyProject(t)
	requestRestore(t, ns, "main", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest})
	b := waitRestore(t, ns, "main", "r1", v1alpha1.RestoreFailed)
	if !strings.Contains(b.Status.LastRestore.Message, "without a parent") {
		t.Errorf("message %q", b.Status.LastRestore.Message)
	}
	waitPhase(t, ns, "main", v1alpha1.BranchRunning)
	if n := countCalls(fp, "CreateGeneration"); n != 0 {
		t.Errorf("%d generations made", n)
	}
}

func TestARestoreThatDoesNotStartGoesBack(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	fp.failToStart("dev", 2)
	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP})
	b := waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreFailed)
	lr := b.Status.LastRestore
	if !lr.Reverting || !strings.Contains(lr.Message, "did not start") {
		t.Errorf("lastRestore %+v", lr)
	}
	if !inOrder(fp.callLog(), []string{"ActivateGeneration(dev,2)", "Start(dev)", "ActivateGeneration(dev,1)", "Start(dev)", "DeleteGeneration(dev,2)"}) {
		t.Errorf("calls %q", fp.callLog())
	}
	b = waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if b.Status.DataGeneration != 1 {
		t.Errorf("dataGeneration %d", b.Status.DataGeneration)
	}
	if cur, gens, _ := fp.generations("dev"); cur != 1 || !slices.Equal(gens, []int{1}) {
		t.Errorf("dev on the volume: current %d of %v", cur, gens)
	}
	if state, gen, _ := fp.postgres("dev"); state != supervisorapi.PGRunning || gen != 1 {
		t.Errorf("postgres %s on %d", state, gen)
	}
}

func TestARestoreClearsGenerationsEarlierTriesLeft(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	// An earlier try made data-2 and lost the answer.
	if _, err := conn.Agent(ns, "").CreateGeneration(context.Background(), "dev", agentapi.CreateGenerationRequest{
		Source: agentapi.Source{Type: agentapi.SourceRestorePoint, RestorePointID: testRP},
	}); err != nil {
		t.Fatal(err)
	}
	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP})
	waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	// data-2 was deleted and made anew: the agent numbers from the highest generation left.
	if !inOrder(fp.callLog(), []string{"ListGenerations(dev)", "DeleteGeneration(dev,2)", "CreateGeneration(dev,RestorePoint)", "ActivateGeneration(dev,2)"}) {
		t.Errorf("calls %q", fp.callLog())
	}
	if cur, gens, _ := fp.generations("dev"); cur != 2 || !slices.Equal(gens, []int{2}) {
		t.Errorf("dev on the volume: current %d of %v", cur, gens)
	}
}

func TestARestoreGoesOnAfterAFailedStep(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	fp.failNext("ActivateGeneration", agentErr(502, agentapi.CodeSupervisorUnavailable, "no answer"))
	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP})
	waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	if n := countCalls(fp, "CreateGeneration"); n != 1 {
		t.Errorf("%d generations made; the restore did not go on from where it was", n)
	}
	if n := countCalls(fp, "ActivateGeneration(dev,2)"); n < 2 {
		t.Errorf("%d activations", n)
	}
}

func TestEachRequestRunsOnce(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	rp := v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP}
	requestRestore(t, ns, "dev", "r1", rp)
	waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	setState(t, ns, "dev", v1alpha1.StateStopped) // another reconcile
	waitPhase(t, ns, "dev", v1alpha1.BranchStopped)
	if n := countCalls(fp, "CreateGeneration"); n != 1 {
		t.Fatalf("%d generations made for one request", n)
	}
	requestRestore(t, ns, "dev", "r2", rp)
	waitRestore(t, ns, "dev", "r2", v1alpha1.RestoreDone)
	if n := countCalls(fp, "CreateGeneration"); n != 2 {
		t.Errorf("%d generations made for two requests", n)
	}
	// Both rollbacks ran within a second or two: their backup branches still got two names.
	first, second := getBranch(t, ns, "dev").Status.LastRestore.BackupBranch, ""
	var list v1alpha1.BranchList
	if err := kenv.Client.List(context.Background(), &list, client.InNamespace(ns), client.MatchingLabels{LabelBackupOf: "dev"}); err != nil || len(list.Items) != 2 {
		t.Fatalf("backup branches: %v, %d", err, len(list.Items))
	}
	for _, it := range list.Items {
		if it.Name != first {
			second = it.Name
		}
	}
	if second == "" || second == first {
		t.Errorf("backup branches %q and %q", first, second)
	}
	// A stopped branch is started to check the restored data, then stopped again.
	waitPhase(t, ns, "dev", v1alpha1.BranchStopped)
}
```

Run(KUBEBUILDER_ASSETS를 설정한 채): `go test -count=1 -run 'Rollback|Reset|Restore|EachRequest' ./internal/controller/`
Expected: FAIL — 빌드는 된다. `waitRestore`가 `lastRestore <nil>`로 시간 초과가 난다.

- [ ] **Step 2: 구현**

`internal/controller/restore.go`:

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

// restorePending reports whether a rollback or reset is under way or asked for. One under way is
// finished before a newer request starts.
func restorePending(b *v1alpha1.Branch) bool {
	lr := b.Status.LastRestore
	if lr != nil && !restoreFinished(lr.Phase) {
		return true
	}
	req := b.Spec.Restore
	return req != nil && req.RequestID != "" && (lr == nil || lr.RequestID != req.RequestID)
}

func restoreFinished(p v1alpha1.RestorePhase) bool {
	return p == v1alpha1.RestoreDone || p == v1alpha1.RestoreFailed
}

// step is one step of a restore. next says the step finished and the restore goes on at the
// phase it set; otherwise Reconcile returns res and err.
type step func(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, sup SupervisorAPI) (next bool, res ctrl.Result, err error)

// restore carries out a rollback (source RestorePoint) or reset (source ParentLatest) step by
// step (spec 9.4). The status is saved before each step, so an operator that restarts goes on
// from the step it was at (spec 10).
func (r *BranchReconciler) restore(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, sup SupervisorAPI) (ctrl.Result, error) {
	lr := b.Status.LastRestore
	if lr == nil || restoreFinished(lr.Phase) {
		req := b.Spec.Restore
		lr = &v1alpha1.RestoreStatus{RequestID: req.RequestID, Phase: v1alpha1.RestorePreparing, Source: req.Source, StartedAt: &metav1.Time{Time: r.now()}}
		b.Status.LastRestore = lr
		if lr.Source.Type == v1alpha1.SourceParentLatest && b.Spec.Parent == "" {
			r.restoreFailed(b, "a branch without a parent cannot be reset")
		}
	}
	ctx = logf.IntoContext(ctx, logf.FromContext(ctx).WithValues("op", "restore", "requestId", lr.RequestID))
	steps := map[v1alpha1.RestorePhase]step{
		v1alpha1.RestorePreparing:  r.restorePrepare,
		v1alpha1.RestoreStopping:   r.restoreStop,
		v1alpha1.RestoreActivating: r.restoreActivate,
		v1alpha1.RestoreStarting:   r.restoreStart,
		v1alpha1.RestoreDetaching:  r.restoreDetach,
	}
	for {
		if !restoreFinished(lr.Phase) {
			b.Status.Phase = v1alpha1.BranchRestoring
		}
		if err := r.saveStatus(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
		if restoreFinished(lr.Phase) {
			logf.FromContext(ctx).Info("restore finished", "phase", lr.Phase, "message", lr.Message)
			return ctrl.Result{}, nil
		}
		next, res, err := steps[lr.Phase](ctx, b, agent, sup)
		if !next {
			if err != nil && lr.Message == "" {
				lr.Message = err.Error() // kept until a step clears it, so a stuck restore says why
			}
			return res, err
		}
	}
}

// restoreFailed ends the restore as failed.
func (r *BranchReconciler) restoreFailed(b *v1alpha1.Branch, msg string) {
	lr := b.Status.LastRestore
	lr.Phase, lr.Message, lr.FinishedAt = v1alpha1.RestoreFailed, msg, &metav1.Time{Time: r.now()}
}

// restorePrepare makes the new generation (Preparing). Generations above the current one are what
// earlier tries made and never activated: they are deleted first, and again once the new one
// exists, in case a lost answer made one more (carry-over from plan 2). Generations below the
// current one may hold a user's data from before an earlier restore and are left alone.
func (r *BranchReconciler) restorePrepare(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, _ SupervisorAPI) (bool, ctrl.Result, error) {
	lr := b.Status.LastRestore
	gens, err := r.clearUnactivated(ctx, b, agent)
	if err != nil {
		res, err := retryLater(err)
		return false, res, err
	}
	if lr.FromGeneration == 0 {
		lr.FromGeneration = int32(gens.Current)
	}
	if lr.ToGeneration == 0 {
		req := agentapi.CreateGenerationRequest{
			Source: agentapi.Source{Type: agentapi.SourceType(lr.Source.Type), RestorePointID: lr.Source.RestorePointID},
		}
		if lr.Source.Type == v1alpha1.SourceParentLatest {
			req.Parent = b.Spec.Parent
		}
		g, err := call(ctx, copyTimeout, func(ctx context.Context) (agentapi.Generation, error) {
			return agent.CreateGeneration(ctx, b.Name, req)
		})
		if err != nil {
			if permanentReason(err) != "" || agentCode(err) == agentapi.CodeNotFound {
				r.restoreFailed(b, err.Error())
				return true, ctrl.Result{}, nil
			}
			if agentCode(err) == agentapi.CodeNotRunning {
				lr.Message = "waiting for the parent to run: " + err.Error()
			}
			res, err := r.retryLater(b, err)
			return false, res, err
		}
		lr.ToGeneration, lr.Message = int32(g.Generation), ""
		if err := r.saveStatus(ctx, b); err != nil { // remember it before anything else
			return false, ctrl.Result{}, err
		}
		if _, err := r.clearUnactivated(ctx, b, agent); err != nil {
			res, err := retryLater(err)
			return false, res, err
		}
	}
	b.Status.LastRestore.Phase = v1alpha1.RestoreStopping
	return true, ctrl.Result{}, nil
}

// clearUnactivated deletes the generations above the current one, except the one this restore
// made, and returns the generations.
func (r *BranchReconciler) clearUnactivated(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI) (agentapi.Generations, error) {
	lr := b.Status.LastRestore
	gens, err := call(ctx, callTimeout, func(ctx context.Context) (agentapi.Generations, error) {
		return agent.ListGenerations(ctx, b.Name)
	})
	if err != nil {
		return gens, err
	}
	if lr.ToGeneration != 0 && !slices.Contains(gens.Generations, int(lr.ToGeneration)) {
		lr.ToGeneration = 0 // gone; make another
	}
	for _, g := range gens.Generations {
		if g <= gens.Current || int32(g) == lr.ToGeneration {
			continue
		}
		if err := r.deleteGeneration(ctx, agent, b.Name, g); err != nil {
			return gens, err
		}
	}
	return gens, nil
}

func (r *BranchReconciler) deleteGeneration(ctx context.Context, agent AgentAPI, branch string, n int) error {
	_, err := call(ctx, callTimeout, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, agent.DeleteGeneration(ctx, branch, n)
	})
	if agentCode(err) == agentapi.CodeNotFound {
		return nil
	}
	return err
}

// restoreStop stops PostgreSQL (Stopping). The switch waits until the supervisor reports it
// stopped (carry-over from plan 1).
func (r *BranchReconciler) restoreStop(ctx context.Context, b *v1alpha1.Branch, _ AgentAPI, sup SupervisorAPI) (bool, ctrl.Result, error) {
	st, err := call(ctx, stopTimeout, sup.Stop)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if st.PG != supervisorapi.PGStopped {
		return false, ctrl.Result{RequeueAfter: waitPoll}, nil
	}
	b.Status.LastRestore.Phase = v1alpha1.RestoreActivating
	return true, ctrl.Result{}, nil
}

// restoreActivate points current at the new generation (Activating).
func (r *BranchReconciler) restoreActivate(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, _ SupervisorAPI) (bool, ctrl.Result, error) {
	lr := b.Status.LastRestore
	if err := r.activate(ctx, agent, b.Name, lr.ToGeneration); err != nil {
		if agentCode(err) == agentapi.CodeConflict { // PostgreSQL is not stopped after all
			lr.Phase = v1alpha1.RestoreStopping
			return true, ctrl.Result{}, nil
		}
		res, err := retryLater(err)
		return false, res, err
	}
	b.Status.DataGeneration = lr.ToGeneration
	lr.Phase = v1alpha1.RestoreStarting
	return true, ctrl.Result{}, nil
}

func (r *BranchReconciler) activate(ctx context.Context, agent AgentAPI, branch string, n int32) error {
	_, err := call(ctx, callTimeout, func(ctx context.Context) (agentapi.Generations, error) {
		return agent.ActivateGeneration(ctx, branch, int(n))
	})
	return err
}

// restoreStart starts PostgreSQL on the new generation (Starting). A 503 means it still starts,
// not that it failed (carry-over from plan 1); a 500 means it exited on the new data, and the
// branch goes back to the old generation (spec 9.4 step 4).
func (r *BranchReconciler) restoreStart(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, sup SupervisorAPI) (bool, ctrl.Result, error) {
	lr := b.Status.LastRestore
	if lr.Reverting {
		return r.restoreRevert(ctx, b, agent, sup)
	}
	st, err := call(ctx, startTimeout, sup.Start)
	switch {
	case err == nil && st.PG == supervisorapi.PGRunning:
		lr.Phase = v1alpha1.RestoreDetaching
		return true, ctrl.Result{}, nil
	case err == nil, stillStarting(err), supervisorStatus(err) == 409:
		return false, ctrl.Result{RequeueAfter: waitPoll}, nil
	case supervisorStatus(err) != 0:
		lr.Reverting = true
		lr.Message = "the restored data did not start: " + err.Error()
		return true, ctrl.Result{}, nil
	}
	return false, ctrl.Result{}, err
}

// restoreRevert puts the old generation back, starts it and deletes the new one; the restore then
// ends as failed.
func (r *BranchReconciler) restoreRevert(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, sup SupervisorAPI) (bool, ctrl.Result, error) {
	lr := b.Status.LastRestore
	why := lr.Message
	if err := r.activate(ctx, agent, b.Name, lr.FromGeneration); err != nil {
		if agentCode(err) == agentapi.CodeConflict { // not stopped: stop it, then switch
			if _, err := call(ctx, stopTimeout, sup.Stop); err != nil {
				return false, ctrl.Result{}, err
			}
			return false, ctrl.Result{RequeueAfter: waitPoll}, nil
		}
		res, err := retryLater(err)
		return false, res, err
	}
	b.Status.DataGeneration = lr.FromGeneration
	st, err := call(ctx, startTimeout, sup.Start)
	switch {
	case err == nil && st.PG == supervisorapi.PGRunning:
	case err == nil, stillStarting(err), supervisorStatus(err) == 409:
		return false, ctrl.Result{RequeueAfter: waitPoll}, nil
	case supervisorStatus(err) != 0:
		why += "; the previous data did not start either: " + err.Error()
	default:
		return false, ctrl.Result{}, err
	}
	if err := r.deleteGeneration(ctx, agent, b.Name, int(lr.ToGeneration)); err != nil {
		res, err := retryLater(err)
		return false, res, err
	}
	r.restoreFailed(b, why)
	return true, ctrl.Result{}, nil
}

// restoreDetach keeps the replaced generation (Detaching): a rollback's as a backup branch, which
// is made only after the move (carry-over from plan 2), a reset's as a restore point.
func (r *BranchReconciler) restoreDetach(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, _ SupervisorAPI) (bool, ctrl.Result, error) {
	lr := b.Status.LastRestore
	req := agentapi.DetachRequest{AsRestorePoint: true}
	if lr.Source.Type == v1alpha1.SourceRestorePoint {
		if lr.BackupBranch == "" {
			name, err := r.freeBackupName(ctx, b, lr.StartedAt.Time)
			if err != nil {
				return false, ctrl.Result{}, err
			}
			lr.BackupBranch = name
			if err := r.saveStatus(ctx, b); err != nil { // a retry must use the same name
				return false, ctrl.Result{}, err
			}
		}
		req = agentapi.DetachRequest{ToBranch: lr.BackupBranch}
	}
	resp, err := call(ctx, callTimeout, func(ctx context.Context) (agentapi.DetachResponse, error) {
		return agent.DetachGeneration(ctx, b.Name, int(lr.FromGeneration), req)
	})
	if req.ToBranch != "" && agentCode(err) == agentapi.CodeConflict {
		// The name is taken (by data whose Branch is not made yet): take a later second's.
		at, _ := time.Parse("20060102150405", lr.BackupBranch[len(lr.BackupBranch)-14:])
		if at.Sub(lr.StartedAt.Time) > time.Minute { // a conflict that a new name does not cure
			r.restoreFailed(b, fmt.Sprintf("the previous data stays on generation %d: %v", lr.FromGeneration, err))
			return true, ctrl.Result{}, nil
		}
		name, ferr := r.freeBackupName(ctx, b, at.Add(time.Second))
		if ferr != nil {
			return false, ctrl.Result{}, ferr
		}
		lr.BackupBranch = name
		return true, ctrl.Result{}, nil // saved, then tried again
	}
	switch {
	case agentCode(err) == agentapi.CodeNotFound: // nothing to keep
		lr.BackupBranch = ""
		lr.Message = "the previous data was gone; nothing was kept"
	case err != nil:
		res, err := retryLater(err)
		return false, res, err
	case req.ToBranch != "":
		if err := r.ensureBackupBranch(ctx, b, lr.BackupBranch); err != nil {
			return false, ctrl.Result{}, err
		}
		lr.Message = ""
	case resp.RestorePoint != nil:
		lr.RestorePointID, lr.Message = resp.RestorePoint.ID, ""
	}
	lr.Phase, lr.FinishedAt = v1alpha1.RestoreDone, &metav1.Time{Time: r.now()}
	return true, ctrl.Result{}, nil
}

// freeBackupName returns the first backup branch name from t on that no Branch has: two rollbacks
// of a branch within one second would otherwise want the same name (spec 9.4 names it to the
// second).
func (r *BranchReconciler) freeBackupName(ctx context.Context, b *v1alpha1.Branch, t time.Time) (string, error) {
	for i := range 60 {
		name := backupBranchName(b.Name, t.Add(time.Duration(i)*time.Second))
		var existing v1alpha1.Branch
		err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, &existing)
		if apierrors.IsNotFound(err) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("every backup branch name of a minute is taken")
}

// ensureBackupBranch makes the stopped branch that holds a rollback's replaced data. Its data is
// there already, so its own reconcile finds it instead of copying.
func (r *BranchReconciler) ensureBackupBranch(ctx context.Context, b *v1alpha1.Branch, name string) error {
	backup := &v1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.Namespace, Labels: map[string]string{LabelBackupOf: b.Name}},
		Spec: v1alpha1.BranchSpec{
			Parent:  b.Name,
			Source:  v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest},
			State:   v1alpha1.StateStopped,
			Compute: b.Spec.Compute,
		},
	}
	err := r.Create(ctx, backup)
	if apierrors.IsAlreadyExists(err) {
		var got v1alpha1.Branch
		if gerr := r.Get(ctx, client.ObjectKeyFromObject(backup), &got); gerr != nil {
			return gerr
		}
		if got.Labels[LabelBackupOf] != b.Name {
			return errors.New("branch " + name + " exists and is not this rollback's backup")
		}
		return nil
	}
	return err
}
```

`internal/controller/branch_controller.go`에서 다음 줄을 찾아:

```go
// token is the project's internal token
```

그 바로 앞에 다음을 넣는다(넣은 내용과 찾은 줄 사이에 빈 줄을 하나 둔다):

```go
// saveStatus writes b's status now. It sends a copy, so b's status, and the pointers into it a
// restore holds, stay the ones in memory; only the resource version is taken back.
func (r *BranchReconciler) saveStatus(ctx context.Context, b *v1alpha1.Branch) error {
	c := b.DeepCopy()
	if err := r.Status().Update(ctx, c); err != nil {
		return err
	}
	b.ResourceVersion = c.ResourceVersion
	return nil
}
```

`internal/controller/branch_controller.go`에서 다음 줄을 찾아:

```go
	// Step 3: the state.
	return r.ensureState(ctx, b, sup)
```

그 바로 앞에 다음을 넣는다(넣은 내용과 찾은 줄 사이에 빈 줄을 하나 둔다):

```go
	// Step 4 (spec 9.4): a rollback or reset takes over while one is open; the state follows.
	if restorePending(b) {
		res, err := r.restore(ctx, b, agent, sup)
		if err != nil || restorePending(b) {
			return res, err
		}
	}
```

- [ ] **Step 3: 통과 확인**

Run: `gofmt -l . && go vet ./internal/controller/ && go test -count=1 ./internal/controller/`
Expected: `ok`

Run(KUBEBUILDER_ASSETS를 설정한 채): `for i in 1 2 3; do go test -count=1 -race ./internal/controller/ | tail -1; done`
Expected: 세 번 모두 `ok`. 새 테스트는 일곱 개다: `TestRollback`, `TestReset`, `TestResetOfMainFails`, `TestARestoreThatDoesNotStartGoesBack`, `TestARestoreClearsGenerationsEarlierTriesLeft`, `TestARestoreGoesOnAfterAFailedStep`, `TestEachRequestRunsOnce`.

- [ ] **Step 4: 커밋**

```bash
git add internal/controller/
git commit -m "feat(controller): 롤백·리셋 — 단계마다 저장해 이어서 진행, 기동 실패 시 되돌리기, 백업 브랜치" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 10: 오퍼레이터 실행 파일과 RBAC

**Files:**
- Create: `cmd/operator/main.go`
- Generate: `config/rbac/role.yaml`
- Modify: `Makefile`, `go.mod`, `go.sum`
- Test: `cmd/operator/main_test.go`

**Interfaces:**
- Consumes: `controller.ProjectReconciler`, `controller.BranchReconciler`, `controller.CacheOptions`, `controller.NewHTTPConnector`, `kubetest.Main`·`AddUser`·`RepoRoot`, `pgtest.FreePort`
- Produces:
  - 실행 파일 `consonance-operator`와 그 플래그:
    - `--agent-image`(`consonance/agent:dev`), `--postgres-image`(`consonance/postgres:17-dev`)
    - `--metrics-bind-address`(`:8080`, `0`이면 끈다), `--health-probe-bind-address`(`:8081`)
    - `--kubeconfig`(비우면 `$KUBECONFIG`, `~/.kube/config`, 클러스터 안), `--leader-elect`(false)
  - 동작: 로그는 slog JSON이다. SIGTERM·SIGINT를 받으면 진행 중인 조정을 마치고 종료 코드 0으로 끝난다.
  - ClusterRole `consonance-operator`(`config/rbac/role.yaml`).

조정기의 RBAC 표지는 이미 Task 6·7(`manager.go`, `project_controller.go`, `branch_controller.go`)에 따로 떨어진 주석 묶음으로 있다. controller-gen은 타입 문서 주석 안에 섞인 표지를 읽지 않는다. 이 작업에서 `make generate`가 그 표지로 ClusterRole을 만든다. 실행 파일 테스트는 이 ClusterRole만 묶인 사용자로 오퍼레이터를 띄운다. 먼저 그 사용자가 역할 밖의 일(네임스페이스 목록)을 하면 Forbidden인지 확인해, envtest가 RBAC를 실제로 적용한다는 것을 증명한다.

- [ ] **Step 1: 실패하는 테스트**

`cmd/operator/main_test.go`:

```go
package main_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"consonance/api/v1alpha1"
	"consonance/internal/controller"
	"consonance/internal/testutil/kubetest"
	"consonance/internal/testutil/pgtest"
)

var env *kubetest.Env

func TestMain(m *testing.M) { os.Exit(kubetest.Main(m, &env)) }

func buildOperator(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "consonance-operator")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

// operatorUser is an API server user bound to the ClusterRole that make generate writes, and its
// kubeconfig: the operator runs with exactly the permissions it will have in a cluster.
func operatorUser(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	b, err := os.ReadFile(filepath.Join(kubetest.RepoRoot(), "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(b, &role); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Create(ctx, &role); err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "consonance-operator"}},
	}
	if err := env.Client.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	kubeconfig, err := env.AddUser("consonance-operator")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOperatorReconcilesWithItsOwnPermissions(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	bin := buildOperator(t)
	kubeconfig := operatorUser(t)
	// The API server enforces RBAC: the user may not do what the role does not grant.
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	as, err := client.New(cfg, client.Options{Scheme: env.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := as.List(ctx, &corev1.NamespaceList{}); !apierrors.IsForbidden(err) {
		t.Fatalf("listing namespaces as the operator: %v, want Forbidden", err)
	}
	probe := fmt.Sprintf("127.0.0.1:%d", pgtest.FreePort(t))
	logPath := filepath.Join(t.TempDir(), "operator.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(bin, "--kubeconfig", kubeconfig, "--metrics-bind-address", "0", "--health-probe-bind-address", probe,
		"--agent-image", "consonance/agent:test")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	logs := func() string { b, _ := os.ReadFile(logPath); return string(b) }
	defer func() {
		_ = cmd.Process.Kill()
		if t.Failed() {
			t.Logf("operator output:\n%s", logs())
		}
	}()

	wait := func(what string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !check() {
			select {
			case err := <-exited:
				t.Fatalf("the operator exited: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %s after 20s", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	wait("healthz", func() bool {
		resp, err := http.Get("http://" + probe + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	ns := env.Namespace(t)
	p := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: controller.ProjectName, Namespace: ns}, Spec: v1alpha1.ProjectSpec{DisplayName: "smoke"}}
	if err := env.Client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	// The agent's service name does not resolve from the test host, so the project stops at
	// AgentReady=False; everything before it is made, and the status is written.
	wait("project status", func() bool {
		var got v1alpha1.Project
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
			return false
		}
		c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionAgentReady)
		return c != nil && c.Reason == "AgentUnavailable" && got.Status.Phase == v1alpha1.ProjectProvisioning
	})
	for name, obj := range map[string]client.Object{
		controller.DBCredentialsSecret: &corev1.Secret{}, controller.InternalTokenSecret: &corev1.Secret{},
		controller.PVCName: &corev1.PersistentVolumeClaim{}, controller.AgentName: &corev1.Pod{},
	} {
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	var pod corev1.Pod
	if err := env.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: controller.AgentName}, &pod); err == nil && pod.Spec.Containers[0].Image != "consonance/agent:test" {
		t.Errorf("agent image %s", pod.Spec.Containers[0].Image)
	}
	if !strings.Contains(logs(), `"msg":"operator starting"`) {
		t.Errorf("no JSON start log:\n%s", logs())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the operator did not exit within 15s of SIGTERM")
	}
}

func TestOperatorRefusesStrayArguments(t *testing.T) {
	bin := buildOperator(t)
	out, err := exec.Command(bin, "extra").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "unexpected arguments") {
		t.Fatalf("stray argument: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, "--help").CombinedOutput(); err != nil {
		t.Fatalf("--help: %v\n%s", err, out)
	}
}
```

Run: `go test ./cmd/operator/`
Expected: FAIL — `no non-test Go files in .../cmd/operator`

- [ ] **Step 2: 구현과 생성**

`cmd/operator/main.go`:

```go
// Command consonance-operator reconciles Projects and Branches (spec 7.1). It runs in
// consonance-system and reaches the agents and supervisors through their services.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"consonance/api/v1alpha1"
	"consonance/internal/controller"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("operator exited", "err", err)
		os.Exit(1)
	}
}

type config struct {
	agentImage    string
	postgresImage string
	metricsAddr   string
	probeAddr     string
	kubeconfig    string
	leaderElect   bool
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("consonance-operator", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.agentImage, "agent-image", "consonance/agent:dev", "volume agent image")
	fs.StringVar(&c.postgresImage, "postgres-image", "consonance/postgres:17-dev", "branch image (PostgreSQL and the supervisor)")
	fs.StringVar(&c.metricsAddr, "metrics-bind-address", ":8080", "metrics listen address; 0 turns metrics off")
	fs.StringVar(&c.probeAddr, "health-probe-bind-address", ":8081", "liveness and readiness listen address")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file; empty means $KUBECONFIG, ~/.kube/config or in-cluster")
	fs.BoolVar(&c.leaderElect, "leader-elect", false, "elect one leader among replicas")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	return c, nil
}

func run(args []string) error {
	c, err := parseFlags(args)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	cfg, err := restConfig(c.kubeconfig)
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: c.metricsAddr},
		HealthProbeBindAddress: c.probeAddr,
		LeaderElection:         c.leaderElect,
		LeaderElectionID:       "consonance-operator",
		Cache:                  controller.CacheOptions(),
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	conn := controller.NewHTTPConnector()
	if err := (&controller.ProjectReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Connector: conn, AgentImage: c.agentImage,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&controller.BranchReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Connector: conn, PostgresImage: c.postgresImage,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	slog.Info("operator starting", "agentImage", c.agentImage, "postgresImage", c.postgresImage)
	// SIGTERM or SIGINT stops the manager, which lets running reconciles finish.
	return mgr.Start(ctrl.SetupSignalHandler())
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return ctrl.GetConfig()
}
```

`Makefile`에서 다음을:

```make
generate: ## deepcopy code and CRD manifests from api/ (controller-gen)
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
```

다음으로 바꾼다:

```make
generate: ## deepcopy code, CRDs and the operator's ClusterRole (controller-gen)
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=consonance-operator paths=./internal/controller/... output:rbac:artifacts:config=config/rbac
```

Run: `go mod tidy && make generate && cat config/rbac/role.yaml`
Expected: ClusterRole `consonance-operator`가 생긴다. 규칙에 다음이 들어 있다:
- `branches`(쓰기 포함), `projects`(읽기), `projects/status`·`branches/status`, `projects/finalizers`·`branches/finalizers`
- `pods`·`services`·`secrets`·`persistentvolumeclaims`·`resourcequotas`, `networkpolicies`
- `leases`, `events`

- [ ] **Step 3: 통과 확인**

Run(KUBEBUILDER_ASSETS를 설정한 채): `go test -count=1 -v ./cmd/operator/ 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected: `TestOperatorReconcilesWithItsOwnPermissions`, `TestOperatorRefusesStrayArguments` PASS

Run: `gofmt -l . && make test && make test-envtest`
Expected: gofmt 출력 없음. 두 명령 모두 모든 패키지 `ok`.

- [ ] **Step 4: 커밋**

```bash
git add Makefile go.mod go.sum cmd/operator/ config/rbac/
git commit -m "feat(operator): 오퍼레이터 실행 파일과 controller-gen이 만드는 ClusterRole" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

### Task 11: 문서

**Files:**
- Modify: `docs/superpowers/specs/2026-10-07-branching-engine-design.md`, `docs/superpowers/plans/2026-10-07-branching-engine-index.md`, `docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`

**Interfaces:**
- Produces: 스펙 4.3·7.1·7.5·8·9.4·13에 이 계획의 결정이 들어가고, 이월 사항 문서는 계획 3이 반영한 항목을 지우고 계획 4·5가 반영할 것을 더한다.

- [ ] **Step 1: 스펙과 목차**

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
1. Secret `db-credentials`(없을 때만 생성), Secret `internal-token`(에이전트·감독 프로세스 간 인증), NetworkPolicy, ResourceQuota를 맞춘다.
```

다음으로 바꾼다:

```markdown
1. Secret `db-credentials`(키 `username`·`password`·`database`), Secret `internal-token`(키 `token`, 에이전트·감독 프로세스 간 인증)을 없을 때만 만들고, NetworkPolicy, ResourceQuota(파드 `2 × maxBranches + 1`, PVC 1개, 저장 용량 `storage.size`)를 맞춘다. Secret 두 개와 PVC에는 소유자를 두지 않는다. Project 객체만 지워져도 데이터와 비밀번호는 남고, 네임스페이스를 지울 때 함께 지워진다.
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
3. 에이전트 파드 `volume-agent`를 띄운다(Project가 소유).
```

다음으로 바꾼다:

```markdown
3. 에이전트 파드와 Service `volume-agent`(8443)를 띄운다(Project가 소유). 에이전트 인자(`--project-id`, `--min-copy-interval`, `--restore-points-per-branch`, `--restore-point-max-age`)는 Project spec에서 오고, 바뀌면 파드를 다시 만든다. PVC 바인딩은 기다리지 않는다(WaitForFirstConsumer 스토리지 클래스는 이 파드가 스케줄돼야 바인딩한다).
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
5. main Branch가 없으면 만들고, 에이전트 `POST /v1/init-main`으로 `branches/main/data-1`을 초기화한다. main의 데이터는 여기서만 만든다.
```

다음으로 바꾼다:

```markdown
5. main Branch가 없으면 만든다(Project가 소유, `protected: true`). main의 데이터는 main의 Branch 조정이 에이전트 `POST /v1/init-main`으로만 만든다.
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
7. 모든 조건이 참이면 phase Ready.
```

다음으로 바꾼다:

```markdown
7. 모든 조건이 참이면 phase Ready. 조건 `Ready`가 네 조건을 요약하고, 실패면 이유(`ReflinkUnsupported`, 네임스페이스 이름이 `cns-`로 시작하지 않으면 `InvalidNamespace`)를 단다. Ready인 프로젝트는 1분마다 에이전트 `GET /v1/usage`로 `status.volume`을 갱신한다.
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
1. main이 아닌 브랜치의 데이터 디렉터리가 없으면 에이전트 `POST /v1/branches/{name}/data`를 호출한다. 성공하면 status `DataReady`, `dataReadyAt` 기록.
```

다음으로 바꾼다:

```markdown
1. 데이터가 없으면 main은 에이전트 `POST /v1/init-main`, 그 밖의 브랜치는 `POST /v1/branches/{name}/data`를 호출한다. 성공하면 status `DataReady`, `dataReadyAt` 기록. 영구 오류(10장)면 phase Failed와 조건 `DataReady=False`(이유 `ReflinkUnsupported`·`InsufficientStorage`·`SourceNotFound`·`Invalid`)로 두고 다시 시도하지 않는다.
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
5. 삭제 시 파이널라이저에서 파드·Service를 지우고 에이전트 `DELETE /v1/branches/{name}/data`를 호출한 뒤 파이널라이저를 뗀다.
```

다음으로 바꾼다:

```markdown
5. 삭제 시 파이널라이저(`consonance.io/branch-cleanup`)에서 파드·Service를 지우고, 파드 객체가 사라진 뒤 에이전트 `DELETE /v1/branches/{name}/data`를 호출하고 파이널라이저를 뗀다. main의 데이터는 지우지 않고(Project 조정이 main Branch를 다시 만든다), Project가 없거나 삭제 중이면 에이전트를 부르지 않는다(네임스페이스 삭제가 볼륨을 가져간다).
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음 줄을 찾아:

```markdown
### 4.4 복구 지점 카탈로그
```

그 바로 앞에 다음을 넣는다(넣은 내용과 찾은 줄 사이에 빈 줄을 하나 둔다):

```markdown
- 조건: `DataReady`(데이터를 만들었는지), `Ready`(원하는 상태에 있는지, 아니면 그 이유).
- `lastRestore`에는 오퍼레이터가 재시작해도 이어서 진행하는 데 쓰는 `source`, `fromGeneration`, `toGeneration`, `restorePointId`(리셋으로 밀려난 세대를 담은 복구 지점), `reverting`, `message`, `startedAt`, `finishedAt`도 둔다. 요청 하나가 끝나기(`Done`·`Failed`) 전에 새 `requestId`가 오면 끝난 뒤에 수행한다.
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
- 같은 PVC(RWO)를 쓰므로 브랜치 파드는 에이전트 파드와 같은 노드에 둔다(필수 podAffinity, `kubernetes.io/hostname`).
```

다음으로 바꾼다:

```markdown
- 같은 PVC(RWO)를 쓰므로 브랜치 파드는 에이전트 파드와 같은 노드에 둔다(필수 podAffinity, `kubernetes.io/hostname`).
- 브랜치 Service `br-<이름>`은 5432와 8008을 노출하고 `publishNotReadyAddresses: true`다. PostgreSQL이 정지해 파드가 준비되지 않아도 오퍼레이터와 에이전트가 감독 프로세스에 닿아야 한다.
- 파드는 서비스 계정 토큰을 마운트하지 않고, `terminationGracePeriodSeconds`는 45초다(감독 프로세스의 30초 fast shutdown, 에이전트의 25초 요청 대기).
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
- 에이전트 파드 8443: `consonance-system`의 오퍼레이터와 API 서버에서만 허용.
```

다음으로 바꾼다:

```markdown
- 에이전트 파드 8443: `consonance-system`의 오퍼레이터와 API 서버에서만 허용.
- `consonance-system`의 오퍼레이터와 API 서버는 파드 라벨 `app.kubernetes.io/name`(`consonance-operator`, `consonance-apiserver`)으로 가린다.
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
에이전트 아웃바운드: 같은 네임스페이스와 DNS만 허용.
```

다음으로 바꾼다:

```markdown
에이전트 아웃바운드: 같은 네임스페이스와 DNS(`kube-system`의 53번 포트)만 허용.
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
1. **Preparing:** 에이전트 `POST /generations`로 새 세대를 만든다. 롤백이면 복구 지점 사본, 리셋이면 부모의 현재 상태(백업 모드 복사).
```

다음으로 바꾼다:

```markdown
1. **Preparing:** 에이전트 `POST /generations`로 새 세대를 만든다. 롤백이면 복구 지점 사본, 리셋이면 부모의 현재 상태(백업 모드 복사). 만들기 전과 만든 뒤에 현재 세대보다 번호가 큰 세대(활성화하지 못한 앞 시도의 결과)를 지운다.
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
4. **Starting:** 감독 프로세스 `/v1/start`. 실패하면 `current`를 옛 세대로 되돌리고 다시 기동한 뒤 phase Failed.
```

다음으로 바꾼다:

```markdown
4. **Starting:** 감독 프로세스 `/v1/start`. 실패(500)하면 `current`를 옛 세대로 되돌리고 다시 기동하고 새 세대를 지운 뒤 phase Failed. 503은 아직 기동 중이라는 뜻이라 실패로 보지 않는다.
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
(최대 40자, 예: `main-before-20261007031500`)다.
```

다음으로 바꾼다:

```markdown
(최대 40자, 예: `main-before-20261007031500`)다. 같은 이름의 브랜치가 이미 있으면 다음 초의 이름을 쓴다.
```

`docs/superpowers/specs/2026-10-07-branching-engine-design.md`에서 다음을:

```markdown
├── config/                  CRD·RBAC·개발용 매니페스트 (kustomize)
```

다음으로 바꾼다:

```markdown
├── config/                  CRD(crd/bases)·RBAC(rbac, controller-gen이 만든다)·개발용 매니페스트 (kustomize)
```

`docs/superpowers/plans/2026-10-07-branching-engine-index.md`에서 다음을:

```markdown
| 3 | [리소스 타입과 오퍼레이터](2026-10-12-branching-engine-3-operator.md) | Project·Branch CRD, Project·Branch 조정(롤백·리셋 포함), 파드·네트워크 정책 명세, envtest | 작성 완료 |
```

다음으로 바꾼다:

```markdown
| 3 | [리소스 타입과 오퍼레이터](2026-10-12-branching-engine-3-operator.md) | Project·Branch CRD, Project·Branch 조정(롤백·리셋 포함), 파드·네트워크 정책 명세, envtest | 구현 완료 |
```

`docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`에서 다음을 지운다(뒤따르는 빈 줄 하나도 지운다):

```markdown
### 계획 3(CRD·오퍼레이터)이 반영할 것

- 브랜치 파드의 `terminationGracePeriodSeconds`는 45초 이상으로 둔다(stop-grace 30초 + SIGQUIT 뒤 대기 10초 + 여유). 쿠버네티스 기본값 30초는 짧다.
- 브랜치 파드에는 항상 `limits.memory`를 준다. 한도가 없으면 Downward API가 노드 메모리를 넘겨 `shared_buffers`가 너무 크게 잡힌다.
- `AUTOSTART`, `MEMORY_LIMIT_BYTES`에는 유효한 값만 준다. 잘못된 값은 조용히 기본값으로 바뀐다.
- `/v1/start`의 503은 아직 기동 중이라는 뜻이다. 실패가 아니므로 9.4의 되돌리기를 하지 말고 `/v1/status`를 다시 확인한다. 500도 기동이 끝났다는 뜻은 아니므로 상태를 다시 확인한다.
- `/v1/start`의 409는 두 가지다. 다른 작업이 진행 중이거나(`ErrBusy`), 실행 중인 세대와 `current`가 다르다(`ErrGenerationMismatch`). 지금은 메시지로만 구분하므로, 오퍼레이터가 둘을 구분해야 하면 기계가 읽을 수 있는 오류 코드를 추가한다.
- 9.4 롤백에서 `current` 전환은 `/v1/status`가 `stopped`를 보인 뒤에 한다. 실행 중에 링크를 바꾸고 `start`를 부르면 409가 온다.
```

- [ ] **Step 2: 이월 사항**

`docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`에서 다음 두 절을 지운다. 이 계획이 모두 반영했다.
- **계획 1의 절:** "계획 1(감독 프로세스)에서" 아래의 `### 계획 3(CRD·오퍼레이터)이 반영할 것`. 위 Step 1의 마지막 수정이 이 절을 지운다.
- **계획 2의 절:** "계획 2(볼륨 에이전트)에서" 아래의 `### 계획 3(CRD·오퍼레이터)이 반영할 것`. 그 제목부터 같은 블록의 `### 계획 4(REST API 서버)가 반영할 것` 바로 앞까지 지운다.

계획 2의 절에서 아직 남은 것이 있다: 메트릭, `requestId` 전달, initdb 시간 측정. 이것들은 아래 새 절의 계획 5 항목으로 옮긴다.

이어서 파일 끝에 다음을 더한다. 실행을 마칠 때 컨트롤러가 리뷰에서 미뤄 둔 지적을 이 절 아래 `### 미뤄 둔 개선 사항`으로 더한다.

```markdown
## 계획 3(CRD·오퍼레이터)에서

### 계획 4(REST API 서버)가 반영할 것

- 프로젝트 생성은 네임스페이스 `cns-<projectId>`와 그 안의 Project `project`를 만드는 것이다. spec에서 비운 값은 CRD 기본값(스펙 14장)이 채운다. 진행은 `status.phase`와 조건(`VolumeReady`, `AgentReady`, `ReflinkSupported`, `MainReady`, 이를 요약한 `Ready`)으로 읽는다.
- 브랜치 생성 전 검사(부모 실행 중, `maxBranches`, 이름 형식)는 API가 한다. 오퍼레이터는 브랜치 수를 세지 않고, 부모가 실행 중이 아니면 데이터 만들기를 기다린다(`DataReady=False`, 이유 `ParentNotRunning`). 롤백이 만든 백업 브랜치(라벨 `consonance.io/backup-of`)도 Branch다. 개수에 넣을지 정한다.
- 브랜치의 영구 실패는 phase `Failed`와 조건 `DataReady=False`의 이유(`ReflinkUnsupported`·`InsufficientStorage`·`SourceNotFound`·`Invalid`)로 드러난다. 기동 실패는 phase `Failed`, 조건 `Ready=False`(이유 `StartFailed`)이고 오퍼레이터가 계속 다시 시도한다.
- 롤백·리셋은 `spec.restore`에 새 `requestId`와 출처를 쓰고, `status.lastRestore.requestId`가 그 값이 되고 `phase`가 `Done`이나 `Failed`가 될 때까지 기다린다. 응답의 `backupBranch`는 `lastRestore.backupBranch`, 리셋이 만든 복구 지점은 `lastRestore.restorePointId`다. 진행 중인 요청이 끝나기 전에 쓴 새 `requestId`는 끝난 뒤에 수행된다. 정지된 브랜치도 새 데이터가 기동하는지 확인하려고 잠시 기동했다가 다시 정지한다.
- 접속 정보는 Secret `db-credentials`(`username`·`password`·`database`)와 Branch `status.endpoint`에서 온다.
- main Branch 객체를 지우면 오퍼레이터가 데이터는 둔 채 다시 만든다. main 삭제는 API가 409로 막는다.

### 계획 5(배포·E2E·성능)가 반영할 것

- 오퍼레이터 Deployment는 `consonance-system`에서 파드 라벨 `app.kubernetes.io/name: consonance-operator`로 띄운다(API 서버는 `consonance-apiserver`). 네트워크 정책이 이 라벨로 오퍼레이터와 API 서버를 가린다. 서비스 계정에는 `config/rbac/role.yaml`의 ClusterRole `consonance-operator`를 묶고, 복제본이 여럿이면 `--leader-elect`를 켠다. 이미지는 `--agent-image`·`--postgres-image`로 준다.
- `consonance/operator` 이미지는 아직 없다(스펙 7.6).
- k3s(kube-router)에서 네트워크 정책이 kubelet 프로브를 막지 않는지, 에이전트의 DNS 이그레스(`kube-system`의 53번 포트)가 맞는지 E2E로 확인한다.
- 스펙 11장의 메트릭은 에이전트에도 오퍼레이터에도 아직 없다. status의 `timings`(`dataReadyAt`, `runningAt`)와 `lastRestore`의 시각만 있다.
- `requestId`: 오퍼레이터는 롤백·리셋 로그에 `spec.restore.requestId`를 남기지만 에이전트와 감독 프로세스에는 넘기지 않는다.
- 에이전트 파드의 CPU 한도(100m)에서 initdb 시간을 잰다. 오퍼레이터의 init-main 호출 제한은 5분이다.
- 개발용 스토리지 클래스 `consonance-xfs`(XFS + reflink)를 만든다. envtest는 PVC를 바인딩하지 않으므로 볼륨 바인딩과 `VolumeReady`는 E2E에서 처음 확인된다.
```

- [ ] **Step 3: 확인과 커밋**

Run: `grep -c '계획 3(CRD·오퍼레이터)이 반영할 것' docs/superpowers/plans/2026-10-07-branching-engine-carryover.md; grep -n '| 3 |' docs/superpowers/plans/2026-10-07-branching-engine-index.md`
Expected: 첫 줄은 `0`이고, 계획 3 행의 상태는 `구현 완료`다.

```bash
git add docs/superpowers/specs/2026-10-07-branching-engine-design.md docs/superpowers/plans/2026-10-07-branching-engine-index.md docs/superpowers/plans/2026-10-07-branching-engine-carryover.md
git commit -m "docs: 스펙·목차·이월 사항에 계획 3(오퍼레이터)의 결정 반영" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
