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
- 통합 테스트: `-race` 없이 빌드하고, `cmd.Wait`에 기한이 없고, 실패하면 postmaster가 남을 수 있고, 부트스트랩은 오류가 없는지만 본다.

이미지·스크립트·테스트 도우미:

- 스모크 테스트 정리(`docker rm -f`)에 `-v`가 없어 실행할 때마다 기반 이미지의 익명 볼륨이 남는다. 종료 코드 0과 "fast shutdown" 로그를 확인하지 않고, `grep -q`와 pipefail이 SIGPIPE로 흔들릴 수 있고, 포트를 모든 인터페이스에 열고, `--pull=never`가 없다.
- `.dockerignore`가 `test`, `hack`, `images`를 빼지 않아 빌드 캐시가 자주 깨진다. 출시 전에 기반 이미지 태그를 다이제스트로 고정한다.
- `TestFreePortIsUsable`은 포트 범위만 확인한다. `SocketDir` 테스트가 없고, `InitDB` 테스트는 `PG_VERSION`이 17인지 보지 않는다. `SharedBuffers` 표에 음수, 정확한 경계, 내림 사례가 없다. `Args`는 입력을 검증하지 않는다.

## 계획 2(볼륨 에이전트)에서

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

이미지·테스트 도우미:

- `xfstest`의 확인은 reflink를 보지 않고, `Dir`의 건너뛰기·정리 계약에 테스트가 없고, `RemoveAll` 오류를 버린다.
- 스모크 테스트에 CPU 한도(100m)가 없다. curl이 시간 초과로 끝나면 `docker logs`·OOMKilled를 보이기 전에 스크립트가 끝난다.

## 계획 3(CRD·오퍼레이터)에서

### 미뤄 둔 개선 사항

조정·상태:

- 롤백·리셋의 단계 저장(`saveStatus`) 충돌과 에이전트 `busy`는 아직 ERROR 수준 로그로 남는다(조정 끝의 status 충돌은 오류로 보지 않게 고쳤다).
- Project 감시와 `For(Branch)`에 조건자가 없다. 1분마다 바뀌는 사용량을 Project status에 쓰면 그 네임스페이스의 모든 브랜치가 깨어 감독 프로세스 상태를 묻는다.
- 응답 없는 에이전트(preflight·사용량)는 2초 간격으로 다시 묻는다(스펙 10장은 지수 백오프). 사용량을 한 번 못 받으면 Ready 프로젝트가 Provisioning으로 바뀐다.
- `ObservedGeneration`은 서비스·파드 단계가 실패해도 올라간다. 파드를 다시 만드는 동안과 정지하는 동안에도 phase가 Running으로 남고, 롤백·리셋 중에도 `Ready`가 참으로 남는다.
- 파드 삭제에 UID 전제 조건이 없고, 파드가 사라졌는지는 캐시로 본다(만든 직후에 지우면 NotFound가 증거가 아니다). 삭제가 멈춰 있을 때 조건·메시지가 없고, 건너뛴 데이터 삭제를 로그에 남기지 않는다. 파이널라이저를 뗀 뒤의 갱신이 오래된 캐시로 NotFound를 한 번 남긴다.
- 복사가 끝난 뒤 status 쓰기를 잃으면 `BackupStartLSN`이 빈 값으로 남는다. 시작·정지 로그에 `op`가 없다(스펙 11장).

롤백·리셋:

- `lastRestore.phase`가 비었거나 모르는 값이면 단계 함수가 없어 패닉을 되풀이하고(PostgreSQL이 멈춘 채 남을 수 있다), `restoreDetach`는 `StartedAt`·`BackupBranch`를 확인 없이 쓴다. 손으로 고친 status에서만 생긴다. 단계에 enum을 두고 모르는 단계는 Failed로 끝낸다.
- 기동이 409·503·500이 아닌 오류로 실패해도 되돌린다. 세대 불일치의 409는 끝없이 기다린다.
- Detaching의 409를 모두 "이름이 있다"로 본다(에이전트는 현재 세대·실행 중인 세대에도 409를 준다). 그러면 1분쯤 뒤 Failed로 끝나 옛 세대가 남는다. `ensureBackupBranch`는 삭제 중인 Branch도 받아들인다. 이름을 찾는 제한은 1분이 아니라 약 2분이다.
- main Branch 객체를 롤백의 Detaching 중에 지우면(kubectl로만 가능) 옮긴 데이터가 Branch 없이 남는다. main이나 Project를 롤백 중에 지우면 `lastRestore`가 사라진다.

실행 파일·권한:

