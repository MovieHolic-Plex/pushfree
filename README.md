# PushFree

**[한국어](README.md)** | [English](README.en.md)

PushFree는 셀프 호스팅이 가능한 [Pushover](https://pushover.net) API 호환
푸시 알림 서비스입니다. 오픈 소스(Apache-2.0)로, 서버는 직접 운영하고,
데이터도 직접 소유하며, 메시지당 요금은 내지 않습니다.

작고 단일한 Go 바이너리 하나가 API, 실시간 WebSocket/SSE 팬아웃, 그리고
내장된 관리자 대시보드를 모두 제공합니다. 네이티브 클라이언트(Android,
데스크톱)는 Open Client 프로토콜을 사용하며, Pushover의 `messages.json`을
이미 사용하는 어떤 도구든 수정 없이 그대로 동작합니다.

## 뱃지

<!-- badges: CI status, license, latest version -->

## 빠른 시작

소스에서 빌드하려면 Go 툴체인(1.26 이상)이 설치되어 있어야 합니다.

```sh
# 1. Build the single static binary (pure Go, no cgo).
cd server
go build -o pushfree ./cmd/pushfree

# 2. Create an account. The FIRST account becomes the admin.
./pushfree &  # listens on :2586 by default
curl -sf -X POST http://localhost:2586/v1/accounts \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"correct-horse"}'

# 3. Log in (sets a session cookie) and create an app token.
curl -sc cookies.txt -X POST http://localhost:2586/v1/accounts/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"correct-horse"}'
TOKEN=$(curl -sb cookies.txt -X POST http://localhost:2586/v1/apps \
  -H 'Content-Type: application/json' -d '{"name":"monitoring"}' | sed -E 's/.*"token":"([^"]+)".*/\1/')
USERKEY=$(curl -sb cookies.txt http://localhost:2586/v1/accounts/me | sed -E 's/.*"user_key":"([^"]+)".*/\1/')

# 4. Send a message (Pushover-compatible).
curl -sf -X POST http://localhost:2586/1/messages.json \
  -d "token=$TOKEN" -d "user=$USERKEY" -d "message=hello from pushfree"

# 5. Health check.
curl -sf http://localhost:2586/health   # -> {"status":"ok"}
```

### Docker

멀티 스테이지 distroless 이미지와 `deploy/docker-compose.yml`(서버, `postgres`
프로필을 통한 선택적 Postgres 서비스 포함)을 제공합니다. 이미지 빌드는
`server/Dockerfile`, compose 파일은 `deploy/docker-compose.yml`에 있습니다.
빌드 후 실행:

```sh
docker build -t pushfree server/
docker compose -f deploy/docker-compose.yml up -d
curl -sf localhost:2586/health   # -> {"status":"ok"}
```

TLS, 리버스 프록시, 백업, Postgres 옵션은
[docs/self-hosting.md](docs/self-hosting.md)를 참고하세요.

## 기능

실제로 구현된 기능입니다 (각 항목은 완료된 계획 항목에 대응하며, 계약
세부사항은 [docs/](docs/)를 참고하세요):

- **Pushover 호환 전송 API** — 전체 필드 계약(메시지/제목/URL 길이 제한은
  UTF-8 rune 단위, 우선순위 `-2..2`, `html`/`monospace` 상호 배타, 단일
  첨부파일 <= 5 MiB, `ttl`, `tags`, `callback`, `encrypted`)을 갖춘
  `POST /1/messages.json`. `server/internal/api/messages.go`
- **계정 및 세션** — 자유로운 가입, 첫 계정이 관리자, argon2id
  비밀번호(RFC 9106), HMAC 서명 세션 쿠키, 방해 금지 시간 설정.
  `server/internal/api/accounts.go`, `server/internal/api/security.go`
- **앱 토큰 및 속도제한 헤더** — `POST/GET/DELETE /v1/apps`; 모든 `/1/*`
  응답에 `X-Limit-App-Limit/Remaining/Reset` 포함.
  `server/internal/api/apps.go`, `server/internal/api/applimit.go`
- **월간 할당량** — 사용자당 월 10,000건 전송, America/Chicago 기준 00:00에
  리셋, `GET /1/apps/limits.json`, 쓰기 전 429 게이트.
  `server/internal/api/quota.go`, `server/internal/quota/quota.go`
- **다중 사용자 팬아웃 및 그룹** — 쉼표로 구분된 `user` 목록(키 50개 이하),
  전달 그룹(CRUD), 실제 수신자마다 할당량 1단위 부여.
  `server/internal/api/groups.go`
- **긴급(우선순위 2) 영수증** — 상태 머신, 내구성 재시도 스케줄러
  (최소 30초, 만료 상한 3시간, 최대 50회 시도 상한), 크래시 복구 타이머,
  ack, cancel, cancel-by-tag, 7일 조회 기간, GC. `server/internal/receipts/`,
  `server/internal/api/receipts.go`, `server/internal/api/cancel.go`
- **콜백 워커** — ack 시 영수증 JSON 웹훅, 2xx 외 응답에 60초 재시도,
  SSRF 허용 목록(loopback/link-local/RFC1918/ULA는 기본 차단).
  `server/internal/callbacks/worker.go`
- **실시간 허브** — `since` 커서 기반 재생을 지원하는 WebSocket 및 SSE,
  45초 keepalive, 기기 등록(`POST /1/devices/login.json`, SHA-256 비밀키).
  `server/internal/hub/`
- **전달 채널** — WS/SSE(일급 지원), 선택적 FCM v1(환경변수 게이트),
  UnifiedPush 디스트리뷰터. `server/internal/fcm/`, `server/internal/up/`
- **방해 금지 시간** — `priority <= 0`인 메시지는 서버 측에서 보류,
  `priority >= 1`은 우회. `server/internal/quiethours/`
- **구독** — 코드 + 앱별 동적 키 + 마이그레이션.
  `server/internal/api/subscriptions.go`
- **종단간 암호화(E2EE)** — GZIP/AES-256-CBC/HMAC 필드를 불투명하게 저장하며,
  서버는 절대 복호화하지 않습니다. `server/internal/e2ee/`
- **검증 및 사운드** — `POST /1/users/validate.json`, `GET /1/sounds.json`
  (내장 사운드 23종). `server/internal/api/validate.go`,
  `server/internal/api/sounds.go`
- **관측 가능성** — `GET /metrics`(Prometheus) + 구조화된 요청 로깅.
  `server/internal/metrics/`
- **보존 및 종료** — 30일 메시지 보존, 3일 첨부파일 BLOB 보존, TTL 폐기,
  WAL 체크포인트를 동반한 정상 종료. `server/internal/retention/`
- **내장 대시보드** — `go:embed`로 `/admin/`에서 서비스되는 Next.js 정적
  익스포트(앱, 할당량, 실시간 SSE 메시지 뷰, 영수증, 방해 금지 시간).
  `server/internal/webmount/`, `web/`
- **클라이언트** — Android(WS/FCM/UnifiedPush 전송 방식, Android 14
  풀스크린 인텐트 흐름을 갖춘 긴급 채널, ack 아웃박스), Tauri 2 데스크톱
  (직접 WS, 중복 제거, ack). `android/`, `desktop/`
- **이중 데이터베이스** — SQLite(기본값, 순수 Go `modernc.org/sqlite`)와
  Postgres(`pgx/v5`)가 동일한 store 인터페이스 뒤에 위치합니다.
  [docs/POSTGRES.md](docs/POSTGRES.md) 참고.

## 문서

- [시작하기](docs/getting-started.md)
- [설정](docs/configuration.md)
- [HTTP API 레퍼런스](docs/api.md)
- [클라이언트(Android, 데스크톱, 대시보드)](docs/clients.md)
- [셀프 호스팅(TLS, 백업, Postgres)](docs/self-hosting.md)
- [Postgres 백엔드](docs/POSTGRES.md)
- [Pushover API 호환성 매트릭스](docs/API-COMPAT.md)

## 프로젝트 구조

```text
server/    Go server (single binary): API, hub, receipts, store, dashboard embed
android/   native Android client (WS / FCM / UnifiedPush)
desktop/   Tauri 2 desktop client (direct WS)
web/       Next.js static-export admin dashboard (embedded into the binary)
deploy/    docker-compose and deployment assets
docs/      documentation set
```

## 라이선스

[Apache License 2.0](LICENSE)에 따라 라이선스됩니다.
