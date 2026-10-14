# 브랜칭 엔진 이월 사항

앞 계획의 구현·리뷰에서 나온 것 중 뒤 계획이 반영해야 할 사항과, 미뤄 둔 개선 사항을 모은다. 뒤 계획을 쓰기 전에 먼저 읽는다. 반영한 항목은 지운다.

## 계획 1(감독 프로세스)에서

### 미뤄 둔 개선 사항

pgctl 프로세스 관리:

- postmaster가 요청 없이 죽으면 남은 백엔드가 고아가 된다. 프로세스 그룹 전체를 종료한다.
- `awaitReady`가 20ms마다 접속을 시도해 기동·복구 중에 "starting up" FATAL 로그가 초당 약 50줄 남는다. `postmaster.pid`의 상태 줄을 읽는 방식(pg_ctl과 같음)으로 바꾼다.
- 종료 코드 0으로 끝난 postmaster에 "exited during startup: <nil>"을 낸다. 종료 오류를 기동마다 따로 두고 `%w`로 감싼다.
- `LocalConnString`이 소켓 경로를 인용하지 않는다. 정지 중에 다시 `Stop`을 부르면 `since`가 바뀐다.
- `currentStatus`가 상태와 세대를 서로 다른 잠금으로 읽어, 전환 순간에 둘이 어긋날 수 있다.
- 테스트: SIGQUIT 격상, `Start`의 ctx 만료, 실행 파일 기동 실패를 고정 테스트로 둔다. `TestStopWhileStarting`의 PID 대기 루프에 기한이 없고, 그 SIGINT는 postmaster가 핸들러를 설치하기 전에 도착한다.

pgctl 백업:

- `Begin`과 `End`가 접속과 즉시 체크포인트 동안 잠금을 잡고 있어 `InProgress`와 `Abort`가 기다린다.
- 시간 초과로 취소된 백업과 시작하지 않은 백업이 같은 `ErrNoBackup`을 돌려준다(지금은 취소 로그만 남긴다).
- `pg_backup_start` 실패, `End` 실패, 명시적 `Abort` 경로를 테스트하지 않는다. `Begin`이 타이머를 `expire(conn)`에 묶는지 고정하는 테스트도 없다.
- `TestBackupTimeoutAbortsSession`은 1초 제한이라 부하가 크면 흔들릴 수 있다. 흔들리면 3~5초로 늘린다.

pgctl 부트스트랩:

- 빈 역할·DB·비밀번호를 거부하지 않는다(실행 파일은 빈 비밀번호 파일을 거부한다).
- `log_statement`와 `log_min_error_statement`만 끈다. auto_explain이나 pgaudit 같은 다른 로깅 설정이 켜지면 문장이 남을 수 있다. SCRAM 검증자를 보내는 방식을 검토한다.
- 비밀번호 설정과 유지, 특수 문자가 든 식별자 인용, 역할만 있고 DB가 없는 상태에서의 복구를 테스트하지 않는다.

감독 프로세스 API와 클라이언트:

- 401은 `GET /v1/status`에서만 테스트한다. `/v1` 경로 전체와 빈 토큰을 표 테스트로 확인한다.
- `StopGrace` 전달과 `PGState` 변환을 테스트하지 않는다. `/v1/backup/start` 본문 크기를 제한하지 않는다.
- 클라이언트: JSON이 아닌 오류 본문은 `APIError.Message`가 비고, 2xx 디코드 오류에 메서드와 경로가 없다. `baseURL` 끝의 `/`를 잘라내지 않아 `//v1/...` 요청이 307 리다이렉트로 한 번 더 왕복한다(Go 1.25로 빌드한 서버는 301을 줘서 POST가 405가 됐다). `Bootstrap`, `APIError.Message`, `StopLSN`을 테스트하지 않는다. 기본 제한 75초는 서버의 기동 제한 60초에 맞춘 상수다.

실행 파일:

- `pg.Stop`과 `httpSrv.Shutdown` 사이에 온 `/v1/start`가 종료 직전에 PostgreSQL을 띄울 수 있다. `Shutdown` 뒤에 `pg.Stop`을 한 번 더 부른다.
- 종료 로그에 신호 이름이 없다. `--help`가 종료 코드 1로 끝난다. `httpSrv.Serve` 오류를 버린다.
- 통합 테스트: `-race` 없이 빌드하고, `cmd.Wait`에 기한이 없고, 실패하면 VM에 postmaster가 남을 수 있고, 부트스트랩은 오류가 없는지만 본다.

이미지·스크립트·테스트 도우미:

- 스모크 테스트 정리(`docker rm -f`)에 `-v`가 없어 실행할 때마다 기반 이미지의 익명 볼륨이 남는다. 종료 코드 0과 "fast shutdown" 로그를 확인하지 않고, `grep -q`와 pipefail이 SIGPIPE로 흔들릴 수 있고, 포트를 모든 인터페이스에 열고, `--pull=never`가 없다.
- `.dockerignore`가 `.superpowers`, `test`, `hack`, `images`를 빼지 않아 빌드 캐시가 자주 깨진다. 출시 전에 기반 이미지 태그를 다이제스트로 고정한다.
- `hack/vm-test.sh`에 패키지 패턴 없이 플래그만 주면 `.`만 테스트한다. `hack/vm-setup.sh`는 `limactl start` 실패를 `|| true`로 숨기고, `NEEDRESTART_SUSPEND=1`이 없고, Go 압축 파일의 SHA-256을 확인하지 않는다.
- `TestFreePortIsUsable`은 포트 범위만 확인한다. `SocketDir` 테스트가 없고, `InitDB` 테스트는 `PG_VERSION`이 17인지 보지 않는다. `SharedBuffers` 표에 음수, 정확한 경계, 내림 사례가 없다. `Args`는 입력을 검증하지 않는다.

## 계획 2(볼륨 에이전트)에서

### 계획 4(REST API 서버)가 반영할 것

- main 리셋은 공개 API가 막는다(스펙 5장 409 `conflict`). 에이전트는 main 삭제만 거부하고, main의 `ParentLatest` 세대 생성(리셋)은 받아들인다.
- `agentclient` 사용 시 주의점은 계획 3 항목과 같다(`agent.Ops`를 구현하지 않음, 리다이렉트를 따라가지 않음, `Code`가 빈 `APIError`).
- `DeleteRestorePoint`를 다시 보내면 404다(지운 뒤에는 없는 복구 지점이다). 공개 API에서 삭제를 재시도할 때 404를 어떻게 다룰지 정한다.
- 브랜치 데이터가 생기기 전에는 `ListRestorePoints`가 404다.
- 복구 지점 라벨은 256바이트까지다(한글 약 85자). 스펙에는 한도가 없으므로 API 문서에 적는다.

### 미뤄 둔 개선 사항

볼륨 레이아웃·기동 시 정리:

- `current`가 없으면 `Recover`가 완성된 세대까지 브랜치째 휴지통으로 옮긴다(에이전트 밖에서 링크를 지우면 데이터를 잃는다). 완성된 세대가 하나도 남지 않을 때만 브랜치를 지우게 하고, InitMain은 표시한 임시 디렉터리에 initdb한다.
- `generations()`·`nextGeneration()`은 브랜치가 없을 때 PathError를 그대로 돌려준다(삭제와 겹친 `ListGenerations`가 404가 아니라 500). `generations()`는 상태를 확인하지 못한 세대를 목록에서 조용히 뺀다. Activate·Detach·DeleteGeneration은 Lstat·`complete()` 때문에 EACCES·EIO를 404로 보고하고(Detach의 거짓 404는 오퍼레이터가 백업 브랜치를 건너뛰게 한다), `ListRestorePoints`는 `os.Stat` 오류를 모두 404로 바꾼다. `stateOf`를 쓴다.
- `DetachGeneration`의 toBranch 재시도 지름길은 원본 브랜치가 없어도 성공한다. 먼저 `currentGeneration(branch)`를 확인한다.
- `acquire`가 돌려주는 release는 두 번 부르면 막힌다(`sync.Once`). `trash`는 rename 양쪽의 ENOENT를 모두 "이미 없음"으로 본다. `makeMarked`는 `os.Remove` 오류를 버린다.
- `trash`가 rename한 뒤 sync에 실패하면(500) 재시도에서는 경로가 이미 없어 sync 없이 성공한다. 그래서 다시 보낸 `DeleteBranchData`가 아직 내구화되지 않은 이동 위에서 204를 줄 수 있다(fsync가 실패하고도 파일 시스템이 살아 있어야 해서 드묾).
- `CloneTree("")`는 작업 디렉터리를 복사한다. 빈 src를 `ErrInvalid`로 거부한다.
- 문서: `CloneTree`는 dst가 비어 있어야 하고 실패하면 일부가 남는다. `switchCurrent`는 작업 잠금 아래에서만 부르고, rename이 실패하면 `current.tmp`가 남는다. `finishMarked`는 파일 데이터가 먼저 내구화돼 있어야 한다. `Recover`의 "건드리지 않는다"는 문자 그대로가 아니다. pgctl의 `ErrNoDataDir` 주석은 디렉터리로 이어지는지 본다고 하지만 `EvalSymlinks`만 확인하고, `Start` 문서에 이 오류가 없다.

복사·복구 지점·카탈로그:

- `backup_label`·`tablespace_map`을 복사에서 빼지 않고, 라벨을 `O_CREATE|O_TRUNC`로 써서 원본에 심은 같은 이름의 심볼릭 링크를 따라간다(테넌트 슈퍼유저가 필요하고 스펙 4.5가 막는다). pg_basebackup처럼 두 이름을 빼고 `O_EXCL`로 쓴다.
- 카탈로그 항목의 `path`를 믿고 이어 붙인다(`../../x`면 볼륨 밖). (branch, id)에서 경로를 만들고, 읽을 때 항목을 검사한다.
- 백업 시작의 409를 모두 `busy`로 보지만 감독 프로세스는 "실행 중 아님"에도 409를 준다. 409면 상태를 다시 확인한다. 보고된 세대 이름을 해석하지 못하면 재시작(409)으로 본다.
- `DeleteBranchData`는 취소된 요청이나 5초 상태 시간 초과도 "파드 없음"으로 본다. 상태 확인 뒤 `ctx.Err()`를 확인한다.
- `addRestorePoint`는 "오류면 카탈로그에 없다"고 보지만, `saveCatalog`는 rename 뒤의 디렉터리 sync에서 실패할 수도 있다(드묾).
- preflight는 FICLONE의 EOPNOTSUPP·EXDEV·ENOTTY·EINVAL만 미지원으로 본다. 다른 errno(ENOSYS·EPERM 등)는 500이 되어 오퍼레이터가 계속 다시 확인한다. 복제본이 원본과 다른 경우(`errReflinkMismatch`)는 시험하지 않는다.

API·클라이언트·실행 파일:

- 인증은 경로마다 `s.auth`로 감싸야 해서 빠뜨리면 조용히 공개된다. `/v1` 전체를 감싸고, 맞는 경로·메서드가 없을 때도 `{error, code}`로 답한다. 400·401 로그 줄에 원인이 없다.
- 클라이언트: 경로 조각을 모두 `branchPath` 안에서 이스케이프하고, 이름을 보내기 전에 검사한다. `Retry-After`는 429에서만 읽고 음수를 버린다. `New`가 `hc`를 복사한다는 점을 문서에 적는다.
- `--supervisor-url` 끝에 `/`가 있으면 감독 프로세스 호출마다 307 리다이렉트로 한 번 더 왕복한다(Go 1.25에서는 POST가 405가 되어 복사가 모두 실패했다). `supervisorclient.New`에서 잘라낸다(계획 1 항목과 같다).
- 종료 시간이 둘로 나뉜다(요청 25초, 그 뒤 ctx 없는 휴지통 비우기). 신호를 받으면 `stop()`·`stopJanitor()`를 부르고 정리 작업도 같은 25초 안에서 기다린다. 두 번째 신호를 삼키고, `Shutdown` 오류를 감싸지 않는다.

테스트:

- cmd/agent: 25초 drain(`Shutdown`을 `Close`로 바꿔도 통과한다), `{branch}`·토큰 연결, `--project-id`, 플래그 검사를 시험하지 않는다. 응답을 붙잡는 가짜 감독 프로세스 하나로 넷 다 시험할 수 있다. `go test -race`는 에이전트 실행 파일을 계측하지 않는다.
- volume: 실행 중인 세대를 복사하는지(`current`가 아니라. 호스트에서도 가능), 507 경계, 백업 시작 전에 거부된 복사(정지·응답 없음)는 간격에 넣지 않는지, 만들 때·정리 작업 때의 보존 정책, 잘못된 ID와 256바이트 라벨, 리셋 복구 지점의 `reason == reset` 조건과 내용, 다른 브랜치 데이터가 남는지, 표시된 세대의 Activate 거부, 취소된 initdb와 동시 InitMain, `stateOf`의 표시 확인 오류와 `%w`, `generations()` 정렬(data-2·data-10), 두 단계 chmod·사라지는 심볼릭 링크·src 오류, 카탈로그의 빈 저장·나이·개수·경계·깨진 파일, ID를 10번 뽑는 제한을 시험하지 않는다.
- 백업 시작이 실패한 경로와 지연된 정지 경로에서 `stopBackup`의 `WithoutCancel`을 취소된 요청으로 시험하지 않는다(두 번째 상태 호출에서 요청을 취소하면 `ErrConflict`, 시작·정지 1/1, 백업이 닫혀 있어야 한다). 백업 시작 도중 요청이 취소되는 경우도 시간 초과 쪽만 시험한다.
- 여러 테스트(`TestFailedCopyEndsBackupAndCleansUp`, reflink 없는 preflight 등)가 TMPDIR에서는 reflink가 안 된다고 가정한다. 시험 복제가 되면 건너뛴다. pgEnv 도우미는 접속을 테스트가 끝날 때까지 연다(max_connections 20).
- 에이전트 API: 성공 본문·Content-Type, 틀린 토큰, `Retry-After` 하한, 5xx를 Error 수준으로 남기는 규칙을 시험하지 않는다. 클라이언트: 2xx 디코드 실패, ctx 취소, 기본 제한 60초를 시험하지 않는다.
- pgctl: `Start`의 "postgres exited during startup" 경로(종료 코드 1을 내는 가짜 실행 파일)와 `ErrNoDataDir`→500 처리기를 시험하지 않는다. `resolveDir`가 `EvalSymlinks` 오류를 버려 EACCES·ELOOP·ENOTDIR가 모두 "does not resolve"가 된다.

