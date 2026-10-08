# 브랜칭 엔진 이월 사항

앞 계획의 구현·리뷰에서 나온 것 중 뒤 계획이 반영해야 할 사항과, 미뤄 둔 개선 사항을 모은다. 뒤 계획을 쓰기 전에 먼저 읽는다. 반영한 항목은 지운다.

## 계획 1(감독 프로세스)에서

### 계획 3(CRD·오퍼레이터)이 반영할 것

- 브랜치 파드의 `terminationGracePeriodSeconds`는 45초 이상으로 둔다(stop-grace 30초 + SIGQUIT 뒤 대기 10초 + 여유). 쿠버네티스 기본값 30초는 짧다.
- 브랜치 파드에는 항상 `limits.memory`를 준다. 한도가 없으면 Downward API가 노드 메모리를 넘겨 `shared_buffers`가 너무 크게 잡힌다.
- `AUTOSTART`, `MEMORY_LIMIT_BYTES`에는 유효한 값만 준다. 잘못된 값은 조용히 기본값으로 바뀐다.
- `/v1/start`의 503은 아직 기동 중이라는 뜻이다. 실패가 아니므로 9.4의 되돌리기를 하지 말고 `/v1/status`를 다시 확인한다. 500도 기동이 끝났다는 뜻은 아니므로 상태를 다시 확인한다.
- `/v1/start`의 409는 두 가지다. 다른 작업이 진행 중이거나(`ErrBusy`), 실행 중인 세대와 `current`가 다르다(`ErrGenerationMismatch`). 지금은 메시지로만 구분하므로, 오퍼레이터가 둘을 구분해야 하면 기계가 읽을 수 있는 오류 코드를 추가한다.
- 9.4 롤백에서 `current` 전환은 `/v1/status`가 `stopped`를 보인 뒤에 한다. 실행 중에 링크를 바꾸고 `start`를 부르면 409가 온다.

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
- 클라이언트: JSON이 아닌 오류 본문은 `APIError.Message`가 비고, 2xx 디코드 오류에 메서드와 경로가 없다. `baseURL` 끝의 `/`를 잘라내지 않아 `//v1/...` 요청이 301을 거쳐 405가 된다. `Bootstrap`, `APIError.Message`, `StopLSN`을 테스트하지 않는다. 기본 제한 75초는 서버의 기동 제한 60초에 맞춘 상수다.

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

### 계획 3(CRD·오퍼레이터)이 반영할 것

