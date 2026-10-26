# 브랜칭 엔진 계획 5: 배포·E2E·성능 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 브랜칭 엔진 전체(오퍼레이터, API 서버, 볼륨 에이전트, 브랜치 이미지)를 개발용 k3s에 올리는 매니페스트와 이미지를 만들고, 스펙 12장의 E2E 시나리오와 15장의 성능 기준을 실제 클러스터에서 확인한다. 앞 계획이 미뤄 둔 운영 항목(권한 축소, 메트릭, requestId 전달, 종료 처리)도 여기서 끝낸다.

**Architecture:** 개발·시험 환경은 사용자의 Podman 머신(Fedora CoreOS 44, 커널 7.1, `/var`가 XFS reflink)이다. 그 안에 k3s를 특권 rootful 컨테이너 `consonance-k3s`로 띄우고, 머신의 `/var/lib/consonance`(XFS)를 넘겨 로컬 경로 프로비저너가 그 아래에 볼륨을 만들게 한다. 이것이 스토리지 클래스 `consonance-xfs`다. PostgreSQL·XFS가 필요한 단위 테스트도 같은 머신의 컨테이너에서 돈다. 이미지는 Mac에서 `docker build`(Podman)로 만들고 k3s의 containerd로 옮긴다. E2E와 성능 측정은 Mac에서 도는 Go 테스트이고, `kubectl port-forward`로 API 서버와 브랜치 PostgreSQL에 닿는다.

**Tech Stack:** Go 1.27, controller-runtime v0.25.2, k3s(컨테이너 `rancher/k3s:v1.36.5-k3s1`), Podman 6.1, kustomize(`kubectl apply -k`), Prometheus client(controller-runtime 메트릭 레지스트리), pgx v5, pgbench(PostgreSQL 17 이미지 안), `github.com/pb33f/libopenapi`(OpenAPI 3.1 검증).

**Spec:** `docs/superpowers/specs/2026-10-07-branching-engine-design.md` (7.5·7.6·8·11·12·13·15·16장). 앞 계획에서 넘어온 사항: `docs/superpowers/plans/2026-10-07-branching-engine-carryover.md`의 "계획 5(배포·E2E·성능)가 반영할 것" 두 절(계획 3, 계획 4)과 계획 1·2의 관련 항목.

## Global Constraints

- 이미지 4종(스펙 7.6): `consonance/postgres:17-dev`, `consonance/agent:dev`, `consonance/operator:dev`, `consonance/apiserver:dev`. 모든 파드는 Pod Security Standard `restricted`(스펙 7.5, 15장 4)로 뜬다. 오퍼레이터·API 서버 이미지는 `gcr.io/distroless/static-debian12:nonroot`(uid 65532) 위에 정적 바이너리 하나다.
- 시스템 네임스페이스 `consonance-system`. 파드 라벨 `app.kubernetes.io/name: consonance-operator` / `consonance-apiserver`(네트워크 정책이 이 라벨로 가린다, 스펙 8). 서비스 계정 `consonance-operator`, `consonance-apiserver`.
- 관리자 키 Secret `consonance-system/consonance-admin-key`(키 `key`, 32바이트 이상)는 저장소에 넣지 않는다. 스크립트가 만든다.
- 스토리지 클래스 `consonance-xfs`(XFS + reflink). API 서버 `--storage-class` 기본값과 같다.
- 성능 기준(스펙 15장, 20회, 부하 없음과 pgbench 쓰기 부하 각각): 브랜치 생성 API P50 ≤ 1초, 새 브랜치 첫 쿼리 P95 ≤ 3초, 롤백 ≤ 10초, 정지 → 실행 P50 ≤ 1초. 프로젝트 생성 시간은 측정해 기록한다.
- E2E 시나리오(스펙 12장): 프로젝트 생성 → 브랜치 생성 → 브랜치에 쓰기 → main 불변 확인 → 복구 지점 → 테이블 삭제 → 롤백 → 데이터 복원 확인 → 정지·기동 → 브랜치 삭제 → 프로젝트 삭제.
- 메트릭 이름(스펙 11장): `consonance_branch_create_seconds`, `consonance_branch_first_ready_seconds`, `consonance_restore_seconds`, `consonance_start_seconds`, `consonance_project_create_seconds`, `consonance_copy_errors_total`.
- 모든 커밋의 트레일러는 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. `bin/`·`spikes/`는 커밋하지 않는다. 사용자의 Podman 머신에서 만드는 것은 이름이 `consonance-`로 시작하는 컨테이너·이미지·디렉터리(`/var/lib/consonance`)뿐이고, 다른 것은 건드리지 않는다.

## 결정 (스펙이나 이월 사항이 정하지 않아 이 계획이 정한 것)

