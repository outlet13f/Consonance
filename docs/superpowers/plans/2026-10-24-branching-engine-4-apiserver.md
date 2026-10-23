# 브랜칭 엔진 계획 4: REST API 서버 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 관리자 API 키로 인증하는 REST API 서버(`cmd/apiserver`, `internal/httpapi`)를 만든다. 프로젝트·브랜치·복구 지점·롤백·리셋·정지/기동·접속 정보를 스펙 5장의 경로와 응답 코드 그대로 제공하고, OpenAPI 3.1 문서를 낸다.

**Architecture:** 서버는 상태가 없다. 쿠버네티스 리소스(네임스페이스, Project, Branch)는 controller-runtime 클라이언트로 쓰고, 읽기는 캐시로 한다. Secret은 캐시하지 않고 그때그때 읽는다. 복구 지점은 API 서버가 프로젝트의 볼륨 에이전트를 직접 부른다(스펙 9.3). 그 밖의 쓰기는 모두 Branch·Project spec을 바꾸는 것이고, 동기 응답이 필요하면 캐시의 status를 제한 시간까지 기다린 뒤 202와 현재 상태를 돌려준다(스펙 7.4).

**Tech Stack:** Go 1.27(툴체인 go1.27.2), `net/http` 라우팅(Go 1.22+ 패턴), controller-runtime v0.25.2, `log/slog` JSON, envtest 1.37.0, 기존 `internal/agentclient`·`internal/secretfile`.

**Spec:** `docs/superpowers/specs/2026-10-07-branching-engine-design.md` (5장, 7.4, 9장, 10~12장, 14장). 앞 계획에서 넘어온 사항: `docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`의 두 "계획 4(REST API 서버)가 반영할 것" 절(계획 2, 계획 3).

## Global Constraints

- 기본 경로 `/v1`, JSON, 인증 헤더 `Authorization: Bearer <관리자 키>`. 관리자 키는 `consonance-system`의 Secret `consonance-admin-key`(키 `key`)를 파일로 마운트해 `--admin-key-file`로 읽는다.
- 오류 본문은 RFC 9457 `application/problem+json`: `{type, title, status, detail}`. `type`은 스펙 5장 표의 값(`validation-error`, `unauthorized`, `not-found`, `conflict`, `rate-limited`, `insufficient-storage`, `internal`) 그대로다.
- 상태 코드와 동기 대기는 스펙 5장 표와 14장 그대로: 브랜치 생성 201(5초 넘으면 202), 정지·기동 200(10초 넘으면 202), 롤백·리셋 200(30초 넘으면 202), 프로젝트 생성·삭제와 브랜치 삭제 202, 복구 지점 생성 201, 복구 지점 삭제 204.
- 429에는 `Retry-After`(초, 정수)를 붙인다.
- 프로젝트 네임스페이스는 `cns-<projectId>`, `projectId`는 소문자 영숫자 12자 무작위 값, 그 안의 Project 이름은 `project`(스펙 4.1).
- 브랜치 이름: DNS-1123 label, 최대 40자. 백업 브랜치 이름 형식 `^.+-before-[0-9]{14}$`는 사용자가 쓸 수 없다(이월 사항).
- 복구 지점 라벨은 256바이트까지(이월 사항, 계획 2).
- 로그는 `log/slog` JSON, 공통 필드 `projectId`, `branch`, `requestId`, `op`(스펙 11장). `requestId`는 요청 헤더 `X-Request-Id`가 있으면 그 값(최대 64자, 영숫자·`-`·`_`), 없으면 새로 만들고 응답 헤더 `X-Request-Id`로 돌려준다.
- 모든 커밋의 트레일러는 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. `bin/`·`spikes/`는 커밋하지 않는다.
- 테스트: `internal/httpapi`는 controller-runtime 가짜 클라이언트 + 가짜 에이전트(스펙 12장). 실행 파일과 RBAC는 envtest(`KUBEBUILDER_ASSETS`는 절대 경로, `export KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2 use 1.37.0 --bin-dir "$PWD/bin/envtest" -p path)`).

## 결정 (스펙이 정하지 않아 이 계획이 정한 것)

1. **브랜치 개수:** 사용자가 만든 브랜치(라벨 `consonance.io/backup-of`가 없는 것, main 포함)가 `maxBranches`를 넘을 수 없다(429). 백업 브랜치는 따로 세어 `maxBranches`개까지 둔다. 롤백 요청 때 백업 브랜치가 이미 `maxBranches`개면 429로 막고 `detail`에 오래된 백업 브랜치를 지우라고 적는다. 그러면 파드 수가 쿼터 `2 × maxBranches + 1`(에이전트 1)을 넘지 않는다.
2. **스토리지 클래스:** API 서버 플래그 `--storage-class`(기본 `consonance-xfs`)를 프로젝트 생성 때 `spec.storage.storageClassName`에 넣는다. 요청 본문으로는 받지 않는다.
3. **동기 대기:** "watch로 기다린다"(7.4)는 캐시(informer)를 100ms마다 읽어 확인하는 것으로 한다. 캐시는 watch로 채워지므로 API 서버에 따로 부하를 주지 않고, 가짜 클라이언트로 시험할 수 있다.
4. **롤백·리셋 결과:** `lastRestore.phase=Done`이면 200, `Failed`면 500 `internal`(`detail`에 `lastRestore.message`)이다.
5. **복구 지점 삭제 재시도:** 에이전트가 404를 주면 그대로 404다. 삭제를 멱등으로 만들지 않는다(스펙 5장 표에 404가 있다).
6. **프로젝트 네임스페이스:** API 서버가 만들 때 라벨 `app.kubernetes.io/managed-by: consonance-apiserver`와 Pod Security 라벨 `pod-security.kubernetes.io/enforce: restricted`를 단다(스펙 15장 4, 최종 리뷰의 "네임스페이스 PSS 라벨은 계획 4").
7. **프로젝트 목록:** 클러스터의 모든 Project를 캐시에서 읽어 `projectId` 순으로 돌려준다. 페이지 나누기는 없다(사용자 계정이 없는 단계, 스펙 1장).
8. **조건 이유 상수:** 오퍼레이터가 쓰는 조건 이유 문자열을 `api/v1alpha1`로 옮겨 내보낸다(이월 사항). API는 그 상수로 실패를 오류 유형에 맞춘다.

## Review Focus

