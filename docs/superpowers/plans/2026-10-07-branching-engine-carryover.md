# 브랜칭 엔진 이월 사항

앞 계획의 구현·리뷰에서 나온 것 중 뒤 계획이 반영해야 할 사항과, 미뤄 둔 개선 사항을 모은다. 뒤 계획을 쓰기 전에 먼저 읽는다. 반영한 항목은 지운다.

## 계획 1(감독 프로세스)에서

### 계획 2(볼륨 에이전트)가 반영할 것

- Lima VM의 `/mnt/project` XFS 루프 볼륨은 VM을 재시작하면 다시 마운트되지 않는다(fstab에 없음, 백킹 파일 `/var/lib/consonance-poc/project.img`). VM 준비 스크립트가 영구적으로 마운트해야 한다.
- 링크와 실행 중인 세대는 다를 수 있다. 브랜치·복구 지점 복사는 감독 프로세스 `GET /v1/status`의 `generation`(실행 중인 세대)을 복사하거나, 그 값이 `current`와 같은지 확인한 뒤 복사한다.
- 감독 프로세스는 `current`를 해석하지 못하면 링크 자체를 `-D`로 넘긴다(이때 `generation`은 빈 값). 해석할 수 없는 링크에서는 `pgctl.Manager.Start`가 기동을 거부하게 바꾼다.
- 슈퍼유저의 TCP 접속 차단은 initdb 때 쓰는 `pg_hba.conf`에서 한다. 감독 프로세스는 로컬 소켓으로만 슈퍼유저를 쓴다.
- `/v1/stop`은 열려 있는 백업 세션을 닫지 않는다. 복사 도중 정지하면 재기동 뒤 최대 30초 동안 `/v1/backup/start`가 409를 돌려준다. 에이전트의 잠금으로 복사와 정지를 직렬화한다.
- 동시 스냅숏을 막는 장치는 PostgreSQL 인스턴스당 `Backups` 하나뿐이다. PG 15부터는 세션마다 비배타적 백업을 동시에 열 수 있다.

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