1. **환경:** Lima VM 대신 Podman 머신(사용자 선택, 2026-10-26). k3s는 머신 안의 rootful 특권 컨테이너 `consonance-k3s`(`--security-opt label=disable`, SELinux는 머신 그대로 Enforcing)다. 머신 자체에 k3s를 설치하지 않는다.
2. **PostgreSQL·XFS 단위 테스트:** `hack/vm-*.sh`와 `make vm-setup`·`test-vm`을 지우고 `hack/pg-test.sh`, `make test-pg`(전부)·`make test-xfs`(volume)로 바꾼다. 머신에서 `golang:1.27-bookworm` + PostgreSQL 17 컨테이너가 저장소(`/Users/...`, virtiofs)와 XFS 디렉터리를 마운트해 돈다.
3. **API 서버 외부 노출:** 개발용만 다룬다. E2E·성능은 `kubectl port-forward`를 쓴다. 인그레스·TLS는 연결 게이트웨이 서브프로젝트로 넘긴다(이월 사항에 적는다).
4. **API 서버의 Secret 권한:** 클러스터 전체 `secrets get`을 뺀다. 오퍼레이터가 프로젝트 네임스페이스마다 RoleBinding `consonance-apiserver`를 만들어 ClusterRole `consonance-apiserver-project`(Secret `get`, `resourceNames: [db-credentials, internal-token]`)를 서비스 계정 `consonance-system/consonance-apiserver`에 묶는다. 네임스페이스 삭제는 ValidatingAdmissionPolicy `consonance-apiserver-namespace-delete`가 라벨 `app.kubernetes.io/managed-by: consonance-apiserver`가 있는 것으로 제한한다.
5. **메트릭:** 여섯 개 모두 오퍼레이터가 낸다(생애 시각을 보는 쪽이 오퍼레이터다). controller-runtime 메트릭 서버(`:8080/metrics`)에 등록한다. 개발 매니페스트에서는 인증 없이 클러스터 안에서만 열린다.
6. **requestId 전달:** 오퍼레이터는 롤백·리셋 단계의 에이전트·감독 프로세스 호출에 헤더 `X-Request-Id: <spec.restore.requestId>`를 붙인다. 에이전트와 감독 프로세스는 받은 값을 로그 필드 `requestId`로 남긴다. 그 밖의 호출은 헤더 없이 둔다.
7. **이 계획에서 하지 않는 것(이월 사항에 남긴다):** 노드 장애 주입과 네트워크 RWO 스토리지의 에이전트 교체 E2E(단일 노드·로컬 스토리지라 재현할 수 없다), pg_bigm·PGroonga 포함 여부(스펙 16장 결정 대기), 리셋 취소와 Project phase `Pending`(스펙 결정 대기).

## Review Focus

1. 개발 클러스터·배포 스크립트를 두 번 돌림 → 두 번째도 오류 없이 같은 상태(이미 있는 컨테이너·Secret·리소스를 그대로 둔다). (Task 2 `dev-cluster up` 재실행 확인, Task 7 `deploy-dev` 재실행 확인)
2. E2E가 중간에 실패함 → 만든 프로젝트를 지우고 끝난다(다음 실행이 깨끗하다). (Task 10의 `t.Cleanup` 삭제와 `TestE2ELeavesNothingBehind`)
3. 이미지가 PSS restricted를 어김(root로 돎, 권한 상승) → 프로젝트 네임스페이스와 `consonance-system`의 `enforce: restricted` 라벨이 막고 E2E가 실패로 드러낸다. (Task 7의 네임스페이스 라벨, Task 10의 `TestPodsAreRestricted`)
4. 성능 측정에 이미지 내려받기·첫 실행 비용이 섞임 → 측정 전에 이미지를 미리 넣고 첫 회는 버린다. (Task 11의 워밍업)
5. 관리자 키 Secret이 저장소나 로그에 남음 → 스크립트는 `bin/dev/admin-key`(무시됨)에만 쓰고, 출력에 키를 찍지 않는다. (Task 7 Step 3)

---

## 파일 구조

| 파일 | 책임 |
| --- | --- |
| `hack/pg-test.sh`, `images/test/Dockerfile` | PostgreSQL·XFS 단위 테스트 컨테이너 (Task 1) |
| `hack/dev-cluster.sh`, `config/dev/storageclass.yaml` | 개발 k3s 컨테이너, 스토리지 클래스, kubeconfig (Task 2) |
| `images/operator/Dockerfile`, `images/apiserver/Dockerfile`, `hack/load-images.sh` | 이미지 2종 추가, k3s로 옮기기 (Task 3) |
| `internal/controller/*.go`(RBAC 표지), `cmd/operator/main.go` | 오퍼레이터 권한 축소, `--leader-election-namespace` (Task 4) |
| `internal/controller/apiserver_access.go`, `internal/httpapi/rbac.go`, `config/rbac/apiserver/project-role.yaml`, `config/policy/namespace-delete.yaml` | API 서버 권한 축소 (Task 5) |
| `internal/controller/metrics.go` | 메트릭 여섯 개 (Task 6) |
| `config/default/*`, `hack/deploy-dev.sh` | kustomize 개발 매니페스트, 배포 (Task 7) |
| `internal/agentclient`, `internal/supervisorclient`, `internal/agent`, `internal/supervisor`, `internal/controller/restore.go` | requestId 전달 (Task 8) |
| `internal/httpapi/server.go`, `internal/httpapi/openapi_validate_test.go` | 응답 시작 뒤 패닉 처리, OpenAPI 3.1 검증 (Task 9) |
| `test/e2e/*_test.go` (빌드 태그 `e2e`) | E2E (Task 10) |
| `test/perf/*_test.go` (빌드 태그 `perf`), `docs/perf/2026-10-26-branching-engine.md` | 성능 측정과 결과 (Task 11) |
| 스펙·목차·이월 사항 | 문서 (Task 12) |