VM·이미지:

- `make test-xfs`에 `-count=1`이 없어 XFS 마운트를 잃어도 캐시된 ok가 나올 수 있다. `xfstest`의 확인은 reflink를 보지 않고, `Dir`의 건너뛰기·정리 계약에 테스트가 없고, `RemoveAll` 오류를 버린다. `vm-setup.sh`는 이미지 파일이 있는지만 보고 그 안에 파일 시스템이 있는지는 보지 않는다.
- 스모크 테스트에 CPU 한도(100m)가 없다. curl이 시간 초과로 끝나면 `docker logs`·OOMKilled를 보이기 전에 스크립트가 끝난다. colima에 buildx가 없어 빌드할 때마다 약 1.4GB의 이름 없는 빌드 단계 이미지가 남는다.

## 계획 3(CRD·오퍼레이터)에서

### 계획 4(REST API 서버)가 반영할 것

- 프로젝트 생성은 네임스페이스 `cns-<projectId>`와 그 안의 Project `project`를 만드는 것이다. spec에서 비운 값은 CRD 기본값(스펙 14장)이 채운다. 진행은 `status.phase`와 조건(`VolumeReady`, `AgentReady`, `ReflinkSupported`, `MainReady`, 이를 요약한 `Ready`)으로 읽는다.
- 브랜치 생성 전 검사(부모 실행 중, `maxBranches`, 이름 형식)는 API가 한다. 오퍼레이터는 브랜치 수를 세지 않고, 부모가 실행 중이 아니면 데이터 만들기를 기다린다(`DataReady=False`, 이유 `ParentNotRunning`). 롤백이 만든 백업 브랜치(라벨 `consonance.io/backup-of`)도 Branch다. 개수에 넣을지 정한다.
- 브랜치의 영구 실패는 phase `Failed`와 조건 `DataReady=False`의 이유(`ReflinkUnsupported`·`InsufficientStorage`·`SourceNotFound`·`Invalid`)로 드러난다. 기동 실패는 phase `Failed`, 조건 `Ready=False`(이유 `StartFailed`)이고 오퍼레이터가 계속 다시 시도한다.
- 롤백·리셋은 `spec.restore`에 새 `requestId`와 출처를 쓰고, `status.lastRestore.requestId`가 그 값이 되고 `phase`가 `Done`이나 `Failed`가 될 때까지 기다린다. 응답의 `backupBranch`는 `lastRestore.backupBranch`, 리셋이 만든 복구 지점은 `lastRestore.restorePointId`다. 진행 중인 요청이 끝나기 전에 쓴 새 `requestId`는 끝난 뒤에 수행된다. 정지된 브랜치도 새 데이터가 기동하는지 확인하려고 잠시 기동했다가 다시 정지한다.
- `spec.restore`는 요청 하나만 담는다. 진행 중인 요청이 끝나기 전에 두 번 쓰면 그사이에 쓴 요청은 수행되지 않는다. API는 진행 중인 요청이 있으면 새 요청을 409로 막는다. main 리셋도 409로 막는다(오퍼레이터는 에이전트를 부르기 전에 Failed로 끝낸다).
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
- 오퍼레이터는 SIGTERM을 받으면 진행 중인 조정의 컨텍스트를 취소하고 끝나기를 기다린다(최대 30초). 끊긴 단계는 재시작 뒤 status에서 이어서 한다.
- 매니페스트를 내기 전에 ClusterRole에서 쓰지 않는 권한을 줄인다. 대상은 Secret·PVC의 update·patch·delete, 파드의 update·patch, Service의 patch, 쿼터·네트워크 정책의 patch·delete, Branch의 patch·delete, status의 get·patch, 리스의 list·watch·patch·delete다. 리스와 이벤트 권한은 `consonance-system`의 Role로 옮긴다. 줄인 뒤 `cmd/operator`의 테스트를 돌린다. 그 테스트는 오퍼레이터 역할로 각 경로를 확인한다. 클러스터 밖에서 리더 선출을 켜려면 `--leader-election-namespace` 플래그가 필요하다.