- `--kubeconfig` 경로는 client-go 기본값(5 QPS)을 쓴다. 신호 처리기를 매니저를 만든 뒤에 단다. `LeaderElectionReleaseOnCancel`을 켜지 않는다. 플래그 오류는 JSON 처리기를 달기 전에 텍스트로 남고 종료 코드가 1이다. readyz가 캐시 동기화를 기다리지 않는다.

테스트:

- 시험하지 않는 경로: 사용량 실패, `BootstrapFailed`, `MainNotRunning`, 1분 사용량 갱신, main 다시 만들기, 삭제 중인 Project에서 브랜치 데이터 건너뛰기, 부모 변경 깨우기(2초 다시 보기와 구분되지 않는다), `parseGeneration` 경계, `permanentDataFailure`의 507·501·400.
- `TestProjectWaitsForItsAgent`는 2초 다시 보기와 `AgentUnavailable`을 고정하지 않는다. 업데이트 루프 확인은 1초 sleep에 기대고 Project의 resourceVersion을 보지 않는다. `project_controller_test.go`의 한 실패 메시지는 조건이 없으면 패닉한다.
- `TestEachRequestRunsOnce`는 두 롤백이 같은 초에 끝날 때만 이름 충돌을 시험한다. 단계 저장을 빼거나 이름 바꾸기 경로를 고치는 변이는 테스트가 잡지 못한다.
- `TestDeletingABranchWaitsForItsPodToGo`는 Fatal로 끝나면 파드의 테스트 파이널라이저를 남긴다. envtest의 ClusterIP 범위 때문에 internal/controller를 `-count` 3 넘게 돌릴 수 없다.
- 오퍼레이터 테스트: `http.Get`에 시간 제한이 없고, 기한 20초가 preflight 제한 10초와 가깝고, 바이너리를 두 번 빌드하고, JSON 확인이 부분 문자열이고, 메트릭을 끈 것을 확인하지 않는다.
- 가짜 에이전트: 멈춘 PostgreSQL의 Bootstrap이 409(실제는 500), 데이터 없는 Status가 `data-0`(실제는 ""), PostgreSQL 상태를 파드 UID에 묶지 않고, 입력을 검사하지 않고, 종료 중인 파드를 응답 없음으로 보고, 파드 Get 오류를 파드 없음으로 본다. `loseAnswers`는 DetachGeneration만 따른다.

## 계획 4(REST API 서버)에서

### 미뤄 둔 개선 사항

API·핸들러:

- 브랜치 생성의 개수 검사와 생성이 원자적이지 않아 동시 요청이 `maxBranches`를 넘길 수 있다(파드 쿼터 `2 × maxBranches + 1`이 막는다). 롤백의 백업 브랜치 개수도 같다.
- 이미 원하는 상태인 정지·기동은 쓰지 않고 캐시를 믿어, watch가 늦는 짧은 구간에 다른 클라이언트가 바꾼 상태를 놓칠 수 있다. 충돌 재시도도 캐시를 다시 읽어, 캐시가 늦으면 같은 ID 재전송이 앞 결과 대신 409가 될 수 있다.
- 롤백·리셋의 앞 결과 재생이 `readyProject` 뒤에 있어, 프로젝트가 Ready가 아니게 되면 재전송이 409다.
- 프로젝트 삭제의 라벨 확인과 삭제 사이에 UID 전제 조건이 없다. 공백만 있는 `displayName`을 받는다. 브랜치 응답은 phase가 `Failed`일 때만 이유를 보여, `ParentNotRunning`·`CopyFailed`로 멈춘 브랜치의 202에는 원인이 없다.
- 복구 지점 생성은 본문 검사(400)를 프로젝트·브랜치 확인보다 먼저 한다. `restorePointOf`의 형식 검사는 두 호출자가 먼저 하므로 쓰이지 않는다. `restoreOpen`은 오퍼레이터의 `restorePending`을 복사한 것이다(내보내 함께 쓴다).
- `writeProblem`은 `%w`로 감싼 `apiError`를 알아보지 못하고, `writeJSON`의 직렬화 실패는 problem+json이 아니다. `waitFor`는 제한 시간과 마지막 확인이 겹치면 한 번을 건너뛸 수 있다. `Waits`의 음수 값을 막지 않는다. 405는 OpenAPI에 없다.
- `http.Server`에 `ReadTimeout`·`IdleTimeout`이 없다(롤백 대기 때문에 `WriteTimeout`은 두지 않는다). VolumeReady의 `NotBound` 이유는 상수가 아니다.

테스트:

- 가짜 클라이언트는 쓴 것을 바로 읽고 `generation`을 올리지 않아, 캐시 지연과 `observedGeneration` 관문의 통과 쪽은 단위 테스트로 시험하지 않는다(envtest의 실행 파일 테스트가 일부를 본다).
- 시험하지 않는 것: `validRequestID`의 CR·LF·NUL, `fromAgent`의 `reflink-unsupported`, `fromKube`의 `IsInvalid`, `storageSize` 경계(1Gi·1G), `compute: {}`, 정리 삭제 실패, 접속 URI의 IPv6·데이터베이스 이스케이프(`pgconn.ParseConfig`로), 준비 안 된 프로젝트의 복구 지점 목록·삭제, 진행 중인 복구를 새로 연 충돌 재시도.
- 가짜 에이전트는 데이터 없는 브랜치의 복구 지점 생성에 404를 주지 않고 목록을 오래된 순으로 준다(실제는 최신 먼저). 실행 파일 테스트는 바이너리를 다섯 번 빌드한다.

## 계획 5(배포·E2E·성능)에서

### 다음 서브프로젝트가 반영할 것

이 계획이 하지 않은 것:

- 노드 장애 주입으로 조정 지연을 재는 것과, 브랜치 파드가 실행 중일 때 에이전트 파드를 바꾸는 경우(이미지 변경)의 E2E는 네트워크 RWO 스토리지 클래스에서 확인한다. 개발 환경은 단일 노드·로컬 스토리지라 재현할 수 없다. 오퍼레이터는 새 에이전트 파드를 실행 중인 브랜치 파드의 노드에 둔다(필수 nodeAffinity). 응답 없는 감독 프로세스는 2초 제한으로 묻고, 2초에서 30초까지 늘려 가며 다시 묻는다.
- pg_bigm·PGroonga를 이미지에 넣을 수 있는지(Debian 패키지·빌드)와 넣을지는 스펙 16장의 결정을 기다린다. 공식 이미지에는 둘 다 없다.
- 리셋 취소: 리셋 요청을 받은 뒤 부모가 정지하면 오퍼레이터는 부모가 실행될 때까지 기다리고, 그동안 리셋을 취소할 방법이 없다(스펙 5장에 취소가 없다). API는 요청 시점의 부모 상태만 검사한다. 이 경우를 E2E에서 확인하고, 취소를 둘지 스펙에서 정한다.
- 스펙 4.2의 Project phase `Pending`은 쓰는 코드가 없다(API는 phase가 빈 프로젝트를 `Provisioning`으로 보인다). 쓸지 목록에서 뺄지 스펙에서 정한다.

연결 게이트웨이 서브프로젝트로:

- 외부 노출(인그레스·TLS). API 서버는 평문 HTTP로 듣고, 개발 환경의 E2E·성능은 `kubectl port-forward`를 쓴다.
- 인그레스(Envoy·Istio 등)가 바깥의 `X-Request-Id`를 덮어쓰지 않게 한다. 롤백·리셋의 멱등 키다.

성능 기준을 넘은 것(스펙 15장 2항, `docs/perf/2026-10-26-branching-engine.md`, 개발 환경의 Podman 머신 가상 디스크 위에서 8개 중 6개 통과):

- 쓰기 부하에서 브랜치 생성 API P50 2.74초(기준 1초 이하). 백업 모드 복사의 즉시 체크포인트에서 쓰기 단계가 P50 1.34초(버퍼 약 9.4k개, sync 0.03초)다. main의 CPU 한도를 2로 늘려 스로틀링을 없애도 쓰기 시간이 그대로여서 CPU 한도는 주된 원인이 아니고, `memory.pressure`는 10ms 이하로 남았다. `checkpoint_flush_after=0` 행에서는 1Gi memcg의 더러운 페이지 한계에 닿아 쓰기가 장치의 writeback 속도로 묶이는 것이 보였다. `checkpoint_flush_after=0`, 더 공격적인 bgwriter(생성 P50 2.44초), `checkpoint_timeout=30s`(생성 P50 3.96초, 202 두 번)는 도움이 되지 않았다. 호스트 경합의 몫은 따로 가려내지 못했다.
- 쓰기 부하에서 새 브랜치 첫 쿼리 P95 3.72초(기준 3초 이하). 원인은 검증하지 않았다(가설: 복사하는 동안 main이 쓴 WAL의 재적용과 호스트 경합). 2026-10-26 첫 전체 실행(ce6bce9)에서는 같은 조건의 P95가 1.63초로 통과했다. 같은 조건이 실행마다 기준의 양쪽에 놓였으므로 이 지표는 경합에 민감하다.
- 다음 단계: 실제 스토리지(NVMe·로컬 SSD, 호스트 스왑 없음)에서 다시 잰다. 그래도 기준을 넘으면 설계 선택지는 셋이다. 최근 복구 지점과 WAL로 복사해 원본에 즉시 체크포인트를 걸지 않기, 복사 전에 분산 체크포인트를 미리 돌리기, 첫 쿼리의 복구(재적용) 비용 줄이기(검증하지 않은 WAL 재적용 가설에 기댄 선택지다. 먼저 첫 쿼리 시간에서 재적용이 차지하는 몫을 잰다).