---

### Task 1: PostgreSQL·XFS 단위 테스트를 Podman 머신에서

**Files:**
- Create: `images/test/Dockerfile`, `hack/pg-test.sh`
- Delete: `hack/vm-setup.sh`, `hack/vm-test.sh`
- Modify: `Makefile`(`vm-setup`·`test-vm` 대신 `test-pg`, `test-xfs`는 새 스크립트로)

**Interfaces:**
- Produces: `hack/pg-test.sh [go test args...]` — 인자가 없으면 `./...`. `make test-pg`, `make test-xfs`(`-race ./internal/volume/... ./internal/testutil/xfstest/...`).
- 이미지 `consonance/test:dev`: `golang:1.27-bookworm` + PGDG의 `postgresql-17`, `xfsprogs`, `gcc`. 테스트는 root가 아닌 사용자로 돈다.
- 실행: Mac의 Podman 클라이언트로 rootless `podman run --rm --userns=keep-id`(머신 사용자와 같은 uid로 돈다; initdb는 root를 거부한다). 마운트는 저장소(Mac 경로 그대로, virtiofs), Go 캐시 볼륨 `consonance-gocache`, XFS 디렉터리(머신의 `/var/lib/consonance/test`를 `/xfs`로). 환경 변수 `CONSONANCE_TEST_PG_BIN=/usr/lib/postgresql/17/bin`, `CONSONANCE_TEST_XFS_DIR=/xfs/test`, `GOFLAGS=-buildvcs=false`.
- XFS 디렉터리는 스크립트가 `podman machine ssh 'sudo install -d -o $(id -u) /var/lib/consonance/test'`로 만든다(머신 사용자 소유, 이미 있으면 그대로).

- [ ] **Step 1:** `images/test/Dockerfile`, `hack/pg-test.sh`를 쓴다. 이미지가 없으면 스크립트가 만든다.
- [ ] **Step 2: 확인** — `make test-xfs`가 `internal/volume`의 XFS 테스트를 건너뛰지 않고 통과한다(`go test -v` 출력에 `SKIP`이 없다: `hack/pg-test.sh -v -run . ./internal/volume/ | grep -c -- '--- SKIP'` → `0`). `make test-pg`가 모든 패키지 `ok`(envtest 패키지는 `KUBEBUILDER_ASSETS`가 없어 건너뛰는 것이 맞다).
- [ ] **Step 3:** `hack/vm-*.sh`와 Makefile의 VM 대상을 지우고, `grep -rn 'vm-test\|vm-setup\|limactl' --include='*.sh' --include=Makefile .`이 아무것도 내지 않게 한다(`spikes/` 제외).
- [ ] **Step 4: 커밋** — `build: PostgreSQL·XFS 단위 테스트를 Podman 머신의 컨테이너에서 (Lima VM 대신)`

### Task 2: 개발 k3s 클러스터와 스토리지 클래스

**Files:**
- Create: `hack/dev-cluster.sh`, `config/dev/storageclass.yaml`
- Modify: `Makefile`(`dev-cluster`, `dev-cluster-down`), `.gitignore`(`bin/dev/`는 이미 `/bin/` 아래)

**Interfaces:**
- `hack/dev-cluster.sh up|down|kubeconfig`:
  - `up`: 머신에 `/var/lib/consonance/storage`를 만들고, 없으면 `sudo podman run -d --name consonance-k3s --privileged --security-opt label=disable --tmpfs /run --tmpfs /var/run -v /var/lib/consonance:/var/lib/consonance -p 6443:6443 rancher/k3s:v1.36.5-k3s1 server --disable traefik --default-local-storage-path /var/lib/consonance/storage --write-kubeconfig-mode 644`. 노드가 Ready가 될 때까지 기다린 뒤 `config/dev/storageclass.yaml`을 적용하고 kubeconfig를 `bin/dev/kubeconfig`에 쓴다(서버 주소는 Mac에서 닿는 `https://127.0.0.1:6443`). 이미 돌고 있으면 그대로 두고 kubeconfig만 다시 쓴다.
  - `down`: 컨테이너를 지운다. `/var/lib/consonance/storage`는 지우지 않는다(`--purge`일 때만).
  - 6443이 Mac에 닿지 않으면(Podman이 rootful 포트를 넘기지 않음) `kubeconfig`는 `podman machine ssh -L`로 터널을 여는 대신 `bin/dev/kubectl` 래퍼를 만든다: `podman machine ssh sudo podman exec -i consonance-k3s kubectl "$@"`. 이 판단은 Step 2에서 하고 보고서에 적는다.