1. 같은 브랜치에 롤백·리셋을 거의 동시에 두 번 보냄 → 둘째는 409이고, 첫째만 수행된다. (Task 9의 `TestSecondRestoreWhileOneRunsIsConflict`)
2. 롤백 대상 복구 지점이 다른 브랜치의 것 → 404 `not-found`. 에이전트 목록에 없는 ID를 받아 오퍼레이터를 영구 실패로 보내지 않는다. (Task 9의 `TestRollbackToAnotherBranchesRestorePointIsNotFound`)
3. 준비가 안 된 프로젝트(Provisioning, reflink 미지원으로 Failed)에 브랜치·복구 지점 요청 → 409 `conflict`, `detail`에 프로젝트 상태. (Task 5의 `TestBranchInAProjectThatIsNotReadyIsConflict`)
4. 관리자 키 비교가 길이·내용으로 새지 않고, 키 파일이 비었으면 서버가 뜨지 않는다. (Task 2의 `TestAuth`, Task 11의 `TestRefusesAnEmptyAdminKey`)
5. 존재하지 않는 프로젝트 ID 형식(대문자, 12자 아님, `cns-` 접두사 포함)을 경로에 넣음 → 404이고 쿠버네티스에 이상한 이름으로 묻지 않는다. (Task 4의 `TestMalformedProjectIDIsNotFound`)

---

## 파일 구조

| 파일 | 책임 |
| --- | --- |
| `api/v1alpha1/reasons.go` | 조건 이유 상수 (Task 1) |
| `internal/httpapi/server.go` | `Server`, 라우팅, 미들웨어 묶기 (Task 2) |
| `internal/httpapi/auth.go` | 관리자 키 미들웨어 (Task 2) |
| `internal/httpapi/problem.go` | problem+json, 오류 유형, 에이전트·쿠버네티스 오류 변환 (Task 2·3) |
| `internal/httpapi/log.go` | requestId, 요청 로그 (Task 2) |
| `internal/httpapi/validate.go` | 이름·본문 검사 (Task 3) |
| `internal/httpapi/wait.go` | 캐시를 읽으며 기다리기 (Task 3) |
| `internal/httpapi/projects.go` | 프로젝트 핸들러와 응답 모양 (Task 4) |
| `internal/httpapi/branches.go` | 브랜치 생성·조회·삭제와 응답 모양 (Task 5) |
| `internal/httpapi/agent.go` | 에이전트 연결 인터페이스, 토큰 읽기 (Task 6) |
| `internal/httpapi/restorepoints.go` | 복구 지점 (Task 6) |
| `internal/httpapi/connection.go` | 접속 정보 (Task 7) |
| `internal/httpapi/state.go` | 정지·기동 (Task 8) |
| `internal/httpapi/restore.go` | 롤백·리셋 (Task 9) |
| `internal/httpapi/openapi.json`, `openapi.go` | OpenAPI 3.1 문서와 그 핸들러 (Task 10) |
| `internal/httpapi/fake_test.go`, `helpers_test.go` | 가짜 에이전트, 가짜 클라이언트 묶음, 상태 흉내 도우미 (Task 2부터 늘어난다) |
| `cmd/apiserver/main.go`, `main_test.go` | 실행 파일, RBAC 확인 (Task 11) |
| `config/rbac/apiserver/role.yaml` | controller-gen이 만드는 ClusterRole `consonance-apiserver` (Task 11) |

---

### Task 1: 조건 이유 상수를 `api/v1alpha1`로

**Files:**
- Create: `api/v1alpha1/reasons.go`
- Modify: `internal/controller/errors.go`, `project_controller.go`, `branch_controller.go`, `restore.go`(문자열 이유를 상수로)
- Test: `api/v1alpha1/reasons_test.go`

**Interfaces:**
- Produces (`api/v1alpha1`):
  - Branch `DataReady=False`: `ReasonReflinkUnsupported`, `ReasonInsufficientStorage`, `ReasonSourceNotFound`, `ReasonInvalid`, `ReasonParentNotRunning`, `ReasonCopyFailed`, `ReasonInitMainFailed`
  - Branch `Ready`: `ReasonSupervisorUnavailable`, `ReasonStartFailed`, `ReasonUnschedulable`, `ReasonPodCreateFailed` (참일 때는 phase 이름 `Running`·`Stopped`)
  - Project: `ReasonAgentUnavailable`, `ReasonAgentPodFailed`, `ReasonMainNotRunning`, `ReasonInvalidNamespace`, `ReasonBootstrapFailed`
  - `PermanentDataFailure(reason string) bool` — 지금 `internal/controller`의 `permanentDataFailure`를 옮긴 것.
- 값은 지금 코드의 문자열 그대로다. 컨트롤러의 기존 상수(`ReasonReflinkUnsupported` 등)는 지우고 `v1alpha1`의 것을 쓴다.

- [ ] **Step 1: 실패하는 테스트** — `TestPermanentDataFailure`: 이유 넷(`ReflinkUnsupported`, `InsufficientStorage`, `SourceNotFound`, `Invalid`)은 참, `ParentNotRunning`·`CopyFailed`·`""`는 거짓.
- [ ] **Step 2: 실행해 실패 확인** — `go test ./api/v1alpha1/ -run TestPermanentDataFailure` → 빌드 실패(`undefined: v1alpha1.PermanentDataFailure`).
- [ ] **Step 3: `reasons.go`를 쓰고, 컨트롤러의 문자열 이유를 모두 상수로 바꾼다.** `grep -rn '"\(StartFailed\|ParentNotRunning\|SupervisorUnavailable\|Unschedulable\|PodCreateFailed\|AgentUnavailable\|AgentPodFailed\|MainNotRunning\|InvalidNamespace\|BootstrapFailed\|CopyFailed\|InitMainFailed\)"' internal/controller/ --include='*.go' | grep -v _test`이 아무것도 내지 않아야 한다(테스트 파일의 문자열은 둔다).
- [ ] **Step 4: 확인** — `go vet ./... && go test ./api/... ./internal/controller/`와, KUBEBUILDER_ASSETS를 둔 채 `go test -count=1 -race ./internal/controller/` → 모두 `ok`.
- [ ] **Step 5: 커밋** — `refactor(api): 조건 이유를 api/v1alpha1의 상수로 내보낸다`

### Task 2: 서버 뼈대 — 라우팅, 인증, problem+json, 요청 로그

**Files:**
- Create: `internal/httpapi/server.go`, `auth.go`, `problem.go`, `log.go`, `agent.go`(인터페이스만)
- Test: `internal/httpapi/server_test.go`, `helpers_test.go`, `fake_test.go`

