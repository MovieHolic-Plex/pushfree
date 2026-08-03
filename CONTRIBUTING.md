# Contributing to PushFree

PushFree is a self-hostable, Pushover-API-compatible push service. The server
is a single Go binary; native clients are Android (Kotlin) and desktop (Rust /
Tauri 2); the admin dashboard is a Next.js static export embedded into the
server binary. This guide describes the toolchains, the test commands, and the
commit conventions used across the repository.

The project layout and feature set are summarized in the
[README](README.md); the per-endpoint contracts live in
[docs/API-COMPAT.md](docs/API-COMPAT.md).

## Repository layout

```text
server/    Go server (single binary): API, hub, receipts, store, dashboard embed
android/   native Android client (WS / FCM / UnifiedPush)
desktop/   Tauri 2 desktop client (direct WS)
web/       Next.js static-export admin dashboard (embedded into the binary)
deploy/    docker-compose and deployment assets
docs/      documentation set
scripts/   setup-android.{ps1,sh} bootstrap helpers
```

## Toolchains

| Component  | Toolchain                                   | Version pin            |
| ---------- | ------------------------------------------- | ---------------------- |
| server     | Go                                          | 1.26+ (CI uses 1.26)   |
| desktop    | Rust (Tauri 2, edition 2021)                | rust-version 1.77.2+   |
| android    | Android Gradle Plugin + JDK 17              | compileSdk 35          |
| web        | Node.js + pnpm (Next.js 15)                 | pnpm (lockfile pinned) |
| docs lint  | optional: lychee (link checker)             | any recent             |

The server is pure Go (`CGO_ENABLED=0`, the `modernc.org/sqlite` driver); no C
compiler is required to build or test it. The desktop client is also pure Rust
(no system Tauri prerequisites for `cargo test`).

### Bootstrap

- **Go**: install from go.dev; verify with `go version`.
- **Rust**: `rustup` stable; the desktop crate pins `rust-version = "1.77.2"`.
- **Android**: run `scripts/setup-android.sh` (Linux/macOS) or
  `scripts/setup-android.ps1` (Windows) after accepting the SDK licenses; it
  expects JDK 17 and the Android SDK. See [docs/clients.md](docs/clients.md).
- **Web**: `corepack enable && corepack prepare pnpm@latest --activate`, then
  `cd web && pnpm install --frozen-lockfile`.

## Building

```sh
# server (single static binary, no cgo)
cd server && go build -o pushfree ./cmd/pushfree

# desktop (debug build)
cd desktop && cargo build

# android debug APK
cd android && ./gradlew assembleDebug

# web (static export -> web/out, later embedded into the server binary)
cd web && pnpm build
```

## Testing

Run the suite for the component you changed. The acceptance targets below are
the same commands CI runs (`.github/workflows/`).

### Server (Go)

```sh
cd server
go build ./...                      # must succeed
go vet ./...                        # must be clean
go test ./... -count=1              # full suite (sqlite backend)
```

Backend-specific store/api lane (also run when touching persistence):

```sh
go test ./internal/store/... ./internal/api/... -count=1            # sqlite
# with a live Postgres + PG_TEST_DSN set:
go test ./... -count=1 -tags postgres
```

A focused subset by feature, mirroring the plan's acceptance clauses:

```sh
go test ./internal/api/...     -run 'TestIngest|TestFanout|TestGroups' -count=1
go test ./internal/api/...     -run 'TestValidate|TestSounds'          -count=1
go test ./internal/quota/...                                           -count=1
go test ./internal/e2ee/...                                            -count=1
go test ./internal/hub/...                                             -count=1
go test ./internal/timers/...                                          -count=1
go test ./internal/callbacks/...                                       -count=1
```

`go test -race` requires cgo (a C compiler). On hosts without one it is
deferred to CI (`.github/workflows/ci.yml`); do not commit a local "-race
clean" claim you could not actually run.

### Desktop (Rust)

```sh
cd desktop
cargo test --locked               # full suite
cargo test e2ee                   # E2EE vectors (shared fixture)
cargo clippy --all-targets -- -D warnings   # must be warning-free
```

### Android (Kotlin)

```sh
cd android
./gradlew testDebugUnitTest                  # all unit tests
./gradlew testDebugUnitTest --tests "*E2ee*" # E2EE vectors (shared fixture)
./gradlew assembleDebug                      # build the APK
./gradlew verifyPaparazziDebug               # golden screenshot tests
```

### Web (Next.js)

```sh
cd web
pnpm install --frozen-lockfile
pnpm build        # static export -> web/out (embedded into the server later)
pnpm lint
```

When you change the dashboard, rebuild `web/out` and re-copy it into the
embed location the server serves (`server/internal/webmount/web/out`) before
re-running `go test ./internal/webmount/...`.

## Commit convention

This project follows [Conventional Commits](https://www.conventionalcommits.org/).

- One logical change = one commit, with implementation and its tests together.
- Subject format: `<type>(<scope>): <summary>`
  - types: `feat`, `fix`, `test`, `ci`, `chore`, `docs`
  - scopes (examples): `server`, `android`, `desktop`, `web`, `deploy`,
    `release`, `clients`
  - examples already in history:
    `feat(server): multi-user fanout and groups`,
    `feat(clients): E2EE decryption on Android and desktop`,
    `docs: documentation set and api-compat matrix`
- Write the summary in the imperative ("add", not "added").

### Mandatory pre-commit gates

Do not commit until the static gates for the touched component pass:

| Component | Gate                                                                 |
| --------- | -------------------------------------------------------------------- |
| server    | `go vet ./...`, `go build ./...`, and the relevant `go test` green   |
| desktop   | `cargo clippy --all-targets -- -D warnings`, `cargo test` green      |
| android   | `./gradlew assembleDebug` + the affected `testDebugUnitTest` green   |
| web       | `pnpm build` + `pnpm lint` green                                     |

Never commit a change whose acceptance target is red, and never push WIP or
merge-by-squash noise — keep history readable.

## Pull requests

- Open a PR against `master` with a clear description and the acceptance
  command(s) you ran.
- Reference the plan todo or issue where relevant.
- Prefer the smallest change that satisfies the goal; avoid drive-by refactors
  outside the scope of the change.
- If you add or change an endpoint, update [docs/API-COMPAT.md](docs/API-COMPAT.md)
  and [docs/api.md](docs/api.md).

## Reporting security issues

See [SECURITY.md](SECURITY.md). Do not open a public issue for a security
vulnerability.

## License

Contributions are licensed under the [Apache License 2.0](LICENSE). By
submitting a change you agree your contribution is licensed under the same
terms as the rest of the project.