- `config/dev/storageclass.yaml`: StorageClass `consonance-xfs`, `provisioner: rancher.io/local-path`, `volumeBindingMode: WaitForFirstConsumer`, `reclaimPolicy: Delete`.
- 이후 작업이 쓰는 것: `KUBECONFIG=bin/dev/kubeconfig`(또는 래퍼), `make dev-cluster`.

- [ ] **Step 1:** 스크립트와 스토리지 클래스를 쓴다.
- [ ] **Step 2: 확인(실제 클러스터)** — `make dev-cluster` 뒤 `kubectl get nodes`가 Ready. 시험 파드 하나(`config/dev/` 밖, 스크립트 안의 heredoc)가 `consonance-xfs` PVC를 붙여 `xfs_info`(또는 `stat -f -c %T`)가 `xfs`이고 `cp --reflink=always`가 성공한다. 끝나면 시험 파드와 PVC를 지운다.
- [ ] **Step 3: 재실행** — `make dev-cluster`를 한 번 더 돌려 오류 없이 끝나고 컨테이너 ID가 같다(Review Focus 1).
- [ ] **Step 4: 커밋** — `build: Podman 머신의 개발용 k3s와 스토리지 클래스 consonance-xfs`

### Task 3: 오퍼레이터·API 서버 이미지와 k3s로 옮기기

**Files:**
- Create: `images/operator/Dockerfile`, `images/apiserver/Dockerfile`, `hack/load-images.sh`, `test/image/binary-smoke.sh`
- Modify: `Makefile`(`images`가 넷 모두, `image-operator`, `image-apiserver`, `load-images`)

**Interfaces:**
- 두 Dockerfile: `golang:1.27-bookworm`에서 `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"` → `gcr.io/distroless/static-debian12:nonroot`, `USER 65532:65532`, `ENTRYPOINT ["/consonance-operator"]`·`["/consonance-apiserver"]`.
- `hack/load-images.sh`: 네 이미지를 `podman save`로 내보내 `consonance-k3s`의 containerd(`ctr -n k8s.io images import -`)로 넣는다. 매니페스트는 `imagePullPolicy: Never`로 쓴다(Task 7).
- `test/image/binary-smoke.sh`: 두 이미지를 `--help`로 돌려 종료 코드 0, `docker inspect`로 사용자가 `65532:65532`.

- [ ] **Step 1:** Dockerfile 둘과 스크립트를 쓴다.
- [ ] **Step 2: 확인** — `make images && test/image/binary-smoke.sh` 통과; `make load-images` 뒤 `podman machine ssh sudo podman exec consonance-k3s crictl images | grep consonance/`에 네 이미지.
- [ ] **Step 3: 커밋** — `build: 오퍼레이터·API 서버 이미지와 개발 k3s로 이미지 옮기기`

### Task 4: 오퍼레이터 권한 축소와 리더 선출 네임스페이스

**Files:**
- Modify: `internal/controller/project_controller.go`, `branch_controller.go`, `manager.go`(RBAC 표지), `cmd/operator/main.go`, `cmd/operator/main_test.go`, `Makefile`(generate가 namespaced Role도 같은 파일에), `config/rbac/role.yaml`(생성물)

**Interfaces:**
- ClusterRole `consonance-operator`에서 뺀다(이월 사항 목록 그대로): Secret·PVC의 `update`·`patch`·`delete`, 파드의 `update`·`patch`, Service의 `patch`, ResourceQuota·NetworkPolicy의 `patch`·`delete`, Branch의 `patch`·`delete`, `*/status`의 `get`·`patch`. 리스와 이벤트 규칙은 `namespace=consonance-system` 표지로 옮겨 Role `consonance-operator`가 된다(controller-gen이 같은 `role.yaml`에 두 문서로 쓴다).
- 남기는 것 중 코드가 실제로 쓰는지 다시 본다: Secret `create`(만들 때만), PVC `create`, ResourceQuota·NetworkPolicy는 `CreateOrUpdate`라 `update` 유지.
- `--leader-election-namespace`(기본 `consonance-system`) 플래그를 더하고 `LeaderElectionNamespace`로 넘긴다. `--leader-elect`일 때만 쓴다.
- Task 5가 더할 RoleBinding 권한(`rolebindings` `get;create;update`, ClusterRole `consonance-apiserver-project`에 대한 `bind`)은 Task 5가 넣는다.

- [ ] **Step 1: 실패하는 테스트** — `cmd/operator/main_test.go`가 `role.yaml`을 여러 문서로 읽게 바꾸고(ClusterRole은 클러스터에, Role은 `consonance-system` 네임스페이스에), 새 `TestOperatorRoleHasOnlyWhatItUses`가 ClusterRole 규칙에 위 목록의 동사가 없음을 단언한다. 실행해 실패 확인.
- [ ] **Step 2:** 표지를 고치고 `make generate`.
- [ ] **Step 3: 확인** — envtest로 `go test -count=1 ./cmd/operator/ ./internal/controller/` 통과. 기존 권한 테스트(파드 교체, 파이널라이저, Branch 생성, 소유 객체)가 줄인 권한으로도 통과해야 한다.
- [ ] **Step 4: 커밋** — `fix(operator): 쓰지 않는 권한을 빼고, 리스·이벤트는 consonance-system의 Role로, 리더 선출 네임스페이스 플래그`