**Interfaces:**
- Produces:
  - `type Config struct { Client client.Client; Reader client.Reader; Agents AgentConnector; AdminKey string; StorageClass string; Now func() time.Time; Log *slog.Logger }` — `Reader`는 캐시하지 않는 읽기(Secret용).
  - `agent.go`: `type AgentAPI interface { CreateRestorePoint(ctx context.Context, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error); ListRestorePoints(ctx context.Context, branch string) ([]agentapi.RestorePoint, error); DeleteRestorePoint(ctx context.Context, branch, id string) error }` — `*agentclient.Client`가 이 셋을 구현한다(이월 사항: 쓰는 쪽에 인터페이스). `type AgentConnector interface { Agent(namespace, token string) AgentAPI }`.
  - 테스트 도우미: `fake_test.go`의 `type fakeAgents struct`(네임스페이스별 복구 지점 목록, `failNext(op string, err error)`, 받은 토큰 기록; `AgentConnector`를 구현), `helpers_test.go`의 `newTestServer(t, objs ...client.Object) (*httptest.Server, client.Client, *fakeAgents)`(가짜 클라이언트 하나를 `Client`와 `Reader`로, 관리자 키 `test-key`, 스토리지 클래스 `consonance-xfs`).
  - `func New(cfg Config) http.Handler`
  - `type problemType string` 상수 `typeValidation = "validation-error"`, `typeUnauthorized = "unauthorized"`, `typeNotFound = "not-found"`, `typeConflict = "conflict"`, `typeRateLimited = "rate-limited"`, `typeInsufficientStorage = "insufficient-storage"`, `typeInternal = "internal"`
  - `type apiError struct { Status int; Type problemType; Detail string; RetryAfter time.Duration }` + `Error() string`; 생성 도우미 `badRequest(format, args...)`, `notFound(...)`, `conflict(...)`, `internal(err)`.
  - `func writeProblem(w http.ResponseWriter, r *http.Request, err error)` — `*apiError`가 아니면 500 `internal`(`detail`은 err 문자열). `title`은 상태 코드의 `http.StatusText`.
  - `func writeJSON(w http.ResponseWriter, status int, v any)`
  - 핸들러 모양: `type handlerFunc func(w http.ResponseWriter, r *http.Request) error` — 오류를 돌려주면 `writeProblem`.
  - 로그 문맥: `logAttrs(ctx, "projectId", id, "branch", name)`로 요청 로그에 필드를 더한다(요청 끝에 한 줄, `op`=라우트 이름).
- 라우트: `GET /healthz`(인증 없음, 200 `ok`), 그 밖의 `/v1/...` 전부 인증. 맞는 경로가 없으면 404 problem(`not-found`), 메서드가 틀리면 405 problem(`validation-error`; 표에 없는 상태지만 본문은 problem+json).
  - `func (s *server) routes() []route` — `route{Method, Pattern, Op string}`. 등록과 Task 10의 OpenAPI 비교가 이 목록을 함께 쓴다.
- 인증: `Authorization: Bearer <키>`. 키 비교는 `crypto/subtle.ConstantTimeCompare`(길이가 달라도 같은 시간 내에 거짓). 헤더 없음·형식 틀림·불일치 모두 401 `unauthorized`, `WWW-Authenticate: Bearer`.

- [ ] **Step 1: 실패하는 테스트** (`server_test.go`)
  - `TestHealthzNeedsNoKey`: `GET /healthz` 키 없이 200.
  - `TestAuth`: 키 없음, `Basic ...`, `Bearer wrong`, `Bearer <키>x`(접두사가 같은 더 긴 값) 모두 401, `Content-Type: application/problem+json`, 본문 `type=="unauthorized"`, `status==401`, 헤더 `WWW-Authenticate: Bearer`. 맞는 키면 401이 아니다.
  - `TestUnknownRouteIsProblem`: 인증한 `GET /v1/nope` → 404 problem `not-found`; `PUT /v1/projects` → 405 problem.
  - `TestRequestIDIsEchoed`: `X-Request-Id: abc-1`이면 응답 헤더가 `abc-1`; 없거나 형식이 틀리면(`a b`, 65자) 새 값(비어 있지 않음, 입력과 다름).
  - `TestRequestLog`: `Config.Log`를 버퍼로 둔 JSON slog 핸들러로 주고 요청 하나 뒤 버퍼에 `"requestId"`, `"op"`, `"status"` 필드가 있는 한 줄.
- [ ] **Step 2: 실패 확인** — `go test ./internal/httpapi/` → 빌드 실패.
- [ ] **Step 3: 구현** — 위 Interfaces 그대로. 라우팅은 `http.ServeMux` 패턴(`"GET /v1/projects/{projectId}"`). `op`는 라우트 등록 때 붙인 이름(예: `project.get`).
- [ ] **Step 4: 통과 확인** — `go test -race ./internal/httpapi/` → `ok`.
- [ ] **Step 5: 커밋** — `feat(httpapi): 라우팅, 관리자 키 인증, problem+json, 요청 로그`

### Task 3: 검사, 오류 변환, 기다리기

**Files:**
- Create: `internal/httpapi/validate.go`, `wait.go`
- Modify: `internal/httpapi/problem.go`
- Test: `validate_test.go`, `problem_test.go`, `wait_test.go`

