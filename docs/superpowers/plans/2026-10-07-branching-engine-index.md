# 브랜칭 엔진 구현 계획 목차

스펙: `docs/superpowers/specs/2026-10-07-branching-engine-design.md`

스펙이 독립적으로 테스트할 수 있는 구성 요소 네 개와 배포·E2E로 이루어져 있어 계획을 다섯 개로 나눈다. 각 계획은 그 자체로 테스트되는 결과물을 낸다. 뒤 계획은 앞 계획이 구현된 뒤에 작성해, 실제 코드의 이름과 타입을 그대로 참조한다.

| # | 계획 | 결과물 | 상태 |
| --- | --- | --- | --- |
| 1 | [감독 프로세스](2026-10-07-branching-engine-1-supervisor.md) | `internal/pgctl`, 감독 프로세스 API·클라이언트, `consonance/postgres:17` 이미지 | 구현 완료 |
| 2 | [볼륨 에이전트](2026-10-08-branching-engine-2-volume-agent.md) | `internal/volume`(reflink 복사, 세대, 카탈로그, 잠금·간격 제한, preflight, initdb), 에이전트 API·클라이언트, `consonance/agent` 이미지 | 작성 완료 |
| 3 | 리소스 타입과 오퍼레이터 | Project·Branch CRD, Project·Branch 조정(롤백·리셋 포함), 파드·네트워크 정책 명세, envtest | 계획 2 구현 후 작성 |
| 4 | REST API 서버 | 엔드포인트, 관리자 키 인증, problem+json, 동기 대기, OpenAPI | 계획 3 구현 후 작성 |
| 5 | 배포·E2E·성능 | 개발용 kustomize 매니페스트, 이미지 4종, k3s E2E 시나리오, 성능 측정, PSS restricted 검증 | 계획 4 구현 후 작성 |

앞 계획의 구현에서 나온, 뒤 계획이 반영할 사항과 미뤄 둔 개선 사항은 [이월 사항](2026-10-07-branching-engine-carryover.md)에 모은다. 뒤 계획을 쓰기 전에 먼저 읽는다.