### Task 5: API 서버의 Secret·네임스페이스 권한 축소

**Files:**
- Create: `internal/controller/apiserver_access.go`, `internal/controller/apiserver_access_test.go`, `config/rbac/apiserver/project-role.yaml`, `config/policy/namespace-delete.yaml`
- Modify: `internal/controller/project_controller.go`(Step 1에 RoleBinding), `internal/httpapi/rbac.go`(Secret `get` 제거), `cmd/apiserver/main_test.go`, `config/rbac/apiserver/role.yaml`(생성물)

**Interfaces:**
- 상수: `APIServerAccount = "consonance-apiserver"`, `APIServerProjectRole = "consonance-apiserver-project"`(`internal/controller/names.go`).
- `desiredAPIServerRoleBinding(ns string) *rbacv1.RoleBinding` — 이름 `consonance-apiserver`, `roleRef` ClusterRole `consonance-apiserver-project`, 주체 ServiceAccount `consonance-system/consonance-apiserver`, 라벨 `managedLabels("apiserver-access")`, Project가 소유.
- Project 조정 1단계에서 `CreateOrUpdate`로 둔다. 오퍼레이터 RBAC 표지에 `rbac.authorization.k8s.io` `rolebindings` `get;list;watch;create;update`와 `clusterroles` `bind`(`resourceNames=consonance-apiserver-project`). 매니저 캐시는 RoleBinding도 라벨로 거른다(`CacheOptions`).
- `config/rbac/apiserver/project-role.yaml`: ClusterRole `consonance-apiserver-project`, 규칙 하나 — `secrets` `get`, `resourceNames: [db-credentials, internal-token]`.
- `config/policy/namespace-delete.yaml`: `ValidatingAdmissionPolicy` `consonance-apiserver-namespace-delete`(대상: `namespaces` `DELETE`, 조건: 요청자가 `system:serviceaccount:consonance-system:consonance-apiserver`이면 `oldObject.metadata.labels['app.kubernetes.io/managed-by'] == 'consonance-apiserver'`) + `ValidatingAdmissionPolicyBinding`(`validationActions: [Deny]`).
- API 서버 ClusterRole에서 `secrets get`이 빠진다.

- [ ] **Step 1: 실패하는 테스트**
  - `TestProjectGivesTheAPIServerItsSecrets`(envtest, `project_controller_test.go` 방식): 프로젝트를 만들면 RoleBinding이 생기고 `roleRef`·주체가 위와 같다.
  - `cmd/apiserver/main_test.go`: 실행 파일 테스트가 `project-role.yaml`도 적용하고, 프로젝트 네임스페이스에 위 RoleBinding을 관리자로 만들어 둔다(오퍼레이터가 없으므로). 그래도 `GET .../connection`이 200이다. 새 `TestAPIServerCannotReadOtherSecrets`: 같은 사용자로 `kube-system`의 Secret을 읽으면 Forbidden.
  - 정책: envtest는 ValidatingAdmissionPolicy를 지원한다(1.37). `TestNamespaceDeletePolicy`: 정책을 적용한 뒤 API 서버 사용자로 라벨 없는 네임스페이스를 지우면 거부, 라벨 있는 것은 허용.
- [ ] **Step 2: 실패 확인**, **Step 3: 구현과 `make generate`**, **Step 4: 확인** — envtest로 `./internal/controller/ ./cmd/apiserver/ ./cmd/operator/` 통과.
- [ ] **Step 5: 커밋** — `fix(rbac): API 서버는 프로젝트마다 두 Secret만 읽고, 네임스페이스는 자기가 만든 것만 지운다`

### Task 6: 메트릭

**Files:**
- Create: `internal/controller/metrics.go`, `internal/controller/metrics_test.go`
- Modify: `project_controller.go`, `branch_controller.go`, `restore.go`

**Interfaces:**
- `metrics.go`: controller-runtime의 `metrics.Registry`에 등록하는 여섯 개.
  - `consonance_branch_create_seconds` (Histogram): Branch `creationTimestamp` → `status.timings.dataReadyAt`. main 제외(init-main은 프로젝트 생성에 든다).
  - `consonance_branch_first_ready_seconds` (Histogram): `creationTimestamp` → 처음 `runningAt`.
  - `consonance_restore_seconds` (Histogram, 라벨 `kind`=`rollback`|`reset`, `result`=`done`|`failed`): `lastRestore.startedAt` → `finishedAt`.
  - `consonance_start_seconds` (Histogram): 정지된 브랜치에 `/v1/start`를 보낸 때 → Running을 확인한 때(오퍼레이터가 잰다).
  - `consonance_project_create_seconds` (Histogram): Project `creationTimestamp` → 처음 phase Ready.
  - `consonance_copy_errors_total` (Counter, 라벨 `reason`): `CreateBranchData`·`CreateGeneration` 실패(에이전트 오류 코드, 코드 없으면 `transport`).
  - 버킷: `prometheus.ExponentialBuckets(0.05, 2, 12)`(50ms~102s).