**Interfaces:**
- Produces:
  - `func validBranchName(name string) error` — DNS-1123 label(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`), 1~40자, 백업 이름 형식 `^.+-before-[0-9]{14}$`이 아니어야 함. 틀리면 `badRequest`.
  - `func validProjectID(id string) bool` — `^[a-z0-9]{12}$`.
  - `func decodeBody(r *http.Request, v any) error` — `Content-Type`이 JSON이 아니거나(비어 있으면 허용), 본문이 1MiB를 넘거나, 알 수 없는 필드가 있거나, JSON이 깨졌으면 400 `validation-error`. 본문이 비면 빈 값으로 둔다.
  - `func fromAgent(err error) error` — `*agentclient.APIError`를 문제로: `not-found`→404, `rate-limited`→429(`RetryAfter` 유지, 최소 1초), `insufficient-storage`→507, `not-running`·`conflict`·`busy`→409, `invalid`→400, 그 밖(`Code` 빈 값 포함)·네트워크 오류→500 `internal`(`detail`에 원인). 에이전트 메시지는 `detail`에 넣는다.
  - `func fromKube(err error) error` — `NotFound`→404, `AlreadyExists`·`Conflict`→409, `Invalid`→400, 그 밖→500.
  - `func waitFor(ctx context.Context, timeout time.Duration, done func(context.Context) (bool, error)) (bool, error)` — 100ms마다 `done`을 부르고, 참이면 `(true, nil)`, 제한 시간이 지나면 `(false, nil)`, `done`이 오류면 그 오류, 요청 ctx가 끝나면 ctx 오류. 처음 한 번은 기다리지 않고 바로 부른다.
- 429 응답의 `Retry-After`는 `ceil(RetryAfter.Seconds())`.

- [ ] **Step 1: 실패하는 테스트**
  - `TestValidBranchName`: 허용 `dev`, `a`, `feature-1`, 40자 `a…a`; 거부 `""`, `Dev`, `-dev`, `dev-`, `dev_1`, 41자, `main-before-20261007031500`, `x-before-12345678901234`. 허용: `before-20261007031500`(앞에 이름이 없음), `dev-before-2026`(14자리가 아님).
  - `TestValidProjectID`: 허용 `a1b2c3d4e5f6`; 거부 `A1b2c3d4e5f6`, `a1b2c3d4e5f`, `cns-a1b2c3d4e5f6`, `a1b2c3d4e5f6a`.
  - `TestDecodeBody`: 알 수 없는 필드 `{"nope":1}` → 400; `text/plain` → 400; 깨진 JSON → 400; 빈 본문 → 빈 값, 오류 없음.
  - `TestFromAgent`: 표의 각 코드 → 상태·유형; `rate-limited` + `RetryAfter: 1500ms` → `Retry-After: 2`; `Code` 빈 `APIError{StatusCode: 404}` → 500.
  - `TestFromKube`: 각 쿠버네티스 오류 → 상태.
  - `TestWaitFor`: 세 번째 호출에 참이면 `(true, nil)`이고 300ms 안팎; 끝내 거짓이면 제한 시간 200ms 뒤 `(false, nil)`; `done`의 오류는 그대로; 바로 참이면 기다리지 않음(10ms 안).
- [ ] **Step 2: 실패 확인** — `go test ./internal/httpapi/` → 빌드 실패.
- [ ] **Step 3: 구현** — Interfaces 그대로.
- [ ] **Step 4: 통과 확인** — `go test -race ./internal/httpapi/` → `ok`.
- [ ] **Step 5: 커밋** — `feat(httpapi): 이름·본문 검사, 에이전트·쿠버네티스 오류 변환, 상태 기다리기`

### Task 4: 프로젝트

**Files:**
- Create: `internal/httpapi/projects.go`
- Test: `projects_test.go`

**Interfaces:**
- Consumes: Task 2·3. `controller.ProjectName`, `controller.NamespacePrefix`, `controller.ProjectID`.
- Produces:
  - 라우트: `POST /v1/projects`(`project.create`), `GET /v1/projects`(`project.list`), `GET /v1/projects/{projectId}`(`project.get`), `DELETE /v1/projects/{projectId}`(`project.delete`).
  - `func (s *server) project(ctx context.Context, id string) (*v1alpha1.Project, error)` — 형식이 틀리면 Get 없이 404; 없으면 404 `not-found`.
  - `func (s *server) readyProject(ctx context.Context, id string) (*v1alpha1.Project, error)` — phase가 `Ready`가 아니면 409 `conflict`, `detail`은 `project <id> is <phase>: <Ready 조건 메시지>`.
  - 응답 `type projectView struct { ID string `json:"id"`; DisplayName string `json:"displayName"`; Phase string `json:"phase"`; Reason string `json:"reason,omitempty"`; Message string `json:"message,omitempty"`; Storage storageView `json:"storage"`; DefaultCompute computeView `json:"defaultCompute"`; Limits limitsView `json:"limits"`; CreatedAt time.Time `json:"createdAt"` }` — `Reason`·`Message`는 `Ready` 조건이 거짓일 때. `storageView{Size, StorageClassName, CapacityBytes, UsedBytes}`, `computeView{CPU, Memory}`(문자열), `limitsView{MaxBranches, MinCopyInterval}`.
  - 생성 본문 `type createProjectRequest struct { DisplayName string `json:"displayName"`; StorageSize string `json:"storageSize,omitempty"`; Compute *computeRequest `json:"compute,omitempty"` }`, `computeRequest{CPU, Memory string}`.
- 생성 흐름: 검사(`displayName` 1~100자, `storageSize`는 `resource.ParseQuantity`가 받고 1Gi 이상, `compute`의 둘 다 있고 양수) → `projectId` 생성(`crypto/rand`, `[a-z0-9]` 12자) → 네임스페이스 `cns-<id>`(결정 6의 라벨) → Project `project`(`displayName`, `storage.size`(주면), `storage.storageClassName`=`--storage-class`, `defaultCompute`(주면); 나머지는 CRD 기본값). 네임스페이스가 이미 있으면(우연한 충돌) 새 ID로 한 번 더, 그래도 있으면 500. Project 만들기가 실패하면 만든 네임스페이스를 지우고 오류.
- 202 본문은 `projectView`(phase가 비었으면 `Provisioning`으로 보인다).
- 삭제: 네임스페이스를 지운다(`PropagationPolicy: Background`) → 202 `{"id": ..., "phase": "Deleting"}`. 이미 지우는 중이어도 202.
- 목록: `{"projects": [projectView...]}`, `projectId` 순. 네임스페이스가 `cns-`로 시작하지 않는 Project는 뺀다.

- [ ] **Step 1: 실패하는 테스트** (가짜 클라이언트, `helpers_test.go`에 `newTestServer(t, objs ...client.Object) (*httptest.Server, client.Client, *fakeAgents)`를 둔다)
  - `TestCreateProject`: `{"displayName":"my-app"}` → 202, 본문 `id`가 12자 `[a-z0-9]`, `phase=="Provisioning"`; 네임스페이스 `cns-<id>`에 라벨 둘(결정 6); Project `project`의 `storageClassName=="consonance-xfs"`, `displayName=="my-app"`.
  - `TestCreateProjectWithSizeAndCompute`: `storageSize:"20Gi"`, `compute:{cpu:"1",memory:"2Gi"}`가 spec에 들어감.
  - `TestCreateProjectRejects`: `displayName` 없음·101자, `storageSize:"abc"`·`"512Mi"`, `compute:{cpu:"1"}`(memory 없음), 알 수 없는 필드 → 모두 400 `validation-error`, 네임스페이스가 하나도 생기지 않음.
  - `TestGetProject`: Ready 프로젝트 → 200, `phase=="Ready"`, `storage.usedBytes`가 status 값; Failed(Ready 조건 False/`ReflinkUnsupported`, 메시지 m) → `reason=="ReflinkUnsupported"`, `message==m`.
  - `TestMalformedProjectIDIsNotFound`: `/v1/projects/Abc`, `/v1/projects/cns-a1b2c3d4e5f6` → 404, 가짜 클라이언트에 Get이 오지 않음(인터셉터로 셈).
  - `TestListProjects`: 프로젝트 셋(하나는 `default` 네임스페이스) → 둘, `id` 순.
  - `TestDeleteProject`: 202, 네임스페이스에 삭제 표시(가짜 클라이언트에서는 사라짐); 없는 ID → 404.
- [ ] **Step 2: 실패 확인** — `go test ./internal/httpapi/ -run Project` → 실패.
- [ ] **Step 3: 구현** — Interfaces와 흐름 그대로.
- [ ] **Step 4: 통과 확인** — `go test -race ./internal/httpapi/` → `ok`.
- [ ] **Step 5: 커밋** — `feat(httpapi): 프로젝트 생성·목록·조회·삭제`

### Task 5: 브랜치 생성·조회·삭제

**Files:**
- Create: `internal/httpapi/branches.go`
- Test: `branches_test.go`

**Interfaces:**
- Consumes: Task 4의 `project`·`readyProject`, Task 3, `controller.LabelBackupOf`. `restorePointId`로 만드는 브랜치의 출처 확인(에이전트 목록에 있는지)은 Task 6이 더한다.
- Produces:
  - 라우트: `POST /v1/projects/{projectId}/branches`(`branch.create`), `GET .../branches`(`branch.list`), `GET .../branches/{branch}`(`branch.get`), `DELETE .../branches/{branch}`(`branch.delete`).
  - `func (s *server) branch(ctx, ns, name string) (*v1alpha1.Branch, error)` — 이름 형식이 틀리면 Get 없이 404, 없으면 404.
  - `func isBackup(b *v1alpha1.Branch) bool` — 라벨 `consonance.io/backup-of`가 있으면 참.
  - `func countBranches(ctx, ns string) (user, backups int, err error)`
  - 응답 `type branchView struct { Name, Parent string; Phase string; State string; Protected bool; BackupOf string `json:"backupOf,omitempty"`; Reason, Message string `json:",omitempty"`; Endpoint *endpointView; CreatedFrom *createdFromView; LastRestore *restoreView; DataReadyAt, RunningAt *time.Time; CreatedAt time.Time }` — JSON 이름은 camelCase(`parent`, `phase`, `state`, `protected`, `backupOf`, `reason`, `message`, `endpoint{host,port}`, `createdFrom{branch,restorePointId}`, `lastRestore{requestId,phase,backupBranch,restorePointId,message}`, `dataReadyAt`, `runningAt`, `createdAt`). `Reason`·`Message`는 phase가 `Failed`일 때 `DataReady` 또는 `Ready` 조건(거짓인 쪽, `DataReady` 먼저)의 것.
  - 생성 본문 `type createBranchRequest struct { Name string; Parent string; RestorePointID string `json:"restorePointId,omitempty"`; State string `json:"state,omitempty"` }`.
- 생성 흐름(순서대로, 첫 실패에서 멈춤):
  1. 본문 검사: `name`은 `validBranchName`, `main`이면 409(이미 있음), `parent` 기본 `main`(형식 검사), `state` 기본 `Running`(`Running`·`Stopped`만).
  2. `readyProject` (아니면 409).
  3. 같은 이름의 Branch가 있으면 409 `conflict`.
  4. 부모가 없으면 404; `restorePointId`가 없을 때(부모의 현재 상태를 복사) 부모 phase가 `Running`이 아니면 409 `conflict`(`detail`: `parent <p> is <phase>`).
  5. 사용자 브랜치 수 ≥ `maxBranches`이면 429 `rate-limited`, `Retry-After: 60`(`detail`: `the project has <n> of <max> branches`) — 표의 "브랜치 최대 개수"다.
  6. Branch를 만든다: `spec.parent`, `spec.source`(`restorePointId`가 있으면 `RestorePoint`), `spec.state`. 소유자 없음.
  7. 최대 5초 기다린다: `status.dataGeneration > 0` 또는 phase가 `Failed`.
     - 데이터 준비됨 → 201 `branchView`.
     - `Failed`이고 `DataReady` 조건 이유가 `v1alpha1.PermanentDataFailure` → 그 이유를 오류로: `SourceNotFound`→404, `InsufficientStorage`→507, `ReflinkUnsupported`·`Invalid`→409(프로젝트 상태 문제), 실패한 Branch는 그대로 둔다(사용자가 지운다; `detail`에 브랜치 이름과 조건 메시지).
     - 제한 시간 → 202 `branchView`.
- 목록: `{"branches": [branchView...]}`, 이름 순, 백업 브랜치 포함(`backupOf`로 구분).
- 삭제: main이면 409 `conflict`. 그 밖은 Delete → 202 `{"name":..., "phase":"Deleting"}`. 이미 지우는 중이어도 202.

- [ ] **Step 1: 실패하는 테스트** (`helpers_test.go`에 `readyProjectObjs(id string) []client.Object`(네임스페이스, Ready Project, Running main), `setBranchStatus(t, c, ns, name, func(*v1alpha1.BranchStatus))`, `afterDelay(d, func())`를 둔다)
  - `TestCreateBranchWaitsForData`: 요청 뒤 300ms에 `dataGeneration=1`로 바꾸는 고루틴 → 201, `name=="dev"`, `parent=="main"`; Branch spec `source.type==ParentLatest`, `state==Running`.
  - `TestCreateBranchAfterFiveSecondsIsAccepted`: 상태를 바꾸지 않음 → 202, 걸린 시간 5초 이상 6초 미만(이 테스트만 `Now`를 쓰지 않고 실제로 기다린다).
  - `TestCreateBranchFromRestorePoint`: `restorePointId`를 주면 `source.type==RestorePoint`, 부모가 `Stopped`여도 409가 아니다(확인은 Task 6이 더한다).
  - `TestCreateBranchRejects`: 이름 `Dev`·`main-before-20261007031500`·41자 → 400; `state:"Paused"` → 400; 없는 부모 → 404; 정지된 부모 → 409; 같은 이름 → 409; `main` → 409.
  - `TestBranchInAProjectThatIsNotReadyIsConflict`: Provisioning·Failed 프로젝트 → 409, `detail`에 phase.
  - `TestBranchLimit`: `maxBranches:2`이고 main+`a` → 429, `Retry-After: 60`; 백업 브랜치(라벨) 둘이 더 있어도 사용자 브랜치가 1이면 생성됨.
  - `TestCreateBranchPermanentFailure`: 상태를 `Failed`+`DataReady=False/SourceNotFound`로 → 404; `InsufficientStorage` → 507.
  - `TestGetAndListBranches`: 상세의 `endpoint`, `createdFrom`, `lastRestore`, 시각; `Failed`면 `reason`·`message`; 목록은 이름 순이고 백업 브랜치의 `backupOf`.
  - `TestDeleteBranch`: main → 409; `dev` → 202; 없는 이름 → 404.
- [ ] **Step 2: 실패 확인** — `go test ./internal/httpapi/ -run Branch` → 실패.
- [ ] **Step 3: 구현** — 흐름 그대로. 동기 대기는 `waitFor(ctx, 5*time.Second, ...)`.
- [ ] **Step 4: 통과 확인** — `go test -race ./internal/httpapi/` → `ok`.
- [ ] **Step 5: 커밋** — `feat(httpapi): 브랜치 생성(데이터 준비까지 대기)·목록·조회·삭제`

### Task 6: 에이전트 연결과 복구 지점

**Files:**
- Create: `internal/httpapi/restorepoints.go`
- Modify: `internal/httpapi/agent.go`, `fake_test.go`
- Modify: `internal/httpapi/branches.go`(복구 지점으로 만드는 브랜치의 출처 확인)
- Test: `restorepoints_test.go`, `branches_test.go`

**Interfaces:**
- Produces:
  - Task 2의 `AgentAPI`·`AgentConnector`를 쓴다. `type HTTPAgents struct{ HTTP *http.Client }`는 `agentclient.New(controller.AgentURL(ns), token, a.HTTP)`.
  - `func (s *server) agent(ctx, ns string) (AgentAPI, error)` — `Reader`로 Secret `internal-token`(키 `token`)을 읽는다. 없거나 비면 500.
  - `func (s *server) restorePointOf(ctx, ns, branch, id string) (agentapi.RestorePoint, error)` — 에이전트 목록에 그 ID가 없으면 404 `not-found`.
  - 라우트: `POST .../branches/{branch}/restore-points`(`restorepoint.create`), `GET .../restore-points`(`restorepoint.list`), `DELETE .../restore-points/{restorePointId}`(`restorepoint.delete`).
  - 응답 `type restorePointView struct { ID, Branch string; CreatedAt time.Time; Reason, Label string; BackupStartLSN string }`(JSON camelCase), 목록 `{"restorePoints": [...]}`(에이전트 순서 = 최신 먼저).
- 생성: `readyProject` → 브랜치 있음(404) → 브랜치 phase `Running`(아니면 409; 복구 지점은 실행 중인 DB의 백업 모드 복사다) → 본문 `{label?}`, 라벨 256바이트 초과 400 → 에이전트 `CreateRestorePoint(branch, {Label, Reason: "api"})` → 201. 에이전트 오류는 `fromAgent`(최소 복사 간격 → 429 + `Retry-After`, 사용률 90% → 507).
- 목록: 에이전트가 404(데이터가 아직 없음, 이월 사항)를 주면 빈 목록 200.
- 삭제: 에이전트 `DeleteRestorePoint` → 204, 404는 404(결정 5).
- 복구 지점 ID 형식(`^rp-[0-9]{8}T[0-9]{6}Z-[a-z0-9]{4}$`)이 틀리면 에이전트에 묻지 않고 404.
- 브랜치 생성의 `restorePointId`: `restorePointOf(ns, parent, id)`로 확인(없으면 404) — Task 5의 흐름 4번 자리에서.
- `fake_test.go`의 `fakeAgents`에 에이전트처럼 행동을 더한다: 브랜치 데이터가 없으면 목록이 404, 만든 복구 지점의 ID는 형식 `rp-<yyyymmddThhmmss>Z-<4자>`.

- [ ] **Step 1: 실패하는 테스트**
  - `TestCreateRestorePoint`: 201, 본문 `reason=="api"`, `label=="before schema change"`; 가짜가 받은 토큰이 Secret의 값.
  - `TestCreateRestorePointRejects`: 257바이트 라벨 → 400; 정지된 브랜치 → 409; 없는 브랜치 → 404; 준비 안 된 프로젝트 → 409.
  - `TestCreateRestorePointAgentErrors`: 가짜가 `rate-limited`+`RetryAfter: 3s` → 429, `Retry-After: 3`; `insufficient-storage` → 507; 네트워크 오류 → 500.
  - `TestListRestorePoints`: 둘 → 순서 유지; 에이전트 404 → 200 `{"restorePoints":[]}`.
  - `TestDeleteRestorePoint`: 204; 에이전트 404 → 404; 형식 틀린 ID → 404이고 에이전트 호출 없음.
  - `TestMissingTokenIsInternal`: `internal-token`이 없으면 500 `internal`.
  - `TestCreateBranchFromAnUnknownRestorePointIsNotFound`(branches_test): 부모의 목록에 없는 ID → 404, Branch가 생기지 않음.
- [ ] **Step 2: 실패 확인** — `go test ./internal/httpapi/ -run RestorePoint` → 실패.
- [ ] **Step 3: 구현**.
- [ ] **Step 4: 통과 확인** — `go test -race ./internal/httpapi/` → `ok`.
- [ ] **Step 5: 커밋** — `feat(httpapi): 복구 지점 생성·목록·삭제, 복구 지점에서 만드는 브랜치의 출처 확인`

### Task 7: 접속 정보

**Files:**
- Create: `internal/httpapi/connection.go`
- Test: `connection_test.go`

**Interfaces:**
- 라우트 `GET .../branches/{branch}/connection`(`branch.connection`).
- 응답 `type connectionView struct { Host string; Port int32; Database, User, Password, URI string }`(JSON `host, port, database, user, password, uri`).
- 값: `host`·`port`는 Branch `status.endpoint`, `database`·`user`·`password`는 Secret `db-credentials`의 `database`·`username`·`password`(`Reader`로). `uri`는 `postgresql://<user>:<password>@<host>:<port>/<database>`를 `net/url`로 만든다(비밀번호를 이스케이프).
- `status.endpoint`가 아직 없으면 409 `conflict`(`detail`: `branch <b> has no endpoint yet`). Secret이 없으면 500.
- 응답 헤더 `Cache-Control: no-store`. 요청 로그에 비밀번호를 남기지 않는다.

- [ ] **Step 1: 실패하는 테스트** — `TestConnection`: 200, 여섯 필드, 비밀번호 `p@ss/w:rd`가 `uri`에서 이스케이프되고 `url.Parse(uri)`로 되읽으면 원래 값; `Cache-Control: no-store`. `TestConnectionWithoutEndpointIsConflict` → 409. `TestConnectionIsNotLogged`: 로그 버퍼에 비밀번호 문자열이 없다.
- [ ] **Step 2: 실패 확인**.
- [ ] **Step 3: 구현**.
- [ ] **Step 4: 통과 확인** — `go test -race ./internal/httpapi/` → `ok`.
- [ ] **Step 5: 커밋** — `feat(httpapi): 브랜치 접속 정보`

### Task 8: 정지·기동

**Files:**
- Create: `internal/httpapi/state.go`
- Test: `state_test.go`

**Interfaces:**
- 라우트 `POST .../branches/{branch}/stop`(`branch.stop`), `POST .../branches/{branch}/start`(`branch.start`).
- 흐름: `project`(Ready가 아니어도 된다: 정지는 언제나 할 수 있어야 한다 — 단 `start`는 `readyProject`) → 브랜치(404) → 롤백·리셋이 진행 중이면 409(`restorePending`과 같은 판정, Task 9의 `restoreOpen(b)`를 이 작업에서 먼저 만든다) → `spec.state`를 바꾼다(충돌이면 다시 읽어 최대 3번) → 최대 10초 기다림: phase가 원하는 상태(`Stopped`·`Running`)면 200 `branchView`, 제한 시간이면 202 `branchView`. 기동 중 phase가 `Failed`(조건 `Ready=False/StartFailed`)가 되면 500 `internal`(`detail`에 조건 메시지).
- 이미 원하는 상태면 spec을 바꾸지 않고 바로 200.
- Produces: `func restoreOpen(b *v1alpha1.Branch) bool` — `lastRestore`가 끝나지 않았거나(`Done`·`Failed`가 아님), `spec.restore.requestId`가 `lastRestore.requestId`와 다르면 참.

- [ ] **Step 1: 실패하는 테스트** — `TestStopWaits`(200ms 뒤 phase Stopped → 200, spec.state Stopped); `TestStartAfterTenSecondsIsAccepted`(→ 202, 10초 이상 11초 미만); `TestStartFailure`(phase Failed+StartFailed → 500); `TestStopWhileRestoringIsConflict`; `TestStopAlreadyStoppedDoesNotWrite`(Update 인터셉터 호출 없음, 200); `TestStateUpdateRetriesConflict`(첫 Update에 Conflict를 돌려주는 인터셉터 → 그래도 200); `TestRestoreOpen` 표(빈 상태, 진행 중, 끝남, 새 requestId).
- [ ] **Step 2: 실패 확인**.
- [ ] **Step 3: 구현**.
- [ ] **Step 4: 통과 확인** — `go test -race ./internal/httpapi/` → `ok`.
- [ ] **Step 5: 커밋** — `feat(httpapi): 브랜치 정지·기동`

### Task 9: 롤백·리셋

**Files:**
- Create: `internal/httpapi/restore.go`
- Test: `restore_test.go`

**Interfaces:**
- Consumes: Task 5·6·8(`restoreOpen`, `restorePointOf`, `countBranches`).
- 라우트 `POST .../branches/{branch}/reset`(`branch.reset`), `POST .../branches/{branch}/rollback`(`branch.rollback`, 본문 `{restorePointId}` 필수).
- 흐름:
  1. `readyProject` → 브랜치(404).
  2. 리셋이고 main이면 409(스펙 5장). 롤백은 main도 된다.
  3. `restoreOpen(b)`면 409 `conflict`(`detail`: `a rollback or reset is in progress`).
  4. 리셋: 부모 phase가 `Running`이 아니면 409(이월 사항). 롤백: `restorePointOf(ns, branch, id)`(없으면 404 — 다른 브랜치의 복구 지점도 404), 백업 브랜치 수 ≥ `maxBranches`이면 429(`Retry-After: 60`, `detail`에 오래된 백업 브랜치를 지우라는 말, 결정 1).
  5. `requestId`는 요청의 `X-Request-Id`(로그와 같은 값, 64자 이하라 `RestoreRequest.requestId`에 맞다)다. 같은 `X-Request-Id`로 다시 보낸 요청은 앞 요청과 같은 것이다: `lastRestore.requestId`가 이미 그 값이면 3·4번을 건너뛰고 spec을 쓰지 않은 채 6번으로 가서 앞 결과를 돌려준다. `spec.restore = {requestId, source}`(리셋은 `ParentLatest`, 롤백은 `RestorePoint`+ID)를 쓴다. `Update`가 Conflict면 다시 읽어 3번에서부터 다시(최대 3번).
  6. 최대 30초 기다림: `lastRestore.requestId == requestId`이고 phase가 `Done`·`Failed`.
     - `Done` → 200 `{"branch": branchView, "backupBranch": ..., "restorePointId": ...}`(롤백은 `backupBranch`, 리셋은 `restorePointId`, 비면 생략).
     - `Failed` → 500 `internal`, `detail`은 `lastRestore.message`.
     - 제한 시간 → 202 `{"branch": branchView, "requestId": ...}`.
- 응답 `type restoreResult struct { Branch branchView `json:"branch"`; RequestID string `json:"requestId,omitempty"`; BackupBranch string `json:"backupBranch,omitempty"`; RestorePointID string `json:"restorePointId,omitempty"` }`.

- [ ] **Step 1: 실패하는 테스트**
  - `TestRollback`: 300ms 뒤 `lastRestore={requestId: <그 값>, phase: Done, backupBranch: "dev-before-20261024010203"}` → 200, `backupBranch`; Branch spec `restore.source.type==RestorePoint`, ID 일치; `requestId`가 응답 헤더 `X-Request-Id`와 같다.
  - `TestReset`: Done + `restorePointId` → 200, `restorePointId`.
  - `TestResetOfMainIsConflict` → 409, spec을 쓰지 않음.
  - `TestResetNeedsARunningParent` → 409.
  - `TestSecondRestoreWhileOneRunsIsConflict`: `lastRestore.phase=Starting`이면 409; `spec.restore.requestId`가 `lastRestore.requestId`와 다르면(아직 시작 안 함) 409.
  - `TestRollbackToAnotherBranchesRestorePointIsNotFound`: 가짜 에이전트에서 ID가 `main`의 것 → `dev` 롤백 404.
  - `TestRollbackNeedsRoomForTheBackup`: `maxBranches:2`, 백업 브랜치 2 → 429, `Retry-After: 60`.
  - `TestRollbackRejects`: 본문 없음·`restorePointId` 형식 틀림 → 400.
  - `TestRestoreFailed`: phase Failed, message m → 500, `detail==m`.
  - `TestRestoreAfterThirtySecondsIsAccepted`: → 202, `requestId` 포함, 30초 이상 31초 미만.
  - `TestRestoreWithTheSameRequestIDReturnsTheEarlierResult`: `lastRestore={requestId:"r1", phase: Done, backupBranch: b}`인 브랜치에 `X-Request-Id: r1`로 롤백 → 200, `backupBranch==b`, Branch spec에 Update가 오지 않음.
- [ ] **Step 2: 실패 확인**.
- [ ] **Step 3: 구현**.
- [ ] **Step 4: 통과 확인** — `go test -race ./internal/httpapi/` → `ok`.
- [ ] **Step 5: 커밋** — `feat(httpapi): 롤백·리셋(진행 중이면 409, 30초까지 대기)`

### Task 10: OpenAPI 3.1 문서

**Files:**
- Create: `internal/httpapi/openapi.json`, `openapi.go`
- Test: `openapi_test.go`

**Interfaces:**
- 라우트 `GET /v1/openapi.json`(인증 없음, `application/json`). 문서는 `//go:embed openapi.json`.
- 문서: `openapi: "3.1.0"`, `info.title: "Consonance API"`, `info.version: "v1alpha1"`, `components.securitySchemes.adminKey`(http bearer), 전역 `security: [{adminKey: []}]`, 모든 경로와 메서드, 응답 코드(스펙 5장 표의 성공 코드와 오류 유형), 스키마 `Project`, `Branch`, `RestorePoint`, `Connection`, `RestoreResult`, `Problem`(`type` enum은 일곱 유형). 복구 지점 라벨 `maxLength: 256`과 "UTF-8 바이트" 설명, 브랜치 이름 `pattern`·`maxLength: 40`과 백업 이름 금지 설명, 동기 대기 제한 시간(5·10·30초)을 각 경로 설명에 적는다.

- [ ] **Step 1: 실패하는 테스트**
  - `TestOpenAPIIsServedWithoutKey`: 200, JSON, `openapi=="3.1.0"`.
  - `TestOpenAPICoversEveryRoute`: 서버가 등록한 라우트 목록(`routes()`가 `[]route{method, pattern, op}`를 돌려주게 Task 2의 등록을 이 작업에서 노출한다)과 문서의 `paths`의 (메서드, 경로) 쌍이 정확히 같다(`/healthz`와 `/v1/openapi.json`은 문서에서 빼도 된다 — 둘을 비교에서 제외).
  - `TestOpenAPIProblemTypes`: `Problem.type` enum이 일곱 값과 같다.
- [ ] **Step 2: 실패 확인**.
- [ ] **Step 3: 문서를 쓰고 핸들러를 단다.**
- [ ] **Step 4: 통과 확인** — `go test -race ./internal/httpapi/` → `ok`.
- [ ] **Step 5: 커밋** — `feat(httpapi): OpenAPI 3.1 문서`

### Task 11: 실행 파일 `cmd/apiserver`와 ClusterRole

**Files:**
- Create: `cmd/apiserver/main.go`, `cmd/apiserver/main_test.go`, `internal/httpapi/rbac.go`(RBAC 표지만)
- Generate: `config/rbac/apiserver/role.yaml`
- Modify: `Makefile`(generate에 두 번째 rbac 줄)

**Interfaces:**
- 플래그: `--listen-address`(`:8080`), `--admin-key-file`(필수), `--storage-class`(`consonance-xfs`), `--kubeconfig`(비우면 `$KUBECONFIG`, 클러스터 안, `~/.kube/config` 순 — 오퍼레이터와 같다). 남는 인자는 거부.
- 동작: 키 파일을 `secretfile.Read`로 읽고 비면 시작하지 않는다(종료 코드 1). controller-runtime 캐시(`Project`, `Branch`; Secret은 캐시하지 않음 — `client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}}`)와 매니저 없는 캐시 시작(`cache.New` + `Start` + `WaitForCacheSync`) 뒤 `http.Server`(ReadHeaderTimeout 10s). SIGTERM·SIGINT를 받으면 `Shutdown`(진행 중 요청을 최대 35초 기다림 — 롤백 대기 30초보다 길게) 뒤 종료 코드 0. 로그는 slog JSON.
- RBAC 표지(`internal/httpapi/rbac.go`): namespaces `get;list;watch;create;delete`, consonance.io projects `get;list;watch;create`, branches `get;list;watch;create;update;delete`, secrets `get`. Makefile: `$(CONTROLLER_GEN) rbac:roleName=consonance-apiserver paths=./internal/httpapi/... output:rbac:artifacts:config=config/rbac/apiserver`.

- [ ] **Step 1: 실패하는 테스트** (`cmd/apiserver/main_test.go`, envtest, `cmd/operator/main_test.go`의 구조를 따른다)
  - `TestAPIServerWithItsOwnPermissions`: `config/rbac/apiserver/role.yaml`만 묶인 사용자로 실행 파일을 띄운다(`--admin-key-file`은 임시 파일). 먼저 그 사용자가 pods 목록에서 Forbidden인지 확인. 그다음 HTTP로 `POST /v1/projects` → 202, 네임스페이스와 Project가 생김; `GET /v1/projects/{id}` → 200; `POST .../branches`(Project status를 관리자로 Ready로, main Branch를 Running으로 만든 뒤) → 202(오퍼레이터가 없어 데이터가 생기지 않음); `GET .../connection`은 endpoint가 없어 409(Secret `get` 권한 경로까지 — 관리자로 `db-credentials`를 만들어 둔다; endpoint도 관리자로 넣으면 200); `DELETE /v1/projects/{id}` → 202. 실행 파일 로그에 `Forbidden`이 없다. SIGTERM → 종료 코드 0.
  - `TestRefusesAnEmptyAdminKey`: 빈 키 파일 → 종료 코드 1, 출력에 이유.
  - `TestRefusesStrayArguments`.
- [ ] **Step 2: 실패 확인** — `go test ./cmd/apiserver/` → `no non-test Go files`.
- [ ] **Step 3: 구현과 `make generate`** — `config/rbac/apiserver/role.yaml`에 ClusterRole `consonance-apiserver`.
- [ ] **Step 4: 통과 확인** — KUBEBUILDER_ASSETS를 둔 채 `go test -count=1 -v ./cmd/apiserver/` → 셋 PASS; `gofmt -l . && make test && make test-envtest` → 모두 `ok`.
- [ ] **Step 5: 커밋** — `feat(apiserver): API 서버 실행 파일과 controller-gen이 만드는 ClusterRole`

### Task 12: 문서

**Files:**
- Modify: `docs/superpowers/specs/2026-10-07-branching-engine-design.md`, `docs/superpowers/plans/2026-10-07-branching-engine-index.md`, `docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`

- [ ] **Step 1: 스펙**
  - 5장: 인증 줄에 키 이름(`consonance-admin-key`의 `key`, `--admin-key-file`)을, 표 아래에 "결정" 1·4·5와 브랜치 이름 규칙(백업 이름 형식 금지), 복구 지점 라벨 256바이트, `X-Request-Id`를 한 줄씩 더한다.
  - 7.4: 동기 대기는 캐시를 100ms마다 읽는 것, Secret은 캐시하지 않음, 종료 때 35초까지 요청을 기다림.
  - 4.1: 프로젝트 네임스페이스 라벨(결정 6).
- [ ] **Step 2: 목차** — 계획 4 행을 `[REST API 서버](2026-10-24-branching-engine-4-apiserver.md)`, 상태 `구현 완료`로.
- [ ] **Step 3: 이월 사항** — 두 "계획 4(REST API 서버)가 반영할 것" 절을 지운다(계획 2 아래의 절은 `### 미뤄 둔 개선 사항` 바로 앞까지, 계획 3 아래의 절은 `### 계획 5` 바로 앞까지). 남은 것(Project phase `Pending` 미사용, 이유 상수 이전은 끝남)은 확인해 처리된 것만 지운다. 파일 끝에 `## 계획 4(REST API 서버)에서` 절과 `### 계획 5(배포·E2E·성능)가 반영할 것`을 더한다:
  - API 서버 Deployment는 `consonance-system`에서 파드 라벨 `app.kubernetes.io/name: consonance-apiserver`, ClusterRole `consonance-apiserver`(`config/rbac/apiserver/role.yaml`), Secret `consonance-admin-key`(키 `key`)를 파일로 마운트해 `--admin-key-file`로 준다. `consonance/apiserver` 이미지는 아직 없다(스펙 7.6).
  - 외부 노출(인그레스·TLS)은 계획 5에서 정한다. API 서버는 평문 HTTP로 듣는다.
  - E2E 시나리오(스펙 12장)는 이 API로 돈다.
  - 스펙 11장의 메트릭은 API 서버에도 없다(오퍼레이터·에이전트와 같이 계획 5).
  - 실행이 끝날 때 컨트롤러가 리뷰에서 미뤄 둔 지적을 이 절 아래 `### 미뤄 둔 개선 사항`으로 더한다.
- [ ] **Step 4: 확인** — `grep -c '계획 4(REST API 서버)가 반영할 것' docs/superpowers/plans/2026-10-07-branching-engine-carryover.md` → `0`; `grep -n '| 4 |' docs/superpowers/plans/2026-10-07-branching-engine-index.md`의 상태가 `구현 완료`.
- [ ] **Step 5: 커밋** — `docs: 스펙·목차·이월 사항에 계획 4(API 서버)의 결정 반영`