### 미뤄 둔 개선 사항

오퍼레이터·RBAC:

- Project의 RoleBinding `consonance-apiserver`의 roleRef가 바뀌었거나 라벨이 없는 같은 이름의 RoleBinding이 있으면(누가 손댄 뒤에만 생긴다) 조정이 오류를 되풀이한다(roleRef가 바뀐 것은 바꿀 수 없는 필드라 갱신이, 라벨이 없는 것은 라벨로 거른 캐시에 보이지 않아 생성이 AlreadyExists로 실패한다). 이 오류가 Project 조정의 1단계를 막아 부트스트랩과 사용량 갱신이 멈춘다(손댄 RoleBinding을 되돌리기 전에는 풀리지 않는다). RoleBinding을 지우고 다시 만들게 한다.
- 정책 테스트(`consonance-apiserver-namespace-delete`)는 라벨 값의 대소문자 변형, `deletecollection`, 테스트가 남기는 네임스페이스를 시험하지 않는다(역할에 `deletecollection`이 없다). `bind`는 오퍼레이터가 클러스터 전체 `secrets get`을 가진 동안 중복이라 테스트로 증명할 수 없다.
- 메트릭: `start_seconds`를 `settled()` 지점에서 status를 저장하기 전에 관측한다(중복·누락은 없다). 테스트 도우미 `settle()`은 앞선 테스트의 늦은 재시도와 `copy_errors`를 다루지 못할 수 있다. 기록·저장 경로를 다시 조정으로 강제하는 테스트가 없다. Ready였던 프로젝트가 Provisioning인 채로 오퍼레이터가 다시 시작하면 `project_create`를 한 번 더 관측한다.
- 컨트롤러: 지워지는 파드(DeletionTimestamp)는 잠깐 동안 Ready=True로 남는다.

API 서버:

- `GET /v1/status`를 Info 수준으로 남긴다. 고른 requestId가 붙은 401, 응답 래퍼의 `Unwrap()`, requestId 되돌리기를 시험하지 않는다. `TestReset`에 음성 사례가 없다.
- OpenAPI 테스트가 얕고, `started()` 문서가 없고, `http.ErrAbortHandler`는 로그 줄도 테스트도 없다.

배포 스크립트·매니페스트:

- `hack/dev-cluster.sh`: `up`의 기동 분기가 `configured` 상태를 다루지 않는다(`created`는 다룬다). 6443 확인이 어떤 서비스든 받아들이고 래퍼 인자를 따옴표로 묶지 않는다. 대기 루프가 말이 없고 시간 초과 때 k3s 로그를 보이지 않는다. `check`는 파일 시스템 종류를 출력만 하고 판정하지 않는다(`cp --reflink=always`가 reflink 없는 곳에서는 실패한다).
- `hack/load-images.sh`: 멈춘 머신을 "클러스터가 실행 중이 아님"으로 보고한다. `hack/pg-test.sh`: 이미 있는 XFS 디렉터리의 소유자를 `install -d`가 다시 바꾸고, 머신이 멈췄을 때 안내가 없다.
- 하위 kustomization이 파일을 이름으로 나열하고, `rollout status` 대기가 겹친다. 테스트 도우미 `readYAMLDocs`는 `\n---\n`로만 문서를 나눈다.

E2E·성능 하니스:

- `go test` 시간 초과(panic)나 Ctrl-C로 끝나면 정리가 돌지 않아 port-forward 자식 프로세스와 `cns-` 네임스페이스가 남을 수 있다(`go test`의 한계).
- 쓰기 부하 롤백은 요청 전에 재확인하지 않는다. `rb`의 평균 tps에 종료 때의 0 tps 줄이 들어간다. `end()`가 원격 pgbench를 `-T 7200`으로만 끝이 묶인 채 둘 수 있다.
- 성능 문서의 "측정한 코드" 커밋 표기는 `gitCommit`이 `test/`·`docs/`를 더러움 검사에서 빼므로, 고치지 않은 하니스로 돌렸는지 알려 주지 않는다(`4ed33e8` 실행은 7ec213c의 하니스를 썼다). 하니스 변경을 따로 표시한다.