- 각 관측은 상태가 처음 바뀌는 조정에서 한 번만 한다(이미 기록된 시각이 있으면 다시 재지 않는다 — 재시작 뒤 중복을 막는다).

- [ ] **Step 1: 실패하는 테스트**(envtest, `testutil.CollectAndCount`/`ToFloat64`): 브랜치 하나를 Running까지 → `branch_create`·`first_ready` 샘플 수가 1씩 늘고, 같은 브랜치를 다시 조정해도 그대로; 롤백 하나 → `restore_seconds{kind="rollback",result="done"}` 1; 복사 실패 주입 → `copy_errors_total{reason="rate-limited"}` 증가; 프로젝트 Ready → `project_create` 1.
- [ ] **Step 2–4:** 실패 확인, 구현, `-race` envtest 3회 통과.
- [ ] **Step 5: 커밋** — `feat(controller): 스펙 11장의 메트릭 여섯 개`

### Task 7: 개발 매니페스트와 배포

**Files:**
- Create: `config/default/kustomization.yaml`, `config/default/namespace.yaml`, `config/default/operator.yaml`, `config/default/apiserver.yaml`, `hack/deploy-dev.sh`
- Modify: `Makefile`(`deploy-dev`, `undeploy-dev`)

**Interfaces:**
- `kustomization.yaml`: `../crd/bases`, `../rbac/role.yaml`, `../rbac/apiserver/role.yaml`, `../rbac/apiserver/project-role.yaml`, `../policy/namespace-delete.yaml`, 네임스페이스, 서비스 계정 둘과 바인딩(ClusterRoleBinding 둘, Role 바인딩 하나), 두 Deployment, API 서버 Service.
- `namespace.yaml`: `consonance-system`, 라벨 `pod-security.kubernetes.io/enforce: restricted`.
- 오퍼레이터 Deployment: 복제본 1, `--agent-image=consonance/agent:dev --postgres-image=consonance/postgres:17-dev`, `imagePullPolicy: Never`, 프로브 `/healthz`·`/readyz`(:8081), 메트릭 포트 8080, 자원 100m/128Mi, 보안 문맥 restricted(`runAsNonRoot`, `allowPrivilegeEscalation: false`, capabilities 전부 제거, seccomp `RuntimeDefault`, `readOnlyRootFilesystem: true`), `terminationGracePeriodSeconds: 40`.
- API 서버 Deployment: 복제본 2, `--admin-key-file=/etc/consonance/admin/key`(Secret `consonance-admin-key` 마운트), `--storage-class=consonance-xfs`, 포트 8080, 프로브 `GET /healthz`, `terminationGracePeriodSeconds: 45`, `lifecycle.preStop.sleep.seconds: 5`(이월 사항의 종료 지연), 같은 보안 문맥. Service `consonance-apiserver`(ClusterIP 8080).
- `hack/deploy-dev.sh`: `bin/dev/admin-key`가 없으면 32바이트 무작위 키(base64url 43자)를 만들어 `umask 077`로 쓰고, Secret을 `kubectl create secret generic ... --dry-run=client -o yaml | kubectl apply -f -`로 둔다(키를 출력하지 않는다). 그다음 `kubectl apply -k config/default`, 두 Deployment가 Available이 될 때까지 기다린다.

- [ ] **Step 1:** 매니페스트와 스크립트를 쓴다.
- [ ] **Step 2: 확인(실제 클러스터)** — `make dev-cluster load-images deploy-dev` 뒤 두 Deployment Available; `kubectl -n consonance-system get pods -o jsonpath` 의 모든 컨테이너가 `runAsNonRoot`; API 서버에 `kubectl port-forward`로 `GET /healthz` 200, 키 없는 `GET /v1/projects` 401, 키를 넣은 `GET /v1/projects` 200.
- [ ] **Step 3: 재실행** — `make deploy-dev`를 다시 돌려 오류 없이 끝나고 키 파일이 바뀌지 않는다(Review Focus 1). 스크립트 출력(`make deploy-dev 2>&1`)에 키 문자열이 없다(Review Focus 5).
- [ ] **Step 4: 커밋** — `build: 개발용 kustomize 매니페스트와 배포 스크립트`

### Task 8: requestId를 에이전트와 감독 프로세스로

**Files:**
- Modify: `internal/agentclient/client.go`, `internal/supervisorclient/client.go`, `internal/agent/server.go`, `internal/supervisor/server.go`, `internal/controller/restore.go`, 각 `_test.go`

**Interfaces:**
- 클라이언트: `func WithRequestID(ctx context.Context, id string) context.Context`(두 패키지 공통 아이디어를 각 패키지에 하나씩; 또는 `internal/agentapi`에 `RequestIDHeader = "X-Request-Id"`와 함께 두고 둘이 쓴다 — 후자로 한다: `agentapi.WithRequestID`, `agentapi.RequestIDFrom`). 클라이언트는 ctx에 ID가 있으면 헤더 `X-Request-Id`를 단다.
- 서버: 에이전트와 감독 프로세스의 요청 로그·오류 로그에 `requestId` 필드(헤더 값, 64자 이하 `[A-Za-z0-9_-]`만, 아니면 빈 값).
- 오퍼레이터: `restore()`가 ctx에 `spec.restore.requestId`를 넣어 단계 호출에 넘긴다.