- 브랜치 Service `br-<이름>`은 5432와 함께 감독 프로세스 포트 8008을 노출한다. 에이전트는 `http://br-<브랜치>:8008`로 감독 프로세스를 부른다(`--supervisor-url`).
- 에이전트 파드 인자는 Project spec에서 온다: `--min-copy-interval`(`limits.minCopyInterval`), `--restore-points-per-branch`·`--restore-point-max-age`(`restorePoints`), `--project-id`. 값을 바꾸면 에이전트 파드를 다시 띄워야 반영된다. 볼륨은 `/volume`, 토큰은 Secret `internal-token`(키 `token`)을 `/etc/consonance/token`에 마운트한다(에이전트는 `/etc/consonance/token/token`을 읽는다).
- main 파드는 init-main이 성공한 뒤에 만든다. 그 전에 뜬 감독 프로세스는 `/v1/start`에 500(`data directory does not resolve`)을 돌려준다.
- 에이전트 오류 코드에 따라 다르게 한다: `busy`·`supervisor-unavailable`·500은 지수 백오프로 재시도, `rate-limited`는 `Retry-After` 뒤 재시도, `not-running`은 부모가 실행 중이 아님(API 409), `conflict`는 상태 문제라 그대로 재시도하지 않는다. `reflink-unsupported`(501)·`insufficient-storage`(507)와 브랜치 데이터 생성의 `not-found`(부모에 데이터 없음)는 영구 오류다(스펙 10장: phase Failed, 재시도하지 않는다). 복사 도중 원본 브랜치를 정지하면 그 뒤 최대 30초 동안 `busy`가 올 수 있다.
- 재시도해도 안전한 호출: `CreateBranchData`(데이터가 있으면 그 세대와 빈 LSN), `InitMain`, `DetachGeneration`(이미 옮겼으면 롤백은 toBranch를, 리셋은 그때 만든 복구 지점을 돌려준다), `DeleteBranchData`(없으면 성공). 그래서 `DetachGeneration`의 404는 옮길 세대가 없다는 뜻이다. Preparing 단계에서 오퍼레이터가 재시작하면 `ListGenerations`로 남은 세대를 확인해, 현재 세대보다 번호가 큰 세대(활성화하지 못한 앞 시도의 결과)만 `DeleteGeneration`으로 치운 뒤 다시 만든다. 새 세대는 언제나 있는 세대들보다 큰 번호를 받는다. 현재 세대보다 번호가 작은 세대는 사용자의 롤백·리셋 전 데이터일 수 있으므로 지우지 않는다.
- `ActivateGeneration`은 감독 프로세스가 `stopped`를 보일 때만 `current`를 바꾼다(다른 상태면 409, 감독 프로세스가 응답하지 않으면 502 `supervisor-unavailable`). n이 이미 현재 세대면 감독 프로세스에 묻지 않고 성공하므로 다시 불러도 안전하다. `DetachGeneration`·`DeleteGeneration`은 n이 현재 세대이거나 PostgreSQL이 그 세대를 쓰고 있으면 409, 감독 프로세스가 응답하지 않으면 502다.
- 스펙 11장의 메트릭은 에이전트에 아직 없다(계획 5).
- 브랜치 데이터 삭제(`DeleteBranchData`)는 브랜치 파드 객체가 사라진 뒤에 부른다. 삭제를 요청한 것만으로는 부족하다. 에이전트는 감독 프로세스가 응답하지 않으면 파드가 없다고 보고 지운다.
- 롤백의 Detaching 단계는 기존 세대를 백업 브랜치로 옮긴 뒤에 백업 Branch CR을 만든다. CR을 먼저 만들면 그 Branch의 조정이 `CreateBranchData`로 데이터를 먼저 만들어, 옮길 자리가 이미 차 있게 된다(`conflict`).
- 스펙 11장 로그의 `requestId`는 계획 2에 만드는 곳이 없다. 오퍼레이터·API 서버가 만들어 에이전트에 넘기는 방법(예: 요청 헤더)을 정하고 에이전트 로그에 남긴다.
- `agentclient.Client`는 `agent.Ops`를 구현하지 않는다(모든 메서드가 `ctx`를 받고 `Preflight`도 오류를 돌려준다). 쓰는 메서드만 모은 인터페이스를 쓰는 쪽에 둔다. 클라이언트는 리다이렉트를 따라가지 않아 3xx도 `APIError`다. `Code`가 빈 `APIError`는 `{error, code}` 본문이 아닌 응답이다. 프록시 같은 중간 장치가 냈거나, 에이전트의 라우터가 직접 낸 301(경로에 `.`·`..` 조각)·404(없는 경로)·405(맞지 않는 메서드)다.
- 에이전트 파드(`volume-agent`)의 `terminationGracePeriodSeconds`도 45초 이상으로 둔다. 에이전트는 SIGTERM을 받으면 진행 중인 요청을 최대 25초 기다린 뒤(그 안에 끝나지 않으면 종료 코드 1) 휴지통 비우기가 끝나기를 기다린다. 쿠버네티스 기본값 30초는 빠듯하다.
- 에이전트 파드의 프로브는 토큰 없이 200을 돌려주는 `GET /healthz`를 쓴다. 에이전트는 기동 시 정리(`Recover`)를 마친 뒤에 요청을 받기 시작한다.

### 계획 4(REST API 서버)가 반영할 것

- main 리셋은 공개 API가 막는다(스펙 5장 409 `conflict`). 에이전트는 main 삭제만 거부하고, main의 `ParentLatest` 세대 생성(리셋)은 받아들인다.
- `agentclient` 사용 시 주의점은 계획 3 항목과 같다(`agent.Ops`를 구현하지 않음, 리다이렉트를 따라가지 않음, `Code`가 빈 `APIError`).