- [ ] **Step 1: 실패하는 테스트** — `agentclient`: `httptest` 서버가 받은 헤더를 기록, `WithRequestID(ctx,"r1")`로 부르면 `X-Request-Id: r1`; 없으면 헤더 없음. `supervisorclient` 같은 것. 에이전트·감독 프로세스 서버: 헤더를 보낸 요청의 로그 줄에 `"requestId":"r1"`. 오퍼레이터(가짜 커넥터가 ctx의 ID를 기록하게): 롤백 중 `CreateGeneration`·`Stop`·`ActivateGeneration`·`Start`·`DetachGeneration`이 그 요청의 ID를 받는다.
- [ ] **Step 2–4:** 실패 확인, 구현, `go test -race`(호스트)·envtest·`make test-pg` 통과.
- [ ] **Step 5: 커밋** — `feat: 롤백·리셋의 requestId를 에이전트와 감독 프로세스 로그까지`

### Task 9: API 서버 마무리 — 응답 뒤 패닉, OpenAPI 3.1 검증

**Files:**
- Modify: `internal/httpapi/server.go`, `internal/httpapi/server_test.go`, `go.mod`, `go.sum`
- Create: `internal/httpapi/openapi_validate_test.go`

**Interfaces:**
- `recovered`: 응답이 이미 시작됐으면(`statusWriter.status != 0`) problem을 쓰지 않고 요청 로그 줄을 Error 수준으로만 남긴다(계획 4 최종 리뷰의 미뤄 둔 사항).
- `TestOpenAPIIsValid31`: `github.com/pb33f/libopenapi`로 `openapi.json`을 읽어 `NewDocument` → `BuildV3Model()`이 오류 없이 끝나고 버전이 3.1이다. 검증 오류가 있으면 문서를 고친다.

- [ ] **Step 1: 실패하는 테스트** — `TestPanicAfterTheResponseStartedIsOnlyLogged`(핸들러가 200을 쓰고 패닉 → 응답 본문에 problem이 붙지 않고, 로그 한 줄이 Error·`panic`). `TestOpenAPIIsValid31`.
- [ ] **Step 2–4:** 실패 확인, 구현(검증이 찾은 문서 오류도 고친다), `go test -race ./internal/httpapi/` 통과.
- [ ] **Step 5: 커밋** — `fix(httpapi): 응답을 시작한 뒤의 패닉은 로그만, OpenAPI 3.1 검증 테스트`

### Task 10: E2E

**Files:**
- Create: `test/e2e/main_test.go`, `test/e2e/api_test.go`(API 클라이언트·port-forward 도우미), `test/e2e/scenario_test.go`, `test/e2e/cluster_test.go`
- Modify: `Makefile`(`e2e`)

**Interfaces:**
- 빌드 태그 `//go:build e2e`. `make e2e` = `go test -tags e2e -count=1 -timeout 30m -v ./test/e2e/`. 환경: `KUBECONFIG=bin/dev/kubeconfig`, 관리자 키 `bin/dev/admin-key`. 클러스터가 없으면 `t.Skip`이 아니라 실패(명시적으로 돌리는 테스트다).
- 도우미: `portForward(t, ns, target string, port int) (localPort int)`(kubectl 하위 프로세스, `t.Cleanup`에서 끝냄), `api(t) *client`(요청 ID를 매번 새로), `createProject(t) (id string)` — `t.Cleanup`에서 프로젝트를 지우고 네임스페이스가 사라질 때까지 기다린다(Review Focus 2), `sql(t, conn connectionView) *pgx.Conn`(브랜치 Service로 port-forward).
- `TestScenario`(스펙 12장 그대로): 프로젝트 생성(Ready까지 폴링, 최대 5분) → 브랜치 `dev` 생성(201 또는 202 뒤 Running까지) → `dev`에 테이블 만들고 행 넣기 → main에 그 테이블이 없음 → `dev` 복구 지점 → `DROP TABLE` → 롤백(200, `backupBranch` 있음) → 행이 돌아옴, 백업 브랜치 존재 → 정지(200, phase Stopped) → 기동(200) → 행이 그대로 → `dev` 삭제(202 뒤 사라짐) → 프로젝트 삭제(202 뒤 네임스페이스 사라짐).
- `TestICUCollation`: main에서 `SELECT collname FROM pg_collation WHERE collprovider='i' AND collname IN ('ko-x-icu','und-x-icu')` → 둘 다 있음; `ORDER BY w COLLATE "und-x-icu"`가 `apple éclair zebra 가방 나비`.
- `TestPodsAreRestricted`(Review Focus 3): 프로젝트 네임스페이스에 `enforce: restricted` 라벨, 모든 파드가 떠 있음(거부되지 않음).
- `TestNetworkPolicies`: 에이전트·브랜치 파드가 Ready(프로브가 막히지 않음), 에이전트에서 `getent hosts kubernetes.default.svc` 성공(DNS 이그레스), 다른 프로젝트 네임스페이스의 파드에서 브랜치 5432로 연결이 안 됨(`kubectl run` 일회성 파드, `restricted` 보안 문맥).
- `TestE2ELeavesNothingBehind`: 위 테스트들이 끝난 뒤(`TestMain`의 마지막) `cns-` 네임스페이스가 하나도 없다.

- [ ] **Step 1:** 도우미와 테스트를 쓴다.
- [ ] **Step 2: 확인(실제 클러스터)** — `make dev-cluster images load-images deploy-dev e2e`가 통과. 실패하면 원인을 고친다: 코드 결함이면 해당 패키지를 고치고 그 패키지의 단위 테스트를 더한 뒤 같은 커밋에 넣는다(보고서에 따로 적는다).
- [ ] **Step 3: 커밋** — `test(e2e): 개발 k3s에서 스펙 12장 시나리오, ICU 정렬, PSS, 네트워크 정책`

### Task 11: 성능 측정

**Files:**
- Create: `test/perf/perf_test.go`(빌드 태그 `perf`), `docs/perf/2026-10-26-branching-engine.md`
- Modify: `Makefile`(`perf`)

**Interfaces:**
- `make perf` = `go test -tags perf -count=1 -timeout 2h -v ./test/perf/`, Task 10의 도우미를 같은 방식으로(복사하지 말고 `test/e2e`의 도우미를 `test/internal/e2eutil` 패키지로 옮겨 둘이 쓴다 — 이 작업에서 옮긴다).
- 측정(각 20회 + 워밍업 1회 버림, Review Focus 4), 부하 없음과 쓰기 부하 두 조건:
  - 브랜치 생성 API 응답 시간(201까지) → P50 ≤ 1초.
  - 새 브랜치 첫 쿼리: 201 받은 때 → `SELECT 1` 성공까지 → P95 ≤ 3초.
  - 롤백: 요청 → 200 → 최대 ≤ 10초(데이터 1GB, `pgbench -i -s 70`).
  - 정지 → 실행: 기동 요청 → 200 → P50 ≤ 1초.
  - 프로젝트 생성: 요청 → Ready(기준 없음, 기록만; init-main 포함).
- 쓰기 부하: main에서 `pgbench -c 4 -T <측정 시간> -N`을 클러스터 안 일회성 파드(`consonance/postgres:17-dev`, `restricted`)로 돌린다.
- 결과: 테스트가 `docs/perf/2026-10-26-branching-engine.md`에 표(조건×지표: P50, P95, 최대, 기준, 통과)를 쓰고, 기준을 넘으면 실패한다. 측정 환경(머신 CPU·메모리, 커널, k3s 버전, 스토리지)을 같이 적는다.

- [ ] **Step 1:** 도우미 이동과 측정 테스트를 쓴다.
- [ ] **Step 2: 확인** — `make perf` 실행, 결과 문서가 생기고 네 기준이 통과. 기준을 넘으면 원인을 조사해 보고서에 적는다(코드 결함이면 고친다; 환경 한계면 숫자와 함께 그대로 적고 BLOCKED가 아니라 DONE_WITH_CONCERNS).
- [ ] **Step 3: 커밋** — `test(perf): 스펙 15장 성능 측정과 결과`

### Task 12: 문서

**Files:**
- Modify: 스펙, 목차, 이월 사항, `README.md`가 있으면 개발 환경 절(없으면 만들지 않는다)

- [ ] **Step 1: 스펙** — 12장 E2E 환경을 "Podman 머신의 k3s 컨테이너(`hack/dev-cluster.sh`)"로, 13장 Make 대상을 실제 목록(`test`, `test-pg`, `test-xfs`, `test-envtest`, `generate`, `images`, `load-images`, `dev-cluster`, `deploy-dev`, `e2e`, `perf`)으로, 7.6에 오퍼레이터·API 서버 이미지가 distroless static nonroot라는 것을, 11장에 메트릭은 오퍼레이터가 낸다는 것과 requestId 전달을, 8장에 API 서버의 Secret 접근이 프로젝트별 RoleBinding이라는 것과 네임스페이스 삭제 정책을 더한다. 15장 아래에 성능 결과 문서 링크.
- [ ] **Step 2: 목차** — 계획 5 행을 링크와 `구현 완료`로.
- [ ] **Step 3: 이월 사항** — 계획 3·4 아래의 "계획 5가 반영할 것" 두 절에서 이 계획이 한 것을 지우고, 남은 것(결정 7)을 파일 끝 `## 계획 5(배포·E2E·성능)에서` 아래 `### 다음 서브프로젝트가 반영할 것`으로 옮긴다. 인그레스·TLS는 연결 게이트웨이로. 실행 끝에 컨트롤러가 `### 미뤄 둔 개선 사항`을 더한다.
- [ ] **Step 4: 확인** — `grep -c '계획 5(배포·E2E·성능)가 반영할 것' docs/superpowers/plans/2026-10-07-branching-engine-carryover.md` → `0`.
- [ ] **Step 5: 커밋** — `docs: 스펙·목차·이월 사항에 계획 5의 결정과 결과 반영`
